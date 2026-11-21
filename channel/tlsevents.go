package channel

import (
	"crypto/tls"
	"fmt"
	"net"
	"time"

	"github.com/dscsystems/go-dnp3/secevent"
)

// tlsFailed reports a failed handshake with the peer of raw.
func (h *secHolder) tlsFailed(raw net.Conn, err error) {
	em := h.em.Load()
	if em == nil {
		return
	}
	ev, ok := secevent.ClassifyTLS(err)
	if !ok {
		ev = secevent.Event{Kind: secevent.KindTLSHandshakeFailed, Err: err, Detail: err.Error()}
	}
	ev.Action = secevent.Rejected
	tlsInfo := ev.Peer.TLS
	if raw != nil {
		ev.Peer = secevent.PeerOf(raw)
		ev.Peer.TLS = tlsInfo
	}
	em.Emit(ev)
}

// tlsEstablished reports weak parameters on a completed handshake.
func (h *secHolder) tlsEstablished(c *tls.Conn) {
	em := h.em.Load()
	if em == nil {
		return
	}
	st := c.ConnectionState()
	weak := st.Version < tls.VersionTLS12
	for _, s := range tls.InsecureCipherSuites() {
		weak = weak || s.ID == st.CipherSuite
	}
	if weak {
		em.Emit(secevent.Event{Kind: secevent.KindTLSWeakParams, Peer: secevent.PeerOf(c),
			Detail: fmt.Sprintf("negotiated %s with %s", tls.VersionName(st.Version), tls.CipherSuiteName(st.CipherSuite))})
	}
}

// ownCertExpiry reports this end's certificate expiring soon.
func (h *secHolder) ownCertExpiry(cfg *tls.Config) {
	em := h.em.Load()
	if em == nil || len(cfg.Certificates) == 0 {
		return
	}
	window := em.Policy().CertExpiry()
	leaf := cfg.Certificates[0].Leaf
	if window <= 0 || leaf == nil {
		return
	}
	if left := time.Until(leaf.NotAfter); left < window {
		em.Emit(secevent.Event{Kind: secevent.KindTLSCertExpiring, Severity: expirySeverity(left),
			Detail: fmt.Sprintf("our own certificate %q expires %s", leaf.Subject.String(), leaf.NotAfter.Format(time.RFC3339))})
	}
}

func expirySeverity(left time.Duration) secevent.Severity {
	if left <= 0 {
		return secevent.Warning
	}
	return secevent.Notice
}
