package secevent

import (
	"fmt"
	"net"
	"net/netip"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/netpeer"
)

// DefaultMaxRaw is how many octets of an offending frame an event keeps.
const DefaultMaxRaw = 64

// Policy configures security event reporting for a session.
//
// A nil *Policy turns reporting off. The zero Policy is valid and reports
// nothing, since it has no sink; [DefaultPolicy] is the zero Policy with the
// defaults spelled out. Everything a Policy turns on is reporting: it never
// changes what goes on the wire unless a field says it does.
type Policy struct {
	// Sink receives the events. It is ignored when Dispatcher is set.
	Sink Sink
	// Dispatcher shares one dispatcher between sessions, so events from a
	// whole process arrive in one stream. The session does not close it.
	// Without it, each session runs its own, closed when Run returns.
	Dispatcher *Dispatcher

	// QueueSize, AggregateWindow and MaxAggregates tune the session's own
	// dispatcher; see [DispatcherOptions].
	QueueSize       int
	AggregateWindow time.Duration
	MaxAggregates   int

	// MaxRaw bounds the octets of an offending frame kept on an event. Zero
	// uses DefaultMaxRaw; negative keeps none.
	MaxRaw int

	// DisableAudit turns off the audit-trail events: restarts, controls,
	// time writes and the other state-changing operations reported even when
	// they succeed. Errors and attacks are still reported.
	DisableAudit bool

	// DisableMACLookup stops the session looking up the peer's MAC address
	// when a connection opens. Lookup is on by default: it costs one read of
	// the kernel neighbour table per connection and never delays or refuses
	// a connection unless PeerBindings says to.
	DisableMACLookup bool
	// Resolver replaces the platform's neighbour table, for instance with a
	// switch's forwarding table. Nil uses netpeer.DefaultResolver.
	Resolver netpeer.Resolver
	// MACCache remembers each IP's MAC, link address and certificate, so a
	// change is noticed. Nil uses netpeer.DefaultCache, shared process-wide.
	MACCache *netpeer.Cache
	// MACLookupTimeout bounds the lookup. Zero uses 200ms.
	MACLookupTimeout time.Duration
	// MACRecheckInterval looks the MAC up again this often while a
	// connection is open, to catch a change behind a live session. Zero, the
	// default, does not.
	MACRecheckInterval time.Duration
	// DisconnectOnMACChange closes a connection whose peer's MAC changed
	// while it was open. It needs MACRecheckInterval.
	DisconnectOnMACChange bool

	// PeerBindings pins IP addresses to MAC addresses. A connection from a
	// pinned IP with another MAC, or none, is reported.
	PeerBindings map[netip.Addr]net.HardwareAddr
	// RejectBindingMismatch closes such a connection as soon as it opens.
	RejectBindingMismatch bool

	// AllowedPeers lists the addresses a server channel expects connections
	// from. A connection from anywhere else is reported. Nil allows any.
	AllowedPeers []netip.Prefix
	// RejectUnlisted closes such a connection at once, before any TLS
	// handshake and before a session sees it.
	RejectUnlisted bool
	// AcceptRate bounds new connections from one IP address, and
	// GlobalAcceptRate new connections from everywhere. Going over is
	// reported as a connection storm or scan. Zero uses the defaults (10 per
	// 10s per IP, 50 per 10s overall); a negative N turns the check off.
	AcceptRate, GlobalAcceptRate Rate
	// RejectOverRate closes connections over the rate instead of only
	// reporting them.
	RejectOverRate bool
	// CertExpiryWarning reports a certificate, the peer's or our own, that
	// expires within this long. Zero uses 30 days; negative turns it off.
	CertExpiryWarning time.Duration

	// LinkTopology says whether the link is shared with other stations,
	// which decides whether a frame for another station is worth reporting.
	// The default takes it from the transport: serial is a shared bus,
	// TCP, TLS and UDP are point to point.
	LinkTopology LinkTopology
	// AllowedMasters lists the link addresses an outstation serves. A
	// primary frame from any other source is reported as
	// link.unexpected_master. Nil, the default, checks nothing: the
	// outstation answers whichever master addresses it.
	AllowedMasters []uint16
	// RejectUnexpectedMaster drops such a frame before the link layer acts
	// on it, so it can neither be answered nor reset the link state.
	RejectUnexpectedMaster bool

	// Functions refuses application functions; see [FunctionPolicy]. Nil
	// refuses nothing.
	Functions *FunctionPolicy
	// BroadcastAllowed lists the function codes a broadcast may carry
	// without access.broadcast_misuse. Nil uses DefaultBroadcastAllowed.
	// READ and CONFIRM are always allowed; WRITE only of time (group 50)
	// and the restart indication (group 80).
	BroadcastAllowed []uint8
	// RejectBroadcastMisuse drops such a broadcast instead of executing it.
	RejectBroadcastMisuse bool
	// ScanThreshold and ScanWindow raise access.scan when one connection
	// asks for this many distinct unknown functions, objects or points
	// within the window. Zero uses 20 in a minute; a negative threshold
	// turns it off.
	ScanThreshold int
	ScanWindow    time.Duration

	// CommandGuard, when set, is asked about every control before the
	// command handler. A status other than SUCCESS refuses it with that
	// status and reports access.function_blocked.
	CommandGuard CommandGuard
	// DuplicateWindow is how long a state-changing request is remembered,
	// so the same octets arriving on another connection are reported as
	// cmd.duplicate — a replay. Zero uses 10 minutes; negative turns it
	// off. Not checked under SAv5, whose challenges catch replays.
	DuplicateWindow time.Duration
	// ClockStepThreshold is the clock change a time write may make before
	// it is reported as ctrl.clock_step. Zero uses a minute; negative turns
	// it off.
	ClockStepThreshold time.Duration

	// AuthFailureRate is how many SAv5 authentication or key change
	// failures one connection may cause before auth.brute_force. Zero uses
	// 5 a minute; a negative N turns it off.
	AuthFailureRate Rate

	// Limits are the flood thresholds; zero rates use DefaultLimits.
	Limits Limits
	// FloodAction is what happens to traffic beyond a limit: Observed
	// (report only, the default), Throttled — a throttled control is
	// answered ThrottleStatus, any other request PARAMETER_ERROR, and
	// neither is executed — or Disconnected. Link status requests,
	// confirmations, link resets and authentication are only ever reported.
	FloodAction Action
	// ThrottleStatus answers a throttled control. Zero uses TOO_MANY_OPS.
	ThrottleStatus dnp3.CommandStatus
	// ConfirmWithheldAfter is how many confirmations in a row the master
	// may let time out before dos.confirm_withheld. Zero uses 3; negative
	// turns it off.
	ConfirmWithheldAfter int

	// MaxFileSize reports a file opened for writing with a larger announced
	// size as file.oversized. Zero does not check.
	MaxFileSize int64
}

