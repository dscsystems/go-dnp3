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
// So this presents a datagram socket as a stream, which works because DNP3
// over UDP puts whole link frames in single datagrams. What it cannot do is
// hide loss: a dropped datagram is a dropped frame, and a fragment spanning
// several frames simply fails to reassemble. Use UDP where the network is
// reliable and the messages are small, and prefer TCP everywhere else.

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

// udpConn presents the datagram socket as a stream.
type udpConn struct{ ch *udpChannel }

func (u *udpConn) Read(p []byte) (int, error) {
	u.ch.mu.Lock()
	conn := u.ch.conn
	u.ch.mu.Unlock()
	if conn == nil {
		return 0, ErrClosed
	}

	n, addr, err := conn.ReadFromUDP(p)
	if err != nil {
		return n, err
	}

	// Learn the peer from the first datagram, so an outstation can answer a
	// master it was not configured with.
	u.ch.mu.Lock()
	if u.ch.remote == nil {
		u.ch.remote = addr
	}
	u.ch.mu.Unlock()
	return n, nil
}

func (u *udpConn) Write(p []byte) (int, error) {
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
