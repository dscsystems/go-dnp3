// Package sechub is the security-event plumbing the master and outstation
// sessions share: the emitter a policy opens, and the per-connection emitter
// that stamps events with the peer's identity.
package sechub

import (
	"io"
	"sync/atomic"
	"time"

	"github.com/dscsystems/go-dnp3/secevent"
)

// Hub holds a session's emitters. The zero Hub reports nothing.
//
// The session goroutine emits through it; ReportCyberEvent, PeerInfo and
// Stats may be called from any goroutine, which is why the per-connection
// emitter is an atomic pointer.
type Hub struct {
	policy *secevent.Policy
	base   *secevent.Emitter
	done   func()
	cur    atomic.Pointer[secevent.Emitter]
	since  time.Time
}

// Start opens the policy's emitter for a session in role and attaches it to
// the channel, if the channel reports events of its own.
func (h *Hub) Start(p *secevent.Policy, role string, ch any) {
	h.policy = p
	h.base, h.done = p.Open(role)
	if a, ok := ch.(secevent.Attachable); ok && h.base != nil {
		a.AttachSecurity(h.base)
	}
}

// Stop closes a dispatcher the session owns.
func (h *Hub) Stop() {
	if h.done != nil {
		h.done()
	}
}

// Enabled reports whether events go anywhere.
func (h *Hub) Enabled() bool { return h.base != nil }

// Audit reports whether audit-trail events are wanted.
func (h *Hub) Audit() bool { return h.base != nil && h.policy.Audit() }

// Connected binds a new connection: later events carry its identity.
func (h *Hub) Connected(conn io.ReadWriteCloser) *secevent.Emitter {
	if h.base == nil {
		return nil
	}
	em := h.base.ForConn(secevent.NextConnID(), secevent.PeerOf(conn))
	h.cur.Store(em)
	h.since = time.Now()
	return em
}

// Disconnected unbinds the connection and returns how long it lasted.
func (h *Hub) Disconnected() time.Duration {
	h.cur.Store(nil)
	if h.since.IsZero() {
		return 0
	}
	return time.Since(h.since)
}

// Emitter returns the connection's emitter, or the session's between
// connections, or nil when reporting is off.
func (h *Hub) Emitter() *secevent.Emitter {
	if em := h.cur.Load(); em != nil {
		return em
	}
	return h.base
}

// Emit submits an event.
func (h *Hub) Emit(e secevent.Event) {
	if h.base == nil {
		return
	}
	h.Emitter().Emit(e)
}

// Peer returns the current connection's peer.
func (h *Hub) Peer() (secevent.PeerInfo, bool) {
	if em := h.cur.Load(); em != nil {
		return em.Peer(), true
	}
	return secevent.PeerInfo{}, false
}

// Stats returns the dispatcher's counters.
func (h *Hub) Stats() secevent.Stats { return h.base.Dispatcher().Stats() }
