package dnp3

import (
	"fmt"
	"math"
	"time"
)

// TimestampQuality describes how much a timestamp can be relied on.
type TimestampQuality uint8

// Timestamp qualities, in increasing order of trust.
const (
	// TimestampInvalid means the measurement carried no time at all. The
	// Timestamp's Time field is the zero value.
	TimestampInvalid TimestampQuality = iota
	// TimestampUnsynchronized means the outstation reported a time while its
	// own clock was not synchronised to the master.
	TimestampUnsynchronized
	// TimestampSynchronized means the outstation's clock was synchronised when
	// the measurement was taken.
	TimestampSynchronized
)

func (q TimestampQuality) String() string {
	switch q {
	case TimestampInvalid:
		return "invalid"
	case TimestampUnsynchronized:
		return "unsynchronized"
	case TimestampSynchronized:
		return "synchronized"
	default:
		return "TimestampQuality(?)"
	}
}

// Timestamp pairs a time with how much that time can be trusted.
//
// The two are kept together deliberately. A DNP3 measurement can carry a
// perfectly well-formed timestamp from an outstation whose clock has never
// been set, and a consumer that ignores the quality will happily file that
// event under 1970 or under whatever the drifted clock says.
type Timestamp struct {
	Time    time.Time
	Quality TimestampQuality
}

// Now returns a synchronized timestamp for t.
func Now(t time.Time) Timestamp { return Timestamp{Time: t, Quality: TimestampSynchronized} }

// Unsynchronized returns a timestamp for t whose source clock was not synced.
func Unsynchronized(t time.Time) Timestamp {
	return Timestamp{Time: t, Quality: TimestampUnsynchronized}
}

// NoTime returns the timestamp used by measurements that carry no time.
func NoTime() Timestamp { return Timestamp{} }

// IsValid reports whether the timestamp carries a usable time.
func (t Timestamp) IsValid() bool { return t.Quality != TimestampInvalid }

func (t Timestamp) String() string {
	if !t.IsValid() {
		return "—"
	}
	return fmt.Sprintf("%s (%s)", t.Time.UTC().Format("2006-01-02T15:04:05.000Z"), t.Quality)
}

// MaxDNP3Time is the largest instant a 48-bit DNP3 timestamp can express:
// 2^48 - 1 milliseconds after the UNIX epoch, which falls in the year 10889.
const MaxDNP3Time = int64(1)<<48 - 1

// TimeToDNP3 converts t to DNP3's wire representation: milliseconds since the
// UNIX epoch, clamped to the 48 bits the encoding provides. Times before the
// epoch clamp to zero.
func TimeToDNP3(t time.Time) uint64 {
	ms := t.UnixMilli()
	switch {
	case ms < 0:
		return 0
	case ms > MaxDNP3Time:
		return uint64(MaxDNP3Time)
	default:
		return uint64(ms)
	}
}

// DNP3ToTime converts a 48-bit DNP3 timestamp to a time.Time in UTC. Bits
// above the low 48 are ignored, matching what a conforming encoder can emit.
func DNP3ToTime(ms uint64) time.Time {
	return time.UnixMilli(int64(ms & uint64(MaxDNP3Time))).UTC()
}

// DoubleBit is the state of a double-bit binary input, which reports a
// two-contact device — typically a breaker with separate open and closed
// auxiliary contacts — and can therefore distinguish a device in transit or
// miswired from one that is definitely open or definitely closed.
type DoubleBit uint8

// Double-bit states. The numeric values are the on-the-wire encoding.
const (
	// DoubleBitIntermediate: both contacts read open. The device is moving.
	DoubleBitIntermediate DoubleBit = 0
	// DoubleBitDeterminedOff: the device is open.
	DoubleBitDeterminedOff DoubleBit = 1
	// DoubleBitDeterminedOn: the device is closed.
	DoubleBitDeterminedOn DoubleBit = 2
	// DoubleBitIndeterminate: both contacts read closed, which is impossible.
	DoubleBitIndeterminate DoubleBit = 3
)

func (d DoubleBit) String() string {
	switch d {
	case DoubleBitIntermediate:
		return "Intermediate"
	case DoubleBitDeterminedOff:
		return "Off"
	case DoubleBitDeterminedOn:
		return "On"
	case DoubleBitIndeterminate:
		return "Indeterminate"
	default:
		return "DoubleBit(?)"
	}
}

// Measurement types. Every one carries a value, a quality octet and a
// timestamp; static variations simply leave the timestamp invalid.

// Binary is a single-bit status input.
type Binary struct {
	Value bool
	Flags Flags
	Time  Timestamp
}

// DoubleBitBinary is a two-bit status input.
type DoubleBitBinary struct {
	Value DoubleBit
	Flags Flags
	Time  Timestamp
}

// Counter is a running count. It wraps rather than saturating.
type Counter struct {
	Value uint32
	Flags Flags
	Time  Timestamp
}

