package sechub

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"time"

	"github.com/dscsystems/go-dnp3/netpeer"
	"github.com/dscsystems/go-dnp3/secevent"
)

func macSource(s netpeer.Source) secevent.MACSource {
	switch s {
	case netpeer.SourceNeighbor:
		return secevent.MACNeighbor
	case netpeer.SourceGateway:
		return secevent.MACGateway
	case netpeer.SourceStatic:
		return secevent.MACStatic
	case netpeer.SourceCustom:
		return secevent.MACCustom
	}
	return secevent.MACNone
}

func (h *Hub) cache() *netpeer.Cache {
	if h.policy.MACCache != nil {
		return h.policy.MACCache
	}
	return netpeer.DefaultCache()
}

// lookup fills in the peer's MAC address.
func (h *Hub) lookup(peer *secevent.PeerInfo) (netpeer.Result, bool) {
	p := h.policy
	if p.DisableMACLookup || !peer.RemoteAddr.IsValid() {
		return netpeer.Result{}, false
	}
	timeout := p.MACLookupTimeout
	if timeout <= 0 {
		timeout = secevent.DefaultMACLookupTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	res, err := netpeer.Lookup(ctx, p.Resolver, peer.RemoteAddr.Addr())
	peer.MAC = res.MAC
	peer.MACSource = macSource(res.Source)
	peer.OnLink = res.OnLink
	peer.Interface = res.Interface
	return res, err == nil
}

// identify looks the peer up, compares it with what was seen before and with
// the pinned bindings, and reports what it finds. It returns false when the
// policy says the connection must not be served.
func (h *Hub) identify(em *secevent.Emitter, peer secevent.PeerInfo, res netpeer.Result, looked bool) bool {
	p := h.policy
	ip := peer.RemoteAddr.Addr()

	if looked && res.OnLink && !res.Local && res.Source == netpeer.SourceNone {
		em.Emit(secevent.Event{Kind: secevent.KindPeerMACUnavailable,
			Detail: "the peer is on-link but has no neighbour table entry"})
	}

	if ip.IsValid() {
		o := netpeer.Observation{IP: ip, MAC: res.MAC, Source: res.Source}
		if peer.TLS != nil {
			o.CertSHA256 = peer.TLS.FingerprintSHA256
		}
		for _, c := range h.cache().Observe(o) {
			h.reportChange(em, c, false)
		}
	}

	if t := peer.TLS; t != nil && !t.NotAfter.IsZero() {
		if w := p.CertExpiry(); w > 0 && time.Until(t.NotAfter) < w {
			em.Emit(secevent.Event{Kind: secevent.KindTLSCertExpiring,
				Detail: fmt.Sprintf("the peer's certificate %q expires %s", t.Subject, t.NotAfter.Format(time.RFC3339))})
		}
	}

	want, pinned := p.PeerBindings[ip]
	if !pinned || bytes.Equal(want, peer.MAC) {
		return true
	}
	got := "none"
	if peer.MAC != nil {
		got = peer.MAC.String()
	}
	detail := fmt.Sprintf("%s is pinned to MAC %s but connected with %s", ip, want, got)
	em.Emit(secevent.Event{Kind: secevent.KindPeerMACPinningViolation, Detail: detail})
	if !p.RejectBindingMismatch {
		return true
	}
	em.Emit(secevent.Event{Kind: secevent.KindConnUnexpectedMAC, Action: secevent.Rejected, Detail: detail})
	return false
}

func (h *Hub) reportChange(em *secevent.Emitter, c netpeer.Change, midSession bool) {
	since := ""
	if !c.LastSeen.IsZero() {
		since = " (last seen " + c.LastSeen.Format(time.RFC3339) + ")"
	}
	switch c.Kind {
	case netpeer.MACChanged:
		kind := secevent.KindPeerMACChanged
		if midSession {
			kind = secevent.KindPeerMACChangedMidsession
		}
		em.Emit(secevent.Event{Kind: kind,
			Detail: fmt.Sprintf("%s changed MAC from %s to %s%s", c.IP, c.OldMAC, c.NewMAC, since)})
	case netpeer.IPChanged:
		em.Emit(secevent.Event{Kind: secevent.KindPeerIPChanged,
			Detail: fmt.Sprintf("MAC %s moved from %s to %s%s", c.NewMAC, c.OldIP, c.IP, since)})
	case netpeer.MACShared:
		em.Emit(secevent.Event{Kind: secevent.KindPeerMACShared,
			Detail: fmt.Sprintf("MAC %s is in use at both %s and %s", c.NewMAC, c.OldIP, c.IP)})
	case netpeer.LinkAddrChanged:
		em.Emit(secevent.Event{Kind: secevent.KindPeerLinkAddrChanged,
			Fields: secevent.HasLinkAddrs, LinkSrc: c.NewLinkAddr,
			Detail: fmt.Sprintf("%s used DNP3 address %d, now %d%s", c.IP, c.OldLinkAddr, c.NewLinkAddr, since)})
	case netpeer.CertChanged:
		em.Emit(secevent.Event{Kind: secevent.KindTLSCertChanged,
			Detail: fmt.Sprintf("%s presented certificate %.16s…, previously %.16s…", c.IP, c.NewCert, c.OldCert)})
	}
}

// SawLinkSource notes the DNP3 source address of a frame from the peer. The
// first one on each connection is compared with what the peer's IP used
// before. It runs on the session goroutine.
func (h *Hub) SawLinkSource(src uint16) {
	if h.base == nil || h.linkSeen {
		return
	}
	h.linkSeen = true
	em := h.cur.Load()
	if em == nil {
		return
	}
	ip := em.Peer().RemoteAddr.Addr()
	if !ip.IsValid() {
		return
	}
	for _, c := range h.cache().Observe(netpeer.Observation{IP: ip, LinkAddr: src, HasLinkAddr: true}) {
		h.reportChange(em, c, false)
	}
}

// recheck looks the peer's MAC up again periodically while the connection is
// open, closing it on a change when the policy says to.
func (h *Hub) recheck(em *secevent.Emitter, conn io.Closer, ip netip.Addr, mac net.HardwareAddr, stop <-chan struct{}) {
	t := time.NewTicker(h.policy.MACRecheckInterval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		peer := secevent.PeerInfo{RemoteAddr: netip.AddrPortFrom(ip, 0)}
		res, ok := h.lookup(&peer)
		if !ok || !peersOwnMAC(res.Source) || bytes.Equal(res.MAC, mac) {
			continue
		}
		h.cache().Observe(netpeer.Observation{IP: ip, MAC: res.MAC, Source: res.Source})
		h.reportChange(em, netpeer.Change{Kind: netpeer.MACChanged, IP: ip, OldMAC: mac, NewMAC: res.MAC}, true)
		mac = res.MAC
		if h.policy.DisconnectOnMACChange {
			_ = conn.Close()
			return
		}
	}
}

// peerChecker is a connection that learns its peer after it opens: a UDP
// socket answering whoever writes first.
type peerChecker interface {
	SetPeerCheck(func(netip.AddrPort) bool)
}

// admitLearned applies the allowlist and the identity checks to a peer the
// connection opened by em has just learned. It runs on the connection's
// reading goroutine and reports whether the peer may be served.
func (h *Hub) admitLearned(em *secevent.Emitter, peer secevent.PeerInfo, ap netip.AddrPort) bool {
	p := h.policy
	peer.RemoteAddr = ap
	if peer.Network == "" {
		peer.Network = "udp"
	}
	ip := ap.Addr()
	known := em.ForConn(em.ConnID(), peer)
	if !p.PeerAllowed(ip) {
		ev := secevent.Event{Kind: secevent.KindConnUnexpected,
			Detail: fmt.Sprintf("%s is not in AllowedPeers", ip)}
		if p.RejectUnlisted {
			ev.Action = secevent.Rejected
			known.Emit(ev)
			return false
		}
		known.Emit(ev)
	}
	res, looked := h.lookup(&peer)
	known = em.ForConn(em.ConnID(), peer)
	if !h.identify(known, peer, res, looked) {
		return false
	}
	// Later events on this connection name the peer; unless it has
	// already ended.
	h.cur.CompareAndSwap(em, known)
	return true
}
