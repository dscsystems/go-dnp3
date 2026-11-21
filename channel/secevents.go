package channel

import (
	"fmt"
	"net"
	"sync/atomic"
	"time"

	"github.com/dscsystems/go-dnp3/secevent"
)

// secHolder carries the emitter a session attached to a channel, for the
// events that happen before any session sees a connection: accept failures,
// refused connections, failed dials and handshakes.
type secHolder struct {
	em atomic.Pointer[secevent.Emitter]
}

func (h *secHolder) attach(em *secevent.Emitter, transport string) {
	h.em.Store(em.WithTransport(transport))
}

func (h *secHolder) emit(e secevent.Event) {
	if em := h.em.Load(); em != nil {
		em.Emit(e)
	}
}

func (h *secHolder) enabled() bool { return h.em.Load() != nil }

// acceptFailed reports a listener error other than the listener closing.
func (h *secHolder) acceptFailed(err error) {
	h.emit(secevent.Event{Kind: secevent.KindConnAcceptError, Err: err,
		Detail: "accepting a connection failed; retrying with backoff"})
}

// rejectedBusy reports a connection turned away because one is being served.
func (h *secHolder) rejectedBusy(c net.Conn) {
	if !h.enabled() {
		return
	}
	h.emit(secevent.Event{Kind: secevent.KindConnRejectedBusy, Action: secevent.Rejected,
		Peer:   secevent.PeerOf(c),
		Detail: "a connection arrived while another is served; closed at once"})
}

// dialFailed reports a failed connection attempt; repeated failures escalate.
func (h *secHolder) dialFailed(addr string, attempt int, err error) {
	sev := secevent.Notice
	if attempt+1 >= dialFailuresBeforeWarning {
		sev = secevent.Warning
	}
	h.emit(secevent.Event{Kind: secevent.KindConnDialFailed, Severity: sev, Err: err,
		Detail: fmt.Sprintf("connecting to %s failed (attempt %d)", addr, attempt+1)})
}

// dialFailuresBeforeWarning is how many consecutive failed dials it takes
// before they are reported as a warning rather than a notice.
const dialFailuresBeforeWarning = 5

// AttachSecurity implements [secevent.Attachable].
func (s *tcpServer) AttachSecurity(em *secevent.Emitter) { s.sec.attach(em, "tcp") }

// AttachSecurity implements [secevent.Attachable].
func (s *tlsServer) AttachSecurity(em *secevent.Emitter) { s.sec.attach(em, "tls") }

// AttachSecurity implements [secevent.Attachable].
func (c *tcpClient) AttachSecurity(em *secevent.Emitter) { c.sec.attach(em, "tcp") }

// AttachSecurity implements [secevent.Attachable].
func (c *tlsClient) AttachSecurity(em *secevent.Emitter) { c.sec.attach(em, "tls") }

// AttachSecurity implements [secevent.Attachable].
func (c *udpChannel) AttachSecurity(em *secevent.Emitter) { c.sec.attach(em, "udp") }

// PeerInfo implements [secevent.PeerInfoer]: the peer of a datagram socket is
// whoever it sends to.
func (u *udpConn) PeerInfo() secevent.PeerInfo {
	u.ch.mu.Lock()
	defer u.ch.mu.Unlock()
	p := secevent.PeerInfo{Network: "udp", ConnectedAt: time.Now()}
	if u.ch.remote != nil {
		p.RemoteAddr = unmap(u.ch.remote.AddrPort())
	}
	if u.ch.conn != nil {
		if la, ok := u.ch.conn.LocalAddr().(*net.UDPAddr); ok {
			p.LocalAddr = unmap(la.AddrPort())
		}
	}
	return p
}

// PeerInfo implements [secevent.PeerInfoer].
func (s *serialConn) PeerInfo() secevent.PeerInfo {
	return secevent.PeerInfo{Network: "serial", SerialPort: s.device, ConnectedAt: time.Now()}
}