// CommandGuard decides whether a control may reach the command handler:
// fc is the application function, group 12 for a CROB (code is its control
// code) or 41 for an analog output (code is zero).
type CommandGuard func(peer PeerInfo, fc uint8, group uint8, index uint16, code dnp3.ControlCode) dnp3.CommandStatus

// Defaults for the duplicate window and the clock step threshold.
const (
	DefaultDuplicateWindow    = 10 * time.Minute
	DefaultClockStepThreshold = time.Minute
)

// Duplicates returns the effective duplicate window, zero when off.
func (p *Policy) Duplicates() time.Duration {
	switch {
	case p == nil || p.DuplicateWindow < 0:
		return 0
	case p.DuplicateWindow == 0:
		return DefaultDuplicateWindow
	}
	return p.DuplicateWindow
}

// ClockStep returns the effective clock step threshold, zero when off.
func (p *Policy) ClockStep() time.Duration {
	switch {
	case p == nil || p.ClockStepThreshold < 0:
		return 0
	case p.ClockStepThreshold == 0:
		return DefaultClockStepThreshold
	}
	return p.ClockStepThreshold
}

// LinkTopology says whether a link is shared with other stations.
type LinkTopology uint8

// Link topologies.
const (
	// TopologyAuto: serial is a shared bus, everything else point to point.
	TopologyAuto LinkTopology = iota
	// PointToPoint: only our peer and we are on the link, so a frame for
	// another station is unusual.
	PointToPoint
	// SharedBus: other stations share the link, so frames for them are
	// ordinary.
	SharedBus
)

// SharedLink reports whether a link over transport ("serial", "tcp", ...)
// carries other stations' traffic under this policy.
func (p *Policy) SharedLink(transport string) bool {
	if p != nil {
		switch p.LinkTopology {
		case PointToPoint:
			return false
		case SharedBus:
			return true
		}
	}
	return transport == "serial" || transport == "multidrop"
}

// MasterAllowed reports whether AllowedMasters admits a link source.
func (p *Policy) MasterAllowed(src uint16) bool {
	if p == nil || p.AllowedMasters == nil {
		return true
	}
	for _, a := range p.AllowedMasters {
		if a == src {
			return true
		}
	}
	return false
}

// Rate is N events per period.
type Rate struct {
	N   int
	Per time.Duration
}

// Defaults for the policy's rates and durations.
var (
	DefaultAcceptRate       = Rate{N: 10, Per: 10 * time.Second}
	DefaultGlobalAcceptRate = Rate{N: 50, Per: 10 * time.Second}
	DefaultAuthFailureRate  = Rate{N: 5, Per: time.Minute}
)

// DefaultCertExpiryWarning is how far ahead a certificate's expiry is reported.
const DefaultCertExpiryWarning = 30 * 24 * time.Hour

// Or returns r, or def when r is the zero Rate. A negative N means off and is
// returned as is.
func (r Rate) Or(def Rate) Rate {
	if r == (Rate{}) {
		return def
	}
	return r
}

