package app

import (
	"encoding/binary"
	"fmt"
)

// Qualifier is the octet following a group and variation that says how the
// objects are addressed and delimited.
//
//	bit 7     reserved, transmitted as zero
//	bits 6-4  index prefix
//	bits 3-0  range specifier
type Qualifier uint8

// IndexPrefix returns the prefix field.
func (q Qualifier) IndexPrefix() IndexPrefix { return IndexPrefix((q >> 4) & 0x07) }

// RangeSpec returns the range specifier field.
func (q Qualifier) RangeSpec() RangeSpec { return RangeSpec(q & 0x0F) }

// Reserved reports whether the reserved high bit is set, which a conforming
// device never transmits.
func (q Qualifier) Reserved() bool { return q&0x80 != 0 }

// MakeQualifier composes a qualifier from its two fields.
func MakeQualifier(p IndexPrefix, r RangeSpec) Qualifier {
	return Qualifier(uint8(p&0x07)<<4 | uint8(r&0x0F))
}

func (q Qualifier) String() string {
	return fmt.Sprintf("%#02x(%s,%s)", uint8(q), q.IndexPrefix(), q.RangeSpec())
}

// IndexPrefix says what precedes each object in the data field.
type IndexPrefix uint8

// Index prefix encodings.
const (
	// PrefixNone: objects are packed with no per-object prefix. Their indexes
	// come from the range field.
	PrefixNone IndexPrefix = 0
	// PrefixIndex1, PrefixIndex2, PrefixIndex4: each object is preceded by its
	// point index, of the given octet width.
	PrefixIndex1 IndexPrefix = 1
	PrefixIndex2 IndexPrefix = 2
	PrefixIndex4 IndexPrefix = 3
	// PrefixSize1, PrefixSize2, PrefixSize4: each object is preceded by its
	// own size, making the data self-describing. Used by variable-length
	// objects such as file transfer.
	PrefixSize1 IndexPrefix = 4
	PrefixSize2 IndexPrefix = 5
	PrefixSize4 IndexPrefix = 6
	// PrefixReserved is not valid on the wire.
	PrefixReserved IndexPrefix = 7
)

var prefixNames = [8]string{
	"none", "index8", "index16", "index32",
	"size8", "size16", "size32", "reserved",
}

func (p IndexPrefix) String() string {
	if int(p) < len(prefixNames) {
		return prefixNames[p]
	}
	return "IndexPrefix(?)"
}

// Octets returns the width of the prefix that precedes each object.
func (p IndexPrefix) Octets() int {
	switch p {
	case PrefixNone:
		return 0
	case PrefixIndex1, PrefixSize1:
		return 1
	case PrefixIndex2, PrefixSize2:
		return 2
	case PrefixIndex4, PrefixSize4:
		return 4
	default:
		return 0
	}
}

// IsIndex reports whether the prefix carries a point index.
func (p IndexPrefix) IsIndex() bool {
	return p == PrefixIndex1 || p == PrefixIndex2 || p == PrefixIndex4
}

// IsSize reports whether the prefix carries an object size, which makes the
// data self-describing and lets a parser walk objects whose length it could
// not otherwise know.
func (p IndexPrefix) IsSize() bool {
	return p == PrefixSize1 || p == PrefixSize2 || p == PrefixSize4
}

// Valid reports whether the encoding is defined by the standard.
func (p IndexPrefix) Valid() bool { return p <= PrefixSize4 }

// RangeSpec says how the set of objects is delimited.
type RangeSpec uint8

// Range specifier encodings.
const (
	// RangeStartStop8, 16, 32: an inclusive start and stop point index.
	RangeStartStop8  RangeSpec = 0
	RangeStartStop16 RangeSpec = 1
	RangeStartStop32 RangeSpec = 2
	// RangeVirtual8, 16, 32: start and stop virtual addresses.
	RangeVirtual8  RangeSpec = 3
	RangeVirtual16 RangeSpec = 4
	RangeVirtual32 RangeSpec = 5
	// RangeAllObjects: no range field. Every object of the type, which is how
	// a class poll and an integrity poll are expressed.
	RangeAllObjects RangeSpec = 6
	// RangeCount8, 16, 32: a count of objects with no index information.
	RangeCount8  RangeSpec = 7
	RangeCount16 RangeSpec = 8
	RangeCount32 RangeSpec = 9
	// RangeReservedA is not valid on the wire.
	RangeReservedA RangeSpec = 10
	// RangeVariable: a one-octet count of objects, each self-delimiting.
	RangeVariable RangeSpec = 11
)

var rangeNames = [16]string{
	"start-stop8", "start-stop16", "start-stop32",
	"virtual8", "virtual16", "virtual32",
	"all-objects",
	"count8", "count16", "count32",
	"reserved", "variable",
	"reserved", "reserved", "reserved", "reserved",
}

func (r RangeSpec) String() string {
	if int(r) < len(rangeNames) {
		return rangeNames[r]
	}
	return "RangeSpec(?)"
}

