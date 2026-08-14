package transport

import (
	"bytes"
	"testing"
)

func TestHeaderRoundTrip(t *testing.T) {
	for i := range 256 {
		b := byte(i)
		if got := ParseHeader(b).Byte(); got != b {
			t.Fatalf("header %#02x round-tripped to %#02x", b, got)
		}
	}
}

func TestHeaderFields(t *testing.T) {
	tests := []struct {
		b    byte
		want Header
	}{
		{0xC0, Header{Fir: true, Fin: true, Seq: 0}},  // single-segment fragment
		{0x40, Header{Fir: true, Seq: 0}},             // first of several
		{0x01, Header{Seq: 1}},                        // middle
		{0x82, Header{Fin: true, Seq: 2}},             // last
		{0xFF, Header{Fir: true, Fin: true, Seq: 63}}, // top of the sequence space
	}
	for _, tc := range tests {
		if got := ParseHeader(tc.b); got != tc.want {
			t.Errorf("ParseHeader(%#02x) = %+v, want %+v", tc.b, got, tc.want)
		}
	}
}

func TestSegmentsFor(t *testing.T) {
	tests := []struct{ n, want int }{
		{0, 1}, // an empty fragment still needs one segment
		{1, 1},
		{249, 1},
		{250, 2},
		{498, 2},
		{499, 3},
		{2048, 9}, // the default maximum fragment
	}
	for _, tc := range tests {
		if got := SegmentsFor(tc.n); got != tc.want {
			t.Errorf("SegmentsFor(%d) = %d, want %d", tc.n, got, tc.want)
		}
	}
}

func TestSegmenterSingleSegment(t *testing.T) {
	var s Segmenter
	frag := []byte{0xC0, 0xC1, 0x01, 0x3C, 0x02, 0x06}

	segs := s.SegmentAll(frag)
	if len(segs) != 1 {
		t.Fatalf("got %d segments, want 1", len(segs))
	}
	h := ParseHeader(segs[0][0])
	if !h.Fir || !h.Fin {
		t.Errorf("header = %v, want both FIR and FIN", h)
	}
	if !bytes.Equal(segs[0][1:], frag) {
		t.Errorf("payload = % x, want % x", segs[0][1:], frag)
	}
}

func TestSegmenterEmptyFragment(t *testing.T) {
	// A zero-object response is a real message and must still be framed.
	var s Segmenter
	segs := s.SegmentAll(nil)
	if len(segs) != 1 {
		t.Fatalf("got %d segments, want 1", len(segs))
	}
	if len(segs[0]) != 1 {
		t.Errorf("segment = % x, want a header only", segs[0])
	}
	h := ParseHeader(segs[0][0])
	if !h.Fir || !h.Fin {
		t.Errorf("header = %v, want FIR and FIN", h)
	}
}

// TestSegmenterMaxFragment covers the size budget from the architecture: a
// 2048-octet fragment becomes nine segments, eight full and one of 56 octets.
func TestSegmenterMaxFragment(t *testing.T) {
	var s Segmenter
	frag := make([]byte, DefaultMaxFragment)
	for i := range frag {
		frag[i] = byte(i)
	}

	segs := s.SegmentAll(frag)
	if len(segs) != 9 {
		t.Fatalf("got %d segments, want 9", len(segs))
	}

	var rebuilt []byte
	for i, seg := range segs {
		h := ParseHeader(seg[0])
		if want := i == 0; h.Fir != want {
			t.Errorf("segment %d: FIR = %v, want %v", i, h.Fir, want)
		}
		if want := i == len(segs)-1; h.Fin != want {
			t.Errorf("segment %d: FIN = %v, want %v", i, h.Fin, want)
		}
		if h.Seq != uint8(i) {
			t.Errorf("segment %d: seq = %d, want %d", i, h.Seq, i)
		}
		if body := len(seg) - 1; i < 8 && body != MaxSegmentPayload {
			t.Errorf("segment %d carries %d octets, want %d", i, body, MaxSegmentPayload)
		}
		rebuilt = append(rebuilt, seg[1:]...)
	}
	if len(segs[8])-1 != 2048-8*MaxSegmentPayload {
		t.Errorf("final segment carries %d octets, want %d", len(segs[8])-1, 2048-8*MaxSegmentPayload)
	}
	if !bytes.Equal(rebuilt, frag) {
		t.Error("segments do not reassemble to the original fragment")
	}
}

func TestSegmenterSequenceWrapsAndPersists(t *testing.T) {
	// The sequence counter is per-link, not per-fragment: it must carry over
	// between fragments and wrap at 64. Restarting it per fragment would make
	// every second fragment look like a replay to a strict receiver.
	var s Segmenter
	s.SetSeq(62)

	for i := range 4 {
		segs := s.SegmentAll([]byte{byte(i)})
		want := uint8((62 + i) % SeqModulus)
		if got := ParseHeader(segs[0][0]).Seq; got != want {
			t.Errorf("fragment %d: seq = %d, want %d", i, got, want)
		}
	}
	if s.Seq() != 2 {
		t.Errorf("next seq = %d, want 2 after wrapping", s.Seq())
	}
}

