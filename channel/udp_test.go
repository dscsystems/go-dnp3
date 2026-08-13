package channel

import (
	"io"
	"net"
	"testing"
	"time"
)

func TestUDPRoundTrip(t *testing.T) {
	// The outstation end binds a known port and learns its peer from the
	// first datagram, which is how a real one answers whichever master polls.
	server := UDPChannel(UDPConfig{LocalAddr: "127.0.0.1:0"})
	t.Cleanup(func() { _ = server.Close() })

	sc, err := server.Connect(t.Context())
	if err != nil {
		t.Fatalf("bind: %v", err)
	}

	addr := ServerAddr(server)
	if addr == nil {
		t.Fatal("the UDP channel did not report its address")
	}

	client := UDPChannel(UDPConfig{LocalAddr: "127.0.0.1:0", RemoteAddr: addr.String()})
	t.Cleanup(func() { _ = client.Close() })

	cc, err := client.Connect(t.Context())
	if err != nil {
		t.Fatalf("client bind: %v", err)
	}

	frame := []byte{0x05, 0x64, 0x05, 0xC0, 0x0A, 0x00, 0x01, 0x00, 0xB1, 0xAC}
	if _, err := cc.Write(frame); err != nil {
		t.Fatalf("write: %v", err)
	}

	buf := make([]byte, 64)
	setDeadline(t, sc, 3*time.Second)
	n, err := sc.Read(buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf[:n]) != string(frame) {
		t.Errorf("got % x, want % x", buf[:n], frame)
	}

	// Having heard from the client, the server can now answer it without ever
	// having been configured with its address.
	if _, err := sc.Write([]byte{0x05, 0x64}); err != nil {
		t.Fatalf("reply: %v", err)
	}
	setDeadline(t, cc, 3*time.Second)
	if _, err := cc.Read(buf); err != nil {
		t.Fatalf("client read: %v", err)
	}
}

// TestUDPWriteBeforePeerKnown covers an outstation that has never been polled:
// it has nowhere to send, and must say so rather than panic.
func TestUDPWriteBeforePeerKnown(t *testing.T) {
	c := UDPChannel(UDPConfig{LocalAddr: "127.0.0.1:0"})
	t.Cleanup(func() { _ = c.Close() })

	conn, err := c.Connect(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte{0x05, 0x64}); err == nil {
		t.Error("writing with no peer known should fail")
	}
}

// TestUDPReconnectReturnsSameSocket covers the fact that a datagram socket has
// no connection to re-establish: a session that reconnects must find its
// binding intact rather than losing the port.
func TestUDPReconnectReturnsSameSocket(t *testing.T) {
	c := UDPChannel(UDPConfig{LocalAddr: "127.0.0.1:0"})
	t.Cleanup(func() { _ = c.Close() })

	if _, err := c.Connect(t.Context()); err != nil {
		t.Fatal(err)
	}
	first := ServerAddr(c).String()

	if _, err := c.Connect(t.Context()); err != nil {
		t.Fatal(err)
	}
	if second := ServerAddr(c).String(); second != first {
		t.Errorf("reconnecting rebound the socket: %s → %s", first, second)
	}
}

func TestUDPClosed(t *testing.T) {
	c := UDPChannel(UDPConfig{LocalAddr: "127.0.0.1:0"})
	_ = c.Close()
	if _, err := c.Connect(t.Context()); err == nil {
		t.Error("a closed channel should refuse to connect")
	}
}

func TestUDPBadAddress(t *testing.T) {
	c := UDPChannel(UDPConfig{LocalAddr: "not-an-address"})
	t.Cleanup(func() { _ = c.Close() })
	if _, err := c.Connect(t.Context()); err == nil {
		t.Error("an unparsable address should fail")
	}
}

func setDeadline(t *testing.T, rw io.ReadWriteCloser, d time.Duration) {
	t.Helper()
	u, ok := rw.(*udpConn)
	if !ok {
		return
	}
	u.ch.mu.Lock()
	conn := u.ch.conn
	u.ch.mu.Unlock()
	if conn != nil {
		_ = conn.SetReadDeadline(time.Now().Add(d))
	}
}

var _ net.Addr = (*net.UDPAddr)(nil)
