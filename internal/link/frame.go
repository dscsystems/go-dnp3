// Package link implements the DNP3 data link layer: frame encoding and
// decoding, the CRC, and the primary and secondary station state machines.
//
// Nothing here performs I/O. Frames are encoded into caller-supplied buffers
// and decoded from byte slices; the state machines are driven by explicit
// event calls and report what they want sent by returning it. Sessions supply
// the socket and the clock.
package link

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Wire constants fixed by IEEE 1815 clause 9.
const (
	// StartByte0 and StartByte1 are the 0x0564 frame delimiter.
	StartByte0 = 0x05
	StartByte1 = 0x64

	// HeaderSize is the fixed part of every frame: two start octets, length,
	// control, destination, source and the header CRC.
	HeaderSize = 10

	// BlockSize is the payload octet count per CRC-protected block.
	BlockSize = 16
	// CRCSize is the octet count of one CRC.
	CRCSize = 2

	// MinLength and MaxLength bound the LEN field, which counts the control,
	// address and payload octets but excludes every CRC.
	MinLength = 5
	MaxLength = 255

	// MaxPayload is the largest user-data payload one frame can carry,
	// MaxLength minus the five octets of control and addresses.
	MaxPayload = MaxLength - MinLength // 250

	// MaxFrameSize is the largest frame on the wire: a full header plus 250
	// payload octets spread over sixteen CRC-protected blocks.
	MaxFrameSize = HeaderSize + MaxPayload + 16*CRCSize // 292
)

// Reserved and broadcast addresses.
const (
	// BroadcastNoConfirm addresses every outstation and requests no
	// application confirmation.
	BroadcastNoConfirm uint16 = 0xFFFF
	// BroadcastMandatoryConfirm addresses every outstation and requires an
	// application confirmation.
	BroadcastMandatoryConfirm uint16 = 0xFFFE
	// BroadcastOptionalConfirm addresses every outstation, leaving the
	// confirmation to the outstation's discretion.
	BroadcastOptionalConfirm uint16 = 0xFFFD
	// SelfAddress lets a master address an outstation without knowing its
	// configured address. Level 3.
	SelfAddress uint16 = 0xFFFC

	reservedLow  uint16 = 0xFFF0
	reservedHigh uint16 = 0xFFFB
)

// IsBroadcast reports whether addr is one of the three broadcast addresses.
func IsBroadcast(addr uint16) bool {
	return addr >= BroadcastOptionalConfirm && addr <= BroadcastNoConfirm
}

// IsReserved reports whether addr falls in the reserved range, which a
// conforming device must not use as its own address.
func IsReserved(addr uint16) bool {
	return addr >= reservedLow && addr <= reservedHigh
}

// IsValidSource reports whether addr can be the source of a frame.
//
// A source address is the sending station's own, so none of the addresses no
// station may hold can appear there: the reserved range, the self-address a
// master uses to reach an outstation whose address it does not know, and the
// three broadcast addresses. Together they are the whole of 0xFFF0-0xFFFF.
//
// A frame claiming one has to be discarded rather than answered, because a
// reply is addressed to the source it came from. Answering a request that
// claims a broadcast source produces a broadcast-addressed reply, which every
// station on the line accepts as its own.
func IsValidSource(addr uint16) bool {
	return !IsReserved(addr) && !IsBroadcast(addr) && addr != SelfAddress
}

// Function is the four-bit function code in the control octet. The same
// numeric value means different things depending on the PRM bit, so the
// constants are split into two blocks and Function.String needs to be told
// which direction the frame travelled.
type Function uint8

// Primary-to-secondary function codes (PRM = 1).
const (
	FuncResetLinkStates     Function = 0
	FuncTestLinkStates      Function = 2
	FuncConfirmedUserData   Function = 3
	FuncUnconfirmedUserData Function = 4
	FuncRequestLinkStatus   Function = 9
)

// Secondary-to-primary function codes (PRM = 0).
const (
	FuncAck          Function = 0
	FuncNack         Function = 1
	FuncLinkStatus   Function = 11
	FuncNotSupported Function = 15
)

