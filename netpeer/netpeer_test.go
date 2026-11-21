package netpeer

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"
)

func neighMsg(ifindex int32, state uint16, ip []byte, mac []byte) []byte {
	b := make([]byte, 0, 64)
	b = append(b, make([]byte, sizeofNdMsg)...)
	b[0] = 2 // AF_INET
	binary.NativeEndian.PutUint32(b[4:8], uint32(ifindex))
	binary.NativeEndian.PutUint16(b[8:10], state)
	attr := func(typ uint16, data []byte) {
		l := 4 + len(data)
		h := make([]byte, 4)
		binary.NativeEndian.PutUint16(h[0:2], uint16(l))
		binary.NativeEndian.PutUint16(h[2:4], typ)
		b = append(b, h...)
		b = append(b, data...)
		for len(b)%4 != 0 {
			b = append(b, 0)
		}
	}
	attr(ndaDst, ip)
	if mac != nil {
		attr(ndaLLAddr, mac)
	}
	return b
}

func TestParseNeighMsg(t *testing.T) {
	mac := []byte{0xaa, 0xbb, 0xcc, 0x00, 0x11, 0x22}
	n, idx, ok := parseNeighMsg(neighMsg(3, nudReachable, []byte{192, 168, 1, 7}, mac))
	if !ok || idx != 3 || n.IP != netip.MustParseAddr("192.168.1.7") || !bytes.Equal(n.MAC, mac) {
		t.Fatalf("got %+v idx=%d ok=%v", n, idx, ok)
	}
	if _, _, ok := parseNeighMsg(neighMsg(3, nudIncomplete, []byte{192, 168, 1, 8}, nil)); ok {
		t.Fatal("an incomplete entry was accepted")
	}
	if _, _, ok := parseNeighMsg(neighMsg(3, nudFailed|nudStale, []byte{192, 168, 1, 9}, mac)); ok {
		t.Fatal("a failed entry was accepted")
	}
	if _, _, ok := parseNeighMsg([]byte{1, 2, 3}); ok {
		t.Fatal("a short message was accepted")
	}
	v6 := netip.MustParseAddr("fe80::1").AsSlice()
	if n, _, ok := parseNeighMsg(neighMsg(2, nudStale, v6, mac)); !ok || !n.IP.Is6() {
		t.Fatalf("v6 = %+v ok=%v", n, ok)
	}
}

func TestParseProcARP(t *testing.T) {
	const arp = `IP address       HW type     Flags       HW address            Mask     Device
192.168.1.1      0x1         0x2         aa:bb:cc:dd:ee:ff     *        eth0
192.168.1.9      0x1         0x0         00:00:00:00:00:00     *        eth0
`
	ns := parseProcARP([]byte(arp))
	if len(ns) != 1 || ns[0].IP != netip.MustParseAddr("192.168.1.1") || ns[0].Interface != "eth0" {
		t.Fatalf("got %+v", ns)
	}
}

func TestParseProcRoute(t *testing.T) {
	if binary.NativeEndian.Uint16([]byte{1, 0}) != 1 {
		t.Skip("route file fixture is little-endian")
	}
	const route = "Iface\tDestination\tGateway \tFlags\n" +
		"eth0\t0001A8C0\t00000000\t0001\n" +
		"eth0\t00000000\t0101A8C0\t0003\n"
	gw, iface, ok := parseProcRoute([]byte(route))
	if !ok || iface != "eth0" || gw != netip.MustParseAddr("192.168.1.1") {
		t.Fatalf("gw=%v iface=%q ok=%v", gw, iface, ok)
	}
}

func withInterfaces(t *testing.T, entries ...ifacePrefix) {
	t.Helper()
	old := interfaces
	interfaces = &interfaceCache{list: func() ([]ifacePrefix, error) { return entries, nil }}
	t.Cleanup(func() { interfaces = old })
}

func TestLookupDecidesOnLinkFirst(t *testing.T) {
	withInterfaces(t,
		ifacePrefix{"eth0", netip.MustParsePrefix("10.0.0.5/24")},
	)
	peerMAC := net.HardwareAddr{1, 2, 3, 4, 5, 6}
	gwMAC := net.HardwareAddr{9, 9, 9, 9, 9, 9}
	r := ResolverFunc(func(_ context.Context, ip netip.Addr) (Neighbor, error) {
		switch ip {
		case netip.MustParseAddr("10.0.0.7"):
			return Neighbor{IP: ip, MAC: peerMAC}, nil
		case netip.MustParseAddr("10.0.0.1"):
			return Neighbor{IP: ip, MAC: gwMAC, Interface: "eth0"}, nil
		}
		return Neighbor{}, ErrNotFound
	})
	oldGW := gatewayFor
	gatewayFor = func(netip.Addr) (netip.Addr, string, bool) { return netip.MustParseAddr("10.0.0.1"), "eth0", true }
	t.Cleanup(func() { gatewayFor = oldGW })
	ctx := context.Background()

	res, err := Lookup(ctx, r, netip.MustParseAddr("10.0.0.7"))
	if err != nil || res.Source != SourceNeighbor || !res.OnLink || res.Interface != "eth0" || !bytes.Equal(res.MAC, peerMAC) {
		t.Fatalf("on-link = %+v err=%v", res, err)
	}
	res, err = Lookup(ctx, r, netip.MustParseAddr("8.8.8.8"))
	if err != nil || res.Source != SourceGateway || res.OnLink || !bytes.Equal(res.MAC, gwMAC) {
		t.Fatalf("off-link = %+v err=%v", res, err)
	}
	res, err = Lookup(ctx, r, netip.MustParseAddr("10.0.0.99"))
	if err != nil || res.Source != SourceNone || !res.OnLink {
		t.Fatalf("missing neighbour = %+v err=%v", res, err)
	}
	res, _ = Lookup(ctx, r, netip.MustParseAddr("127.0.0.1"))
	if res.Source != SourceNone || !res.OnLink {
		t.Fatalf("loopback = %+v", res)
	}
	res, _ = Lookup(ctx, r, netip.MustParseAddr("10.0.0.5"))
	if res.Source != SourceNone || !res.OnLink {
		t.Fatalf("own address = %+v", res)
	}
}

