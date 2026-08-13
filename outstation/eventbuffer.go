package outstation

import (
	"sync"

	"github.com/dscsystems/go-dnp3"
)

// Event is one queued change of state.
//
// Exactly one of the measurement fields is meaningful, selected by Type. A
// tagged union rather than an interface keeps events allocation-free in the
// buffer, which matters when a storm queues thousands per second.
type Event struct {
	Type      dnp3.PointType
	Index     uint16
	Class     dnp3.Class
	Variation uint8
	Time      dnp3.Timestamp

	Binary        dnp3.Binary
	DoubleBit     dnp3.DoubleBitBinary
	Counter       dnp3.Counter
	FrozenCounter dnp3.FrozenCounter
	Analog        dnp3.Analog
	BinaryOutput  dnp3.BinaryOutputStatus
	AnalogOutput  dnp3.AnalogOutputStatus
	OctetString   dnp3.OctetString

	// selected marks an event that has been put into a response but not yet
	// confirmed by the master.
	selected bool
}

// EventBufferConfig sizes the buffer.
type EventBufferConfig struct {
	// MaxEvents is the total capacity across all classes. Zero uses
	// DefaultMaxEvents.
	MaxEvents int
}

// DefaultMaxEvents is the buffer capacity when a configuration does not set
// one.
const DefaultMaxEvents = 1000

// EventBuffer holds events waiting to be reported.
//
// The lifecycle is the part that matters, and the part implementations get
// wrong: an event is queued, then *selected* when it goes into a response,
// and only removed when the master confirms that response. An outstation that
// drops events at transmission loses exactly the data a sequence-of-events
// record exists to preserve — and loses it silently, because the master has
// no way to know an event it never saw was sent.
type EventBuffer struct {
	mu       sync.Mutex
	events   []Event
	max      int
	overflow bool
}

// NewEventBuffer returns an event buffer.
func NewEventBuffer(cfg EventBufferConfig) *EventBuffer {
	maxEvents := cfg.MaxEvents
	if maxEvents <= 0 {
		maxEvents = DefaultMaxEvents
	}
	return &EventBuffer{
		events: make([]Event, 0, maxEvents),
		max:    maxEvents,
	}
}

// Add queues an event, discarding the oldest if the buffer is full.
//
// Dropping the oldest rather than the newest is deliberate: after an overflow
// the master's picture is already incomplete, and the recent past is what an
// operator needs. The overflow is latched so it can be reported in the
// internal indications, which is the only way the master learns there is a
// hole in its record.
func (b *EventBuffer) Add(e Event) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if len(b.events) >= b.max {
		b.events = append(b.events[:0], b.events[1:]...)
		b.overflow = true
	}
	b.events = append(b.events, e)
}

// Count returns how many events are queued for the given classes.
func (b *EventBuffer) Count(mask dnp3.Class) int {
	b.mu.Lock()
	defer b.mu.Unlock()

	n := 0
	for i := range b.events {
		if b.events[i].Class&mask != 0 {
			n++
		}
	}
	return n
}

// Total returns how many events are queued in all.
func (b *EventBuffer) Total() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.events)
}

// Classes returns the mask of classes that have events waiting, which is what
// the internal indications report.
func (b *EventBuffer) Classes() dnp3.Class {
	b.mu.Lock()
	defer b.mu.Unlock()

	var mask dnp3.Class
	for i := range b.events {
		mask |= b.events[i].Class
	}
	return mask
}

// Overflowed reports whether events have been lost since the flag was last
// cleared.
func (b *EventBuffer) Overflowed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.overflow
}

// ClearOverflow resets the overflow flag, which a master does implicitly by
// reading the events that remain.
func (b *EventBuffer) ClearOverflow() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.overflow = false
}

// Select marks up to limit unselected events of the given classes and returns
// copies of them, oldest first.
//
// The events stay in the buffer. They are removed only by [EventBuffer.Confirm].
func (b *EventBuffer) Select(mask dnp3.Class, limit int) []Event {
	b.mu.Lock()
	defer b.mu.Unlock()

	if limit <= 0 {
		return nil
	}
	out := make([]Event, 0, min(limit, len(b.events)))
	for i := range b.events {
		if len(out) >= limit {
			break
		}
		e := &b.events[i]
		if e.selected || e.Class&mask == 0 {
			continue
		}
		e.selected = true
		out = append(out, *e)
	}
	return out
}

// SelectedCount returns how many events are awaiting confirmation.
func (b *EventBuffer) SelectedCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()

	n := 0
	for i := range b.events {
		if b.events[i].selected {
			n++
		}
	}
	return n
}

// Confirm removes every selected event. The master has acknowledged the
// response that carried them, so they can finally be dropped.
func (b *EventBuffer) Confirm() int {
	b.mu.Lock()
	defer b.mu.Unlock()

	kept := b.events[:0]
	removed := 0
	for _, e := range b.events {
		if e.selected {
			removed++
			continue
		}
		kept = append(kept, e)
	}
	b.events = kept
	return removed
}

// Unselect returns every selected event to the queue, which is what happens
// when a confirmation does not arrive. The events are re-sent rather than lost.
func (b *EventBuffer) Unselect() int {
	b.mu.Lock()
	defer b.mu.Unlock()

	n := 0
	for i := range b.events {
		if b.events[i].selected {
			b.events[i].selected = false
			n++
		}
	}
	return n
}

// Reset empties the buffer, as a cold restart does.
func (b *EventBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.events = b.events[:0]
	b.overflow = false
}