var primaryFuncNames = map[Function]string{
	FuncResetLinkStates:     "RESET_LINK_STATES",
	FuncTestLinkStates:      "TEST_LINK_STATES",
	FuncConfirmedUserData:   "CONFIRMED_USER_DATA",
	FuncUnconfirmedUserData: "UNCONFIRMED_USER_DATA",
	FuncRequestLinkStatus:   "REQUEST_LINK_STATUS",
}

var secondaryFuncNames = map[Function]string{
	FuncAck:          "ACK",
	FuncNack:         "NACK",
	FuncLinkStatus:   "LINK_STATUS",
	FuncNotSupported: "NOT_SUPPORTED",
}

// Name returns the function code's name for the given direction.
func (f Function) Name(primary bool) string {
	tbl := secondaryFuncNames
	if primary {
		tbl = primaryFuncNames
	}
	if n, ok := tbl[f]; ok {
		return n
	}
	return fmt.Sprintf("FUNC_%d", uint8(f))
}

// Control is the link control octet.
//
//	bit 7  DIR   direction: set when sent from the master station
//	bit 6  PRM   primary message
//	bit 5  FCB   frame count bit, toggled per confirmed transmission
//	bit 4  FCV   frame count valid (primary) / DFC data flow control (secondary)
//	bits 3-0     function code
type Control struct {
	Dir  bool
	Prm  bool
	Fcb  bool
	Fcv  bool // DFC when Prm is false
	Func Function
}

// Control octet bit masks.
const (
	ctrlDir  = 0x80
	ctrlPrm  = 0x40
	ctrlFcb  = 0x20
	ctrlFcv  = 0x10
	ctrlFunc = 0x0F
)

// Byte encodes the control octet.
func (c Control) Byte() byte {
	var b byte
	if c.Dir {
		b |= ctrlDir
	}
	if c.Prm {
		b |= ctrlPrm
	}
	if c.Fcb {
		b |= ctrlFcb
	}
	if c.Fcv {
		b |= ctrlFcv
	}
	return b | byte(c.Func)&ctrlFunc
}

// ParseControl decodes a control octet.
func ParseControl(b byte) Control {
	return Control{
		Dir:  b&ctrlDir != 0,
		Prm:  b&ctrlPrm != 0,
		Fcb:  b&ctrlFcb != 0,
		Fcv:  b&ctrlFcv != 0,
		Func: Function(b & ctrlFunc),
	}
}

// Dfc returns the data-flow-control bit, which occupies the FCV position on
// frames from a secondary station. A set DFC means the secondary's buffers are
// full and the primary must stop sending user data.
func (c Control) Dfc() bool { return !c.Prm && c.Fcv }

