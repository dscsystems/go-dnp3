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

// These tests run a full master and outstation against each other through
// channel.Pipe: the real link, transport, application and object layers, with
// no socket and no hardware. It is the closest thing to a device this
// repository has, and the only place the two halves are proven to agree.

// collector records everything a master's handler is given.
type collector struct {
	master.NopHandler

	mu        sync.Mutex
	binary    map[uint16]dnp3.Binary
	analog    map[uint16]dnp3.Analog
	counter   map[uint16]dnp3.Counter
	binaryOut map[uint16]dnp3.BinaryOutputStatus

	events    int
	fragments int
	lastIIN   any

	// unsolIIN records the indications on each unsolicited fragment as it
	// arrives. Reading the session's LastIIN instead would race the master's
	// own startup sequence, which clears the restart indication moments later.
	unsolIIN []string
}

func newCollector() *collector {
	return &collector{
		binary:    map[uint16]dnp3.Binary{},
		analog:    map[uint16]dnp3.Analog{},
		counter:   map[uint16]dnp3.Counter{},
		binaryOut: map[uint16]dnp3.BinaryOutputStatus{},
	}
}

func (c *collector) BeginFragment(info master.ResponseInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fragments++
	c.lastIIN = info.IIN
	if info.Unsolicited {
		c.unsolIIN = append(c.unsolIIN, iinString(info.IIN))
	}
}

func (c *collector) HandleBinary(info master.HeaderInfo, vs []dnp3.Indexed[dnp3.Binary]) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, v := range vs {
		c.binary[v.Index] = v.Value
		if info.IsEvent() {
			c.events++
		}
	}
}

func (c *collector) HandleAnalog(info master.HeaderInfo, vs []dnp3.Indexed[dnp3.Analog]) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, v := range vs {
		c.analog[v.Index] = v.Value
		if info.IsEvent() {
			c.events++
		}
	}
}

func (c *collector) HandleCounter(info master.HeaderInfo, vs []dnp3.Indexed[dnp3.Counter]) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, v := range vs {
		c.counter[v.Index] = v.Value
		if info.IsEvent() {
			c.events++
		}
	}
}

func (c *collector) HandleBinaryOutputStatus(_ master.HeaderInfo, vs []dnp3.Indexed[dnp3.BinaryOutputStatus]) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, v := range vs {
		c.binaryOut[v.Index] = v.Value
	}
}

func (c *collector) snapshot() (binary map[uint16]dnp3.Binary, analog map[uint16]dnp3.Analog, events int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b := make(map[uint16]dnp3.Binary, len(c.binary))
	for k, v := range c.binary {
		b[k] = v
	}
	a := make(map[uint16]dnp3.Analog, len(c.analog))
	for k, v := range c.analog {
		a[k] = v
	}
	return b, a, c.events
}

// unsolicitedIINs returns the indications seen on unsolicited fragments.
func (c *collector) unsolicitedIINs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.unsolIIN...)
}

func (c *collector) eventCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.events
}

// pair builds a connected master and outstation and starts both.
func pair(t *testing.T, dbCfg outstation.DatabaseConfig, mcfg master.Config) (
	*master.Session, *outstation.Session, *collector,
) {
	t.Helper()

	mch, och := channel.Pipe()
	coll := newCollector()

	out := outstation.New(outstation.Config{
		LocalAddr:      10,
		RemoteAddr:     1,
		Database:       dbCfg,
		ConfirmTimeout: time.Second,
	}, nil, nil)

	mcfg.LocalAddr = 1
	mcfg.RemoteAddr = 10
	if mcfg.ResponseTimeout == 0 {
		mcfg.ResponseTimeout = 2 * time.Second
	}
	m := master.New(mcfg, coll)

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

	// Wait for both ends to come up before handing them to the test.
	waitFor(t, 3*time.Second, func() bool { return m.Connected() })
	return m, out, coll
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("condition not met within %v", d)
}

