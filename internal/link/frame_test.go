package link

import (
	"bytes"
	"errors"
	"math/rand/v2"
	"testing"
)

func TestControlRoundTrip(t *testing.T) {
	// Every one of the 256 control octets must survive parse and re-encode.
	for i := range 256 {
		b := byte(i)
		if got := ParseControl(b).Byte(); got != b {
			t.Fatalf("control %#02x round-tripped to %#02x", b, got)
		}
	}
}

func TestControlFields(t *testing.T) {
	tests := []struct {
		name string
		b    byte
		want Control
	}{
		{
			name: "master reset link states",
			b:    0xC0,
			want: Control{Dir: true, Prm: true, Func: FuncResetLinkStates},
		},
		{
			name: "master confirmed user data fcb set",
			b:    0xF3,
			want: Control{Dir: true, Prm: true, Fcb: true, Fcv: true, Func: FuncConfirmedUserData},
		},
		{
			name: "outstation ack",
			b:    0x00,
			want: Control{Func: FuncAck},
		},
		{
			name: "outstation link status with dfc",
			b:    0x1B,
			want: Control{Fcv: true, Func: FuncLinkStatus},
		},
		{
			name: "master unconfirmed user data",
			b:    0xC4,
			want: Control{Dir: true, Prm: true, Func: FuncUnconfirmedUserData},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseControl(tc.b)
			if got != tc.want {
				t.Errorf("ParseControl(%#02x) = %+v, want %+v", tc.b, got, tc.want)
			}
			if got.Byte() != tc.b {
				t.Errorf("re-encode = %#02x, want %#02x", got.Byte(), tc.b)
			}
		})
	}
}

func TestControlDFCOnlyOnSecondary(t *testing.T) {
	// The same bit position is FCV on a primary frame and DFC on a secondary
	// one. Confusing them stalls a link permanently, so pin the distinction.
	primary := ParseControl(0xF3)   // DIR|PRM|FCB|FCV, confirmed user data
	secondary := ParseControl(0x1B) // FCV position set, link status
	if primary.Dfc() {
		t.Error("primary frame reported DFC")
	}
	if !secondary.Dfc() {
		t.Error("secondary frame with the bit set did not report DFC")
	}
}

