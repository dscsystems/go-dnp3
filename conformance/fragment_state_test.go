package conformance

import (
	"sync"
	"testing"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/outstation"
)

// countingHandler is recordingHandler with a lock, so a test may read the
// counts from its own goroutine while the session goroutine writes them.
type countingHandler struct {
	mu       sync.Mutex
	operates int
}

func (c *countingHandler) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.operates
}

func (c *countingHandler) SelectCROB(uint16, dnp3.ControlRelayOutputBlock) dnp3.CommandStatus {
	return dnp3.CommandSuccess
}

func (c *countingHandler) OperateCROB(uint16, dnp3.ControlRelayOutputBlock, outstation.OperateType) dnp3.CommandStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.operates++
	return dnp3.CommandSuccess
}

func (c *countingHandler) SelectAnalog(uint16, outstation.AnalogOutput) dnp3.CommandStatus {
	return dnp3.CommandSuccess
}

func (c *countingHandler) OperateAnalog(uint16, outstation.AnalogOutput, outstation.OperateType) dnp3.CommandStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.operates++
	return dnp3.CommandSuccess
}

// sendRaw transmits a request with the application control field set exactly
// as given, which the harness's own send does not allow: it always builds a
// well-formed FIR+FIN request with a fresh sequence number.
func sendRaw(h *harness, ctrl app.Control, fc app.FuncCode, objs ...app.ObjectHeader) {
	h.t.Helper()
	frag := app.BuildRequest(nil, ctrl, fc, objs...)
	if err := h.stack.Send(h.conn, frag); err != nil {
		h.t.Fatalf("send: %v", err)
	}
}

// Report: application fragment state is not validated. The outstation
// dispatches every request fragment the moment it parses, keeping no record
// of the last sequence number it acted on.
//
// A master retransmits a request whenever it does not see the response —
// the response was lost, or its own link-layer retry fired — and the
// retransmission carries the same sequence number precisely so the
// outstation can recognise it. IEEE 1815 requires the outstation to answer a
// repeat with the previous response rather than executing it a second time.
// With no such check, a lost response to a control turns one operator action
// into two operations of the point.
func TestDuplicateRequestIsNotExecutedTwice(t *testing.T) {
	rec := &countingHandler{}
	h := newHarness(t, outstation.Config{Database: smallDB()}, rec)

	ctrl := app.Control{Fir: true, Fin: true, Seq: 3}
	operate := crobHeader(0, dnp3.ControlLatchOn)

	before := h.count()
	sendRaw(h, ctrl, app.FuncDirectOperate, operate)
	first := h.await(before)
	if got := commandStatus(t, first); got != dnp3.CommandSuccess {
		t.Fatalf("the first operate was not accepted (%v); the test proves nothing", got)
	}
	if rec.count() != 1 {
		t.Fatalf("operates = %d after one request, want 1", rec.count())
	}

	// The identical request again, same sequence number: a retransmission,
	// not a second operator action.
	before = h.count()
	sendRaw(h, ctrl, app.FuncDirectOperate, operate)
	h.await(before)

	if rec.count() != 1 {
		t.Errorf("operates = %d, want 1: a retransmitted request carrying the same "+
			"sequence number was executed a second time, so a single control "+
			"operated the point twice", rec.count())
	}
}

// A fragment with FIR clear is a continuation of a request series, not a
// request. Acting on one means acting on part of a request whose beginning
// either never arrived or was discarded — the outstation cannot yet know
// what is being asked of it.
func TestRequestFragmentWithoutFirIsNotExecuted(t *testing.T) {
	rec := &countingHandler{}
	h := newHarness(t, outstation.Config{Database: smallDB()}, rec)

	// FIR clear and FIN clear: unambiguously a middle fragment, with no
	// series in progress for it to belong to.
	sendRaw(h, app.Control{Fir: false, Fin: false, Seq: 5},
		app.FuncDirectOperate, crobHeader(0, dnp3.ControlLatchOn))

	time.Sleep(200 * time.Millisecond)

	if rec.count() != 0 {
		t.Errorf("operates = %d, want 0: a control carried by a fragment that is not the "+
			"start of a request was executed as though the request were complete",
			rec.count())
	}
}
