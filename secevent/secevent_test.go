package secevent

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dscsystems/go-dnp3"
)

type collect struct {
	mu sync.Mutex
	ev []Event
}

func (c *collect) HandleEvent(e Event) { c.mu.Lock(); c.ev = append(c.ev, e); c.mu.Unlock() }
func (c *collect) events() []Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Event(nil), c.ev...)
}

func TestEveryKindHasDefaults(t *testing.T) {
	for _, k := range Kinds() {
		if k.Category() == 0 || k.DefaultSeverity() == 0 {
			t.Errorf("%s has no category or severity", k)
		}
		prefix, _, ok := strings.Cut(string(k), ".")
		if !ok || prefix == "" {
			t.Errorf("%s is not a dotted code", k)
		}
	}
	if len(Kinds()) < 150 {
		t.Fatalf("catalogue has %d kinds; it lost some", len(Kinds()))
	}
}

func TestNilEmitterIsSafe(t *testing.T) {
	var em *Emitter
	em.Emit(Event{Kind: KindConnClosed})
	if em.Enabled() || em.ForConn(1, PeerInfo{}) != nil || em.ConnID() != 0 {
		t.Fatal("a nil emitter is not inert")
	}
	if NewEmitter(nil, "outstation", 0) != nil {
		t.Fatal("an emitter without a dispatcher should be nil")
	}
	var p *Policy
	if em, done := p.Open("outstation"); em != nil {
		t.Fatal("a nil policy produced an emitter")
	} else {
		done()
	}
}

func TestEmitterStampsDefaultsAndIdentity(t *testing.T) {
	c := &collect{}
	d := NewDispatcher(SyncSink(c), DispatcherOptions{})
	defer d.Close()
	peer := PeerInfo{Network: "tcp"}
	em := NewEmitter(d, "outstation", 4).ForConn(7, peer)
	raw := []byte{1, 2, 3, 4, 5, 6}
	em.Emit(Event{Kind: KindCtrlColdRestart, Raw: raw})
	raw[0] = 99

	got := c.events()
	if len(got) != 1 {
		t.Fatalf("got %d events", len(got))
	}
	e := got[0]
	if e.Category != CatControl || e.Severity != Alert || e.Technique != "T0816" ||
		e.Role != "outstation" || e.ConnID != 7 || e.Transport != "tcp" || e.Protocol != "dnp3" ||
		e.Time.IsZero() || e.Count != 1 {
		t.Fatalf("event not stamped: %+v", e)
	}
	if !bytes.Equal(e.Raw, []byte{1, 2, 3, 4}) {
		t.Fatalf("Raw = %v, want a truncated copy", e.Raw)
	}
}

func TestDispatcherAggregatesRepeats(t *testing.T) {
	c := &collect{}
	d := NewDispatcher(SyncSink(c), DispatcherOptions{AggregateWindow: 100 * time.Millisecond})
	em := NewEmitter(d, "outstation", 0).ForConn(1, PeerInfo{})
	for range 5 {
		em.Emit(Event{Kind: KindLinkCRCError})
	}
	// A different key is a different event.
	em.Emit(Event{Kind: KindAccessUnknownPoint, Fields: HasIndex, Index: 1})
	em.Emit(Event{Kind: KindAccessUnknownPoint, Fields: HasIndex, Index: 2})
	if n := len(c.events()); n != 3 {
		t.Fatalf("before the window closes: %d events, want 3", n)
	}
	time.Sleep(250 * time.Millisecond)
	got := c.events()
	if len(got) != 4 || got[3].Kind != KindLinkCRCError || got[3].Count != 4 || got[3].FirstSeen.IsZero() {
		t.Fatalf("after the window: %+v", got)
	}
	_ = d.Close()
	st := d.Stats()
	if st.ByKind[KindLinkCRCError] != 5 || st.Aggregated != 4 || st.Delivered != 4 {
		t.Fatalf("stats = %+v", st)
	}
}

func TestAlwaysDeliverKindsAreNotAggregated(t *testing.T) {
	c := &collect{}
	d := NewDispatcher(SyncSink(c), DispatcherOptions{})
	defer d.Close()
	em := NewEmitter(d, "outstation", 0)
	for range 3 {
		em.Emit(Event{Kind: KindAuthFailure})
	}
	if n := len(c.events()); n != 3 {
		t.Fatalf("%d events, want 3", n)
	}
}

func TestCloseFlushesAggregates(t *testing.T) {
	c := &collect{}
	d := NewDispatcher(c, DispatcherOptions{AggregateWindow: time.Hour})
	em := NewEmitter(d, "outstation", 0)
	em.Emit(Event{Kind: KindLinkGarbage})
	em.Emit(Event{Kind: KindLinkGarbage})
	_ = d.Close()
	got := c.events()
	if len(got) != 2 || got[1].Count != 1 {
		t.Fatalf("after close: %+v", got)
	}
	if d.Submit(Event{Kind: KindLinkGarbage}) {
		t.Fatal("submit after close was accepted")
	}
}

