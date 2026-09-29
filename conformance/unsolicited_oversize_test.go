package conformance

import (
	"testing"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/outstation"
)

// An unsolicited response is one fragment. When more events were selected than
// fitted, the surplus stayed selected and the confirm removed it, so a master
// saw a handful of events out of dozens and nothing said the rest were gone.
func TestUnsolicitedSurplusEventsAreNotDiscarded(t *testing.T) {
	const points = 60

	db := smallDB()
	db.Binary = points
	h := newHarness(t, outstation.Config{
		Database:      db,
		MaxTxFragment: 40,
		Unsolicited: outstation.UnsolicitedConfig{
			Enabled:        true,
			ConfirmTimeout: time.Second,
			MaxRetries:     3,
		},
	}, nil)

	null := h.await(0)
	sendUnsolConfirm(h, null.Header.Control.Seq)
	enableUnsolicited(h)

	seen := map[uint32]bool{}
	h.out.Update(func(d *outstation.Database) {
		for i := range points {
			d.UpdateBinary(uint16(i), dnp3.Binary{Value: true, Flags: dnp3.Online})
		}
	})

	next := h.count()
	for len(seen) < points {
		frag := h.await(next)
		next++
		for _, o := range frag.Objects {
			if o.Group != 2 {
				continue
			}
			// Each event is a one-octet index followed by its value octets.
			step := len(o.Data) / int(o.Range.Count)
			for k := 0; k < int(o.Range.Count); k++ {
				seen[uint32(o.Data[k*step])] = true
			}
		}
		sendUnsolConfirm(h, frag.Header.Control.Seq)
	}

	if len(seen) != points {
		t.Fatalf("master saw %d of %d events", len(seen), points)
	}
}
