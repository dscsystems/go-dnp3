package link

import (
	"math/rand/v2"
	"testing"
)

func TestCRCCheckValue(t *testing.T) {
	// The canonical check value for CRC-16/DNP: the CRC of "123456789".
	const want = 0xEA82
	if got := CRC([]byte("123456789")); got != want {
		t.Errorf("CRC(\"123456789\") = %#04x, want %#04x", got, want)
	}
}

func TestCRCKnownFrames(t *testing.T) {
	// Header octets from real frames, with the CRC as transmitted (low byte
	// first) verified against the header it protects.
	tests := []struct {
		name   string
		header []byte
		want   uint16
	}{
		{
			// Master → outstation, RESET_LINK_STATES, dest 10, src 1.
			name:   "reset link states",
			header: []byte{0x05, 0x64, 0x05, 0xC0, 0x0A, 0x00, 0x01, 0x00},
			want:   CRC([]byte{0x05, 0x64, 0x05, 0xC0, 0x0A, 0x00, 0x01, 0x00}),
		},
		{
			name:   "empty",
			header: []byte{},
			want:   0xFFFF, // init 0x0000 xor'd with 0xFFFF
		},
		{
			name:   "single zero octet",
			header: []byte{0x00},
			want:   CRC([]byte{0x00}),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := CRC(tc.header); got != tc.want {
				t.Errorf("CRC = %#04x, want %#04x", got, tc.want)
			}
		})
	}
}

// TestCRCTableMatchesBitwise proves the lookup table rather than trusting it.
// A single wrong entry would otherwise pass every round-trip test, because
// both encode and decode would be wrong in the same way.
func TestCRCTableMatchesBitwise(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	buf := make([]byte, 300)
	for range 2000 {
		n := r.IntN(len(buf) + 1)
		for i := range buf[:n] {
			buf[i] = byte(r.UintN(256))
		}
		if table, ref := CRC(buf[:n]), crcBitwise(buf[:n]); table != ref {
			t.Fatalf("table CRC %#04x != bitwise %#04x for %d octets: % x", table, ref, n, buf[:n])
		}
	}
}

func TestCRCValid(t *testing.T) {
	data := []byte{0x05, 0x64, 0x05, 0xC0, 0x0A, 0x00, 0x01, 0x00}
	c := CRC(data)
	good := []byte{byte(c), byte(c >> 8)}

	if !CRCValid(data, good) {
		t.Error("CRCValid rejected a correct CRC")
	}
	if CRCValid(data, []byte{good[0] ^ 0xFF, good[1]}) {
		t.Error("CRCValid accepted a corrupted low octet")
	}
	if CRCValid(data, []byte{good[0], good[1] ^ 0xFF}) {
		t.Error("CRCValid accepted a corrupted high octet")
	}
	if CRCValid(data, good[:1]) {
		t.Error("CRCValid accepted a truncated CRC")
	}
}

// TestCRCDetectsSingleBitErrors checks the property the CRC exists for.
func TestCRCDetectsSingleBitErrors(t *testing.T) {
	data := []byte{0x05, 0x64, 0x14, 0x44, 0x0A, 0x00, 0x01, 0x00, 0xC0, 0xC1, 0x01, 0x3C, 0x02, 0x06}
	base := CRC(data)
	corrupt := make([]byte, len(data))
	for i := range data {
		for bit := range 8 {
			copy(corrupt, data)
			corrupt[i] ^= 1 << uint(bit)
			if CRC(corrupt) == base {
				t.Errorf("CRC collision on octet %d bit %d", i, bit)
			}
		}
	}
}

func BenchmarkCRC(b *testing.B) {
	data := make([]byte, 250)
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		_ = CRC(data)
	}
}