// A sink slower than the event rate costs events, counted and reported, never
// a blocked submitter.
func TestSlowSinkDropsAndReports(t *testing.T) {
	release := make(chan struct{})
	c := &collect{}
	sink := SinkFunc(func(e Event) {
		if e.Kind == KindConnAccepted {
			<-release
		}
		c.HandleEvent(e)
	})
	d := NewDispatcher(sink, DispatcherOptions{QueueSize: 2, AggregateWindow: -1})
	em := NewEmitter(d, "outstation", 0)
	em.Emit(Event{Kind: KindConnAccepted}) // the sink blocks on this one
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	for range 50 {
		em.Emit(Event{Kind: KindConnClosed})
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("emitting blocked on a slow sink")
	}
	close(release)
	_ = d.Close()
	var dropped *Event
	for _, e := range c.events() {
		if e.Kind == KindMetaEventsDropped {
			dropped = &e
		}
	}
	if dropped == nil || dropped.Count != 48 {
		t.Fatalf("drop report = %+v", dropped)
	}
	if d.Stats().Dropped != 48 {
		t.Fatalf("stats dropped = %d", d.Stats().Dropped)
	}
}

func TestSinkPanicIsRecoveredAndReportedOnce(t *testing.T) {
	var kinds []Kind
	sink := SinkFunc(func(e Event) {
		kinds = append(kinds, e.Kind)
		if e.Kind == KindConnClosed {
			panic("boom")
		}
	})
	d := NewDispatcher(SyncSink(sink), DispatcherOptions{AggregateWindow: -1})
	em := NewEmitter(d, "outstation", 0)
	em.Emit(Event{Kind: KindConnClosed})
	em.Emit(Event{Kind: KindConnClosed})
	em.Emit(Event{Kind: KindConnAccepted})
	_ = d.Close()
	want := []Kind{KindConnClosed, KindMetaSinkPanic, KindConnClosed, KindConnAccepted}
	if len(kinds) != len(want) {
		t.Fatalf("kinds = %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("kinds = %v, want %v", kinds, want)
		}
	}
	if d.Stats().SinkPanics != 2 {
		t.Fatalf("panics = %d", d.Stats().SinkPanics)
	}
}

func TestJSONIsStable(t *testing.T) {
	e := Event{
		Time: time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC), Kind: KindCmdRejected,
		Category: CatCommand, Severity: Notice, Action: Rejected, Protocol: "dnp3",
		Role: "outstation", Transport: "tcp", ConnID: 3,
		Peer:   PeerInfo{Network: "tcp", MAC: net.HardwareAddr{0, 1, 2, 3, 4, 5}},
		Fields: HasIndex | HasCommandStatus | HasAppFunc, Index: 4, AppFunc: 5,
		CommandStatus: dnp3.CommandNotSupported, Detail: "no such point", Count: 1,
	}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"time":"2026-11-21T12:00:00Z","kind":"cmd.rejected","category":"command","severity":"notice","action":"rejected","protocol":"dnp3","role":"outstation","transport":"tcp","conn_id":3,"peer":{"network":"tcp","mac":"00:01:02:03:04:05"},"app_func":"DIRECT_OPERATE","index":4,"command_status":"` +
		"NOT_SUPPORTED" + `","detail":"no such point"}`
	if string(b) != want {
		t.Fatalf("JSON drifted:\n got %s\nwant %s", b, want)
	}
}

func TestSinksFilterAndFanOut(t *testing.T) {
	a, b := &collect{}, &collect{}
	s := MultiSink(a, FilterSink(Alert, []Category{CatControl}, b))
	s.HandleEvent(Event{Kind: KindCtrlColdRestart, Severity: Alert, Category: CatControl})
	s.HandleEvent(Event{Kind: KindCtrlTimeWrite, Severity: Notice, Category: CatControl})
	s.HandleEvent(Event{Kind: KindAuthFailure, Severity: Alert, Category: CatAuth})
	if len(a.events()) != 3 || len(b.events()) != 1 {
		t.Fatalf("multi=%d filtered=%d", len(a.events()), len(b.events()))
	}

	var buf bytes.Buffer
	SlogSink(slog.New(slog.NewTextHandler(&buf, nil))).HandleEvent(Event{Kind: KindAuthFailure, Severity: Alert, Detail: "x"})
	if !strings.Contains(buf.String(), "level=ERROR") || !strings.Contains(buf.String(), "kind=auth.failure") {
		t.Fatalf("slog output: %s", buf.String())
	}

	buf.Reset()
	j := JSONLinesSink(&buf)
	j.HandleEvent(Event{Kind: KindConnClosed})
	j.HandleEvent(Event{Kind: KindConnClosed})
	if strings.Count(buf.String(), "\n") != 2 {
		t.Fatalf("json lines: %q", buf.String())
	}

	ch := make(chan Event, 1)
	cs := NewChannelSink(ch)
	cs.HandleEvent(Event{})
	cs.HandleEvent(Event{})
	if cs.Dropped() != 1 {
		t.Fatalf("channel sink dropped %d", cs.Dropped())
	}
}

func TestPeerOfReadsAddresses(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	p := PeerOf(c)
	if p.Network != "tcp" || p.RemoteAddr.String() != l.Addr().String() || p.ConnectedAt.IsZero() {
		t.Fatalf("peer = %+v", p)
	}
}
