// Package transport implements the DNP3 transport function: the single-octet
// layer that cuts application fragments into link-sized segments and puts them
// back together.
//
// It is the thinnest layer in the stack and the one most worth getting exactly
// right. A reassembler that accepts a segment it should have dropped hands a
// corrupted fragment to the application layer, which then reports measurements
// that were never sent.
//
// Nothing here performs I/O or reads a clock.
package transport

import (
	"errors"
	"fmt"
)

// Wire constants fixed by IEEE 1815 clause 8.
const (
	// HeaderSize is the transport header: one octet per segment.
	HeaderSize = 1

	// MaxSegmentPayload is the largest application-fragment slice one segment
	// can carry: the link layer's 250-octet payload less the transport header.
	MaxSegmentPayload = 249

	// MaxSegmentSize is a full segment including its header.
	MaxSegmentSize = HeaderSize + MaxSegmentPayload

	// SeqModulus is the transport sequence space. Six bits.
	SeqModulus = 64

	// DefaultMaxFragment is the default cap on a reassembled fragment. The
	// standard's default maximum application fragment size is 2048 octets;
	// larger values are legal by negotiation but must be bounded, because the
	// reassembler buffers the whole fragment before delivering it.
	DefaultMaxFragment = 2048
)

// Transport header bit masks.
const (
	fin = 0x80
	fir = 0x40
	seq = 0x3F
)

// SegmentsFor returns how many segments a fragment of n octets requires.
func SegmentsFor(n int) int {
	if n <= 0 {
		return 1 // a zero-length fragment still occupies one segment
	}
	return (n + MaxSegmentPayload - 1) / MaxSegmentPayload
}

// Header is a decoded transport header.
type Header struct {
	Fir bool
	Fin bool
	Seq uint8 // 0..63
}

// ParseHeader decodes a transport header octet.
func ParseHeader(b byte) Header {
	return Header{Fir: b&fir != 0, Fin: b&fin != 0, Seq: b & seq}
}

// Byte encodes the header octet.
func (h Header) Byte() byte {
	var b byte
	if h.Fin {
		b |= fin
	}
	if h.Fir {
		b |= fir
	}
	return b | (h.Seq & seq)
}

func (h Header) String() string {
	f := "   "
	switch {
	case h.Fir && h.Fin:
		f = "FIR|FIN"
	case h.Fir:
		f = "FIR"
	case h.Fin:
		f = "FIN"
	}
	return fmt.Sprintf("seq=%02d %s", h.Seq, f)
}

// ErrFragmentTooLarge means a fragment exceeded the configured maximum.
var ErrFragmentTooLarge = errors.New("transport: fragment exceeds maximum size")

// ---------- Segmenter ----------

// Segmenter cuts application fragments into segments.
//
// It holds one fragment at a time. Call Reset to load a fragment, then Next
// until it reports done. The sequence number persists across fragments, which
// is what the standard requires: the counter is per-link, not per-fragment.
type Segmenter struct {
	frag []byte
	off  int
	seq  uint8
	live bool
}

// Seq returns the sequence number the next segment will carry.
func (s *Segmenter) Seq() uint8 { return s.seq }

// SetSeq forces the sequence counter, for tests and for resuming a session.
func (s *Segmenter) SetSeq(v uint8) { s.seq = v % SeqModulus }

// Reset loads a fragment for segmentation, discarding any fragment in
// progress.
func (s *Segmenter) Reset(fragment []byte) {
	s.frag = fragment
	s.off = 0
	s.live = true
}

// Pending reports whether segments remain to be emitted.
func (s *Segmenter) Pending() bool { return s.live }

// Clear abandons the loaded fragment.
//
// It is distinct from Reset(nil), which loads an *empty* fragment — a real
// message that still occupies one segment. A caller wanting "nothing to send"
// must say so, or a segmenter reset with nil reports itself pending forever.
func (s *Segmenter) Clear() {
	s.frag = nil
	s.off = 0
	s.live = false
}

