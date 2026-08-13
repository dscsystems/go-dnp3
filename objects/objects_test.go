package objects

import (
	"bytes"
	"testing"
	"time"

	"github.com/dscsystems/go-dnp3"
)

// testTime is a fixed instant with whole milliseconds, since DNP3 carries no
// finer resolution and a sub-millisecond component would fail every round trip
// for the wrong reason.
var testTime = time.Date(2026, 8, 13, 14, 30, 45, 123_000_000, time.UTC)

func syncedCtx() Context {
	return Context{Synchronized: true}.WithCTO(testTime.Add(-2 * time.Second))
}

// sizeOf returns a codec's object size from the registry, failing the test if
// the descriptor is missing or not a whole number of octets.
func sizeOf(t *testing.T, gv GroupVar) int {
	t.Helper()
	d, ok := Lookup(gv)
	if !ok {
		t.Fatalf("%s has no descriptor", gv)
	}
	n, ok := d.SizeOctets()
	if !ok {
		t.Fatalf("%s is not a whole number of octets", gv)
	}
	return n
}

// TestEveryCodecRoundTrips is the test that justifies the generator: encode
// then decode must be the identity for every group and variation in the spec.
//
// A generator that gets one field offset wrong produces a codec that is
// self-consistent and completely wrong on the wire, so this is paired with the
// golden-hex tests below, which pin the actual octets.
func TestEveryCodecRoundTrips(t *testing.T) {
	ctx := syncedCtx()

	t.Run("binary", func(t *testing.T) {
		for gv, c := range binaryCodecs {
			d, _ := Lookup(gv)
			want := dnp3.Binary{Value: true, Flags: dnp3.Online}
			if d.HasTime {
				want.Time = dnp3.Timestamp{Time: testTime, Quality: dnp3.TimestampSynchronized}
			}

			buf := c.Write(nil, want, ctx)
			if got, exp := len(buf), sizeOf(t, gv); got != exp {
				t.Errorf("%s: wrote %d octets, want %d", gv, got, exp)
				continue
			}
			got := c.Parse(buf, ctx)
			if got.Value != want.Value {
				t.Errorf("%s: value %v, want %v", gv, got.Value, want.Value)
			}
			// The state rides in bit 7 of the flags octet, so compare only the
			// quality bits.
			if got.Flags&^dnp3.StateBit != want.Flags&^dnp3.StateBit {
				t.Errorf("%s: flags %v, want %v", gv, got.Flags, want.Flags)
			}
			checkTime(t, gv, d, got.Time, want.Time)
		}
	})

	t.Run("doublebit", func(t *testing.T) {
		for gv, c := range doublebitCodecs {
			d, _ := Lookup(gv)
			want := dnp3.DoubleBitBinary{Value: dnp3.DoubleBitDeterminedOn, Flags: dnp3.Online}
			if d.HasTime {
				want.Time = dnp3.Timestamp{Time: testTime, Quality: dnp3.TimestampSynchronized}
			}

			buf := c.Write(nil, want, ctx)
			if got, exp := len(buf), sizeOf(t, gv); got != exp {
				t.Errorf("%s: wrote %d octets, want %d", gv, got, exp)
				continue
			}
			got := c.Parse(buf, ctx)
			if got.Value != want.Value {
				t.Errorf("%s: value %v, want %v", gv, got.Value, want.Value)
			}
			// A double-bit state occupies bits 6 and 7 of the flags octet.
			if got.Flags&0x3F != want.Flags&0x3F {
				t.Errorf("%s: flags %v, want %v", gv, got.Flags, want.Flags)
			}
			checkTime(t, gv, d, got.Time, want.Time)
		}
	})

	t.Run("counter", func(t *testing.T) {
		for gv, c := range counterCodecs {
			d, _ := Lookup(gv)
			want := dnp3.Counter{Value: 4096, Flags: dnp3.Online}
			if d.HasTime {
				want.Time = dnp3.Timestamp{Time: testTime, Quality: dnp3.TimestampSynchronized}
			}
			roundTripNumeric(t, gv, d, c, want,
				func(a, b dnp3.Counter) bool { return a.Value == b.Value },
				func(v dnp3.Counter) (dnp3.Flags, dnp3.Timestamp) { return v.Flags, v.Time },
				ctx)
		}
	})

	t.Run("frozencounter", func(t *testing.T) {
		for gv, c := range frozencounterCodecs {
			d, _ := Lookup(gv)
			want := dnp3.FrozenCounter{Value: 4096, Flags: dnp3.Online}
			if d.HasTime {
				want.Time = dnp3.Timestamp{Time: testTime, Quality: dnp3.TimestampSynchronized}
			}
			roundTripNumeric(t, gv, d, c, want,
				func(a, b dnp3.FrozenCounter) bool { return a.Value == b.Value },
				func(v dnp3.FrozenCounter) (dnp3.Flags, dnp3.Timestamp) { return v.Flags, v.Time },
				ctx)
		}
	})

	t.Run("analog", func(t *testing.T) {
		for gv, c := range analogCodecs {
			d, _ := Lookup(gv)
			// 300 is representable exactly in every analog variation,
			// including the 16-bit and single-precision ones.
			want := dnp3.Analog{Value: 300, Flags: dnp3.Online}
			if d.HasTime {
				want.Time = dnp3.Timestamp{Time: testTime, Quality: dnp3.TimestampSynchronized}
			}
			roundTripNumeric(t, gv, d, c, want,
				func(a, b dnp3.Analog) bool { return a.Value == b.Value },
				func(v dnp3.Analog) (dnp3.Flags, dnp3.Timestamp) { return v.Flags, v.Time },
				ctx)
		}
	})

	t.Run("binaryoutput", func(t *testing.T) {
		for gv, c := range binaryoutputCodecs {
			d, _ := Lookup(gv)
			want := dnp3.BinaryOutputStatus{Value: true, Flags: dnp3.Online}
			if d.HasTime {
				want.Time = dnp3.Timestamp{Time: testTime, Quality: dnp3.TimestampSynchronized}
			}
			buf := c.Write(nil, want, ctx)
			if got, exp := len(buf), sizeOf(t, gv); got != exp {
				t.Errorf("%s: wrote %d octets, want %d", gv, got, exp)
				continue
			}
			got := c.Parse(buf, ctx)
			if got.Value != want.Value {
				t.Errorf("%s: value %v, want %v", gv, got.Value, want.Value)
			}
			checkTime(t, gv, d, got.Time, want.Time)
		}
	})

	t.Run("analogoutput", func(t *testing.T) {
		for gv, c := range analogoutputCodecs {
			d, _ := Lookup(gv)
			want := dnp3.AnalogOutputStatus{Value: 300, Flags: dnp3.Online}
			if d.HasTime {
				want.Time = dnp3.Timestamp{Time: testTime, Quality: dnp3.TimestampSynchronized}
			}
			roundTripNumeric(t, gv, d, c, want,
				func(a, b dnp3.AnalogOutputStatus) bool { return a.Value == b.Value },
				func(v dnp3.AnalogOutputStatus) (dnp3.Flags, dnp3.Timestamp) { return v.Flags, v.Time },
				ctx)
		}
	})
}

