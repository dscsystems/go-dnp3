package conformance

import (
	"testing"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/outstation"
)

// P1 report: an unsolicited retry changes its sequence number. A master tells
// a genuine retransmission apart from new data purely by the sequence number
// matching the one it already has outstanding (see master/session.go's
// onUnsolicited: "Our confirm was lost, not the data. Confirm again but do not
// deliver the measurements a second time."). If the outstation bumps the
// sequence on every attempt, including retries of an unconfirmed response,
// that duplicate-detection can never fire and a master delivers the same
// events twice.
//
// The null unsolicited response — sent before any event data, to announce the
// outstation exists — goes through the exact same send/timeout/retry path as
// an event-carrying one and is simpler to force: a harness that never sends
// CONFIRM leaves it permanently unconfirmed, so every retry is observable.
func TestUnsolicitedRetryKeepsItsSequenceNumber(t *testing.T) {
	h := newHarness(t, outstation.Config{
		Database: smallDB(),
		Unsolicited: outstation.UnsolicitedConfig{
			Enabled:        true,
			ConfirmTimeout: 100 * time.Millisecond,
			MaxRetries:     5,
		},
	}, nil)

	// The harness never sends CONFIRM, so the null response is retried
	// (ConfirmTimeout, then again) without ever being acknowledged.
	first := h.await(0)
	if !first.Header.Control.Uns {
		t.Fatalf("the first spontaneous fragment is not marked unsolicited: %+v", first.Header)
	}

	second := h.await(1)
	if !second.Header.Control.Uns {
		t.Fatalf("the retry is not marked unsolicited: %+v", second.Header)
	}

	if second.Header.Control.Seq != first.Header.Control.Seq {
		t.Errorf("retry sequence = %d, want it unchanged from the original %d — "+
			"a master's duplicate detection matches on sequence number and cannot "+
			"catch this, so it will deliver the retried response as new data",
			second.Header.Control.Seq, first.Header.Control.Seq)
	}
}

// sendUnsolConfirm acknowledges an unsolicited response.
//
// The harness's own sendConfirm builds a solicited confirm (Uns clear), which
// the outstation would route to onConfirm instead of onUnsolicitedConfirm and
// so would never actually confirm anything used by these tests.
func sendUnsolConfirm(h *harness, seq uint8) {
	h.t.Helper()
	frag := app.AppendHeader(nil, app.Header{
		Control: app.Control{Fir: true, Fin: true, Uns: true, Seq: seq},
		Func:    app.FuncConfirm,
	})
	if err := h.stack.Send(h.conn, frag); err != nil {
		h.t.Fatalf("unsolicited confirm: %v", err)
	}
}

// enableUnsolicited sends ENABLE_UNSOLICITED for class 1, which is the
// device-level switch pollUnsolicited also checks (s.unsolClasses): without
// it the outstation never leaves the null-response phase, whatever events are
// queued.
func enableUnsolicited(h *harness) {
	h.t.Helper()
	before := h.count()
	h.send(app.FuncEnableUnsolicited, app.ReadAllObjects(60, 2))
	h.await(before) // the solicited reply to the enable request itself
}

