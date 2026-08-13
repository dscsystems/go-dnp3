package link

import (
	"bytes"
	"testing"
)

// FuzzParser feeds arbitrary octets to the frame parser.
//
// A DNP3 stack parses bytes from field devices it does not control, over links
// that corrupt them. The parser must never panic, never allocate without
// bound, and never emit a frame that does not round-trip back to the octets it
// claims to have consumed.
func FuzzParser(f *testing.F) {
	// Seed with a valid frame, a corrupted one, and the shapes most likely to
	// trip an offset bug.
	valid, _ := Encode(nil, Header{
		Control: Control{Dir: true, Prm: true, Func: FuncUnconfirmedUserData},
		Dest:    10, Src: 1, Length: MinLength + 5,
	}, []byte{0xC0, 0xC1, 0x01, 0x3C, 0x02})
	f.Add(valid)
	f.Add(append(append([]byte{}, valid...), valid...))
	f.Add(append([]byte{0xFF, 0xFF}, valid...))
	f.Add([]byte{0x05, 0x64})
	f.Add([]byte{0x05, 0x64, 0x05, 0xC0, 0x0A, 0x00, 0x01, 0x00})
	f.Add([]byte{0x05, 0x64, 0xFF, 0xC4, 0x0A, 0x00, 0x01, 0x00, 0x00, 0x00})
	f.Add([]byte{0x05, 0x05, 0x05, 0x64, 0x64, 0x64})
	f.Add(bytes.Repeat([]byte{0x05, 0x64}, 200))

	f.Fuzz(func(t *testing.T, data []byte) {
		p := NewParser()

		err := p.Drain(bytes.NewReader(data), func(frame Frame) error {
			// Any frame the parser accepts must be internally consistent.
			if got := len(frame.Payload); got != frame.Header.PayloadLen() {
				t.Fatalf("payload is %d octets but the header declares %d",
					got, frame.Header.PayloadLen())
			}
			if frame.Header.Length < MinLength {
				t.Fatalf("accepted a frame with LEN=%d", frame.Header.Length)
			}
			if len(frame.Payload) > MaxPayload {
				t.Fatalf("accepted a %d octet payload", len(frame.Payload))
			}

			// And it must re-encode to exactly the octets it was decoded from.
			// This is the property that catches a decoder accepting something
			// its own encoder could never produce.
			wire, err := Encode(nil, frame.Header, frame.Payload)
			if err != nil {
				t.Fatalf("a decoded frame failed to re-encode: %v", err)
			}
			if len(wire) != FrameSize(len(frame.Payload)) {
				t.Fatalf("re-encoded to %d octets, want %d",
					len(wire), FrameSize(len(frame.Payload)))
			}
			again, n, err := Decode(wire, nil)
			if err != nil {
				t.Fatalf("re-encoded frame failed to decode: %v", err)
			}
			if n != len(wire) || again.Header != frame.Header ||
				!bytes.Equal(again.Payload, frame.Payload) {
				t.Fatal("frame did not survive a re-encode round trip")
			}
			return nil
		})
		if err != nil {
			t.Fatalf("Drain returned an unexpected error: %v", err)
		}

		// The parser's memory is fixed at construction, whatever it was fed.
		if cap(p.buf) != parserBufSize {
			t.Fatalf("parser buffer grew to %d octets", cap(p.buf))
		}
		if p.Buffered() > parserBufSize {
			t.Fatalf("parser holds %d octets, more than its buffer", p.Buffered())
		}
	})
}

// FuzzParserChunked feeds the same octets split at an arbitrary point, since a
// real socket delivers a stream in chunks the sender never chose. Splitting a
// frame must not change what comes out.
func FuzzParserChunked(f *testing.F) {
	valid, _ := Encode(nil, Header{
		Control: Control{Dir: true, Prm: true, Func: FuncUnconfirmedUserData},
		Dest:    10, Src: 1, Length: MinLength + 3,
	}, []byte{0xC0, 0xC1, 0x01})
	f.Add(valid, uint8(4))
	f.Add(append(append([]byte{}, valid...), valid...), uint8(13))
	f.Add([]byte{0x05, 0x64, 0x05}, uint8(2))

	f.Fuzz(func(t *testing.T, data []byte, splitAt uint8) {
		whole := drainAll(t, data)

		split := int(splitAt)
		if len(data) > 0 {
			split %= len(data) + 1
		} else {
			split = 0
		}
		chunked := drainAll(t, data[:split], data[split:])

		if len(whole) != len(chunked) {
			t.Fatalf("whole feed produced %d frames, split feed %d", len(whole), len(chunked))
		}
		for i := range whole {
			if whole[i].Header != chunked[i].Header {
				t.Fatalf("frame %d header differs between whole and split feeds", i)
			}
			if !bytes.Equal(whole[i].Payload, chunked[i].Payload) {
				t.Fatalf("frame %d payload differs between whole and split feeds", i)
			}
		}
	})
}

