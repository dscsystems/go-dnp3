// Package netpeer finds the MAC address behind a peer's IP address, and
// remembers which MAC, DNP3 link address and TLS certificate each IP has
// shown before, so a change can be noticed.
//
// A MAC address is only meaningful for a peer on a directly attached subnet.
// For anyone else the neighbour table holds the next router's address, which
// is reported as such ([SourceGateway]) rather than as the peer's: a router
// replacement must not look like a spoofed peer.
//
// It works with any net.Conn and does not depend on the rest of go-dnp3.
package netpeer

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"time"
)

// Source says where a MAC address came from.
type Source uint8

// Sources.
const (
	// SourceNone means no MAC address is known.
	SourceNone Source = iota
	// SourceNeighbor is the peer's own address, from the neighbour table.
	SourceNeighbor
	// SourceGateway is the next hop's address: the peer is not on-link.
	SourceGateway
	// SourceStatic came from configuration.
	SourceStatic
	// SourceCustom came from an application resolver.
	SourceCustom
)

// ErrUnsupported means the platform has no neighbour table backend.
var ErrUnsupported = errors.New("netpeer: neighbour lookup not supported on this platform")

// ErrNotFound means the neighbour table has no usable entry for the address.
var ErrNotFound = errors.New("netpeer: no neighbour entry")

// Neighbor is one neighbour table entry.
type Neighbor struct {
	IP        netip.Addr
	MAC       net.HardwareAddr
	Interface string
}

// Resolver looks an address up in a neighbour table.
type Resolver interface {
	Lookup(ctx context.Context, ip netip.Addr) (Neighbor, error)
}

// ResolverFunc adapts a function to a Resolver.
type ResolverFunc func(ctx context.Context, ip netip.Addr) (Neighbor, error)

// Lookup calls f.
func (f ResolverFunc) Lookup(ctx context.Context, ip netip.Addr) (Neighbor, error) {
	return f(ctx, ip)
}

// Authoritative is implemented by a resolver that knows peers' MAC addresses
// directly — from a switch's forwarding table, an inventory, a test — rather
// than from this host's neighbour table. [Lookup] asks it about every address,
// skipping the on-link decision, and reports what it says as SourceCustom.
type Authoritative interface {
	Resolver
	Authoritative() bool
}

// Result is what [Lookup] found out about a peer.
type Result struct {
	MAC       net.HardwareAddr
	Source    Source
	OnLink    bool
	Interface string
	// Gateway is the next hop whose MAC was reported, for SourceGateway.
	Gateway netip.Addr
	// Local means the address is this host's own, loopback included: there
	// is no neighbour to look up.
	Local bool
}

// Lookup finds the MAC address for a peer, deciding first whether the peer
// is on-link. A nil resolver uses the platform's. Not finding one is not an
// error: the Result says SourceNone.
func Lookup(ctx context.Context, r Resolver, ip netip.Addr) (Result, error) {
	ip = ip.Unmap()
	if !ip.IsValid() {
		return Result{}, nil
	}
	if r == nil {
		r = DefaultResolver()
	}
	if a, ok := r.(Authoritative); ok && a.Authoritative() {
		n, err := r.Lookup(ctx, ip)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				err = nil
			}
			return Result{}, err
		}
		return Result{MAC: n.MAC, Source: SourceCustom, Interface: n.Interface}, nil
	}
	if ip.IsLoopback() {
		return Result{OnLink: true, Interface: loopbackName(), Local: true}, nil
	}
	iface, onLink, local := interfaces.find(ip)
	if local {
		// One of our own addresses: the kernel talks to itself.
		return Result{OnLink: true, Interface: iface, Local: true}, nil
	}
	if onLink {
		n, err := r.Lookup(ctx, ip)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				err = nil
			}
			return Result{OnLink: true, Interface: iface}, err
		}
		if n.Interface == "" {
			n.Interface = iface
		}
		return Result{MAC: n.MAC, Source: SourceNeighbor, OnLink: true, Interface: n.Interface}, nil
	}
	gw, giface, ok := gatewayFor(ip)
	if !ok {
		return Result{}, nil
	}
	n, err := r.Lookup(ctx, gw)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			err = nil
		}
		return Result{Interface: giface, Gateway: gw}, err
	}
	return Result{MAC: n.MAC, Source: SourceGateway, Interface: giface, Gateway: gw}, nil
}

// OnLink reports whether ip is inside a subnet attached to one of this host's
// interfaces, and which.
func OnLink(ip netip.Addr) (iface string, ok bool) {
	ip = ip.Unmap()
	if ip.IsLoopback() {
		return loopbackName(), true
	}
	iface, ok, local := interfaces.find(ip)
	return iface, ok || local
}

// interfaceCache holds the host's attached prefixes, refreshed at most once a
// minute, or at once when an address is not found in them.
type interfaceCache struct {
	mu      sync.Mutex
	at      time.Time
	entries []ifacePrefix
	// list is replaceable in tests.
	list func() ([]ifacePrefix, error)
}

type ifacePrefix struct {
	name   string
	prefix netip.Prefix
}

var interfaces = &interfaceCache{list: systemPrefixes}

const interfaceRefresh = time.Minute

func (c *interfaceCache) find(ip netip.Addr) (iface string, onLink, local bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for attempt := 0; attempt < 2; attempt++ {
		if attempt == 1 || c.entries == nil || time.Since(c.at) > interfaceRefresh {
			if attempt == 1 && time.Since(c.at) < time.Second {
				break // just refreshed; it is genuinely not there
			}
			if e, err := c.list(); err == nil {
				c.entries, c.at = e, time.Now()
			}
		}
		for _, e := range c.entries {
			if e.prefix.Addr() == ip {
				return e.name, false, true
			}
		}
		for _, e := range c.entries {
			if e.prefix.Contains(ip) {
				return e.name, true, false
			}
		}
	}
	return "", false, false
}

func systemPrefixes() ([]ifacePrefix, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var out []ifacePrefix
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			addr, ok := netip.AddrFromSlice(ipn.IP)
			if !ok {
				continue
			}
			bits, _ := ipn.Mask.Size()
			out = append(out, ifacePrefix{name: ifc.Name, prefix: netip.PrefixFrom(addr.Unmap(), bits)})
		}
	}
	return out, nil
}

var (
	defaultMu       sync.RWMutex
	defaultResolver Resolver = platformResolver()
)

// DefaultResolver returns the resolver used when none is given.
func DefaultResolver() Resolver {
	defaultMu.RLock()
	defer defaultMu.RUnlock()
	return defaultResolver
}

// SetDefaultResolver replaces the process-wide default resolver, for tests or
// for a MAC source such as a switch's forwarding table. Nil restores the
// platform's.
func SetDefaultResolver(r Resolver) {
	defaultMu.Lock()
	defer defaultMu.Unlock()
	if r == nil {
		r = platformResolver()
	}
	defaultResolver = r
}

// gatewayFor finds the next hop for an off-link address; replaceable in tests.
var gatewayFor = defaultGateway