// Next appends the next segment to dst and returns the extended slice.
//
// The returned bool is false when no fragment is loaded. A zero-length
// fragment produces exactly one segment carrying FIR and FIN, which is how a
// zero-object response is framed.
func (s *Segmenter) Next(dst []byte) ([]byte, bool) {
	if !s.live {
		return dst, false
	}

	first := s.off == 0
	end := min(s.off+MaxSegmentPayload, len(s.frag))
	last := end >= len(s.frag)

	h := Header{Fir: first, Fin: last, Seq: s.seq}
	dst = append(dst, h.Byte())
	dst = append(dst, s.frag[s.off:end]...)

	s.off = end
	s.seq = (s.seq + 1) % SeqModulus
	if last {
		s.live = false
		s.frag = nil
	}
	return dst, true
}

// SegmentAll is the batch form of Reset and Next: it returns every segment of
// fragment as a slice of independently allocated segments. Session code uses
// the incremental API; this exists for tests and for callers that would rather
// have the whole list.
func (s *Segmenter) SegmentAll(fragment []byte) [][]byte {
	s.Reset(fragment)
	out := make([][]byte, 0, SegmentsFor(len(fragment)))
	for {
		seg, ok := s.Next(make([]byte, 0, MaxSegmentSize))
		if !ok {
			return out
		}
		out = append(out, seg)
	}
}

// ---------- Reassembler ----------

// DiscardReason says why a segment or partial fragment was dropped. These are
// counted separately because "the link is unreliable" is not a diagnosis: a
// session dropping segments for out-of-order sequence numbers has a very
// different problem from one whose peer keeps restarting mid-fragment.
type DiscardReason uint8

// Discard reasons.
const (
	// DiscardNone is the zero value: nothing was dropped.
	DiscardNone DiscardReason = iota
	// DiscardEmptySegment means a segment carried no header octet.
	DiscardEmptySegment
	// DiscardNoFIR means a continuation segment arrived with no fragment in
	// progress, usually the tail of a fragment whose start was lost.
	DiscardNoFIR
	// DiscardUnexpectedFIR means a new fragment started before the previous
	// one finished. The partial fragment is dropped and the new one begins.
	DiscardUnexpectedFIR
	// DiscardBadSequence means a segment's sequence number was not the
	// expected successor.
	DiscardBadSequence
	// DiscardOverflow means the fragment exceeded the configured maximum.
	DiscardOverflow
)

func (d DiscardReason) String() string {
	switch d {
	case DiscardNone:
		return "none"
	case DiscardEmptySegment:
		return "empty segment"
	case DiscardNoFIR:
		return "continuation without FIR"
	case DiscardUnexpectedFIR:
		return "FIR during assembly"
	case DiscardBadSequence:
		return "sequence mismatch"
	case DiscardOverflow:
		return "fragment overflow"
	default:
		return "DiscardReason(?)"
	}
}

// Stats counts what the reassembler saw.
type Stats struct {
	SegmentsReceived   uint64
	FragmentsCompleted uint64
	SegmentsDiscarded  uint64
	Discards           [6]uint64 // indexed by DiscardReason
}

// Discarded returns the count for one discard reason.
func (s Stats) Discarded(r DiscardReason) uint64 {
	if int(r) >= len(s.Discards) {
		return 0
	}
	return s.Discards[r]
}

// Reassembler rebuilds application fragments from segments.
//
// A Reassembler is not safe for concurrent use; one belongs to one session.
type Reassembler struct {
	// MaxFragment caps a reassembled fragment. Zero means DefaultMaxFragment.
	MaxFragment int

	buf      []byte
	expect   uint8
	assembly bool
	stats    Stats
}

// NewReassembler returns a reassembler with the given fragment cap. Pass zero
// for DefaultMaxFragment.
func NewReassembler(maxFragment int) *Reassembler {
	if maxFragment <= 0 {
		maxFragment = DefaultMaxFragment
	}
	return &Reassembler{
		MaxFragment: maxFragment,
		buf:         make([]byte, 0, maxFragment),
	}
}

