package conformance

import (
	"testing"

	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/outstation"
)

// Report: requests requiring objects succeed when empty. ParseFragment's
// object loop simply does not run when a fragment carries no object data, so
// an empty READ or WRITE parses cleanly with no objects; handle then
// dispatches it like any other request, and onRead and onWrite both iterate
// frag.Objects, so a request that asks for nothing produces an empty, entirely
// successful response.
//
// A master that sent a request it believes meaningful gets back a success with
// no data and no indication that anything was wrong — indistinguishable, to
// it, from an outstation that genuinely had nothing to report.
func TestEmptyRequestsThatRequireObjectsAreRejected(t *testing.T) {
	tests := []struct {
		name string
		fc   app.FuncCode
	}{
		{"read", app.FuncRead},
		{"write", app.FuncWrite},
		{"direct operate", app.FuncDirectOperate},
		{"select", app.FuncSelect},
		{"enable unsolicited", app.FuncEnableUnsolicited},
		{"assign class", app.FuncAssignClass},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, outstation.Config{Database: smallDB()}, &countingHandler{})

			resp := h.request(tc.fc) // no object headers at all

			if len(resp.Objects) != 0 {
				t.Errorf("the response to an empty request carried %d objects",
					len(resp.Objects))
			}
			if !resp.Header.IIN.Has(app.IINParameterError) {
				t.Errorf("IIN = %v, want PARAMETER_ERROR: %v requires objects, and a request "+
					"carrying none was answered as though it had succeeded",
					resp.Header.IIN, tc.fc)
			}
		})
	}
}

// Function codes that legitimately carry no objects must keep working, so the
// check cannot simply reject every empty request.
func TestEmptyRequestsThatNeedNoObjectsStillSucceed(t *testing.T) {
	tests := []struct {
		name string
		fc   app.FuncCode
	}{
		{"delay measure", app.FuncDelayMeasure},
		{"record current time", app.FuncRecordCurrentTime},
		{"cold restart", app.FuncColdRestart},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, outstation.Config{Database: smallDB()}, &countingHandler{})

			resp := h.request(tc.fc)

			if resp.Header.IIN.Has(app.IINParameterError) {
				t.Errorf("IIN = %v: %v carries no objects by design and must not be "+
					"reported as a parameter error", resp.Header.IIN, tc.fc)
			}
		})
	}
}
