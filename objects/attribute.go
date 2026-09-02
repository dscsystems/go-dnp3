package objects

import (
	"encoding/binary"
	"fmt"
	"math"

	"github.com/dscsystems/go-dnp3"
)

// Group 0 is hand-written for the same reason group 70 is — the objects carry
// their own length — and for one more: there is nothing to generate. The
// variation is the attribute's identity rather than an encoding, so the table
// would need a row per attribute a device might invent.
//
// The encoding is the smallest in the protocol. One octet of data type, one of
// length, and that many octets of value.

// ErrAttribute means a group 0 object could not be decoded. It wraps
// [dnp3.ErrMalformed].
var ErrAttribute = fmt.Errorf("%w: device attribute", dnp3.ErrMalformed)

// AttributeHeaderSize is the data type and length that introduce every
// attribute value.
const AttributeHeaderSize = 2

// MaxAttributeValue is the longest value a one-octet length can carry.
const MaxAttributeValue = 255

// ParseAttribute decodes one device attribute value and reports how many
// octets it consumed, so a header carrying several can be walked.
//
// set and variation come from the object header — the header is where an
// attribute's identity lives — and are copied onto the result so a caller
// holding only the attribute still knows what it is.
func ParseAttribute(set, variation uint8, buf []byte) (dnp3.Attribute, int, error) {
	if len(buf) < AttributeHeaderSize {
		return dnp3.Attribute{}, 0, fmt.Errorf("%w: g0v%d is %d octets, needs %d",
			ErrAttribute, variation, len(buf), AttributeHeaderSize)
	}

	a := dnp3.Attribute{
		Set:       set,
		Variation: variation,
		Type:      dnp3.AttributeType(buf[0]),
	}
	size := int(buf[1])

	end := AttributeHeaderSize + size
	if end > len(buf) {
		return dnp3.Attribute{}, 0, fmt.Errorf("%w: g0v%d declares %d octets with %d left",
			ErrAttribute, variation, size, len(buf)-AttributeHeaderSize)
	}
	value := buf[AttributeHeaderSize:end]

	switch a.Type {
	case dnp3.AttrVisibleString:
		a.Text = string(value)

	case dnp3.AttrUnsignedInt:
		n, err := attributeUint(value)
		if err != nil {
			return dnp3.Attribute{}, 0, fmt.Errorf("g0v%d: %w", variation, err)
		}
		a.Number = int64(n)

	case dnp3.AttrSignedInt:
		n, err := attributeInt(value)
		if err != nil {
			return dnp3.Attribute{}, 0, fmt.Errorf("g0v%d: %w", variation, err)
		}
		a.Number = n

	case dnp3.AttrFloat:
		switch size {
		case 4:
			a.Real = float64(math.Float32frombits(binary.LittleEndian.Uint32(value)))
		case 8:
			a.Real = math.Float64frombits(binary.LittleEndian.Uint64(value))
		default:
			return dnp3.Attribute{}, 0, fmt.Errorf("%w: g0v%d is a %d octet float",
				ErrAttribute, variation, size)
		}

	case dnp3.AttrTime:
		if size != Time48Size {
			return dnp3.Attribute{}, 0, fmt.Errorf("%w: g0v%d is a %d octet time",
				ErrAttribute, variation, size)
		}
		a.Time = ParseTime48(value).Time

	default:
		// An octet string, a bit string, or a type this implementation has
		// never heard of. The octets are kept either way: a value nobody can
		// interpret is still worth showing, and a device is entitled to use a
		// type code newer than this code.
		a.Octets = append([]byte(nil), value...)
	}

	return a, end, nil
}

// attributeUint decodes an unsigned attribute of one, two or four octets.
//
// The width is whatever the device sent. Anything else is refused rather than
// padded, because a length nobody expects usually means the value is not what
// this thinks it is.
func attributeUint(b []byte) (uint64, error) {
	switch len(b) {
	case 1:
		return uint64(b[0]), nil
	case 2:
		return uint64(binary.LittleEndian.Uint16(b)), nil
	case 4:
		return uint64(binary.LittleEndian.Uint32(b)), nil
	case 8:
		return binary.LittleEndian.Uint64(b), nil
	default:
		return 0, fmt.Errorf("%w: an unsigned value of %d octets", ErrAttribute, len(b))
	}
}

