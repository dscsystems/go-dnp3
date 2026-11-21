package sechub

import (
	"fmt"
	"time"

	"github.com/dscsystems/go-dnp3/internal/secmon"
	"github.com/dscsystems/go-dnp3/secevent"
)

// rateState is one per-connection rate detector.
type rateState struct {
	b       secmon.Bucket
	tripped bool
}

// limit counts n occurrences of what key names against rate r on the
// current connection. over says the rate is exceeded now; first says this
// is the first occurrence over it since the rate was last respected, so a
// sustained flood is reported once.
func (h *Hub) limit(key string, r secevent.Rate, n int) (over, first bool) {
	if h.base == nil || r.Off() {
		return false, false
	}
	if h.rates == nil {
		h.rates = make(map[string]*rateState)
	}
	st := h.rates[key]
	if st == nil {
		st = &rateState{b: secmon.NewBucket(r.N, r.Per)}
		h.rates[key] = st
	}
	if st.b.AllowN(time.Now(), n) {
		st.tripped = false
		return false, false
	}
	first = !st.tripped
	st.tripped = true
	return true, first
}

// Exceeded counts one occurrence against rate r and reports true the first
// time the rate is exceeded, then not again until it has been respected.
func (h *Hub) Exceeded(key string, r secevent.Rate) bool {
	_, first := h.limit(key, r, 1)
	return first
}

// Over counts one occurrence against rate r and reports whether the rate is
// exceeded now.
func (h *Hub) Over(key string, r secevent.Rate) bool {
	over, _ := h.limit(key, r, 1)
	return over
}

// Flood counts n occurrences of flood class k against rate r. Over the
// rate, it reports the flood once and returns the policy's action for this
// occurrence: Throttled (refuse it) or Disconnected (drop the connection,
// see Drop). Within the rate, or for a class that is only ever reported
// (reportOnly), it returns Observed: carry on. A caller that cannot refuse
// one occurrence treats Throttled as Observed.
func (h *Hub) Flood(k secevent.Kind, r secevent.Rate, n int, reportOnly bool, e secevent.Event) secevent.Action {
	over, first := h.limit(string(k), r, n)
	if !over {
		return secevent.Observed
	}
	action := h.policy.Flood()
	if reportOnly {
		action = secevent.Observed
	}
	if first {
		e.Kind, e.Action = k, action
		if e.Detail == "" {
			e.Detail = fmt.Sprintf("more than %d in %v", r.N, r.Per)
		} else {
			e.Detail += fmt.Sprintf(": more than %d in %v", r.N, r.Per)
		}
		h.Emit(e)
	}
	if action == secevent.Disconnected {
		h.drop = string(k)
	}
	return action
}

// Drop returns the flood class that asked for the connection to be
// dropped, if one did, and forgets it.
func (h *Hub) Drop() (string, bool) {
	d := h.drop
	h.drop = ""
	return d, d != ""
}