func TestFrameSize(t *testing.T) {
	tests := []struct{ payload, want int }{
		{0, 10},    // header only
		{1, 13},    // header + 1 octet + CRC
		{16, 28},   // header + one full block
		{17, 31},   // header + one full block + 1 octet block
		{250, 292}, // the maximum frame
	}
	for _, tc := range tests {
		if got := FrameSize(tc.payload); got != tc.want {
			t.Errorf("FrameSize(%d) = %d, want %d", tc.payload, got, tc.want)
		}
	}
	if MaxFrameSize != 292 {
		t.Errorf("MaxFrameSize = %d, want 292", MaxFrameSize)
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	sizes := []int{0, 1, 15, 16, 17, 31, 32, 33, 100, 249, 250}
	r := rand.New(rand.NewPCG(7, 11))

	for _, n := range sizes {
		payload := make([]byte, n)
		for i := range payload {
			payload[i] = byte(r.UintN(256))
		}
		h := Header{
			Control: Control{Dir: true, Prm: true, Fcb: true, Fcv: true, Func: FuncConfirmedUserData},
			Dest:    1024,
			Src:     1,
			Length:  uint8(MinLength + n),
		}

		wire, err := Encode(nil, h, payload)
		if err != nil {
			t.Fatalf("payload %d: Encode: %v", n, err)
		}
		if len(wire) != FrameSize(n) {
			t.Fatalf("payload %d: encoded %d octets, want %d", n, len(wire), FrameSize(n))
		}

		f, consumed, err := Decode(wire, nil)
		if err != nil {
			t.Fatalf("payload %d: Decode: %v", n, err)
		}
		if consumed != len(wire) {
			t.Errorf("payload %d: consumed %d, want %d", n, consumed, len(wire))
		}
		if f.Header != h {
			t.Errorf("payload %d: header = %+v, want %+v", n, f.Header, h)
		}
		if !bytes.Equal(f.Payload, payload) {
			t.Errorf("payload %d: payload mismatch", n)
		}
	}
}

func TestEncodeKnownFrame(t *testing.T) {
	// A master's RESET_LINK_STATES to outstation 10 from master 1 is a fixed
	// ten-octet frame. Pinning it catches endianness and field-order slips
	// that a round-trip test cannot.
	h := Header{
		Control: Control{Dir: true, Prm: true, Func: FuncResetLinkStates},
		Dest:    10,
		Src:     1,
		Length:  MinLength,
	}
	got, err := Encode(nil, h, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{0x05, 0x64, 0x05, 0xC0, 0x0A, 0x00, 0x01, 0x00}
	want = appendCRC(want, want)
	if !bytes.Equal(got, want) {
		t.Errorf("frame = % x\nwant     % x", got, want)
	}
}

func TestEncodeRejectsOversizePayload(t *testing.T) {
	_, err := Encode(nil, Header{}, make([]byte, MaxPayload+1))
	if !errors.Is(err, ErrPayloadTooLong) {
		t.Errorf("err = %v, want ErrPayloadTooLong", err)
	}
}

func TestEncodeAppendsWithoutAllocating(t *testing.T) {
	buf := make([]byte, 0, MaxFrameSize)
	h := Header{Control: Control{Prm: true, Func: FuncUnconfirmedUserData}, Length: MinLength + 250}
	out, err := Encode(buf, h, make([]byte, 250))
	if err != nil {
		t.Fatal(err)
	}
	if cap(out) != cap(buf) {
		t.Errorf("Encode grew the buffer: cap %d → %d", cap(buf), cap(out))
	}
}

func TestDecodeErrors(t *testing.T) {
	valid, err := Encode(nil, Header{
		Control: Control{Prm: true, Func: FuncUnconfirmedUserData},
		Dest:    10, Src: 1, Length: MinLength + 20,
	}, make([]byte, 20))
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		mut  func([]byte) []byte
		want error
	}{
		{"empty", func([]byte) []byte { return nil }, ErrShortFrame},
		{"truncated header", func(b []byte) []byte { return b[:9] }, ErrShortFrame},
		{"truncated body", func(b []byte) []byte { return b[:len(b)-1] }, ErrShortFrame},
		{"bad start", func(b []byte) []byte { c := clone(b); c[0] = 0x06; return c }, ErrBadStart},
		{"header crc", func(b []byte) []byte { c := clone(b); c[8] ^= 0xFF; return c }, ErrHeaderCRC},
		{"body crc", func(b []byte) []byte { c := clone(b); c[len(c)-1] ^= 0xFF; return c }, ErrBodyCRC},
		{"corrupt payload", func(b []byte) []byte { c := clone(b); c[12] ^= 0xFF; return c }, ErrBodyCRC},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := Decode(tc.mut(valid), nil)
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestDecodeBadLength(t *testing.T) {
	// LEN below the five-octet minimum is malformed even with a valid CRC,
	// so the header has to be re-CRC'd after tampering to reach the check.
	hdr := []byte{0x05, 0x64, 0x04, 0xC4, 0x0A, 0x00, 0x01, 0x00}
	hdr = appendCRC(hdr, hdr)
	if _, err := DecodeHeader(hdr); !errors.Is(err, ErrBadLength) {
		t.Errorf("err = %v, want ErrBadLength", err)
	}
}

func TestBroadcastAndReservedAddresses(t *testing.T) {
	for _, a := range []uint16{0xFFFD, 0xFFFE, 0xFFFF} {
		if !IsBroadcast(a) {
			t.Errorf("%#04x should be broadcast", a)
		}
	}
	for _, a := range []uint16{0, 1, 65532, 0xFFF0, 0xFFFB} {
		if IsBroadcast(a) {
			t.Errorf("%#04x should not be broadcast", a)
		}
	}
	if !IsReserved(0xFFF0) || !IsReserved(0xFFFB) {
		t.Error("reserved range boundaries misclassified")
	}
	if IsReserved(SelfAddress) {
		t.Error("the self-address is usable, not reserved")
	}
	if IsReserved(0xFFEF) {
		t.Error("0xFFEF is below the reserved range")
	}
}

func clone(b []byte) []byte { return append([]byte(nil), b...) }

func BenchmarkEncode(b *testing.B) {
	buf := make([]byte, 0, MaxFrameSize)
	payload := make([]byte, 250)
	h := Header{Control: Control{Prm: true, Func: FuncUnconfirmedUserData}, Length: 255}
	b.SetBytes(int64(FrameSize(250)))
	for b.Loop() {
		_, _ = Encode(buf[:0], h, payload)
	}
}

func BenchmarkDecode(b *testing.B) {
	h := Header{Control: Control{Prm: true, Func: FuncUnconfirmedUserData}, Dest: 10, Src: 1, Length: 255}
	wire, _ := Encode(nil, h, make([]byte, 250))
	payload := make([]byte, MaxPayload)
	b.SetBytes(int64(len(wire)))
	for b.Loop() {
		_, _, _ = Decode(wire, payload)
	}
}