// Off reports whether the rate is turned off.
func (r Rate) Off() bool { return r.N < 0 || r.Per <= 0 }

// CertExpiry returns the effective certificate expiry warning, zero when off.
func (p *Policy) CertExpiry() time.Duration {
	switch {
	case p == nil || p.CertExpiryWarning < 0:
		return 0
	case p.CertExpiryWarning == 0:
		return DefaultCertExpiryWarning
	}
	return p.CertExpiryWarning
}

// PeerAllowed reports whether AllowedPeers admits ip.
func (p *Policy) PeerAllowed(ip netip.Addr) bool {
	if p == nil || p.AllowedPeers == nil {
		return true
	}
	ip = ip.Unmap()
	for _, pre := range p.AllowedPeers {
		if pre.Contains(ip) {
			return true
		}
	}
	return false
}

// DefaultMACLookupTimeout bounds a MAC lookup when the policy does not.
const DefaultMACLookupTimeout = 200 * time.Millisecond

// DefaultPolicy returns a report-only policy delivering to sink.
func DefaultPolicy(sink Sink) *Policy {
	return &Policy{
		Sink:            sink,
		QueueSize:       DefaultQueueSize,
		AggregateWindow: DefaultAggregateWindow,
		MaxAggregates:   DefaultMaxAggregates,
		MaxRaw:          DefaultMaxRaw,
	}
}

// Valid reports whether the policy can be used.
func (p *Policy) Valid() error {
	if p == nil {
		return nil
	}
	if p.QueueSize < 0 || p.MaxAggregates < 0 {
		return fmt.Errorf("%w: secevent: negative queue or aggregate bound", dnp3.ErrBadConfig)
	}
	if p.DisconnectOnMACChange && p.MACRecheckInterval <= 0 {
		return fmt.Errorf("%w: secevent: DisconnectOnMACChange needs MACRecheckInterval", dnp3.ErrBadConfig)
	}
	for _, pre := range p.AllowedPeers {
		if !pre.IsValid() {
			return fmt.Errorf("%w: secevent: AllowedPeers holds an invalid prefix", dnp3.ErrBadConfig)
		}
	}
	if err := p.validFlood(); err != nil {
		return err
	}
	if p.LinkTopology > SharedBus {
		return fmt.Errorf("%w: secevent: unknown LinkTopology %d", dnp3.ErrBadConfig, p.LinkTopology)
	}
	if p.RejectUnexpectedMaster && p.AllowedMasters == nil {
		return fmt.Errorf("%w: secevent: RejectUnexpectedMaster needs AllowedMasters", dnp3.ErrBadConfig)
	}
	for ip, mac := range p.PeerBindings {
		if !ip.IsValid() || len(mac) == 0 {
			return fmt.Errorf("%w: secevent: PeerBindings entry %v=%v is incomplete", dnp3.ErrBadConfig, ip, mac)
		}
	}
	return nil
}

// Open returns the emitter a session uses under this policy, and a function
// to call when the session ends, which closes a dispatcher the policy did not
// supply. A nil policy yields a nil emitter: reporting is off. So does one
// with neither sink nor dispatcher, unless it refuses something — then the
// events are built and discarded, so the refusals still happen.
func (p *Policy) Open(role string) (*Emitter, func()) {
	if p == nil || (p.Sink == nil && p.Dispatcher == nil && !p.enforces()) {
		return nil, func() {}
	}
	maxRaw := p.MaxRaw
	if maxRaw == 0 {
		maxRaw = DefaultMaxRaw
	}
	if p.Dispatcher != nil {
		em := NewEmitter(p.Dispatcher, role, maxRaw)
		em.policy = p
		return em, func() {}
	}
	sink := p.Sink
	if sink == nil {
		sink = SinkFunc(func(Event) {})
	}
	d := NewDispatcher(sink, DispatcherOptions{
		QueueSize: p.QueueSize, AggregateWindow: p.AggregateWindow, MaxAggregates: p.MaxAggregates,
	})
	em := NewEmitter(d, role, maxRaw)
	em.policy = p
	return em, func() { _ = d.Close() }
}

// enforces reports whether the policy refuses anything, as opposed to only
// reporting it.
func (p *Policy) enforces() bool {
	return p.RejectUnlisted || p.RejectOverRate || p.RejectBindingMismatch ||
		p.DisconnectOnMACChange || p.RejectUnexpectedMaster || p.RejectBroadcastMisuse ||
		p.Functions != nil || p.CommandGuard != nil || p.FloodAction == Throttled ||
		p.FloodAction == Disconnected
}

// Audit reports whether audit-trail events are wanted.
func (p *Policy) Audit() bool { return p != nil && !p.DisableAudit }

// Attachable is implemented by channels that report connection-level events
// — accept errors, refused connections, TLS failures — which happen before
// any session sees a connection. A session attaches its emitter when it
// starts running a channel.
type Attachable interface {
	AttachSecurity(em *Emitter)
}
