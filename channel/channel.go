// Package channel provides the physical layer beneath a DNP3 session: the
// thing that produces a byte stream and reproduces it after a failure.
//
// Every transport DNP3 runs over — TCP, TLS, UDP, serial, or an in-process
// pipe — reduces to the same contract: hand me a connection, and hand me
// another one when this breaks. Keeping that behind one small interface is
// what lets the session layer be written once.
package channel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"sync"
	"time"
)

// Channel produces connections for a session.
//
// Connect blocks until a connection is available or the context is done. A
// session calls it again after every disconnection, so implementations own
// their own reconnect timing.
type Channel interface {
	Connect(ctx context.Context) (io.ReadWriteCloser, error)
	Close() error
	String() string
}

// ErrClosed means the channel has been shut down and will produce no more
// connections.
var ErrClosed = errors.New("channel: closed")

// Retry describes how long to wait between connection attempts.
//
// The jitter matters more than it looks. A substation that loses a switch
// brings every master's connection down at the same instant; without jitter
// they all retry in lockstep and keep colliding, turning one outage into a
// self-sustaining thundering herd.
type Retry struct {
	Min    time.Duration
	Max    time.Duration
	Factor float64
	// Jitter is the fraction of the delay to randomise, from 0 to 1.
	Jitter float64
}

// DefaultRetry backs off from half a second to a minute.
var DefaultRetry = Retry{
	Min:    500 * time.Millisecond,
	Max:    60 * time.Second,
	Factor: 2,
	Jitter: 0.2,
}

// NoRetry connects once and gives up, which is what tests and one-shot tools
// want.
var NoRetry = Retry{Min: 0, Max: 0, Factor: 1}

// delay returns the wait before attempt n, counting from zero.
func (r Retry) delay(n int) time.Duration {
	if r.Min <= 0 {
		return 0
	}
	d := float64(r.Min)
	for range n {
		d *= r.Factor
		if r.Max > 0 && d >= float64(r.Max) {
			d = float64(r.Max)
			break
		}
	}
	if r.Jitter > 0 {
		// Symmetric jitter: the delay is scaled by 1±Jitter.
		d *= 1 + r.Jitter*(2*rand.Float64()-1)
	}
	return time.Duration(d)
}

// sleep waits for the backoff delay or the context, whichever comes first.
func (r Retry) sleep(ctx context.Context, attempt int) error {
	d := r.delay(attempt)
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ---------- TCP client ----------

type tcpClient struct {
	addr    string
	retry   Retry
	dialer  net.Dialer
	attempt int

	mu     sync.Mutex
	closed bool
}

// TCPClient returns a channel that dials addr, retrying with backoff.
func TCPClient(addr string, retry Retry) Channel {
	return &tcpClient{
		addr:   addr,
		retry:  retry,
		dialer: net.Dialer{Timeout: 10 * time.Second},
	}
}

func (c *tcpClient) Connect(ctx context.Context) (io.ReadWriteCloser, error) {
	for {
		c.mu.Lock()
		closed := c.closed
		c.mu.Unlock()
		if closed {
			return nil, ErrClosed
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		conn, err := c.dialer.DialContext(ctx, "tcp", c.addr)
		if err == nil {
			c.attempt = 0
			return conn, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if c.retry.Min <= 0 {
			return nil, err
		}

		if serr := c.retry.sleep(ctx, c.attempt); serr != nil {
			return nil, serr
		}
		c.attempt++
	}
}

func (c *tcpClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

func (c *tcpClient) String() string { return "tcp-client " + c.addr }

// ---------- TCP server ----------

type tcpServer struct {
	addr string

	mu       sync.Mutex
	listener *net.TCPListener
	closed   bool
}

// TCPServer returns a channel that accepts inbound connections on addr,
// serving one at a time.
//
// An outstation that accepts one master at a time is the common field
// configuration; serving several concurrently needs a session per connection,
// which belongs above this layer.
func TCPServer(addr string) Channel {
	return &tcpServer{addr: addr}
}

// Addr returns the address the listener bound to, which is how a test that
// asked for port 0 finds out what it got.
func (s *tcpServer) Addr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener == nil {
		return nil
	}
	return s.listener.Addr()
}

func (s *tcpServer) listen() (*net.TCPListener, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	if s.listener != nil {
		return s.listener, nil
	}
	l, err := net.Listen("tcp", s.addr)
	if err != nil {
		return nil, err
	}
	tl, ok := l.(*net.TCPListener)
	if !ok {
		_ = l.Close()
		return nil, fmt.Errorf("channel: expected a TCP listener for %q", s.addr)
	}
	s.listener = tl
	return tl, nil
}

func (s *tcpServer) Connect(ctx context.Context) (io.ReadWriteCloser, error) {
	l, err := s.listen()
	if err != nil {
		return nil, err
	}
	conn, err := acceptContext(ctx, l)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

// acceptContext accepts one connection, giving up when the context ends.
//
// The interruption is a deadline rather than a close. Closing the listener
// would work once and then leave the channel permanently unable to accept —
// a session whose connect was cancelled could never come back, which is
// exactly what a reconnect is.
func acceptContext(ctx context.Context, l *net.TCPListener) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = l.SetDeadline(time.Now())
		case <-done:
		}
	}()

	conn, err := l.Accept()
	// Clear the deadline so the next accept is not immediately stale.
	_ = l.SetDeadline(time.Time{})

	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, err
	}

	// A connection that arrived while the caller was giving up. Cancellation
	// and the handshake race each other, and the deadline that interrupts an
	// accept is set by another goroutine, so there is a window in which both
	// happen.
	//
	// Handing it back would give it to a caller that has stopped waiting for
	// it and will not close it, leaving a peer that believes it is connected
	// talking to nothing — which on a listener serving one session at a time
	// is a device that never gets a second chance to be accepted.
	if ctxErr := ctx.Err(); ctxErr != nil {
		_ = conn.Close()
		return nil, ctxErr
	}
	return conn, nil
}

