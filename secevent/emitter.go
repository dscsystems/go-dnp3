package secevent

import (
	"sync/atomic"
	"time"
)

var connIDs atomic.Uint64

// NextConnID returns a process-wide unique connection identifier, for
// correlating the events of one connection.
func NextConnID() uint64 { return connIDs.Add(1) }

// Emitter stamps events with where they came from and submits them to a
// dispatcher. An Emitter is immutable; [Emitter.ForConn] derives one for a
// connection.
//
// Every method is safe on a nil *Emitter and does nothing, which is how a
// session without a policy pays only a nil check.
type Emitter struct {
	d         *Dispatcher
	role      string
	transport string
	connID    uint64
	peer      PeerInfo
	maxRaw    int
	policy    *Policy
}

// NewEmitter returns an emitter for one endpoint in the given role
// ("master", "outstation", "bus"). maxRaw bounds the octets of Raw kept;
// negative keeps none.
func NewEmitter(d *Dispatcher, role string, maxRaw int) *Emitter {
	if d == nil {
		return nil
	}
	return &Emitter{d: d, role: role, maxRaw: maxRaw}
}

// ForConn returns an emitter whose events carry a connection's identity.
func (em *Emitter) ForConn(connID uint64, peer PeerInfo) *Emitter {
	if em == nil {
		return nil
	}
	c := *em
	c.connID, c.peer = connID, peer
	c.transport = transportOf(peer)
	return &c
}

// WithTransport returns an emitter that reports the given transport name.
func (em *Emitter) WithTransport(transport string) *Emitter {
	if em == nil {
		return nil
	}
	c := *em
	c.transport = transport
	return &c
}

// Enabled reports whether events go anywhere. Call sites that would have to
// build an expensive Detail check it first.
func (em *Emitter) Enabled() bool { return em != nil }

// ConnID returns the connection identifier, zero when not bound to one.
func (em *Emitter) ConnID() uint64 {
	if em == nil {
		return 0
	}
	return em.connID
}

// Peer returns the connection's peer.
func (em *Emitter) Peer() PeerInfo {
	if em == nil {
		return PeerInfo{}
	}
	return em.peer
}

// Transport returns the transport events are stamped with.
func (em *Emitter) Transport() string {
	if em == nil {
		return ""
	}
	return em.transport
}

// Policy returns the policy the emitter was opened from, or nil.
func (em *Emitter) Policy() *Policy {
	if em == nil {
		return nil
	}
	return em.policy
}

// Dispatcher returns the dispatcher events go to.
func (em *Emitter) Dispatcher() *Dispatcher {
	if em == nil {
		return nil
	}
	return em.d
}

// Emit fills in the event's defaults and submits it. It never blocks.
func (em *Emitter) Emit(e Event) {
	if em == nil {
		return
	}
	em.d.Submit(em.Stamp(e))
}

// Stamp fills in what the emitter knows and the kind's defaults, leaving
// anything the event already sets.
func (em *Emitter) Stamp(e Event) Event {
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	info := kindTable[e.Kind]
	if e.Category == 0 {
		e.Category = info.cat
	}
	if e.Severity == 0 {
		e.Severity = info.sev
	}
	if e.Technique == "" {
		e.Technique = info.technique
	}
	if em == nil {
		return e
	}
	e.Protocol = "dnp3"
	if e.Role == "" {
		e.Role = em.role
	}
	if e.Transport == "" {
		e.Transport = em.transport
	}
	if e.ConnID == 0 {
		e.ConnID = em.connID
	}
	if e.Peer.addressless() {
		// Keep a certificate the event carries — a TLS refusal names the
		// peer's — and take the rest from the connection.
		tls := e.Peer.TLS
		e.Peer = em.peer
		if tls != nil {
			e.Peer.TLS = tls
		}
	}
	switch {
	case em.maxRaw < 0 || (e.Raw != nil && secretBearing(e.Raw)):
		// Never keep octets that may hold a password or a key, whatever
		// the event: a sink is a log, and logs travel.
		e.Raw = nil
	case e.Raw != nil:
		n := min(len(e.Raw), em.maxRaw)
		e.Raw = append([]byte(nil), e.Raw[:n]...)
	}
	return e
}

func transportOf(p PeerInfo) string {
	switch {
	case p.TLS != nil:
		return "tls"
	case p.SerialPort != "":
		return "serial"
	default:
		return p.Network
	}
}

// Reporter is implemented by sessions, so code that holds one — a command
// handler, say — can report what only it knows. The session fills in its
// identity and the connection's.
type Reporter interface {
	ReportCyberEvent(Event)
}