func TestCacheReportsChanges(t *testing.T) {
	c := NewCache(time.Hour, 10)
	t0 := time.Now()
	ipA, ipB := netip.MustParseAddr("10.0.0.7"), netip.MustParseAddr("10.0.0.8")
	m1, m2 := net.HardwareAddr{1, 1, 1, 1, 1, 1}, net.HardwareAddr{2, 2, 2, 2, 2, 2}

	if ch := c.Observe(Observation{IP: ipA, MAC: m1, Source: SourceNeighbor, LinkAddr: 1, HasLinkAddr: true, CertSHA256: "aa", Time: t0}); len(ch) != 0 {
		t.Fatalf("first sight reported %+v", ch)
	}
	ch := c.Observe(Observation{IP: ipA, MAC: m2, Source: SourceNeighbor, LinkAddr: 2, HasLinkAddr: true, CertSHA256: "bb", Time: t0.Add(time.Second)})
	kinds := map[ChangeKind]bool{}
	for _, x := range ch {
		kinds[x.Kind] = true
	}
	if !kinds[MACChanged] || !kinds[LinkAddrChanged] || !kinds[CertChanged] || len(ch) != 3 {
		t.Fatalf("changes = %+v", ch)
	}

	// m2 turns up at another IP while still live at ipA: shared.
	ch = c.Observe(Observation{IP: ipB, MAC: m2, Source: SourceNeighbor, Time: t0.Add(2 * time.Second)})
	if len(ch) != 1 || ch[0].Kind != MACShared || ch[0].OldIP != ipA {
		t.Fatalf("shared = %+v", ch)
	}
	// Much later it moves back: an IP change, not sharing.
	ch = c.Observe(Observation{IP: ipA, MAC: m2, Source: SourceNeighbor, Time: t0.Add(10 * time.Minute)})
	if len(ch) != 1 || ch[0].Kind != IPChanged || ch[0].OldIP != ipB {
		t.Fatalf("moved = %+v", ch)
	}

	// A gateway's MAC is not the peer's and is never compared.
	if ch := c.Observe(Observation{IP: ipA, MAC: m1, Source: SourceGateway, Time: t0.Add(11 * time.Minute)}); len(ch) != 0 {
		t.Fatalf("gateway MAC reported %+v", ch)
	}
}

func TestCacheExpiresAndBounds(t *testing.T) {
	c := NewCache(time.Minute, 2)
	t0 := time.Now()
	m1, m2 := net.HardwareAddr{1, 1, 1, 1, 1, 1}, net.HardwareAddr{2, 2, 2, 2, 2, 2}
	ip := netip.MustParseAddr("10.0.0.7")
	c.Observe(Observation{IP: ip, MAC: m1, Source: SourceNeighbor, Time: t0})
	if ch := c.Observe(Observation{IP: ip, MAC: m2, Source: SourceNeighbor, Time: t0.Add(2 * time.Minute)}); len(ch) != 0 {
		t.Fatalf("an expired entry was compared: %+v", ch)
	}
	for i := range 5 {
		c.Observe(Observation{IP: netip.AddrFrom4([4]byte{10, 1, 0, byte(i)}), Time: t0})
	}
	if c.Len() != 2 {
		t.Fatalf("len = %d, want the bound of 2", c.Len())
	}
}

func TestCacheSaveLoad(t *testing.T) {
	c := NewCache(0, 0)
	ip := netip.MustParseAddr("10.0.0.7")
	c.Observe(Observation{IP: ip, MAC: net.HardwareAddr{1, 2, 3, 4, 5, 6}, Source: SourceNeighbor, LinkAddr: 3, HasLinkAddr: true})
	var buf bytes.Buffer
	if err := c.Save(&buf); err != nil {
		t.Fatal(err)
	}
	d := NewCache(0, 0)
	if err := d.Load(&buf); err != nil {
		t.Fatal(err)
	}
	ch := d.Observe(Observation{IP: ip, MAC: net.HardwareAddr{6, 5, 4, 3, 2, 1}, Source: SourceNeighbor})
	if len(ch) != 1 || ch[0].Kind != MACChanged {
		t.Fatalf("after reload: %+v", ch)
	}
}

// The real backend answers without error for an address it has no entry for.
func TestPlatformResolverRuns(t *testing.T) {
	_, err := DefaultResolver().Lookup(context.Background(), netip.MustParseAddr("192.0.2.123"))
	if err != nil && !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrUnsupported) {
		t.Fatalf("lookup: %v", err)
	}
}
