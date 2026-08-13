package app

import (
	"errors"
	"fmt"
)

// Fragment header sizes.
const (
	// RequestHeaderSize is the application control octet plus the function
	// code.
	RequestHeaderSize = 2
	// ResponseHeaderSize adds the two internal indication octets.
	ResponseHeaderSize = 4

	// SeqModulus is the application sequence space. Four bits, distinct from
	// the transport function's six.
	SeqModulus = 16

	// DefaultMaxFragment is the standard's default maximum application
	// fragment size.
	DefaultMaxFragment = 2048
)

// Application control octet bit masks.
const (
	acFir = 0x80
	acFin = 0x40
	acCon = 0x20
	acUns = 0x10
	acSeq = 0x0F
)

// Control is the application control octet.
//
//	bit 7  FIR  first fragment of a response series
//	bit 6  FIN  final fragment of a response series
//	bit 5  CON  the sender requires an application-layer confirmation
//	bit 4  UNS  this fragment belongs to the unsolicited sequence space
//	bits 3-0    sequence number
type Control struct {
	Fir bool
	Fin bool
	Con bool
	Uns bool
	Seq uint8 // 0..15
}

// ParseControl decodes an application control octet.
func ParseControl(b byte) Control {
	return Control{
		Fir: b&acFir != 0,
		Fin: b&acFin != 0,
		Con: b&acCon != 0,
		Uns: b&acUns != 0,
		Seq: b & acSeq,
	}
}

// Byte encodes the application control octet.
func (c Control) Byte() byte {
	var b byte
	if c.Fir {
		b |= acFir
	}
	if c.Fin {
		b |= acFin
	}
	if c.Con {
		b |= acCon
	}
	if c.Uns {
		b |= acUns
	}
	return b | (c.Seq & acSeq)
}

// Single reports whether the fragment is both the first and the last of its
// series, which is the common case.
func (c Control) Single() bool { return c.Fir && c.Fin }

func (c Control) String() string {
	s := fmt.Sprintf("seq=%02d", c.Seq)
	for _, f := range []struct {
		on   bool
		name string
	}{{c.Fir, "FIR"}, {c.Fin, "FIN"}, {c.Con, "CON"}, {c.Uns, "UNS"}} {
		if f.on {
			s += " " + f.name
		}
	}
	return s
}

// Header is a decoded application fragment header.
//
// IIN is meaningful only when Func is a response code; for requests it is
// zero and IsResponse reports false.
type Header struct {
	Control Control
	Func    FuncCode
	IIN     IIN
}

// IsResponse reports whether the fragment carries an IIN field.
func (h Header) IsResponse() bool { return h.Func.IsResponse() }

// Size returns the encoded size of the header.
func (h Header) Size() int {
	if h.IsResponse() {
		return ResponseHeaderSize
	}
	return RequestHeaderSize
}

func (h Header) String() string {
	if h.IsResponse() {
		return fmt.Sprintf("%s %s iin=%s", h.Func, h.Control, h.IIN)
	}
	return fmt.Sprintf("%s %s", h.Func, h.Control)
}

// Errors returned when decoding a fragment header.
var (
	// ErrShortFragment means the fragment is too short to hold its header.
	ErrShortFragment = errors.New("app: fragment shorter than its header")
	// ErrTruncated means an object header or its data ran past the end of the
	// fragment.
	ErrTruncated = errors.New("app: truncated object data")
	// ErrBadQualifier means a qualifier octet used a reserved encoding.
	ErrBadQualifier = errors.New("app: reserved qualifier encoding")
	// ErrBadRange means a range field was internally inconsistent, such as a
	// stop index below its start.
	ErrBadRange = errors.New("app: invalid range")
	// ErrUnknownObject means the object's size could not be resolved, so the
	// fragment cannot be walked past this header.
	ErrUnknownObject = errors.New("app: unknown object size")
	// ErrFragmentTooLarge means the fragment exceeded the configured maximum.
	ErrFragmentTooLarge = errors.New("app: fragment exceeds maximum size")
)

// ParseHeader decodes the fragment header at the front of buf and returns the
// header with the number of octets it occupied.
func ParseHeader(buf []byte) (Header, int, error) {
	if len(buf) < RequestHeaderSize {
		return Header{}, 0, ErrShortFragment
	}

	h := Header{
		Control: ParseControl(buf[0]),
		Func:    FuncCode(buf[1]),
	}
	if !h.IsResponse() {
		return h, RequestHeaderSize, nil
	}
	if len(buf) < ResponseHeaderSize {
		return Header{}, 0, ErrShortFragment
	}
	h.IIN = ParseIIN(buf[2], buf[3])
	return h, ResponseHeaderSize, nil
}

// AppendHeader appends the encoded header to dst.
func AppendHeader(dst []byte, h Header) []byte {
	dst = append(dst, h.Control.Byte(), byte(h.Func))
	if h.IsResponse() {
		iin1, iin2 := h.IIN.Octets()
		dst = append(dst, iin1, iin2)
	}
	return dst
}
