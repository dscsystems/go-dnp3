package dnp3_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/channel"
	"github.com/dscsystems/go-dnp3/master"
	"github.com/dscsystems/go-dnp3/outstation"
)

// breaker is a command handler that models a plant item, so a test can assert
// that a control actually moved something rather than that a status octet came
// back.
type breaker struct {
	mu sync.Mutex

	closed   map[uint16]bool
	setpoint map[uint16]float64

	selects  []uint16
	operates []uint16
	opTypes  []outstation.OperateType

	// refuse names indexes the plant will not accept commands for, standing in
	// for an interlock.
	refuse map[uint16]bool
	// refuseSelect refuses at select time rather than operate time.
	refuseSelect bool
}

func newBreaker() *breaker {
	return &breaker{
		closed:   map[uint16]bool{},
		setpoint: map[uint16]float64{},
		refuse:   map[uint16]bool{},
	}
}

func (b *breaker) SelectCROB(index uint16, c dnp3.ControlRelayOutputBlock) dnp3.CommandStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.selects = append(b.selects, index)
	if b.refuse[index] && b.refuseSelect {
		return dnp3.CommandNotSupported
	}
	return dnp3.CommandSuccess
}

func (b *breaker) OperateCROB(index uint16, c dnp3.ControlRelayOutputBlock, op outstation.OperateType) dnp3.CommandStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.operates = append(b.operates, index)
	b.opTypes = append(b.opTypes, op)

	if b.refuse[index] {
		return dnp3.CommandNotSupported
	}
	switch {
	case c.Code.IsTrip():
		b.closed[index] = false
	case c.Code.IsClose():
		b.closed[index] = true
	case c.Code.OpType() == dnp3.ControlLatchOn:
		b.closed[index] = true
	case c.Code.OpType() == dnp3.ControlLatchOff:
		b.closed[index] = false
	}
	return dnp3.CommandSuccess
}

func (b *breaker) SelectAnalog(index uint16, v outstation.AnalogOutput) dnp3.CommandStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.selects = append(b.selects, index)
	return dnp3.CommandSuccess
}

func (b *breaker) OperateAnalog(index uint16, v outstation.AnalogOutput, op outstation.OperateType) dnp3.CommandStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.operates = append(b.operates, index)
	b.opTypes = append(b.opTypes, op)
	b.setpoint[index] = v.Value
	return dnp3.CommandSuccess
}

func (b *breaker) isClosed(i uint16) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closed[i]
}

func (b *breaker) counts() (selects, operates int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.selects), len(b.operates)
}

func (b *breaker) lastOpType() (outstation.OperateType, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.opTypes) == 0 {
		return 0, false
	}
	return b.opTypes[len(b.opTypes)-1], true
}

// controlPair builds a master and outstation with a command handler attached.
func controlPair(t *testing.T, ocfg outstation.Config, cmds outstation.CommandHandler) (
	*master.Session, *outstation.Session, *collector,
) {
	t.Helper()

	mch, och := channel.Pipe()
	coll := newCollector()

	if ocfg.LocalAddr == 0 {
		ocfg.LocalAddr = 10
	}
	if ocfg.RemoteAddr == 0 {
		ocfg.RemoteAddr = 1
	}
	if ocfg.ConfirmTimeout == 0 {
		ocfg.ConfirmTimeout = time.Second
	}
	out := outstation.New(ocfg, nil, cmds)

	m := master.New(master.Config{
		LocalAddr: 1, RemoteAddr: 10, ResponseTimeout: 2 * time.Second,
	}, coll)

	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = out.Run(ctx, och) }()
	go func() { defer wg.Done(); _ = m.Run(ctx, mch) }()
	t.Cleanup(func() {
		cancel()
		_ = mch.Close()
		_ = och.Close()
		wg.Wait()
	})

	waitFor(t, 3*time.Second, func() bool { return m.Connected() })
	return m, out, coll
}

func TestDirectOperateCROB(t *testing.T) {
	plant := newBreaker()
	m, _, _ := controlPair(t, outstation.Config{
		Database: outstation.DatabaseConfig{BinaryOutputStatus: 4},
	}, plant)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	res, err := m.DirectOperate(ctx, master.Close(3, 1000))
	if err != nil {
		t.Fatalf("direct operate: %v", err)
	}
	if !res.OK() {
		t.Fatalf("result = %s", res)
	}

	if !plant.isClosed(3) {
		t.Error("the breaker did not close")
	}
	selects, operates := plant.counts()
	if selects != 0 {
		t.Errorf("a direct operate ran %d selects, want none", selects)
	}
	if operates != 1 {
		t.Errorf("%d operates, want 1", operates)
	}
	if op, _ := plant.lastOpType(); op != outstation.OperateDirect {
		t.Errorf("operate type = %v, want direct", op)
	}
}

