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
