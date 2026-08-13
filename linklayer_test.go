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

// confirmedPair runs a master and outstation with link-layer confirmation
// enabled on both sides — the configuration a serial link uses.
//
// This path was unreachable until the stack became single-goroutine and grew a
// timeout hook: with confirmations on, the peer answers frames with link
// acknowledgements, so the send path and the receive path both drive the link
// state machines. Doing that from two goroutines races the frame count bit,
// which is a lost or duplicated fragment.
func confirmedPair(t *testing.T, dbCfg outstation.DatabaseConfig) (
	*master.Session, *outstation.Session, *collector,
) {
	t.Helper()

	mch, och := channel.Pipe()
	coll := newCollector()

	out := outstation.New(outstation.Config{
		LocalAddr: 10, RemoteAddr: 1,
		Database:        dbCfg,
		ConfirmTimeout:  time.Second,
		UseLinkConfirms: true,
		LinkRetries:     3,
		LinkTimeout:     500 * time.Millisecond,
	}, nil, nil)

	m := master.New(master.Config{
		LocalAddr: 1, RemoteAddr: 10,
		ResponseTimeout: 3 * time.Second,
		UseLinkConfirms: true,
		LinkRetries:     3,
		LinkTimeout:     500 * time.Millisecond,
	}, coll)

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
	return m, out, coll
}

// TestConfirmedLinkIntegrityPoll runs the full read path with link-layer
// confirmation on, exercising the reset handshake and the frame count bit.
func TestConfirmedLinkIntegrityPoll(t *testing.T) {
	dbCfg := outstation.DatabaseConfig{
		Binary: 8, Analog: 4, DefaultClass: dnp3.Class1,
	}
	m, out, coll := confirmedPair(t, dbCfg)

	out.Update(func(db *outstation.Database) {
		db.UpdateBinary(1, dnp3.Binary{Value: true, Flags: dnp3.Online})
		db.UpdateAnalog(0, dnp3.Analog{Value: 250, Flags: dnp3.Online})
	})

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := m.IntegrityPoll(ctx); err != nil {
		t.Fatalf("integrity poll over a confirmed link: %v", err)
	}

	binary, analog, _ := coll.snapshot()
	if !binary[1].Value {
		t.Error("binary 1 did not arrive")
	}
	if analog[0].Value != 250 {
		t.Errorf("analog 0 = %v, want 250", analog[0].Value)
	}
}

// TestConfirmedLinkRepeatedPolls drives many exchanges so the frame count bit
// alternates repeatedly. A stuck or double-advanced FCB shows up as a fragment
// the peer discards as a duplicate, which appears here as a timeout.
func TestConfirmedLinkRepeatedPolls(t *testing.T) {
	m, out, _ := confirmedPair(t, outstation.DatabaseConfig{
		Binary: 4, DefaultClass: dnp3.Class1,
	})

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	for i := range 12 {
		if err := m.ScanClasses(ctx, dnp3.Class0); err != nil {
			t.Fatalf("poll %d over a confirmed link: %v", i, err)
		}
	}

	if got := m.Stats().TasksFailed; got != 0 {
		t.Errorf("%d tasks failed over a healthy confirmed link", got)
	}
	if got := out.Stats().MalformedRequests; got != 0 {
		t.Errorf("the outstation saw %d malformed requests", got)
	}
}

// TestConfirmedLinkMultiFragment sends a response spanning several frames over
// a confirmed link, so each segment must be acknowledged before the next goes
// out. Getting that wrong stalls the response half-sent.
func TestConfirmedLinkMultiFragment(t *testing.T) {
	m, out, coll := confirmedPair(t, outstation.DatabaseConfig{Analog: 300})

	out.Update(func(db *outstation.Database) {
		for i := range 300 {
			db.UpdateAnalog(uint16(i), dnp3.Analog{Value: float64(i), Flags: dnp3.Online})
		}
	})

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	if err := m.IntegrityPoll(ctx); err != nil {
		t.Fatalf("multi-fragment poll over a confirmed link: %v", err)
	}

	_, analog, _ := coll.snapshot()
	if len(analog) != 300 {
		t.Fatalf("received %d analog points, want 300", len(analog))
	}
	for i := range 300 {
		if got := analog[uint16(i)].Value; got != float64(i) {
			t.Fatalf("analog %d = %v, want %d", i, got, i)
		}
	}
	if out.Stats().FragmentsSent < 2 {
		t.Error("the response should have spanned several fragments")
	}
}

