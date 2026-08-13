package link

import (
	"bytes"
	"errors"
	"io"
	"math/rand/v2"
	"testing"
)

// mkframe builds a valid frame carrying n octets of recognisable payload.
func mkframe(t *testing.T, dest, src uint16, n int) ([]byte, []byte) {
	t.Helper()
	payload := make([]byte, n)
	for i := range payload {
		payload[i] = byte(i)
	}
	h := Header{
		Control: Control{Dir: true, Prm: true, Func: FuncUnconfirmedUserData},
		Dest:    dest, Src: src, Length: uint8(MinLength + n),
	}
	wire, err := Encode(nil, h, payload)
	if err != nil {
		t.Fatal(err)
	}
	return wire, payload
}

func TestParserSingleFrame(t *testing.T) {
	wire, payload := mkframe(t, 10, 1, 20)
	p := NewParser()
	if _, err := p.Write(wire); err != nil {
		t.Fatal(err)
	}

	f, err := p.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if !bytes.Equal(f.Payload, payload) {
		t.Errorf("payload = % x, want % x", f.Payload, payload)
	}
	if f.Header.Dest != 10 || f.Header.Src != 1 {
		t.Errorf("addresses = %d→%d, want 1→10", f.Header.Src, f.Header.Dest)
	}
	if _, err := p.Next(); !errors.Is(err, ErrNeedMore) {
		t.Errorf("second Next err = %v, want ErrNeedMore", err)
	}
	if p.Stats().FramesDecoded != 1 {
		t.Errorf("FramesDecoded = %d, want 1", p.Stats().FramesDecoded)
	}
}

func TestParserBackToBackFrames(t *testing.T) {
	p := NewParser()
	var stream []byte
	for i := range 5 {
		w, _ := mkframe(t, uint16(10+i), 1, i*7)
		stream = append(stream, w...)
	}
	if _, err := p.Write(stream); err != nil {
		t.Fatal(err)
	}

	for i := range 5 {
		f, err := p.Next()
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if want := uint16(10 + i); f.Header.Dest != want {
			t.Errorf("frame %d: dest = %d, want %d", i, f.Header.Dest, want)
		}
		if len(f.Payload) != i*7 {
			t.Errorf("frame %d: payload %d octets, want %d", i, len(f.Payload), i*7)
		}
	}
	if _, err := p.Next(); !errors.Is(err, ErrNeedMore) {
		t.Errorf("err = %v, want ErrNeedMore", err)
	}
}