// roundTripNumeric encodes and decodes one value, checking size, value, flags
// and time.
func roundTripNumeric[T any](
	t *testing.T, gv GroupVar, d Descriptor, c Codec[T], want T,
	sameValue func(a, b T) bool,
	parts func(T) (dnp3.Flags, dnp3.Timestamp),
	ctx Context,
) {
	t.Helper()

	buf := c.Write(nil, want, ctx)
	if got, exp := len(buf), sizeOf(t, gv); got != exp {
		t.Errorf("%s: wrote %d octets, want %d", gv, got, exp)
		return
	}

	got := c.Parse(buf, ctx)
	if !sameValue(got, want) {
		t.Errorf("%s: value did not survive the round trip: %+v vs %+v", gv, got, want)
	}

	gotFlags, gotTime := parts(got)
	wantFlags, wantTime := parts(want)
	if d.HasFlags {
		if gotFlags != wantFlags {
			t.Errorf("%s: flags %v, want %v", gv, gotFlags, wantFlags)
		}
	} else if gotFlags != dnp3.Online {
		// A no-flag variation carries no quality, so the decoder reports
		// online rather than marking good data offline.
		t.Errorf("%s: no-flag variation decoded flags %v, want ONLINE", gv, gotFlags)
	}
	checkTime(t, gv, d, gotTime, wantTime)
}

