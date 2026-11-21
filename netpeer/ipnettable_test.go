package netpeer

import (
	"bytes"
	"encoding/binary"
	"net/netip"
	"testing"
)

func ipNetRow(family uint16, ip []byte, ifindex uint32, mac []byte, state uint32) []byte {
	row := make([]byte, ipNetRowSize)
	binary.LittleEndian.PutUint16(row, family)
	if family == winAFInet {
		copy(row[4:], ip)
	} else {
		copy(row[8:], ip)
	}
	binary.LittleEndian.PutUint32(row[rowIfIndex:], ifindex)
	copy(row[rowPhysical:], mac)
	binary.LittleEndian.PutUint32(row[rowPhysLength:], uint32(len(mac)))
	binary.LittleEndian.PutUint32(row[rowState:], state)
	return row
}

func TestParseIPNetTable2(t *testing.T) {
	mac := []byte{0x00, 0x1b, 0x21, 0x0a, 0x0b, 0x0c}
	v6 := netip.MustParseAddr("fe80::1").As16()
	rows := [][]byte{
		ipNetRow(winAFInet, []byte{10, 0, 0, 5}, 7, mac, 5),             // reachable
		ipNetRow(winAFInet, []byte{10, 0, 0, 6}, 7, nil, 1),             // incomplete
		ipNetRow(winAFInet, []byte{10, 0, 0, 7}, 7, mac, 0),             // unreachable
		ipNetRow(winAFInet, []byte{10, 0, 0, 8}, 7, make([]byte, 6), 6), // zero MAC
		ipNetRow(winAFInet6, v6[:], 9, mac, 4),                          // stale
	}
	b := binary.LittleEndian.AppendUint32(nil, uint32(len(rows)+1)) // one row truncated
	b = append(b, 0, 0, 0, 0)
	for _, r := range rows {
		b = append(b, r...)
	}
	b = append(b, make([]byte, 10)...)

	ns := parseIPNetTable2(b)
	if len(ns) != 2 {
		t.Fatalf("entries = %+v", ns)
	}
	if ns[0].IP != netip.MustParseAddr("10.0.0.5") || ns[0].ifIndex != 7 || !bytes.Equal(ns[0].MAC, mac) {
		t.Fatalf("v4 entry = %+v", ns[0])
	}
	if ns[1].IP != netip.MustParseAddr("fe80::1") || ns[1].ifIndex != 9 {
		t.Fatalf("v6 entry = %+v", ns[1])
	}
	if parseIPNetTable2([]byte{1}) != nil {
		t.Fatal("short buffer parsed")
	}
}
