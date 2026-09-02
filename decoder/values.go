package decoder

import (
	"bytes"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/objects"
)

// Value is one decoded measurement together with the point index it was
// reported at.
//
// The value is held as a formatted string rather than as a typed union: a
// decoder's job is to show what arrived, and every consumer of this package —
// a log line, a terminal table, a text dump — wants text. Callers that need
// typed measurements should use the object codecs directly.
type Value struct {
	Index uint16
	Type  dnp3.PointType
	Value string
	Flags dnp3.Flags
	Time  dnp3.Timestamp
}

func (v Value) String() string {
	s := fmt.Sprintf("[%d] %s", v.Index, v.Value)

	// The state bit is already spelled out as ON or OFF, so repeating it as a
	// quality flag is noise in the one place the output has to stay scannable.
	flags := v.Flags
	if v.Type == dnp3.TypeBinary || v.Type == dnp3.TypeBinaryOutputStatus {
		flags &^= dnp3.StateBit
	}
	if flags != 0 {
		s += "  " + flags.StringFor(v.Type)
	}
	if v.Time.IsValid() {
		s += "  " + v.Time.String()
	}
	return s
}

// DecodeValues decodes the measurements an object header introduces.
//
// It returns nil for headers that carry no measurements — class objects,
// commands, times — rather than an error, because those are perfectly normal
// and a decoder that errored on them would be noise. The bool reports whether
// the header was one this function knows how to decode.
//
// ctx supplies the session state the objects themselves do not carry: whether
// the outstation's clock is synchronised, and the common time of occurrence
// that relative-time events are measured from.
func DecodeValues(h app.ObjectHeader, ctx objects.Context) ([]Value, bool) {
	gv := objects.GV(h.Group, h.Variation)
	if len(h.Data) == 0 {
		return nil, false
	}

	// Octet strings first: their length is the variation number, so there is
	// no descriptor row to look up and a registry-first check would drop them.
	if h.Group == 110 || h.Group == 111 {
		return decodeOctetStrings(h), true
	}

	// File transfer likewise: its objects are self-describing and carry no
	// point index, so there is no descriptor row to look up and nothing the
	// measurement path below could do with them.
	if h.Group == 70 {
		return decodeFileObjects(h)
	}

	// And device attributes, where the variation is the attribute's identity
	// rather than an encoding: there is no row to look up because there is no
	// table that could hold one per attribute a device might invent.
	if h.Group == 0 {
		return decodeAttributes(h)
	}

	d, ok := objects.Lookup(gv)
	if !ok {
		return nil, false
	}

	// Commands are not measurements, but they are the single most important
	// thing to be able to read in a capture: an operator debugging a failed
	// trip needs to see the control code and the status that came back.
	if d.Kind == objects.KindCommand {
		return decodeCommands(h, d)
	}
	if d.Measurement == dnp3.TypeUnknown {
		return nil, false
	}

	count := int(h.Count())
	if d.Packed {
		return decodePacked(h, d, count), true
	}

	size, ok := d.SizeOctets()
	if !ok || size == 0 {
		return nil, false
	}

	prefix := h.Qualifier.IndexPrefix()
	prefixLen := 0
	if prefix.IsIndex() {
		prefixLen = prefix.Octets()
	}

	out := make([]Value, 0, count)
	off := 0
	for i := range count {
		if off+prefixLen+size > len(h.Data) {
			break // the framing layer validated this; stop rather than panic
		}

		index := uint16(h.Range.IndexOf(uint32(i)))
		if prefixLen > 0 {
			index = uint16(readPrefix(h.Data[off:], prefixLen))
			off += prefixLen
		}

		out = append(out, decodeOne(gv, d, index, h.Data[off:off+size], ctx))
		off += size
	}
	return out, true
}

// decodeOne dispatches to the codec for the measurement type.
func decodeOne(gv objects.GroupVar, d objects.Descriptor, index uint16, buf []byte, ctx objects.Context) Value {
	v := Value{Index: index, Type: d.Measurement}

	switch d.Measurement {
	case dnp3.TypeBinary:
		if c, ok := objects.BinaryCodec(gv); ok {
			m := c.Parse(buf, ctx)
			v.Value, v.Flags, v.Time = boolText(m.Value), m.Flags, m.Time
		}
	case dnp3.TypeDoubleBitBinary:
		if c, ok := objects.DoubleBitCodec(gv); ok {
			m := c.Parse(buf, ctx)
			v.Value, v.Flags, v.Time = m.Value.String(), m.Flags, m.Time
		}
	case dnp3.TypeCounter:
		if c, ok := objects.CounterCodec(gv); ok {
			m := c.Parse(buf, ctx)
			v.Value, v.Flags, v.Time = fmt.Sprint(m.Value), m.Flags, m.Time
		}
	case dnp3.TypeFrozenCounter:
		if c, ok := objects.FrozenCounterCodec(gv); ok {
			m := c.Parse(buf, ctx)
			v.Value, v.Flags, v.Time = fmt.Sprint(m.Value), m.Flags, m.Time
		}
	case dnp3.TypeAnalog:
		if c, ok := objects.AnalogCodec(gv); ok {
			m := c.Parse(buf, ctx)
			v.Value, v.Flags, v.Time = formatFloat(m.Value), m.Flags, m.Time
		}
	case dnp3.TypeBinaryOutputStatus:
		if c, ok := objects.BinaryOutputCodec(gv); ok {
			m := c.Parse(buf, ctx)
			v.Value, v.Flags, v.Time = boolText(m.Value), m.Flags, m.Time
		}
	case dnp3.TypeAnalogOutputStatus:
		if c, ok := objects.AnalogOutputCodec(gv); ok {
			m := c.Parse(buf, ctx)
			v.Value, v.Flags, v.Time = formatFloat(m.Value), m.Flags, m.Time
		}
	}
	return v
}

