package app

import (
	"bytes"
	"testing"
)

// FuzzParseFragment feeds arbitrary octets to the fragment parser.
//
// The application layer has the most attacker-controlled structure in the
// stack: counts, ranges and size prefixes are all fields a peer chooses, and
// each is a chance to index past the end of a buffer. The parser must never
// panic, never report an object header extending past the fragment, and never
// consume zero octets for a header — which would spin forever.
func FuzzParseFragment(f *testing.F) {
	f.Add([]byte{0xC0, 0x01, 60, 2, 0x06, 60, 3, 0x06, 60, 4, 0x06, 60, 1, 0x06})
	f.Add([]byte{0xC0, 0x81, 0x00, 0x00, 1, 2, 0x00, 0, 3, 1, 1, 1, 1})
	f.Add([]byte{0xC0, 0x81, 0x00, 0x00, 1, 1, 0x00, 0, 9, 0xAA, 0x03})
	f.Add([]byte{0xC0, 0x05, 12, 1, 0x17, 1, 3, 3, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	f.Add([]byte{0xC0, 0x81, 0, 0, 70, 3, 0x5B, 2, 3, 0, 1, 2, 3, 2, 0, 4, 5})
	f.Add([]byte{0xC0, 0x81, 0, 0, 30, 2, 0x02, 0, 0, 0, 0, 0xFF, 0xFF, 0xFF, 0xFF})
	f.Add([]byte{0xC0})
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		frag, err := ParseFragment(nil, data)

		// Whether or not parsing succeeded, every header it did produce must
		// be internally consistent and point inside the fragment.
		for i, o := range frag.Objects {
			if o.Offset < 0 || o.Offset > len(data) {
				t.Fatalf("header %d offset %d outside a %d octet fragment", i, o.Offset, len(data))
			}
			if o.DataOffset < o.Offset || o.DataOffset > len(data) {
				t.Fatalf("header %d data offset %d outside the fragment", i, o.DataOffset)
			}
			if o.DataOffset+len(o.Data) > len(data) {
				t.Fatalf("header %d data runs %d octets past the fragment end",
					o.DataOffset+len(o.Data)-len(data), i)
			}
			if o.Qualifier.Reserved() {
				t.Fatalf("header %d accepted a qualifier with the reserved bit set", i)
			}
			if !o.Qualifier.IndexPrefix().Valid() || !o.Qualifier.RangeSpec().Valid() {
				t.Fatalf("header %d accepted a reserved qualifier encoding: %v", i, o.Qualifier)
			}
			if o.Range.Spec.IsStartStop() && o.Range.Stop < o.Range.Start {
				t.Fatalf("header %d accepted stop %d below start %d",
					i, o.Range.Stop, o.Range.Start)
			}
		}

		if err != nil {
			return
		}

		// A clean parse means the headers tile the fragment exactly.
		off := frag.Header.Size()
		for i, o := range frag.Objects {
			if o.Offset != off {
				t.Fatalf("header %d starts at %d, want %d — headers do not tile the fragment",
					i, o.Offset, off)
			}
			off += o.Size()
		}
		if off != len(data) {
			t.Fatalf("headers cover %d octets of a %d octet fragment", off, len(data))
		}
	})
}

