package outstation

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/dscsystems/go-dnp3/internal/link"
)

// oneConnChannel hands out one end of a net.Pipe and then blocks, so a test
// can watch one connection's whole life from the other end.
type oneConnChannel struct {
	conn   net.Conn
	once   sync.Once
	closed chan struct{}
}

func newOneConnChannel() (*oneConnChannel, net.Conn) {
	a, b := net.Pipe()
	return &oneConnChannel{conn: a, closed: make(chan struct{})}, b
}

func (c *oneConnChannel) Connect(ctx context.Context) (io.ReadWriteCloser, error) {
	var conn io.ReadWriteCloser
	c.once.Do(func() { conn = &notifyConn{Conn: c.conn, closed: c.closed} })
	if conn != nil {
		return conn, nil
	}
	<-ctx.Done()
	return nil, ctx.Err()
}
func (c *oneConnChannel) Close() error   { return nil }
func (c *oneConnChannel) String() string { return "one-conn" }

type notifyConn struct {
	net.Conn
	once   sync.Once
	closed chan struct{}
}

func (c *notifyConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

func runOutstation(t *testing.T, cfg Config) (*oneConnChannel, net.Conn) {
	t.Helper()
	ch, peer := newOneConnChannel()
	t.Cleanup(func() { _ = peer.Close() })
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	t.Cleanup(func() { cancel(); <-done })
	s := New(cfg, nil, nil)
	go func() { defer close(done); _ = s.Run(ctx, ch) }()
	return ch, peer
}

func waitClosed(t *testing.T, ch *oneConnChannel, within time.Duration) time.Duration {
	t.Helper()
	start := time.Now()
	select {
	case <-ch.closed:
		return time.Since(start)
	case <-time.After(within):
		t.Fatalf("the outstation did not drop the connection within %v", within)
		return 0
	}
}

// readFrame reads one link frame from the peer end.
func readFrame(t *testing.T, peer net.Conn, within time.Duration) link.Frame {
	t.Helper()
	_ = peer.SetReadDeadline(time.Now().Add(within))
	p := link.NewParser()
	buf := make([]byte, link.MaxFrameSize)
	for {
		if f, err := p.Next(); err == nil {
			return f
		}
		n, err := peer.Read(buf)
		if err != nil {
			t.Fatalf("reading a frame: %v", err)
		}
		_, _ = p.Write(buf[:n])
	}
}

// A connection that says nothing is probed with a link status request after
// IdleTimeout and dropped when the probe goes unanswered.
func TestOutstationIdleTimeoutDropsASilentPeer(t *testing.T) {
	ch, peer := runOutstation(t, Config{
		LocalAddr: 10, RemoteAddr: 1,
		IdleTimeout: 150 * time.Millisecond, LinkTimeout: 100 * time.Millisecond,
	})

	f := readFrame(t, peer, 2*time.Second)
	if !f.Header.Control.Prm || f.Header.Control.Func != link.FuncRequestLinkStatus {
		t.Fatalf("probe = %v, want a primary REQUEST_LINK_STATUS", f.Header.Control)
	}
	go func() { _, _ = io.Copy(io.Discard, peer) }()
	waitClosed(t, ch, 2*time.Second)
}

// A master that answers the probe keeps its connection.
func TestOutstationIdleTimeoutKeepsAPeerThatAnswers(t *testing.T) {
	ch, peer := runOutstation(t, Config{
		LocalAddr: 10, RemoteAddr: 1,
		IdleTimeout: 150 * time.Millisecond, LinkTimeout: 100 * time.Millisecond,
	})
	stop := time.After(800 * time.Millisecond)
	for probes := 0; ; probes++ {
		select {
		case <-stop:
			if probes == 0 {
				t.Fatal("no probe was sent")
			}
			select {
			case <-ch.closed:
				t.Fatal("a master that answered every probe was dropped")
			default:
			}
			return
		default:
		}
		f := readFrame(t, peer, 2*time.Second)
		reply, err := link.Encode(nil, link.Header{
			Control: link.Control{Dir: true, Func: link.FuncLinkStatus},
			Dest:    f.Header.Src, Src: f.Header.Dest, Length: link.MinLength,
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := peer.Write(reply); err != nil {
			t.Fatalf("answering the probe: %v", err)
		}
	}
}

// A link frame that starts and never finishes is dropped after
// FrameAssemblyTimeout.
func TestOutstationFrameAssemblyTimeoutDropsATrickle(t *testing.T) {
	ch, peer := runOutstation(t, Config{
		LocalAddr: 10, RemoteAddr: 1,
		FrameAssemblyTimeout: 150 * time.Millisecond,
	})
	go func() { _, _ = io.Copy(io.Discard, peer) }()
	if _, err := peer.Write([]byte{link.StartByte0, link.StartByte1, 0x05}); err != nil {
		t.Fatal(err)
	}
	waitClosed(t, ch, 2*time.Second)
}

// A peer that never reads makes the outstation's first write miss its
// deadline, and the outstation drops it rather than waiting for good.
func TestOutstationWriteTimeoutDropsAPeerThatDoesNotRead(t *testing.T) {
	ch, _ := runOutstation(t, Config{
		LocalAddr: 10, RemoteAddr: 1,
		WriteTimeout: 100 * time.Millisecond,
		// The startup null unsolicited response is the first write.
		Unsolicited: UnsolicitedConfig{Enabled: true},
	})
	waitClosed(t, ch, 2*time.Second)
}

// None of the timeouts are on by default: a silent peer keeps its connection.
func TestOutstationTimeoutsAreOffByDefault(t *testing.T) {
	ch, peer := runOutstation(t, Config{LocalAddr: 10, RemoteAddr: 1})
	go func() { _, _ = io.Copy(io.Discard, peer) }()
	_, _ = peer.Write([]byte{link.StartByte0, link.StartByte1})
	select {
	case <-ch.closed:
		t.Fatal("the connection was dropped with every timeout off")
	case <-time.After(500 * time.Millisecond):
	}
}