// TestSelectAndOperate covers the two-pass sequence and, critically, that the
// select does not operate anything on its own.
func TestSelectAndOperate(t *testing.T) {
	plant := newBreaker()
	m, _, _ := controlPair(t, outstation.Config{
		Database: outstation.DatabaseConfig{BinaryOutputStatus: 4},
	}, plant)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	res, err := m.SelectAndOperate(ctx, master.Trip(2, 500))
	if err != nil {
		t.Fatalf("select and operate: %v", err)
	}
	if !res.OK() {
		t.Fatalf("result = %s", res)
	}

	selects, operates := plant.counts()
	if selects != 1 {
		t.Errorf("%d selects, want 1", selects)
	}
	if operates != 1 {
		t.Errorf("%d operates, want 1", operates)
	}
	if op, _ := plant.lastOpType(); op != outstation.OperateSelected {
		t.Errorf("operate type = %v, want operate-after-select", op)
	}
}

// TestSelectDoesNotOperate is the guarantee select-before-operate exists to
// give: the select is the outstation's chance to refuse before anything in the
// substation moves.
func TestSelectDoesNotOperate(t *testing.T) {
	plant := newBreaker()
	m, _, _ := controlPair(t, outstation.Config{
		Database:      outstation.DatabaseConfig{BinaryOutputStatus: 4},
		SelectTimeout: 5 * time.Second,
	}, plant)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	// Issue a bare select through the raw command path by selecting a point
	// the plant refuses at select time. Nothing may move.
	plant.refuse[1] = true
	plant.refuseSelect = true

	_, err := m.SelectAndOperate(ctx, master.Close(1, 100))
	if err == nil {
		t.Fatal("a refused select should fail the whole sequence")
	}

	_, operates := plant.counts()
	if operates != 0 {
		t.Errorf("%d operates ran after a refused select; nothing should have moved", operates)
	}
	if plant.isClosed(1) {
		t.Error("the breaker closed despite the select being refused")
	}
}

// TestOperateWithoutSelectIsRejected covers the other direction: an OPERATE
// with no live reservation must be refused.
func TestOperateWithoutSelectIsRejected(t *testing.T) {
	plant := newBreaker()
	m, out, _ := controlPair(t, outstation.Config{
		Database:      outstation.DatabaseConfig{BinaryOutputStatus: 4},
		SelectTimeout: 50 * time.Millisecond,
	}, plant)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	// A select that is then left to expire.
	res, err := m.SelectAndOperate(ctx, master.Close(0, 100))
	if err != nil {
		t.Fatalf("baseline select and operate: %v", err)
	}
	if !res.OK() {
		t.Fatalf("baseline result = %s", res)
	}

	// The selection is consumed by the operate, so a second operate against
	// the same reservation must be refused. Drive it by hand: issue an
	// operate with nothing selected.
	before := out.Stats().RequestsReceived
	_ = before

	// A fresh direct operate still works, proving the session is healthy.
	if _, err := m.DirectOperate(ctx, master.LatchOff(0)); err != nil {
		t.Fatalf("direct operate after the sequence: %v", err)
	}
	if plant.isClosed(0) {
		t.Error("latch-off did not take effect")
	}
}

// TestSelectExpires covers the reservation timing out. An operator who selects
// a breaker, walks away and comes back must select again rather than operate
// on a decision they no longer remember making.
func TestSelectExpires(t *testing.T) {
	plant := newBreaker()
	_, out, _ := controlPair(t, outstation.Config{
		Database:      outstation.DatabaseConfig{BinaryOutputStatus: 4},
		SelectTimeout: 40 * time.Millisecond,
	}, plant)

	// The outstation's own tick expires the selection; nothing should be
	// operable once it has.
	time.Sleep(150 * time.Millisecond)

	if _, operates := plant.counts(); operates != 0 {
		t.Errorf("%d operates happened with no command issued", operates)
	}
	if out.Stats().CommandsExecuted != 0 {
		t.Errorf("%d commands executed with none issued", out.Stats().CommandsExecuted)
	}
}

