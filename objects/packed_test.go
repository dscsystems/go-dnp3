package objects

import (
	"bytes"
	"math"
	"testing"

	"github.com/dscsystems/go-dnp3"
)

func TestPackedOctets(t *testing.T) {
	tests := []struct {
		count, bits, want int
	}{
		{0, 1, 0},
		{1, 1, 1},
		{8, 1, 1},
		{9, 1, 2},
		{10, 1, 2}, // the case from the architecture: ten points in two octets
		{1, 2, 1},
		{4, 2, 1},
		{5, 2, 2},
	}
	for _, tc := range tests {
		if got := PackedOctets(tc.count, tc.bits); got != tc.want {
			t.Errorf("PackedOctets(%d, %d) = %d, want %d", tc.count, tc.bits, got, tc.want)
		}
	}
}

func TestPackedBinaryRoundTrip(t *testing.T) {
	values := []bool{true, false, true, false, true, false, true, false, true, true}

	buf := AppendPackedBinary(nil, values)
	if len(buf) != 2 {
		t.Fatalf("ten one-bit points packed into %d octets, want 2", len(buf))
	}
	// Least significant bit first: 0b01010101 then 0b00000011.
	if !bytes.Equal(buf, []byte{0x55, 0x03}) {
		t.Errorf("packed = % x, want 55 03", buf)
	}

	got := ParsePackedBinary(buf, len(values), nil)
	if len(got) != len(values) {
		t.Fatalf("parsed %d values, want %d", len(got), len(values))
	}
	for i, want := range values {
		if got[i].Value != want {
			t.Errorf("bit %d = %v, want %v", i, got[i].Value, want)
		}
		if got[i].Flags != dnp3.Online {
			t.Errorf("bit %d flags = %v, want ONLINE", i, got[i].Flags)
		}
	}
}

func TestPackedDoubleBitRoundTrip(t *testing.T) {
	values := []dnp3.DoubleBit{
		dnp3.DoubleBitDeterminedOn,
		dnp3.DoubleBitDeterminedOff,
		dnp3.DoubleBitIntermediate,
		dnp3.DoubleBitIndeterminate,
		dnp3.DoubleBitDeterminedOn,
	}

	buf := AppendPackedDoubleBit(nil, values)
	if len(buf) != 2 {
		t.Fatalf("five two-bit points packed into %d octets, want 2", len(buf))
	}

	got := ParsePackedDoubleBit(buf, len(values), nil)
	for i, want := range values {
		if got[i].Value != want {
			t.Errorf("pair %d = %v, want %v", i, got[i].Value, want)
		}
	}
}

func TestPackedBinaryOutputRoundTrip(t *testing.T) {
	values := []bool{false, true, true}
	buf := AppendPackedBinary(nil, values)
	got := ParsePackedBinaryOutput(buf, len(values), nil)
	for i, want := range values {
		if got[i].Value != want {
			t.Errorf("bit %d = %v, want %v", i, got[i].Value, want)
		}
	}
}

// TestPackedParseToleratesShortBuffer covers the case where the object count
// and the octets available disagree.
//
// They come from different fields of a header a peer controls, and nothing
// guarantees they agree. Reading past the buffer must yield false rather than
// panic — the framing layer already rejects the header, but the codec must not
// be the thing that crashes.
func TestPackedParseToleratesShortBuffer(t *testing.T) {
	got := ParsePackedBinary([]byte{0xFF}, 64, nil)
	if len(got) != 64 {
		t.Fatalf("parsed %d values, want 64", len(got))
	}
	for i := 8; i < 64; i++ {
		if got[i].Value {
			t.Fatalf("bit %d read past the buffer as true", i)
		}
	}

	db := ParsePackedDoubleBit([]byte{0xFF}, 32, nil)
	if len(db) != 32 {
		t.Fatalf("parsed %d pairs, want 32", len(db))
	}
}

