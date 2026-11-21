package sechub

import (
	"time"

	"github.com/dscsystems/go-dnp3/internal/secmon"
	"github.com/dscsystems/go-dnp3/secevent"
)

// rateState is one per-connection rate detector.
type rateState struct {
	b       secmon.Bucket
	tripped bool
}

// Exceeded counts one occurrence of what key names against rate r, on the
// current connection, and reports true the first time the rate is exceeded.
// It reports false again until the rate has been respected once more, so a
// sustained flood is one event, not one per occurrence. An Off rate never
// trips.
func (h *Hub) Exceeded(key string, r secevent.Rate) bool {
	if h.base == nil || r.Off() {
		return false
	}
	if h.rates == nil {
		h.rates = make(map[string]*rateState)
	}
	st := h.rates[key]
	if st == nil {
		st = &rateState{b: secmon.NewBucket(r.N, r.Per)}
		h.rates[key] = st
	}
	if st.b.Allow(time.Now()) {
		st.tripped = false
		return false
	}
	if st.tripped {
		return false
	}
	st.tripped = true
	return true
}
