package multidrop

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/channel"
	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/internal/link"
	"github.com/dscsystems/go-dnp3/master"
	"github.com/dscsystems/go-dnp3/outstation"
)

// These tests run several real sessions over one stream. That is the whole
// claim of this package — a serial port cannot be opened twice, so the sessions
// have to share one — and the only way to prove it is to make two masters and
// two outstations talk past each other and check that nobody hears the wrong
// device.

// collector records the analogs a master is given.
type collector struct {
	master.NopHandler

	mu     sync.Mutex
	analog map[uint16]float64
}

func newCollector() *collector {
	return &collector{analog: map[uint16]float64{}}
}

func (c *collector) HandleAnalog(_ master.HeaderInfo, vs []dnp3.Indexed[dnp3.Analog]) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, v := range vs {
		c.analog[v.Index] = v.Value.Value
	}
}

func (c *collector) value(index uint16) (float64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.analog[index]
	return v, ok
}

// TestBusCarriesSeveralStations polls two outstations from two masters over one
// stream, with the link layer both unconfirmed and confirmed — the second is
// how a real serial line is configured, and it puts acknowledgements in the
// middle of every exchange for the arbitration to get right.
func TestBusCarriesSeveralStations(t *testing.T) {
	for _, confirms := range []bool{false, true} {
		t.Run(fmt.Sprintf("link_confirms=%v", confirms), func(t *testing.T) {
			addrs := []uint16{10, 11}

			mside, oside := channel.Pipe()
			mbus := New(mside, Config{})
			obus := New(oside, Config{})

			ctx, cancel := context.WithCancel(t.Context())
			var wg sync.WaitGroup
			t.Cleanup(func() {
				cancel()
				_ = mbus.Close()
				_ = obus.Close()
				wg.Wait()
			})

			masters := make([]*master.Session, len(addrs))
			colls := make([]*collector, len(addrs))
			outs := make([]*outstation.Session, len(addrs))
			mchans := make([]channel.Channel, len(addrs))

			for i, addr := range addrs {
				mch, err := mbus.Add(Station{LocalAddr: 1, RemoteAddr: addr, Master: true})
				if err != nil {
					t.Fatalf("adding master for %d: %v", addr, err)
				}
				mchans[i] = mch

				och, err := obus.Add(Station{LocalAddr: addr, RemoteAddr: 1})
				if err != nil {
					t.Fatalf("adding outstation %d: %v", addr, err)
				}

				out := outstation.New(outstation.Config{
					LocalAddr:       addr,
					RemoteAddr:      1,
					Database:        outstation.DatabaseConfig{Analog: 2},
					UseLinkConfirms: confirms,
					LinkTimeout:     200 * time.Millisecond,
					LinkRetries:     3,
				}, nil, nil)
				outs[i] = out

				// Each outstation reports a value only it could have sent, so a
				// misrouted response is a wrong number rather than a silence.
				value := float64(100 + addr)
				out.Update(func(db *outstation.Database) {
					db.UpdateAnalog(0, dnp3.Analog{Value: value, Flags: dnp3.Online})
				})

				colls[i] = newCollector()
				masters[i] = master.New(master.Config{
					LocalAddr:       1,
					RemoteAddr:      addr,
					ResponseTimeout: 2 * time.Second,
					UseLinkConfirms: confirms,
					LinkTimeout:     200 * time.Millisecond,
					LinkRetries:     3,
				}, colls[i])

				wg.Add(1)
				go func() { defer wg.Done(); _ = out.Run(ctx, och) }()
			}

			// Every outstation is on the line before a master transmits. A
			// station whose session has not connected yet is a device that is
			// not powered up: the bus drops what is addressed to it, and this
			// test is about routing rather than about a master's retries.
			for i, out := range outs {
				waitFor(t, 5*time.Second, func() bool { return out.Stats().Connections > 0 })
				m := masters[i]
				mch := mchans[i]
				wg.Add(1)
				go func() { defer wg.Done(); _ = m.Run(ctx, mch) }()
			}

			start := time.Now()
			for i, m := range masters {
				waitFor(t, 5*time.Second, func() bool { return m.Connected() })

				pollCtx, pollCancel := context.WithTimeout(ctx, 10*time.Second)
				err := m.IntegrityPoll(pollCtx)
				pollCancel()
				if err != nil {
					t.Fatalf("master %d: integrity poll: %v", addrs[i], err)
				}

				want := float64(100 + addrs[i])
				got, ok := colls[i].value(0)
				if !ok {
					t.Fatalf("master %d received no analog 0", addrs[i])
				}
				if got != want {
					t.Errorf("master %d read analog 0 = %v, want %v: a response reached the wrong session",
						addrs[i], got, want)
				}
			}

			// Nothing here should ever wait for the turnaround: every exchange
			// ends with a reply that hands the line back. A run that takes one
			// means a station is holding the line it has finished with, which
			// costs nothing on a pipe and paces a real line to a crawl.
			if elapsed := time.Since(start); elapsed >= DefaultTurnaround {
				t.Errorf("polling both outstations took %v, at least one full turnaround: a hold was not released", elapsed)
			}

			for name, bus := range map[string]*Bus{"master side": mbus, "outstation side": obus} {
				st := bus.Stats()
				if st.FramesRouted == 0 {
					t.Errorf("the %s bus routed nothing: %+v", name, st)
				}
				if st.FramesUnrouted != 0 || st.FramesDropped != 0 {
					t.Errorf("the %s bus lost frames: %+v", name, st)
				}
				if st.HeaderCRCErrors+st.BodyCRCErrors+st.BadLength != 0 {
					t.Errorf("the %s bus saw framing errors on a clean pipe: %+v", name, st)
				}
			}
		})
	}
}