func TestPackedEmpty(t *testing.T) {
	if got := AppendPackedBinary(nil, nil); got != nil {
		t.Errorf("packing no values produced % x", got)
	}
	if got := ParsePackedBinary(nil, 0, nil); len(got) != 0 {
		t.Errorf("parsing zero values produced %d", len(got))
	}
}

// ---------- Commands ----------

func TestCROBRoundTrip(t *testing.T) {
	want := dnp3.ControlRelayOutputBlock{
		Code:    dnp3.ControlPulseOn | dnp3.ControlTrip,
		Count:   1,
		OnTime:  1000,
		OffTime: 500,
		Status:  dnp3.CommandSuccess,
	}

	buf := AppendCROB(nil, want)
	if len(buf) != CROBSize {
		t.Fatalf("encoded %d octets, want %d", len(buf), CROBSize)
	}
	// Control code, count, on time, off time, status — all little-endian.
	golden := []byte{0x41, 0x01, 0xE8, 0x03, 0x00, 0x00, 0xF4, 0x01, 0x00, 0x00, 0x00}
	if !bytes.Equal(buf, golden) {
		t.Errorf("encoded % x\nwant     % x", buf, golden)
	}

	got := ParseCROB(buf)
	if got != want {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}
	if !got.Code.IsTrip() {
		t.Error("the trip coil selection did not survive")
	}
}

func TestAnalogOutputCommandRoundTrips(t *testing.T) {
	t.Run("int32", func(t *testing.T) {
		want := dnp3.AnalogOutputInt32{Value: -70000, Status: dnp3.CommandSuccess}
		buf := AppendAnalogOutputInt32(nil, want)
		if len(buf) != AnalogOutput32Size {
			t.Fatalf("encoded %d octets, want %d", len(buf), AnalogOutput32Size)
		}
		if got := ParseAnalogOutputInt32(buf); got != want {
			t.Errorf("got %+v, want %+v", got, want)
		}
	})

	t.Run("int16", func(t *testing.T) {
		want := dnp3.AnalogOutputInt16{Value: -300, Status: dnp3.CommandNotSupported}
		buf := AppendAnalogOutputInt16(nil, want)
		if len(buf) != AnalogOutput16Size {
			t.Fatalf("encoded %d octets, want %d", len(buf), AnalogOutput16Size)
		}
		if got := ParseAnalogOutputInt16(buf); got != want {
			t.Errorf("got %+v, want %+v", got, want)
		}
	})

	t.Run("float32", func(t *testing.T) {
		want := dnp3.AnalogOutputFloat32{Value: 13.75, Status: dnp3.CommandSuccess}
		buf := AppendAnalogOutputFloat32(nil, want)
		if len(buf) != AnalogOutputFloatSize {
			t.Fatalf("encoded %d octets, want %d", len(buf), AnalogOutputFloatSize)
		}
		if got := ParseAnalogOutputFloat32(buf); got != want {
			t.Errorf("got %+v, want %+v", got, want)
		}
	})

	t.Run("float64", func(t *testing.T) {
		want := dnp3.AnalogOutputFloat64{Value: 13.7521, Status: dnp3.CommandSuccess}
		buf := AppendAnalogOutputFloat64(nil, want)
		if len(buf) != AnalogOutputDblSize {
			t.Fatalf("encoded %d octets, want %d", len(buf), AnalogOutputDblSize)
		}
		if got := ParseAnalogOutputFloat64(buf); got != want {
			t.Errorf("got %+v, want %+v", got, want)
		}
	})
}

