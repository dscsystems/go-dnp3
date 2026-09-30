// Package security implements the symmetric primitives used by DNP3 SAv5.
package security

import (
	"crypto/aes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"errors"
)

var ErrAuthentication = errors.New("DNP3 authentication failed")

// MAC computes the networked SAv5 SHA-256 HMAC (algorithm 4).
func MAC(key []byte, parts ...[]byte) []byte {
	h := hmac.New(sha256.New, key)
	for _, p := range parts {
		_, _ = h.Write(p)
	}
	return h.Sum(nil)[:16]
}

// Wrap implements RFC 3394 using the default initialization vector.
func Wrap(key, plaintext []byte) ([]byte, error) {
	if len(plaintext) < 16 || len(plaintext)%8 != 0 {
		return nil, ErrAuthentication
	}
	c, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(plaintext)+8)
	for i := range 8 {
		out[i] = 0xa6
	}
	copy(out[8:], plaintext)
	n := len(plaintext) / 8
	var b [16]byte
	for j := 0; j < 6; j++ {
		for i := 1; i <= n; i++ {
			copy(b[:8], out[:8])
			copy(b[8:], out[8*i:8*i+8])
			c.Encrypt(b[:], b[:])
			binary.BigEndian.PutUint64(out[:8], binary.BigEndian.Uint64(b[:8])^uint64(n*j+i))
			copy(out[8*i:8*i+8], b[8:])
		}
	}
	return out, nil
}

// Unwrap verifies the RFC 3394 integrity check before returning plaintext.
func Unwrap(key, wrapped []byte) ([]byte, error) {
	if len(wrapped) < 24 || len(wrapped)%8 != 0 {
		return nil, ErrAuthentication
	}
	c, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	out := append([]byte(nil), wrapped...)
	n := len(out)/8 - 1
	var b [16]byte
	for j := 5; j >= 0; j-- {
		for i := n; i >= 1; i-- {
			binary.BigEndian.PutUint64(b[:8], binary.BigEndian.Uint64(out[:8])^uint64(n*j+i))
			copy(b[8:], out[8*i:8*i+8])
			c.Decrypt(b[:], b[:])
			copy(out[:8], b[:8])
			copy(out[8*i:8*i+8], b[8:])
		}
	}
	iv := []byte{0xa6, 0xa6, 0xa6, 0xa6, 0xa6, 0xa6, 0xa6, 0xa6}
	if subtle.ConstantTimeCompare(out[:8], iv) != 1 {
		clear(out)
		return nil, ErrAuthentication
	}
	return out[8:], nil
}
