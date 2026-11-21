package sechub

import (
	"crypto/sha256"
	"time"

	"github.com/dscsystems/go-dnp3/internal/secmon"
)

// maxDupEntries bounds the duplicate window's memory.
const maxDupEntries = 1024

// Duplicate notes a state-changing request's function and object octets,
// and reports the connection that sent the same within the policy's window,
// if another did. The window spans connections: that is the point of it.
func (h *Hub) Duplicate(fc uint8, objects []byte) (prevConn uint64, dup bool) {
	if h.base == nil {
		return 0, false
	}
	window := h.policy.Duplicates()
	if window == 0 {
		return 0, false
	}
	if h.dups == nil {
		h.dups = secmon.NewDupWindow(maxDupEntries, window)
	}
	d := sha256.New()
	d.Write([]byte{fc})
	d.Write(objects)
	var key [32]byte
	d.Sum(key[:0])
	return h.dups.Seen(key, h.Emitter().ConnID(), time.Now())
}