// TestAddRejectsAmbiguousStations covers the configurations where routing has
// no answer. Refusing them at setup is the point: the alternative is a line
// that works until the second station is polled.
func TestAddRejectsAmbiguousStations(t *testing.T) {
	bus := New(blockingChannel{}, Config{})
	t.Cleanup(func() { _ = bus.Close() })

	if _, err := bus.Add(Station{LocalAddr: 1, RemoteAddr: 10, Master: true}); err != nil {
		t.Fatalf("first master: %v", err)
	}
	// A second master polling a different outstation is the normal case.
	if _, err := bus.Add(Station{LocalAddr: 1, RemoteAddr: 11, Master: true}); err != nil {
		t.Fatalf("second master: %v", err)
	}
	if _, err := bus.Add(Station{LocalAddr: 1, RemoteAddr: 10, Master: true}); err == nil {
		t.Error("two masters polling the same outstation were accepted")
	}
	// An outstation takes every source, so it cannot share an address.
	if _, err := bus.Add(Station{LocalAddr: 20, RemoteAddr: 1}); err != nil {
		t.Fatalf("outstation: %v", err)
	}
	if _, err := bus.Add(Station{LocalAddr: 20, RemoteAddr: 2}); err == nil {
		t.Error("two outstations at address 20 were accepted")
	}
	if _, err := bus.Add(Station{LocalAddr: link.BroadcastNoConfirm, RemoteAddr: 1}); err == nil {
		t.Error("a station at the broadcast address was accepted")
	}
}

// TestConnectReportsChannelFailure proves a port that is not there surfaces as
// a session error rather than as sessions that quietly never connect.
func TestConnectReportsChannelFailure(t *testing.T) {
	want := errors.New("no such device")
	bus := New(failingChannel{err: want}, Config{})
	t.Cleanup(func() { _ = bus.Close() })

	ch, err := bus.Add(Station{LocalAddr: 1, RemoteAddr: 10, Master: true})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := ch.Connect(t.Context()); !errors.Is(err, want) {
		t.Errorf("connect returned %v, want the channel's own error", err)
	}
}