func (c Control) String() string {
	dir := "MSTR→OUTS"
	if !c.Dir {
		dir = "OUTS→MSTR"
	}
	s := fmt.Sprintf("%s %s", dir, c.Func.Name(c.Prm))
	if c.Prm {
		if c.Fcv {
			s += fmt.Sprintf(" FCB=%d FCV", b2i(c.Fcb))
		}
	} else if c.Fcv {
		s += " DFC"
	}
	return s
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Header is the fixed part of a link frame.
type Header struct {
	Control Control
	Dest    uint16
	Src     uint16
	// Length is the LEN octet: five plus the payload size.
	Length uint8
}

// PayloadLen returns the payload octet count the header declares.
func (h Header) PayloadLen() int { return int(h.Length) - MinLength }

// Frame is a decoded link frame.
type Frame struct {
	Header Header
	// Payload is the reassembled user data with block CRCs stripped. It
	// aliases the caller's buffer when decoded by Decode; copy it if it must
	// outlive the buffer.
	Payload []byte
}

// Errors returned by frame decoding. They are wrapped by callers, so classify
// with errors.Is rather than comparing directly.
var (
	// ErrShortFrame means the buffer holds fewer octets than the frame needs.
	// It is not a protocol violation: the caller should read more.
	ErrShortFrame = errors.New("link: incomplete frame")
	// ErrBadStart means the buffer does not begin with 0x0564.
	ErrBadStart = errors.New("link: bad start octets")
	// ErrBadLength means the LEN octet is outside 5..255.
	ErrBadLength = errors.New("link: length out of range")
	// ErrHeaderCRC means the header CRC did not verify.
	ErrHeaderCRC = errors.New("link: header CRC mismatch")
	// ErrBodyCRC means a body block CRC did not verify.
	ErrBodyCRC = errors.New("link: body CRC mismatch")
	// ErrPayloadTooLong means an encode was asked for more than 250 octets.
	ErrPayloadTooLong = errors.New("link: payload exceeds 250 octets")
)

// bodySize returns the on-the-wire octet count of a payload of n octets once
// it has been split into CRC-protected blocks.
func bodySize(n int) int {
	if n == 0 {
		return 0
	}
	blocks := (n + BlockSize - 1) / BlockSize
	return n + blocks*CRCSize
}

// FrameSize returns the total wire size of a frame carrying n payload octets.
func FrameSize(n int) int { return HeaderSize + bodySize(n) }

// Encode appends a complete frame to dst and returns the extended slice.
//
// Passing a nil dst allocates; passing a buffer with capacity
// MaxFrameSize does not. The header CRC and every block CRC are computed here,
// so callers never handle CRCs directly.
func Encode(dst []byte, h Header, payload []byte) ([]byte, error) {
	if len(payload) > MaxPayload {
		return nil, fmt.Errorf("%w: %d octets", ErrPayloadTooLong, len(payload))
	}

	start := len(dst)
	dst = append(dst,
		StartByte0, StartByte1,
		byte(MinLength+len(payload)),
		h.Control.Byte(),
		0, 0, // destination, filled below
		0, 0, // source, filled below
	)
	binary.LittleEndian.PutUint16(dst[start+4:], h.Dest)
	binary.LittleEndian.PutUint16(dst[start+6:], h.Src)
	dst = appendCRC(dst, dst[start:start+8])

	for off := 0; off < len(payload); off += BlockSize {
		end := min(off+BlockSize, len(payload))
		dst = append(dst, payload[off:end]...)
		dst = appendCRC(dst, payload[off:end])
	}
	return dst, nil
}

// DecodeHeader parses the fixed header from buf and verifies its CRC.
//
// It returns ErrShortFrame when buf holds fewer than HeaderSize octets, which
// callers treat as "read more" rather than as an error.
func DecodeHeader(buf []byte) (Header, error) {
	if len(buf) < HeaderSize {
		return Header{}, ErrShortFrame
	}
	if buf[0] != StartByte0 || buf[1] != StartByte1 {
		return Header{}, ErrBadStart
	}
	if !CRCValid(buf[:8], buf[8:10]) {
		return Header{}, ErrHeaderCRC
	}
	length := buf[2]
	if length < MinLength {
		return Header{}, fmt.Errorf("%w: LEN=%d", ErrBadLength, length)
	}
	return Header{
		Control: ParseControl(buf[3]),
		Dest:    binary.LittleEndian.Uint16(buf[4:6]),
		Src:     binary.LittleEndian.Uint16(buf[6:8]),
		Length:  length,
	}, nil
}

// Decode parses one complete frame from the front of buf.
//
// It returns the frame, the number of octets consumed, and an error. The
// returned payload is written into payloadBuf when that has enough capacity,
// avoiding an allocation on the receive path; pass nil to let Decode allocate.
func Decode(buf, payloadBuf []byte) (Frame, int, error) {
	h, err := DecodeHeader(buf)
	if err != nil {
		return Frame{}, 0, err
	}

	payloadLen := h.PayloadLen()
	total := FrameSize(payloadLen)
	if len(buf) < total {
		return Frame{}, 0, ErrShortFrame
	}

	payload := payloadBuf[:0]
	if cap(payload) < payloadLen {
		payload = make([]byte, 0, payloadLen)
	}

	body := buf[HeaderSize:total]
	for off := 0; off < payloadLen; off += BlockSize {
		n := min(BlockSize, payloadLen-off)
		block := body[:n]
		if !CRCValid(block, body[n:n+CRCSize]) {
			return Frame{}, 0, fmt.Errorf("%w at payload offset %d", ErrBodyCRC, off)
		}
		payload = append(payload, block...)
		body = body[n+CRCSize:]
	}

	return Frame{Header: h, Payload: payload}, total, nil
}

func (f Frame) String() string {
	return fmt.Sprintf("[%s] %d→%d len=%d payload=%dB",
		f.Header.Control, f.Header.Src, f.Header.Dest, f.Header.Length, len(f.Payload))
}
