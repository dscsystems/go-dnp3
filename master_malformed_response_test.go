package dnp3_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/channel"
	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/internal/stack"
	"github.com/dscsystems/go-dnp3/master"
)

// A response the master cannot parse is the outstation's fault, not silence.
// It used to surface as ErrTimeout and bump ResponseTimeout, the counter that
// means the outstation did not answer.
func TestMalformedResponseIsNotATimeout(t *testing.T) {
	mch, och := channel.Pipe()
	m := master.New(master.Config{LocalAddr: 1, RemoteAddr: 10, ResponseTimeout: 3 * time.Second}, master.NopHandler{})

	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = m.Run(ctx, mch) }()

	conn, err := och.Connect(ctx)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	var mu sync.Mutex
	armed := false

	wg.Add(1)
	go func() {
		defer wg.Done()
		st := stack.New(stack.Config{LocalAddr: 10, RemoteAddr: 1, IsMaster: false})
		buf := make([]byte, stack.ReadChunk)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				_ = st.Receive(conn, buf[:n], func(r stack.Received) {
					frag, perr := app.ParseFragment(nil, r.Fragment)
					if perr != nil || frag.Header.Func == app.FuncConfirm {
						return
					}
					mu.Lock()
					fire := armed
					armed = false
					mu.Unlock()

					var body []byte
					if fire {
						// An object header promising more data than follows.
						body = []byte{1, 2, 0x00, 0x00, 0x09}
					}
					_ = st.SendTo(conn, r.Source, response(
						app.Control{Fir: true, Fin: true, Seq: frag.Header.Control.Seq}, body))
				})
			}
			if err != nil {
				return
			}
		}
	}()

	t.Cleanup(func() {
		cancel()
		_ = conn.Close()
		_ = mch.Close()
		_ = och.Close()
		wg.Wait()
	})

	waitFor(t, 3*time.Second, func() bool { return m.Connected() })
	time.Sleep(300 * time.Millisecond) // let the startup sequence finish

	mu.Lock()
	armed = true
	mu.Unlock()
	timeoutsBefore := m.Stats().ResponseTimeout

	pollCtx, pollCancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer pollCancel()
	err = m.IntegrityPoll(pollCtx)

	if !errors.Is(err, dnp3.ErrMalformed) {
		t.Fatalf("integrity poll error = %v, want ErrMalformed", err)
	}
	if errors.Is(err, dnp3.ErrTimeout) {
		t.Error("a malformed response was reported as a timeout")
	}
	if got := m.Stats().ResponseTimeout; got != timeoutsBefore {
		t.Errorf("ResponseTimeout went %d -> %d for a response that did arrive", timeoutsBefore, got)
	}
}
