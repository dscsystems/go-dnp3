package channel

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

func TestPipeConnectsBothEnds(t *testing.T) {
	a, b := Pipe()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })

	ctx := t.Context()
	ca, err := a.Connect(ctx)
	if err != nil {
		t.Fatalf("side a: %v", err)
	}
	cb, err := b.Connect(ctx)
	if err != nil {
		t.Fatalf("side b: %v", err)
	}

	// net.Pipe is synchronous, so the write needs a reader waiting.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := ca.Write([]byte("hello")); err != nil {
			t.Errorf("write: %v", err)
		}
	}()

	buf := make([]byte, 5)
	if _, err := io.ReadFull(cb, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	wg.Wait()

	if string(buf) != "hello" {
		t.Errorf("read %q, want hello", buf)
	}
}

// TestPipeReconnect covers a session dropping and coming back, which is what
// every reconnect test in this repository depends on working.
func TestPipeReconnect(t *testing.T) {
	a, b := Pipe()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })

	ctx := t.Context()
	ca1, _ := a.Connect(ctx)
	cb1, _ := b.Connect(ctx)
	_ = ca1.Close()
	_ = cb1.Close()

	ca2, err := a.Connect(ctx)
	if err != nil {
		t.Fatalf("reconnect a: %v", err)
	}
	cb2, err := b.Connect(ctx)
	if err != nil {
		t.Fatalf("reconnect b: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = ca2.Write([]byte("x"))
	}()
	buf := make([]byte, 1)
	if _, err := io.ReadFull(cb2, buf); err != nil {
		t.Fatalf("the reconnected pair is not connected: %v", err)
	}
	wg.Wait()
}

// TestPipeOneSidedReconnect covers the asymmetric case: one end drops and
// redials before its peer noticed. It must get a fresh pair rather than the
// stale end it already holds, or it would talk into an abandoned pipe.
func TestPipeOneSidedReconnect(t *testing.T) {
	a, b := Pipe()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })

	ctx := t.Context()
	first, err := a.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Error("redialling the same side handed back the same connection")
	}

	// The peer's end must pair with the newer one.
	peer, err := b.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = second.Write([]byte("y"))
	}()
	buf := make([]byte, 1)
	if _, err := io.ReadFull(peer, buf); err != nil {
		t.Fatalf("the peer did not pair with the newest connection: %v", err)
	}
	wg.Wait()
}

func TestPipeCloseStopsConnecting(t *testing.T) {
	a, b := Pipe()
	_ = a.Close()

	if _, err := a.Connect(t.Context()); !errors.Is(err, ErrClosed) {
		t.Errorf("err = %v, want ErrClosed", err)
	}
	// Closing either end closes the pair.
	if _, err := b.Connect(t.Context()); !errors.Is(err, ErrClosed) {
		t.Errorf("peer err = %v, want ErrClosed", err)
	}
}

func TestPipeRespectsContext(t *testing.T) {
	a, b := Pipe()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := a.Connect(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestPipeString(t *testing.T) {
	a, b := Pipe()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	if a.String() == b.String() {
		t.Error("the two ends should be distinguishable in a log")
	}
}

// ---------- Retry ----------

func TestRetryBacksOff(t *testing.T) {
	r := Retry{Min: 100 * time.Millisecond, Max: time.Second, Factor: 2}

	// Without jitter the sequence is exact and must saturate at Max.
	want := []time.Duration{
		100 * time.Millisecond,
		200 * time.Millisecond,
		400 * time.Millisecond,
		800 * time.Millisecond,
		time.Second,
		time.Second,
	}
	for i, w := range want {
		if got := r.delay(i); got != w {
			t.Errorf("delay(%d) = %v, want %v", i, got, w)
		}
	}
}

// TestRetryJitterSpreadsRetries covers the reason jitter exists. A substation
// losing a switch drops every master's connection at the same instant; without
// jitter they retry in lockstep and keep colliding, turning one outage into a
// self-sustaining herd.
func TestRetryJitterSpreadsRetries(t *testing.T) {
	r := Retry{Min: time.Second, Max: time.Minute, Factor: 2, Jitter: 0.5}

	seen := map[time.Duration]bool{}
	for range 50 {
		d := r.delay(0)
		seen[d] = true
		if d < 500*time.Millisecond || d > 1500*time.Millisecond {
			t.Fatalf("jittered delay %v outside the ±50%% band around 1s", d)
		}
	}
	if len(seen) < 10 {
		t.Errorf("only %d distinct delays across 50 draws; jitter is not spreading retries", len(seen))
	}
}

func TestNoRetryReturnsZero(t *testing.T) {
	if got := NoRetry.delay(3); got != 0 {
		t.Errorf("NoRetry delay = %v, want 0", got)
	}
}

func TestRetrySleepRespectsContext(t *testing.T) {
	r := Retry{Min: time.Hour, Max: time.Hour, Factor: 1}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()

	start := time.Now()
	if err := r.sleep(ctx, 0); err == nil {
		t.Error("sleep should report the cancelled context")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("sleep ignored the context for %v", elapsed)
	}
}

// ---------- TCP ----------

func TestTCPServerAndClient(t *testing.T) {
	server := TCPServer("127.0.0.1:0")
	t.Cleanup(func() { _ = server.Close() })

	accepted := make(chan io.ReadWriteCloser, 1)
	go func() {
		c, err := server.Connect(t.Context())
		if err == nil {
			accepted <- c
		}
	}()

	// Wait for the listener to bind so the port is known.
	var addr string
	for range 100 {
		if a := TCPServerAddr(server); a != nil {
			addr = a.String()
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if addr == "" {
		t.Fatal("the server never bound")
	}

	client := TCPClient(addr, NoRetry)
	t.Cleanup(func() { _ = client.Close() })

	cc, err := client.Connect(t.Context())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer cc.Close()

	sc := <-accepted
	defer sc.Close()

	if _, err := cc.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(sc, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "ping" {
		t.Errorf("read %q, want ping", buf)
	}
}

func TestTCPClientFailsWithoutRetry(t *testing.T) {
	// Port 1 on loopback is reliably refused and needs no listener.
	c := TCPClient("127.0.0.1:1", NoRetry)
	t.Cleanup(func() { _ = c.Close() })

	if _, err := c.Connect(t.Context()); err == nil {
		t.Error("connecting to a closed port should fail when retries are off")
	}
}

func TestTCPClientCloseStopsConnecting(t *testing.T) {
	c := TCPClient("127.0.0.1:1", DefaultRetry)
	_ = c.Close()

	if _, err := c.Connect(t.Context()); !errors.Is(err, ErrClosed) {
		t.Errorf("err = %v, want ErrClosed", err)
	}
}

func TestTCPClientRetryRespectsContext(t *testing.T) {
	c := TCPClient("127.0.0.1:1", Retry{Min: time.Hour, Max: time.Hour, Factor: 1})
	t.Cleanup(func() { _ = c.Close() })

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := c.Connect(ctx); err == nil {
		t.Error("expected a failure")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("retry ignored the context for %v", elapsed)
	}
}

func TestChannelStrings(t *testing.T) {
	for _, c := range []Channel{TCPClient("host:20000", NoRetry), TCPServer(":20000")} {
		if c.String() == "" {
			t.Error("a channel needs a name for logs")
		}
		_ = c.Close()
	}
	if TCPServerAddr(TCPClient("x:1", NoRetry)) != nil {
		t.Error("TCPServerAddr should return nil for a non-server channel")
	}
}
