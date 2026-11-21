package dnp3_test

import (
	"context"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/dscsystems/go-dnp3/channel"
	"github.com/dscsystems/go-dnp3/master"
	"github.com/dscsystems/go-dnp3/netpeer"
	"github.com/dscsystems/go-dnp3/outstation"
	"github.com/dscsystems/go-dnp3/secevent"
)

// switchTable is an authoritative resolver whose answer a test can change,
// standing in for a peer whose MAC changes behind the same IP.
type switchTable struct {
	mu  sync.Mutex
	mac net.HardwareAddr
}

func (s *switchTable) set(mac net.HardwareAddr) { s.mu.Lock(); s.mac = mac; s.mu.Unlock() }
func (s *switchTable) Authoritative() bool      { return true }
func (s *switchTable) Lookup(_ context.Context, ip netip.Addr) (netpeer.Neighbor, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return netpeer.Neighbor{IP: ip, MAC: s.mac}, nil
}

var (
	mac1 = net.HardwareAddr{0x02, 0, 0, 0, 0, 1}
	mac2 = net.HardwareAddr{0x02, 0, 0, 0, 0, 2}
)

func dialAndClose(t *testing.T, addr string, log *eventLog, want secevent.Kind) secevent.Event {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	return log.wait(t, want)
}

func TestSecurityEventsMACChangeAndPinning(t *testing.T) {
	sw := &switchTable{mac: mac1}
	cache := netpeer.NewCache(0, 0)
	log := newEventLog()
	pol := log.policy()
	pol.Resolver, pol.MACCache = sw, cache
	addr := runOutstationOn(t, outstation.Config{LocalAddr: 10, RemoteAddr: 1, Security: pol},
		channel.TCPServer("127.0.0.1:0"))

	e := dialAndClose(t, addr, log, secevent.KindConnAccepted)
	if e.Peer.MAC.String() != mac1.String() || e.Peer.MACSource != secevent.MACCustom {
		t.Fatalf("accepted peer = %+v", e.Peer)
	}
	log.wait(t, secevent.KindConnClosed)

	sw.set(mac2)
	changed := dialAndClose(t, addr, log, secevent.KindPeerMACChanged)
	if changed.Severity != secevent.Alert {
		t.Fatalf("mac changed = %+v", changed)
	}
}

func TestSecurityEventsMACPinningRejects(t *testing.T) {
	sw := &switchTable{mac: mac2}
	log := newEventLog()
	pol := log.policy()
	pol.Resolver, pol.MACCache = sw, netpeer.NewCache(0, 0)
	pol.PeerBindings = map[netip.Addr]net.HardwareAddr{netip.MustParseAddr("127.0.0.1"): mac1}
	pol.RejectBindingMismatch = true
	addr := runOutstationOn(t, outstation.Config{LocalAddr: 10, RemoteAddr: 1, Security: pol},
		channel.TCPServer("127.0.0.1:0"))

	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	log.wait(t, secevent.KindPeerMACPinningViolation)
	if e := log.wait(t, secevent.KindConnUnexpectedMAC); e.Action != secevent.Rejected {
		t.Fatalf("unexpected mac = %+v", e)
	}
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("the refused connection is still open")
	}
}

func TestSecurityEventsMACChangeMidSession(t *testing.T) {
	sw := &switchTable{mac: mac1}
	log := newEventLog()
	pol := log.policy()
	pol.Resolver, pol.MACCache = sw, netpeer.NewCache(0, 0)
	pol.MACRecheckInterval = 30 * time.Millisecond
	pol.DisconnectOnMACChange = true
	addr := runOutstationOn(t, outstation.Config{LocalAddr: 10, RemoteAddr: 1, Security: pol},
		channel.TCPServer("127.0.0.1:0"))

	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	log.wait(t, secevent.KindConnAccepted)
	sw.set(mac2)
	log.wait(t, secevent.KindPeerMACChangedMidsession)
	log.wait(t, secevent.KindConnClosed)
}

// The same IP turning up with another DNP3 source address is reported.
func TestSecurityEventsLinkAddressChange(t *testing.T) {
	log := newEventLog()
	pol := log.policy()
	pol.MACCache = netpeer.NewCache(0, 0)
	addr := runOutstationOn(t, outstation.Config{LocalAddr: 10, RemoteAddr: 1, Security: pol},
		channel.TCPServer("127.0.0.1:0"))

	for _, local := range []uint16{1, 2} {
		m := master.New(master.Config{LocalAddr: local, RemoteAddr: 10, ResponseTimeout: time.Second}, nil)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { defer close(done); _ = m.Run(ctx, channel.TCPClient(addr, channel.NoRetry)) }()
		if err := m.IntegrityPoll(ctx); err != nil {
			t.Fatalf("poll from %d: %v", local, err)
		}
		cancel()
		<-done
	}
	e := log.wait(t, secevent.KindPeerLinkAddrChanged)
	if e.LinkSrc != 2 {
		t.Fatalf("link addr changed = %+v", e)
	}
}
