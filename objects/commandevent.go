package objects

import (
	"encoding/binary"
	"math"

	"github.com/dscsystems/go-dnp3"
)

// Command events record a control that was operated: group 13 for a binary
// output and group 43 for an analog output. Both begin with a status octet.

// CommandEventSize returns the size in octets of a group 13 or 43 variation,
// and whether it is one.
func CommandEventSize(group, variation uint8) (int, bool) {
	switch group {
	case 13:
		switch variation {
		case 1:
			return 1, true
		case 2:
			return 1 + Time48Size, true
		}
	case 43:
		switch variation {
		case 1, 5:
			return 5, true
		case 2:
			return 3, true
		case 3, 7:
			return 5 + Time48Size, true
		case 4:
			return 3 + Time48Size, true
		case 6:
			return 9, true
		case 8:
			return 9 + Time48Size, true
		}
	}
	return 0, false
}

func commandEventHasTime(group, variation uint8) bool {
	if group == 13 {
		return variation == 2
	}
	switch variation {
	case 3, 4, 7, 8:
		return true
	}
	return false
}

// AppendCommandEvent appends one group 13 or 43 object. It appends nothing for
// a variation that does not exist.
//
// A binary event packs the status into the low seven bits of one octet and the
// commanded state into the top one. An analog event carries the status octet
// and then the value at the variation's width; an integer variation saturates
// rather than wrapping.
func AppendCommandEvent(dst []byte, group, variation uint8, e dnp3.CommandEvent) []byte {
	if _, ok := CommandEventSize(group, variation); !ok {
		return dst
	}

	if group == 13 {
		b := byte(e.Status) & 0x7F
		if e.State {
			b |= 0x80
		}
		dst = append(dst, b)
	} else {
		dst = append(dst, byte(e.Status))
		switch variation {
		case 1, 3:
			dst = binary.LittleEndian.AppendUint32(dst, uint32(saturate(e.Value, math.MinInt32, math.MaxInt32)))
		case 2, 4:
			dst = binary.LittleEndian.AppendUint16(dst, uint16(saturate(e.Value, math.MinInt16, math.MaxInt16)))
		case 5, 7:
			dst = binary.LittleEndian.AppendUint32(dst, math.Float32bits(float32(e.Value)))
		case 6, 8:
			dst = binary.LittleEndian.AppendUint64(dst, math.Float64bits(e.Value))
		}
	}
	if commandEventHasTime(group, variation) {
		dst = AppendTime48(dst, e.Time)
	}
	return dst
}

func saturate(v float64, lo, hi int64) int64 {
	switch {
	case v != v:
		return 0
	case v >= float64(hi):
		return hi
	case v <= float64(lo):
		return lo
	}
	return int64(v)
}

// ParseCommandEvent decodes one group 13 or 43 object. It reports false if the
// variation does not exist or buf is too short.
func ParseCommandEvent(group, variation uint8, buf []byte) (dnp3.CommandEvent, bool) {
	size, ok := CommandEventSize(group, variation)
	if !ok || len(buf) < size {
		return dnp3.CommandEvent{}, false
	}

	var e dnp3.CommandEvent
	if group == 13 {
		e.Status = dnp3.CommandStatus(buf[0] & 0x7F)
		e.State = buf[0]&0x80 != 0
	} else {
		e.Analog = true
		e.Status = dnp3.CommandStatus(buf[0])
		switch variation {
		case 1, 3:
			e.Value = float64(int32(binary.LittleEndian.Uint32(buf[1:])))
		case 2, 4:
			e.Value = float64(int16(binary.LittleEndian.Uint16(buf[1:])))
		case 5, 7:
			e.Value = float64(math.Float32frombits(binary.LittleEndian.Uint32(buf[1:])))
		case 6, 8:
			e.Value = math.Float64frombits(binary.LittleEndian.Uint64(buf[1:]))
		}
	}
	if commandEventHasTime(group, variation) {
		e.Time = ParseTime48(buf[size-Time48Size:])
	}
	return e, true
}
