package dnp3_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/channel"
	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/internal/stack"
	"github.com/dscsystems/go-dnp3/master"
)

// binaryCounter records how many binary values the master delivers.
type binaryCounter struct {
	master.NopHandler
	mu     sync.Mutex
	values int
}

func (b *binaryCounter) HandleBinary(_ master.HeaderInfo, values []dnp3.Indexed[dnp3.Binary]) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.values += len(values)
}

func (b *binaryCounter) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.values
}

// binaryBody is one group 1 variation 2 point at index 0, online and set.
func binaryBody() []byte {
	return app.AppendObjectHeader(nil, app.ObjectHeader{
		Group: 1, Variation: 2,
		Qualifier: app.MakeQualifier(app.PrefixNone, app.RangeStartStop8),
		Range:     app.Range{Spec: app.RangeStartStop8, Start: 0, Stop: 0, Count: 1},
		Data:      []byte{0x81}, // online, state set
	})
}

func response(ctrl app.Control, body []byte) []byte {
	frag := app.AppendHeader(nil, app.Header{
		Control: ctrl,
		Func:    app.FuncResponse,
	})
	return append(frag, body...)
}

// Report: application fragment state is not validated. The master's
// onSolicited accepts a response on a sequence-number match alone — it never
// reads FIR, and keeps no state about a series in progress — so it cannot
// tell a fresh fragment from a repeat of one it has already delivered.
//
// This outstation gives every fragment of a solicited multi-fragment
// response the request's own sequence number (see sendFragments' pendingSeq),
// so against it the sequence number cannot distinguish one fragment of a
// series from another either. A retransmitted fragment — the outstation's
// confirm was lost, so it repeated it — is therefore delivered to the
// application a second time, and the same measurement is counted twice.
func TestMasterDoesNotRedeliverARepeatedFragment(t *testing.T) {
	mch, och := channel.Pipe()

	h := &binaryCounter{}
	m := master.New(master.Config{
		LocalAddr: 1, RemoteAddr: 10,
		ResponseTimeout: 3 * time.Second,
	}, h)

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
	arm := func() { mu.Lock(); armed = true; mu.Unlock() }

	// A raw outstation: it answers every request with an empty response so
	// the master's startup sequence completes, until the test arms it. The
	// armed answer is a two-fragment series whose first fragment is sent
	// twice — a retransmission, exactly as it would look if the outstation
	// never saw the confirm for it.
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
					seq := frag.Header.Control.Seq

					mu.Lock()
					fire := armed
					armed = false
					mu.Unlock()

					if !fire {
						_ = st.SendTo(conn, r.Source,
							response(app.Control{Fir: true, Fin: true, Seq: seq}, nil))
						return
					}

					first := app.Control{Fir: true, Fin: false, Seq: seq}
					_ = st.SendTo(conn, r.Source, response(first, binaryBody()))
					// The very same fragment again.
					_ = st.SendTo(conn, r.Source, response(first, binaryBody()))
					_ = st.SendTo(conn, r.Source,
						response(app.Control{Fir: false, Fin: true, Seq: seq}, nil))
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

	arm()
	pollCtx, pollCancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer pollCancel()
	if err := m.IntegrityPoll(pollCtx); err != nil {
		t.Fatalf("integrity poll: %v", err)
	}

	if got := h.count(); got != 1 {
		t.Errorf("the master delivered %d binary values, want 1: the repeated fragment was "+
			"delivered again rather than recognised as one already received, so a single "+
			"measurement reaches the application twice", got)
	}
}

// A response whose first fragment has FIR clear is a continuation of a
// series that was never started. The master has nothing to attach it to, so
// it must not be delivered as though it began a response.
func TestMasterIgnoresAResponseThatDoesNotStartASeries(t *testing.T) {
	mch, och := channel.Pipe()

	h := &binaryCounter{}
	m := master.New(master.Config{
		LocalAddr: 1, RemoteAddr: 10,
		ResponseTimeout: time.Second,
	}, h)

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
					seq := frag.Header.Control.Seq

					mu.Lock()
					fire := armed
					armed = false
					mu.Unlock()

					ctrl := app.Control{Fir: true, Fin: true, Seq: seq}
					body := []byte(nil)
					if fire {
						// FIR clear, FIN set: the tail of a series that never
						// began, carrying measurements.
						ctrl = app.Control{Fir: false, Fin: true, Seq: seq}
						body = binaryBody()
					}
					_ = st.SendTo(conn, r.Source, response(ctrl, body))
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
	time.Sleep(300 * time.Millisecond)

	mu.Lock()
	armed = true
	mu.Unlock()

	pollCtx, pollCancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer pollCancel()
	_ = m.IntegrityPoll(pollCtx)

	if got := h.count(); got != 0 {
		t.Errorf("the master delivered %d binary values from a response fragment with FIR "+
			"clear; there was no series in progress for it to continue", got)
	}
}
