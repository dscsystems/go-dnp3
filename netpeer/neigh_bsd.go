//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package netpeer

import (
	"context"
	"net"
	"net/netip"
	"syscall"

	"golang.org/x/net/route"
)

type bsdResolver struct{}

func platformResolver() Resolver { return bsdResolver{} }

func loopbackName() string { return "lo0" }

// Lookup dumps the kernel's link-layer entries (ARP and NDP) from the
// routing socket, as arp(8) and ndp(8) do.
func (bsdResolver) Lookup(ctx context.Context, ip netip.Addr) (Neighbor, error) {
	if err := ctx.Err(); err != nil {
		return Neighbor{}, err
	}
	af := syscall.AF_INET
	if !ip.Is4() {
		af = syscall.AF_INET6
	}
	b, err := route.FetchRIB(af, ribFlags, rtfLLInfo)
	if err != nil {
		return Neighbor{}, err
	}
	msgs, err := route.ParseRIB(ribFlags, b)
	if err != nil {
		return Neighbor{}, err
	}
	want := ip.WithZone("")
	for _, n := range neighborsFromRIB(msgs) {
		if n.IP == want {
			return n, nil
		}
	}
	return Neighbor{}, ErrNotFound
}

func neighborsFromRIB(msgs []route.Message) []Neighbor {
	names := map[int]string{}
	var out []Neighbor
	for _, m := range msgs {
		rm, ok := m.(*route.RouteMessage)
		if !ok || rm.Err != nil || len(rm.Addrs) <= syscall.RTAX_GATEWAY {
			continue
		}
		ip := addrOf(rm.Addrs[syscall.RTAX_DST])
		la, ok := rm.Addrs[syscall.RTAX_GATEWAY].(*route.LinkAddr)
		if !ip.IsValid() || !ok || len(la.Addr) == 0 || isZeroMAC(la.Addr) {
			continue // incomplete entry: no link-layer address yet
		}
		name := la.Name
		if name == "" {
			idx := la.Index
			if idx == 0 {
				idx = rm.Index
			}
			n, seen := names[idx]
			if !seen {
				if ifc, err := net.InterfaceByIndex(idx); err == nil {
					n = ifc.Name
				}
				names[idx] = n
			}
			name = n
		}
		out = append(out, Neighbor{IP: ip, MAC: append(net.HardwareAddr(nil), la.Addr...), Interface: name})
	}
	return out
}

func addrOf(a route.Addr) netip.Addr {
	switch a := a.(type) {
	case *route.Inet4Addr:
		return netip.AddrFrom4(a.IP)
	case *route.Inet6Addr:
		return netip.AddrFrom16(a.IP).Unmap()
	}
	return netip.Addr{}
}

// defaultGateway finds the IPv4 default route in the routing table. IPv6
// peers off-link get no gateway MAC.
func defaultGateway(ip netip.Addr) (netip.Addr, string, bool) {
	if !ip.Is4() {
		return netip.Addr{}, "", false
	}
	b, err := route.FetchRIB(syscall.AF_INET, route.RIBTypeRoute, 0)
	if err != nil {
		return netip.Addr{}, "", false
	}
	msgs, err := route.ParseRIB(route.RIBTypeRoute, b)
	if err != nil {
		return netip.Addr{}, "", false
	}
	for _, m := range msgs {
		rm, ok := m.(*route.RouteMessage)
		if !ok || rm.Flags&syscall.RTF_GATEWAY == 0 || rm.Flags&syscall.RTF_UP == 0 ||
			len(rm.Addrs) <= syscall.RTAX_GATEWAY {
			continue
		}
		if dst := addrOf(rm.Addrs[syscall.RTAX_DST]); !dst.IsValid() || !dst.IsUnspecified() {
			continue
		}
		if len(rm.Addrs) > syscall.RTAX_NETMASK {
			if mask := addrOf(rm.Addrs[syscall.RTAX_NETMASK]); mask.IsValid() && !mask.IsUnspecified() {
				continue
			}
		}
		gw := addrOf(rm.Addrs[syscall.RTAX_GATEWAY])
		if !gw.Is4() || gw.IsUnspecified() {
			continue
		}
		name := ""
		if ifc, err := net.InterfaceByIndex(rm.Index); err == nil {
			name = ifc.Name
		}
		return gw, name, true
	}
	return netip.Addr{}, "", false
}