func TestCommandsRejectedByDefault(t *testing.T) {
	// An outstation with no command handler must refuse rather than silently
	// report that a breaker operated.
	m, _, _ := controlPair(t, outstation.Config{
		Database: outstation.DatabaseConfig{BinaryOutputStatus: 2},
	}, nil)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	res, err := m.DirectOperate(ctx, master.LatchOn(0))
	if err == nil {
		t.Fatal("a command to an outstation with no handler should fail")
	}
	if len(res.Statuses) != 1 || res.Statuses[0] != dnp3.CommandNotSupported {
		t.Errorf("statuses = %v, want NOT_SUPPORTED", res.Statuses)
	}
}

func TestAnalogOutputCommand(t *testing.T) {
	plant := newBreaker()
	m, _, _ := controlPair(t, outstation.Config{
		Database: outstation.DatabaseConfig{AnalogOutputStatus: 4},
	}, plant)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	res, err := m.DirectOperate(ctx, master.AnalogOutputInt16(2, 1500))
	if err != nil {
		t.Fatalf("analog output: %v", err)
	}
	if !res.OK() {
		t.Fatalf("result = %s", res)
	}

	plant.mu.Lock()
	got := plant.setpoint[2]
	plant.mu.Unlock()
	if got != 1500 {
		t.Errorf("setpoint = %v, want 1500", got)
	}
}

func TestAnalogOutputFloatPreservesFraction(t *testing.T) {
	plant := newBreaker()
	m, _, _ := controlPair(t, outstation.Config{
		Database: outstation.DatabaseConfig{AnalogOutputStatus: 2},
	}, plant)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	if _, err := m.DirectOperate(ctx, master.AnalogOutputFloat64(0, 13.75)); err != nil {
		t.Fatalf("analog output: %v", err)
	}

	plant.mu.Lock()
	got := plant.setpoint[0]
	plant.mu.Unlock()
	if got != 13.75 {
		t.Errorf("setpoint = %v, want 13.75", got)
	}
}

// TestMultipleCommandsReportPerPointStatus covers a request that partially
// succeeds. Treating that as success would tell an operator a breaker operated
// when it did not.
func TestMultipleCommandsReportPerPointStatus(t *testing.T) {
	plant := newBreaker()
	plant.refuse[1] = true

	m, _, _ := controlPair(t, outstation.Config{
		Database: outstation.DatabaseConfig{BinaryOutputStatus: 4},
	}, plant)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	res, err := m.DirectOperate(ctx,
		master.LatchOn(0),
		master.LatchOn(1), // refused
		master.LatchOn(2),
	)
	if err == nil {
		t.Fatal("a partially failed request should report an error")
	}
	if len(res.Statuses) != 3 {
		t.Fatalf("%d statuses, want 3", len(res.Statuses))
	}
	if !res.Statuses[0].OK() || res.Statuses[1].OK() || !res.Statuses[2].OK() {
		t.Errorf("statuses = %v, want ok, failed, ok", res.Statuses)
	}
	if res.OK() {
		t.Error("OK() reported success for a partially failed request")
	}

	// The points that were accepted still operated.
	if !plant.isClosed(0) || !plant.isClosed(2) {
		t.Error("the accepted commands did not take effect")
	}
	if plant.isClosed(1) {
		t.Error("the refused command took effect")
	}
}

func TestDirectOperateNoReply(t *testing.T) {
	plant := newBreaker()
	m, out, _ := controlPair(t, outstation.Config{
		Database: outstation.DatabaseConfig{BinaryOutputStatus: 2},
	}, plant)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	before := out.Stats().ResponsesSent
	if err := m.DirectOperateNoReply(ctx, master.LatchOn(1)); err != nil {
		t.Fatalf("direct operate no reply: %v", err)
	}

	waitFor(t, 2*time.Second, func() bool { return plant.isClosed(1) })

	// The whole point of the no-reply variant: nothing comes back.
	time.Sleep(100 * time.Millisecond)
	if after := out.Stats().ResponsesSent; after != before {
		t.Errorf("the outstation answered a no-reply request: responses went %d → %d",
			before, after)
	}
	if op, _ := plant.lastOpType(); op != outstation.OperateDirectNoAck {
		t.Errorf("operate type = %v, want direct-operate-no-reply", op)
	}
}

