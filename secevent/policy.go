package secevent

import (
	"fmt"
	"time"

	"github.com/dscsystems/go-dnp3"
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
}

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
	return nil
}

// Open returns the emitter a session uses under this policy, and a function
// to call when the session ends, which closes a dispatcher the policy did not
// supply. A nil policy, or one with neither sink nor dispatcher, yields a nil
// emitter: reporting is off.
func (p *Policy) Open(role string) (*Emitter, func()) {
	if p == nil || (p.Sink == nil && p.Dispatcher == nil) {
		return nil, func() {}
	}
	maxRaw := p.MaxRaw
	if maxRaw == 0 {
		maxRaw = DefaultMaxRaw
	}
	if p.Dispatcher != nil {
		return NewEmitter(p.Dispatcher, role, maxRaw), func() {}
	}
	d := NewDispatcher(p.Sink, DispatcherOptions{
		QueueSize: p.QueueSize, AggregateWindow: p.AggregateWindow, MaxAggregates: p.MaxAggregates,
	})
	return NewEmitter(d, role, maxRaw), func() { _ = d.Close() }
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
