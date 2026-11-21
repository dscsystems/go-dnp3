//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package netpeer

import (
	"syscall"

	"golang.org/x/net/route"
)

// The sysctl that dumps routes carrying given flags, and the flag marking
// link-layer (ARP/NDP) entries.
const (
	ribFlags  route.RIBType = syscall.NET_RT_FLAGS
	rtfLLInfo               = syscall.RTF_LLINFO
)