func TestSegmenterIncrementalAPI(t *testing.T) {
	var s Segmenter
	if s.Pending() {
		t.Error("a fresh segmenter has nothing pending")
	}
	s.Reset(make([]byte, 600))

	n := 0
	buf := make([]byte, 0, MaxSegmentSize)
	for s.Pending() {
		out, ok := s.Next(buf[:0])
		if !ok {
			t.Fatal("Next reported no segment while Pending was true")
		}
		n++
		if len(out) > MaxSegmentSize {
			t.Errorf("segment %d is %d octets, over the maximum", n, len(out))
		}
	}
	if n != 3 {
		t.Errorf("emitted %d segments, want 3", n)
	}
	if _, ok := s.Next(nil); ok {
		t.Error("Next should report false once drained")
	}
}

// ---------- Reassembler ----------

// feed runs a fragment through the segmenter and into the reassembler,
// returning the delivered fragment.
//
// The copy is made with make and copy rather than append, because appending
// zero elements to a nil slice yields nil — which would erase exactly the
// distinction the reassembler goes out of its way to preserve, between "no
// fragment" and "an empty fragment".
func feed(t *testing.T, r *Reassembler, frag []byte) []byte {
	t.Helper()
	var s Segmenter
	var out []byte
	for _, seg := range s.SegmentAll(frag) {
		res := r.Accept(seg)
		if res.Complete() {
			out = make([]byte, len(res.Fragment))
			copy(out, res.Fragment)
		}
	}
	return out
}

func TestReassemblerRoundTrip(t *testing.T) {
	for _, n := range []int{0, 1, 248, 249, 250, 500, 1000, 2048} {
		frag := make([]byte, n)
		for i := range frag {
			frag[i] = byte(i * 7)
		}
		r := NewReassembler(0)
		got := feed(t, r, frag)

		if n == 0 {
			// An empty fragment must be delivered as empty, not swallowed.
			if got == nil {
				t.Error("empty fragment was not delivered")
			}
			continue
		}
		if !bytes.Equal(got, frag) {
			t.Errorf("size %d: fragment did not survive the round trip", n)
		}
		if r.Stats().FragmentsCompleted != 1 {
			t.Errorf("size %d: FragmentsCompleted = %d, want 1", n, r.Stats().FragmentsCompleted)
		}
		if r.InProgress() {
			t.Errorf("size %d: assembly still in progress", n)
		}
	}
}

func TestReassemblerConsecutiveFragments(t *testing.T) {
	r := NewReassembler(0)
	var s Segmenter

	for i := range 5 {
		frag := bytes.Repeat([]byte{byte(i)}, 300)
		var got []byte
		for _, seg := range s.SegmentAll(frag) {
			if res := r.Accept(seg); res.Complete() {
				got = append([]byte(nil), res.Fragment...)
			}
		}
		if !bytes.Equal(got, frag) {
			t.Fatalf("fragment %d did not round-trip", i)
		}
	}
	if r.Stats().SegmentsDiscarded != 0 {
		t.Errorf("discarded %d segments from a clean stream", r.Stats().SegmentsDiscarded)
	}
}

func TestReassemblerRejectsContinuationWithoutFIR(t *testing.T) {
	r := NewReassembler(0)
	res := r.Accept([]byte{0x01, 0xAA, 0xBB}) // no FIR, nothing in progress

	if res.Complete() {
		t.Error("delivered a fragment from a stray continuation")
	}
	if res.Discarded != DiscardNoFIR {
		t.Errorf("reason = %v, want %v", res.Discarded, DiscardNoFIR)
	}
	if r.Stats().Discarded(DiscardNoFIR) != 1 {
		t.Error("the discard was not counted")
	}
}

func TestReassemblerRejectsSequenceGap(t *testing.T) {
	r := NewReassembler(0)
	r.Accept([]byte{0x40, 0x01}) // FIR, seq 0

	// Seq 2 arrives where seq 1 was expected. Stitching around the hole would
	// deliver a fragment the peer never sent.
	res := r.Accept([]byte{0x82, 0x03}) // FIN, seq 2
	if res.Complete() {
		t.Fatal("delivered a fragment across a sequence gap")
	}
	if res.Discarded != DiscardBadSequence {
		t.Errorf("reason = %v, want %v", res.Discarded, DiscardBadSequence)
	}
	if r.InProgress() {
		t.Error("assembly should be abandoned after a sequence gap")
	}
}

func TestReassemblerRejectsDuplicateSegment(t *testing.T) {
	// A repeated sequence number is the same failure as a gap: the fragment
	// can no longer be trusted.
	r := NewReassembler(0)
	r.Accept([]byte{0x40, 0x01}) // FIR, seq 0
	r.Accept([]byte{0x01, 0x02}) // seq 1
	res := r.Accept([]byte{0x01, 0x02})

	if res.Discarded != DiscardBadSequence {
		t.Errorf("reason = %v, want %v", res.Discarded, DiscardBadSequence)
	}
}