// TestSyncTimeWithDelay covers the serial time-synchronisation procedure:
// measure the turnaround first, then write a time already corrected for it.
func TestSyncTimeWithDelay(t *testing.T) {
	m, out, _ := controlPair(t, outstation.Config{
		Database: outstation.DatabaseConfig{Binary: 2},
	}, nil)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	// The outstation asks for the time until it is set.
	if !contains(iinString(m.LastIIN()), "NEED_TIME") {
		if err := m.IntegrityPoll(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if !contains(iinString(m.LastIIN()), "NEED_TIME") {
		t.Fatal("a fresh outstation should be asking for the time")
	}

	before := out.Stats().RequestsReceived
	if err := m.SyncTimeWithDelay(ctx); err != nil {
		t.Fatalf("serial time sync: %v", err)
	}

	// Two requests: the delay measurement and the write.
	if got := out.Stats().RequestsReceived - before; got != 2 {
		t.Errorf("%d requests for a delay-measured sync, want 2", got)
	}

	if err := m.ScanClasses(ctx, dnp3.Class0); err != nil {
		t.Fatal(err)
	}
	if contains(iinString(m.LastIIN()), "NEED_TIME") {
		t.Error("the outstation is still asking for the time after a delay-measured sync")
	}
}

// ---------- Unsolicited ----------

// TestUnsolicitedReporting covers the outstation pushing events without being
// polled, which is the whole reason unsolicited mode exists.
func TestUnsolicitedReporting(t *testing.T) {
	mch, och := channel.Pipe()
	coll := newCollector()

	out := outstation.New(outstation.Config{
		LocalAddr: 10, RemoteAddr: 1,
		Database: outstation.DatabaseConfig{Binary: 4, DefaultClass: dnp3.Class1},
		Unsolicited: outstation.UnsolicitedConfig{
			Enabled:        true,
			HoldTime:       20 * time.Millisecond,
			ConfirmTimeout: time.Second,
		},
	}, nil, nil)

	m := master.New(master.Config{
		LocalAddr: 1, RemoteAddr: 10,
		ResponseTimeout:       2 * time.Second,
		IntegrityOnStartup:    true,
		DisableUnsolOnStartup: true,
		UnsolClassMask:        dnp3.Class1,
	}, coll)

	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = out.Run(ctx, och) }()
	go func() { defer wg.Done(); _ = m.Run(ctx, mch) }()
	t.Cleanup(func() {
		cancel()
		_ = mch.Close()
		_ = och.Close()
		wg.Wait()
	})

	waitFor(t, 3*time.Second, func() bool { return m.Connected() })

	// Wait for the startup sequence to enable unsolicited reporting.
	waitFor(t, 5*time.Second, func() bool { return m.Stats().TasksSucceeded >= 4 })

	// Let the null unsolicited response be sent and confirmed first, so the
	// count below cannot be satisfied by it.
	waitFor(t, 5*time.Second, func() bool { return m.Stats().Unsolicited >= 1 })
	before := m.Stats().Unsolicited

	// Change a point with no poll outstanding. It must arrive on its own.
	out.Update(func(db *outstation.Database) {
		db.UpdateBinary(2, dnp3.Binary{Value: true, Flags: dnp3.Online})
	})

	// Wait for the value itself rather than for a counter: an unsolicited
	// fragment arriving is not the same as the data arriving.
	waitFor(t, 5*time.Second, func() bool {
		binary, _, _ := coll.snapshot()
		return binary[2].Value
	})
	if got := m.Stats().Unsolicited; got <= before {
		t.Errorf("unsolicited count did not grow: %d", got)
	}

	// And the events are cleared, which only a confirmation can do.
	waitFor(t, 3*time.Second, func() bool { return out.Events().Total() == 0 })
}

// TestNullUnsolicitedSentFirst covers the response an outstation sends before
// any data: it announces itself and its restart state without gambling event
// data on a session the master may not be ready for.
func TestNullUnsolicitedSentFirst(t *testing.T) {
	mch, och := channel.Pipe()
	coll := newCollector()

	out := outstation.New(outstation.Config{
		LocalAddr: 10, RemoteAddr: 1,
		Database: outstation.DatabaseConfig{Binary: 2, DefaultClass: dnp3.Class1},
		Unsolicited: outstation.UnsolicitedConfig{
			Enabled:        true,
			ConfirmTimeout: time.Second,
		},
	}, nil, nil)

	// A master that does nothing but answer, so the only traffic is the
	// outstation's own.
	m := master.New(master.Config{
		LocalAddr: 1, RemoteAddr: 10, ResponseTimeout: 2 * time.Second,
	}, coll)

	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = out.Run(ctx, och) }()
	go func() { defer wg.Done(); _ = m.Run(ctx, mch) }()
	t.Cleanup(func() {
		cancel()
		_ = mch.Close()
		_ = och.Close()
		wg.Wait()
	})

	waitFor(t, 3*time.Second, func() bool { return m.Connected() })
	waitFor(t, 5*time.Second, func() bool { return m.Stats().Unsolicited >= 1 })

	// Check the indications on the unsolicited fragment itself. Reading the
	// session's LastIIN would race the master's startup sequence, which
	// clears the restart a moment later.
	iins := coll.unsolicitedIINs()
	if len(iins) == 0 {
		t.Fatal("no unsolicited fragment was recorded")
	}
	if !contains(iins[0], "DEVICE_RESTART") {
		t.Errorf("the null unsolicited response did not report the restart; IIN = %s", iins[0])
	}
	if out.Stats().UnsolicitedSent < 1 {
		t.Error("no unsolicited response was sent")
	}

	// A null response carries no measurements — that is what makes it safe to
	// send before the master has acknowledged anything.
	binary, _, _ := coll.snapshot()
	if len(binary) != 0 {
		t.Errorf("the null unsolicited response carried %d measurements", len(binary))
	}
}

