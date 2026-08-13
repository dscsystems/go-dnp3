package outstation

import (
	"testing"

	"github.com/dscsystems/go-dnp3"
)

func ev(class dnp3.Class, index uint16) Event {
	return Event{Type: dnp3.TypeBinary, Index: index, Class: class, Variation: 2}
}

// TestSelectDoesNotRemove is the rule the whole event model rests on: putting
// an event into a response does not drop it. Only a confirmation does.
//
// An outstation that removed events at transmission would lose exactly the
// data a sequence-of-events record exists to preserve, and lose it silently —
// the master has no way to know an event it never received was sent.
func TestSelectDoesNotRemove(t *testing.T) {
	b := NewEventBuffer(EventBufferConfig{MaxEvents: 10})
	b.Add(ev(dnp3.Class1, 0))
	b.Add(ev(dnp3.Class1, 1))

	got := b.Select(dnp3.Class1, 10)
	if len(got) != 2 {
		t.Fatalf("selected %d events, want 2", len(got))
	}
	if b.Total() != 2 {
		t.Errorf("selection removed events: %d remain, want 2", b.Total())
	}
	if b.SelectedCount() != 2 {
		t.Errorf("%d events marked selected, want 2", b.SelectedCount())
	}

	// A second selection must not hand out the same events again, or a
	// multi-fragment response would repeat them.
	if again := b.Select(dnp3.Class1, 10); len(again) != 0 {
		t.Errorf("re-selected %d already-selected events", len(again))
	}
}

func TestConfirmRemovesOnlySelected(t *testing.T) {
	b := NewEventBuffer(EventBufferConfig{MaxEvents: 10})
	b.Add(ev(dnp3.Class1, 0))
	b.Add(ev(dnp3.Class1, 1))

	b.Select(dnp3.Class1, 1) // take just the first
	b.Add(ev(dnp3.Class1, 2))

	if n := b.Confirm(); n != 1 {
		t.Errorf("confirmed %d events, want 1", n)
	}
	if b.Total() != 2 {
		t.Errorf("%d events remain, want 2", b.Total())
	}
	if b.SelectedCount() != 0 {
		t.Errorf("%d events still selected after a confirm", b.SelectedCount())
	}
}

// TestUnselectRequeues covers the confirm timeout: the master never answered,
// so the events go back in the queue to be sent again rather than vanishing.
func TestUnselectRequeues(t *testing.T) {
	b := NewEventBuffer(EventBufferConfig{MaxEvents: 10})
	b.Add(ev(dnp3.Class1, 0))
	b.Add(ev(dnp3.Class1, 1))
	b.Select(dnp3.Class1, 10)

	if n := b.Unselect(); n != 2 {
		t.Errorf("unselected %d events, want 2", n)
	}
	if b.Total() != 2 {
		t.Errorf("%d events remain, want 2", b.Total())
	}
	if got := b.Select(dnp3.Class1, 10); len(got) != 2 {
		t.Errorf("re-selected %d events after an unselect, want 2", len(got))
	}
}

func TestSelectRespectsClassMask(t *testing.T) {
	b := NewEventBuffer(EventBufferConfig{MaxEvents: 10})
	b.Add(ev(dnp3.Class1, 0))
	b.Add(ev(dnp3.Class2, 1))
	b.Add(ev(dnp3.Class3, 2))

	if got := b.Select(dnp3.Class2, 10); len(got) != 1 || got[0].Index != 1 {
		t.Errorf("class 2 selection = %v", got)
	}
	if got := b.Select(dnp3.Class1|dnp3.Class3, 10); len(got) != 2 {
		t.Errorf("selected %d events for classes 1 and 3, want 2", len(got))
	}
}

func TestSelectRespectsLimit(t *testing.T) {
	b := NewEventBuffer(EventBufferConfig{MaxEvents: 10})
	for i := range 5 {
		b.Add(ev(dnp3.Class1, uint16(i)))
	}
	got := b.Select(dnp3.Class1, 3)
	if len(got) != 3 {
		t.Fatalf("selected %d events, want 3", len(got))
	}
	// Oldest first: a sequence of events is only useful in order.
	for i, e := range got {
		if e.Index != uint16(i) {
			t.Errorf("event %d has index %d; selection is not oldest-first", i, e.Index)
		}
	}
	if b.Select(dnp3.Class1, 0) != nil {
		t.Error("a zero limit should select nothing")
	}
}

// TestOverflowDropsOldestAndLatches covers what happens when a device produces
// events faster than the master collects them.
func TestOverflowDropsOldestAndLatches(t *testing.T) {
	b := NewEventBuffer(EventBufferConfig{MaxEvents: 3})
	for i := range 5 {
		b.Add(ev(dnp3.Class1, uint16(i)))
	}

	if b.Total() != 3 {
		t.Errorf("%d events buffered, want the 3 the buffer holds", b.Total())
	}
	if !b.Overflowed() {
		t.Error("the overflow was not latched")
	}

	// The oldest went, not the newest: after a loss the recent past is what an
	// operator needs.
	got := b.Select(dnp3.Class1, 10)
	if len(got) != 3 || got[0].Index != 2 || got[2].Index != 4 {
		t.Errorf("kept events %v; want indexes 2, 3, 4", indexes(got))
	}

	b.ClearOverflow()
	if b.Overflowed() {
		t.Error("ClearOverflow did not clear the flag")
	}
}

func indexes(evs []Event) []uint16 {
	out := make([]uint16, len(evs))
	for i, e := range evs {
		out[i] = e.Index
	}
	return out
}

func TestClassesReportsWhatIsWaiting(t *testing.T) {
	b := NewEventBuffer(EventBufferConfig{})
	if b.Classes() != 0 {
		t.Error("an empty buffer should report no classes")
	}

	b.Add(ev(dnp3.Class1, 0))
	b.Add(ev(dnp3.Class3, 1))

	got := b.Classes()
	if got&dnp3.Class1 == 0 || got&dnp3.Class3 == 0 {
		t.Errorf("classes = %v, want 1 and 3", got)
	}
	if got&dnp3.Class2 != 0 {
		t.Errorf("classes = %v, should not include 2", got)
	}
	if b.Count(dnp3.Class1) != 1 {
		t.Errorf("class 1 count = %d, want 1", b.Count(dnp3.Class1))
	}
}

func TestResetEmpties(t *testing.T) {
	b := NewEventBuffer(EventBufferConfig{MaxEvents: 2})
	b.Add(ev(dnp3.Class1, 0))
	b.Add(ev(dnp3.Class1, 1))
	b.Add(ev(dnp3.Class1, 2)) // overflows

	b.Reset()
	if b.Total() != 0 || b.Overflowed() {
		t.Error("Reset left state behind")
	}
}

func TestDefaultCapacity(t *testing.T) {
	b := NewEventBuffer(EventBufferConfig{})
	for range DefaultMaxEvents + 10 {
		b.Add(ev(dnp3.Class1, 0))
	}
	if b.Total() != DefaultMaxEvents {
		t.Errorf("%d events buffered, want the default %d", b.Total(), DefaultMaxEvents)
	}
}