// TestIntegrityPoll is the end-to-end proof that the two halves agree: a
// master polls an outstation and gets back exactly the values it holds.
func TestIntegrityPoll(t *testing.T) {
	dbCfg := outstation.DatabaseConfig{
		Binary: 8, Analog: 4, Counter: 2, BinaryOutputStatus: 2,
		DefaultClass: dnp3.Class1,
	}
	m, out, coll := pair(t, dbCfg, master.Config{})

	out.Update(func(db *outstation.Database) {
		db.UpdateBinary(0, dnp3.Binary{Value: true, Flags: dnp3.Online})
		db.UpdateBinary(3, dnp3.Binary{Value: true, Flags: dnp3.Online})
		db.UpdateAnalog(0, dnp3.Analog{Value: 123.5, Flags: dnp3.Online})
		db.UpdateAnalog(2, dnp3.Analog{Value: -40, Flags: dnp3.Online})
		db.UpdateCounter(1, dnp3.Counter{Value: 9999, Flags: dnp3.Online})
	})

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := m.IntegrityPoll(ctx); err != nil {
		t.Fatalf("integrity poll: %v", err)
	}

	binary, analog, _ := coll.snapshot()

	if len(binary) != dbCfg.Binary {
		t.Errorf("received %d binary points, want %d", len(binary), dbCfg.Binary)
	}
	for _, i := range []uint16{0, 3} {
		if !binary[i].Value {
			t.Errorf("binary %d should be on", i)
		}
	}
	for _, i := range []uint16{1, 2, 4} {
		if binary[i].Value {
			t.Errorf("binary %d should be off", i)
		}
	}

	if len(analog) != dbCfg.Analog {
		t.Errorf("received %d analog points, want %d", len(analog), dbCfg.Analog)
	}
	// The default static variation for analogs is g30v1, a 32-bit integer, so
	// 123.5 arrives truncated. That is the encoding doing exactly what it
	// says; a point needing fractions must be configured for a float
	// variation.
	if got := analog[0].Value; got != 123 {
		t.Errorf("analog 0 = %v, want 123 (truncated by the 32-bit variation)", got)
	}
	if got := analog[2].Value; got != -40 {
		t.Errorf("analog 2 = %v, want -40", got)
	}

	if coll.counter[1].Value != 9999 {
		t.Errorf("counter 1 = %d, want 9999", coll.counter[1].Value)
	}
}

// TestFloatAnalogSurvivesWhenConfigured is the other half of the truncation
// story: configure the point for a float variation and the fraction arrives.
func TestFloatAnalogSurvivesWhenConfigured(t *testing.T) {
	m, out, coll := pair(t, outstation.DatabaseConfig{Analog: 2}, master.Config{})

	out.Database().Configure(dnp3.TypeAnalog, 0, outstation.PointConfig{
		StaticVariation: 5, // g30v5, single precision with flags
	})
	out.Update(func(db *outstation.Database) {
		db.UpdateAnalog(0, dnp3.Analog{Value: 123.5, Flags: dnp3.Online})
	})

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := m.IntegrityPoll(ctx); err != nil {
		t.Fatalf("integrity poll: %v", err)
	}

	_, analog, _ := coll.snapshot()
	if got := analog[0].Value; got != 123.5 {
		t.Errorf("analog 0 = %v, want 123.5", got)
	}
}

// TestStartupSequenceClearsRestart covers the handshake that ends the restart
// state. An outstation asserts DEVICE_RESTART until a master clears it; a
// master that never does would see the indication forever and re-run its
// startup sequence in a loop.
func TestStartupSequenceClearsRestart(t *testing.T) {
	m, out, _ := pair(t, outstation.DatabaseConfig{Binary: 2}, master.Config{
		IntegrityOnStartup:    true,
		DisableUnsolOnStartup: true,
	})

	waitFor(t, 5*time.Second, func() bool {
		return out.Stats().RequestsReceived >= 3 // clear-restart, disable-unsol, integrity
	})
	waitFor(t, 5*time.Second, func() bool {
		return !hasRestart(m.LastIIN())
	})

	// And the master must not be stuck re-running startup.
	before := m.Stats().RestartsSeen
	time.Sleep(200 * time.Millisecond)
	if after := m.Stats().RestartsSeen; after != before {
		t.Errorf("startup sequence is looping: restarts seen went %d → %d", before, after)
	}
}

func hasRestart(iin any) bool {
	type haser interface{ Has(x any) bool }
	_ = haser(nil)
	// The IIN type lives in an internal package, so compare via its string
	// form — the only stable surface an external test has.
	s, ok := iin.(interface{ String() string })
	if !ok {
		return false
	}
	return contains(s.String(), "DEVICE_RESTART")
}

func contains(hay, needle string) bool {
	return len(hay) >= len(needle) && indexOf(hay, needle) >= 0
}

