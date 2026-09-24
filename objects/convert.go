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
// indistinguishable from a real reading. Saturating is not enough on its own
// either, because a pegged 32767 looks exactly like a real 32767. So each
// helper also reports whether the value fell outside what the type can hold,
// and the generated writers set OVER_RANGE when it did — which is what the
// flag exists for: the value exceeds the range of the variation reported.
//
// NaN counts as out of range. It has no integer representation at all, and
// reporting it as a plain zero would pass a failed reading off as a real one.

// clampInt16 converts to int16, saturating rather than wrapping, and reports
// whether v was outside the int16 range.
func clampInt16(v float64) (int16, bool) {
	switch {
	case math.IsNaN(v):
		return 0, true
	case v > math.MaxInt16:
		return math.MaxInt16, true
	case v < math.MinInt16:
		return math.MinInt16, true
	default:
		return int16(v), false
	}
}

// clampInt32 converts to int32, saturating rather than wrapping, and reports
// whether v was outside the int32 range.
func clampInt32(v float64) (int32, bool) {
	switch {
	case math.IsNaN(v):
		return 0, true
	case v > math.MaxInt32:
		return math.MaxInt32, true
	case v < math.MinInt32:
		return math.MinInt32, true
	default:
		return int32(v), false
	}
}

// clampUint16 converts to uint16, saturating rather than wrapping, and reports
// whether v was outside the uint16 range.
func clampUint16(v float64) (uint16, bool) {
	switch {
	case math.IsNaN(v):
		return 0, true
	case v < 0:
		return 0, true
	case v > math.MaxUint16:
		return math.MaxUint16, true
	default:
		return uint16(v), false
	}
}

// clampUint32 converts to uint32, saturating rather than wrapping, and reports
// whether v was outside the uint32 range.
func clampUint32(v float64) (uint32, bool) {
	switch {
	case math.IsNaN(v):
		return 0, true
	case v < 0:
		return 0, true
	case v > math.MaxUint32:
		return math.MaxUint32, true
	default:
		return uint32(v), false
	}
}