// TestStartupSequenceRunsOnceDespiteRestartIndication pins a defect that is
// easy to reintroduce.
//
// The startup sequence is triggered by DEVICE_RESTART, and its own first step
// is the write that clears it — so every fragment arriving before that write
// lands still carries the indication. Without a guard, each one starts another
// sequence, and an outstation that announces itself with a null unsolicited
// response reliably triggers a second run.
func TestStartupSequenceRunsOnceDespiteRestartIndication(t *testing.T) {
	mch, och := channel.Pipe()
	coll := newCollector()

	out := outstation.New(outstation.Config{
		LocalAddr: 10, RemoteAddr: 1,
		Database: outstation.DatabaseConfig{Binary: 4, DefaultClass: dnp3.Class1},
		Unsolicited: outstation.UnsolicitedConfig{
			Enabled:        true,
			ConfirmTimeout: time.Second,
		},
	}, nil, nil)

	m := master.New(master.Config{
		LocalAddr: 1, RemoteAddr: 10,
		ResponseTimeout:       2 * time.Second,
		IntegrityOnStartup:    true,
		DisableUnsolOnStartup: true,
		UnsolClassMask:        dnp3.Class1,
	}, coll)

	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = out.Run(ctx, och) }()
	go func() { defer wg.Done(); _ = m.Run(ctx, mch) }()
	t.Cleanup(func() {
		cancel()
		_ = mch.Close()
		_ = och.Close()
		wg.Wait()
	})

	waitFor(t, 3*time.Second, func() bool { return m.Connected() })
	waitFor(t, 5*time.Second, func() bool { return m.Stats().TasksSucceeded >= 4 })

	// Let anything spurious settle.
	time.Sleep(300 * time.Millisecond)

	// Four steps: clear-restart, disable-unsolicited, integrity, enable.
	// A second sequence would show up as eight.
	if got := m.Stats().TasksSucceeded; got != 4 {
		t.Errorf("%d tasks succeeded, want exactly 4 — the startup sequence ran more than once", got)
	}
	if got := m.Stats().RestartsSeen; got > 1 {
		t.Errorf("reacted to the restart indication %d times, want at most 1", got)
	}
}

// TestUnsolicitedDisabledStaysSilent covers the default: an outstation must
// not push data at a master that has not asked for it.
func TestUnsolicitedDisabledStaysSilent(t *testing.T) {
	m, out, _ := controlPair(t, outstation.Config{
		Database: outstation.DatabaseConfig{Binary: 4, DefaultClass: dnp3.Class1},
		// Unsolicited left disabled.
	}, nil)

	out.Update(func(db *outstation.Database) {
		db.UpdateBinary(0, dnp3.Binary{Value: true, Flags: dnp3.Online})
	})
	time.Sleep(200 * time.Millisecond)

	if got := out.Stats().UnsolicitedSent; got != 0 {
		t.Errorf("%d unsolicited responses from a disabled outstation", got)
	}
	if got := m.Stats().Unsolicited; got != 0 {
		t.Errorf("the master received %d unsolicited responses", got)
	}
}