func checkTime(t *testing.T, gv GroupVar, d Descriptor, got, want dnp3.Timestamp) {
	t.Helper()
	if !d.HasTime {
		if got.IsValid() {
			t.Errorf("%s: variation carries no time but decoded %v", gv, got)
		}
		return
	}
	if !got.Time.Equal(want.Time) {
		t.Errorf("%s: time %v, want %v", gv, got.Time, want.Time)
	}
	if got.Quality != want.Quality {
		t.Errorf("%s: time quality %v, want %v", gv, got.Quality, want.Quality)
	}
}

// TestGoldenEncodings pins the actual octets for a sample of variations.
//
// Round-trip tests prove the codecs agree with themselves; only fixed octets
// prove they agree with the standard. These were hand-derived from the field
// layouts in IEEE 1815.
func TestGoldenEncodings(t *testing.T) {
	ctx := Context{Synchronized: true}

	t.Run("g1v2 binary with flags", func(t *testing.T) {
		// ONLINE plus the state bit for a point that is on.
		want := []byte{0x81}
		got := binaryCodecs[GV(1, 2)].Write(nil, dnp3.Binary{Value: true, Flags: dnp3.Online}, ctx)
		if !bytes.Equal(got, want) {
			t.Errorf("got % x, want % x", got, want)
		}
		// And off is ONLINE alone.
		got = binaryCodecs[GV(1, 2)].Write(nil, dnp3.Binary{Value: false, Flags: dnp3.Online}, ctx)
		if !bytes.Equal(got, []byte{0x01}) {
			t.Errorf("off state: got % x, want 01", got)
		}
	})

	t.Run("g3v2 double-bit with flags", func(t *testing.T) {
		// DETERMINED_ON is 0b10 in bits 7 and 6, over ONLINE.
		got := doublebitCodecs[GV(3, 2)].Write(nil,
			dnp3.DoubleBitBinary{Value: dnp3.DoubleBitDeterminedOn, Flags: dnp3.Online}, ctx)
		if !bytes.Equal(got, []byte{0x81}) {
			t.Errorf("got % x, want 81", got)
		}
	})

	t.Run("g30v2 analog 16-bit with flags", func(t *testing.T) {
		// ONLINE then 300 little-endian.
		got := analogCodecs[GV(30, 2)].Write(nil, dnp3.Analog{Value: 300, Flags: dnp3.Online}, ctx)
		if !bytes.Equal(got, []byte{0x01, 0x2C, 0x01}) {
			t.Errorf("got % x, want 01 2c 01", got)
		}
	})

	t.Run("g30v5 analog float", func(t *testing.T) {
		// ONLINE then 1.5 as an IEEE 754 single.
		got := analogCodecs[GV(30, 5)].Write(nil, dnp3.Analog{Value: 1.5, Flags: dnp3.Online}, ctx)
		want := []byte{0x01, 0x00, 0x00, 0xC0, 0x3F}
		if !bytes.Equal(got, want) {
			t.Errorf("got % x, want % x", got, want)
		}
	})

	t.Run("g20v1 counter 32-bit with flags", func(t *testing.T) {
		got := counterCodecs[GV(20, 1)].Write(nil, dnp3.Counter{Value: 1, Flags: dnp3.Online}, ctx)
		if !bytes.Equal(got, []byte{0x01, 0x01, 0x00, 0x00, 0x00}) {
			t.Errorf("got % x, want 01 01 00 00 00", got)
		}
	})

	t.Run("g2v2 binary event with absolute time", func(t *testing.T) {
		ts := dnp3.Timestamp{Time: time.UnixMilli(0x0000_0102_0304).UTC(), Quality: dnp3.TimestampSynchronized}
		got := binaryCodecs[GV(2, 2)].Write(nil,
			dnp3.Binary{Value: true, Flags: dnp3.Online, Time: ts}, ctx)
		// Flags, then the timestamp as six little-endian octets.
		want := []byte{0x81, 0x04, 0x03, 0x02, 0x01, 0x00, 0x00}
		if !bytes.Equal(got, want) {
			t.Errorf("got % x, want % x", got, want)
		}
	})
}

