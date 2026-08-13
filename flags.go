package dnp3

import "strings"

// Flags is the quality octet that accompanies most measurements.
//
// The low five bits mean the same thing for every measurement type. The upper
// three are type-specific: what is CHATTER_FILTER on a binary input is
// OVER_RANGE on an analog input and ROLLOVER on a counter. Because the
// interpretation depends on the point type, Flags stores the raw octet and
// leaves naming to the per-type accessors below.
type Flags uint8

// Flag bits common to every measurement type.
const (
	// Online means the point is being read from the field. A cleared Online
	// bit is the single most important quality signal in DNP3: the value
	// present alongside it is not trustworthy.
	Online Flags = 0x01
	// Restart means the value has not been updated since the device restarted.
	Restart Flags = 0x02
	// CommLost means communication with the source of the point has failed.
	CommLost Flags = 0x04
	// RemoteForced means the value was forced by a downstream device.
	RemoteForced Flags = 0x08
	// LocalForced means the value was forced by the outstation itself.
	LocalForced Flags = 0x10
)

// Type-specific bits. Each is valid only for the measurement types named.
const (
	// ChatterFilter applies to binary and double-bit binary inputs: the point
	// is toggling faster than the outstation's filter allows.
	ChatterFilter Flags = 0x20
	// Rollover applies to counters. Deprecated by the standard in favour of
	// letting the counter wrap, but still emitted by older devices.
	Rollover Flags = 0x20
	// Discontinuity applies to counters: the value cannot be compared against
	// the previous reading.
	Discontinuity Flags = 0x40
	// OverRange applies to analog inputs and outputs: the value exceeds the
	// range the point can represent.
	OverRange Flags = 0x20
	// ReferenceErr applies to analog inputs and outputs: the reference used to
	// digitise the value is not accurate.
	ReferenceErr Flags = 0x40
	// StateBit carries the value itself for binary points inside a flags
	// octet, as in group 1 variation 2.
	StateBit Flags = 0x80
)

// Has reports whether every bit in mask is set.
func (f Flags) Has(mask Flags) bool { return f&mask == mask }

// HasAny reports whether any bit in mask is set.
func (f Flags) HasAny(mask Flags) bool { return f&mask != 0 }

// Set returns f with every bit in mask set.
func (f Flags) Set(mask Flags) Flags { return f | mask }

// Clear returns f with every bit in mask cleared.
func (f Flags) Clear(mask Flags) Flags { return f &^ mask }

// IsGood reports whether the value carrying these flags may be trusted: online,
// not restarting, not comm-lost, and not forced from either end.
func (f Flags) IsGood() bool {
	return f.Has(Online) && !f.HasAny(Restart|CommLost|RemoteForced|LocalForced)
}

// commonNames indexes the five type-independent bits, low bit first.
var commonNames = [5]string{"ONLINE", "RESTART", "COMM_LOST", "REMOTE_FORCED", "LOCAL_FORCED"}

// String renders the common bits by name. Type-specific bits are shown by
// position because their meaning depends on the point type; use
// [Flags.StringFor] when the type is known.
func (f Flags) String() string { return f.StringFor(TypeUnknown) }

// PointType identifies a measurement type, which fixes the meaning of the
// upper three quality bits.
type PointType uint8

// Measurement types.
const (
	TypeUnknown PointType = iota
	TypeBinary
	TypeDoubleBitBinary
	TypeCounter
	TypeFrozenCounter
	TypeAnalog
	TypeBinaryOutputStatus
	TypeAnalogOutputStatus
	TypeOctetString
)

var pointTypeNames = map[PointType]string{
	TypeUnknown:            "Unknown",
	TypeBinary:             "Binary",
	TypeDoubleBitBinary:    "DoubleBitBinary",
	TypeCounter:            "Counter",
	TypeFrozenCounter:      "FrozenCounter",
	TypeAnalog:             "Analog",
	TypeBinaryOutputStatus: "BinaryOutputStatus",
	TypeAnalogOutputStatus: "AnalogOutputStatus",
	TypeOctetString:        "OctetString",
}

func (p PointType) String() string {
	if s, ok := pointTypeNames[p]; ok {
		return s
	}
	return "PointType(?)"
}

// StringFor renders the flags with the upper bits named according to t.
func (f Flags) StringFor(t PointType) string {
	if f == 0 {
		return "—"
	}
	var b strings.Builder
	for i, name := range commonNames {
		if f&(1<<uint(i)) != 0 {
			if b.Len() > 0 {
				b.WriteByte('|')
			}
			b.WriteString(name)
		}
	}
	for _, hi := range upperBitNames(t) {
		if f&hi.bit != 0 {
			if b.Len() > 0 {
				b.WriteByte('|')
			}
			b.WriteString(hi.name)
		}
	}
	return b.String()
}

type namedBit struct {
	bit  Flags
	name string
}

func upperBitNames(t PointType) []namedBit {
	switch t {
	case TypeBinary, TypeBinaryOutputStatus, TypeDoubleBitBinary:
		return []namedBit{{ChatterFilter, "CHATTER_FILTER"}, {0x40, "BIT6"}, {StateBit, "STATE"}}
	case TypeCounter, TypeFrozenCounter:
		return []namedBit{{Rollover, "ROLLOVER"}, {Discontinuity, "DISCONTINUITY"}, {0x80, "BIT7"}}
	case TypeAnalog, TypeAnalogOutputStatus:
		return []namedBit{{OverRange, "OVER_RANGE"}, {ReferenceErr, "REFERENCE_ERR"}, {0x80, "BIT7"}}
	default:
		return []namedBit{{0x20, "BIT5"}, {0x40, "BIT6"}, {0x80, "BIT7"}}
	}
}
