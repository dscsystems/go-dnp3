//go:build windows

package netpeer

import (
	"context"
	"net"
	"net/netip"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	iphlpapi          = windows.NewLazySystemDLL("iphlpapi.dll")
	procGetIPNetTable = iphlpapi.NewProc("GetIpNetTable2")
)

type windowsResolver struct{}

func platformResolver() Resolver { return windowsResolver{} }

func loopbackName() string { return "Loopback Pseudo-Interface 1" }

// Lookup reads the neighbour table (ARP and NDP) with GetIpNetTable2.
func (windowsResolver) Lookup(ctx context.Context, ip netip.Addr) (Neighbor, error) {
	if err := ctx.Err(); err != nil {
		return Neighbor{}, err
	}
	if err := procGetIPNetTable.Find(); err != nil {
		return Neighbor{}, ErrUnsupported
	}
	family := uintptr(windows.AF_INET)
	if !ip.Is4() {
		family = windows.AF_INET6
	}
	var table unsafe.Pointer
	r, _, _ := procGetIPNetTable.Call(family, uintptr(unsafe.Pointer(&table)))
	if r != 0 {
		return Neighbor{}, windows.Errno(r)
	}
	defer windows.FreeMibTable(table)
	n := *(*uint32)(table)
	buf := unsafe.Slice((*byte)(table), ipNetTableHeader+int(n)*ipNetRowSize)
	want := ip.WithZone("")
	for _, e := range parseIPNetTable2(buf) {
		if e.IP != want {
			continue
		}
		if ifc, err := net.InterfaceByIndex(int(e.ifIndex)); err == nil {
			e.Interface = ifc.Name
		}
		return e.Neighbor, nil
	}
	return Neighbor{}, ErrNotFound
}

// defaultGateway picks the IPv4 default route with the lowest metric. IPv6
// peers off-link get no gateway MAC.
func defaultGateway(ip netip.Addr) (netip.Addr, string, bool) {
	if !ip.Is4() {
		return netip.Addr{}, "", false
	}
	var table *windows.MibIpForwardTable2
	if err := windows.GetIpForwardTable2(windows.AF_INET, &table); err != nil {
		return netip.Addr{}, "", false
	}
	defer windows.FreeMibTable(unsafe.Pointer(table))
	var (
		best   netip.Addr
		index  uint32
		metric uint32
	)
	for _, row := range table.Rows() {
		if row.DestinationPrefix.PrefixLength != 0 || row.Loopback != 0 {
			continue
		}
		gw := rawInet(row.NextHop)
		if !gw.Is4() || gw.IsUnspecified() {
			continue
		}
		if !best.IsValid() || row.Metric < metric {
			best, index, metric = gw, row.InterfaceIndex, row.Metric
		}
	}
	if !best.IsValid() {
		return netip.Addr{}, "", false
	}
	name := ""
	if ifc, err := net.InterfaceByIndex(int(index)); err == nil {
		name = ifc.Name
	}
	return best, name, true
}

func rawInet(a windows.RawSockaddrInet) netip.Addr {
	b := unsafe.Slice((*byte)(unsafe.Pointer(&a)), unsafe.Sizeof(a))
	return sockaddrInet(b)
}
