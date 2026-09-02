package app

import (
	"encoding/binary"
	"fmt"
)

// ObjectHeaderSize is the fixed part of an object header: group, variation
// and qualifier.
const ObjectHeaderSize = 3

// ObjectSizer resolves how large one object of a group and variation is.
//
// The application layer cannot walk a fragment without this: the octets
// following an object header are only delimited by the size implied by the
// group and variation. Keeping it an interface is what lets this package stay
// independent of the object codecs — in a full stack the generated object
// registry supplies it.
type ObjectSizer interface {
	// SizeBits returns the encoded size of a single object in bits.
	//
	// Bits rather than octets because several groups are bit-packed: a group 1
	// variation 1 binary input occupies one bit, and a range of them shares
	// octets. A size of zero means the object carries no data at all, as with
	// the class objects of group 60.
	//
	// ok is false when the group and variation are unknown or when the object
	// is variable-length, in which case the encoding must make its size
	// self-describing through a size prefix.
	SizeBits(group, variation uint8) (bits int, ok bool)
}

// ObjectHeader is one decoded object header together with the raw octets of
// the objects it introduces.
type ObjectHeader struct {
	Group     uint8
	Variation uint8
	Qualifier Qualifier
	Range     Range

	// Data is the object data this header introduces, aliasing the fragment
	// it was parsed from. It excludes the header itself but includes any
	// per-object index or size prefixes.
	Data []byte

	// Offset is where this header begins in the fragment, and DataOffset
	// where its data begins. Decoders and hex viewers need both to point at
	// the octets they are describing.
	Offset     int
	DataOffset int
}

// GroupVar returns the group and variation as a single value, which is how
// object registries are keyed.
func (h ObjectHeader) GroupVar() uint16 {
	return uint16(h.Group)<<8 | uint16(h.Variation)
}

// Count returns the number of objects the header describes.
func (h ObjectHeader) Count() uint32 { return h.Range.Count }

// Size returns the total octets the header and its data occupy.
func (h ObjectHeader) Size() int {
	return ObjectHeaderSize + h.Qualifier.RangeSpec().Octets() + len(h.Data)
}

func (h ObjectHeader) String() string {
	return fmt.Sprintf("g%dv%d %s %s data=%dB",
		h.Group, h.Variation, h.Qualifier, h.Range, len(h.Data))
}

// readUintLE reads a little-endian unsigned integer of width octets.
func readUintLE(buf []byte, width int) uint32 {
	switch width {
	case 1:
		return uint32(buf[0])
	case 2:
		return uint32(binary.LittleEndian.Uint16(buf))
	case 4:
		return binary.LittleEndian.Uint32(buf)
	default:
		return 0
	}
}

// objectDataLen returns how many octets of object data follow a header.
//
// buf must begin at the first octet of the object data. The result is the
// count of octets those objects occupy, which the caller uses to advance to
// the next header.
//
// carriesData comes from the fragment's function code — see
// [FuncCode.CarriesObjectData]. In a read request the header names points
// rather than carrying them, and no amount of inspecting the header itself
// reveals that.
func objectDataLen(sizer ObjectSizer, h ObjectHeader, buf []byte, carriesData bool) (int, error) {
	prefix := h.Qualifier.IndexPrefix()

	// A size prefix makes the data self-describing, so it can be walked
	// without knowing anything about the group — and, unlike everything below,
	// without knowing the function code either.
	//
	// That exception is what file transfer needs. A READ request carries no
	// object data by the general rule, but a read of a file block carries a
	// group 70 object holding the handle and the block number; so do the
	// delete and file-info requests. The object says how long it is, so there
	// is nothing to infer and nothing to get wrong.
	if prefix.IsSize() && h.Range.Count > 0 && h.Variation != 0 {
		return walkSizePrefixed(prefix.Octets(), h.Range.Count, buf)
	}

	if !carriesData {
		return 0, nil
	}
	// Variation zero means "whatever variation you use by default". It appears
	// only in requests and never carries data.
	if h.Variation == 0 {
		return 0, nil
	}
	// "All objects" has no count on the wire, so it cannot introduce data.
	// It is how a class poll asks for everything.
	if h.Range.Spec == RangeAllObjects {
		return 0, nil
	}
	if h.Range.Count == 0 {
		return 0, nil
	}

	// A group 0 device attribute carries its own data type and length, so it
	// is walkable without a size table — which is just as well, because its
	// variation is not an encoding at all but the number of the attribute
	// being reported, and no table could enumerate what a device might say
	// about itself.
	//
	// Unlike the free-format case above this does depend on the function code:
	// in a read request the same header names an attribute and stops there.
	if h.Group == 0 {
		return walkAttributes(h.Range.Count, buf)
	}

	bits, ok := sizer.SizeBits(h.Group, h.Variation)
	if !ok {
		return 0, fmt.Errorf("%w: g%dv%d", ErrUnknownObject, h.Group, h.Variation)
	}
	if bits == 0 {
		return 0, nil
	}

	prefixOctets := prefix.Octets()

	if bits < 8 {
		// Bit-packed objects share octets across the whole range, so a
		// per-object index prefix cannot be expressed alongside them.
		if prefixOctets != 0 {
			return 0, fmt.Errorf("%w: g%dv%d is bit-packed but the qualifier carries a %s prefix",
				ErrBadQualifier, h.Group, h.Variation, prefix)
		}
		total := (uint64(h.Range.Count)*uint64(bits) + 7) / 8
		return checkFits(total, buf)
	}

	if bits%8 != 0 {
		return 0, fmt.Errorf("%w: g%dv%d is %d bits, neither packed nor a whole octet count",
			ErrUnknownObject, h.Group, h.Variation, bits)
	}

	total := uint64(h.Range.Count) * (uint64(prefixOctets) + uint64(bits)/8)
	return checkFits(total, buf)
}