// Stats returns a snapshot of the counters.
func (r *Reassembler) Stats() Stats { return r.stats }

// InProgress reports whether a fragment is partially assembled.
func (r *Reassembler) InProgress() bool { return r.assembly }

// Buffered returns the octet count assembled so far.
func (r *Reassembler) Buffered() int { return len(r.buf) }

// Reset abandons any fragment in progress. A session calls this when the link
// is re-established, because a fragment cannot span a connection.
func (r *Reassembler) Reset() {
	r.buf = r.buf[:0]
	r.assembly = false
}

// Result is the outcome of feeding one segment.
type Result struct {
	// Fragment is the completed application fragment, or nil if the fragment
	// is still incomplete. It aliases the reassembler's buffer and is valid
	// only until the next call to Accept.
	Fragment []byte
	// Discarded says what was dropped, if anything. A segment can both
	// complete a fragment and report a discard: an unexpected FIR drops the
	// partial fragment and starts a new one in the same call.
	Discarded DiscardReason
}

// Complete reports whether a fragment was delivered.
func (r Result) Complete() bool { return r.Fragment != nil }

func (r *Reassembler) maxFragment() int {
	if r.MaxFragment <= 0 {
		return DefaultMaxFragment
	}
	return r.MaxFragment
}

// Accept feeds one transport segment, header octet included.
//
// The returned fragment aliases the reassembler's internal buffer and is
// invalidated by the next call; copy it if it must outlive that.
func (r *Reassembler) Accept(segment []byte) Result {
	r.stats.SegmentsReceived++

	if len(segment) < HeaderSize {
		return r.discard(DiscardEmptySegment)
	}

	h := ParseHeader(segment[0])
	payload := segment[HeaderSize:]

	if h.Fir {
		// A FIR while assembling means the peer restarted the fragment —
		// typically after its own timeout. The partial fragment is
		// unrecoverable, but the new one is perfectly good, so drop the old,
		// report it, and carry on rather than dropping both.
		reported := DiscardNone
		if r.assembly && len(r.buf) > 0 {
			reported = DiscardUnexpectedFIR
			r.stats.SegmentsDiscarded++
			r.stats.Discards[DiscardUnexpectedFIR]++
		}
		r.buf = r.buf[:0]
		r.assembly = true
		r.expect = h.Seq

		res := r.append(h, payload)
		if res.Discarded == DiscardNone {
			res.Discarded = reported
		}
		return res
	}

	if !r.assembly {
		// A continuation with nothing to continue. The fragment's opening
		// segment was lost, so every segment until the next FIR is useless.
		return r.discard(DiscardNoFIR)
	}

	if h.Seq != r.expect {
		// A gap or a duplicate. Either way the fragment is now unreliable:
		// silently stitching around the hole would deliver a fragment the
		// peer never sent.
		r.Reset()
		return r.discard(DiscardBadSequence)
	}

	return r.append(h, payload)
}

// append adds a validated segment's payload and completes the fragment if the
// segment carried FIN.
func (r *Reassembler) append(h Header, payload []byte) Result {
	if len(r.buf)+len(payload) > r.maxFragment() {
		r.Reset()
		return r.discard(DiscardOverflow)
	}

	r.buf = append(r.buf, payload...)
	r.expect = (h.Seq + 1) % SeqModulus

	if !h.Fin {
		return Result{}
	}

	r.assembly = false
	r.stats.FragmentsCompleted++
	// A completed fragment may legitimately be empty, so return a non-nil
	// zero-length slice: callers distinguish "no fragment" from "empty
	// fragment" by nilness, and conflating them would swallow a valid
	// zero-object response.
	if r.buf == nil {
		r.buf = []byte{}
	}
	return Result{Fragment: r.buf}
}

func (r *Reassembler) discard(reason DiscardReason) Result {
	r.stats.SegmentsDiscarded++
	r.stats.Discards[reason]++
	return Result{Discarded: reason}
}
