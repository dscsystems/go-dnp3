package conformance

import (
	"testing"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/outstation"
)

// eventCount totals the points carried by a response fragment.
func eventCount(frag app.Fragment) int {
	n := 0
	for _, h := range frag.Objects {
		n += int(h.Range.Count)
	}
	return n
}

// Report: an unsolicited retry can still lose newly queued events.
//
// Keeping the sequence number on a retry (which a master's duplicate
// detection depends on) is only half of what a retry has to do. The other
// half is retransmitting the *same* fragment: onUnsolicitedTimeout unselects
// every event, and the retry then calls EventBuffer.Select afresh, which
// picks up anything queued in the meantime. The retry therefore goes out
// carrying more events than the transmission it is repeating, under that
// transmission's sequence number.
//
// A master matches purely on that sequence number (master/session.go's
// onUnsolicited: "duplicate := s.hasUnsolSeq && seq == s.unsolSeq", and a
// duplicate skips deliver entirely) — so it drops the whole payload,
// including the events it has never seen, and confirms it anyway. That
// confirm reaches onUnsolicitedConfirm, which calls EventBuffer.Confirm and
// removes every selected event. The new event is gone from both ends.
func TestUnsolicitedRetryRepeatsTheOriginalEventSet(t *testing.T) {
	h := newHarness(t, outstation.Config{
		Database: smallDB(),
		Unsolicited: outstation.UnsolicitedConfig{
			Enabled:        true,
			ConfirmTimeout: 250 * time.Millisecond,
			MaxRetries:     5,
		},
	}, nil)

	null := h.await(0)
	sendUnsolConfirm(h, null.Header.Control.Seq)
	enableUnsolicited(h)

	// One event, transmitted and deliberately never confirmed.
	before := h.count()
	h.out.Update(func(db *outstation.Database) {
		db.UpdateBinary(0, dnp3.Binary{Value: true, Flags: dnp3.Online})
	})
	first := h.await(before)
	if eventCount(first) != 1 {
		t.Fatalf("expected the first transmission to carry exactly one event, got %d: %+v",
			eventCount(first), first)
	}

	// A second event appears while that transmission is still outstanding.
	// pollUnsolicited will not send it on its own — it returns early while
	// unsol.awaiting is set — so it simply waits in the queue until the
	// confirm timeout unselects the first event and the retry selects both.
	h.out.Update(func(db *outstation.Database) {
		db.UpdateBinary(1, dnp3.Binary{Value: true, Flags: dnp3.Online})
	})

	retry := h.await(before + 1)
	if retry.Header.Control.Seq != first.Header.Control.Seq {
		t.Fatalf("the retry did not keep the original sequence number (got %d, want %d); "+
			"this defect cannot be exercised without that holding first",
			retry.Header.Control.Seq, first.Header.Control.Seq)
	}

	if got := eventCount(retry); got != eventCount(first) {
		t.Errorf("the retry carries %d events where the transmission it repeats carried %d; "+
			"a master matching on the unchanged sequence number treats the whole fragment as "+
			"a duplicate and delivers none of it, so the %d new event(s) are never seen",
			got, eventCount(first), got-eventCount(first))
	}

	// Confirming the retry may only clear what the retry actually carried.
	// The second event was never offered to the master under a sequence
	// number it would accept, so it must survive and go out on its own.
	before = h.count()
	sendUnsolConfirm(h, retry.Header.Control.Seq)

	fresh := h.await(before)
	if fresh.Header.Control.Seq == first.Header.Control.Seq {
		t.Errorf("the event held back during the retry was re-sent under the retry's own "+
			"sequence number %d; a master matches that as a duplicate and drops it",
			first.Header.Control.Seq)
	}
	if got := eventCount(fresh); got != 1 {
		t.Errorf("the transmission after the confirm carries %d events, want the 1 that was "+
			"held back while the retry repeated the original", got)
	}
}
