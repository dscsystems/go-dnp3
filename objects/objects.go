// Package objects implements the DNP3 object group and variation codecs.
//
// Most of this package is generated from objects/spec/dnp3_objects.yaml, which
// is the single source of truth for every group, variation, size and field
// layout. Regenerate with `make generate`; the output is committed so
// consumers never run the generator.
//
// Hand-written code lives here for the encodings the table cannot express:
// bit-packed objects, whose objects share octets, and commands, whose fields
// map onto purpose-built structs rather than a measurement type.
package objects

import (
	"fmt"
	"time"

	"github.com/dscsystems/go-dnp3"
)

// The generator emits into this package and into internal/app, from the one
// spec, so the framing layer's size table and these codecs cannot disagree.
//go:generate go run ../internal/gen -root ..

// GroupVar identifies an object type: its group and its variation.
type GroupVar struct {
	Group     uint8
	Variation uint8
}

// GV is shorthand for constructing a GroupVar.
func GV(group, variation uint8) GroupVar { return GroupVar{group, variation} }

func (gv GroupVar) String() string { return fmt.Sprintf("g%dv%d", gv.Group, gv.Variation) }

// Key returns the packed form used as a map key on the wire side.
func (gv GroupVar) Key() uint16 { return uint16(gv.Group)<<8 | uint16(gv.Variation) }

// Kind classifies what an object is for, which is what lets a master decide
// whether a header names data, a command, or a class to poll.
type Kind uint8

// Object kinds.
const (
	KindUnknown Kind = iota
	KindStatic
	KindEvent
	KindCommand
	KindCommandEvent
	KindTime
	KindClass
	KindIndication
	KindDeadband
	KindString
	KindFile
	KindAttribute
)

var kindNames = [...]string{
	"unknown", "static", "event", "command", "command-event",
	"time", "class", "indication", "deadband", "string", "file", "attribute",
}

func (k Kind) String() string {
	if int(k) < len(kindNames) {
		return kindNames[k]
	}
	return "Kind(?)"
}

// Descriptor is everything known about one group and variation.
type Descriptor struct {
	GV          GroupVar
	Name        string
	Level       int
	Kind        Kind
	Measurement dnp3.PointType

	// SizeBits is the encoded size of one object. Values under eight mean the
	// objects are bit-packed and share octets across a range.
	SizeBits int
	Packed   bool

	HasFlags bool
	HasTime  bool
	// RelativeTime marks the variations whose timestamp is an offset from a
	// preceding group 51 common-time-of-occurrence object rather than an
	// absolute time.
	RelativeTime bool

	// ValueBits is the width of the object's value field, and FloatValue says
	// whether it is IEEE 754 rather than an integer.
	//
	// These are recorded rather than inferred from the variation number
	// because the mapping is not consistent across groups: variation 3 is a
	// 32-bit integer in group 30 and a single-precision float in group 40. An
	// outstation choosing which variation can carry a reading needs the real
	// answer, not a rule that happens to hold for one group.
	ValueBits  int
	FloatValue bool
}

// SizeOctets returns the object's size in whole octets, and whether it has
// one. Packed objects do not: they are measured per range, not per object.
func (d Descriptor) SizeOctets() (int, bool) {
	if d.Packed || d.SizeBits%8 != 0 {
		return 0, false
	}
	return d.SizeBits / 8, true
}

func (d Descriptor) String() string {
	return fmt.Sprintf("%s %s (%s, L%d)", d.GV, d.Name, d.Kind, d.Level)
}

// Lookup returns the descriptor for a group and variation.
func Lookup(gv GroupVar) (Descriptor, bool) {
	d, ok := descriptors[gv]
	return d, ok
}

// All returns every descriptor the spec defines, keyed by group and variation.
// The returned map must not be modified.
func All() map[GroupVar]Descriptor { return descriptors }

// Context carries what a decoder needs that is not in the object itself.
//
// Two things fall into that category, and both are properties of the session
// rather than of the octets: whether the outstation's clock was synchronised
// when it stamped an event, and the common time of occurrence that a
// relative-time event is measured from.
type Context struct {
	// Synchronized reports whether the outstation's clock was synchronised.
	// A master sets this false while the outstation is asserting NEED_TIME.
	Synchronized bool

	// CTO is the common time of occurrence from the most recent group 51
	// object in this fragment, and HasCTO says whether one was seen.
	CTO    time.Time
	HasCTO bool
}

