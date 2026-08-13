package link

import (
	"bytes"
	"errors"
	"io"
)

// Stats counts what the parser saw. Every discard reason gets its own counter
// because "the link is flaky" is not an actionable diagnosis — knowing whether
// the losses are CRC errors, bad lengths, or garbage between frames is.
type Stats struct {
	FramesDecoded   uint64
	BytesDiscarded  uint64
	HeaderCRCErrors uint64
	BodyCRCErrors   uint64
	BadLength       uint64
	Resyncs         uint64
}

// parserBufSize holds two maximum frames, so a read that straddles a frame
// boundary never forces a slide mid-frame.
const parserBufSize = MaxFrameSize * 2

// delimiter is package-level so the resync scan does not allocate it per call.
var delimiter = []byte{StartByte0, StartByte1}

// Parser turns a byte stream into frames.
//
// It is resynchronizing: a corrupted frame costs the frames it overlaps, not
// the connection. On any framing failure the parser discards one octet, scans
// forward to the next 0x0564, and tries again — which is what lets a stack
// survive line noise, a peer that half-closes mid-frame, or a device that
// emits a malformed frame every few thousand messages.
//
// The parser allocates once, at construction. Buffered octets live in a fixed
// array addressed by read and write offsets; decoding slides them to the front
// rather than reallocating, so a session running for months holds steady.
//
// A Parser is not safe for concurrent use. One belongs to one connection.
type Parser struct {
	buf  []byte // fixed backing store of parserBufSize octets
	r, w int    // buffered octets are buf[r:w]

	stats Stats

	// payload backs the frame returned by Next, so decoding does not allocate.
	// The returned Frame aliases it and is valid only until the next call.
	payload [MaxPayload]byte
}

// NewParser returns a parser sized to hold two maximum frames.
func NewParser() *Parser {
	return &Parser{buf: make([]byte, parserBufSize)}
}

// Stats returns a snapshot of the parser's counters.
func (p *Parser) Stats() Stats { return p.stats }

// Buffered returns the number of unconsumed octets held by the parser.
func (p *Parser) Buffered() int { return p.w - p.r }

// Free returns how many octets Write can accept without discarding.
func (p *Parser) Free() int { return len(p.buf) - p.Buffered() }

// ErrNeedMore means the buffered octets do not yet contain a complete frame.
// It is the normal way Next reports "read more from the socket" and is not a
// protocol error.
var ErrNeedMore = errors.New("link: need more data")

// ErrBufferFull means Write could not accept every octet offered. Drain the
// complete frames with Next and write the remainder.
//
// The parser refuses octets rather than dropping them because a link layer
// that discards silently produces the worst class of field bug: frames that
// vanish with nothing in the logs to say why.
var ErrBufferFull = errors.New("link: parser buffer full")

// Write appends received octets, satisfying io.Writer.
//
// It accepts as much as the buffer can hold and returns ErrBufferFull with a
// short count if the caller offered more. The buffer holds two maximum frames,
// so a reader that drains complete frames between reads of MaxFrameSize octets
// — which [Parser.Drain] does — never sees a short write.
func (p *Parser) Write(b []byte) (int, error) {
	// Only the tail of the array is writable, so reclaim the consumed prefix
	// before measuring room. Comparing against total free space instead would
	// let the copy below truncate silently.
	if len(b) > len(p.buf)-p.w {
		p.slide()
	}
	n := copy(p.buf[p.w:], b)
	p.w += n
	if n < len(b) {
		return n, ErrBufferFull
	}
	return n, nil
}

// Next decodes the next frame from the buffered octets.
//
// It returns ErrNeedMore when more input is required. The returned frame's
// Payload aliases the parser's internal buffer and is invalidated by the next
// call to Next; copy it if it must outlive that.
func (p *Parser) Next() (Frame, error) {
	for {
		buf := p.buf[p.r:p.w]

		if len(buf) < HeaderSize {
			return Frame{}, ErrNeedMore
		}

		if buf[0] != StartByte0 || buf[1] != StartByte1 {
			p.resync()
			continue
		}

		f, n, err := Decode(buf, p.payload[:])
		switch {
		case err == nil:
			p.r += n
			p.stats.FramesDecoded++
			return f, nil

		case errors.Is(err, ErrShortFrame):
			// The frame is well-formed so far but incomplete. Anything up to
			// MaxFrameSize might still arrive, so wait rather than resync.
			return Frame{}, ErrNeedMore

		case errors.Is(err, ErrHeaderCRC):
			p.stats.HeaderCRCErrors++
		case errors.Is(err, ErrBodyCRC):
			p.stats.BodyCRCErrors++
		case errors.Is(err, ErrBadLength):
			p.stats.BadLength++
		}

		// Drop the leading octet so the resync scan cannot re-match the
		// delimiter it just rejected, then hunt for the next one.
		p.r++
		p.stats.BytesDiscarded++
		p.resync()
	}
}

// resync discards octets up to the next 0x0564 delimiter.
func (p *Parser) resync() {
	p.stats.Resyncs++

	buf := p.buf[p.r:p.w]
	i := bytes.Index(buf, delimiter)
	switch {
	case i < 0:
		// No delimiter in what we hold. Keep a trailing 0x05, since its 0x64
		// may be in the next read, and discard everything before it.
		keep := 0
		if len(buf) > 0 && buf[len(buf)-1] == StartByte0 {
			keep = 1
		}
		p.stats.BytesDiscarded += uint64(len(buf) - keep)
		p.r = p.w - keep
	case i > 0:
		p.stats.BytesDiscarded += uint64(i)
		p.r += i
	}
	p.slide()
}

// slide moves buffered octets to the front of the backing array, reclaiming
// the space consumed frames left behind. It copies at most one frame's worth.
func (p *Parser) slide() {
	if p.r == 0 {
		return
	}
	p.w = copy(p.buf, p.buf[p.r:p.w])
	p.r = 0
}

// Drain reads r to exhaustion, invoking fn for each decoded frame. It returns
// when r reports an error, propagating anything but io.EOF.
//
// The frame passed to fn is only valid for the duration of the call.
func (p *Parser) Drain(r io.Reader, fn func(Frame) error) error {
	chunk := make([]byte, MaxFrameSize)
	for {
		n, rerr := r.Read(chunk)

		// Writing at most one frame's worth and draining after each write
		// keeps the buffer from filling, but loop anyway rather than assume
		// it: a short write that went unnoticed would corrupt the stream.
		for pending := chunk[:n]; len(pending) > 0; {
			written, werr := p.Write(pending)
			pending = pending[written:]

			for {
				f, err := p.Next()
				if errors.Is(err, ErrNeedMore) {
					break
				}
				if err != nil {
					return err
				}
				if err := fn(f); err != nil {
					return err
				}
			}

			if werr != nil && written == 0 && p.Buffered() == len(p.buf) {
				// The buffer is full of octets that will never form a frame.
				// Drop the leading octet and resync rather than spin.
				p.r++
				p.stats.BytesDiscarded++
				p.resync()
			}
		}

		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return nil
			}
			return rerr
		}
	}
}
