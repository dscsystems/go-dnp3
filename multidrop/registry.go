package multidrop

import (
	"fmt"
	"sync"

	"github.com/dscsystems/go-dnp3/channel"
)

// A [Bus] already solves one channel being opened twice by one part of a
// program that knows to share it. It does nothing for two parts that do not
// know about each other — a master built in one package, an outstation
// simulator built in another, both configured to talk to "/dev/ttyUSB0" — and
// each independently doing the right thing, by itself, produces the exact
// failure multidrop exists to prevent: two Buses, two calls to Connect, and
// the OS refusing the second.
//
// A [Registry] is where independent callers meet. Each asks for a bus by the
// channel it would open; the first caller for a given channel builds one, and
// every later caller for an equivalent channel gets that same bus back
// instead of building a second one.

// Registry shares buses across callers that ask for the same physical
// channel.
//
// The zero value is not usable; construct one with [NewRegistry]. A program
// with several independent components that might reach the same device keeps
// one Registry — typically one for the whole process — and has each ask it
// for a bus rather than constructing one directly.
type Registry struct {
	mu      sync.Mutex
	entries map[string]*registryEntry
	// byBus finds the entry for a Release without a linear scan, and is why
	// Bus is not its own map key: two buses are never == by construction, but
	// looking one up by the pointer a caller hands back is still a second
	// index, not a property of the first.
	byBus map[*Bus]string
}

// registryEntry is one shared bus and how many callers are using it.
type registryEntry struct {
	bus  *Bus
	ch   channel.Channel
	refs int
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		entries: map[string]*registryEntry{},
		byBus:   map[*Bus]string{},
	}
}

// Open returns the bus for ch, reusing an existing one if this registry
// already has one for an equivalent channel.
//
// Equivalence is [channel.Channel.String]: two channels built the same way —
// the same serial device at the same baud, the same host and port — describe
// themselves identically, which is what lets two callers that have never met
// recognise they mean the same line. Two channels that merely reach the same
// device through different settings (one with retry, one without) are not
// equivalent by this test and get separate buses; String does not describe
// retry policy, only identity, so that is deliberate — arbitration timing and
// queue depth belong to whoever opens the bus first regardless, and giving
// every caller its own bus when the physical identity does differ is the
// existing, correct behaviour.
//
// The bus is built, and cfg takes effect, only on the first Open for a given
// channel; cfg on every later call for the same channel is ignored, since the
// bus already exists and is not rebuilt under an active caller. If ch is not
// the one that ends up owning the bus — because an equivalent one already
// existed — ch is closed before Open returns, since nothing will ever call
// Connect on it and an unclosed one would otherwise sit there implying it is
// in use.
//
// Every Open must be matched by exactly one [Registry.Release]. The bus, and
// the channel beneath it, stays open until the last caller that opened it
// releases it — not the first, since another caller may still depend on it.
func (r *Registry) Open(ch channel.Channel, cfg Config) *Bus {
	key := ch.String()

	r.mu.Lock()
	defer r.mu.Unlock()

	if e, ok := r.entries[key]; ok {
		e.refs++
		if ch != e.ch {
			// A distinct object describing the same channel: this caller
			// built its own before asking, not knowing one already existed.
			// Closing it is safe before it is ever connected, and leaving it
			// open would misrepresent it as live.
			_ = ch.Close()
		}
		return e.bus
	}

	b := New(ch, cfg)
	r.entries[key] = &registryEntry{bus: b, ch: ch, refs: 1}
	r.byBus[b] = key
	return b
}

// Release gives up one reference to b, which must have come from
// [Registry.Open] on this registry.
//
// The bus is closed once every caller that opened it has released it. Until
// then Release only counts down: closing on an early release would drop the
// line out from under callers still using it, which is the whole failure this
// type exists to prevent — the same mistake one level up.
//
// Once a bus was obtained through a Registry, release it through the
// Registry rather than calling [Bus.Close] directly. Bus.Close does not know
// about other callers and would close the shared channel regardless of how
// many are still using it; the registry's bookkeeping would then disagree
// with the bus's own state, and a later Open for the same channel would
// return a bus that is already closed.
func (r *Registry) Release(b *Bus) error {
	r.mu.Lock()

	key, ok := r.byBus[b]
	if !ok {
		r.mu.Unlock()
		return fmt.Errorf("multidrop: release of a bus this registry did not open")
	}

	e := r.entries[key]
	e.refs--
	if e.refs > 0 {
		r.mu.Unlock()
		return nil
	}
	delete(r.entries, key)
	delete(r.byBus, b)
	r.mu.Unlock()

	return b.Close()
}

// Len returns how many distinct buses the registry currently holds open, for
// diagnostics and tests.
func (r *Registry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.entries)
}
