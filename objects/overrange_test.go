package objects

import (
	"math"
	"testing"

	"github.com/dscsystems/go-dnp3"
)

// A value that does not fit an analog's integer variation is saturated, and a
// saturated reading has to say so: 32767 in a 16-bit variation is either a
// real 32767 or a pegged 40000, and OVER_RANGE is the only thing that tells a
// master which. The flag is the standard's answer to exactly this — the value
// exceeds the range of the variation reported.
func TestAnalogIntegerVariationsFlagOverRange(t *testing.T) {
	tests := []struct {
		name  string
		gv    GroupVar
		value float64
		over  bool
	}{
		{"g30v1 in range", GV(30, 1), 1000, false},
		{"g30v1 over int32", GV(30, 1), 1e12, true},
		{"g30v2 in range", GV(30, 2), 32767, false},
		{"g30v2 over int16", GV(30, 2), 40000, true},
		{"g30v2 under int16", GV(30, 2), -40000, true},
		{"g30v2 not a number", GV(30, 2), math.NaN(), true},
		{"g32v2 event over int16", GV(32, 2), 40000, true},
		{"g40v2 output status over int16", GV(40, 2), 40000, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var c Codec[dnp3.Analog]
			var ok bool
			if tc.gv.Group == 40 {
				ao, found := AnalogOutputCodec(tc.gv)
				if !found {
					t.Fatalf("no codec for %v", tc.gv)
				}
				buf := ao.Write(nil, dnp3.AnalogOutputStatus{Value: tc.value, Flags: dnp3.Online}, Context{})
				got := ao.Parse(buf, Context{}).Flags.Has(dnp3.OverRange)
				if got != tc.over {
					t.Errorf("OVER_RANGE = %v, want %v", got, tc.over)
				}
				return
			}
			if c, ok = AnalogCodec(tc.gv); !ok {
				t.Fatalf("no codec for %v", tc.gv)
			}
			buf := c.Write(nil, dnp3.Analog{Value: tc.value, Flags: dnp3.Online}, Context{})
			got := c.Parse(buf, Context{})
			if got.Flags.Has(dnp3.OverRange) != tc.over {
				t.Errorf("OVER_RANGE = %v, want %v (value read back %v)",
					got.Flags.Has(dnp3.OverRange), tc.over, got.Value)
			}
			if !got.Flags.Has(dnp3.Online) {
				t.Error("setting OVER_RANGE lost the flags the point already carried")
			}
		})
	}
}

// Floating-point variations hold the value as it is, so nothing saturates and
// nothing is flagged.
func TestAnalogFloatVariationsDoNotFlagOverRange(t *testing.T) {
	c, _ := AnalogCodec(GV(30, 5))
	buf := c.Write(nil, dnp3.Analog{Value: 1e12, Flags: dnp3.Online}, Context{})
	if c.Parse(buf, Context{}).Flags.Has(dnp3.OverRange) {
		t.Error("a float variation flagged a value it can represent")
	}
}
