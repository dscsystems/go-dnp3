package dnp3_test

import (
	"bytes"
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/channel"
	"github.com/dscsystems/go-dnp3/master"
	"github.com/dscsystems/go-dnp3/outstation"
)

// P1 report: with link-layer confirmation enabled, a response split across
// several application fragments is never fully sent. sendFragments sends every
// body in one synchronous loop, but with UseLinkConfirms every stack.SendTo
// call leaves the stack Busy() until the peer's link-layer ACK arrives — which
// cannot happen before the loop's *next* iteration, since nothing yields back
// to the session's read loop in between. So body[1] is asked for before
// body[0] is link-acknowledged and is refused outright, and the outstation
// tears the connection down rather than deliver a truncated response.
//
// A small MaxTxFragment forces an ordinary integrity poll over a handful of
// points to split into several application fragments, which is what actually
// happens in the field for a large point count against the standard's
// 2048-octet default — this just needs a smaller number of points to trigger
// it in a test.
func TestConfirmedLinkMultiFragmentResponse(t *testing.T) {
	mch, och := channel.Pipe()

	out := outstation.New(outstation.Config{
		LocalAddr: 10, RemoteAddr: 1,
		Database:        outstation.DatabaseConfig{Binary: 40},
		MaxTxFragment:   20, // forces the class-0 response into several fragments
		UseLinkConfirms: true,
		LinkTimeout:     300 * time.Millisecond,
		LinkRetries:     3,
		ConfirmTimeout:  2 * time.Second,
	}, nil, nil)

	out.Update(func(db *outstation.Database) {
		for i := range uint16(40) {
			db.UpdateBinary(i, dnp3.Binary{Value: true, Flags: dnp3.Online})
		}
	})

	m := master.New(master.Config{
		LocalAddr: 1, RemoteAddr: 10,
		ResponseTimeout: 3 * time.Second,
		UseLinkConfirms: true,
		LinkTimeout:     300 * time.Millisecond,
		LinkRetries:     3,
	}, nil)

	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = out.Run(ctx, och) }()
	go func() { defer wg.Done(); _ = m.Run(ctx, mch) }()
	t.Cleanup(func() {
		cancel()
		_ = mch.Close()
		_ = och.Close()
		wg.Wait()
	})

	waitFor(t, 3*time.Second, func() bool { return m.Connected() })

	pollCtx, pollCancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer pollCancel()
	err := m.IntegrityPoll(pollCtx)

	st := out.Stats()
	if st.RequestsReceived == 0 {
		t.Fatalf("the outstation never received the poll; the test setup is broken, not the code under test")
	}
	if err != nil {
		t.Fatalf("integrity poll: %v (outstation stats: %+v) — confirmed links cannot "+
			"send a response split across several application fragments", err, st)
	}
	// Then confirm the poll really was answered across several fragments,
	// since a single-fragment answer would prove nothing about this defect.
	//
	// Counting fragments against responses says that directly. Waiting for it
	// matters: the outstation bumps its counters just *after* writing each
	// fragment, so the master — which completes the moment the final fragment
	// lands — can return from the poll while the outstation goroutine has yet
	// to account for it. Sampling the instant the poll returns reads that gap
	// and sees one fragment fewer than went out.
	waitFor(t, 2*time.Second, func() bool {
		s := out.Stats()
		return s.FragmentsSent > s.ResponsesSent
	})

	if st = out.Stats(); st.FragmentsSent <= st.ResponsesSent {
		t.Fatalf("%d fragment(s) across %d response(s): nothing was split, so the test did "+
			"not actually force a multi-fragment response", st.FragmentsSent, st.ResponsesSent)
	}
}

// recordingChannel records every frame written through the connections it
// hands out.
type recordingChannel struct {
	channel.Channel
	mu     sync.Mutex
	writes [][]byte
}

func (c *recordingChannel) Connect(ctx context.Context) (io.ReadWriteCloser, error) {
	conn, err := c.Channel.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return recordingConn{conn, c}, nil
}

type recordingConn struct {
	io.ReadWriteCloser
	c *recordingChannel
}

func (r recordingConn) Write(p []byte) (int, error) {
	r.c.mu.Lock()
	r.c.writes = append(r.c.writes, bytes.Clone(p))
	r.c.mu.Unlock()
	return r.ReadWriteCloser.Write(p)
}

// The master sent a link-confirmed frame without arming the retransmission
// timer, and while a request was in flight it slept until the response
// deadline regardless of the link. Against a peer that never acknowledges,
// the first retransmission has to come one link timeout after the frame —
// not on the next tick, and not at the response timeout.
func TestConfirmedLinkRetransmitsAtTheLinkTimeout(t *testing.T) {
	const linkTimeout = 200 * time.Millisecond
	mraw, och := channel.Pipe()
	mch := &recordingChannel{Channel: mraw}

	m := master.New(master.Config{
		LocalAddr: 1, RemoteAddr: 10,
		ResponseTimeout: 10 * time.Second,
		UseLinkConfirms: true,
		LinkTimeout:     linkTimeout,
		LinkRetries:     3,
	}, nil)

	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = m.Run(ctx, mch) }()
	go func() {
		// A peer that reads everything and answers nothing.
		defer wg.Done()
		conn, err := och.Connect(ctx)
		if err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, conn)
	}()
	t.Cleanup(func() {
		cancel()
		_ = mch.Close()
		_ = och.Close()
		wg.Wait()
	})

	go func() { _ = m.IntegrityPoll(ctx) }()

	start := time.Now()
	var first, second time.Time
	waitFor(t, 3*time.Second, func() bool {
		mch.mu.Lock()
		defer mch.mu.Unlock()
		if len(mch.writes) >= 1 && first.IsZero() {
			first = time.Now()
		}
		if len(mch.writes) >= 2 {
			second = time.Now()
			return true
		}
		return false
	})
	if gap := second.Sub(first); gap < linkTimeout/2 {
		t.Fatalf("retransmitted %v after the first frame, before the %v link timeout", gap, linkTimeout)
	}
	if elapsed := second.Sub(start); elapsed > 2*time.Second {
		t.Fatalf("first retransmission took %v; the link timeout is %v", elapsed, linkTimeout)
	}
}
