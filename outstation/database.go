// Package outstation implements the DNP3 outstation role: the device that
// holds measurements, answers a master's polls, and executes its commands.
package outstation

import (
	"bytes"
	"sync"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/objects"
)

// PointConfig describes how one point is reported.
type PointConfig struct {
	// Class is the event class the point's events are assigned to.
	// [dnp3.ClassNone] suppresses events for the point entirely.
	Class dnp3.Class
	// StaticVariation is the variation used when the point is reported in
	// response to a class 0 or range read.
	StaticVariation uint8
	// EventVariation is the variation used when the point's events are
	// reported.
	EventVariation uint8
	// Deadband is how far an analog or counter must move before it generates
	// an event. It is ignored for binary types, which event on any change.
	Deadband float64
}

// DatabaseConfig sizes the database and sets the defaults every point starts
// with.
type DatabaseConfig struct {
	Binary             int
	DoubleBitBinary    int
	Counter            int
	FrozenCounter      int
	Analog             int
	BinaryOutputStatus int
	AnalogOutputStatus int
	OctetString        int

	// Defaults applied to every point at construction. A caller changes
	// individual points afterwards with [Database.Configure].
	DefaultClass dnp3.Class
}

// point pairs a value with its configuration and the last value reported, so
// deadbands are measured against what the master was actually told.
type point[T any] struct {
	value    T
	cfg      PointConfig
	reported float64
	hasEvent bool
}

// Database holds an outstation's measurements.
//
// It is not safe for concurrent use on its own. Access goes through
// [Session.Update] and the session's own goroutine, which serialises it.
type Database struct {
	binary    []point[dnp3.Binary]
	doubleBit []point[dnp3.DoubleBitBinary]
	counter   []point[dnp3.Counter]
	frozen    []point[dnp3.FrozenCounter]
	analog    []point[dnp3.Analog]
	binaryOut []point[dnp3.BinaryOutputStatus]
	analogOut []point[dnp3.AnalogOutputStatus]
	octet     []point[dnp3.OctetString]

	events *EventBuffer

	// mu guards reads taken outside the session goroutine, such as a
	// diagnostic snapshot.
	mu sync.RWMutex
}

// defaultStaticVariations are the variations used when a point's
// configuration does not name one. They are the widest lossless encoding for
// each type, which is the safe default: a narrower one silently truncates.
var defaultStaticVariations = map[dnp3.PointType]uint8{
	dnp3.TypeBinary:             2, // g1v2, with flags
	dnp3.TypeDoubleBitBinary:    2, // g3v2
	dnp3.TypeCounter:            1, // g20v1, 32-bit with flags
	dnp3.TypeFrozenCounter:      1, // g21v1
	dnp3.TypeAnalog:             1, // g30v1, 32-bit with flags
	dnp3.TypeBinaryOutputStatus: 2, // g10v2
	dnp3.TypeAnalogOutputStatus: 1, // g40v1
}

var defaultEventVariations = map[dnp3.PointType]uint8{
	dnp3.TypeBinary:             2, // g2v2, with absolute time
	dnp3.TypeDoubleBitBinary:    2, // g4v2
	dnp3.TypeCounter:            5, // g22v5, with time
	dnp3.TypeFrozenCounter:      5, // g23v5
	dnp3.TypeAnalog:             3, // g32v3, 32-bit with time
	dnp3.TypeBinaryOutputStatus: 2, // g11v2
	dnp3.TypeAnalogOutputStatus: 3, // g42v3
}

// NewDatabase builds a database from a configuration.
func NewDatabase(cfg DatabaseConfig, events *EventBuffer) *Database {
	db := &Database{events: events}

	db.binary = makePoints[dnp3.Binary](cfg.Binary, dnp3.TypeBinary, cfg.DefaultClass)
	db.doubleBit = makePoints[dnp3.DoubleBitBinary](cfg.DoubleBitBinary, dnp3.TypeDoubleBitBinary, cfg.DefaultClass)
	db.counter = makePoints[dnp3.Counter](cfg.Counter, dnp3.TypeCounter, cfg.DefaultClass)
	db.frozen = makePoints[dnp3.FrozenCounter](cfg.FrozenCounter, dnp3.TypeFrozenCounter, cfg.DefaultClass)
	db.analog = makePoints[dnp3.Analog](cfg.Analog, dnp3.TypeAnalog, cfg.DefaultClass)
	db.binaryOut = makePoints[dnp3.BinaryOutputStatus](cfg.BinaryOutputStatus, dnp3.TypeBinaryOutputStatus, cfg.DefaultClass)
	db.analogOut = makePoints[dnp3.AnalogOutputStatus](cfg.AnalogOutputStatus, dnp3.TypeAnalogOutputStatus, cfg.DefaultClass)
	db.octet = makePoints[dnp3.OctetString](cfg.OctetString, dnp3.TypeOctetString, cfg.DefaultClass)

	return db
}