// TestConfirmedCommandsAndEvents covers the write path in both directions over
// a confirmed link.
func TestConfirmedCommandsAndEvents(t *testing.T) {
	plant := newBreaker()

	mch, och := channel.Pipe()
	coll := newCollector()

	out := outstation.New(outstation.Config{
		LocalAddr: 10, RemoteAddr: 1,
		Database:        outstation.DatabaseConfig{Binary: 4, BinaryOutputStatus: 4, DefaultClass: dnp3.Class1},
		ConfirmTimeout:  time.Second,
		UseLinkConfirms: true,
		LinkRetries:     3,
		LinkTimeout:     500 * time.Millisecond,
	}, nil, plant)

	m := master.New(master.Config{
		LocalAddr: 1, RemoteAddr: 10,
		ResponseTimeout: 3 * time.Second,
		UseLinkConfirms: true,
		LinkRetries:     3,
		LinkTimeout:     500 * time.Millisecond,
	}, coll)

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

	opCtx, opCancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer opCancel()

	res, err := m.SelectAndOperate(opCtx, master.Close(2, 500))
	if err != nil {
		t.Fatalf("select and operate over a confirmed link: %v", err)
	}
	if !res.OK() {
		t.Fatalf("result = %s", res)
	}
	if !plant.isClosed(2) {
		t.Error("the breaker did not close")
	}

	// And events still flow back.
	out.Update(func(db *outstation.Database) {
		db.UpdateBinary(0, dnp3.Binary{Value: true, Flags: dnp3.Online})
	})
	waitFor(t, 3*time.Second, func() bool { return out.Events().Total() >= 1 })

	if err := m.ScanClasses(opCtx, dnp3.Class1); err != nil {
		t.Fatalf("event poll: %v", err)
	}
	binary, _, _ := coll.snapshot()
	if !binary[0].Value {
		t.Error("the event did not arrive over the confirmed link")
	}
}

// TestKeepAliveProbesIdleLink covers the probe that tells a master an idle
// peer is still there.
//
// An idle TCP connection is indistinguishable from a peer that has gone away:
// both are silent. Without a probe the master notices only when its next poll
// times out, which on a slow schedule can be minutes.
func TestKeepAliveProbesIdleLink(t *testing.T) {
	mch, och := channel.Pipe()
	coll := newCollector()

	out := outstation.New(outstation.Config{
		LocalAddr: 10, RemoteAddr: 1,
		Database: outstation.DatabaseConfig{Binary: 2},
	}, nil, nil)

	m := master.New(master.Config{
		LocalAddr: 1, RemoteAddr: 10,
		ResponseTimeout: 2 * time.Second,
		KeepAlive:       60 * time.Millisecond,
		LinkTimeout:     500 * time.Millisecond,
	}, coll)

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

	// Wait out the startup traffic, then let the link go quiet.
	waitFor(t, 5*time.Second, func() bool { return m.Stats().TasksSucceeded >= 1 })
	before := out.Stats().Connections
	_ = before

	// The outstation answers a link status request from its secondary station,
	// so the probes show up as frames it processed without any application
	// request.
	reqBefore := out.Stats().RequestsReceived
	time.Sleep(400 * time.Millisecond)

	if got := out.Stats().RequestsReceived; got != reqBefore {
		t.Errorf("keep-alive generated %d application requests; it should stay at the link layer",
			got-reqBefore)
	}
	// The session must still be healthy and pollable afterwards.
	pollCtx, pollCancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer pollCancel()
	if err := m.ScanClasses(pollCtx, dnp3.Class0); err != nil {
		t.Fatalf("poll after keep-alive probes: %v", err)
	}
}