// FrozenCounter is a counter value captured at a freeze command.
type FrozenCounter struct {
	Value uint32
	Flags Flags
	Time  Timestamp
}

// Analog is an analog input. The stack carries every analog variation —
// 16-bit, 32-bit, single and double precision — as a float64 and narrows only
// at the encoding boundary.
type Analog struct {
	Value float64
	Flags Flags
	Time  Timestamp
}

// BinaryOutputStatus reports the present state of a control point.
type BinaryOutputStatus struct {
	Value bool
	Flags Flags
	Time  Timestamp
}

// AnalogOutputStatus reports the present value of an analog control point.
type AnalogOutputStatus struct {
	Value float64
	Flags Flags
	Time  Timestamp
}

// OctetString is a variable-length opaque value, group 110 and 111. The
// standard caps it at 255 octets.
type OctetString []byte

// MaxOctetStringLen is the largest octet string the encoding allows.
const MaxOctetStringLen = 255

// Indexed pairs a measurement with the point index it was reported at.
type Indexed[T any] struct {
	Index uint16
	Value T
}

// Class is a bit mask over the DNP3 event classes plus class 0, the static
// data set. Masks are how polls are expressed: an integrity poll is
// Class0 | Class1 | Class2 | Class3.
type Class uint8

// Class mask bits.
const (
	Class0 Class = 1 << iota // static data
	Class1                   // event class 1, conventionally the most urgent
	Class2
	Class3
	// ClassNone assigns a point to no event class, suppressing its events.
	ClassNone Class = 0
	// Class123 is every event class, excluding static data.
	Class123 = Class1 | Class2 | Class3
	// ClassAll is an integrity poll: static data plus every event class.
	ClassAll = Class0 | Class123
)

// Has reports whether every class in mask is present.
func (c Class) Has(mask Class) bool { return c&mask == mask }

func (c Class) String() string {
	if c == 0 {
		return "none"
	}
	out := make([]byte, 0, 8)
	for i, bit := range []Class{Class0, Class1, Class2, Class3} {
		if c&bit != 0 {
			if len(out) > 0 {
				out = append(out, '+')
			}
			out = append(out, byte('0'+i))
		}
	}
	return string(out)
}

// ControlCode is the operation field of a Control Relay Output Block. It packs
// an operation type in the low four bits with three modifier flags above.
type ControlCode uint8

// Control operation types, occupying the low nibble of a ControlCode.
const (
	ControlNUL          ControlCode = 0x00
	ControlPulseOn      ControlCode = 0x01
	ControlPulseOff     ControlCode = 0x02
	ControlLatchOn      ControlCode = 0x03
	ControlLatchOff     ControlCode = 0x04
	controlOpTypeMask   ControlCode = 0x0F
	controlQueue        ControlCode = 0x10 // obsolete; kept for round-tripping
	controlClear        ControlCode = 0x20
	controlTripCloseMsk ControlCode = 0xC0
)

// Trip/close modifiers, which pair with an operation type to drive the two
// coils of a breaker.
const (
	// ControlClose sets the close coil field.
	ControlClose ControlCode = 0x80
	// ControlTrip sets the trip coil field.
	ControlTrip ControlCode = 0x40
)

// OpType returns the operation type nibble.
func (c ControlCode) OpType() ControlCode { return c & controlOpTypeMask }

// IsTrip reports whether the trip coil is selected.
func (c ControlCode) IsTrip() bool { return c&controlTripCloseMsk == ControlTrip }

// IsClose reports whether the close coil is selected.
func (c ControlCode) IsClose() bool { return c&controlTripCloseMsk == ControlClose }

// Clear reports whether the clear bit is set, which cancels a queued or
// running operation on the point.
func (c ControlCode) Clear() bool { return c&controlClear != 0 }

func (c ControlCode) String() string {
	var op string
	switch c.OpType() {
	case ControlNUL:
		op = "NUL"
	case ControlPulseOn:
		op = "PULSE_ON"
	case ControlPulseOff:
		op = "PULSE_OFF"
	case ControlLatchOn:
		op = "LATCH_ON"
	case ControlLatchOff:
		op = "LATCH_OFF"
	default:
		op = fmt.Sprintf("OP(%d)", uint8(c.OpType()))
	}
	switch {
	case c.IsTrip():
		op += "|TRIP"
	case c.IsClose():
		op += "|CLOSE"
	}
	if c.Clear() {
		op += "|CLEAR"
	}
	if c&controlQueue != 0 {
		op += "|QUEUE"
	}
	return op
}

// CommandStatus is the outcome an outstation reports for a control operation.
type CommandStatus uint8