// TestNoFlagVariationsReportOnline pins the decision that a variation carrying
// no quality information decodes as online. The alternative would mark
// perfectly good readings as offline and hide them from every consumer.
func TestNoFlagVariationsReportOnline(t *testing.T) {
	ctx := Context{}
	for _, gv := range []GroupVar{GV(30, 3), GV(30, 4), GV(20, 5), GV(20, 6)} {
		d, ok := Lookup(gv)
		if !ok || d.HasFlags {
			t.Fatalf("%s should be a no-flag variation", gv)
		}
	}
	if v := analogCodecs[GV(30, 4)].Parse([]byte{0x2C, 0x01}, ctx); v.Flags != dnp3.Online {
		t.Errorf("g30v4 flags = %v, want ONLINE", v.Flags)
	}
	if v := counterCodecs[GV(20, 5)].Parse([]byte{1, 0, 0, 0}, ctx); v.Flags != dnp3.Online {
		t.Errorf("g20v5 flags = %v, want ONLINE", v.Flags)
	}
}

// ---------- Relative time ----------

func TestRelativeTimeNeedsCTO(t *testing.T) {
	// Without a common time of occurrence the offset means nothing. Anchoring
	// it to the epoch would file the event in 1970 and look like data.
	noCTO := Context{Synchronized: true}
	got := binaryCodecs[GV(2, 3)].Parse([]byte{0x81, 0xE8, 0x03}, noCTO)
	if got.Time.IsValid() {
		t.Errorf("time = %v, want invalid without a CTO", got.Time)
	}
	if !got.Value {
		t.Error("the value should still decode without a CTO")
	}
}

func TestRelativeTimeResolvesAgainstCTO(t *testing.T) {
	base := testTime
	ctx := Context{Synchronized: true}.WithCTO(base)

	got := binaryCodecs[GV(2, 3)].Parse([]byte{0x81, 0xE8, 0x03}, ctx) // 1000 ms
	want := base.Add(time.Second)
	if !got.Time.Time.Equal(want) {
		t.Errorf("time = %v, want %v", got.Time.Time, want)
	}
	if got.Time.Quality != dnp3.TimestampSynchronized {
		t.Errorf("quality = %v, want synchronized", got.Time.Quality)
	}
}

func TestRelativeOffset(t *testing.T) {
	base := testTime
	ctx := Context{}.WithCTO(base)

	tests := []struct {
		name string
		at   time.Time
		want uint16
	}{
		{"at the CTO", base, 0},
		{"one second after", base.Add(time.Second), 1000},
		{"at the limit", base.Add(65535 * time.Millisecond), 65535},
		{"past the limit clamps", base.Add(2 * time.Minute), 65535},
		{"before the CTO clamps", base.Add(-time.Second), 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ctx.RelativeOffset(dnp3.Timestamp{Time: tc.at, Quality: dnp3.TimestampSynchronized})
			if got != tc.want {
				t.Errorf("RelativeOffset = %d, want %d", got, tc.want)
			}
		})
	}

	if (Context{}).RelativeOffset(dnp3.Now(base)) != 0 {
		t.Error("without a CTO the offset should be zero")
	}
}

