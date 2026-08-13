package objects

import (
	"encoding/binary"
	"math"

	"github.com/dscsystems/go-dnp3"
)

// Command objects are hand-written because they do not decode into a
// measurement type. A CROB is five heterogeneous fields that mean something
// only together, and an analog output command puts its value before its status
// where every status object puts them the other way round. Generating these
// would mean teaching the generator about field roles it needs nowhere else.

// CROBSize is the encoded size of a group 12 variation 1 control relay output
// block: control code, count, on time, off time and status.
const CROBSize = 11

// ParseCROB decodes a control relay output block from buf, which must hold at
// least [CROBSize] octets.
func ParseCROB(buf []byte) dnp3.ControlRelayOutputBlock {
	_ = buf[CROBSize-1]
	return dnp3.ControlRelayOutputBlock{
		Code:    dnp3.ControlCode(buf[0]),
		Count:   buf[1],
		OnTime:  binary.LittleEndian.Uint32(buf[2:6]),
		OffTime: binary.LittleEndian.Uint32(buf[6:10]),
		Status:  dnp3.CommandStatus(buf[10]),
	}
}

// AppendCROB encodes a control relay output block.
func AppendCROB(dst []byte, c dnp3.ControlRelayOutputBlock) []byte {
	dst = append(dst, byte(c.Code), c.Count)
	dst = binary.LittleEndian.AppendUint32(dst, c.OnTime)
	dst = binary.LittleEndian.AppendUint32(dst, c.OffTime)
	return append(dst, byte(c.Status))
}

// Encoded sizes of the four analog output command variations.
const (
	AnalogOutput32Size    = 5 // group 41 variation 1
	AnalogOutput16Size    = 3 // group 41 variation 2
	AnalogOutputFloatSize = 5 // group 41 variation 3
	AnalogOutputDblSize   = 9 // group 41 variation 4
)

// ParseAnalogOutputInt32 decodes a group 41 variation 1 command.
func ParseAnalogOutputInt32(buf []byte) dnp3.AnalogOutputInt32 {
	_ = buf[AnalogOutput32Size-1]
	return dnp3.AnalogOutputInt32{
		Value:  int32(binary.LittleEndian.Uint32(buf[0:4])),
		Status: dnp3.CommandStatus(buf[4]),
	}
}

// AppendAnalogOutputInt32 encodes a group 41 variation 1 command.
func AppendAnalogOutputInt32(dst []byte, v dnp3.AnalogOutputInt32) []byte {
	dst = binary.LittleEndian.AppendUint32(dst, uint32(v.Value))
	return append(dst, byte(v.Status))
}

// ParseAnalogOutputInt16 decodes a group 41 variation 2 command.
func ParseAnalogOutputInt16(buf []byte) dnp3.AnalogOutputInt16 {
	_ = buf[AnalogOutput16Size-1]
	return dnp3.AnalogOutputInt16{
		Value:  int16(binary.LittleEndian.Uint16(buf[0:2])),
		Status: dnp3.CommandStatus(buf[2]),
	}
}

// AppendAnalogOutputInt16 encodes a group 41 variation 2 command.
func AppendAnalogOutputInt16(dst []byte, v dnp3.AnalogOutputInt16) []byte {
	dst = binary.LittleEndian.AppendUint16(dst, uint16(v.Value))
	return append(dst, byte(v.Status))
}

// ParseAnalogOutputFloat32 decodes a group 41 variation 3 command.
func ParseAnalogOutputFloat32(buf []byte) dnp3.AnalogOutputFloat32 {
	_ = buf[AnalogOutputFloatSize-1]
	return dnp3.AnalogOutputFloat32{
		Value:  math.Float32frombits(binary.LittleEndian.Uint32(buf[0:4])),
		Status: dnp3.CommandStatus(buf[4]),
	}
}

// AppendAnalogOutputFloat32 encodes a group 41 variation 3 command.
func AppendAnalogOutputFloat32(dst []byte, v dnp3.AnalogOutputFloat32) []byte {
	dst = binary.LittleEndian.AppendUint32(dst, math.Float32bits(v.Value))
	return append(dst, byte(v.Status))
}

// ParseAnalogOutputFloat64 decodes a group 41 variation 4 command.
func ParseAnalogOutputFloat64(buf []byte) dnp3.AnalogOutputFloat64 {
	_ = buf[AnalogOutputDblSize-1]
	return dnp3.AnalogOutputFloat64{
		Value:  math.Float64frombits(binary.LittleEndian.Uint64(buf[0:8])),
		Status: dnp3.CommandStatus(buf[8]),
	}
}

// AppendAnalogOutputFloat64 encodes a group 41 variation 4 command.
func AppendAnalogOutputFloat64(dst []byte, v dnp3.AnalogOutputFloat64) []byte {
	dst = binary.LittleEndian.AppendUint64(dst, math.Float64bits(v.Value))
	return append(dst, byte(v.Status))
}

// ---------- Time objects ----------

// Time48Size is the encoded size of an absolute DNP3 timestamp.
const Time48Size = 6

// ParseTime48 decodes a 48-bit absolute timestamp, as carried by group 50
// variation 1 and the group 51 common-time-of-occurrence objects.
func ParseTime48(buf []byte) dnp3.Timestamp {
	_ = buf[Time48Size-1]
	return dnp3.Timestamp{
		Time:    dnp3.DNP3ToTime(readTime48(buf)),
		Quality: dnp3.TimestampSynchronized,
	}
}

// AppendTime48 encodes a 48-bit absolute timestamp.
func AppendTime48(dst []byte, t dnp3.Timestamp) []byte {
	return appendTime48(dst, dnp3.TimeToDNP3(t.Time))
}

// ParseTimeDelay decodes a group 52 time delay, returning milliseconds.
//
// Variation 1 is coarse and counts seconds; variation 2 is fine and counts
// milliseconds. Both are returned in milliseconds so callers need not care,
// which is the whole reason the two variations exist separately on the wire.
func ParseTimeDelay(variation uint8, buf []byte) uint32 {
	_ = buf[1]
	v := uint32(binary.LittleEndian.Uint16(buf[0:2]))
	if variation == 1 {
		return v * 1000
	}
	return v
}
