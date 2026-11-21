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

// FloodMode says what a caller can do about traffic over a flood limit.
type FloodMode uint8

const (
	// Refusable traffic can be throttled or disconnected, as the policy says.
	Refusable FloodMode = iota
	// Unrefusable traffic was already processed, or cannot be refused
	// without losing data: Throttled only reports, Disconnected still drops.
	Unrefusable
	// ReportOnly traffic is never acted on: refusing it would break the link
	// or authentication itself.
	ReportOnly
)

// Flood counts n occurrences of flood class k against rate r. Over the
// rate, it reports the flood once and returns the action taken for this
// occurrence: Throttled (refuse it) or Disconnected (drop the connection,
// see Drop). Within the rate, or when mode rules the policy's action out, it
// returns Observed: carry on. The event records the action actually taken.
func (h *Hub) Flood(k secevent.Kind, r secevent.Rate, n int, mode FloodMode, e secevent.Event) secevent.Action {
	return h.flood(string(k), k, r, n, mode, e)
}

// flood is Flood with the rate detector named by key, for a kind fed by
// more than one limit.
func (h *Hub) flood(key string, k secevent.Kind, r secevent.Rate, n int, mode FloodMode, e secevent.Event) secevent.Action {
	over, first := h.limit(key, r, n)
	if !over {
		return secevent.Observed
	}
	action := h.policy.Flood()
	switch {
	case mode == ReportOnly:
		action = secevent.Observed
	case mode == Unrefusable && action == secevent.Throttled:
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
		h.drop = key
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

// backlog tracks how long a session's receive queue has been full.
type backlog struct {
	since    time.Time
	reported bool
}

// BacklogAfter is how long the receive queue must stay full before
// dos.rx_backlog.
const BacklogAfter = time.Second

// Backlog reports dos.rx_backlog when the receive queue has been full for
// BacklogAfter: the peer sends faster than this end processes. It re-arms
// once the queue is below half.
func (h *Hub) Backlog(queued, capacity int, now time.Time) {
	if h.base == nil {
		return
	}
	b := &h.backlog
	switch {
	case queued < capacity/2:
		*b = backlog{}
	case queued < capacity:
	case b.since.IsZero():
		b.since = now
	case !b.reported && now.Sub(b.since) >= BacklogAfter:
		b.reported = true
		h.Emit(secevent.Event{Kind: secevent.KindDoSRxBacklog,
			Detail: fmt.Sprintf("receive queue full for %v: the peer sends faster than it is processed",
				now.Sub(b.since).Round(time.Millisecond))})
	}
}