// Command status codes defined by the standard.
const (
	CommandSuccess           CommandStatus = 0
	CommandTimeout           CommandStatus = 1
	CommandNoSelect          CommandStatus = 2
	CommandFormatError       CommandStatus = 3
	CommandNotSupported      CommandStatus = 4
	CommandAlreadyActive     CommandStatus = 5
	CommandHardwareError     CommandStatus = 6
	CommandLocal             CommandStatus = 7
	CommandTooManyOps        CommandStatus = 8
	CommandNotAuthorized     CommandStatus = 9
	CommandAutomationInhibit CommandStatus = 10
	CommandProcessingLimited CommandStatus = 11
	CommandOutOfRange        CommandStatus = 12
	CommandDownstreamLocal   CommandStatus = 13
	CommandAlreadyComplete   CommandStatus = 14
	CommandBlocked           CommandStatus = 15
	CommandCanceled          CommandStatus = 16
	CommandBlockedOtherMstr  CommandStatus = 17
	CommandDownstreamFail    CommandStatus = 18
	CommandNonParticipating  CommandStatus = 126
	CommandUndefined         CommandStatus = 127
)

var commandStatusNames = map[CommandStatus]string{
	CommandSuccess:           "SUCCESS",
	CommandTimeout:           "TIMEOUT",
	CommandNoSelect:          "NO_SELECT",
	CommandFormatError:       "FORMAT_ERROR",
	CommandNotSupported:      "NOT_SUPPORTED",
	CommandAlreadyActive:     "ALREADY_ACTIVE",
	CommandHardwareError:     "HARDWARE_ERROR",
	CommandLocal:             "LOCAL",
	CommandTooManyOps:        "TOO_MANY_OPS",
	CommandNotAuthorized:     "NOT_AUTHORIZED",
	CommandAutomationInhibit: "AUTOMATION_INHIBIT",
	CommandProcessingLimited: "PROCESSING_LIMITED",
	CommandOutOfRange:        "OUT_OF_RANGE",
	CommandDownstreamLocal:   "DOWNSTREAM_LOCAL",
	CommandAlreadyComplete:   "ALREADY_COMPLETE",
	CommandBlocked:           "BLOCKED",
	CommandCanceled:          "CANCELED",
	CommandBlockedOtherMstr:  "BLOCKED_OTHER_MASTER",
	CommandDownstreamFail:    "DOWNSTREAM_FAIL",
	CommandNonParticipating:  "NON_PARTICIPATING",
	CommandUndefined:         "UNDEFINED",
}

func (s CommandStatus) String() string {
	if n, ok := commandStatusNames[s]; ok {
		return n
	}
	return fmt.Sprintf("CommandStatus(%d)", uint8(s))
}

// OK reports whether the command succeeded.
func (s CommandStatus) OK() bool { return s == CommandSuccess }

// ControlRelayOutputBlock is a group 12 variation 1 control: the command used
// to operate breakers, reclosers and other discrete outputs.
type ControlRelayOutputBlock struct {
	Code ControlCode
	// Count is how many times to execute the operation. Zero is legal and
	// means "do nothing", which some masters use to probe a point.
	Count uint8
	// OnTime and OffTime are milliseconds, used by the pulse operations.
	OnTime  uint32
	OffTime uint32
	// Status is meaningful only on a response echo.
	Status CommandStatus
}

func (c ControlRelayOutputBlock) String() string {
	return fmt.Sprintf("CROB{%s count=%d on=%dms off=%dms status=%s}",
		c.Code, c.Count, c.OnTime, c.OffTime, c.Status)
}

// AnalogOutputInt16 is a group 41 variation 2 control.
type AnalogOutputInt16 struct {
	Value  int16
	Status CommandStatus
}

// AnalogOutputInt32 is a group 41 variation 1 control.
type AnalogOutputInt32 struct {
	Value  int32
	Status CommandStatus
}

// AnalogOutputFloat32 is a group 41 variation 3 control.
type AnalogOutputFloat32 struct {
	Value  float32
	Status CommandStatus
}

// AnalogOutputFloat64 is a group 41 variation 4 control.
type AnalogOutputFloat64 struct {
	Value  float64
	Status CommandStatus
}

// AnalogOutput is any of the four analog output command variations.
type AnalogOutput interface {
	AnalogOutputInt16 | AnalogOutputInt32 | AnalogOutputFloat32 | AnalogOutputFloat64
}

// RestartMode selects between the two restart function codes.
type RestartMode uint8

// Restart modes.
const (
	// ColdRestart reinitialises the outstation completely, as though power
	// had been cycled.
	ColdRestart RestartMode = iota
	// WarmRestart reinitialises only the communications process.
	WarmRestart
)

func (m RestartMode) String() string {
	if m == ColdRestart {
		return "cold"
	}
	return "warm"
}

// AnalogFitsIn16 reports whether v can be encoded as a 16-bit analog without
// loss, which the outstation needs when a master requests a narrow variation.
func AnalogFitsIn16(v float64) bool {
	return v >= math.MinInt16 && v <= math.MaxInt16 && v == math.Trunc(v)
}

// AnalogFitsIn32 reports whether v can be encoded as a 32-bit analog without
// loss.
func AnalogFitsIn32(v float64) bool {
	return v >= math.MinInt32 && v <= math.MaxInt32 && v == math.Trunc(v)
}
