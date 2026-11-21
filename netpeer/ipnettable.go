package netpeer

import (
	"encoding/binary"
	"net"
	"net/netip"
)

// Layout of Windows' MIB_IPNET_TABLE2 (netioapi.h), the same on every
// architecture: a ULONG count padded to 8, then MIB_IPNET_ROW2 entries.
const (
	ipNetTableHeader = 8
	ipNetRowSize     = 88

	rowAddress    = 0  // SOCKADDR_INET (28 octets)
	rowIfIndex    = 28 // NET_IFINDEX
	rowPhysical   = 40 // UCHAR[32]
	rowPhysLength = 72 // ULONG
	rowState      = 76 // NL_NEIGHBOR_STATE

	winAFInet  = 2
	winAFInet6 = 23

	// NL_NEIGHBOR_STATE: Unreachable 0, Incomplete 1, Probe 2, Delay 3,
	// Stale 4, Reachable 5, Permanent 6.
	nlnsProbe     = 2
	nlnsPermanent = 6
)

type winNeighbor struct {
	Neighbor
	ifIndex uint32
}

// parseIPNetTable2 decodes a MIB_IPNET_TABLE2 copied out of the API's buffer.
// Windows stores the table in native (little-endian) order.
func parseIPNetTable2(b []byte) []winNeighbor {
	if len(b) < ipNetTableHeader {
		return nil
	}
	n := int(binary.LittleEndian.Uint32(b))
	var out []winNeighbor
	for i := 0; i < n; i++ {
		off := ipNetTableHeader + i*ipNetRowSize
		if off+ipNetRowSize > len(b) {
			break
		}
		row := b[off : off+ipNetRowSize]
		state := binary.LittleEndian.Uint32(row[rowState:])
		if state < nlnsProbe || state > nlnsPermanent {
			continue
		}
		ip := sockaddrInet(row[rowAddress : rowAddress+28])
		l := int(binary.LittleEndian.Uint32(row[rowPhysLength:]))
		if !ip.IsValid() || l == 0 || l > 32 {
			continue
		}
		mac := append(net.HardwareAddr(nil), row[rowPhysical:rowPhysical+l]...)
		if isZeroMAC(mac) {
			continue
		}
		out = append(out, winNeighbor{
			Neighbor: Neighbor{IP: ip, MAC: mac},
			ifIndex:  binary.LittleEndian.Uint32(row[rowIfIndex:]),
		})
	}
	return out
}

// sockaddrInet decodes a SOCKADDR_INET: family, then sockaddr_in or
// sockaddr_in6 (whose address follows the flow info).
func sockaddrInet(b []byte) netip.Addr {
	switch binary.LittleEndian.Uint16(b) {
	case winAFInet:
		return netip.AddrFrom4([4]byte(b[4:8]))
	case winAFInet6:
		return netip.AddrFrom16([16]byte(b[8:24])).Unmap()
	}
	return netip.Addr{}
}
