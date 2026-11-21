//go:build linux

package netpeer

import (
	"bytes"
	"encoding/binary"
	"net/netip"
	"testing"
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
