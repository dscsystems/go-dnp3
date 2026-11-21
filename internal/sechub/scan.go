package sechub

import (
	"fmt"
	"time"

	"github.com/dscsystems/go-dnp3/secevent"
)

// scan counts the distinct unknown things one connection asked for: function
// codes, objects, points. A master that knows its outstation asks for few;
// one probing to find out what is there asks for many (T0846, T0861).
type scan struct {
	seen     map[string]time.Time
	reported time.Time
}

// maxScanKeys bounds the memory a hostile peer can make the tracker use.
const maxScanKeys = 1024

// Unknown notes that the peer asked for something this station does not
// have, identified by key, and raises access.scan when the policy's
// threshold of distinct keys is reached within its window.
func (h *Hub) Unknown(key string) {
	if h.base == nil {
		return
	}
	n, window := h.policy.Scan()
	if n == 0 {
		return
	}
	now := time.Now()
	if h.scan.seen == nil {
		h.scan.seen = make(map[string]time.Time)
	}
	for k, t := range h.scan.seen {
		if now.Sub(t) > window {
			delete(h.scan.seen, k)
		}
	}
	if len(h.scan.seen) < maxScanKeys {
		h.scan.seen[key] = now
	}
	if len(h.scan.seen) >= n && now.Sub(h.scan.reported) > window {
		h.scan.reported = now
		h.Emit(secevent.Event{Kind: secevent.KindAccessScan,
			Detail: fmt.Sprintf("%d distinct unknown functions, objects or points asked for within %v", len(h.scan.seen), window)})
	}
}
