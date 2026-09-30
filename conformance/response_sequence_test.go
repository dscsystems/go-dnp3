package conformance

import (
	"testing"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/outstation"
)

// The first fragment of a response carries the request's sequence number and
// each later one increments it; a confirm names the fragment it acknowledges.
func TestMultiFragmentResponseSequenceNumbersIncrement(t *testing.T) {
	h := newHarness(t, outstation.Config{
		Database:       outstation.DatabaseConfig{Binary: 60, DefaultClass: dnp3.Class1},
		MaxTxFragment:  30, // forces several small fragments
		ConfirmTimeout: 2 * time.Second,
	}, nil)

	before := h.count()
	h.send(app.FuncRead, app.ReadAllObjects(60, 1))
	reqSeq := h.seq

	frag := h.await(before)
	for i := 0; ; i++ {
		want := (reqSeq + uint8(i)) % app.SeqModulus
		if got := frag.Header.Control.Seq; got != want {
			t.Fatalf("fragment %d has sequence %d, want %d", i, got, want)
		}
		if i == 0 != frag.Header.Control.Fir {
			t.Fatalf("fragment %d has FIR = %v", i, frag.Header.Control.Fir)
		}
		if frag.Header.Control.Fin {
			if i == 0 {
				t.Fatal("the response fit in one fragment; the test needs several")
			}
			return
		}
		if !frag.Header.Control.Con {
			t.Fatalf("fragment %d of a series does not ask for a confirm", i)
		}
		before = h.count()
		h.sendConfirm(frag.Header.Control.Seq)
		frag = h.await(before)
	}
}
