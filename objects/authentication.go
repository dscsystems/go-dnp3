package objects

import (
	"encoding/binary"
	"fmt"

	"github.com/dscsystems/go-dnp3"
)

var ErrAuthenticationObject = fmt.Errorf("%w: authentication object", dnp3.ErrMalformed)

// AuthChallenge is g120v1. Algorithm 4 is SHA-256 HMAC truncated to 16 bytes.
type AuthChallenge struct {
	Sequence          uint32
	User              uint16
	Algorithm, Reason uint8
	Data              []byte
}

func AppendAuthChallenge(dst []byte, v AuthChallenge) []byte {
	dst = binary.LittleEndian.AppendUint32(dst, v.Sequence)
	dst = binary.LittleEndian.AppendUint16(dst, v.User)
	dst = append(dst, v.Algorithm, v.Reason)
	return append(dst, v.Data...)
}
func ParseAuthChallenge(data []byte) (AuthChallenge, error) {
	if len(data) < 12 {
		return AuthChallenge{}, ErrAuthenticationObject
	}
	return AuthChallenge{binary.LittleEndian.Uint32(data), binary.LittleEndian.Uint16(data[4:]), data[6], data[7], append([]byte(nil), data[8:]...)}, nil
}

// AuthReply is g120v2.
type AuthReply struct {
	Sequence uint32
	User     uint16
	MAC      []byte
}

func AppendAuthReply(dst []byte, v AuthReply) []byte {
	dst = binary.LittleEndian.AppendUint32(dst, v.Sequence)
	dst = binary.LittleEndian.AppendUint16(dst, v.User)
	return append(dst, v.MAC...)
}
func ParseAuthReply(data []byte) (AuthReply, error) {
	if len(data) < 7 {
		return AuthReply{}, ErrAuthenticationObject
	}
	return AuthReply{binary.LittleEndian.Uint32(data), binary.LittleEndian.Uint16(data[4:]), append([]byte(nil), data[6:]...)}, nil
}

// AuthKeyStatus is g120v5. MAC authenticates the most recent key-change ASDU.
type AuthKeyStatus struct {
	Sequence                         uint32
	User                             uint16
	WrapAlgorithm, Status, Algorithm uint8
	Challenge, MAC                   []byte
}

func AppendAuthKeyStatus(dst []byte, v AuthKeyStatus) []byte {
	dst = binary.LittleEndian.AppendUint32(dst, v.Sequence)
	dst = binary.LittleEndian.AppendUint16(dst, v.User)
	dst = append(dst, v.WrapAlgorithm, v.Status, v.Algorithm)
	dst = binary.LittleEndian.AppendUint16(dst, uint16(len(v.Challenge)))
	dst = append(dst, v.Challenge...)
	return append(dst, v.MAC...)
}
func ParseAuthKeyStatus(data []byte) (AuthKeyStatus, error) {
	if len(data) < 11 {
		return AuthKeyStatus{}, ErrAuthenticationObject
	}
	n := int(binary.LittleEndian.Uint16(data[9:11]))
	if n < 4 || n > len(data)-11 {
		return AuthKeyStatus{}, ErrAuthenticationObject
	}
	return AuthKeyStatus{binary.LittleEndian.Uint32(data), binary.LittleEndian.Uint16(data[4:]), data[6], data[7], data[8], append([]byte(nil), data[11:11+n]...), append([]byte(nil), data[11+n:]...)}, nil
}

// AuthKeyChange is g120v6. Wrapped contains the RFC 3394 encrypted key data.
type AuthKeyChange struct {
	Sequence uint32
	User     uint16
	Wrapped  []byte
}

func AppendAuthKeyChange(dst []byte, v AuthKeyChange) []byte {
	dst = binary.LittleEndian.AppendUint32(dst, v.Sequence)
	dst = binary.LittleEndian.AppendUint16(dst, v.User)
	return append(dst, v.Wrapped...)
}
func ParseAuthKeyChange(data []byte) (AuthKeyChange, error) {
	if len(data) < 30 || (len(data)-6)%8 != 0 {
		return AuthKeyChange{}, ErrAuthenticationObject
	}
	return AuthKeyChange{binary.LittleEndian.Uint32(data), binary.LittleEndian.Uint16(data[4:]), append([]byte(nil), data[6:]...)}, nil
}