// TestConnectWaitsForTheLine covers a session started before the port is
// available: it waits rather than failing, and it still honours its context.
func TestConnectWaitsForTheLine(t *testing.T) {
	bus := New(blockingChannel{}, Config{})
	t.Cleanup(func() { _ = bus.Close() })

	ch, err := bus.Add(Station{LocalAddr: 1, RemoteAddr: 10, Master: true})
	if err != nil {
		t.Fatalf("add: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, err := ch.Connect(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("connect returned %v, want the context error", err)
	}
}

// TestStationCloseLeavesTheBusUp covers one session shutting down: the line and
// every other station on it carry on.
func TestStationCloseLeavesTheBusUp(t *testing.T) {
	a, b := channel.Pipe()
	t.Cleanup(func() { _ = b.Close() })

	bus := New(a, Config{})
	t.Cleanup(func() { _ = bus.Close() })

	one, err := bus.Add(Station{LocalAddr: 1, RemoteAddr: 10, Master: true})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	two, err := bus.Add(Station{LocalAddr: 1, RemoteAddr: 11, Master: true})
	if err != nil {
		t.Fatalf("add: %v", err)
	}

	if _, err := one.Connect(t.Context()); err != nil {
		t.Fatalf("connect one: %v", err)
	}
	if _, err := two.Connect(t.Context()); err != nil {
		t.Fatalf("connect two: %v", err)
	}
	if err := one.Close(); err != nil {
		t.Fatalf("close one: %v", err)
	}

	if _, err := two.Connect(t.Context()); err != nil {
		t.Errorf("the surviving station lost the line: %v", err)
	}
	if _, err := one.Connect(t.Context()); !errors.Is(err, channel.ErrClosed) {
		t.Errorf("a removed station reconnected: %v", err)
	}
}

// TestBusReconnects covers the line dropping under the sessions: their
// connections end, the bus reopens the channel, and they come back onto it. A
// session cannot tell this from a socket that dropped, which is the point.
func TestBusReconnects(t *testing.T) {
	a, b := channel.Pipe()
	bus := New(a, Config{})
	t.Cleanup(func() { _ = bus.Close(); _ = b.Close() })

	ch, err := bus.Add(Station{LocalAddr: 1, RemoteAddr: 10, Master: true})
	if err != nil {
		t.Fatalf("add: %v", err)
	}

	peer, err := b.Connect(t.Context())
	if err != nil {
		t.Fatalf("peer connect: %v", err)
	}
	first, err := ch.Connect(t.Context())
	if err != nil {
		t.Fatalf("station connect: %v", err)
	}

	// Drop the line from the far end.
	_ = peer.Close()

	buf := make([]byte, link.MaxFrameSize)
	if _, err := first.Read(buf); !errors.Is(err, io.EOF) {
		t.Fatalf("the station read %v after the line dropped, want EOF", err)
	}

	peer, err = b.Connect(t.Context())
	if err != nil {
		t.Fatalf("peer reconnect: %v", err)
	}
	second, err := ch.Connect(t.Context())
	if err != nil {
		t.Fatalf("station reconnect: %v", err)
	}

	// Prove the new connection is really carrying traffic: a frame from the
	// outstation has to reach the station that asked for it.
	frame, err := link.Encode(nil, link.Header{
		Control: link.Control{Func: link.FuncLinkStatus},
		Dest:    1,
		Src:     10,
	}, nil)
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	go func() { _, _ = peer.Write(frame) }()

	n, err := second.Read(buf)
	if err != nil {
		t.Fatalf("read after reconnect: %v", err)
	}
	if !bytes.Equal(buf[:n], frame) {
		t.Errorf("read % x, want % x", buf[:n], frame)
	}

	if st := bus.Stats(); st.Connections != 2 {
		t.Errorf("the bus connected %d times, want 2", st.Connections)
	}
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("condition not met within %v", d)
}

type failingChannel struct{ err error }

func (f failingChannel) Connect(context.Context) (io.ReadWriteCloser, error) { return nil, f.err }
func (failingChannel) Close() error                                          { return nil }
func (failingChannel) String() string                                        { return "failing" }

// blockingChannel stands in for a port that is not there yet: the connect
// blocks, as a retrying channel's would.
type blockingChannel struct{}

func (blockingChannel) Connect(ctx context.Context) (io.ReadWriteCloser, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (blockingChannel) Close() error   { return nil }
func (blockingChannel) String() string { return "blocking" }

// ---------- arbitration ----------

func TestArbiterHoldsTheLineForOneMaster(t *testing.T) {
	var a arbiter
	a.init(time.Second)

	one, two := &conn{}, &conn{}
	if err := a.acquire(one); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	got := make(chan error, 1)
	go func() { got <- a.acquire(two) }()

	select {
	case <-got:
		t.Fatal("a second master transmitted while the first was awaiting a reply")
	case <-time.After(20 * time.Millisecond):
	}

	// The reply arrives complete, which ends the exchange.
	a.observe(one, true)

	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("acquire after release: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("the line was never handed over")
	}
}

// TestArbiterExpiresASilentHold is what keeps one dead outstation from stopping
// the line for everybody else.
func TestArbiterExpiresASilentHold(t *testing.T) {
	var a arbiter
	a.init(50 * time.Millisecond)

	one, two := &conn{}, &conn{}
	if err := a.acquire(one); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	start := time.Now()
	if err := a.acquire(two); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 40*time.Millisecond {
		t.Errorf("the line was taken after %v, before the turnaround elapsed", elapsed)
	}
}

// TestArbiterExtendsWhileTheReplyArrives covers a response spanning several
// frames: releasing on the first would let another master transmit into the
// middle of it.
func TestArbiterExtendsWhileTheReplyArrives(t *testing.T) {
	var a arbiter
	a.init(60 * time.Millisecond)

	one, two := &conn{}, &conn{}
	if err := a.acquire(one); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	got := make(chan error, 1)
	go func() { got <- a.acquire(two) }()

	// Keep the reply coming for longer than one turnaround.
	for range 3 {
		time.Sleep(30 * time.Millisecond)
		a.observe(one, false)
		select {
		case <-got:
			t.Fatal("the line was taken while a reply was still arriving")
		default:
		}
	}

	a.observe(one, true)
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("the line was never handed over")
	}
}

func TestArbiterDisabled(t *testing.T) {
	var a arbiter
	a.init(0)

	one, two := &conn{}, &conn{}
	if err := a.acquire(one); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := a.acquire(two); err != nil {
		t.Fatalf("acquire with arbitration off: %v", err)
	}
}

func TestExpectsReply(t *testing.T) {
	build := func(ctrl link.Control, dest uint16, payload ...byte) []byte {
		f, err := link.Encode(nil, link.Header{Control: ctrl, Dest: dest, Src: 1}, payload)
		if err != nil {
			t.Fatalf("encoding: %v", err)
		}
		return f
	}
	frame := func(dest uint16, payload ...byte) []byte {
		return build(link.Control{Dir: true, Prm: true, Func: link.FuncUnconfirmedUserData}, dest, payload...)
	}
	// An acknowledgement a master sends back to an outstation: a secondary
	// frame, and the most common thing on a confirmed link.
	ack := build(link.Control{Dir: true, Func: link.FuncAck}, 10)

	// A first segment: transport FIR and FIN, then the application header.
	segment := func(fn app.FuncCode) []byte { return []byte{0xC0, 0xC0, byte(fn)} }

	cases := []struct {
		name  string
		frame []byte
		want  bool
	}{
		{"a read is answered", frame(10, segment(app.FuncRead)...), true},
		{"a confirm is not", frame(10, segment(app.FuncConfirm)...), false},
		{"a no-reply control is not", frame(10, segment(app.FuncDirectOperateNR)...), false},
		{"nothing answers a broadcast", frame(link.BroadcastNoConfirm, segment(app.FuncWrite)...), false},
		{"a link status request is answered", frame(10), true},
		{"an acknowledgement is not", ack, false},
		{"a continuation segment keeps the line", frame(10, 0x01, 0x00, byte(app.FuncConfirm)), true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := expectsReply(c.frame); got != c.want {
				t.Errorf("expectsReply = %v, want %v", got, c.want)
			}
		})
	}
}