// walkAttributes advances over count device attributes, each of which is a
// one-octet data type, a one-octet length, and that many octets of value.
func walkAttributes(count uint32, buf []byte) (int, error) {
	const header = 2 // the data type and the length
	off := 0
	for range count {
		if off+header > len(buf) {
			return 0, ErrTruncated
		}
		size := int(buf[off+1])
		if off+header+size > len(buf) {
			return 0, ErrTruncated
		}
		off += header + size
	}
	return off, nil
}

// walkSizePrefixed advances over count objects, each introduced by its own
// size field of width octets.
func walkSizePrefixed(width int, count uint32, buf []byte) (int, error) {
	off := 0
	for range count {
		if off+width > len(buf) {
			return 0, ErrTruncated
		}
		size := readUintLE(buf[off:], width)
		off += width
		if uint64(off)+uint64(size) > uint64(len(buf)) {
			return 0, ErrTruncated
		}
		off += int(size)
	}
	return off, nil
}

// checkFits converts a computed length to an int after confirming the buffer
// actually holds it. The comparison happens in 64 bits because a 32-bit count
// multiplied by an object size overflows an int on a 32-bit platform, and an
// overflowed length would index past the fragment.
func checkFits(total uint64, buf []byte) (int, error) {
	if total > uint64(len(buf)) {
		return 0, ErrTruncated
	}
	return int(total), nil
}

// ParseObjectHeader decodes one object header and its data from the front of
// buf, which must begin at the group octet.
//
// offset is the position of buf within the enclosing fragment and is recorded
// on the result so decoders can point at the original octets. carriesData says
// whether object data follows the header, which depends on the enclosing
// fragment's function code — see [FuncCode.CarriesObjectData].
func ParseObjectHeader(sizer ObjectSizer, buf []byte, offset int, carriesData bool) (ObjectHeader, int, error) {
	if sizer == nil {
		sizer = DefaultSizer
	}
	if len(buf) < ObjectHeaderSize {
		return ObjectHeader{}, 0, ErrTruncated
	}

	h := ObjectHeader{
		Group:     buf[0],
		Variation: buf[1],
		Qualifier: Qualifier(buf[2]),
		Offset:    offset,
	}

	q := h.Qualifier
	if q.Reserved() {
		return ObjectHeader{}, 0, fmt.Errorf("%w: reserved bit set in %#02x", ErrBadQualifier, uint8(q))
	}
	if !q.IndexPrefix().Valid() {
		return ObjectHeader{}, 0, fmt.Errorf("%w: index prefix %#x", ErrBadQualifier, uint8(q.IndexPrefix()))
	}
	if !q.RangeSpec().Valid() {
		return ObjectHeader{}, 0, fmt.Errorf("%w: range specifier %#x", ErrBadQualifier, uint8(q.RangeSpec()))
	}

	rng, rangeLen, err := parseRange(q.RangeSpec(), buf[ObjectHeaderSize:])
	if err != nil {
		return ObjectHeader{}, 0, err
	}
	h.Range = rng

	dataStart := ObjectHeaderSize + rangeLen
	h.DataOffset = offset + dataStart

	dataLen, err := objectDataLen(sizer, h, buf[dataStart:], carriesData)
	if err != nil {
		return ObjectHeader{}, 0, err
	}
	h.Data = buf[dataStart : dataStart+dataLen]

	return h, dataStart + dataLen, nil
}

// AppendObjectHeader appends an object header and its data to dst.
//
// The caller is responsible for data being consistent with the group,
// variation, qualifier and range — this function does not re-derive the size.
func AppendObjectHeader(dst []byte, h ObjectHeader) []byte {
	dst = append(dst, h.Group, h.Variation, byte(h.Qualifier))
	dst = appendRange(dst, h.Range)
	return append(dst, h.Data...)
}
