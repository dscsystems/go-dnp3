package objects

import "github.com/dscsystems/go-dnp3"

// Bit-packed objects share octets across a whole range, so they have no
// per-object codec: the unit of encoding is the range, not the object. Group 1
// variation 1 puts ten binary inputs in two octets rather than ten.
//
// These are hand-written because the generator's model is one object at a
// time, and forcing a range-shaped encoding through it would complicate every
// other variation to serve five.

// PackedOctets returns how many octets a range of count objects occupies at
// bitsPerObject bits each.
func PackedOctets(count int, bitsPerObject int) int {
	if count <= 0 || bitsPerObject <= 0 {
		return 0
	}
	return (count*bitsPerObject + 7) / 8
}

// ParsePackedBinary decodes count single-bit binary values from buf.
//
// It is used for group 1 variation 1 (binary inputs), group 10 variation 1
// (binary output status) and group 80 variation 1 (internal indications),
// which share an encoding.
//
// Packed variations carry no quality information, so every value comes back
// online — the encoding has nowhere to say otherwise.
func ParsePackedBinary(buf []byte, count int, out []dnp3.Binary) []dnp3.Binary {
	for i := range count {
		out = append(out, dnp3.Binary{
			Value: bitAt(buf, i),
			Flags: dnp3.Online,
		})
	}
	return out
}

// ParsePackedBinaryOutput decodes count single-bit binary output statuses.
func ParsePackedBinaryOutput(buf []byte, count int, out []dnp3.BinaryOutputStatus) []dnp3.BinaryOutputStatus {
	for i := range count {
		out = append(out, dnp3.BinaryOutputStatus{
			Value: bitAt(buf, i),
			Flags: dnp3.Online,
		})
	}
	return out
}

// ParsePackedDoubleBit decodes count two-bit double-bit binary values, as
// group 3 variation 1 encodes them.
func ParsePackedDoubleBit(buf []byte, count int, out []dnp3.DoubleBitBinary) []dnp3.DoubleBitBinary {
	for i := range count {
		pair := i * 2
		idx, shift := pair/8, uint(pair%8)
		var v dnp3.DoubleBit
		if idx < len(buf) {
			v = dnp3.DoubleBit((buf[idx] >> shift) & 0x03)
		}
		out = append(out, dnp3.DoubleBitBinary{Value: v, Flags: dnp3.Online})
	}
	return out
}

// AppendPackedBinary encodes values as single bits, least significant bit
// first, padding the final octet with zeros.
func AppendPackedBinary(dst []byte, values []bool) []byte {
	if len(values) == 0 {
		return dst
	}
	start := len(dst)
	dst = append(dst, make([]byte, PackedOctets(len(values), 1))...)
	for i, v := range values {
		if v {
			dst[start+i/8] |= 1 << uint(i%8)
		}
	}
	return dst
}

// AppendPackedDoubleBit encodes values as two-bit pairs, least significant
// pair first.
func AppendPackedDoubleBit(dst []byte, values []dnp3.DoubleBit) []byte {
	if len(values) == 0 {
		return dst
	}
	start := len(dst)
	dst = append(dst, make([]byte, PackedOctets(len(values), 2))...)
	for i, v := range values {
		pair := i * 2
		dst[start+pair/8] |= byte(v&0x03) << uint(pair%8)
	}
	return dst
}

// bitAt reads the i'th bit, least significant bit of the first octet first.
// Reading past the buffer yields false rather than panicking, because the
// count and the buffer come from different fields of an attacker-controlled
// header and are not guaranteed to agree.
func bitAt(buf []byte, i int) bool {
	idx, shift := i/8, uint(i%8)
	if idx >= len(buf) {
		return false
	}
	return buf[idx]&(1<<shift) != 0
}