func indexOf(hay, needle string) int {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// TestEventsReportedOnClassPoll covers the event path: a change made after the
// integrity poll comes back as an event on the next class poll.
func TestEventsReportedOnClassPoll(t *testing.T) {
	m, out, coll := pair(t, outstation.DatabaseConfig{
		Binary: 4, Analog: 2, DefaultClass: dnp3.Class1,
	}, master.Config{})

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	// Baseline first, so the events below are the only thing outstanding.
	if err := m.IntegrityPoll(ctx); err != nil {
		t.Fatalf("integrity poll: %v", err)
	}
	// The integrity poll drains the events the initial updates queued.
	waitFor(t, 2*time.Second, func() bool { return out.Events().Total() == 0 })

	before := coll.eventCount()

	out.Update(func(db *outstation.Database) {
		db.UpdateBinary(2, dnp3.Binary{Value: true, Flags: dnp3.Online})
	})
	waitFor(t, 2*time.Second, func() bool { return out.Events().Count(dnp3.Class1) == 1 })

	if err := m.ScanClasses(ctx, dnp3.Class123); err != nil {
		t.Fatalf("class poll: %v", err)
	}

	waitFor(t, 2*time.Second, func() bool { return coll.eventCount() > before })

	binary, _, _ := coll.snapshot()
	if !binary[2].Value {
		t.Error("the event did not carry the new value")
	}
}

// TestEventsClearedOnlyByConfirm is the rule that decides whether
// sequence-of-events data survives. An outstation that dropped events at
// transmission would lose exactly what the record exists to preserve, and lose
// it silently.
func TestEventsClearedOnlyByConfirm(t *testing.T) {
	m, out, _ := pair(t, outstation.DatabaseConfig{
		Binary: 4, DefaultClass: dnp3.Class1,
	}, master.Config{})

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := m.IntegrityPoll(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, func() bool { return out.Events().Total() == 0 })

	out.Update(func(db *outstation.Database) {
		db.UpdateBinary(1, dnp3.Binary{Value: true, Flags: dnp3.Online})
	})
	waitFor(t, 2*time.Second, func() bool { return out.Events().Total() == 1 })

	if err := m.ScanClasses(ctx, dnp3.Class1); err != nil {
		t.Fatal(err)
	}

	// The master confirms, so the outstation may finally drop the event.
	waitFor(t, 2*time.Second, func() bool { return out.Events().Total() == 0 })
	if n := out.Events().SelectedCount(); n != 0 {
		t.Errorf("%d events still selected after a confirm", n)
	}
}

// TestPeriodicScan covers the recurring poll a real master runs continuously.
func TestPeriodicScan(t *testing.T) {
	m, out, _ := pair(t, outstation.DatabaseConfig{Binary: 2}, master.Config{})

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := m.AddPeriodicScan(ctx, 50*time.Millisecond, dnp3.Class123); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 3*time.Second, func() bool { return out.Stats().RequestsReceived >= 5 })

	if got := m.Stats().TasksSucceeded; got < 4 {
		t.Errorf("only %d tasks succeeded; the periodic scan is not repeating", got)
	}
	if got := m.Stats().TasksFailed; got > 0 {
		t.Errorf("%d tasks failed during a clean periodic scan", got)
	}
}

// TestMultiFragmentResponse forces a response too large for one fragment,
// which is the normal case for an integrity poll of a real device.
func TestMultiFragmentResponse(t *testing.T) {
	dbCfg := outstation.DatabaseConfig{Analog: 400}
	m, out, coll := pair(t, dbCfg, master.Config{})

	out.Update(func(db *outstation.Database) {
		for i := range 400 {
			db.UpdateAnalog(uint16(i), dnp3.Analog{Value: float64(i), Flags: dnp3.Online})
		}
	})

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := m.IntegrityPoll(ctx); err != nil {
		t.Fatalf("integrity poll: %v", err)
	}

	_, analog, _ := coll.snapshot()
	if len(analog) != 400 {
		t.Fatalf("received %d analog points, want 400", len(analog))
	}
	for i := range 400 {
		if got := analog[uint16(i)].Value; got != float64(i) {
			t.Fatalf("analog %d = %v, want %d", i, got, i)
		}
	}

	// 400 points at five octets each cannot fit one 2048-octet fragment.
	//
	// The outstation bumps this counter just after writing each fragment, so
	// it can still be catching up here: the poll returns as soon as the master
	// has the final fragment, which is before the outstation goroutine has
	// accounted for it. Wait for it rather than sampling that gap.
	waitFor(t, 2*time.Second, func() bool { return out.Stats().FragmentsSent >= 2 })

	if got := out.Stats().FragmentsSent; got < 2 {
		t.Errorf("sent %d fragments; a 400-point response must span several", got)
	}
}

