package conformance

import (
	"testing"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/objects"
	"github.com/dscsystems/go-dnp3/outstation"
)

// decodeAnalog finds analog point index in a response and decodes it exactly
// as a master would: through the codec for the variation its header claims.
func decodeAnalog(t *testing.T, resp app.Fragment, index uint32) (float64, bool) {
	t.Helper()
	for _, o := range resp.Objects {
		if o.Group != 30 || index < o.Range.Start || index > o.Range.Stop {
			continue
		}
		gv := objects.GV(o.Group, o.Variation)
		c, ok := objects.AnalogCodec(gv)
		if !ok {
			t.Fatalf("no codec for %v", gv)
		}
		d, _ := objects.Lookup(gv)
		size, _ := d.SizeOctets()
		off := int(index-o.Range.Start) * size
		return c.Parse(o.Data[off:off+size], objects.Context{}).Value, true
	}
	return 0, false
}

// Report: a class 0 read uses the first point's static variation for every
// point of that type.
//
// buildStaticRange resolves variation zero — "use the point's configured
// default" — by looking up the config of the point at the *start* of the
// range, then encodes the whole range with that one group and variation.
// PointConfig.StaticVariation is documented as a property of each point ("the
// variation used when the point is reported in response to a class 0 or range
// read"), so a database that mixes them is misreported: every point after the
// first is encoded as whatever the first one asked for.
//
// Mixing an integer variation with a float one is where this does real
// damage. Group 30 variation 1 is a 32-bit integer; variation 5 is a float.
// A float point encoded through the integer codec loses its fraction, and the
// master has no way to tell — it decodes exactly what the header claims.
func TestClassZeroHonoursEachPointsStaticVariation(t *testing.T) {
	h := newHarness(t, outstation.Config{
		Database: outstation.DatabaseConfig{Analog: 2},
	}, nil)

	h.out.Update(func(db *outstation.Database) {
		// Point 0 reports as a 32-bit integer, point 1 as a float.
		db.Configure(dnp3.TypeAnalog, 0, outstation.PointConfig{StaticVariation: 1})
		db.Configure(dnp3.TypeAnalog, 1, outstation.PointConfig{StaticVariation: 5})
		db.UpdateAnalog(0, dnp3.Analog{Value: 7, Flags: dnp3.Online})
		db.UpdateAnalog(1, dnp3.Analog{Value: 1.5, Flags: dnp3.Online})
	})

	resp := h.request(app.FuncRead, app.ReadAllObjects(60, 1)) // class 0

	var variations []uint8
	for _, o := range resp.Objects {
		if o.Group == 30 {
			variations = append(variations, o.Variation)
		}
	}
	if len(variations) == 0 {
		t.Fatalf("the class 0 response carried no analog objects: %v", resp.Objects)
	}

	// Point 1 asked to be reported as a float, so variation 5 has to appear
	// somewhere in the response.
	var sawFloat bool
	for _, v := range variations {
		if v == 5 {
			sawFloat = true
		}
	}
	if !sawFloat {
		t.Errorf("the response reports analogs as variation(s) %v, with no variation 5; "+
			"point 1 is configured as a float and is being encoded through the integer "+
			"codec the first point asked for, so its value is truncated", variations)
	}
}

// Class 0 is not the only way in. A read naming the group with variation zero
// — "each point's own default" — reaches the same branch in buildStaticRange,
// so it misreports the same way.
func TestVariationZeroRangeReadHonoursEachPointsStaticVariation(t *testing.T) {
	h := newHarness(t, outstation.Config{
		Database: outstation.DatabaseConfig{Analog: 2},
	}, nil)

	h.out.Update(func(db *outstation.Database) {
		db.Configure(dnp3.TypeAnalog, 0, outstation.PointConfig{StaticVariation: 1})
		db.Configure(dnp3.TypeAnalog, 1, outstation.PointConfig{StaticVariation: 5})
		db.UpdateAnalog(0, dnp3.Analog{Value: 7, Flags: dnp3.Online})
		db.UpdateAnalog(1, dnp3.Analog{Value: 1.5, Flags: dnp3.Online})
	})

	resp := h.request(app.FuncRead, app.ReadRange(30, 0, 0, 1))

	if got, ok := decodeAnalog(t, resp, 1); !ok || got != 1.5 {
		t.Errorf("g30v0 range read returned analog 1 as %v (found=%v), want 1.5", got, ok)
	}
}

// An explicit variation is the master's choice and overrides the per-point
// default: every point comes back as what was asked for. A fix must not
// disturb this.
func TestExplicitVariationStillAppliesToEveryPoint(t *testing.T) {
	h := newHarness(t, outstation.Config{
		Database: outstation.DatabaseConfig{Analog: 2},
	}, nil)

	h.out.Update(func(db *outstation.Database) {
		db.Configure(dnp3.TypeAnalog, 0, outstation.PointConfig{StaticVariation: 1})
		db.Configure(dnp3.TypeAnalog, 1, outstation.PointConfig{StaticVariation: 5})
		db.UpdateAnalog(0, dnp3.Analog{Value: 7, Flags: dnp3.Online})
		db.UpdateAnalog(1, dnp3.Analog{Value: 1.5, Flags: dnp3.Online})
	})

	resp := h.request(app.FuncRead, app.ReadRange(30, 5, 0, 1))

	for _, o := range resp.Objects {
		if o.Group == 30 && o.Variation != 5 {
			t.Errorf("asked for g30v5 and got a g30v%d header", o.Variation)
		}
	}
	if got, _ := decodeAnalog(t, resp, 0); got != 7 {
		t.Errorf("analog 0 read back as %v, want 7", got)
	}
}

// The same defect seen from the value rather than the encoding: what the
// master actually reads back for the float point.
func TestClassZeroDoesNotTruncateFloatStatics(t *testing.T) {
	h := newHarness(t, outstation.Config{
		Database: outstation.DatabaseConfig{Analog: 2},
	}, nil)

	h.out.Update(func(db *outstation.Database) {
		db.Configure(dnp3.TypeAnalog, 0, outstation.PointConfig{StaticVariation: 1})
		db.Configure(dnp3.TypeAnalog, 1, outstation.PointConfig{StaticVariation: 5})
		db.UpdateAnalog(0, dnp3.Analog{Value: 7, Flags: dnp3.Online})
		db.UpdateAnalog(1, dnp3.Analog{Value: 1.5, Flags: dnp3.Online})
	})

	resp := h.request(app.FuncRead, app.ReadAllObjects(60, 1))

	got, ok := decodeAnalog(t, resp, 1)
	if !ok {
		t.Fatalf("analog point 1 was not in the class 0 response: %v", resp.Objects)
	}
	if got != 1.5 {
		t.Errorf("analog 1 read back as %v, want 1.5: the point is configured to report as "+
			"a float but was encoded with the first point's integer variation", got)
	}
}
