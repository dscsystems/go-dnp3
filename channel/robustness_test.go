package channel

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// serveOne runs server.Connect in the background and hands back what it
// accepted.
func serveOne(t *testing.T, server Channel) <-chan io.ReadWriteCloser {
	t.Helper()
	accepted := make(chan io.ReadWriteCloser, 1)
	go func() {
		c, err := server.Connect(t.Context())
		if err != nil {
			accepted <- nil
			return
		}
		accepted <- c
	}()
	return accepted
}

func waitAddr(t *testing.T, server Channel) string {
	t.Helper()
	for range 400 {
		if a := ServerAddr(server); a != nil {
			return a.String()
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the server never bound")
	return ""
}

// readClosed reports whether the peer closes c within d.
func readClosed(c net.Conn, d time.Duration) bool {
	_ = c.SetReadDeadline(time.Now().Add(d))
	_, err := c.Read(make([]byte, 1))
	return err != nil && !errors.Is(err, context.DeadlineExceeded) && !isTimeout(err)
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// With BusyReject, a connection that arrives while another is served is
// closed at once instead of waiting in the backlog, and once the served
// connection ends the next one is accepted normally.
func TestTCPServerBusyRejectClosesTheSecondConnection(t *testing.T) {
	server := TCPServerWithOptions("127.0.0.1:0", ServerOptions{Busy: BusyReject})
	t.Cleanup(func() { _ = server.Close() })
	accepted := serveOne(t, server)
	addr := waitAddr(t, server)

	first, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	served := <-accepted
	if served == nil {
		t.Fatal("the first connection was not accepted")
	}

	second, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if !readClosed(second, 2*time.Second) {
		t.Fatal("the second connection was left waiting instead of being closed")
	}
	if readClosed(first, 100*time.Millisecond) {
		t.Fatal("rejecting the second connection closed the first")
	}

	// The slot frees up when the served connection closes.
	_ = served.Close()
	accepted = serveOne(t, server)
	third, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer third.Close()
	select {
	case c := <-accepted:
		if c == nil {
			t.Fatal("the third connection was not accepted")
		}
		_ = c.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("the third connection was never accepted")
	}
}

// The default leaves a second connection open in the backlog, as before.
func TestTCPServerDefaultLeavesTheSecondConnectionWaiting(t *testing.T) {
	server := TCPServer("127.0.0.1:0")
	t.Cleanup(func() { _ = server.Close() })
	accepted := serveOne(t, server)
	addr := waitAddr(t, server)

	first, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if c := <-accepted; c != nil {
		defer c.Close()
	}
	second, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if readClosed(second, 200*time.Millisecond) {
		t.Fatal("the default policy closed the second connection")
	}
}

// A peer that connects to a TLS server and never handshakes is dropped after
// HandshakeTimeout, and the server goes on to accept the next peer.
func TestTLSServerHandshakeTimeoutIsConfigurable(t *testing.T) {
	_, ca, sCert, sKey, cCert, cKey := certs(t)
	server, err := TLSServer("127.0.0.1:0", TLSConfig{
		CertFile: sCert, KeyFile: sKey, CAFile: ca,
		HandshakeTimeout: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	accepted := serveOne(t, server)
	addr := waitAddr(t, server)

	silent, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()
	start := time.Now()
	if !readClosed(silent, 3*time.Second) {
		t.Fatal("a peer that never handshook was not dropped")
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("dropped after %v, want about the 200ms handshake timeout", took)
	}

	client, err := TLSClient(addr, TLSConfig{
		CertFile: cCert, KeyFile: cKey, CAFile: ca, ServerName: "localhost",
	}, NoRetry)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	cc, err := client.Connect(t.Context())
	if err != nil {
		t.Fatalf("TLS dial after the silent peer: %v", err)
	}
	defer cc.Close()
	select {
	case c := <-accepted:
		if c == nil {
			t.Fatal("the real client was not accepted")
		}
		_ = c.Close()
	case <-time.After(3 * time.Second):
		t.Fatal("the real client was never accepted")
	}
}

// PinPeer drops datagrams from anyone but the configured peer; without it they
// are delivered, as before.
func TestUDPPinPeerDropsForeignDatagrams(t *testing.T) {
	for _, pin := range []bool{false, true} {
		t.Run(map[bool]string{false: "unpinned", true: "pinned"}[pin], func(t *testing.T) {
			peer, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
			if err != nil {
				t.Fatal(err)
			}
			defer peer.Close()
			stranger, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
			if err != nil {
				t.Fatal(err)
			}
			defer stranger.Close()

			ch := UDPChannel(UDPConfig{LocalAddr: "127.0.0.1:0", RemoteAddr: peer.LocalAddr().String(), PinPeer: pin})
			t.Cleanup(func() { _ = ch.Close() })
			conn, err := ch.Connect(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			local := ServerAddr(ch).(*net.UDPAddr)

			if _, err := stranger.WriteToUDP([]byte("stranger"), local); err != nil {
				t.Fatal(err)
			}
			time.Sleep(50 * time.Millisecond) // let the stranger's datagram land first
			if _, err := peer.WriteToUDP([]byte("peer"), local); err != nil {
				t.Fatal(err)
			}

			setDeadline(t, conn, 2*time.Second)
			buf := make([]byte, 64)
			n, err := conn.Read(buf)
			if err != nil {
				t.Fatal(err)
			}
			want := "stranger"
			if pin {
				want = "peer"
			}
			if got := string(buf[:n]); got != want {
				t.Fatalf("first datagram read = %q, want %q", got, want)
			}
			if got := ch.(*udpChannel).foreign.Load(); got != 1 {
				t.Fatalf("foreign datagrams counted = %d, want 1", got)
			}
		})
	}
}