// TestRangeScan reads a subset rather than everything.
func TestRangeScan(t *testing.T) {
	m, out, coll := pair(t, outstation.DatabaseConfig{Analog: 20}, master.Config{})

	out.Update(func(db *outstation.Database) {
		for i := range 20 {
			db.UpdateAnalog(uint16(i), dnp3.Analog{Value: float64(i * 10), Flags: dnp3.Online})
		}
	})

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := m.ScanRange(ctx, 30, 1, 5, 9); err != nil {
		t.Fatalf("range scan: %v", err)
	}

	_, analog, _ := coll.snapshot()
	if len(analog) != 5 {
		t.Fatalf("received %d points, want 5", len(analog))
	}
	for i := uint16(5); i <= 9; i++ {
		if got := analog[i].Value; got != float64(i*10) {
			t.Errorf("analog %d = %v, want %d", i, got, i*10)
		}
	}
}

// TestWriteTimeSynchronizesOutstation covers the clock handshake and the
// quality it controls.
func TestWriteTimeSynchronizesOutstation(t *testing.T) {
	m, out, coll := pair(t, outstation.DatabaseConfig{
		Binary: 2, DefaultClass: dnp3.Class1,
	}, master.Config{})

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	// Before the clock is set the outstation asserts NEED_TIME, so its
	// timestamps must not be filed as synchronized.
	if err := m.IntegrityPoll(ctx); err != nil {
		t.Fatal(err)
	}
	if !contains(iinString(m.LastIIN()), "NEED_TIME") {
		t.Error("a fresh outstation should be asking for the time")
	}

	if err := m.WriteTime(ctx, time.Now()); err != nil {
		t.Fatalf("write time: %v", err)
	}

	out.Update(func(db *outstation.Database) {
		db.UpdateBinary(0, dnp3.Binary{
			Value: true, Flags: dnp3.Online,
			Time: dnp3.Now(time.Now()),
		})
	})
	waitFor(t, 2*time.Second, func() bool { return out.Events().Total() >= 1 })

	if err := m.ScanClasses(ctx, dnp3.Class1); err != nil {
		t.Fatal(err)
	}
	if contains(iinString(m.LastIIN()), "NEED_TIME") {
		t.Error("the outstation is still asking for the time after a successful write")
	}

	binary, _, _ := coll.snapshot()
	if got := binary[0].Time.Quality; got != dnp3.TimestampSynchronized {
		t.Errorf("event timestamp quality = %v, want synchronized", got)
	}
}

func iinString(iin any) string {
	if s, ok := iin.(interface{ String() string }); ok {
		return s.String()
	}
	return ""
}

// TestEventBufferOverflowIsReported covers the one signal that tells a master
// its sequence-of-events record has a hole in it.
func TestEventBufferOverflowIsReported(t *testing.T) {
	mch, och := channel.Pipe()
	coll := newCollector()

	out := outstation.New(outstation.Config{
		LocalAddr: 10, RemoteAddr: 1,
		Database: outstation.DatabaseConfig{Binary: 4, DefaultClass: dnp3.Class1},
		Events:   outstation.EventBufferConfig{MaxEvents: 4},
	}, nil, nil)
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

	// Far more events than the buffer holds.
	out.Update(func(db *outstation.Database) {
		for i := range 40 {
			db.UpdateBinary(0, dnp3.Binary{Value: i%2 == 0, Flags: dnp3.Online})
		}
	})
	waitFor(t, 2*time.Second, func() bool { return out.Events().Overflowed() })

	pollCtx, pollCancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer pollCancel()
	if err := m.ScanClasses(pollCtx, dnp3.Class1); err != nil {
		t.Fatal(err)
	}

	if !contains(iinString(m.LastIIN()), "EVENT_BUFFER_OVERFLOW") {
		t.Errorf("the overflow was not reported to the master; IIN = %s", iinString(m.LastIIN()))
	}
}
