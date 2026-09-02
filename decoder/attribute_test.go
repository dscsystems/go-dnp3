package decoder

import (
	"strings"
	"testing"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/objects"
)

// attributeHeader wraps one attribute the way it travels: the variation names
// it, the range names the set.
func attributeHeader(t *testing.T, a dnp3.Attribute) app.ObjectHeader {
	t.Helper()
	value, err := objects.AppendAttribute(nil, a)
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	return app.ObjectHeader{
		Group: 0, Variation: a.Variation,
		Qualifier: app.MakeQualifier(app.PrefixNone, app.RangeStartStop8),
		Range: app.Range{
			Spec: app.RangeStartStop8, Start: uint32(a.Set), Stop: uint32(a.Set), Count: 1,
		},
		Data: value,
	}
}

func TestDecodeAttributes(t *testing.T) {
	cases := []struct {
		name string
		attr dnp3.Attribute
		want []string
	}{
		{
			"a named string",
			objects.StringAttribute(250, "RTU-9000"),
			[]string{"product name and model", "RTU-9000", "string"},
		},
		{
			"a count",
			objects.UintAttribute(226, 32),
			[]string{"number of binary inputs", "32", "uint"},
		},
		{
			"one this library has no name for",
			objects.StringAttribute(17, "private"),
			[]string{"attribute 17", "private"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			values, ok := DecodeValues(attributeHeader(t, c.attr), objects.Context{})
			if !ok {
				t.Fatal("the header was not decoded")
			}
			if len(values) != 1 {
				t.Fatalf("decoded %d values, want 1", len(values))
			}
			// The index column carries the variation, which is the nearest
			// thing an attribute has to a point index.
			if values[0].Index != uint16(c.attr.Variation) {
				t.Errorf("index %d, want the variation %d", values[0].Index, c.attr.Variation)
			}
			for _, want := range c.want {
				if !strings.Contains(values[0].Value, want) {
					t.Errorf("rendering %q does not contain %q", values[0].Value, want)
				}
			}
		})
	}
}

// A malformed attribute is reported rather than dropped: a device sending one
// is the most interesting thing in the capture.
func TestDecodeMalformedAttribute(t *testing.T) {
	h := app.ObjectHeader{
		Group: 0, Variation: 250,
		Qualifier: app.MakeQualifier(app.PrefixNone, app.RangeStartStop8),
		Range:     app.Range{Spec: app.RangeStartStop8, Count: 1},
		Data:      []byte{1, 9, 'a'}, // a length of 9 with one octet behind it
	}

	values, ok := DecodeValues(h, objects.Context{})
	if !ok || len(values) != 1 {
		t.Fatalf("decoded %v, %v", values, ok)
	}
	if !strings.Contains(values[0].Value, "malformed") {
		t.Errorf("rendering %q does not say the attribute was malformed", values[0].Value)
	}
}

// A whole fragment carrying an attribute alongside measurements has to decode
// as both: before group 0 was walkable, the unknown size rejected everything.
func TestDecodeFragmentWithAttributes(t *testing.T) {
	frag := []byte{
		0xC0, 0x81, 0x00, 0x00,
		0, 250, 0x00, 0, 0, // g0v250, set 0
		1, 3, 'R', 'T', 'U', // a visible string
		1, 2, 0x00, 0, 0, 0x81, // g1v2, one binary
	}

	f, err := app.ParseFragment(nil, frag)
	if err != nil {
		t.Fatalf("ParseFragment: %v", err)
	}
	if len(f.Objects) != 2 {
		t.Fatalf("parsed %d headers, want the attribute and the binary", len(f.Objects))
	}

	attrs, ok := DecodeValues(f.Objects[0], objects.Context{})
	if !ok || len(attrs) != 1 || !strings.Contains(attrs[0].Value, "RTU") {
		t.Errorf("attribute decoded as %v", attrs)
	}
	points, ok := DecodeValues(f.Objects[1], objects.Context{})
	if !ok || len(points) != 1 {
		t.Errorf("the binary alongside it decoded as %v", points)
	}
}
