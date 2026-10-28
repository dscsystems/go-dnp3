//go:build interop

package otfabric

import (
	"bytes"
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/channel"
	"github.com/dscsystems/go-dnp3/master"
)

// Our master against the reference outstations. Each test starts its own
// container: the controls and time writes change an outstation, and the
// fixture is only the starting state.

// collector records the static values a master receives.
type collector struct {
	master.NopHandler

	mu        sync.Mutex
	binary    map[uint32]dnp3.Binary
	doubleBit map[uint32]dnp3.DoubleBitBinary
	binaryOut map[uint32]dnp3.BinaryOutputStatus
	counter   map[uint32]dnp3.Counter
	frozen    map[uint32]dnp3.FrozenCounter
	analog    map[uint32]dnp3.Analog
	analogOut map[uint32]dnp3.AnalogOutputStatus
	octets    map[uint32]dnp3.OctetString
	events    int
}

func newCollector() *collector {
	return &collector{
		binary:    map[uint32]dnp3.Binary{},
		doubleBit: map[uint32]dnp3.DoubleBitBinary{},
		binaryOut: map[uint32]dnp3.BinaryOutputStatus{},
		counter:   map[uint32]dnp3.Counter{},
		frozen:    map[uint32]dnp3.FrozenCounter{},
		analog:    map[uint32]dnp3.Analog{},
		analogOut: map[uint32]dnp3.AnalogOutputStatus{},
		octets:    map[uint32]dnp3.OctetString{},
	}
}

// store keeps static values and counts events.
func store[T any](c *collector, into map[uint32]T, info master.HeaderInfo, vs []dnp3.Indexed[T]) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if info.IsEvent() {
		c.events += len(vs)
		return
	}
	for _, v := range vs {
		into[v.Index] = v.Value
	}
}

func (c *collector) HandleBinary(i master.HeaderInfo, vs []dnp3.Indexed[dnp3.Binary]) {
	store(c, c.binary, i, vs)
}
func (c *collector) HandleDoubleBit(i master.HeaderInfo, vs []dnp3.Indexed[dnp3.DoubleBitBinary]) {
	store(c, c.doubleBit, i, vs)
}
func (c *collector) HandleBinaryOutputStatus(i master.HeaderInfo, vs []dnp3.Indexed[dnp3.BinaryOutputStatus]) {
	store(c, c.binaryOut, i, vs)
}
func (c *collector) HandleCounter(i master.HeaderInfo, vs []dnp3.Indexed[dnp3.Counter]) {
	store(c, c.counter, i, vs)
}
func (c *collector) HandleFrozenCounter(i master.HeaderInfo, vs []dnp3.Indexed[dnp3.FrozenCounter]) {
	store(c, c.frozen, i, vs)
}
func (c *collector) HandleAnalog(i master.HeaderInfo, vs []dnp3.Indexed[dnp3.Analog]) {
	store(c, c.analog, i, vs)
}
func (c *collector) HandleAnalogOutputStatus(i master.HeaderInfo, vs []dnp3.Indexed[dnp3.AnalogOutputStatus]) {
	store(c, c.analogOut, i, vs)
}
func (c *collector) HandleOctetString(i master.HeaderInfo, vs []dnp3.Indexed[dnp3.OctetString]) {
	store(c, c.octets, i, vs)
}

// connect points our master at a reference outstation. The master does
// nothing on its own: no startup poll, no unsolicited changes.
func connect(t *testing.T, port string, fx *fixture) (*master.Session, *collector) {
	t.Helper()

	coll := newCollector()
	m := master.New(master.Config{
		LocalAddr:       fx.Outstation.MasterAddress,
		RemoteAddr:      fx.Outstation.Address,
		ResponseTimeout: 5 * time.Second,
	}, coll)

	ch := channel.TCPClient("127.0.0.1:"+port, channel.DefaultRetry)
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = m.Run(ctx, ch) }()
	t.Cleanup(func() {
		cancel()
		_ = ch.Close()
		wg.Wait()
	})

	for deadline := time.Now().Add(20 * time.Second); !m.Connected(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("never connected")
		}
	}
	return m, coll
}