func makePoints[T any](n int, pt dnp3.PointType, class dnp3.Class) []point[T] {
	pts := make([]point[T], n)
	for i := range pts {
		pts[i].cfg = PointConfig{
			Class:           class,
			StaticVariation: defaultStaticVariations[pt],
			EventVariation:  defaultEventVariations[pt],
		}
	}
	return pts
}

// Counts returns how many points of each type the database holds.
func (db *Database) Counts() DatabaseConfig {
	return DatabaseConfig{
		Binary:             len(db.binary),
		DoubleBitBinary:    len(db.doubleBit),
		Counter:            len(db.counter),
		FrozenCounter:      len(db.frozen),
		Analog:             len(db.analog),
		BinaryOutputStatus: len(db.binaryOut),
		AnalogOutputStatus: len(db.analogOut),
		OctetString:        len(db.octet),
	}
}

// Configure sets the reporting configuration for one point. It is a no-op for
// an index the database does not have, so a configuration file listing a point
// that was removed does not panic the outstation at startup.
func (db *Database) Configure(pt dnp3.PointType, index uint16, cfg PointConfig) bool {
	db.mu.Lock()
	defer db.mu.Unlock()

	i := int(index)
	switch pt {
	case dnp3.TypeBinary:
		return setConfig(db.binary, i, cfg)
	case dnp3.TypeDoubleBitBinary:
		return setConfig(db.doubleBit, i, cfg)
	case dnp3.TypeCounter:
		return setConfig(db.counter, i, cfg)
	case dnp3.TypeFrozenCounter:
		return setConfig(db.frozen, i, cfg)
	case dnp3.TypeAnalog:
		return setConfig(db.analog, i, cfg)
	case dnp3.TypeBinaryOutputStatus:
		return setConfig(db.binaryOut, i, cfg)
	case dnp3.TypeAnalogOutputStatus:
		return setConfig(db.analogOut, i, cfg)
	case dnp3.TypeOctetString:
		return setConfig(db.octet, i, cfg)
	}
	return false
}

func setConfig[T any](pts []point[T], i int, cfg PointConfig) bool {
	if i < 0 || i >= len(pts) {
		return false
	}
	if cfg.StaticVariation == 0 {
		cfg.StaticVariation = pts[i].cfg.StaticVariation
	}
	if cfg.EventVariation == 0 {
		cfg.EventVariation = pts[i].cfg.EventVariation
	}
	pts[i].cfg = cfg
	return true
}

// AssignClass sets the event class of every point of a type, which is what the
// ASSIGN_CLASS function code does.
func (db *Database) AssignClass(pt dnp3.PointType, class dnp3.Class) {
	db.mu.Lock()
	defer db.mu.Unlock()

	switch pt {
	case dnp3.TypeBinary:
		assignClass(db.binary, class)
	case dnp3.TypeDoubleBitBinary:
		assignClass(db.doubleBit, class)
	case dnp3.TypeCounter:
		assignClass(db.counter, class)
	case dnp3.TypeFrozenCounter:
		assignClass(db.frozen, class)
	case dnp3.TypeAnalog:
		assignClass(db.analog, class)
	case dnp3.TypeBinaryOutputStatus:
		assignClass(db.binaryOut, class)
	case dnp3.TypeAnalogOutputStatus:
		assignClass(db.analogOut, class)
	case dnp3.TypeOctetString:
		assignClass(db.octet, class)
	}
}

func assignClass[T any](pts []point[T], class dnp3.Class) {
	for i := range pts {
		pts[i].cfg.Class = class
	}
}

// ---------- Updates ----------

// UpdateBinary sets a binary input, generating an event if the value or its
// quality changed and the point is assigned to an event class.
func (db *Database) UpdateBinary(index uint16, v dnp3.Binary) {
	db.mu.Lock()
	defer db.mu.Unlock()

	i := int(index)
	if i >= len(db.binary) {
		return
	}
	p := &db.binary[i]
	changed := p.value.Value != v.Value || p.value.Flags != v.Flags
	p.value = v
	if changed {
		db.raise(p.cfg, Event{
			Type: dnp3.TypeBinary, Index: index,
			Variation: p.cfg.EventVariation, Binary: v, Time: v.Time,
		})
	}
}

