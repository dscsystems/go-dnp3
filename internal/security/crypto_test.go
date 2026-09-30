package security

import (
	"bytes"
	"encoding/hex"
	"testing"
)

func TestRFC3394KnownVector(t *testing.T) {
	key, _ := hex.DecodeString("000102030405060708090a0b0c0d0e0f")
	plain, _ := hex.DecodeString("00112233445566778899aabbccddeeff")
	want, _ := hex.DecodeString("1fa68b0a8112b447aef34bd8fb5a7b829d3e862371d2cfe5")
	got, err := Wrap(key, plain)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("wrap %x: %v", got, err)
	}
	back, err := Unwrap(key, want)
	if err != nil || !bytes.Equal(back, plain) {
		t.Fatalf("unwrap %x: %v", back, err)
	}
	for i := range want {
		bad := append([]byte(nil), want...)
		bad[i] ^= 1
		if _, err := Unwrap(key, bad); err == nil {
			t.Fatalf("accepted corrupt byte %d", i)
		}
	}
}

// Vectors from RFC 3394 section 4, each cross-checked against OpenSSL's
// id-aes*-wrap. Between them they cover every key size a SAv5 update key can
// have (128 and 256 bits) and data of two, three and four blocks.
func TestRFC3394Vectors(t *testing.T) {
	data := "00112233445566778899aabbccddeeff000102030405060708090a0b0c0d0e0f"
	tests := []struct {
		name          string
		keyBytes, len int
		want          string
	}{
		{"128-bit KEK, 128-bit data", 16, 16, "1fa68b0a8112b447aef34bd8fb5a7b829d3e862371d2cfe5"},
		{"192-bit KEK, 128-bit data", 24, 16, "96778b25ae6ca435f92b5b97c050aed2468ab8a17ad84e5d"},
		{"256-bit KEK, 128-bit data", 32, 16, "64e8c3f9ce0f5ba263e9777905818a2a93c8191e7d6e8ae7"},
		{"192-bit KEK, 192-bit data", 24, 24, "031d33264e15d33268f24ec260743edce1c6c7ddee725a936ba814915c6762d2"},
		{"256-bit KEK, 256-bit data", 32, 32, "28c9f404c4b810f4cbccb35cfb87f8263f5786e2d80ed326cbc7f0e71a99f43bfb988b9b7a02dd21"},
		{"128-bit KEK, 256-bit data", 16, 32, "11826840774d993ff9c2fa02cca3cea0e93b1e1cf96361f93ea6dc2f345194e7b30f964c79f9e61d"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			key := make([]byte, tc.keyBytes)
			for i := range key {
				key[i] = byte(i)
			}
			plain, _ := hex.DecodeString(data)
			plain = plain[:tc.len]
			want, _ := hex.DecodeString(tc.want)

			got, err := Wrap(key, plain)
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("wrap = %x, %v; want %x", got, err, want)
			}
			back, err := Unwrap(key, want)
			if err != nil || !bytes.Equal(back, plain) {
				t.Fatalf("unwrap = %x, %v; want %x", back, err, plain)
			}
		})
	}
}

func TestUnwrapRejectsTheWrongKeyAndBadLengths(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 16)
	wrapped, err := Wrap(key, bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	wrong := bytes.Repeat([]byte{8}, 16)
	if _, err := Unwrap(wrong, wrapped); err == nil {
		t.Error("unwrapped with the wrong key")
	}
	for _, n := range []int{0, 8, 16, 23, 25} {
		if _, err := Unwrap(key, make([]byte, n)); err == nil {
			t.Errorf("unwrapped %d octets", n)
		}
	}
	for _, n := range []int{0, 8, 15, 17} {
		if _, err := Wrap(key, make([]byte, n)); err == nil {
			t.Errorf("wrapped %d octets", n)
		}
	}
	if _, err := Wrap(make([]byte, 5), make([]byte, 16)); err == nil {
		t.Error("wrapped under a key of no valid size")
	}
}

// RFC 4231 test case 2 for HMAC-SHA-256, truncated to the 16 octets SAv5's
// networked MAC (algorithm 4) uses. The parts are hashed as one message, so how
// the caller splits a message cannot change its MAC.
func TestMACIsTruncatedHMACSHA256(t *testing.T) {
	key, msg := []byte("Jefe"), []byte("what do ya want for nothing?")
	want, _ := hex.DecodeString("5bdcc146bf60754e6a042426089575c7")

	if got := MAC(key, msg); !bytes.Equal(got, want) {
		t.Errorf("MAC = %x, want %x", got, want)
	}
	if got := MAC(key, msg[:9], msg[9:20], msg[20:]); !bytes.Equal(got, want) {
		t.Errorf("a split message gave %x, want %x", got, want)
	}
	if got := MAC(key, msg, []byte{0}); bytes.Equal(got, want) {
		t.Error("appending an octet did not change the MAC")
	}
}
