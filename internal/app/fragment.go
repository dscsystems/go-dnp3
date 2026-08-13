package app

import (
	"fmt"
	"strings"
)

// Fragment is a fully parsed application fragment.
//
// Objects alias the buffer the fragment was parsed from and are invalidated
// when that buffer is reused.
type Fragment struct {
	Header  Header
	Objects []ObjectHeader

	// Raw is the octets the fragment was parsed from.
	Raw []byte
}

func (f Fragment) String() string {
	var b strings.Builder
	b.WriteString(f.Header.String())
	for _, o := range f.Objects {
		b.WriteString("\n  ")
		b.WriteString(o.String())
	}
	return b.String()
}

// ParseFragment decodes a complete application fragment.
//
// sizer resolves object sizes; pass nil to use [DefaultSizer]. The returned
// fragment aliases buf.
//
// Parsing stops at the first malformed object header and returns an error
// along with the headers decoded so far, because a decoder showing an operator
// the three headers it understood before the corruption is more useful than
// one showing nothing.
func ParseFragment(sizer ObjectSizer, buf []byte) (Fragment, error) {
	if sizer == nil {
		sizer = DefaultSizer
	}

	h, n, err := ParseHeader(buf)
	if err != nil {
		return Fragment{Raw: buf}, err
	}

	frag := Fragment{Header: h, Raw: buf}
	carriesData := h.Func.CarriesObjectData()

	for off := n; off < len(buf); {
		oh, used, err := ParseObjectHeader(sizer, buf[off:], off, carriesData)
		if err != nil {
			return frag, fmt.Errorf("object header at offset %d: %w", off, err)
		}
		if used == 0 {
			// A header that consumes nothing would loop forever. No valid
			// encoding does this, but a parser must not depend on that.
			return frag, fmt.Errorf("%w: zero-length object header at offset %d", ErrTruncated, off)
		}
		frag.Objects = append(frag.Objects, oh)
		off += used
	}

	return frag, nil
}

// ParseRequest decodes a fragment and confirms it is a request.
func ParseRequest(sizer ObjectSizer, buf []byte) (Fragment, error) {
	f, err := ParseFragment(sizer, buf)
	if err != nil {
		return f, err
	}
	if f.Header.IsResponse() {
		return f, fmt.Errorf("app: expected a request, got %s", f.Header.Func)
	}
	return f, nil
}

// ParseResponse decodes a fragment and confirms it is a response.
func ParseResponse(sizer ObjectSizer, buf []byte) (Fragment, error) {
	f, err := ParseFragment(sizer, buf)
	if err != nil {
		return f, err
	}
	if !f.Header.IsResponse() {
		return f, fmt.Errorf("app: expected a response, got %s", f.Header.Func)
	}
	return f, nil
}

// ---------- Building ----------

// Builder assembles an application fragment.
//
// It enforces the fragment size limit as headers are added, so a caller
// discovers a fragment will not fit while it can still do something about it —
// splitting the response across fragments — rather than after encoding.
type Builder struct {
	buf []byte
	max int
	// headerWritten guards against emitting object headers before the
	// fragment header.
	headerWritten bool
}

// NewBuilder returns a builder capped at max octets. Pass zero for
// [DefaultMaxFragment].
func NewBuilder(max int) *Builder {
	if max <= 0 {
		max = DefaultMaxFragment
	}
	return &Builder{buf: make([]byte, 0, max), max: max}
}

// Reset clears the builder for reuse, keeping its buffer.
func (b *Builder) Reset() {
	b.buf = b.buf[:0]
	b.headerWritten = false
}

// Len returns the octets written so far.
func (b *Builder) Len() int { return len(b.buf) }

// Remaining returns how many octets can still be written.
func (b *Builder) Remaining() int { return b.max - len(b.buf) }

// Bytes returns the fragment built so far. It aliases the builder's buffer and
// is invalidated by Reset or any further write.
func (b *Builder) Bytes() []byte { return b.buf }

// SetHeader writes the fragment header. It must be called before any object
// header.
func (b *Builder) SetHeader(h Header) error {
	if b.headerWritten {
		return fmt.Errorf("app: fragment header already written")
	}
	if h.Size() > b.Remaining() {
		return ErrFragmentTooLarge
	}
	b.buf = AppendHeader(b.buf, h)
	b.headerWritten = true
	return nil
}

// AddObject appends an object header and its data.
//
// It returns ErrFragmentTooLarge when the object would overflow the fragment,
// leaving the builder unchanged so the caller can close this fragment and
// start the next.
func (b *Builder) AddObject(h ObjectHeader) error {
	if !b.headerWritten {
		return fmt.Errorf("app: object header written before the fragment header")
	}
	if h.Size() > b.Remaining() {
		return ErrFragmentTooLarge
	}
	b.buf = AppendObjectHeader(b.buf, h)
	return nil
}

// Fits reports whether an object header would still fit.
func (b *Builder) Fits(h ObjectHeader) bool { return h.Size() <= b.Remaining() }

// BuildRequest is the short form for a request carrying zero or more object
// headers.
func BuildRequest(dst []byte, c Control, fc FuncCode, objects ...ObjectHeader) []byte {
	dst = AppendHeader(dst, Header{Control: c, Func: fc})
	for _, o := range objects {
		dst = AppendObjectHeader(dst, o)
	}
	return dst
}

// BuildResponse is the short form for a response carrying zero or more object
// headers.
func BuildResponse(dst []byte, c Control, fc FuncCode, iin IIN, objects ...ObjectHeader) []byte {
	dst = AppendHeader(dst, Header{Control: c, Func: fc, IIN: iin})
	for _, o := range objects {
		dst = AppendObjectHeader(dst, o)
	}
	return dst
}

// ReadAllObjects builds the object header that asks for every object of a
// group and variation — qualifier 0x06. Class polls are expressed this way:
// group 60 variation 1 for static data, variations 2 through 4 for the event
// classes.
func ReadAllObjects(group, variation uint8) ObjectHeader {
	return ObjectHeader{
		Group:     group,
		Variation: variation,
		Qualifier: MakeQualifier(PrefixNone, RangeAllObjects),
		Range:     Range{Spec: RangeAllObjects},
	}
}

// ReadRange builds the object header that asks for an inclusive index range,
// choosing the narrowest range encoding that fits.
func ReadRange(group, variation uint8, start, stop uint32) ObjectHeader {
	spec := RangeStartStop32
	switch {
	case stop <= 0xFF:
		spec = RangeStartStop8
	case stop <= 0xFFFF:
		spec = RangeStartStop16
	}
	return ObjectHeader{
		Group:     group,
		Variation: variation,
		Qualifier: MakeQualifier(PrefixNone, spec),
		Range:     Range{Spec: spec, Start: start, Stop: stop, Count: stop - start + 1},
	}
}
