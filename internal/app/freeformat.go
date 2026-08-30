package app

import (
	"encoding/binary"
	"fmt"
)

// Free format is the qualifier a variable-length object travels under: a size
// prefix in front of every object and a count of them in the range field,
// which is what lets a parser walk objects whose length it cannot look up.
//
// File transfer is the reason it exists. A file command carries a name and a
// transport object carries a block of file data, so neither has a size the
// object table could state.

// FreeFormatQualifier is 0x5B: a two-octet size before each object, with a
// one-octet count of objects. It is the encoding IEEE 1815 specifies for group
// 70, and the only free-format combination devices use in practice.
var FreeFormatQualifier = MakeQualifier(PrefixSize2, RangeVariable)

// MaxFreeFormatObject is the largest single free-format object, bounded by the
// two-octet size prefix that introduces it.
const MaxFreeFormatObject = 0xFFFF

// FreeFormat builds an object header carrying one variable-length object.
//
// The size prefix goes in the data rather than being derived at encode time:
// [ObjectHeader.Data] is the octets exactly as they go on the wire, and having
// one place that decides what the header means is what keeps the encoder and
// the size walk in agreement.
func FreeFormat(group, variation uint8, object []byte) (ObjectHeader, error) {
	if len(object) > MaxFreeFormatObject {
		return ObjectHeader{}, fmt.Errorf(
			"app: a free-format object of %d octets exceeds the %d a size prefix can carry",
			len(object), MaxFreeFormatObject)
	}

	data := make([]byte, 0, 2+len(object))
	data = binary.LittleEndian.AppendUint16(data, uint16(len(object)))
	data = append(data, object...)

	return ObjectHeader{
		Group:     group,
		Variation: variation,
		Qualifier: FreeFormatQualifier,
		Range:     Range{Spec: RangeVariable, Count: 1},
		Data:      data,
	}, nil
}

// FreeFormatObjects returns the objects a free-format header carries, with
// their size prefixes stripped. The returned slices alias the header's data.
//
// It reports an error for a header that is not free format, so a caller cannot
// mistake a fixed-size encoding of the same group for one — the octets would
// decode into something that looked plausible and was not.
func FreeFormatObjects(h ObjectHeader) ([][]byte, error) {
	prefix := h.Qualifier.IndexPrefix()
	if !prefix.IsSize() {
		return nil, fmt.Errorf("app: g%dv%d arrived with qualifier %s, which is not free format",
			h.Group, h.Variation, h.Qualifier)
	}

	width := prefix.Octets()
	out := make([][]byte, 0, h.Range.Count)

	for off := 0; off < len(h.Data); {
		if off+width > len(h.Data) {
			return nil, fmt.Errorf("app: g%dv%d: a size prefix runs past the end of the header",
				h.Group, h.Variation)
		}
		size := int(readUintLE(h.Data[off:], width))
		off += width

		if off+size > len(h.Data) {
			return nil, fmt.Errorf("app: g%dv%d: an object of %d octets runs past the end of the header",
				h.Group, h.Variation, size)
		}
		out = append(out, h.Data[off:off+size])
		off += size
	}
	return out, nil
}

// FirstFreeFormatObject returns the single object a header carries, which is
// what every group 70 exchange in practice contains.
func FirstFreeFormatObject(h ObjectHeader) ([]byte, error) {
	objs, err := FreeFormatObjects(h)
	if err != nil {
		return nil, err
	}
	if len(objs) == 0 {
		return nil, fmt.Errorf("app: g%dv%d carries no object", h.Group, h.Variation)
	}
	return objs[0], nil
}
