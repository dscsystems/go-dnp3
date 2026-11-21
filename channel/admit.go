package channel

import (
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/dscsystems/go-dnp3/internal/secmon"
	"github.com/dscsystems/go-dnp3/secevent"
)

// admission applies a policy's allowlist and accept rates to connections a
// server channel accepts, before any TLS handshake or session.
type admission struct {
	mu     sync.Mutex
	perIP  *secmon.Keyed[netip.Addr]
	global secmon.Bucket
	built  bool
}

func (a *admission) build(p *secevent.Policy) {
	if a.built {
		return
	}
	a.built = true
	if r := p.AcceptRate.Or(secevent.DefaultAcceptRate); !r.Off() {
		a.perIP = secmon.NewKeyed[netip.Addr](r.N, r.Per, 4096)
	}
	if r := p.GlobalAcceptRate.Or(secevent.DefaultGlobalAcceptRate); !r.Off() {
		a.global = secmon.NewBucket(r.N, r.Per)
	}
}

// admit reports on a newly accepted connection and decides whether it may be
// served. A connection it refuses has already been closed.
func (h *secHolder) admit(c net.Conn) bool {
	em := h.em.Load()
	if em == nil {
		return true
	}
	p := em.Policy()
	peer := secevent.PeerOf(c)
	ip := peer.RemoteAddr.Addr()
	if !ip.IsValid() {
		return true
	}

	if !p.PeerAllowed(ip) {
		ev := secevent.Event{Kind: secevent.KindConnUnexpected, Peer: peer,
			Detail: fmt.Sprintf("%s is not in AllowedPeers", ip)}
		if p.RejectUnlisted {
			ev.Action = secevent.Rejected
			em.Emit(ev)
			_ = c.Close()
			return false
		}
		em.Emit(ev)
	}

	now := time.Now()
	h.adm.mu.Lock()
	h.adm.build(p)
	ipOK := h.adm.perIP == nil || h.adm.perIP.Allow(ip, now)
	globalOK := h.adm.global.Allow(now)
	h.adm.mu.Unlock()
	if ipOK && globalOK {
		return true
	}
	detail := fmt.Sprintf("new connections from %s exceed AcceptRate", ip)
	if ipOK {
		detail = "new connections from all peers exceed GlobalAcceptRate"
	}
	ev := secevent.Event{Kind: secevent.KindConnRate, Peer: peer, Detail: detail}
	if p.RejectOverRate {
		ev.Action = secevent.Rejected
		em.Emit(ev)
		_ = c.Close()
		return false
	}
	em.Emit(ev)
	return true
}