// TestReassemblerRestartOnUnexpectedFIR covers the peer that gives up
// mid-fragment and starts over. The partial fragment is lost, but the new one
// must be delivered — dropping both would turn one lost fragment into two.
func TestReassemblerRestartOnUnexpectedFIR(t *testing.T) {
	r := NewReassembler(0)
	r.Accept([]byte{0x40, 0xAA}) // FIR, seq 0 — abandoned

	res := r.Accept([]byte{0xC5, 0xBB, 0xCC}) // FIR|FIN, seq 5 — a fresh fragment
	if !res.Complete() {
		t.Fatal("the restarted fragment was not delivered")
	}
	if !bytes.Equal(res.Fragment, []byte{0xBB, 0xCC}) {
		t.Errorf("fragment = % x, want BB CC", res.Fragment)
	}
	if res.Discarded != DiscardUnexpectedFIR {
		t.Errorf("reason = %v, want %v", res.Discarded, DiscardUnexpectedFIR)
	}
	if r.Stats().Discarded(DiscardUnexpectedFIR) != 1 {
		t.Error("the abandoned fragment was not counted")
	}
}

func TestReassemblerSequenceWrap(t *testing.T) {
	r := NewReassembler(0)
	r.Accept([]byte{0x40 | 62, 0x01})   // FIR, seq 62
	r.Accept([]byte{63, 0x02})          // seq 63
	res := r.Accept([]byte{0x80, 0x03}) // FIN, seq 0 — wrapped

	if !res.Complete() {
		t.Fatal("the sequence did not wrap from 63 to 0")
	}
	if !bytes.Equal(res.Fragment, []byte{0x01, 0x02, 0x03}) {
		t.Errorf("fragment = % x", res.Fragment)
	}
}

func TestReassemblerEnforcesMaxFragment(t *testing.T) {
	r := NewReassembler(300)
	var s Segmenter

	var discarded bool
	for _, seg := range s.SegmentAll(make([]byte, 600)) {
		if res := r.Accept(seg); res.Discarded == DiscardOverflow {
			discarded = true
		} else if res.Complete() {
			t.Fatal("delivered a fragment over the configured maximum")
		}
	}
	if !discarded {
		t.Error("oversize fragment was not reported as an overflow")
	}
	if r.InProgress() {
		t.Error("assembly should be abandoned after an overflow")
	}
}

func TestReassemblerRecoversAfterDiscard(t *testing.T) {
	// The point of counting discards separately is that the session keeps
	// going. Verify a good fragment still gets through after a bad one.
	r := NewReassembler(0)
	r.Accept([]byte{0x05, 0xFF}) // stray continuation

	got := feed(t, r, []byte{0xC0, 0xC1, 0x01})
	if !bytes.Equal(got, []byte{0xC0, 0xC1, 0x01}) {
		t.Errorf("fragment = % x, want C0 C1 01", got)
	}
}

func TestReassemblerEmptySegment(t *testing.T) {
	r := NewReassembler(0)
	if res := r.Accept(nil); res.Discarded != DiscardEmptySegment {
		t.Errorf("reason = %v, want %v", res.Discarded, DiscardEmptySegment)
	}
}

func TestReassemblerReset(t *testing.T) {
	r := NewReassembler(0)
	r.Accept([]byte{0x40, 0x01})
	if !r.InProgress() {
		t.Fatal("expected assembly in progress")
	}
	r.Reset()
	if r.InProgress() || r.Buffered() != 0 {
		t.Error("Reset did not abandon the partial fragment")
	}
}

func TestReassemblerZeroValueUsable(t *testing.T) {
	// A zero-value Reassembler must work, since sessions may embed one by
	// value rather than calling the constructor.
	var r Reassembler
	res := r.Accept([]byte{0xC0, 0x01, 0x02})
	if !res.Complete() || !bytes.Equal(res.Fragment, []byte{0x01, 0x02}) {
		t.Errorf("zero-value reassembler failed: %+v", res)
	}
}

func TestDiscardReasonStrings(t *testing.T) {
	for r := DiscardNone; r <= DiscardOverflow; r++ {
		if s := r.String(); s == "" || s == "DiscardReason(?)" {
			t.Errorf("reason %d has no name", r)
		}
	}
}

func BenchmarkSegmentAndReassemble(b *testing.B) {
	frag := make([]byte, DefaultMaxFragment)
	var s Segmenter
	r := NewReassembler(0)
	segbuf := make([]byte, 0, MaxSegmentSize)

	b.SetBytes(int64(len(frag)))
	b.ReportAllocs()
	for b.Loop() {
		s.Reset(frag)
		for s.Pending() {
			seg, _ := s.Next(segbuf[:0])
			r.Accept(seg)
		}
	}
}