// decodePacked handles the bit-packed variations, whose unit of encoding is
// the range rather than the object.
func decodePacked(h app.ObjectHeader, d objects.Descriptor, count int) []Value {
	out := make([]Value, 0, count)

	switch d.Measurement {
	case dnp3.TypeDoubleBitBinary:
		for i, m := range objects.ParsePackedDoubleBit(h.Data, count, nil) {
			out = append(out, Value{
				Index: uint16(h.Range.IndexOf(uint32(i))),
				Type:  d.Measurement,
				Value: m.Value.String(),
				Flags: m.Flags,
			})
		}
	case dnp3.TypeBinaryOutputStatus:
		for i, m := range objects.ParsePackedBinaryOutput(h.Data, count, nil) {
			out = append(out, Value{
				Index: uint16(h.Range.IndexOf(uint32(i))),
				Type:  d.Measurement,
				Value: boolText(m.Value),
				Flags: m.Flags,
			})
		}
	default:
		for i, m := range objects.ParsePackedBinary(h.Data, count, nil) {
			out = append(out, Value{
				Index: uint16(h.Range.IndexOf(uint32(i))),
				Type:  d.Measurement,
				Value: boolText(m.Value),
				Flags: m.Flags,
			})
		}
	}
	return out
}

// decodeCommands renders control relay output blocks and analog output
// commands, which carry their own structure rather than a measurement.
func decodeCommands(h app.ObjectHeader, d objects.Descriptor) ([]Value, bool) {
	size, ok := d.SizeOctets()
	if !ok || size == 0 {
		return nil, false
	}

	prefix := h.Qualifier.IndexPrefix()
	prefixLen := 0
	if prefix.IsIndex() {
		prefixLen = prefix.Octets()
	}

	out := make([]Value, 0, h.Count())
	off := 0
	for i := range int(h.Count()) {
		if off+prefixLen+size > len(h.Data) {
			break
		}

		index := uint16(h.Range.IndexOf(uint32(i)))
		if prefixLen > 0 {
			index = uint16(readPrefix(h.Data[off:], prefixLen))
			off += prefixLen
		}

		buf := h.Data[off : off+size]
		off += size

		var text string
		switch h.Group {
		case 12:
			c := objects.ParseCROB(buf)
			text = fmt.Sprintf("%s count=%d on=%dms off=%dms → %s",
				c.Code, c.Count, c.OnTime, c.OffTime, c.Status)
		case 41:
			text = analogOutputText(h.Variation, buf)
		default:
			continue
		}
		out = append(out, Value{Index: index, Value: text})
	}
	return out, len(out) > 0
}

func analogOutputText(variation uint8, buf []byte) string {
	switch variation {
	case 1:
		c := objects.ParseAnalogOutputInt32(buf)
		return fmt.Sprintf("%d (int32) → %s", c.Value, c.Status)
	case 2:
		c := objects.ParseAnalogOutputInt16(buf)
		return fmt.Sprintf("%d (int16) → %s", c.Value, c.Status)
	case 3:
		c := objects.ParseAnalogOutputFloat32(buf)
		return fmt.Sprintf("%s (float32) → %s", formatFloat(float64(c.Value)), c.Status)
	case 4:
		c := objects.ParseAnalogOutputFloat64(buf)
		return fmt.Sprintf("%s (float64) → %s", formatFloat(c.Value), c.Status)
	}
	return ""
}

// decodeOctetStrings renders group 110 and 111 objects as text, falling back
// to hex when the bytes are not printable — a serial number is usually ASCII,
// but nothing in the protocol says it must be.
func decodeOctetStrings(h app.ObjectHeader) []Value {
	size := int(h.Variation)
	if size == 0 {
		return nil
	}

	prefixLen := 0
	if p := h.Qualifier.IndexPrefix(); p.IsIndex() {
		prefixLen = p.Octets()
	}

	out := make([]Value, 0, h.Count())
	off := 0
	for i := range int(h.Count()) {
		if off+prefixLen+size > len(h.Data) {
			break
		}
		index := uint16(h.Range.IndexOf(uint32(i)))
		if prefixLen > 0 {
			index = uint16(readPrefix(h.Data[off:], prefixLen))
			off += prefixLen
		}
		raw := h.Data[off : off+size]
		off += size

		out = append(out, Value{
			Index: index,
			Type:  dnp3.TypeOctetString,
			Value: octetText(raw),
		})
	}
	return out
}

// octetText renders an octet string for display.
func octetText(raw []byte) string {
	printable := true
	for _, c := range raw {
		if c != 0 && (c < 0x20 || c > 0x7E) {
			printable = false
			break
		}
	}
	if printable {
		return strconv.Quote(string(bytes.TrimRight(raw, "\x00")))
	}
	return fmt.Sprintf("% x", raw)
}

func readPrefix(buf []byte, width int) uint32 {
	switch width {
	case 1:
		return uint32(buf[0])
	case 2:
		return uint32(buf[0]) | uint32(buf[1])<<8
	case 4:
		return uint32(buf[0]) | uint32(buf[1])<<8 | uint32(buf[2])<<16 | uint32(buf[3])<<24
	}
	return 0
}

// boolText renders a binary state the way an operator reads a mimic panel,
// not the way a programmer reads a bool.
func boolText(v bool) string {
	if v {
		return "ON"
	}
	return "OFF"
}

// formatFloat prints an analog the way a value belongs in a telemetry table:
// whole numbers without a trailing ".0", fractions without trailing zeros, and
// no exponent notation for the ranges telemetry actually uses.
func formatFloat(v float64) string {
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return fmt.Sprintf("%d", int64(v))
	}
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.6f", v), "0"), ".")
}
