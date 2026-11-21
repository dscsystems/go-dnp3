// Package sechub is the security-event plumbing the master and outstation
// sessions share: the emitter a policy opens, and the per-connection emitter
// that stamps events with the peer's identity.
package sechub

import (
	"io"
	"sync/atomic"
	"time"

	"github.com/dscsystems/go-dnp3/internal/secmon"
	"github.com/dscsystems/go-dnp3/netpeer"
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
	// shared is base, for the methods other goroutines call.
	shared atomic.Pointer[secevent.Emitter]
	since  time.Time

	// linkSeen says the connection's first DNP3 source has been noted, and
	// stopRecheck ends the connection's MAC re-check, if one runs.
	linkSeen    bool
	stopRecheck chan struct{}

	// scan is the connection's probe detector; dups outlives connections.
	scan scan
	dups *secmon.DupWindow
	// rates are the connection's rate detectors, by name.
	rates map[string]*rateState
	// drop names a flood whose policy is to disconnect.
	drop string
	// backlog is the connection's receive queue detector.
	backlog backlog
}

// Start opens the policy's emitter for a session in role and attaches it to
// the channel, if the channel reports events of its own.
func (h *Hub) Start(p *secevent.Policy, role string, ch any) {
	h.policy = p
	h.base, h.done = p.Open(role)
	h.shared.Store(h.base)
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

// Connected binds a new connection: later events carry its identity. It
// looks the peer's MAC up and checks it against history and the pinned
// bindings, reporting what it finds, and returns false when the policy says
// the connection must be refused. Event kind opened is emitted first.
func (h *Hub) Connected(conn io.ReadWriteCloser, opened secevent.Kind) bool {
	if h.base == nil {
		return true
	}
	peer := secevent.PeerOf(conn)
	res, looked := h.lookup(&peer)
	em := h.base.ForConn(secevent.NextConnID(), peer)
	h.cur.Store(em)
	h.since = time.Now()
	h.linkSeen = false
	h.scan = scan{}
	h.rates = nil
	h.drop = ""
	h.backlog = backlog{}

	detail := "connection opened"
	if peer.MAC != nil {
		detail += " from MAC " + peer.MAC.String() + " (" + peer.MACSource.String() + ")"
	}
	em.Emit(secevent.Event{Kind: opened, Detail: detail})
	if !h.identify(em, peer, res, looked) {
		return false
	}
	if h.policy.MACRecheckInterval > 0 && peersOwnMAC(res.Source) {
		h.stopRecheck = make(chan struct{})
		go h.recheck(em, conn, peer.RemoteAddr.Addr(), peer.MAC, h.stopRecheck)
	}
	return true
}

// Disconnected unbinds the connection and returns how long it lasted.
func (h *Hub) Disconnected() time.Duration {
	if h.stopRecheck != nil {
		close(h.stopRecheck)
		h.stopRecheck = nil
	}
	h.cur.Store(nil)
	if h.since.IsZero() {
		return 0
	}
	return time.Since(h.since)
}

// Emitter returns the connection's emitter, or the session's between
// connections, or nil when reporting is off. It is safe from any goroutine.
func (h *Hub) Emitter() *secevent.Emitter {
	if em := h.cur.Load(); em != nil {
		return em
	}
	return h.shared.Load()
}

// Emit submits an event. It is safe from any goroutine.
func (h *Hub) Emit(e secevent.Event) { h.Emitter().Emit(e) }

// AuditFromAnywhere is Audit for goroutines other than the session's.
func (h *Hub) AuditFromAnywhere() bool { return h.Emitter().Policy().Audit() }

// Peer returns the current connection's peer.
func (h *Hub) Peer() (secevent.PeerInfo, bool) {
	if em := h.cur.Load(); em != nil {
		return em.Peer(), true
	}
	return secevent.PeerInfo{}, false
}

// Stats returns the dispatcher's counters.
func (h *Hub) Stats() secevent.Stats { return h.shared.Load().Dispatcher().Stats() }

// peersOwnMAC reports whether a MAC from this source is the peer's own rather
// than a router's.
func peersOwnMAC(s netpeer.Source) bool {
	return s == netpeer.SourceNeighbor || s == netpeer.SourceStatic || s == netpeer.SourceCustom
}