// TestAnalogOutputFieldOrder pins the ordering that differs from group 40.
//
// An analog output *command* puts its value first and its status last; an
// analog output *status* object puts its flags first. Reading one with the
// other's layout gives a plausible-looking wrong number.
func TestAnalogOutputFieldOrder(t *testing.T) {
	buf := AppendAnalogOutputInt16(nil, dnp3.AnalogOutputInt16{Value: 300, Status: dnp3.CommandSuccess})
	if !bytes.Equal(buf, []byte{0x2C, 0x01, 0x00}) {
		t.Errorf("g41v2 = % x, want 2c 01 00 (value then status)", buf)
	}

	// The status object is the other way round: flags, then value.
	st := analogoutputCodecs[GV(40, 2)].Write(nil,
		dnp3.AnalogOutputStatus{Value: 300, Flags: dnp3.Online}, Context{})
	if !bytes.Equal(st, []byte{0x01, 0x2C, 0x01}) {
		t.Errorf("g40v2 = % x, want 01 2c 01 (flags then value)", st)
	}
}

func TestTime48RoundTrip(t *testing.T) {
	ts := dnp3.Timestamp{Time: testTime, Quality: dnp3.TimestampSynchronized}
	buf := AppendTime48(nil, ts)
	if len(buf) != Time48Size {
		t.Fatalf("encoded %d octets, want %d", len(buf), Time48Size)
	}
	if got := ParseTime48(buf); !got.Time.Equal(ts.Time) {
		t.Errorf("time = %v, want %v", got.Time, ts.Time)
	}
}

func TestParseTimeDelay(t *testing.T) {
	// Variation 1 counts seconds, variation 2 counts milliseconds. Both come
	// back in milliseconds so callers need not care.
	if got := ParseTimeDelay(1, []byte{0x02, 0x00}); got != 2000 {
		t.Errorf("coarse delay = %d ms, want 2000", got)
	}
	if got := ParseTimeDelay(2, []byte{0xE8, 0x03}); got != 1000 {
		t.Errorf("fine delay = %d ms, want 1000", got)
	}
}

// ---------- Clamping ----------

// TestClampSaturatesRatherThanWrapping covers the conversion Go leaves
// undefined. On amd64 a bare int16(40000.0) yields -32768: a reading at the
// opposite end of the scale, indistinguishable from a real one.
func TestClampSaturatesRatherThanWrapping(t *testing.T) {
	tests := []struct {
		name string
		in   float64
		want int16
	}{
		{"in range", 300, 300},
		{"at the maximum", 32767, 32767},
		{"over the maximum", 40000, 32767},
		{"far over", 1e12, 32767},
		{"at the minimum", -32768, -32768},
		{"under the minimum", -40000, -32768},
		{"positive infinity", math.Inf(1), 32767},
		{"negative infinity", math.Inf(-1), -32768},
		{"not a number", math.NaN(), 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := clampInt16(tc.in); got != tc.want {
				t.Errorf("clampInt16(%v) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}

	if got := clampInt32(1e12); got != math.MaxInt32 {
		t.Errorf("clampInt32(1e12) = %d, want %d", got, int32(math.MaxInt32))
	}
	if got := clampUint16(-5); got != 0 {
		t.Errorf("clampUint16(-5) = %d, want 0", got)
	}
	if got := clampUint32(1e12); got != math.MaxUint32 {
		t.Errorf("clampUint32(1e12) = %d, want %d", got, uint32(math.MaxUint32))
	}
	if got := clampUint32(math.NaN()); got != 0 {
		t.Errorf("clampUint32(NaN) = %d, want 0", got)
	}
}

// TestOverRangeAnalogSaturates is the clamp reaching the wire: a 40000 reading
// on a 16-bit point encodes as the maximum, not as a negative number.
func TestOverRangeAnalogSaturates(t *testing.T) {
	buf := analogCodecs[GV(30, 2)].Write(nil,
		dnp3.Analog{Value: 40000, Flags: dnp3.Online | dnp3.OverRange}, Context{})

	got := analogCodecs[GV(30, 2)].Parse(buf, Context{})
	if got.Value != 32767 {
		t.Errorf("value = %v, want 32767 — an over-range reading must saturate, not wrap", got.Value)
	}
	if !got.Flags.HasAny(dnp3.OverRange) {
		t.Error("the OVER_RANGE quality bit should survive to say why")
	}
}
