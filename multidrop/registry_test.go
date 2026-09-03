package multidrop

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/channel"
	"github.com/dscsystems/go-dnp3/master"
	"github.com/dscsystems/go-dnp3/outstation"
)

// The scenario a Registry exists for: two independent callers, neither aware
// of the other, each constructing what they believe is their own channel for
// the same physical line. Without it, the second Open reproduces the exact
// failure multidrop was built to prevent — two opens of one device — one
// layer up.

// fakeChannel stands in for a real transport channel without touching the
// network or a port, so a test can assert on exactly what a Registry does
// with it: whether it was ever connected, and whether it was closed.
type fakeChannel struct {
	name string

	mu       sync.Mutex
	closed   bool
	connects int
}

func (f *fakeChannel) Connect(ctx context.Context) (io.ReadWriteCloser, error) {
	f.mu.Lock()
	f.connects++
	f.mu.Unlock()
	<-ctx.Done()
	return nil, ctx.Err()
}

func (f *fakeChannel) Close() error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	return nil
}

func (f *fakeChannel) String() string { return f.name }

func (f *fakeChannel) wasClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

func (f *fakeChannel) connectCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connects
}

// TestRegistryReusesAnEquivalentChannel is the reported scenario: a second
// caller building its own channel for a line already open gets the existing
// bus, and its own channel — never needed — is closed rather than left open
// implying it is in use.
func TestRegistryReusesAnEquivalentChannel(t *testing.T) {
	r := NewRegistry()

	first := &fakeChannel{name: "serial /dev/ttyUSB0 9600/8N1"}
	busA := r.Open(first, Config{})
	t.Cleanup(func() { _ = r.Release(busA) })

	second := &fakeChannel{name: "serial /dev/ttyUSB0 9600/8N1"}
	busB := r.Open(second, Config{})
	t.Cleanup(func() { _ = r.Release(busB) })

	if busA != busB {
		t.Fatal("two opens of an equivalent channel returned different buses")
	}
	if r.Len() != 1 {
		t.Errorf("the registry holds %d buses, want 1", r.Len())
	}

	// The redundant channel was never needed and never touched the wire.
	if !second.wasClosed() {
		t.Error("the redundant channel was left open")
	}
	if second.connectCount() != 0 {
		t.Error("the redundant channel was connected; it should never be used")
	}

	// And the one actually in use is untouched by the second caller.
	if first.wasClosed() {
		t.Error("the channel actually in use was closed by a caller that never opened it")
	}
}

// A channel describing a different device gets its own bus.
func TestRegistryGivesDistinctChannelsDistinctBuses(t *testing.T) {
	r := NewRegistry()

	busA := r.Open(&fakeChannel{name: "serial /dev/ttyUSB0 9600/8N1"}, Config{})
	busB := r.Open(&fakeChannel{name: "serial /dev/ttyUSB1 9600/8N1"}, Config{})
	t.Cleanup(func() { _ = r.Release(busA); _ = r.Release(busB) })

	if busA == busB {
		t.Fatal("two different devices were given the same bus")
	}
	if r.Len() != 2 {
		t.Errorf("the registry holds %d buses, want 2", r.Len())
	}
}

// Passing the exact same channel object back to Open — as opposed to a
// different object that merely describes the same line — must not close it:
// it is not redundant, it is the one already in use.
func TestRegistryOpenIsSafeWithTheSameObjectTwice(t *testing.T) {
	r := NewRegistry()

	ch := &fakeChannel{name: "serial /dev/ttyUSB0 9600/8N1"}
	busA := r.Open(ch, Config{})
	busB := r.Open(ch, Config{})
	t.Cleanup(func() { _ = r.Release(busA); _ = r.Release(busB) })

	if busA != busB {
		t.Fatal("opening the same object twice produced different buses")
	}
	if ch.wasClosed() {
		t.Error("the channel actually in use was closed")
	}
}

// The bus is not torn down until every caller that opened it has released it.
func TestRegistryKeepsTheBusUntilTheLastRelease(t *testing.T) {
	r := NewRegistry()

	ch := &fakeChannel{name: "serial /dev/ttyUSB0 9600/8N1"}
	busA := r.Open(ch, Config{})
	busB := r.Open(ch, Config{})

	if err := r.Release(busA); err != nil {
		t.Fatalf("first release: %v", err)
	}
	if ch.wasClosed() {
		t.Error("the channel was closed while a second caller still holds the bus")
	}
	if r.Len() != 1 {
		t.Errorf("the registry holds %d buses after one of two releases, want 1", r.Len())
	}

	if err := r.Release(busB); err != nil {
		t.Fatalf("second release: %v", err)
	}
	if !ch.wasClosed() {
		t.Error("the channel was not closed once every caller released it")
	}
	if r.Len() != 0 {
		t.Errorf("the registry holds %d buses after the last release, want 0", r.Len())
	}
}

// A caller cannot release what it did not open, whether that is a bus this
// registry never issued or one already released to the point of closing.
func TestRegistryReleaseRejectsWhatItDidNotOpen(t *testing.T) {
	r := NewRegistry()

	foreign := New(&fakeChannel{name: "elsewhere"}, Config{})
	if err := r.Release(foreign); err == nil {
		t.Fatal("released a bus this registry never opened")
	}

	ch := &fakeChannel{name: "serial /dev/ttyUSB0 9600/8N1"}
	b := r.Open(ch, Config{})
	if err := r.Release(b); err != nil {
		t.Fatalf("first release: %v", err)
	}
	if err := r.Release(b); err == nil {
		t.Fatal("a second release of an already-closed bus was accepted")
	}
}