// attributeInt decodes a signed attribute, sign-extending from its width.
func attributeInt(b []byte) (int64, error) {
	switch len(b) {
	case 1:
		return int64(int8(b[0])), nil
	case 2:
		return int64(int16(binary.LittleEndian.Uint16(b))), nil
	case 4:
		return int64(int32(binary.LittleEndian.Uint32(b))), nil
	case 8:
		return int64(binary.LittleEndian.Uint64(b)), nil
	default:
		return 0, fmt.Errorf("%w: a signed value of %d octets", ErrAttribute, len(b))
	}
}

// AppendAttribute encodes an attribute's value: its data type, its length, and
// the octets themselves. The identity goes in the object header, not here.
func AppendAttribute(dst []byte, a dnp3.Attribute) ([]byte, error) {
	var value []byte

	switch a.Type {
	case dnp3.AttrVisibleString:
		value = []byte(a.Text)

	case dnp3.AttrUnsignedInt, dnp3.AttrSignedInt:
		// The narrowest width that holds the number, which is what devices
		// send: a point count of 6 arrives as one octet, not eight.
		value = appendNarrowInt(nil, a.Number, a.Type == dnp3.AttrSignedInt)

	case dnp3.AttrFloat:
		value = binary.LittleEndian.AppendUint32(nil, math.Float32bits(float32(a.Real)))

	case dnp3.AttrTime:
		value = appendTime48(nil, dnp3.TimeToDNP3(a.Time))

	default:
		value = a.Octets
	}

	if len(value) > MaxAttributeValue {
		return nil, fmt.Errorf("%w: g0v%d is %d octets, and the length field holds %d",
			ErrAttribute, a.Variation, len(value), MaxAttributeValue)
	}

	dst = append(dst, byte(a.Type), byte(len(value)))
	return append(dst, value...), nil
}

// appendNarrowInt encodes an integer in the fewest octets that hold it.
func appendNarrowInt(dst []byte, v int64, signed bool) []byte {
	switch {
	case signed && v >= math.MinInt8 && v <= math.MaxInt8:
		return append(dst, byte(int8(v)))
	case !signed && v >= 0 && v <= math.MaxUint8:
		return append(dst, byte(v))
	case signed && v >= math.MinInt16 && v <= math.MaxInt16:
		return binary.LittleEndian.AppendUint16(dst, uint16(int16(v)))
	case !signed && v >= 0 && v <= math.MaxUint16:
		return binary.LittleEndian.AppendUint16(dst, uint16(v))
	case signed && v >= math.MinInt32 && v <= math.MaxInt32:
		return binary.LittleEndian.AppendUint32(dst, uint32(int32(v)))
	case !signed && v >= 0 && v <= math.MaxUint32:
		return binary.LittleEndian.AppendUint32(dst, uint32(v))
	default:
		return binary.LittleEndian.AppendUint64(dst, uint64(v))
	}
}

// ---------- constructors ----------

// StringAttribute builds a visible-string attribute, which is what most of
// the ones a person reads are.
func StringAttribute(variation uint8, text string) dnp3.Attribute {
	return dnp3.Attribute{
		Variation: variation, Type: dnp3.AttrVisibleString, Text: text,
	}
}

// UintAttribute builds an unsigned attribute, which is what the counts and
// the capability flags are.
func UintAttribute(variation uint8, v uint64) dnp3.Attribute {
	return dnp3.Attribute{
		Variation: variation, Type: dnp3.AttrUnsignedInt, Number: int64(v),
	}
}

// IntAttribute builds a signed attribute.
func IntAttribute(variation uint8, v int64) dnp3.Attribute {
	return dnp3.Attribute{
		Variation: variation, Type: dnp3.AttrSignedInt, Number: v,
	}
}