func TestTimeQualityFollowsSynchronization(t *testing.T) {
	// An outstation asserting NEED_TIME is telling the master its clock is not
	// set. Events it stamps must not be filed as synchronized.
	unsynced := Context{Synchronized: false}
	got := binaryCodecs[GV(2, 2)].Parse([]byte{0x81, 0, 0, 0, 0, 0, 0}, unsynced)
	if got.Time.Quality != dnp3.TimestampUnsynchronized {
		t.Errorf("quality = %v, want unsynchronized", got.Time.Quality)
	}
}

// ---------- Registry ----------

func TestRegistryCoversSpec(t *testing.T) {
	if len(descriptors) < 100 {
		t.Errorf("only %d descriptors; the spec should define far more", len(descriptors))
	}

	// Every measurement descriptor that is neither packed nor variable must
	// have a codec, or the generator silently skipped it.
	for gv, d := range descriptors {
		if d.Measurement == dnp3.TypeUnknown || d.Packed {
			continue
		}
		if !hasCodec(gv, d.Measurement) {
			t.Errorf("%s (%s) has a descriptor but no codec", gv, d.Measurement)
		}
	}
}

func hasCodec(gv GroupVar, pt dnp3.PointType) bool {
	switch pt {
	case dnp3.TypeBinary:
		_, ok := BinaryCodec(gv)
		return ok
	case dnp3.TypeDoubleBitBinary:
		_, ok := DoubleBitCodec(gv)
		return ok
	case dnp3.TypeCounter:
		_, ok := CounterCodec(gv)
		return ok
	case dnp3.TypeFrozenCounter:
		_, ok := FrozenCounterCodec(gv)
		return ok
	case dnp3.TypeAnalog:
		_, ok := AnalogCodec(gv)
		return ok
	case dnp3.TypeBinaryOutputStatus:
		_, ok := BinaryOutputCodec(gv)
		return ok
	case dnp3.TypeAnalogOutputStatus:
		_, ok := AnalogOutputCodec(gv)
		return ok
	}
	return false
}

func TestLookupAndDescriptorMetadata(t *testing.T) {
	d, ok := Lookup(GV(32, 3))
	if !ok {
		t.Fatal("g32v3 should be in the registry")
	}
	if d.Kind != KindEvent {
		t.Errorf("kind = %v, want event", d.Kind)
	}
	if d.Measurement != dnp3.TypeAnalog {
		t.Errorf("measurement = %v, want Analog", d.Measurement)
	}
	if !d.HasFlags || !d.HasTime || d.RelativeTime {
		t.Errorf("g32v3 should have flags and an absolute time: %+v", d)
	}
	if n, ok := d.SizeOctets(); !ok || n != 11 {
		t.Errorf("size = %d octets (ok=%v), want 11", n, ok)
	}

	if _, ok := Lookup(GV(99, 1)); ok {
		t.Error("g99v1 should not be in the registry")
	}
}

func TestPackedDescriptorsHaveNoOctetSize(t *testing.T) {
	for _, gv := range []GroupVar{GV(1, 1), GV(3, 1), GV(10, 1), GV(80, 1)} {
		d, ok := Lookup(gv)
		if !ok {
			t.Fatalf("%s missing from the registry", gv)
		}
		if !d.Packed {
			t.Errorf("%s should be packed", gv)
		}
		if _, ok := d.SizeOctets(); ok {
			t.Errorf("%s is packed and has no per-object octet size", gv)
		}
	}
}

func TestGroupVarString(t *testing.T) {
	if got := GV(30, 2).String(); got != "g30v2" {
		t.Errorf("String() = %q, want g30v2", got)
	}
	if got := GV(1, 1).Key(); got != 0x0101 {
		t.Errorf("Key() = %#04x, want 0x0101", got)
	}
}

func TestKindString(t *testing.T) {
	for k := KindUnknown; k <= KindAttribute; k++ {
		if s := k.String(); s == "" || s == "Kind(?)" {
			t.Errorf("kind %d has no name", k)
		}
	}
}