// Octets returns the width of the range field on the wire.
func (r RangeSpec) Octets() int {
	switch r {
	case RangeStartStop8, RangeVirtual8:
		return 2 // a start and a stop, one octet each
	case RangeStartStop16, RangeVirtual16:
		return 4
	case RangeStartStop32, RangeVirtual32:
		return 8
	case RangeAllObjects:
		return 0
	case RangeCount8, RangeVariable:
		return 1
	case RangeCount16:
		return 2
	case RangeCount32:
		return 4
	default:
		return 0
	}
}

// IsStartStop reports whether the range carries a start and stop index.
func (r RangeSpec) IsStartStop() bool {
	return r <= RangeVirtual32
}

// IsCount reports whether the range carries a plain object count.
func (r RangeSpec) IsCount() bool {
	return r == RangeCount8 || r == RangeCount16 || r == RangeCount32 || r == RangeVariable
}

// Valid reports whether the encoding is defined by the standard.
func (r RangeSpec) Valid() bool {
	return r <= RangeCount32 || r == RangeVariable
}

// Range is a decoded range field.
type Range struct {
	Spec RangeSpec
	// Start and Stop are the inclusive index bounds, valid when the specifier
	// is a start-stop form.
	Start uint32
	Stop  uint32
	// Count is the number of objects the header describes. For a start-stop
	// range it is derived as Stop-Start+1; for RangeAllObjects it is zero,
	// because the count is not on the wire.
	Count uint32
}

func (r Range) String() string {
	switch {
	case r.Spec == RangeAllObjects:
		return "all"
	case r.Spec.IsStartStop():
		return fmt.Sprintf("[%d..%d]", r.Start, r.Stop)
	default:
		return fmt.Sprintf("count=%d", r.Count)
	}
}

// IndexOf returns the point index of the i'th object described by a start-stop
// range. It is meaningless for count ranges, where indexes come from per-object
// prefixes instead.
func (r Range) IndexOf(i uint32) uint32 { return r.Start + i }

// parseRange decodes the range field for spec from the front of buf, returning
// the range and the octets consumed.
func parseRange(spec RangeSpec, buf []byte) (Range, int, error) {
	n := spec.Octets()
	if len(buf) < n {
		return Range{}, 0, ErrTruncated
	}
	rng := Range{Spec: spec}

	switch spec {
	case RangeAllObjects:
		return rng, 0, nil

	case RangeStartStop8, RangeVirtual8:
		rng.Start, rng.Stop = uint32(buf[0]), uint32(buf[1])
	case RangeStartStop16, RangeVirtual16:
		rng.Start = uint32(binary.LittleEndian.Uint16(buf[0:2]))
		rng.Stop = uint32(binary.LittleEndian.Uint16(buf[2:4]))
	case RangeStartStop32, RangeVirtual32:
		rng.Start = binary.LittleEndian.Uint32(buf[0:4])
		rng.Stop = binary.LittleEndian.Uint32(buf[4:8])

	case RangeCount8, RangeVariable:
		rng.Count = uint32(buf[0])
	case RangeCount16:
		rng.Count = uint32(binary.LittleEndian.Uint16(buf[0:2]))
	case RangeCount32:
		rng.Count = binary.LittleEndian.Uint32(buf[0:4])

	default:
		return Range{}, 0, fmt.Errorf("%w: range specifier %#x", ErrBadQualifier, uint8(spec))
	}

	if spec.IsStartStop() {
		if rng.Stop < rng.Start {
			return Range{}, 0, fmt.Errorf("%w: stop %d below start %d", ErrBadRange, rng.Stop, rng.Start)
		}
		// Stop-Start+1 is computed in 64 bits: a range of 0..0xFFFFFFFF is
		// legal on the wire and overflows a uint32 count.
		count := uint64(rng.Stop) - uint64(rng.Start) + 1
		if count > uint64(^uint32(0)) {
			return Range{}, 0, fmt.Errorf("%w: %d objects", ErrBadRange, count)
		}
		rng.Count = uint32(count)
	}
	return rng, n, nil
}

// appendRange encodes a range field.
func appendRange(dst []byte, r Range) []byte {
	switch r.Spec {
	case RangeAllObjects:
		return dst
	case RangeStartStop8, RangeVirtual8:
		return append(dst, byte(r.Start), byte(r.Stop))
	case RangeStartStop16, RangeVirtual16:
		return binary.LittleEndian.AppendUint16(
			binary.LittleEndian.AppendUint16(dst, uint16(r.Start)), uint16(r.Stop))
	case RangeStartStop32, RangeVirtual32:
		return binary.LittleEndian.AppendUint32(
			binary.LittleEndian.AppendUint32(dst, r.Start), r.Stop)
	case RangeCount8, RangeVariable:
		return append(dst, byte(r.Count))
	case RangeCount16:
		return binary.LittleEndian.AppendUint16(dst, uint16(r.Count))
	case RangeCount32:
		return binary.LittleEndian.AppendUint32(dst, r.Count)
	default:
		return dst
	}
}
