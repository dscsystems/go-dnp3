package dnp3_test

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/dscsystems/go-dnp3/channel"
	"github.com/dscsystems/go-dnp3/master"
	"github.com/dscsystems/go-dnp3/outstation"
	"github.com/dscsystems/go-dnp3/secevent"
)

// eventLog collects events and lets a test wait for one.
type eventLog struct {
	mu     sync.Mutex
	events []secevent.Event
	added  chan struct{}
}

func newEventLog() *eventLog { return &eventLog{added: make(chan struct{}, 1)} }

func (l *eventLog) HandleEvent(e secevent.Event) {
	l.mu.Lock()
	l.events = append(l.events, e)
	l.mu.Unlock()
	select {
	case l.added <- struct{}{}:
	default:
	}
}

func (l *eventLog) find(k secevent.Kind) (secevent.Event, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, e := range l.events {
		if e.Kind == k {
			return e, true
		}
	}
	return secevent.Event{}, false
}

func (l *eventLog) wait(t *testing.T, k secevent.Kind) secevent.Event {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		if e, ok := l.find(k); ok {
			return e
		}
		select {
		case <-l.added:
		case <-deadline:
			l.mu.Lock()
			defer l.mu.Unlock()
			var got []secevent.Kind
			for _, e := range l.events {
				got = append(got, e.Kind)
			}
			t.Fatalf("no %s event; got %v", k, got)
		}
	}
}

func (l *eventLog) policy() *secevent.Policy {
	return &secevent.Policy{Sink: secevent.SyncSink(l)}
}

// runOutstationOn starts an outstation on ch and returns the bound address.
func runOutstationOn(t *testing.T, cfg outstation.Config, ch channel.Channel) string {
	t.Helper()
	return runOutstationWith(t, cfg, nil, ch)
}

// runOutstationWith is runOutstationOn with a command handler.
func runOutstationWith(t *testing.T, cfg outstation.Config, cmds outstation.CommandHandler, ch channel.Channel) string {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	t.Cleanup(func() { cancel(); <-done; _ = ch.Close() })
	o := outstation.New(cfg, nil, cmds)
	go func() { defer close(done); _ = o.Run(ctx, ch) }()
	for range 400 {
		if a := channel.ServerAddr(ch); a != nil {
			return a.String()
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the outstation never bound")
	return ""
}

// A connection that sends nothing and closes is reported from accept to close,
// with the peer's address, as a likely scan.
func TestSecurityEventsConnectionLifecycle(t *testing.T) {
	log := newEventLog()
	addr := runOutstationOn(t, outstation.Config{LocalAddr: 10, RemoteAddr: 1, Security: log.policy()},
		channel.TCPServer("127.0.0.1:0"))

	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	accepted := log.wait(t, secevent.KindConnAccepted)
	if accepted.Peer.RemoteAddr.String() != c.LocalAddr().String() || accepted.Role != "outstation" ||
		accepted.Transport != "tcp" || accepted.ConnID == 0 {
		t.Fatalf("accepted = %+v", accepted)
	}
	_ = c.Close()
	noData := log.wait(t, secevent.KindConnNoData)
	closed := log.wait(t, secevent.KindConnClosed)
	if noData.ConnID != accepted.ConnID || closed.ConnID != accepted.ConnID {
		t.Fatal("the connection's events do not share its ConnID")
	}
}

func TestSecurityEventsBusyRejection(t *testing.T) {
	log := newEventLog()
	addr := runOutstationOn(t, outstation.Config{LocalAddr: 10, RemoteAddr: 1, Security: log.policy()},
		channel.TCPServerWithOptions("127.0.0.1:0", channel.ServerOptions{Busy: channel.BusyReject}))

	first, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	log.wait(t, secevent.KindConnAccepted)
	second, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	e := log.wait(t, secevent.KindConnRejectedBusy)
	if e.Action != secevent.Rejected || e.Peer.RemoteAddr.String() != second.LocalAddr().String() {
		t.Fatalf("rejected = %+v", e)
	}
}

func TestSecurityEventsIdleTimeout(t *testing.T) {
	log := newEventLog()
	addr := runOutstationOn(t, outstation.Config{
		LocalAddr: 10, RemoteAddr: 1, Security: log.policy(),
		IdleTimeout: 100 * time.Millisecond, LinkTimeout: 100 * time.Millisecond,
	}, channel.TCPServer("127.0.0.1:0"))
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if e := log.wait(t, secevent.KindConnIdleTimeout); e.Action != secevent.Disconnected {
		t.Fatalf("idle = %+v", e)
	}
}

func TestSecurityEventsUDPForeignSource(t *testing.T) {
	log := newEventLog()
	peer, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	addr := runOutstationOn(t, outstation.Config{LocalAddr: 10, RemoteAddr: 1, Security: log.policy()},
		channel.UDPChannel(channel.UDPConfig{LocalAddr: "127.0.0.1:0", RemoteAddr: peer.LocalAddr().String(), PinPeer: true}))

	stranger, err := net.Dial("udp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer stranger.Close()
	if _, err := stranger.Write([]byte{0x05, 0x64}); err != nil {
		t.Fatal(err)
	}
	e := log.wait(t, secevent.KindConnUDPForeignSource)
	if e.Action != secevent.Blocked || e.Peer.RemoteAddr.String() != stranger.LocalAddr().String() {
		t.Fatalf("foreign = %+v", e)
	}
}

func TestSecurityEventsMasterDialAndConnect(t *testing.T) {
	log := newEventLog()
	// Nothing listens here, so the dial fails.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := l.Addr().String()
	_ = l.Close()

	m := master.New(master.Config{LocalAddr: 1, RemoteAddr: 10, Security: log.policy()}, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := m.Run(ctx, channel.TCPClient(dead, channel.NoRetry)); err == nil {
		t.Fatal("dialling nothing succeeded")
	}
	if e := log.wait(t, secevent.KindConnDialFailed); e.Role != "master" || e.Err == nil {
		t.Fatalf("dial failed = %+v", e)
	}

	// And a session that connects reports it.
	log2 := newEventLog()
	addr := runOutstationOn(t, outstation.Config{LocalAddr: 10, RemoteAddr: 1}, channel.TCPServer("127.0.0.1:0"))
	m2 := master.New(master.Config{LocalAddr: 1, RemoteAddr: 10, Security: log2.policy()}, nil)
	ctx2, cancel2 := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _ = m2.Run(ctx2, channel.TCPClient(addr, channel.NoRetry)) }()
	e := log2.wait(t, secevent.KindConnConnected)
	if p, ok := m2.PeerInfo(); !ok || p.RemoteAddr.String() != addr || e.Peer.RemoteAddr.String() != addr {
		t.Fatalf("peer = %+v ok=%v event=%+v", p, ok, e)
	}
	cancel2()
	<-done
	log2.wait(t, secevent.KindConnClosed)
	if st := m2.SecurityStats(); st.ByKind[secevent.KindConnConnected] != 1 {
		t.Fatalf("stats = %+v", st)
	}
}
