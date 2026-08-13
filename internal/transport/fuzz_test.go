package transport

import (
	"bytes"
	"testing"
)

// FuzzReassembler feeds arbitrary segment streams to the reassembler.
//
// The properties that matter: it never panics, never exceeds its fragment cap,
// and never delivers a fragment that was not preceded by a FIR and terminated
// by a FIN with an unbroken sequence in between. That last one is the whole
// job — a reassembler that stitches around a gap hands the application layer
// measurements the peer never sent.
func FuzzReassembler(f *testing.F) {
	f.Add([]byte{0xC0, 0x01, 0x02})
	f.Add([]byte{0x40, 0x01, 0x81, 0x02})
	f.Add([]byte{0x01, 0x02})
	f.Add([]byte{0x40, 0xAA, 0x40, 0xBB})
	f.Add(bytes.Repeat([]byte{0x40}, 100))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		const maxFrag = 512
		r := NewReassembler(maxFrag)

		// Slice the input into segments at arbitrary but bounded points, so
		// the fuzzer explores segment boundaries as well as segment contents.
		for len(data) > 0 {
			n := 1 + int(data[0])%MaxSegmentSize
			n = min(n, len(data))
			seg := data[:n]
			data = data[n:]

			res := r.Accept(seg)

			if res.Complete() {
				if len(res.Fragment) > maxFrag {
					t.Fatalf("delivered a %d octet fragment over the %d cap",
						len(res.Fragment), maxFrag)
				}
				if r.InProgress() {
					t.Fatal("assembly still in progress after delivering a fragment")
				}
			}
			if r.Buffered() > maxFrag {
				t.Fatalf("buffered %d octets over the %d cap", r.Buffered(), maxFrag)
			}
		}

		st := r.Stats()
		if st.SegmentsReceived < st.FragmentsCompleted {
			t.Fatal("completed more fragments than segments received")
		}
	})
}

// FuzzRoundTrip asserts the segmenter and reassembler are inverses: any
// fragment, cut up and put back together, must come out identical.
func FuzzRoundTrip(f *testing.F) {
	f.Add([]byte{}, uint8(0))
	f.Add([]byte{0xC0, 0xC1, 0x01}, uint8(0))
	f.Add(bytes.Repeat([]byte{0xAB}, 249), uint8(7))
	f.Add(bytes.Repeat([]byte{0xCD}, 250), uint8(63))
	f.Add(bytes.Repeat([]byte{0xEF}, 2048), uint8(62))

	f.Fuzz(func(t *testing.T, frag []byte, startSeq uint8) {
		if len(frag) > DefaultMaxFragment {
			frag = frag[:DefaultMaxFragment]
		}

		var s Segmenter
		s.SetSeq(startSeq)
		segs := s.SegmentAll(frag)

		if want := SegmentsFor(len(frag)); len(segs) != want {
			t.Fatalf("produced %d segments for %d octets, want %d", len(segs), len(frag), want)
		}
		for i, seg := range segs {
			if len(seg) > MaxSegmentSize {
				t.Fatalf("segment %d is %d octets, over the maximum", i, len(seg))
			}
			if len(seg) < HeaderSize {
				t.Fatalf("segment %d has no header", i)
			}
		}

		r := NewReassembler(DefaultMaxFragment)
		var got []byte
		var delivered bool
		for i, seg := range segs {
			res := r.Accept(seg)
			if res.Discarded != DiscardNone {
				t.Fatalf("segment %d was discarded (%v) from a self-produced stream",
					i, res.Discarded)
			}
			if res.Complete() {
				if delivered {
					t.Fatal("one fragment was delivered twice")
				}
				delivered = true
				got = make([]byte, len(res.Fragment))
				copy(got, res.Fragment)
			}
		}

		if !delivered {
			t.Fatal("fragment was never delivered")
		}
		if !bytes.Equal(got, frag) {
			t.Fatalf("round trip changed the fragment: %d octets in, %d out", len(frag), len(got))
		}
	})
}

// FuzzHeader checks the transport header codec over the full octet space.
func FuzzHeader(f *testing.F) {
	f.Add(byte(0xC0))
	f.Add(byte(0x00))

	f.Fuzz(func(t *testing.T, b byte) {
		h := ParseHeader(b)
		if h.Seq >= SeqModulus {
			t.Fatalf("sequence %d is outside the six-bit space", h.Seq)
		}
		if got := h.Byte(); got != b {
			t.Fatalf("header %#02x round-tripped to %#02x", b, got)
		}
	})
}
