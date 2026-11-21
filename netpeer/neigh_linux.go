//go:build linux

package netpeer

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// Neighbour states, from linux/neighbour.h.
const (
	nudIncomplete = 0x01
	nudReachable  = 0x02
	nudStale      = 0x04
	nudDelay      = 0x08
	nudProbe      = 0x10
	nudFailed     = 0x20
	nudNoARP      = 0x40
	nudPermanent  = 0x80

	ndaDst    = 1
	ndaLLAddr = 2

	sizeofNdMsg = 12
)

func usableState(state uint16) bool {
	return state&(nudReachable|nudStale|nudDelay|nudProbe|nudPermanent|nudNoARP) != 0 &&
		state&(nudIncomplete|nudFailed) == 0
}

type linuxResolver struct{}

func platformResolver() Resolver { return linuxResolver{} }

func loopbackName() string { return "lo" }

// Lookup dumps the kernel neighbour table over netlink, falling back to
// /proc/net/arp for IPv4 when netlink is refused (a seccomp profile, say).
func (linuxResolver) Lookup(ctx context.Context, ip netip.Addr) (Neighbor, error) {
	if err := ctx.Err(); err != nil {
		return Neighbor{}, err
	}
	ns, err := netlinkNeighbors()
	if err != nil {
		if !ip.Is4() {
			return Neighbor{}, err
		}
		f, ferr := os.ReadFile("/proc/net/arp")
		if ferr != nil {
			return Neighbor{}, err
		}
		ns = parseProcARP(f)
	}
	for _, n := range ns {
		if n.IP == ip {
			return n, nil
		}
	}
	return Neighbor{}, ErrNotFound
}

func netlinkNeighbors() ([]Neighbor, error) {
	tab, err := syscall.NetlinkRIB(syscall.RTM_GETNEIGH, syscall.AF_UNSPEC)
	if err != nil {
		return nil, err
	}
	msgs, err := syscall.ParseNetlinkMessage(tab)
	if err != nil {
		return nil, err
	}
	names := map[int32]string{}
	var out []Neighbor
	for _, m := range msgs {
		if m.Header.Type != syscall.RTM_NEWNEIGH {
			continue
		}
		n, ifindex, ok := parseNeighMsg(m.Data)
		if !ok {
			continue
		}
		name, seen := names[ifindex]
		if !seen {
			if ifc, err := net.InterfaceByIndex(int(ifindex)); err == nil {
				name = ifc.Name
			}
			names[ifindex] = name
		}
		n.Interface = name
		out = append(out, n)
	}
	return out, nil
}

// parseNeighMsg decodes one RTM_NEWNEIGH payload: an ndmsg header followed by
// route attributes.
func parseNeighMsg(b []byte) (Neighbor, int32, bool) {
	if len(b) < sizeofNdMsg {
		return Neighbor{}, 0, false
	}
	ifindex := int32(binary.NativeEndian.Uint32(b[4:8]))
	state := binary.NativeEndian.Uint16(b[8:10])
	if !usableState(state) {
		return Neighbor{}, 0, false
	}
	var n Neighbor
	for rest := b[sizeofNdMsg:]; len(rest) >= 4; {
		l := int(binary.NativeEndian.Uint16(rest[0:2]))
		typ := binary.NativeEndian.Uint16(rest[2:4])
		if l < 4 || l > len(rest) {
			break
		}
		data := rest[4:l]
		switch typ {
		case ndaDst:
			if a, ok := netip.AddrFromSlice(data); ok {
				n.IP = a.Unmap()
			}
		case ndaLLAddr:
			if len(data) == 6 || len(data) == 8 || len(data) == 20 {
				n.MAC = append(net.HardwareAddr(nil), data...)
			}
		}
		adv := (l + 3) &^ 3
		if adv > len(rest) {
			break
		}
		rest = rest[adv:]
	}
	if !n.IP.IsValid() || n.MAC == nil || isZeroMAC(n.MAC) {
		return Neighbor{}, 0, false
	}
	return n, ifindex, true
}

// parseProcARP reads /proc/net/arp. Only complete entries (flag 0x2) count.
func parseProcARP(b []byte) []Neighbor {
	var out []Neighbor
	sc := bufio.NewScanner(bytes.NewReader(b))
	first := true
	for sc.Scan() {
		if first {
			first = false
			continue
		}
		f := strings.Fields(sc.Text())
		if len(f) < 6 {
			continue
		}
		ip, err := netip.ParseAddr(f[0])
		if err != nil {
			continue
		}
		flags, err := strconv.ParseUint(strings.TrimPrefix(f[2], "0x"), 16, 32)
		if err != nil || flags&0x2 == 0 {
			continue
		}
		mac, err := net.ParseMAC(f[3])
		if err != nil || isZeroMAC(mac) {
			continue
		}
		out = append(out, Neighbor{IP: ip, MAC: mac, Interface: f[5]})
	}
	return out
}

// defaultGateway returns the IPv4 default route's gateway from
// /proc/net/route. IPv6 peers off-link get no gateway MAC.
func defaultGateway(ip netip.Addr) (netip.Addr, string, bool) {
	if !ip.Is4() {
		return netip.Addr{}, "", false
	}
	b, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return netip.Addr{}, "", false
	}
	return parseProcRoute(b)
}

func parseProcRoute(b []byte) (netip.Addr, string, bool) {
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 4 || f[1] != "00000000" {
			continue
		}
		// The kernel prints the big-endian address as a host-order integer,
		// so the integer's native encoding is the address's octets.
		v, err := strconv.ParseUint(f[2], 16, 32)
		if err != nil {
			continue
		}
		var raw [4]byte
		binary.NativeEndian.PutUint32(raw[:], uint32(v))
		gw := netip.AddrFrom4(raw)
		if gw.IsUnspecified() {
			continue
		}
		return gw, f[0], true
	}
	return netip.Addr{}, "", false
}