// FuzzFragmentRoundTrip asserts that anything the parser accepts, the builder
// reproduces byte for byte. This is the property that catches a parser
// accepting an encoding its own writer could never emit.
func FuzzFragmentRoundTrip(f *testing.F) {
	f.Add([]byte{0xC0, 0x01, 60, 2, 0x06, 60, 1, 0x06})
	f.Add([]byte{0xC0, 0x81, 0x00, 0x00, 1, 2, 0x00, 0, 3, 1, 1, 1, 1})
	f.Add([]byte{0xE1, 0x82, 0x02, 0x00, 32, 2, 0x17, 1, 5, 1, 100, 0})

	f.Fuzz(func(t *testing.T, data []byte) {
		frag, err := ParseFragment(nil, data)
		if err != nil {
			return
		}

		var rebuilt []byte
		if frag.Header.IsResponse() {
			rebuilt = BuildResponse(nil, frag.Header.Control, frag.Header.Func,
				frag.Header.IIN, frag.Objects...)
		} else {
			rebuilt = BuildRequest(nil, frag.Header.Control, frag.Header.Func,
				frag.Objects...)
		}

		if !bytes.Equal(rebuilt, data) {
			t.Fatalf("round trip changed the fragment:\n in: % x\nout: % x", data, rebuilt)
		}

		// And the rebuilt fragment must parse back to the same structure.
		again, err := ParseFragment(nil, rebuilt)
		if err != nil {
			t.Fatalf("a rebuilt fragment failed to parse: %v", err)
		}
		if again.Header != frag.Header {
			t.Fatalf("header changed across the round trip: %v vs %v", again.Header, frag.Header)
		}
		if len(again.Objects) != len(frag.Objects) {
			t.Fatalf("object count changed: %d vs %d", len(again.Objects), len(frag.Objects))
		}
	})
}

// FuzzObjectHeader targets a single object header with the count and range
// fields under direct fuzzer control, which is where the length arithmetic
// lives.
func FuzzObjectHeader(f *testing.F) {
	f.Add(uint8(30), uint8(2), uint8(0x00), []byte{0, 15})
	f.Add(uint8(1), uint8(1), uint8(0x00), []byte{0, 255})
	f.Add(uint8(12), uint8(1), uint8(0x17), []byte{1, 3})
	f.Add(uint8(70), uint8(3), uint8(0x5B), []byte{2, 0xFF, 0xFF})
	f.Add(uint8(30), uint8(2), uint8(0x02), []byte{0, 0, 0, 0, 0xFF, 0xFF, 0xFF, 0xFF})

	f.Fuzz(func(t *testing.T, group, variation, qualifier uint8, rest []byte) {
		buf := append([]byte{group, variation, qualifier}, rest...)

		h, n, err := ParseObjectHeader(nil, buf, 0, true)
		if err != nil {
			return
		}
		if n <= 0 {
			t.Fatalf("a successful parse consumed %d octets", n)
		}
		if n > len(buf) {
			t.Fatalf("consumed %d octets from a %d octet buffer", n, len(buf))
		}
		if h.Size() != n {
			t.Fatalf("Size() = %d but the parse consumed %d", h.Size(), n)
		}
		if len(h.Data) > len(buf) {
			t.Fatalf("data is %d octets from a %d octet buffer", len(h.Data), len(buf))
		}

		// Re-encoding must reproduce exactly the octets consumed.
		out := AppendObjectHeader(nil, h)
		if !bytes.Equal(out, buf[:n]) {
			t.Fatalf("round trip changed the header:\n in: % x\nout: % x", buf[:n], out)
		}
	})
}

// FuzzBuilder drives the fragment builder to confirm it never exceeds its cap
// and never produces something its own parser rejects.
func FuzzBuilder(f *testing.F) {
	f.Add(uint8(0xC0), uint8(0x81), uint16(2048), uint8(20))
	f.Add(uint8(0xC0), uint8(0x01), uint16(64), uint8(200))

	f.Fuzz(func(t *testing.T, ac, fc uint8, maxFrag uint16, objects uint8) {
		maxSize := int(maxFrag)%DefaultMaxFragment + ResponseHeaderSize
		b := NewBuilder(maxSize)

		h := Header{Control: ParseControl(ac), Func: FuncCode(fc)}
		if err := b.SetHeader(h); err != nil {
			return
		}

		for range objects {
			o := ReadAllObjects(60, 1)
			if !b.Fits(o) {
				break
			}
			if err := b.AddObject(o); err != nil {
				break
			}
		}

		if b.Len() > maxSize {
			t.Fatalf("builder produced %d octets over its %d cap", b.Len(), maxSize)
		}
		if b.Remaining() < 0 {
			t.Fatalf("Remaining() = %d", b.Remaining())
		}
		if _, err := ParseFragment(nil, b.Bytes()); err != nil {
			t.Fatalf("the builder produced a fragment its own parser rejects: %v", err)
		}
	})
}