// A registered bus can still be reopened for the same channel after every
// caller has released it — the entry is gone, not remembered as permanently
// taken.
func TestRegistryReopensAfterFullRelease(t *testing.T) {
	r := NewRegistry()

	name := "serial /dev/ttyUSB0 9600/8N1"
	first := r.Open(&fakeChannel{name: name}, Config{})
	if err := r.Release(first); err != nil {
		t.Fatalf("release: %v", err)
	}

	second := r.Open(&fakeChannel{name: name}, Config{})
	t.Cleanup(func() { _ = r.Release(second) })

	if first == second {
		t.Error("a bus reused after being fully released and rebuilt should be a new instance")
	}
	if r.Len() != 1 {
		t.Errorf("the registry holds %d buses, want 1", r.Len())
	}
}

// Only the first Open's configuration takes effect; a later Open for the same
// channel does not silently reconfigure a bus other callers depend on.
func TestRegistryConfigIsFixedByTheFirstOpen(t *testing.T) {
	r := NewRegistry()

	ch := "serial /dev/ttyUSB0 9600/8N1"
	first := r.Open(&fakeChannel{name: ch}, Config{Queue: 3})
	second := r.Open(&fakeChannel{name: ch}, Config{Queue: 99})
	t.Cleanup(func() { _ = r.Release(first); _ = r.Release(second) })

	if first != second {
		t.Fatal("expected the same bus")
	}
	if first.cfg.Queue != 3 {
		t.Errorf("queue = %d, want the first caller's 3", first.cfg.Queue)
	}
}

// TestRegistryEndToEnd is the scenario in full: two independently built
// channels claiming the same line, real DNP3 traffic carried over the one bus
// that actually wins, exactly as TestBusCarriesSeveralStations proves for a
// hand-built Bus.
func TestRegistryEndToEnd(t *testing.T) {
	mside, oside := channel.Pipe()

	// Two independent parts of a program each reaching for the master side of
	// the same line without knowing about the other build two distinct
	// channel objects for it — which is the realistic case: nobody hands a
	// live channel to a second caller, they each construct their own. The
	// first to ask wins; its object wraps the real pipe end so a successful
	// poll proves the bus is genuinely carrying traffic over it. The second
	// is a decoy reporting the same identity — what an equivalent
	// independently-built channel would report — and Open must recognise it
	// as redundant without touching the real one still in use.
	r := NewRegistry()
	name := "shared-line"

	real := &namedChannel{Channel: mside, name: name}
	busA := r.Open(real, Config{})

	decoy := &fakeChannel{name: name}
	busB := r.Open(decoy, Config{})
	t.Cleanup(func() { _ = r.Release(busA); _ = r.Release(busB) })

	if decoy.connectCount() != 0 {
		t.Error("the decoy channel was connected; the real one should carry all the traffic")
	}

	if busA != busB {
		t.Fatal("the second caller for the shared line got its own bus")
	}

	obus := New(oside, Config{})
	t.Cleanup(func() { _ = obus.Close() })

	mch, err := busA.Add(Station{LocalAddr: 1, RemoteAddr: 10, Master: true})
	if err != nil {
		t.Fatalf("adding the master station: %v", err)
	}
	och, err := obus.Add(Station{LocalAddr: 10, RemoteAddr: 1})
	if err != nil {
		t.Fatalf("adding the outstation: %v", err)
	}

	out := outstation.New(outstation.Config{
		LocalAddr: 10, RemoteAddr: 1,
		Database: outstation.DatabaseConfig{Analog: 1},
	}, nil, nil)
	out.Update(func(db *outstation.Database) {
		db.UpdateAnalog(0, dnp3.Analog{Value: 42, Flags: dnp3.Online})
	})

	coll := &registryCollector{}
	m := master.New(master.Config{
		LocalAddr: 1, RemoteAddr: 10, ResponseTimeout: 2 * time.Second,
	}, coll)

	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = out.Run(ctx, och) }()
	go func() { defer wg.Done(); _ = m.Run(ctx, mch) }()
	t.Cleanup(func() { cancel(); wg.Wait() })

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !m.Connected() {
		time.Sleep(2 * time.Millisecond)
	}
	if !m.Connected() {
		t.Fatal("the master never connected over the reused bus")
	}

	pollCtx, pollCancel := context.WithTimeout(ctx, 5*time.Second)
	defer pollCancel()
	if err := m.IntegrityPoll(pollCtx); err != nil {
		t.Fatalf("integrity poll: %v", err)
	}
	if got, ok := coll.value(); !ok || got != 42 {
		t.Errorf("read analog 0 = %v (present=%v), want 42", got, ok)
	}
}

// namedChannel wraps a real channel but reports a caller-chosen identity, so
// a test can construct two distinct Go values that a Registry must recognise
// as the same line — which is exactly what happens when two independent
// callers separately build a channel for the same serial device.
type namedChannel struct {
	channel.Channel
	name string
}

func (n *namedChannel) String() string { return n.name }

type registryCollector struct {
	master.NopHandler
	mu  sync.Mutex
	got float64
	ok  bool
}

func (c *registryCollector) HandleAnalog(_ master.HeaderInfo, vs []dnp3.Indexed[dnp3.Analog]) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, v := range vs {
		if v.Index == 0 {
			c.got, c.ok = v.Value.Value, true
		}
	}
}

func (c *registryCollector) value() (float64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.got, c.ok
}
