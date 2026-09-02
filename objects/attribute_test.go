package objects

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/dscsystems/go-dnp3"
)

func TestAttributeRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		attr dnp3.Attribute
		want string // what Value() should render
	}{
		{"a string", StringAttribute(250, "GO-DNP3 RTU"), "GO-DNP3 RTU"},
		{"an empty string", StringAttribute(245, ""), ""},
		{"a small count", UintAttribute(226, 6), "6"},
		{"a fragment size", UintAttribute(228, 2048), "2048"},
		{"a large count", UintAttribute(220, 70000), "70000"},
		{"a negative number", IntAttribute(204, -3), "-3"},
		{
			"octets",
			dnp3.Attribute{Variation: 211, Type: dnp3.AttrOctetString, Octets: []byte{1, 2, 3}},
			"01 02 03",
		},
		{
			"a time",
			dnp3.Attribute{
				Variation: 205, Type: dnp3.AttrTime,
				Time: time.Date(2026, 5, 4, 3, 2, 1, 0, time.UTC),
			},
			"2026-05-04T03:02:01Z",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			buf, err := AppendAttribute(nil, c.attr)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			// The length octet has to describe what follows it, or a reader
			// walking several attributes lands in the middle of the next one.
			if int(buf[1]) != len(buf)-AttributeHeaderSize {
				t.Errorf("length octet says %d, value is %d octets", buf[1], len(buf)-AttributeHeaderSize)
			}

			got, n, err := ParseAttribute(0, c.attr.Variation, buf)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if n != len(buf) {
				t.Errorf("consumed %d octets of %d", n, len(buf))
			}
			if got.Variation != c.attr.Variation || got.Type != c.attr.Type {
				t.Errorf("got %+v, want variation %d type %s",
					got, c.attr.Variation, c.attr.Type)
			}
			if got.Value() != c.want {
				t.Errorf("value %q, want %q", got.Value(), c.want)
			}
		})
	}
}

// An unsigned attribute is encoded in the fewest octets that hold it, which is
// what devices send and what a master has to accept in any width.
func TestAttributeIntegerWidths(t *testing.T) {
	cases := []struct {
		value uint64
		size  int
	}{
		{0, 1}, {255, 1}, {256, 2}, {65535, 2}, {65536, 4}, {1 << 33, 8},
	}
	for _, c := range cases {
		buf, err := AppendAttribute(nil, UintAttribute(220, c.value))
		if err != nil {
			t.Fatalf("%d: %v", c.value, err)
		}
		if got := len(buf) - AttributeHeaderSize; got != c.size {
			t.Errorf("%d encoded in %d octets, want %d", c.value, got, c.size)
		}
		back, _, err := ParseAttribute(0, 220, buf)
		if err != nil {
			t.Fatalf("%d: %v", c.value, err)
		}
		if uint64(back.Number) != c.value {
			t.Errorf("%d came back as %d", c.value, back.Number)
		}
	}
}

// Several attributes in one header are walked by the length each carries.
func TestAttributesWalkTogether(t *testing.T) {
	var buf []byte
	for _, a := range []dnp3.Attribute{
		StringAttribute(250, "model"),
		UintAttribute(226, 12),
		StringAttribute(252, "vendor"),
	} {
		var err error
		if buf, err = AppendAttribute(buf, a); err != nil {
			t.Fatal(err)
		}
	}

	var got []string
	for off := 0; off < len(buf); {
		a, n, err := ParseAttribute(0, 0, buf[off:])
		if err != nil {
			t.Fatalf("at %d: %v", off, err)
		}
		got = append(got, a.Value())
		off += n
	}

	want := []string{"model", "12", "vendor"}
	if len(got) != len(want) {
		t.Fatalf("walked %d attributes %q, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("attribute %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// A type this implementation does not know still yields its octets: a device
// is entitled to use an encoding newer than this code, and a value nobody can
// interpret is still worth showing.
func TestAttributeUnknownType(t *testing.T) {
	buf := []byte{99, 3, 'a', 'b', 'c'}
	a, n, err := ParseAttribute(0, 200, buf)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if n != len(buf) {
		t.Errorf("consumed %d of %d", n, len(buf))
	}
	if !bytes.Equal(a.Octets, []byte("abc")) {
		t.Errorf("octets %q", a.Octets)
	}
	if a.Value() != "abc" {
		t.Errorf("value %q", a.Value())
	}
}

func TestAttributeRejectsMalformed(t *testing.T) {
	cases := []struct {
		name string
		buf  []byte
	}{
		{"nothing", nil},
		{"a type with no length", []byte{1}},
		{"a length past the end", []byte{1, 9, 'a', 'b'}},
		{"a float of the wrong width", []byte{byte(dnp3.AttrFloat), 3, 1, 2, 3}},
		{"a time of the wrong width", []byte{byte(dnp3.AttrTime), 2, 1, 2}},
		{"an integer of an odd width", []byte{byte(dnp3.AttrUnsignedInt), 3, 1, 2, 3}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, _, err := ParseAttribute(0, 240, c.buf); err == nil {
				t.Fatal("accepted a malformed attribute")
			} else if !errors.Is(err, dnp3.ErrMalformed) {
				t.Errorf("error %v does not wrap dnp3.ErrMalformed", err)
			}
		})
	}
}

// The length field is one octet, so a value that will not fit has to be
// refused rather than truncated into something a reader would believe.
func TestAttributeTooLong(t *testing.T) {
	long := make([]byte, MaxAttributeValue+1)
	_, err := AppendAttribute(nil, dnp3.Attribute{
		Variation: 250, Type: dnp3.AttrOctetString, Octets: long,
	})
	if err == nil {
		t.Fatal("encoded a value longer than the length field")
	}
	if !errors.Is(err, dnp3.ErrMalformed) {
		t.Errorf("error %v does not wrap dnp3.ErrMalformed", err)
	}
}

func TestAttributeNames(t *testing.T) {
	// A named attribute reads as its name; an unnamed one still says which it
	// is, because a number is better than nothing when a device invents an
	// attribute of its own.
	a := StringAttribute(250, "RTU-9000")
	if got := a.String(); got != "product name and model: RTU-9000" {
		t.Errorf("got %q", got)
	}

	unknown := StringAttribute(17, "private")
	if got := unknown.Name(); got != "attribute 17" {
		t.Errorf("got %q, want a numbered placeholder", got)
	}

	other := dnp3.Attribute{Set: 3, Variation: 17, Type: dnp3.AttrVisibleString}
	if got := other.Name(); got != "set 3 attribute 17" {
		t.Errorf("got %q, want the set named too", got)
	}
	if _, ok := dnp3.AttributeName(250); !ok {
		t.Error("250 should be a known attribute")
	}
}