// needsTime reports whether the last response asked for the time.
func needsTime(m *master.Session) bool {
	return strings.Contains(m.LastIIN().String(), "NEED_TIME")
}

func timeout(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// eachServer runs the test against every selected reference outstation.
func eachServer(t *testing.T, fn func(t *testing.T, a *adapter, s *refServer)) {
	for _, a := range selected() {
		t.Run(a.name, func(t *testing.T) {
			s := a.startServer(t)
			fn(t, a, s)
		})
	}
}

// TestMasterIntegrityPoll reads the whole database and compares every point,
// value and quality, with the fixture the image was built from.
func TestMasterIntegrityPoll(t *testing.T) {
	eachServer(t, func(t *testing.T, a *adapter, s *refServer) {
		m, coll := connect(t, s.port, a.fix)
		if err := m.IntegrityPoll(timeout(t)); err != nil {
			t.Fatalf("integrity poll: %v", err)
		}

		coll.mu.Lock()
		defer coll.mu.Unlock()
		fx := a.fix
		// The state of a binary input rides in the top bit of its flags
		// octet, and a double-bit input's in the top two.
		const (
			stateBit       = dnp3.StateBit
			doubleBitState = dnp3.Flags(0xC0)
		)

		for _, p := range fx.BinaryInputs {
			got, ok := coll.binary[uint32(p.Index)]
			if !ok || got.Value != p.bool() || got.Flags&^stateBit != p.flags() {
				t.Errorf("binary input %d: got %+v (present %v), want %v flags %v", p.Index, got, ok, p.bool(), p.flags())
			}
		}
		for _, p := range fx.DoubleBitInputs {
			got, ok := coll.doubleBit[uint32(p.Index)]
			if !ok || float64(got.Value) != p.number() || got.Flags&^doubleBitState != p.flags() {
				t.Errorf("double-bit input %d: got %+v (present %v), want %v flags %v", p.Index, got, ok, p.number(), p.flags())
			}
		}
		for _, p := range fx.BinaryOutputs {
			got, ok := coll.binaryOut[uint32(p.Index)]
			if !ok || got.Value != p.bool() || got.Flags&^stateBit != p.flags() {
				t.Errorf("binary output %d: got %+v (present %v), want %v flags %v", p.Index, got, ok, p.bool(), p.flags())
			}
		}
		for _, p := range fx.Counters {
			got, ok := coll.counter[uint32(p.Index)]
			if !ok || float64(got.Value) != p.number() || got.Flags != p.flags() {
				t.Errorf("counter %d: got %+v (present %v), want %v flags %v", p.Index, got, ok, p.number(), p.flags())
			}
		}
		for _, p := range fx.FrozenCounters {
			got, ok := coll.frozen[uint32(p.Index)]
			if !ok || float64(got.Value) != p.number() || got.Flags != p.flags() {
				t.Errorf("frozen counter %d: got %+v (present %v), want %v flags %v", p.Index, got, ok, p.number(), p.flags())
			}
		}
		for _, p := range fx.AnalogInputs {
			got, ok := coll.analog[uint32(p.Index)]
			if !ok || got.Value != p.number() || got.Flags != p.flags() {
				t.Errorf("analog input %d: got %+v (present %v), want %v flags %v", p.Index, got, ok, p.number(), p.flags())
			}
		}
		for _, p := range fx.AnalogOutputs {
			got, ok := coll.analogOut[uint32(p.Index)]
			if !ok || got.Value != p.number() || got.Flags != p.flags() {
				t.Errorf("analog output %d: got %+v (present %v), want %v flags %v", p.Index, got, ok, p.number(), p.flags())
			}
		}
		for _, p := range fx.OctetStrings {
			got, ok := coll.octets[uint32(p.Index)]
			if !ok || !bytes.Equal(got, p.octets()) {
				t.Errorf("octet string %d: got %x (present %v), want %x", p.Index, []byte(got), ok, p.octets())
			}
		}

		if st := m.Stats(); st.TasksFailed > 0 || st.ResponseTimeout > 0 {
			t.Errorf("%d task failures, %d timeouts", st.TasksFailed, st.ResponseTimeout)
		}
		if fx.Outstation.NeedTime && !needsTime(m) {
			t.Errorf("the fixture asks for the time, the outstation's IIN does not: %s", m.LastIIN())
		}
	})
}

// TestMasterRangeReads reads each static group by range, the other way a
// master asks for data, and checks nothing is rejected.
func TestMasterRangeReads(t *testing.T) {
	eachServer(t, func(t *testing.T, a *adapter, s *refServer) {
		m, coll := connect(t, s.port, a.fix)
		ctx := timeout(t)

		type read struct {
			group, variation uint8
			count            int
		}
		fx := a.fix
		for _, r := range []read{
			{1, 2, len(fx.BinaryInputs)},
			{20, 5, len(fx.Counters)},
			{30, 1, len(fx.AnalogInputs)},
			{30, 5, len(fx.AnalogInputs)},
			{40, 1, len(fx.AnalogOutputs)},
		} {
			if r.count == 0 {
				continue
			}
			if err := m.ScanRange(ctx, r.group, r.variation, 0, uint16(r.count-1)); err != nil {
				t.Errorf("range read g%dv%d [0,%d]: %v", r.group, r.variation, r.count-1, err)
			}
		}

		// A range ending past the last point is a request for points that do
		// not exist: the outstation may answer with what it has or refuse with
		// PARAMETER_ERROR, but it must answer.
		err := m.ScanRange(ctx, 30, 1, 0, uint16(len(fx.AnalogInputs)+5))
		if errors.Is(err, dnp3.ErrTimeout) {
			t.Errorf("no answer to a range past the last point: %v", err)
		}

		coll.mu.Lock()
		defer coll.mu.Unlock()
		if len(fx.AnalogInputs) > 0 && len(coll.analog) < len(fx.AnalogInputs) {
			t.Errorf("read %d analog inputs, want %d", len(coll.analog), len(fx.AnalogInputs))
		}
	})
}

// TestMasterControls runs both control sequences against an output that
// requires a select and one that does not, and the outstation's own account
// of what it received.
func TestMasterControls(t *testing.T) {
	eachServer(t, func(t *testing.T, a *adapter, s *refServer) {
		m, _ := connect(t, s.port, a.fix)
		ctx := timeout(t)
		fx := a.fix

		direct := firstOutput(fx.BinaryOutputs, false)
		sbo := firstOutput(fx.BinaryOutputs, true)
		if direct == nil || sbo == nil {
			t.Skip("the fixture has no binary outputs of both kinds")
		}

		if res, err := m.DirectOperate(ctx, master.LatchOn(direct.Index)); err != nil || !res.OK() {
			t.Errorf("direct operate latch on %d: %v %v", direct.Index, res.Statuses, err)
		}
		if res, err := m.SelectAndOperate(ctx, master.LatchOn(sbo.Index)); err != nil || !res.OK() {
			t.Errorf("select-before-operate latch on %d: %v %v", sbo.Index, res.Statuses, err)
		}
		if res, err := m.SelectAndOperate(ctx, master.LatchOff(sbo.Index)); err != nil || !res.OK() {
			t.Errorf("select-before-operate latch off %d: %v %v", sbo.Index, res.Statuses, err)
		}

		// What the outstation says it received is what went on the wire.
		if len(s.events(t)) == 0 || s.events(t)[0].str("event") != "ready" {
			t.Fatalf("no ready event: %v", s.events(t))
		}
		wantEvents := []struct {
			function string
			index    uint16
			code     float64
		}{
			{"direct-operate", direct.Index, 3},
			{"select", sbo.Index, 3},
			{"operate", sbo.Index, 3},
			{"select", sbo.Index, 4},
			{"operate", sbo.Index, 4},
		}
		var got []event
		for _, e := range s.events(t) {
			if e.str("event") == "command" {
				got = append(got, e)
			}
		}
		if len(got) != len(wantEvents) {
			t.Fatalf("outstation saw %d commands, want %d: %v", len(got), len(wantEvents), got)
		}
		for i, w := range wantEvents {
			e := got[i]
			if e.str("function") != w.function || e.str("type") != "g12v1" ||
				e["index"] != float64(w.index) || e["code"] != w.code || e.str("status") != "SUCCESS" {
				t.Errorf("command %d: outstation saw %v, want %+v", i, e, w)
			}
		}
	})
}

// TestMasterAnalogControls covers the analog output encodings and a value
// the outstation must refuse.
func TestMasterAnalogControls(t *testing.T) {
	eachServer(t, func(t *testing.T, a *adapter, s *refServer) {
		m, _ := connect(t, s.port, a.fix)
		ctx := timeout(t)

		direct := firstOutput(a.fix.AnalogOutputs, false)
		sbo := firstOutput(a.fix.AnalogOutputs, true)
		if direct == nil || sbo == nil {
			t.Skip("the fixture has no analog outputs of both kinds")
		}

		if res, err := m.DirectOperate(ctx, master.AnalogOutputFloat32(direct.Index, 42.5)); err != nil || !res.OK() {
			t.Errorf("direct operate float32: %v %v", res.Statuses, err)
		}
		if res, err := m.DirectOperate(ctx, master.AnalogOutputInt32(direct.Index, -123456)); err != nil || !res.OK() {
			t.Errorf("direct operate int32: %v %v", res.Statuses, err)
		}
		if res, err := m.DirectOperate(ctx, master.AnalogOutputFloat64(direct.Index, 0.25)); err != nil || !res.OK() {
			t.Errorf("direct operate float64: %v %v", res.Statuses, err)
		}
		if res, err := m.SelectAndOperate(ctx, master.AnalogOutputInt16(sbo.Index, 50)); err != nil || !res.OK() {
			t.Errorf("select-before-operate int16: %v %v", res.Statuses, err)
		}

		// Outside the output's range: refused by status, and as a failed
		// command, never as a success.
		over := int16(math.MaxInt16)
		if sbo.Max != nil && *sbo.Max < float64(over) {
			res, err := m.DirectOperate(ctx, master.AnalogOutputInt16(direct.Index, 0)) // sanity: still works
			if err != nil || !res.OK() {
				t.Errorf("direct operate after the others: %v %v", res.Statuses, err)
			}
			res, err = m.SelectAndOperate(ctx, master.AnalogOutputInt16(sbo.Index, over))
			if err == nil || res.OK() {
				t.Errorf("a value of %d on an output limited to %v was accepted: %v", over, *sbo.Max, res.Statuses)
			}
		}
	})
}

// TestMasterTimeSync runs both synchronisation procedures and checks the
// outstation saw a time write for each.
func TestMasterTimeSync(t *testing.T) {
	eachServer(t, func(t *testing.T, a *adapter, s *refServer) {
		if !a.caps.has("timeSyncLan") {
			t.Skip("the outstation does not take the LAN procedure")
		}
		m, _ := connect(t, s.port, a.fix)
		ctx := timeout(t)

		if err := m.SyncTime(ctx); err != nil {
			t.Errorf("LAN clock write: %v", err)
		}
		if a.caps.has("timeSyncNonLan") {
			if err := m.SyncTimeWithDelay(ctx); err != nil {
				t.Errorf("delay-measured clock write: %v", err)
			}
		}
		if err := m.SyncTimeRecorded(ctx); err != nil {
			t.Errorf("recorded-time clock write: %v", err)
		}

		writes := 0
		for _, e := range s.events(t) {
			if e.str("event") == "time-sync" {
				writes++
			}
		}
		if writes == 0 {
			t.Errorf("the outstation reported no time write: %v", s.events(t))
		}

		// A synchronised outstation stops asking for the time.
		if err := m.ScanClasses(ctx, dnp3.Class0); err != nil {
			t.Fatal(err)
		}
		if needsTime(m) {
			t.Errorf("NEED_TIME still set after the clock was written: %s", m.LastIIN())
		}
	})
}

// TestMasterRestart asks for both restarts and checks the outstation took them.
func TestMasterRestart(t *testing.T) {
	eachServer(t, func(t *testing.T, a *adapter, s *refServer) {
		if !a.caps.has("restart") {
			t.Skip("the outstation does not take restarts")
		}
		m, _ := connect(t, s.port, a.fix)
		ctx := timeout(t)

		for _, mode := range []dnp3.RestartMode{dnp3.ColdRestart, dnp3.WarmRestart} {
			if err := m.Restart(ctx, mode); err != nil {
				t.Errorf("%v restart: %v", mode, err)
			}
		}

		var types []string
		for _, e := range s.events(t) {
			if e.str("event") == "restart" {
				types = append(types, e.str("type"))
			}
		}
		if len(types) != 2 || types[0] != "cold" || types[1] != "warm" {
			t.Errorf("outstation saw restarts %v, want [cold warm]", types)
		}
	})
}

// TestMasterEventPolls polls the event classes, enables and disables
// unsolicited reporting, and freezes counters: the remaining requests a
// master makes of any outstation. None of them may fail.
func TestMasterEventPolls(t *testing.T) {
	eachServer(t, func(t *testing.T, a *adapter, s *refServer) {
		m, _ := connect(t, s.port, a.fix)
		ctx := timeout(t)

		for range 3 {
			if err := m.ScanClasses(ctx, dnp3.Class123); err != nil {
				t.Fatalf("class poll: %v", err)
			}
		}
		for _, c := range []dnp3.Class{dnp3.Class1, dnp3.Class2, dnp3.Class3} {
			if err := m.ScanClasses(ctx, c); err != nil {
				t.Errorf("poll of %v: %v", c, err)
			}
		}
		if a.caps.has("unsolicited") {
			if err := m.EnableUnsolicited(ctx, dnp3.Class123); err != nil {
				t.Errorf("enable unsolicited: %v", err)
			}
			if err := m.DisableUnsolicited(ctx, dnp3.Class123); err != nil {
				t.Errorf("disable unsolicited: %v", err)
			}
		}
		if a.caps.has("freeze") && len(a.fix.Counters) > 0 {
			if err := m.FreezeCounters(ctx, false); err != nil {
				t.Errorf("freeze: %v", err)
			}
			if err := m.FreezeCounters(ctx, true); err != nil {
				t.Errorf("freeze and clear: %v", err)
			}
		}
		if st := m.Stats(); st.TasksFailed > 0 {
			t.Errorf("%d tasks failed", st.TasksFailed)
		}
	})
}

// TestMasterSeesControlEvents makes an output change a point that reports
// events, and checks the change arrives on a class poll: the path from a
// control to an event to a master, across two implementations.
func TestMasterSeesControlEvents(t *testing.T) {
	eachServer(t, func(t *testing.T, a *adapter, s *refServer) {
		fx := a.fix
		out := firstOutput(fx.BinaryOutputs, false)
		if out == nil || out.Target == nil {
			t.Skip("the fixture has no binary output that drives an input")
		}
		target := find(fx.BinaryInputs, *out.Target)
		if target == nil || target.Class == 0 {
			t.Skip("the output's target reports no events")
		}

		m, coll := connect(t, s.port, fx)
		ctx := timeout(t)

		// Drain whatever the fixture starts with.
		if err := m.IntegrityPoll(ctx); err != nil {
			t.Fatal(err)
		}
		coll.mu.Lock()
		before := coll.events
		coll.mu.Unlock()

		// Move the target away from its current value.
		cmd := master.LatchOff(out.Index)
		if !target.bool() {
			cmd = master.LatchOn(out.Index)
		}
		if res, err := m.DirectOperate(ctx, cmd); err != nil || !res.OK() {
			t.Fatalf("operate: %v %v", res.Statuses, err)
		}

		deadline := time.Now().Add(10 * time.Second)
		for {
			if err := m.ScanClasses(ctx, dnp3.Class123); err != nil {
				t.Fatal(err)
			}
			coll.mu.Lock()
			n := coll.events
			coll.mu.Unlock()
			if n > before {
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("no event arrived after the control moved a point that reports events")
			}
			time.Sleep(200 * time.Millisecond)
		}
	})
}