// TestParserByteAtATime feeds the stream one octet at a time, which is the
// worst case a real socket can produce and the case most likely to expose an
// offset bug.
func TestParserByteAtATime(t *testing.T) {
	wire, payload := mkframe(t, 10, 1, 100)
	p := NewParser()

	for i, b := range wire {
		if _, err := p.Write([]byte{b}); err != nil {
			t.Fatal(err)
		}
		f, err := p.Next()
		if i < len(wire)-1 {
			if !errors.Is(err, ErrNeedMore) {
				t.Fatalf("octet %d: err = %v, want ErrNeedMore", i, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("final octet: %v", err)
		}
		if !bytes.Equal(f.Payload, payload) {
			t.Error("payload mismatch after byte-at-a-time feed")
		}
	}
}

func TestParserResyncAfterGarbage(t *testing.T) {
	wire, payload := mkframe(t, 10, 1, 16)
	garbage := []byte{0xDE, 0xAD, 0xBE, 0xEF, 0x05, 0x00, 0x64}

	p := NewParser()
	if _, err := p.Write(append(append([]byte{}, garbage...), wire...)); err != nil {
		t.Fatal(err)
	}

	f, err := p.Next()
	if err != nil {
		t.Fatalf("Next after garbage: %v", err)
	}
	if !bytes.Equal(f.Payload, payload) {
		t.Error("payload mismatch after resync")
	}
	if p.Stats().Resyncs == 0 {
		t.Error("Resyncs counter did not move")
	}
	if p.Stats().BytesDiscarded == 0 {
		t.Error("BytesDiscarded counter did not move")
	}
}

func TestParserRecoversAfterCorruptFrame(t *testing.T) {
	// A frame whose header CRC is wrong must cost only that frame. The good
	// frame behind it still has to come out.
	bad, _ := mkframe(t, 10, 1, 24)
	bad[8] ^= 0xFF // corrupt the header CRC
	good, payload := mkframe(t, 11, 1, 24)

	p := NewParser()
	if _, err := p.Write(append(bad, good...)); err != nil {
		t.Fatal(err)
	}

	f, err := p.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if f.Header.Dest != 11 {
		t.Errorf("recovered the wrong frame: dest %d", f.Header.Dest)
	}
	if !bytes.Equal(f.Payload, payload) {
		t.Error("payload mismatch")
	}
	if p.Stats().HeaderCRCErrors != 1 {
		t.Errorf("HeaderCRCErrors = %d, want 1", p.Stats().HeaderCRCErrors)
	}
}

func TestParserRecoversAfterBodyCRCError(t *testing.T) {
	bad, _ := mkframe(t, 10, 1, 32)
	bad[len(bad)-1] ^= 0xFF // corrupt the final block CRC
	good, payload := mkframe(t, 11, 1, 8)

	p := NewParser()
	if _, err := p.Write(append(bad, good...)); err != nil {
		t.Fatal(err)
	}

	f, err := p.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if f.Header.Dest != 11 || !bytes.Equal(f.Payload, payload) {
		t.Errorf("recovered frame = %v, want the good one", f)
	}
	if p.Stats().BodyCRCErrors != 1 {
		t.Errorf("BodyCRCErrors = %d, want 1", p.Stats().BodyCRCErrors)
	}
}

// TestParserTrailingStartByte covers the split-delimiter case: a read that
// ends on 0x05 whose 0x64 arrives in the next read.
func TestParserTrailingStartByte(t *testing.T) {
	wire, payload := mkframe(t, 10, 1, 4)
	p := NewParser()

	if _, err := p.Write([]byte{0xAA, 0xBB, wire[0]}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Next(); !errors.Is(err, ErrNeedMore) {
		t.Fatalf("err = %v, want ErrNeedMore", err)
	}
	if _, err := p.Write(wire[1:]); err != nil {
		t.Fatal(err)
	}

	f, err := p.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if !bytes.Equal(f.Payload, payload) {
		t.Error("payload mismatch across a split delimiter")
	}
}

// TestParserStaysBounded runs a long stream through a parser and asserts the
// backing array never grows. A parser that reallocates per frame would leak
// steadily on a session that runs for months.
func TestParserStaysBounded(t *testing.T) {
	p := NewParser()
	startCap := cap(p.buf)

	r := rand.New(rand.NewPCG(3, 5))
	for range 5000 {
		wire, _ := mkframe(t, 10, 1, r.IntN(MaxPayload+1))
		if _, err := p.Write(wire); err != nil {
			t.Fatal(err)
		}
		for {
			if _, err := p.Next(); errors.Is(err, ErrNeedMore) {
				break
			} else if err != nil {
				t.Fatal(err)
			}
		}
	}
	if cap(p.buf) != startCap {
		t.Errorf("buffer grew from %d to %d octets", startCap, cap(p.buf))
	}
	if p.Buffered() != 0 {
		t.Errorf("%d octets left buffered", p.Buffered())
	}
	if p.Stats().FramesDecoded != 5000 {
		t.Errorf("FramesDecoded = %d, want 5000", p.Stats().FramesDecoded)
	}
}

func TestParserNoAllocationsPerFrame(t *testing.T) {
	wire, _ := mkframe(t, 10, 1, 250)
	p := NewParser()

	got := testing.AllocsPerRun(200, func() {
		if _, err := p.Write(wire); err != nil {
			t.Fatal(err)
		}
		if _, err := p.Next(); err != nil {
			t.Fatal(err)
		}
	})
	if got != 0 {
		t.Errorf("%.1f allocations per frame, want 0", got)
	}
}

// TestParserWriteRefusesRatherThanDropping pins the contract that a full
// buffer produces a short write and an error, never a silent discard.
func TestParserWriteRefusesRatherThanDropping(t *testing.T) {
	p := NewParser()
	huge := bytes.Repeat([]byte{0xAA}, parserBufSize*3)

	n, err := p.Write(huge)
	if !errors.Is(err, ErrBufferFull) {
		t.Fatalf("err = %v, want ErrBufferFull", err)
	}
	if n != parserBufSize {
		t.Errorf("accepted %d octets, want %d", n, parserBufSize)
	}
	if p.Buffered() != parserBufSize {
		t.Errorf("buffered %d octets, want %d", p.Buffered(), parserBufSize)
	}

	// The refused octets are still the caller's to re-offer. After the parser
	// resyncs through the garbage there is room again.
	if _, err := p.Next(); !errors.Is(err, ErrNeedMore) {
		t.Fatalf("Next err = %v, want ErrNeedMore", err)
	}
	if p.Free() == 0 {
		t.Error("resyncing through garbage should have freed space")
	}
}

// TestParserDrainHandlesOversizedReads feeds Drain from a reader that returns
// far more than one frame at a time, which is what a fast peer over loopback
// looks like.
func TestParserDrainHandlesOversizedReads(t *testing.T) {
	var stream []byte
	const frames = 40
	for i := range frames {
		w, _ := mkframe(t, uint16(100+i), 1, 200)
		stream = append(stream, w...)
	}

	var got int
	p := NewParser()
	err := p.Drain(bytes.NewReader(stream), func(f Frame) error {
		if want := uint16(100 + got); f.Header.Dest != want {
			t.Errorf("frame %d: dest = %d, want %d", got, f.Header.Dest, want)
		}
		got++
		return nil
	})
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if got != frames {
		t.Errorf("decoded %d frames, want %d", got, frames)
	}
	if p.Stats().BytesDiscarded != 0 {
		t.Errorf("discarded %d octets from a clean stream", p.Stats().BytesDiscarded)
	}
}

func TestParserDrain(t *testing.T) {
	var stream []byte
	for i := range 3 {
		w, _ := mkframe(t, uint16(20+i), 1, 10)
		stream = append(stream, w...)
	}

	var got []uint16
	p := NewParser()
	err := p.Drain(bytes.NewReader(stream), func(f Frame) error {
		got = append(got, f.Header.Dest)
		return nil
	})
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if len(got) != 3 || got[0] != 20 || got[2] != 22 {
		t.Errorf("destinations = %v, want [20 21 22]", got)
	}
}

func TestParserDrainPropagatesCallbackError(t *testing.T) {
	wire, _ := mkframe(t, 10, 1, 4)
	sentinel := errors.New("stop")
	p := NewParser()
	err := p.Drain(bytes.NewReader(wire), func(Frame) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want the callback's error", err)
	}
}

func TestParserDrainStopsOnReadError(t *testing.T) {
	sentinel := errors.New("boom")
	p := NewParser()
	err := p.Drain(errReader{sentinel}, func(Frame) error { return nil })
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want the reader's error", err)
	}
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

var _ io.Reader = errReader{}

func BenchmarkParserThroughput(b *testing.B) {
	payload := make([]byte, 250)
	h := Header{Control: Control{Prm: true, Func: FuncUnconfirmedUserData}, Dest: 10, Src: 1, Length: 255}
	wire, _ := Encode(nil, h, payload)

	p := NewParser()
	b.SetBytes(int64(len(wire)))
	b.ReportAllocs()
	for b.Loop() {
		_, _ = p.Write(wire)
		_, _ = p.Next()
	}
}
