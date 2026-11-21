package secevent

import (
	"container/list"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Sink receives events. A sink is called from one goroutine at a time, the
// dispatcher's, and may take its time: a slow sink costs queued events, never
// a protocol goroutine's progress.
type Sink interface {
	HandleEvent(Event)
}

// SinkFunc adapts a function to a Sink.
type SinkFunc func(Event)

// HandleEvent calls f.
func (f SinkFunc) HandleEvent(e Event) { f(e) }

// Defaults for a [Dispatcher].
const (
	DefaultQueueSize       = 1024
	DefaultAggregateWindow = time.Second
	DefaultMaxAggregates   = 4096
)

// DispatcherOptions tunes a [Dispatcher]. Zero values take the defaults.
type DispatcherOptions struct {
	// QueueSize bounds the events waiting for the sink. When it is full,
	// events are dropped and counted.
	QueueSize int
	// AggregateWindow is how long repeats of an event are folded into one.
	// Negative disables aggregation.
	AggregateWindow time.Duration
	// MaxAggregates bounds how many distinct events are being aggregated at
	// once. The oldest is flushed to make room.
	MaxAggregates int
}

// Dispatcher queues events and delivers them to a sink on its own goroutine.
//
// Submitting never blocks. When the queue is full the event is dropped and
// counted, and the next event delivered is preceded by a
// [KindMetaEventsDropped] event carrying the count, so a drop is never
// silent.
//
// Repeats are aggregated: the first event with a given connection, kind and
// key is delivered at once, and further ones within AggregateWindow are
// counted and delivered as one event when the window closes. Kinds marked
// always-deliver bypass this. Without it, the event path would amplify the
// very floods it reports.
//
// A Dispatcher can be shared by several sessions.
type Dispatcher struct {
	sink   Sink
	sync   bool
	window time.Duration
	maxAgg int

	q       chan Event
	dropped atomic.Uint64
	closing chan struct{}
	done    chan struct{}

	// admit orders Submit against Close: a submitter holds it shared while
	// it checks closed and queues, Close holds it exclusively to set closed.
	// So every event Submit accepted is in the queue before the worker
	// drains it for the last time.
	admit  sync.RWMutex
	closed bool

	// deliverMu serialises every step that may call the sink, and guards
	// out and panicked. It is taken before mu, and the sink is called with
	// it held but mu released, so a sink may call Stats.
	deliverMu sync.Mutex
	out       []Event
	panicked  bool

	// mu guards the aggregation state and the counters.
	mu    sync.Mutex
	aggs  map[aggKey]*list.Element
	order *list.List // of *aggregate, oldest first
	stats Stats
}

type aggKey struct {
	conn uint64
	kind Kind
	key  string
}

type aggregate struct {
	key   aggKey
	first time.Time
	count int // repeats after the first, not yet delivered
	last  Event
}

// NewDispatcher returns a dispatcher delivering to sink. It starts a goroutine
// that runs until Close.
func NewDispatcher(sink Sink, opts DispatcherOptions) *Dispatcher {
	if opts.QueueSize <= 0 {
		opts.QueueSize = DefaultQueueSize
	}
	if opts.AggregateWindow == 0 {
		opts.AggregateWindow = DefaultAggregateWindow
	}
	if opts.MaxAggregates <= 0 {
		opts.MaxAggregates = DefaultMaxAggregates
	}
	d := &Dispatcher{
		sink:    sink,
		window:  opts.AggregateWindow,
		maxAgg:  opts.MaxAggregates,
		q:       make(chan Event, opts.QueueSize),
		closing: make(chan struct{}),
		done:    make(chan struct{}),
		aggs:    map[aggKey]*list.Element{},
		order:   list.New(),
		stats:   newStats(),
	}
	if s, ok := sink.(syncSink); ok {
		d.sink, d.sync = s.Sink, true
	}
	go d.run()
	return d
}

// Submit queues an event without blocking. It reports false when the event
// was dropped because the queue is full or the dispatcher is closed.
func (d *Dispatcher) Submit(e Event) bool {
	if d == nil {
		return false
	}
	d.admit.RLock()
	defer d.admit.RUnlock()
	if d.closed {
		return false
	}
	if d.sync {
		d.step(func() { d.process(e, time.Now()) })
		return true
	}
	select {
	case d.q <- e:
		return true
	default:
		d.dropped.Add(1)
		return false
	}
}

// step runs f on the aggregation state, then hands the sink whatever f
// queued for delivery, in order.
func (d *Dispatcher) step(f func()) {
	d.deliverMu.Lock()
	defer d.deliverMu.Unlock()
	d.mu.Lock()
	f()
	d.mu.Unlock()
	for i := range d.out {
		d.call(d.out[i])
	}
	clear(d.out)
	d.out = d.out[:0]
}

// Close flushes queued and aggregated events to the sink and stops the
// dispatcher. Events submitted afterwards are dropped.
func (d *Dispatcher) Close() error {
	if d == nil {
		return nil
	}
	d.admit.Lock()
	if !d.closed {
		d.closed = true
		close(d.closing)
	}
	d.admit.Unlock()
	<-d.done
	return nil
}

func (d *Dispatcher) run() {
	defer close(d.done)
	tick := d.window / 2
	if tick <= 0 {
		tick = 100 * time.Millisecond
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case e := <-d.q:
			d.step(func() { d.process(e, time.Now()) })
		case now := <-t.C:
			d.step(func() {
				d.noteDrops(now)
				d.flushExpired(now, false)
			})
		case <-d.closing:
			// No Submit is mid-send once closing is closed, so this drain
			// sees every event that was accepted.
			for {
				select {
				case e := <-d.q:
					d.step(func() { d.process(e, time.Now()) })
					continue
				default:
				}
				break
			}
			d.step(func() {
				now := time.Now()
				d.noteDrops(now)
				d.flushExpired(now, true)
			})
			return
		}
	}
}

