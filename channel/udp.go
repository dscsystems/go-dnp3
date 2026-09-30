package channel

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
)

// UDP is a legal DNP3 transport, and a genuinely awkward one.
//
// The stack above expects a stream: the link layer resynchronises on a
// delimiter, and the transport function reassembles fragments across frames.
// UDP delivers datagrams that may be dropped, duplicated or reordered, and
// none of those layers were designed to repair that — the link layer's frame
// count bit assumes an ordered channel.
//
// So this presents a datagram socket as a stream, keeping each datagram whole
// in both directions: one read out of a datagram may span several link frames,
// and the frames of one application fragment leave as a single datagram. What
// it cannot do is hide loss: a dropped datagram is a dropped fragment. A
// message has to fit in one datagram, so use UDP where the network is reliable
// and the messages are small, and prefer TCP everywhere else.

// UDPConfig describes a UDP endpoint.
type UDPConfig struct {
	// LocalAddr is the address to bind. Empty binds an ephemeral port on all
	// interfaces, which is what a master normally wants.
	LocalAddr string
	// RemoteAddr is where to send. Empty means reply to whoever writes first,
	// which is what an outstation normally wants.
	RemoteAddr string
}

// UDPChannel returns a channel over a UDP socket.
func UDPChannel(cfg UDPConfig) Channel {
	return &udpChannel{cfg: cfg}
}

type udpChannel struct {
	cfg UDPConfig

	mu     sync.Mutex
	conn   *net.UDPConn
	remote *net.UDPAddr
	closed bool
}

func (c *udpChannel) Addr() net.Addr {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil
	}
	return c.conn.LocalAddr()
}

func (c *udpChannel) Connect(ctx context.Context) (io.ReadWriteCloser, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return nil, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.conn != nil {
		// A datagram socket has no connection to re-establish, so a
		// reconnecting session gets the same socket back.
		return &udpConn{ch: c}, nil
	}

	local := c.cfg.LocalAddr
	if local == "" {
		local = ":0"
	}
	laddr, err := net.ResolveUDPAddr("udp", local)
	if err != nil {
		return nil, fmt.Errorf("channel: resolving %q: %w", local, err)
	}

	conn, err := net.ListenUDP("udp", laddr)
	if err != nil {
		return nil, err
	}
	c.conn = conn

	if c.cfg.RemoteAddr != "" {
		raddr, err := net.ResolveUDPAddr("udp", c.cfg.RemoteAddr)
		if err != nil {
			_ = conn.Close()
			c.conn = nil
			return nil, fmt.Errorf("channel: resolving %q: %w", c.cfg.RemoteAddr, err)
		}
		c.remote = raddr
	}
	return &udpConn{ch: c}, nil
}

func (c *udpChannel) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	if c.conn != nil {
		err := c.conn.Close()
		c.conn = nil
		return err
	}
	return nil
}

func (c *udpChannel) String() string {
	if c.cfg.RemoteAddr != "" {
		return "udp " + c.cfg.LocalAddr + "→" + c.cfg.RemoteAddr
	}
	return "udp " + c.cfg.LocalAddr
}

// maxDatagram is the most a UDP datagram can carry.
const maxDatagram = 65507

// udpConn presents the datagram socket as a stream.
//
// A datagram is a message, so the two directions keep it whole. On the way in
// a datagram is read entire and handed out in as many Reads as the caller's
// buffer needs: reading straight into a caller's buffer sized for one link
// frame would throw away the rest of a datagram carrying several. On the way
// out, the frames of one message are gathered and sent as one datagram — see
// BeginMessage.
type udpConn struct {
	ch *udpChannel

	// rbuf holds the datagram being read out, and pending what is left of it.
	rbuf    []byte
	pending []byte

	// wbuf gathers the frames of a message between BeginMessage and
	// EndMessage.
	wmu      sync.Mutex
	wbuf     []byte
	batching bool
}

func (u *udpConn) Read(p []byte) (int, error) {
	for len(u.pending) == 0 {
		u.ch.mu.Lock()
		conn := u.ch.conn
		u.ch.mu.Unlock()
		if conn == nil {
			return 0, ErrClosed
		}
		if u.rbuf == nil {
			u.rbuf = make([]byte, maxDatagram)
		}

		n, addr, err := conn.ReadFromUDP(u.rbuf)
		if err != nil {
			return 0, err
		}

		// Learn the peer from the first datagram, so an outstation can answer
		// a master it was not configured with.
		u.ch.mu.Lock()
		if u.ch.remote == nil {
			u.ch.remote = addr
		}
		u.ch.mu.Unlock()

		u.pending = u.rbuf[:n] // an empty datagram loops round for the next
	}

	n := copy(p, u.pending)
	u.pending = u.pending[n:]
	return n, nil
}

// BeginMessage starts gathering writes into one datagram. A message is one
// application fragment, which over UDP has to arrive in a single datagram
// however many link frames it takes; the stack brackets the frames of a
// fragment with BeginMessage and EndMessage. Writes outside a message go out
// as datagrams of their own, which is right for a link-layer acknowledgement.
func (u *udpConn) BeginMessage() {
	u.wmu.Lock()
	defer u.wmu.Unlock()
	u.batching = true
	u.wbuf = u.wbuf[:0]
}

// EndMessage sends what BeginMessage gathered as one datagram.
func (u *udpConn) EndMessage() error {
	u.wmu.Lock()
	defer u.wmu.Unlock()
	u.batching = false
	if len(u.wbuf) == 0 {
		return nil
	}
	_, err := u.send(u.wbuf)
	u.wbuf = u.wbuf[:0]
	return err
}

func (u *udpConn) Write(p []byte) (int, error) {
	u.wmu.Lock()
	defer u.wmu.Unlock()

	if u.batching {
		if len(u.wbuf)+len(p) > maxDatagram {
			return 0, fmt.Errorf("channel: a UDP message exceeds one datagram (%d octets)", maxDatagram)
		}
		u.wbuf = append(u.wbuf, p...)
		return len(p), nil
	}
	return u.send(p)
}

// send transmits p as one datagram to the peer.
func (u *udpConn) send(p []byte) (int, error) {
	u.ch.mu.Lock()
	conn, remote := u.ch.conn, u.ch.remote
	u.ch.mu.Unlock()

	if conn == nil {
		return 0, ErrClosed
	}
	if remote == nil {
		// Nothing has been heard from yet, so there is nowhere to send. This
		// is normal for an outstation that has not been polled.
		return 0, fmt.Errorf("channel: no UDP peer known yet")
	}
	return conn.WriteToUDP(p, remote)
}

// Close does not close the socket: the channel owns it, and a session
// reconnecting must find it still there.
func (u *udpConn) Close() error { return nil }
