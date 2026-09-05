package conformance

import (
	"testing"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/outstation"
)

// P1 report: intermediate confirms can discard later events. sendFragments
// builds static data into the response before it builds events (onRead calls
// buildStaticRange for every object header as it walks the request, and only
// appends the selected events afterwards), so a response spanning several
// fragments puts events in a *later* fragment while an *earlier* one — sent
// and requiring confirmation first, since only the last fragment's Con
// depends on hasEvents — carries none.
//
// All fragments in one multi-fragment response share the same Seq (the
// request's own), which is the only field a confirm is matched against
// (onConfirm: "if !s.awaitingConfirm || h.Control.Seq != s.confirmSeq"). A
// confirm for the first fragment is therefore indistinguishable, at the
// protocol level, from a confirm for the one that actually carries the
// events — and onConfirm's response to either is the same: unconditionally
// call EventBuffer.Confirm, which removes every currently selected event.
//
// A master that confirms each fragment as it parses it — which is legal, and
// exactly what this library's own master does (see master/session.go's
// onSolicited, which sends the confirm immediately rather than waiting for
// Fin) — will trigger this on any ordinary multi-fragment poll that mixes
// events with static data, not just an adversarial one.
func TestIntermediateConfirmDoesNotDiscardLaterEvents(t *testing.T) {
	h := newHarness(t, outstation.Config{
		Database: outstation.DatabaseConfig{
			Binary:       60,
			DefaultClass: dnp3.Class1,
		},
		MaxTxFragment:  30, // forces several small fragments
		ConfirmTimeout: 2 * time.Second,
	}, nil)

	// One event, so what fragment 1 carries is exactly one selected event —
	// easy to tell "still there" from "gone" by count.
	h.out.Update(func(db *outstation.Database) {
		db.UpdateBinary(0, dnp3.Binary{Value: true, Flags: dnp3.Online})
	})

	// One read combining event classes with class 0 static, which is exactly
	// what an integrity poll sends in a single request.
	before := h.count()
	h.send(app.FuncRead,
		app.ReadAllObjects(60, 2), // class 1 events
		app.ReadAllObjects(60, 1), // class 0 static
	)

	fragment0 := h.await(before)
	if fragment0.Header.Control.Fin {
		t.Fatal("the response fit in a single fragment; the test needs at least two to isolate the bug")
	}
	if !fragment0.Header.Control.Con {
		t.Fatal("the first fragment does not ask for a confirmation, so there is nothing to confirm early")
	}

	// Confirm only the first fragment — the one built from static data, with
	// no events in it. The second, where the event actually lives, is never
	// confirmed here: exactly "a later fragment that may not arrive."
	h.sendConfirm(fragment0.Header.Control.Seq)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && h.out.Stats().ConfirmsReceived == 0 {
		time.Sleep(2 * time.Millisecond)
	}
	if h.out.Stats().ConfirmsReceived == 0 {
		t.Fatal("the outstation never registered the confirm")
	}

	if got := h.out.Events().SelectedCount(); got == 0 {
		t.Error("confirming the first fragment discarded the event carried by the " +
			"second, unconfirmed one — it would be lost for good if that fragment " +
			"never arrived")
	}
}
