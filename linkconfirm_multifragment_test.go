package dnp3_test

import (
	"context"
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
	if st.ResponsesSent < 2 {
		t.Fatalf("only %d response(s) sent; the test did not actually force a multi-fragment "+
			"response, so it proves nothing about this defect", st.ResponsesSent)
	}
}
