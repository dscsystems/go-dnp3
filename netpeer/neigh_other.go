//go:build !linux && !windows && !darwin && !dragonfly && !freebsd && !netbsd && !openbsd

package netpeer

import (
	"context"
	"net/netip"
)

type unsupportedResolver struct{}

func platformResolver() Resolver { return unsupportedResolver{} }

func loopbackName() string { return "lo0" }

func (unsupportedResolver) Lookup(context.Context, netip.Addr) (Neighbor, error) {
	return Neighbor{}, ErrUnsupported
}

func defaultGateway(netip.Addr) (netip.Addr, string, bool) { return netip.Addr{}, "", false }