// noteDrops delivers a report of events dropped since the last one.
func (d *Dispatcher) noteDrops(now time.Time) {
	n := d.dropped.Swap(0)
	if n == 0 {
		return
	}
	d.stats.Dropped += n
	d.deliver(Event{
		Time: now, Kind: KindMetaEventsDropped, Category: CatMeta, Severity: Warning,
		Protocol: "dnp3", Count: int(n),
		Detail: fmt.Sprintf("%d security events were dropped because the queue was full", n),
	})
}

// process handles one event. d.mu is held.
func (d *Dispatcher) process(e Event, now time.Time) {
	d.noteDrops(now)
	d.stats.count(e)
	if d.window < 0 || e.Kind.AlwaysDeliver() {
		d.deliver(e)
		return
	}
	k := aggKey{conn: e.ConnID, kind: e.Kind, key: aggregationKey(e)}
	if el, ok := d.aggs[k]; ok {
		a := el.Value.(*aggregate)
		if now.Sub(a.first) < d.window {
			a.count++
			a.last = e
			d.stats.Aggregated++
			return
		}
		d.flush(el)
	}
	d.deliver(e)
	d.aggs[k] = d.order.PushBack(&aggregate{key: k, first: now})
	for d.order.Len() > d.maxAgg {
		d.flush(d.order.Front())
	}
}

// flushExpired delivers aggregates whose window has closed, or all of them.
func (d *Dispatcher) flushExpired(now time.Time, all bool) {
	for el := d.order.Front(); el != nil; {
		next := el.Next()
		a := el.Value.(*aggregate)
		if !all && now.Sub(a.first) < d.window {
			break // ordered oldest first
		}
		d.flush(el)
		el = next
	}
}

// flush delivers an aggregate's repeats, if it has any, and forgets it.
func (d *Dispatcher) flush(el *list.Element) {
	a := el.Value.(*aggregate)
	d.order.Remove(el)
	delete(d.aggs, a.key)
	if a.count == 0 {
		return
	}
	e := a.last
	e.Count = a.count
	e.FirstSeen = a.first
	d.deliver(e)
}

// deliver queues one event for the sink. d.mu and d.deliverMu are held;
// step hands it over once d.mu is released.
func (d *Dispatcher) deliver(e Event) {
	if e.Count == 0 {
		e.Count = 1
	}
	d.stats.Delivered++
	d.out = append(d.out, e)
}

// call hands one event to the sink, surviving a panicking sink. d.deliverMu
// is held and d.mu is not.
func (d *Dispatcher) call(e Event) {
	defer func() {
		if r := recover(); r != nil {
			d.mu.Lock()
			d.stats.SinkPanics++
			first := !d.panicked
			if first {
				d.stats.Delivered++
			}
			d.mu.Unlock()
			if !first {
				return
			}
			d.panicked = true
			d.call(Event{
				Time: time.Now(), Kind: KindMetaSinkPanic, Category: CatMeta, Severity: Critical,
				Protocol: "dnp3", Count: 1, Detail: fmt.Sprintf("the event sink panicked: %v", r),
			})
		}
	}()
	if d.sink != nil {
		d.sink.HandleEvent(e)
	}
}

// aggregationKey is what tells two events of the same kind on the same
// connection apart for aggregation: the same malformed request from the same
// station is one event repeated, an unknown point at a different index is
// another event.
//
// So is an outcome: a control accepted and the same control refused, or a
// failure for a different reason, are not repeats of each other.
//
// Events not tied to a connection — a refused connection, a failed accept —
// are told apart by the peer's address instead.
func aggregationKey(e Event) string {
	var peer string
	if e.ConnID == 0 && e.Peer.RemoteAddr.IsValid() {
		peer = e.Peer.RemoteAddr.Addr().String()
	}
	if e.Fields&(HasLinkAddrs|HasAppFunc|HasObject|HasIndex|HasUser|HasCommandStatus|HasControlCode) == 0 &&
		e.Value == "" && e.Action == Observed {
		return peer
	}
	return fmt.Sprintf("%s/%d/%d/%d/%d.%d/%d/%d/%d/%d/%d/%s", peer, e.LinkSrc, e.LinkDest, e.AppFunc,
		e.Group, e.Variation, e.Index, e.User, e.CommandStatus, e.ControlCode, e.Action, e.Value)
}

// syncSink marks a sink for synchronous delivery.
type syncSink struct{ Sink }

// SyncSink makes a dispatcher deliver to s on the submitting goroutine.
//
// It exists for tests, which want to see an event the moment it is emitted.
// Never use it in production: a slow sink would stall the protocol goroutine
// that emitted the event.
func SyncSink(s Sink) Sink { return syncSink{s} }