// UpdateDoubleBit sets a double-bit binary input.
func (db *Database) UpdateDoubleBit(index uint16, v dnp3.DoubleBitBinary) {
	db.mu.Lock()
	defer db.mu.Unlock()

	i := int(index)
	if i >= len(db.doubleBit) {
		return
	}
	p := &db.doubleBit[i]
	changed := p.value.Value != v.Value || p.value.Flags != v.Flags
	p.value = v
	if changed {
		db.raise(p.cfg, Event{
			Type: dnp3.TypeDoubleBitBinary, Index: index,
			Variation: p.cfg.EventVariation, DoubleBit: v, Time: v.Time,
		})
	}
}

// UpdateBinaryOutputStatus sets a binary output's reported state.
func (db *Database) UpdateBinaryOutputStatus(index uint16, v dnp3.BinaryOutputStatus) {
	db.mu.Lock()
	defer db.mu.Unlock()

	i := int(index)
	if i >= len(db.binaryOut) {
		return
	}
	p := &db.binaryOut[i]
	changed := p.value.Value != v.Value || p.value.Flags != v.Flags
	p.value = v
	if changed {
		db.raise(p.cfg, Event{
			Type: dnp3.TypeBinaryOutputStatus, Index: index,
			Variation: p.cfg.EventVariation, BinaryOutput: v, Time: v.Time,
		})
	}
}

// UpdateCounter sets a counter.
func (db *Database) UpdateCounter(index uint16, v dnp3.Counter) {
	db.mu.Lock()
	defer db.mu.Unlock()

	i := int(index)
	if i >= len(db.counter) {
		return
	}
	p := &db.counter[i]
	flagsChanged := p.value.Flags != v.Flags
	valueChanged := p.value.Value != v.Value
	p.value = v
	if (flagsChanged || valueChanged) && p.shouldReport(float64(v.Value), flagsChanged) {
		db.raise(p.cfg, Event{
			Type: dnp3.TypeCounter, Index: index,
			Variation: p.cfg.EventVariation, Counter: v, Time: v.Time,
		})
	}
}

// UpdateFrozenCounter sets a frozen counter.
func (db *Database) UpdateFrozenCounter(index uint16, v dnp3.FrozenCounter) {
	db.mu.Lock()
	defer db.mu.Unlock()

	i := int(index)
	if i >= len(db.frozen) {
		return
	}
	p := &db.frozen[i]
	changed := p.value.Value != v.Value || p.value.Flags != v.Flags
	p.value = v
	if changed {
		db.raise(p.cfg, Event{
			Type: dnp3.TypeFrozenCounter, Index: index,
			Variation: p.cfg.EventVariation, FrozenCounter: v, Time: v.Time,
		})
	}
}

// UpdateAnalog sets an analog input.
//
// An event is generated when the value moves further than the deadband from
// the value last *reported*, not from the value last stored. Comparing against
// the stored value lets a point drift indefinitely in deadband-sized steps
// without ever reporting — the classic implementation bug, and one that hides
// a slow ramp toward a limit.
func (db *Database) UpdateAnalog(index uint16, v dnp3.Analog) {
	db.mu.Lock()
	defer db.mu.Unlock()

	i := int(index)
	if i >= len(db.analog) {
		return
	}
	p := &db.analog[i]
	flagsChanged := p.value.Flags != v.Flags
	p.value = v

	if p.shouldReport(v.Value, flagsChanged) {
		db.raise(p.cfg, Event{
			Type: dnp3.TypeAnalog, Index: index,
			Variation: p.cfg.EventVariation, Analog: v, Time: v.Time,
		})
	}
}

// UpdateAnalogOutputStatus sets an analog output's reported value.
func (db *Database) UpdateAnalogOutputStatus(index uint16, v dnp3.AnalogOutputStatus) {
	db.mu.Lock()
	defer db.mu.Unlock()

	i := int(index)
	if i >= len(db.analogOut) {
		return
	}
	p := &db.analogOut[i]
	flagsChanged := p.value.Flags != v.Flags
	p.value = v

	if p.shouldReport(v.Value, flagsChanged) {
		db.raise(p.cfg, Event{
			Type: dnp3.TypeAnalogOutputStatus, Index: index,
			Variation: p.cfg.EventVariation, AnalogOutput: v, Time: v.Time,
		})
	}
}

// shouldReport decides whether an update warrants an event, and records that
// the point has now been reported at least once.
//
// The first-update latch is set unconditionally rather than inside the
// deadband branch. Folding it into a `flagsChanged || exceedsDeadband(...)`
// expression looks equivalent and is not: Go short-circuits, so an update that
// changed the flags never reaches the deadband check, never sets the latch,
// and the *next* update then reports as if it were the first — firing an event
// no matter how small the move. That is a deadband that silently does nothing
// on every other update.
func (p *point[T]) shouldReport(v float64, flagsChanged bool) bool {
	first := !p.hasEvent
	p.hasEvent = true

	if first || flagsChanged {
		p.reported = v
		return true
	}

	delta := v - p.reported
	if delta < 0 {
		delta = -delta
	}
	if delta > p.cfg.Deadband {
		p.reported = v
		return true
	}
	return false
}

