package objects

import (
	"bytes"
	"testing"

	"github.com/dscsystems/go-dnp3"
)

// FuzzCodecs feeds arbitrary octets to every generated codec.
//
// The framing layer guarantees a codec is handed at least its object's size,
// but nothing guarantees the *contents*: every octet inside comes from a peer.
// A codec must never panic on any bit pattern, and whatever it decodes must
// re-encode to the same octets — a decoder that accepts an encoding its own
// writer could not produce has a field offset wrong.
func FuzzCodecs(f *testing.F) {
	f.Add(byte(1), byte(2), []byte{0x81})
	f.Add(byte(30), byte(2), []byte{0x01, 0x2C, 0x01})
	f.Add(byte(32), byte(3), []byte{0x01, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	f.Add(byte(2), byte(3), []byte{0x81, 0xE8, 0x03})
	f.Add(byte(30), byte(6), bytes.Repeat([]byte{0xFF}, 9))
	f.Add(byte(20), byte(1), []byte{0x01, 0xFF, 0xFF, 0xFF, 0xFF})

	f.Fuzz(func(t *testing.T, group, variation byte, data []byte) {
		gv := GV(group, variation)
		d, ok := Lookup(gv)
		if !ok || d.Packed {
			return
		}
		size, ok := d.SizeOctets()
		if !ok || size == 0 || len(data) < size {
			return
		}
		buf := data[:size]

		ctx := Context{Synchronized: true}.WithCTO(testTime)

		switch d.Measurement {
		case dnp3.TypeBinary:
			checkRoundTrip(t, gv, buf, ctx, binaryCodecs[gv])
		case dnp3.TypeDoubleBitBinary:
			checkRoundTrip(t, gv, buf, ctx, doublebitCodecs[gv])
		case dnp3.TypeCounter:
			checkRoundTrip(t, gv, buf, ctx, counterCodecs[gv])
		case dnp3.TypeFrozenCounter:
			checkRoundTrip(t, gv, buf, ctx, frozencounterCodecs[gv])
		case dnp3.TypeAnalog:
			checkAnalogRoundTrip(t, gv, buf, ctx, analogCodecs[gv], d)
		case dnp3.TypeBinaryOutputStatus:
			checkRoundTrip(t, gv, buf, ctx, binaryoutputCodecs[gv])
		case dnp3.TypeAnalogOutputStatus:
			checkAnalogOutRoundTrip(t, gv, buf, ctx, analogoutputCodecs[gv], d)
		}
	})
}

// checkRoundTrip asserts decode then encode reproduces the original octets.
func checkRoundTrip[T any](t *testing.T, gv GroupVar, buf []byte, ctx Context, c Codec[T]) {
	t.Helper()
	if c.Parse == nil {
		return
	}
	v := c.Parse(buf, ctx)
	out := c.Write(nil, v, ctx)
	if len(out) != len(buf) {
		t.Fatalf("%s: re-encoded to %d octets from %d", gv, len(out), len(buf))
	}
	if !bytes.Equal(out, buf) {
		t.Fatalf("%s: round trip changed the octets\n in: % x\nout: % x", gv, buf, out)
	}
}

// checkAnalogRoundTrip is separate from checkRoundTrip because the float
// variations cannot round-trip every bit pattern: a signalling NaN is a legal
// encoding, and passing one through a float64 normalises it to a quiet NaN.
// That is a property of IEEE 754, not a codec bug, so those variations are
// checked for size and freedom from panics rather than for octet equality.
//
// Whether a variation is float is read from the descriptor rather than guessed
// from the variation number: variation 3 is a 32-bit integer in group 30 and a
// single-precision float in group 40.
func checkAnalogRoundTrip(t *testing.T, gv GroupVar, buf []byte, ctx Context, c Codec[dnp3.Analog], d Descriptor) {
	t.Helper()
	if c.Parse == nil {
		return
	}
	v := c.Parse(buf, ctx)
	out := c.Write(nil, v, ctx)
	if len(out) != len(buf) {
		t.Fatalf("%s: re-encoded to %d octets from %d", gv, len(out), len(buf))
	}
	if !d.FloatValue && !bytes.Equal(out, buf) {
		t.Fatalf("%s: round trip changed the octets\n in: % x\nout: % x", gv, buf, out)
	}
}

func checkAnalogOutRoundTrip(t *testing.T, gv GroupVar, buf []byte, ctx Context, c Codec[dnp3.AnalogOutputStatus], d Descriptor) {
	t.Helper()
	if c.Parse == nil {
		return
	}
	v := c.Parse(buf, ctx)
	out := c.Write(nil, v, ctx)
	if len(out) != len(buf) {
		t.Fatalf("%s: re-encoded to %d octets from %d", gv, len(out), len(buf))
	}
	if !d.FloatValue && !bytes.Equal(out, buf) {
		t.Fatalf("%s: round trip changed the octets\n in: % x\nout: % x", gv, buf, out)
	}
}

// FuzzPacked drives the bit-packed decoders, where the object count and the
// buffer length come from different header fields and are not guaranteed to
// agree.
func FuzzPacked(f *testing.F) {
	f.Add([]byte{0xAA, 0x03}, uint16(10))
	f.Add([]byte{}, uint16(8))
	f.Add([]byte{0xFF}, uint16(0))

	f.Fuzz(func(t *testing.T, data []byte, count uint16) {
		n := int(count % 4096) // bound the allocation the fuzzer can request

		if got := ParsePackedBinary(data, n, nil); len(got) != n {
			t.Fatalf("ParsePackedBinary returned %d values, want %d", len(got), n)
		}
		if got := ParsePackedBinaryOutput(data, n, nil); len(got) != n {
			t.Fatalf("ParsePackedBinaryOutput returned %d values, want %d", len(got), n)
		}
		if got := ParsePackedDoubleBit(data, n, nil); len(got) != n {
			t.Fatalf("ParsePackedDoubleBit returned %d values, want %d", len(got), n)
		}

		// Whatever comes out must pack back to the documented width.
		bits := make([]bool, n)
		if got, want := len(AppendPackedBinary(nil, bits)), PackedOctets(n, 1); got != want {
			t.Fatalf("packed %d bits into %d octets, want %d", n, got, want)
		}
	})
}

// FuzzCommands drives the hand-written command codecs.
func FuzzCommands(f *testing.F) {
	f.Add(bytes.Repeat([]byte{0}, CROBSize))
	f.Add([]byte{0x41, 0x01, 0xE8, 0x03, 0, 0, 0, 0, 0, 0, 0})

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) >= CROBSize {
			c := ParseCROB(data)
			if out := AppendCROB(nil, c); !bytes.Equal(out, data[:CROBSize]) {
				t.Fatalf("CROB round trip changed the octets\n in: % x\nout: % x",
					data[:CROBSize], out)
			}
		}
		if len(data) >= AnalogOutput32Size {
			v := ParseAnalogOutputInt32(data)
			if out := AppendAnalogOutputInt32(nil, v); !bytes.Equal(out, data[:AnalogOutput32Size]) {
				t.Fatal("analog output int32 round trip changed the octets")
			}
		}
		if len(data) >= AnalogOutput16Size {
			v := ParseAnalogOutputInt16(data)
			if out := AppendAnalogOutputInt16(nil, v); !bytes.Equal(out, data[:AnalogOutput16Size]) {
				t.Fatal("analog output int16 round trip changed the octets")
			}
		}
		if len(data) >= Time48Size {
			ts := ParseTime48(data)
			if out := AppendTime48(nil, ts); !bytes.Equal(out, data[:Time48Size]) {
				t.Fatalf("time48 round trip changed the octets\n in: % x\nout: % x",
					data[:Time48Size], out)
			}
		}
	})
}
