package objects

import "math"

// Helpers the generated codecs call. They live here rather than in the
// generated file so their behaviour can be reasoned about and tested directly.

// appendTime48 appends a 48-bit little-endian DNP3 timestamp.
//
// encoding/binary has no six-octet helper, and DNP3 uses that width
// everywhere it carries a time.
func appendTime48(dst []byte, ms uint64) []byte {
	return append(dst,
		byte(ms), byte(ms>>8), byte(ms>>16),
		byte(ms>>24), byte(ms>>32), byte(ms>>40))
}

// readTime48 decodes a 48-bit little-endian DNP3 timestamp.
func readTime48(buf []byte) uint64 {
	return uint64(buf[0]) | uint64(buf[1])<<8 | uint64(buf[2])<<16 |
		uint64(buf[3])<<24 | uint64(buf[4])<<32 | uint64(buf[5])<<40
}

// The clamp helpers exist because converting an out-of-range float64 to an
// integer type is not defined in Go — the result is
// implementation-dependent, and on amd64 it is the minimum value of the type.
//
// That matters here. An analog point configured as 16-bit whose reading drifts
// past 32767 would encode as -32768: a value at the opposite end of the scale,
// indistinguishable from a real reading. Saturating is not perfect either, but
// a pegged reading is recognisable as a pegged reading, and the OVER_RANGE
// quality bit is there to say so.

// clampInt16 converts to int16, saturating rather than wrapping.
func clampInt16(v float64) int16 {
	switch {
	case math.IsNaN(v):
		return 0
	case v >= math.MaxInt16:
		return math.MaxInt16
	case v <= math.MinInt16:
		return math.MinInt16
	default:
		return int16(v)
	}
}

// clampInt32 converts to int32, saturating rather than wrapping.
func clampInt32(v float64) int32 {
	switch {
	case math.IsNaN(v):
		return 0
	case v >= math.MaxInt32:
		return math.MaxInt32
	case v <= math.MinInt32:
		return math.MinInt32
	default:
		return int32(v)
	}
}

// clampUint16 converts to uint16, saturating rather than wrapping.
func clampUint16(v float64) uint16 {
	switch {
	case math.IsNaN(v) || v <= 0:
		return 0
	case v >= math.MaxUint16:
		return math.MaxUint16
	default:
		return uint16(v)
	}
}

// clampUint32 converts to uint32, saturating rather than wrapping.
func clampUint32(v float64) uint32 {
	switch {
	case math.IsNaN(v) || v <= 0:
		return 0
	case v >= math.MaxUint32:
		return math.MaxUint32
	default:
		return uint32(v)
	}
}