// drainAll feeds each chunk through the parser's documented contract — write,
// then drain every complete frame, then write the remainder — and returns the
// frames decoded, with payloads copied so they survive later calls.
//
// Going through Drain rather than calling Write directly is what makes the
// whole-versus-split comparison meaningful: Write is allowed to accept a short
// count, so a test that ignored that would be comparing two different amounts
// of input rather than two chunkings of the same input.
func drainAll(t *testing.T, chunks ...[]byte) []Frame {
	t.Helper()
	p := NewParser()
	var out []Frame
	collect := func(f Frame) error {
		f.Payload = append([]byte(nil), f.Payload...)
		out = append(out, f)
		return nil
	}
	for _, c := range chunks {
		if err := p.Drain(bytes.NewReader(c), collect); err != nil {
			t.Fatalf("Drain: %v", err)
		}
	}
	return out
}

// FuzzDecode targets Decode directly, without the parser's resync in front of
// it, so malformed input reaches the block-CRC walk unfiltered.
func FuzzDecode(f *testing.F) {
	f.Add([]byte{0x05, 0x64, 0x05, 0xC0, 0x0A, 0x00, 0x01, 0x00, 0x00, 0x00})
	f.Add(bytes.Repeat([]byte{0x00}, MaxFrameSize))
	f.Add([]byte{0x05, 0x64, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF})

	buf := make([]byte, MaxPayload)
	f.Fuzz(func(t *testing.T, data []byte) {
		frame, n, err := Decode(data, buf)
		if err != nil {
			return
		}
		if n > len(data) {
			t.Fatalf("Decode consumed %d octets from a %d octet buffer", n, len(data))
		}
		if n != FrameSize(len(frame.Payload)) {
			t.Fatalf("consumed %d octets for a %d octet payload", n, len(frame.Payload))
		}
		if len(frame.Payload) > MaxPayload {
			t.Fatalf("decoded a %d octet payload", len(frame.Payload))
		}
	})
}

// FuzzSecondary drives the secondary state machine with arbitrary frames. It
// must always answer a primary frame and never hand up a payload it did not
// receive.
func FuzzSecondary(f *testing.F) {
	f.Add(byte(0xC0), uint16(10), uint16(1), []byte(nil))
	f.Add(byte(0xF3), uint16(10), uint16(1), []byte{0xC0, 0x01})
	f.Add(byte(0x00), uint16(1), uint16(10), []byte(nil))

	f.Fuzz(func(t *testing.T, ctrl byte, dest, src uint16, payload []byte) {
		if len(payload) > MaxPayload {
			payload = payload[:MaxPayload]
		}
		s := &Secondary{LocalAddr: dest}
		frame := Frame{
			Header: Header{
				Control: ParseControl(ctrl),
				Dest:    dest, Src: src,
				Length: uint8(MinLength + len(payload)),
			},
			Payload: payload,
		}

		// Run the same frame repeatedly: a state machine that leaks state
		// across identical inputs will diverge.
		for range 4 {
			res := s.OnFrame(frame)

			if res.Payload != nil && !bytes.Equal(res.Payload, payload) {
				t.Fatal("secondary delivered a payload it did not receive")
			}
			if res.Reply != nil {
				if res.Reply.Header.Control.Prm {
					t.Fatal("a secondary reply must have PRM clear")
				}
				if res.Reply.Header.Src != dest || res.Reply.Header.Dest != src {
					t.Fatalf("reply addressed %d→%d, want %d→%d",
						res.Reply.Header.Src, res.Reply.Header.Dest, dest, src)
				}
				if _, err := Encode(nil, res.Reply.Header, res.Reply.Payload); err != nil {
					t.Fatalf("secondary produced an unencodable reply: %v", err)
				}
			} else if frame.Header.Control.Prm &&
				frame.Header.Control.Func != FuncUnconfirmedUserData {
				t.Fatalf("no reply to primary function %v", frame.Header.Control.Func)
			}
		}
	})
}
