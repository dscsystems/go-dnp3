package app

import "strings"

// IIN holds the two internal indication octets an outstation returns on every
// response: its running health and error report.
//
// The low octet is IIN1 and the high octet is IIN2, matching the order they
// appear on the wire.
type IIN uint16

// IIN1 bits — outstation state.
const (
	// IINBroadcast: the last request was received via a broadcast address.
	IINBroadcast IIN = 0x0001
	// IINClass1Events: class 1 events are available. The master should poll.
	IINClass1Events IIN = 0x0002
	// IINClass2Events: class 2 events are available.
	IINClass2Events IIN = 0x0004
	// IINClass3Events: class 3 events are available.
	IINClass3Events IIN = 0x0008
	// IINNeedTime: the outstation wants its clock set.
	IINNeedTime IIN = 0x0010
	// IINLocalControl: one or more points are in local control mode and will
	// not accept commands.
	IINLocalControl IIN = 0x0020
	// IINDeviceTrouble: a device-specific fault. Its meaning is defined by the
	// outstation, not the standard.
	IINDeviceTrouble IIN = 0x0040
	// IINDeviceRestart: the outstation has restarted. The master must re-run
	// its startup sequence and clear this bit; leaving it set means the
	// outstation keeps reporting a restart it has already recovered from.
	IINDeviceRestart IIN = 0x0080
)

// IIN2 bits — request errors.
const (
	// IINNoFuncCodeSupport: the function code is not implemented.
	IINNoFuncCodeSupport IIN = 0x0100
	// IINObjectUnknown: the request referenced a group or variation the
	// outstation does not have.
	IINObjectUnknown IIN = 0x0200
	// IINParameterError: qualifier, range or data fields were not valid.
	IINParameterError IIN = 0x0400
	// IINEventBufferOverflow: events were lost because the buffer filled. The
	// master's picture of the sequence of events now has a hole in it.
	IINEventBufferOverflow IIN = 0x0800
	// IINAlreadyExecuting: the requested operation is already running.
	IINAlreadyExecuting IIN = 0x1000
	// IINConfigCorrupt: the outstation's configuration is not valid.
	IINConfigCorrupt IIN = 0x2000
	// IINReserved1 and IINReserved2 must be transmitted as zero.
	IINReserved1 IIN = 0x4000
	IINReserved2 IIN = 0x8000
)

// EventClassMask is every "events available" bit.
const EventClassMask = IINClass1Events | IINClass2Events | IINClass3Events

// ErrorMask is every IIN2 bit that reports a problem with the request.
const ErrorMask = IINNoFuncCodeSupport | IINObjectUnknown | IINParameterError |
	IINAlreadyExecuting | IINConfigCorrupt

// ParseIIN decodes the two IIN octets in wire order.
func ParseIIN(iin1, iin2 byte) IIN { return IIN(iin1) | IIN(iin2)<<8 }

// Octets returns the two IIN octets in wire order.
func (i IIN) Octets() (iin1, iin2 byte) { return byte(i), byte(i >> 8) }

// Has reports whether every bit in mask is set.
func (i IIN) Has(mask IIN) bool { return i&mask == mask }

// HasAny reports whether any bit in mask is set.
func (i IIN) HasAny(mask IIN) bool { return i&mask != 0 }

// Set returns i with every bit in mask set.
func (i IIN) Set(mask IIN) IIN { return i | mask }

// Clear returns i with every bit in mask cleared.
func (i IIN) Clear(mask IIN) IIN { return i &^ mask }

// HasEvents reports whether any event class has data waiting.
func (i IIN) HasEvents() bool { return i.HasAny(EventClassMask) }

// HasError reports whether the outstation rejected something about the
// request.
func (i IIN) HasError() bool { return i.HasAny(ErrorMask) }

// EventClasses returns the event-class bits alone.
func (i IIN) EventClasses() IIN { return i & EventClassMask }

var iinNames = []struct {
	bit  IIN
	name string
}{
	{IINBroadcast, "BROADCAST"},
	{IINClass1Events, "CLASS_1_EVENTS"},
	{IINClass2Events, "CLASS_2_EVENTS"},
	{IINClass3Events, "CLASS_3_EVENTS"},
	{IINNeedTime, "NEED_TIME"},
	{IINLocalControl, "LOCAL_CONTROL"},
	{IINDeviceTrouble, "DEVICE_TROUBLE"},
	{IINDeviceRestart, "DEVICE_RESTART"},
	{IINNoFuncCodeSupport, "NO_FUNC_CODE_SUPPORT"},
	{IINObjectUnknown, "OBJECT_UNKNOWN"},
	{IINParameterError, "PARAMETER_ERROR"},
	{IINEventBufferOverflow, "EVENT_BUFFER_OVERFLOW"},
	{IINAlreadyExecuting, "ALREADY_EXECUTING"},
	{IINConfigCorrupt, "CONFIG_CORRUPT"},
	{IINReserved1, "RESERVED_1"},
	{IINReserved2, "RESERVED_2"},
}

// String renders the set bits by name, which is what a protocol log needs. An
// unset IIN renders as an em dash rather than "0x0000", because "no
// indications" is the common case and should not look like a value.
func (i IIN) String() string {
	if i == 0 {
		return "—"
	}
	var b strings.Builder
	for _, n := range iinNames {
		if i&n.bit != 0 {
			if b.Len() > 0 {
				b.WriteByte('|')
			}
			b.WriteString(n.name)
		}
	}
	return b.String()
}