// raise queues an event if the point is assigned to an event class.
func (db *Database) raise(cfg PointConfig, e Event) {
	if db.events == nil || cfg.Class == dnp3.ClassNone {
		return
	}
	e.Class = cfg.Class
	db.events.Add(e)
}

// UpdateOctetString sets an octet string point.
//
// Octet strings are how a device reports the things that are text rather than
// measurements: a point name, a firmware version, a serial number. Their
// encoding is unusual — the variation number *is* the length — so changing the
// string's length changes the variation the outstation reports it in, which is
// legal and which masters must cope with.
func (db *Database) UpdateOctetString(index uint16, v dnp3.OctetString) {
	db.mu.Lock()
	defer db.mu.Unlock()

	i := int(index)
	if i >= len(db.octet) {
		return
	}
	if len(v) > dnp3.MaxOctetStringLen {
		// The length has to fit a variation number. Truncating loses data, but
		// refusing silently would lose all of it.
		v = v[:dnp3.MaxOctetStringLen]
	}

	p := &db.octet[i]
	changed := !bytes.Equal(p.value, v)
	p.value = append(p.value[:0], v...)
	if changed {
		db.raise(p.cfg, Event{
			Type: dnp3.TypeOctetString, Index: index,
			Variation: uint8(len(v)), OctetString: p.value,
		})
	}
}

// ---------- Reads ----------

// Binary returns a binary input and whether the index exists.
func (db *Database) Binary(index uint16) (dnp3.Binary, PointConfig, bool) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return get(db.binary, index)
}

// DoubleBit returns a double-bit binary input.
func (db *Database) DoubleBit(index uint16) (dnp3.DoubleBitBinary, PointConfig, bool) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return get(db.doubleBit, index)
}

// Counter returns a counter.
func (db *Database) Counter(index uint16) (dnp3.Counter, PointConfig, bool) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return get(db.counter, index)
}

// FrozenCounter returns a frozen counter.
func (db *Database) FrozenCounter(index uint16) (dnp3.FrozenCounter, PointConfig, bool) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return get(db.frozen, index)
}

// Analog returns an analog input.
func (db *Database) Analog(index uint16) (dnp3.Analog, PointConfig, bool) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return get(db.analog, index)
}

// BinaryOutputStatus returns a binary output's state.
func (db *Database) BinaryOutputStatus(index uint16) (dnp3.BinaryOutputStatus, PointConfig, bool) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return get(db.binaryOut, index)
}

// AnalogOutputStatus returns an analog output's value.
func (db *Database) AnalogOutputStatus(index uint16) (dnp3.AnalogOutputStatus, PointConfig, bool) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return get(db.analogOut, index)
}

func get[T any](pts []point[T], index uint16) (T, PointConfig, bool) {
	var zero T
	i := int(index)
	if i < 0 || i >= len(pts) {
		return zero, PointConfig{}, false
	}
	return pts[i].value, pts[i].cfg, true
}

// OctetString returns an octet string point.
func (db *Database) OctetString(index uint16) (dnp3.OctetString, PointConfig, bool) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return get(db.octet, index)
}

// FreezeCounters copies every counter into its frozen counterpart, which is
// what the freeze function codes do.
func (db *Database) FreezeCounters() {
	db.mu.Lock()
	defer db.mu.Unlock()

	n := min(len(db.counter), len(db.frozen))
	for i := range n {
		c := db.counter[i].value
		db.frozen[i].value = dnp3.FrozenCounter{
			Value: c.Value,
			Flags: c.Flags,
			Time:  c.Time,
		}
	}
}

// staticGroupVar returns the group and variation a point is reported with for
// a static read.
func staticGroupVar(pt dnp3.PointType, variation uint8) objects.GroupVar {
	var group uint8
	switch pt {
	case dnp3.TypeBinary:
		group = 1
	case dnp3.TypeDoubleBitBinary:
		group = 3
	case dnp3.TypeCounter:
		group = 20
	case dnp3.TypeFrozenCounter:
		group = 21
	case dnp3.TypeAnalog:
		group = 30
	case dnp3.TypeBinaryOutputStatus:
		group = 10
	case dnp3.TypeAnalogOutputStatus:
		group = 40
	case dnp3.TypeOctetString:
		group = 110
	}
	return objects.GV(group, variation)
}