func (s *tcpServer) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.listener != nil {
		return s.listener.Close()
	}
	return nil
}

func (s *tcpServer) String() string { return "tcp-server " + s.addr }

// TCPServerAddr returns the bound address of a channel created by
// [TCPServer], or nil if it has not bound yet.
func TCPServerAddr(c Channel) net.Addr {
	if s, ok := c.(*tcpServer); ok {
		return s.Addr()
	}
	return nil
}

// ---------- In-process pipe ----------

// Pipe returns two channels connected to each other in memory.
//
// This is what every integration test runs over, and what the terminal
// explorer's demo mode uses: a full master and outstation talking to each
// other through the real link, transport and application layers, with no
// socket and no hardware.
func Pipe() (a, b Channel) {
	p := &pipePair{done: make(chan struct{})}
	return &pipeEnd{pair: p, side: 0}, &pipeEnd{pair: p, side: 1}
}

type pipePair struct {
	mu      sync.Mutex
	pending [2]io.ReadWriteCloser
	claimed [2]bool

	once sync.Once
	done chan struct{}
}

// claim hands out one end of a connected pair.
//
// The first side to ask creates the pair and parks the other end for its peer;
// the second side collects it, and the slot resets for the next reconnect. A
// side that asks twice without its peer having collected — which is what a
// reconnect looks like when only one end dropped — gets a fresh pair, and the
// stale one is closed so the peer sees the disconnection rather than talking
// into an abandoned pipe.
func (p *pipePair) claim(side int) (io.ReadWriteCloser, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	select {
	case <-p.done:
		return nil, ErrClosed
	default:
	}

	if p.pending[0] == nil || p.claimed[side] {
		p.discardLocked()
		x, y := net.Pipe()
		p.pending = [2]io.ReadWriteCloser{x, y}
		p.claimed = [2]bool{}
	}

	c := p.pending[side]
	p.claimed[side] = true
	if p.claimed[0] && p.claimed[1] {
		p.pending = [2]io.ReadWriteCloser{}
		p.claimed = [2]bool{}
	}
	return c, nil
}

// discardLocked closes any unclaimed ends. The caller holds the lock.
func (p *pipePair) discardLocked() {
	for i, c := range p.pending {
		if c != nil {
			_ = c.Close()
			p.pending[i] = nil
		}
	}
}

type pipeEnd struct {
	pair *pipePair
	side int
}

func (e *pipeEnd) Connect(ctx context.Context) (io.ReadWriteCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return e.pair.claim(e.side)
}

func (e *pipeEnd) Close() error {
	e.pair.once.Do(func() {
		close(e.pair.done)
		e.pair.mu.Lock()
		e.pair.discardLocked()
		e.pair.mu.Unlock()
	})
	return nil
}

func (e *pipeEnd) String() string { return fmt.Sprintf("pipe-%d", e.side) }
