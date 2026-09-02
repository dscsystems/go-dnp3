package conformance

import (
	"testing"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/objects"
	"github.com/dscsystems/go-dnp3/outstation"
)

// What a master actually sends to read device attributes, built by hand rather
// than by this library's own master: an outstation that only answers the
// request shape its sibling produces has not been tested at all.

// readAttributes builds the request a master sends for one attribute, or for
// all of them with variation 254.
func readAttributes(set, variation uint8) app.ObjectHeader {
	return app.ObjectHeader{
		Group:     0,
		Variation: variation,
		Qualifier: app.MakeQualifier(app.PrefixNone, app.RangeStartStop8),
		Range: app.Range{
			Spec: app.RangeStartStop8, Start: uint32(set), Stop: uint32(set), Count: 1,
		},
	}
}

// attributesIn pulls every group 0 object out of a response.
func attributesIn(t *testing.T, frag app.Fragment) map[uint8]dnp3.Attribute {
	t.Helper()
	out := map[uint8]dnp3.Attribute{}

	for _, h := range frag.Objects {
		if h.Group != 0 {
			continue
		}
		set := uint8(h.Range.Start)
		a, _, err := objects.ParseAttribute(set, h.Variation, h.Data)
		if err != nil {
			t.Fatalf("g0v%d: %v", h.Variation, err)
		}
		out[h.Variation] = a
	}
	return out
}

func attributeHarness(t *testing.T) *harness {
	t.Helper()
	return newHarness(t, outstation.Config{
		Database: smallDB(),
		Attributes: []dnp3.Attribute{
			objects.StringAttribute(252, "DSC Systems"),
			objects.StringAttribute(250, "GO-DNP3"),
			objects.UintAttribute(249, 2),
		},
	}, nil)
}

// A read of variation 254 returns every attribute, each in its own object
// header, because the variation is what names it.
func TestAttributeReadAll(t *testing.T) {
	h := attributeHarness(t)
	resp := h.request(app.FuncRead, readAttributes(0, dnp3.AttrAll))

	attrs := attributesIn(t, resp)
	if len(attrs) < 3 {
		t.Fatalf("got %d attributes, want at least the three configured", len(attrs))
	}
	if got := attrs[252]; got.Value() != "DSC Systems" {
		t.Errorf("g0v252 = %q", got.Value())
	}
	if got := attrs[249]; got.Number != 2 || got.Type != dnp3.AttrUnsignedInt {
		t.Errorf("g0v249 = %+v", got)
	}

	// Each attribute is its own header, and the range on each is the set.
	for _, o := range resp.Objects {
		if o.Group != 0 {
			continue
		}
		if !o.Qualifier.RangeSpec().IsStartStop() {
			t.Errorf("g0v%d came back with qualifier %s", o.Variation, o.Qualifier)
		}
		if o.Range.Start != 0 || o.Range.Stop != 0 {
			t.Errorf("g0v%d reports set %d..%d, want 0..0",
				o.Variation, o.Range.Start, o.Range.Stop)
		}
	}
	if resp.Header.IIN.Has(app.IINObjectUnknown) {
		t.Error("a read of all attributes set OBJECT_UNKNOWN")
	}
}

// A read of one variation returns that one and nothing else.
func TestAttributeReadOne(t *testing.T) {
	h := attributeHarness(t)
	resp := h.request(app.FuncRead, readAttributes(0, 250))

	attrs := attributesIn(t, resp)
	if len(attrs) != 1 {
		t.Fatalf("got %d attributes, want 1: %v", len(attrs), attrs)
	}
	if got := attrs[250]; got.Value() != "GO-DNP3" {
		t.Errorf("g0v250 = %q", got.Value())
	}
}

// An attribute the device does not keep is refused with the indication that
// says so, rather than answered with something else or with silence.
func TestAttributeUnknownVariation(t *testing.T) {
	h := attributeHarness(t)
	resp := h.request(app.FuncRead, readAttributes(0, 199))

	if len(attributesIn(t, resp)) != 0 {
		t.Error("an attribute came back for a variation the device does not have")
	}
	if !resp.Header.IIN.Has(app.IINObjectUnknown) {
		t.Errorf("IIN %v, want OBJECT_UNKNOWN", resp.Header.IIN)
	}
}

// A set the device does not use is empty, and says so the same way.
func TestAttributeUnknownSet(t *testing.T) {
	h := attributeHarness(t)
	resp := h.request(app.FuncRead, readAttributes(9, dnp3.AttrAll))

	if len(attributesIn(t, resp)) != 0 {
		t.Error("set 9 returned attributes")
	}
	if !resp.Header.IIN.Has(app.IINObjectUnknown) {
		t.Errorf("IIN %v, want OBJECT_UNKNOWN", resp.Header.IIN)
	}
}

// Asking which attributes exist is a distinct encoding this implementation
// does not have. Refusing it is right; inventing an answer would not be.
func TestAttributeListRefused(t *testing.T) {
	h := attributeHarness(t)
	resp := h.request(app.FuncRead, readAttributes(0, dnp3.AttrList))

	if len(attributesIn(t, resp)) != 0 {
		t.Error("the list request was answered with attributes")
	}
	if !resp.Header.IIN.Has(app.IINObjectUnknown) {
		t.Errorf("IIN %v, want OBJECT_UNKNOWN", resp.Header.IIN)
	}
}

// An attribute read does not disturb the rest of the outstation: the next poll
// answers normally, and the restart indication is not cleared by it.
func TestAttributeReadLeavesTheSessionAlone(t *testing.T) {
	h := attributeHarness(t)

	before := h.request(app.FuncRead, app.ReadAllObjects(60, 1))
	h.request(app.FuncRead, readAttributes(0, dnp3.AttrAll))
	after := h.request(app.FuncRead, app.ReadAllObjects(60, 1))

	if len(before.Objects) != len(after.Objects) {
		t.Errorf("a class 0 poll returned %d headers before the attribute read and %d after",
			len(before.Objects), len(after.Objects))
	}
	if !after.Header.IIN.Has(app.IINDeviceRestart) {
		t.Error("the attribute read cleared the restart indication")
	}
}

// The counts an outstation derives describe the database a master is about to
// poll, so they have to be the database's own numbers.
func TestAttributeDerivedCountsMatchTheDatabase(t *testing.T) {
	db := smallDB()
	h := newHarness(t, outstation.Config{Database: db}, nil)

	attrs := attributesIn(t, h.request(app.FuncRead, readAttributes(0, dnp3.AttrAll)))

	for variation, want := range map[uint8]int64{
		outstation.AttrBinaryInputCount:  int64(db.Binary),
		outstation.AttrAnalogInputCount:  int64(db.Analog),
		outstation.AttrCounterCount:      int64(db.Counter),
		outstation.AttrBinaryOutputCount: int64(db.BinaryOutputStatus),
		outstation.AttrAnalogOutputCount: int64(db.AnalogOutputStatus),
	} {
		got, ok := attrs[variation]
		if !ok {
			t.Errorf("g0v%d was not reported", variation)
			continue
		}
		if got.Number != want {
			t.Errorf("g0v%d = %d, want %d", variation, got.Number, want)
		}
	}
}