// The fix must not go too far the other way: a retry keeps its sequence
// number, but a genuinely new transmission — the first attempt at a fresh
// batch of events, sent once the previous one is actually confirmed — has to
// advance it as before, or a master would start rejecting real new data as a
// duplicate of whatever came before it.
//
// This also covers the data-carrying case rather than only the null
// response: the null response and an event-carrying one are built by
// different code in sendUnsolicited (a null body versus buildEvents), so both
// are worth exercising independently.
func TestUnsolicitedNewTransmissionsStillAdvanceTheSequence(t *testing.T) {
	h := newHarness(t, outstation.Config{
		Database: smallDB(),
		Unsolicited: outstation.UnsolicitedConfig{
			Enabled:        true,
			ConfirmTimeout: 300 * time.Millisecond,
			MaxRetries:     5,
		},
	}, nil)

	null := h.await(0)
	if len(null.Objects) != 0 {
		t.Fatalf("the first unsolicited response is not the empty null one: %+v", null)
	}
	sendUnsolConfirm(h, null.Header.Control.Seq)
	enableUnsolicited(h)

	// A fresh event, and the response it produces: a genuinely new
	// transmission, which must not reuse the null response's sequence number.
	before := h.count()
	h.out.Update(func(db *outstation.Database) {
		db.UpdateBinary(0, dnp3.Binary{Value: true, Flags: dnp3.Online})
	})
	firstData := h.await(before)
	if len(firstData.Objects) == 0 {
		t.Fatalf("expected an event-carrying response: %+v", firstData)
	}
	if firstData.Header.Control.Seq == null.Header.Control.Seq {
		t.Fatalf("the first data response reused the null response's sequence number %d",
			null.Header.Control.Seq)
	}

	// Confirm it, then raise a second, independent event. Its response must
	// advance the sequence again, not reuse firstData's.
	sendUnsolConfirm(h, firstData.Header.Control.Seq)
	before = h.count()
	h.out.Update(func(db *outstation.Database) {
		db.UpdateBinary(1, dnp3.Binary{Value: true, Flags: dnp3.Online})
	})
	secondData := h.await(before)
	if len(secondData.Objects) == 0 {
		t.Fatalf("expected a second event-carrying response: %+v", secondData)
	}
	if secondData.Header.Control.Seq != (firstData.Header.Control.Seq+1)%app.SeqModulus {
		t.Errorf("second transmission's sequence = %d, want %d (the next one after %d)",
			secondData.Header.Control.Seq, (firstData.Header.Control.Seq+1)%app.SeqModulus,
			firstData.Header.Control.Seq)
	}
}

// The retry itself must actually carry the events, not just reuse the right
// sequence number on an otherwise-empty resend: TestUnsolicitedRetryKeepsItsSequenceNumber
// proves the sequence with the null response, which never carries objects, so
// this proves the same fix holds when there is real event data on the wire.
func TestUnsolicitedDataRetryKeepsItsSequenceNumber(t *testing.T) {
	h := newHarness(t, outstation.Config{
		Database: smallDB(),
		Unsolicited: outstation.UnsolicitedConfig{
			Enabled:        true,
			ConfirmTimeout: 150 * time.Millisecond,
			MaxRetries:     5,
		},
	}, nil)

	null := h.await(0)
	sendUnsolConfirm(h, null.Header.Control.Seq)
	enableUnsolicited(h)

	before := h.count()
	h.out.Update(func(db *outstation.Database) {
		db.UpdateBinary(0, dnp3.Binary{Value: true, Flags: dnp3.Online})
	})

	// Never confirmed: it times out and is retried.
	first := h.await(before)
	if len(first.Objects) == 0 {
		t.Fatalf("expected an event-carrying response: %+v", first)
	}
	second := h.await(before + 1)
	if len(second.Objects) == 0 {
		t.Fatalf("expected the retry to carry the same event: %+v", second)
	}
	if second.Header.Control.Seq != first.Header.Control.Seq {
		t.Errorf("retry sequence = %d, want it unchanged from the original %d",
			second.Header.Control.Seq, first.Header.Control.Seq)
	}
}

// Giving up after MaxRetries resets the retry count, and the attempt that
// follows — once the back-off period passes — is a fresh transmission in its
// own right, so it must get its own new sequence number rather than
// perpetually reusing the one from the attempt that was abandoned.
func TestUnsolicitedSequenceAdvancesAfterGivingUp(t *testing.T) {
	h := newHarness(t, outstation.Config{
		Database: smallDB(),
		Unsolicited: outstation.UnsolicitedConfig{
			Enabled:        true,
			ConfirmTimeout: 80 * time.Millisecond,
			MaxRetries:     1,
		},
	}, nil)

	// Never confirmed: one send, one retry (still MaxRetries=1), then the
	// outstation gives up and backs off for ConfirmTimeout before trying the
	// null response again.
	first := h.await(0)
	retry := h.await(1)
	if retry.Header.Control.Seq != first.Header.Control.Seq {
		t.Fatalf("the retry did not keep the sequence number (got %d, want %d); "+
			"the give-up case cannot be exercised without that holding first",
			retry.Header.Control.Seq, first.Header.Control.Seq)
	}

	afterGiveUp := h.await(2)
	if afterGiveUp.Header.Control.Seq == first.Header.Control.Seq {
		t.Errorf("the attempt after giving up reused sequence %d instead of advancing past it",
			first.Header.Control.Seq)
	}
}
