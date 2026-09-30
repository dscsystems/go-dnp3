package conformance

import (
	"testing"
	"time"

	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/outstation"
)

// sendRaw transmits an application fragment exactly as given.
func (h *harness) sendRaw(fragment []byte) {
	h.t.Helper()
	if err := h.stack.Send(h.conn, fragment); err != nil {
		h.t.Fatalf("send: %v", err)
	}
}

// expectSilence fails if the outstation says anything after before fragments.
func (h *harness) expectSilence(before int) {
	h.t.Helper()
	time.Sleep(150 * time.Millisecond)
	if got := h.count(); got != before {
		h.t.Errorf("the outstation answered a message it should have discarded (%d new fragment(s))", got-before)
	}
}

// CON and UNS belong to responses. A request setting either is discarded
// unanswered rather than acted on.
func TestRequestWithConOrUnsIsDiscarded(t *testing.T) {
	for _, ctrl := range []app.Control{
		{Fir: true, Fin: true, Con: true, Seq: 3},
		{Fir: true, Fin: true, Uns: true, Seq: 3},
	} {
		h := newHarness(t, outstation.Config{Database: smallDB()}, nil)
		before := h.count()
		h.sendRaw(app.BuildRequest(nil, ctrl, app.FuncRead, app.ReadAllObjects(60, 1)))
		h.expectSilence(before)
		if got := h.out.Stats().MalformedRequests; got != 1 {
			t.Errorf("control %+v: MalformedRequests = %d, want 1", ctrl, got)
		}
	}
}

// A CONFIRM is a bare header; objects after it are not an acknowledgement.
func TestConfirmCarryingObjectsIsDiscarded(t *testing.T) {
	h := newHarness(t, outstation.Config{Database: smallDB()}, nil)
	before := h.count()
	h.sendRaw(app.BuildRequest(nil, app.Control{Fir: true, Fin: true, Seq: 1},
		app.FuncConfirm, app.ReadAllObjects(60, 1)))
	h.expectSilence(before)
	if got := h.out.Stats().MalformedRequests; got != 1 {
		t.Errorf("MalformedRequests = %d, want 1", got)
	}
}

// A prefix and range that do not go together name no points anyone can agree
// on, so the request is refused rather than guessed at.
func TestInconsistentQualifierIsRefused(t *testing.T) {
	h := newHarness(t, outstation.Config{Database: smallDB()}, nil)

	// 0x16: a one-octet index prefix on an all-objects range.
	resp := h.request(app.FuncRead, app.ObjectHeader{
		Group: 1, Variation: 2,
		Qualifier: app.MakeQualifier(app.PrefixIndex1, app.RangeAllObjects),
		Range:     app.Range{Spec: app.RangeAllObjects},
	})
	if !resp.Header.IIN.Has(app.IINParameterError) {
		t.Errorf("IIN = %v, want PARAMETER_ERROR", resp.Header.IIN)
	}
	if len(resp.Objects) != 0 {
		t.Errorf("the refused request produced %d objects", len(resp.Objects))
	}
}