// TimeQuality returns the quality to stamp on an absolute timestamp.
func (c Context) TimeQuality() dnp3.TimestampQuality {
	if c.Synchronized {
		return dnp3.TimestampSynchronized
	}
	return dnp3.TimestampUnsynchronized
}

// RelativeTime resolves a relative-time offset against the context's common
// time of occurrence.
//
// Without a CTO the offset means nothing, so the result is an invalid
// timestamp rather than one anchored to the epoch. Silently anchoring would
// file the event in 1970 and look like data rather than a missing base.
func (c Context) RelativeTime(offsetMillis uint16) dnp3.Timestamp {
	if !c.HasCTO {
		return dnp3.NoTime()
	}
	return dnp3.Timestamp{
		Time:    c.CTO.Add(time.Duration(offsetMillis) * time.Millisecond),
		Quality: c.TimeQuality(),
	}
}

// RelativeOffset returns the millisecond offset to encode for a relative-time
// event, measured from the context's common time of occurrence.
//
// An event before the CTO, or more than 65535 ms after it, cannot be expressed
// in the sixteen bits the encoding provides. Such an event needs its own CTO
// rather than a clamped offset, so callers building a fragment should emit a
// fresh group 51 object instead of relying on the clamp here.
func (c Context) RelativeOffset(t dnp3.Timestamp) uint16 {
	if !c.HasCTO || !t.IsValid() {
		return 0
	}
	delta := t.Time.Sub(c.CTO).Milliseconds()
	switch {
	case delta < 0:
		return 0
	case delta > 0xFFFF:
		return 0xFFFF
	default:
		return uint16(delta)
	}
}

// WithCTO returns a copy of the context with its common time of occurrence
// set, as a parser does on encountering a group 51 object.
func (c Context) WithCTO(t time.Time) Context {
	c.CTO = t
	c.HasCTO = true
	return c
}

// Codec parses and writes one group and variation of a measurement type.
//
// Parse assumes buf holds at least the object's size; callers get that
// guarantee from the framing layer, which has already validated the header's
// length arithmetic against the fragment.
type Codec[T any] struct {
	Parse func(buf []byte, ctx Context) T
	Write func(dst []byte, v T, ctx Context) []byte
}

// Codec lookups, one per measurement type. Generics do not let a single map
// hold codecs of differing types, and the alternative — one struct with seven
// mostly-nil function fields — trades a compile-time guarantee for a runtime
// one. These stay separate.

// BinaryCodec returns the codec for a binary input variation.
func BinaryCodec(gv GroupVar) (Codec[dnp3.Binary], bool) {
	c, ok := binaryCodecs[gv]
	return c, ok
}

// DoubleBitCodec returns the codec for a double-bit binary input variation.
func DoubleBitCodec(gv GroupVar) (Codec[dnp3.DoubleBitBinary], bool) {
	c, ok := doublebitCodecs[gv]
	return c, ok
}

// CounterCodec returns the codec for a counter variation.
func CounterCodec(gv GroupVar) (Codec[dnp3.Counter], bool) {
	c, ok := counterCodecs[gv]
	return c, ok
}

// FrozenCounterCodec returns the codec for a frozen counter variation.
func FrozenCounterCodec(gv GroupVar) (Codec[dnp3.FrozenCounter], bool) {
	c, ok := frozencounterCodecs[gv]
	return c, ok
}

// AnalogCodec returns the codec for an analog input variation.
func AnalogCodec(gv GroupVar) (Codec[dnp3.Analog], bool) {
	c, ok := analogCodecs[gv]
	return c, ok
}

// BinaryOutputCodec returns the codec for a binary output status variation.
func BinaryOutputCodec(gv GroupVar) (Codec[dnp3.BinaryOutputStatus], bool) {
	c, ok := binaryoutputCodecs[gv]
	return c, ok
}

// AnalogOutputCodec returns the codec for an analog output status variation.
func AnalogOutputCodec(gv GroupVar) (Codec[dnp3.AnalogOutputStatus], bool) {
	c, ok := analogoutputCodecs[gv]
	return c, ok
}
