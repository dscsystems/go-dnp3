// Package decoder turns DNP3 octets into a structured trace.
//
// It produces a tree, not log strings. One consumer renders it to a log, one
// to a terminal UI, one to text for the command-line decoder — and none of
// them re-implement any parsing. That is the whole point: there is exactly one
// place in this repository that knows how to read a DNP3 frame.
package decoder

import (
	"fmt"
	"strings"

	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/internal/link"
	"github.com/dscsystems/go-dnp3/internal/transport"
	"github.com/dscsystems/go-dnp3/objects"
)

// Direction says which way octets travelled.
type Direction uint8

// Directions.
const (
	DirUnknown Direction = iota
	DirTx
	DirRx
)

func (d Direction) String() string {
	switch d {
	case DirTx:
		return "TX"
	case DirRx:
		return "RX"
	default:
		return "--"
	}
}

// LinkInfo describes one decoded link frame.
type LinkInfo struct {
	Control link.Control
	Dest    uint16
	Src     uint16
	Length  uint8
	// PayloadLen is the user data the frame carried.
	PayloadLen int
	// FrameSize is the total octets the frame occupied on the wire.
	FrameSize int
}

// TransportInfo describes one decoded transport segment.
type TransportInfo struct {
	Header transport.Header
	// Complete is set when this segment completed an application fragment.
	Complete bool
	// Discarded says what the reassembler dropped, if anything.
	Discarded transport.DiscardReason
}

// AppInfo describes a decoded application fragment.
type AppInfo struct {
	Header  app.Header
	Objects []app.ObjectHeader
	// Values holds the measurements decoded from each object header, indexed
	// to match Objects. Entries are nil for headers that carry no
	// measurements — class objects, commands, times.
	Values [][]Value
	// Err is set when the fragment header parsed but the object headers did
	// not. The headers decoded before the failure are still in Objects,
	// because showing an operator what was understood before the corruption
	// beats showing nothing.
	Err error
}

// Trace is everything decodable about one link frame.
//
// A frame always yields link and transport information. It yields application
// information only when it completed a fragment, since a fragment can span
// nine frames and only the last one finishes it.
type Trace struct {
	Direction Direction
	Link      LinkInfo
	Transport *TransportInfo
	App       *AppInfo

	// Raw is the frame's octets as they appeared on the wire.
	Raw []byte
	// Err is set when the frame itself could not be decoded.
	Err error
}

// Decoder reassembles a stream of octets into traces.
//
// It holds link and transport state, so one Decoder belongs to one direction
// of one connection. Feeding both directions into a single decoder would
// interleave two independent transport sequences and produce nonsense.
type Decoder struct {
	dir    Direction
	parser *link.Parser
	reasm  *transport.Reassembler
	sizer  app.ObjectSizer

	// synchronized records whether the outstation's clock is believed to be
	// set, which decides the quality stamped on every timestamp decoded. A
	// session updates it from the NEED_TIME internal indication.
	synchronized bool
}

// New returns a decoder for one direction of one connection. Pass a nil sizer
// to use the application layer's default.
//
// Timestamps are treated as synchronized until told otherwise; call
// [Decoder.SetSynchronized] from a session that has seen NEED_TIME. An offline
// tool has no way to know, and marking every timestamp in a capture as
// unsynchronized would be a claim the octets do not support either way.
func New(dir Direction, sizer app.ObjectSizer) *Decoder {
	if sizer == nil {
		sizer = app.DefaultSizer
	}
	return &Decoder{
		dir:          dir,
		parser:       link.NewParser(),
		reasm:        transport.NewReassembler(0),
		sizer:        sizer,
		synchronized: true,
	}
}

// SetSynchronized records whether the outstation's clock is set, which decides
// the quality stamped on decoded timestamps.
func (d *Decoder) SetSynchronized(v bool) { d.synchronized = v }

// Reset clears link and transport state, as when a connection is
// re-established.
func (d *Decoder) Reset() {
	d.parser = link.NewParser()
	d.reasm.Reset()
}

// Stats returns the underlying parser and reassembler counters.
func (d *Decoder) Stats() (link.Stats, transport.Stats) {
	return d.parser.Stats(), d.reasm.Stats()
}

// Feed decodes octets and invoked fn for each frame found.
//
// Octets that do not yet form a complete frame are buffered until they do.
func (d *Decoder) Feed(data []byte, fn func(Trace)) {
	for len(data) > 0 {
		n, _ := d.parser.Write(data)
		if n == 0 {
			// The buffer is full of octets that cannot form a frame. Drain
			// what we can and drop forward rather than spinning.
			d.drain(fn)
			if n, _ = d.parser.Write(data); n == 0 {
				return
			}
		}
		data = data[n:]
		d.drain(fn)
	}
}

func (d *Decoder) drain(fn func(Trace)) {
	for {
		f, err := d.parser.Next()
		if err != nil {
			return
		}
		fn(d.trace(f))
	}
}

// trace builds the trace for one decoded link frame, advancing transport
// reassembly and parsing the application fragment when one completes.
func (d *Decoder) trace(f link.Frame) Trace {
	raw, _ := link.Encode(nil, f.Header, f.Payload)

	t := Trace{
		Direction: d.dir,
		Raw:       raw,
		Link: LinkInfo{
			Control:    f.Header.Control,
			Dest:       f.Header.Dest,
			Src:        f.Header.Src,
			Length:     f.Header.Length,
			PayloadLen: len(f.Payload),
			FrameSize:  link.FrameSize(len(f.Payload)),
		},
	}

	// Only user-data frames carry a transport segment. A link ACK or a link
	// status reply has no payload above it.
	fn := f.Header.Control.Func
	if !f.Header.Control.Prm ||
		(fn != link.FuncConfirmedUserData && fn != link.FuncUnconfirmedUserData) {
		return t
	}
	if len(f.Payload) == 0 {
		return t
	}

	res := d.reasm.Accept(f.Payload)
	ti := TransportInfo{
		Header:    transport.ParseHeader(f.Payload[0]),
		Complete:  res.Complete(),
		Discarded: res.Discarded,
	}
	t.Transport = &ti

	if !res.Complete() {
		return t
	}

	frag, err := app.ParseFragment(d.sizer, res.Fragment)
	t.App = &AppInfo{
		Header:  frag.Header,
		Objects: frag.Objects,
		Values:  d.decodeValues(frag.Objects),
		Err:     err,
	}
	return t
}

// decodeValues decodes the measurements in each object header, threading the
// common time of occurrence forward.
//
// A group 51 object sets the base that any relative-time event *after it in
// the same fragment* is measured from, so this has to walk the headers in
// order and carry the context along rather than decoding each independently.
func (d *Decoder) decodeValues(headers []app.ObjectHeader) [][]Value {
	if len(headers) == 0 {
		return nil
	}

	ctx := objects.Context{Synchronized: d.synchronized}
	out := make([][]Value, len(headers))

	for i, h := range headers {
		if h.Group == groupCTO && len(h.Data) >= objects.Time48Size {
			ctx = ctx.WithCTO(objects.ParseTime48(h.Data).Time)
			continue
		}
		if vals, ok := DecodeValues(h, ctx); ok {
			out[i] = vals
		}
	}
	return out
}

// groupCTO is the common-time-of-occurrence group, whose objects set the base
// for the relative-time event variations that follow them.
const groupCTO = 51

// DecodeFrame decodes a single self-contained frame without any session state.
//
// It is the one-shot form used by offline tools: a frame pasted from a capture
// is assumed to carry a complete fragment, which is true for the single-frame
// messages that make up most DNP3 traffic. Multi-frame fragments need a
// [Decoder], which carries the reassembly state across frames.
func DecodeFrame(sizer app.ObjectSizer, data []byte) (Trace, int, error) {
	// Decode once up front purely to learn the frame's length, so the caller
	// gets a precise error and an accurate consumed count rather than having
	// to infer either from the stream decoder.
	_, n, err := link.Decode(data, nil)
	if err != nil {
		return Trace{Raw: data, Err: err}, 0, err
	}

	d := New(DirUnknown, sizer)
	var out Trace
	var found bool
	d.Feed(data[:n], func(t Trace) {
		out, found = t, true
	})
	if !found {
		return Trace{Raw: data[:n], Err: link.ErrShortFrame}, n, link.ErrShortFrame
	}
	return out, n, nil
}

// ---------- Rendering ----------

// Render writes a human-readable form of the trace to b.
//
// The layout is a layer tree, indented, so an operator can see at a glance
// which layer a problem lives in.
func (t Trace) Render(b *strings.Builder, showHex bool) {
	fmt.Fprintf(b, "%s  %s\n", t.Direction, t.linkLine())

	if t.Err != nil {
		fmt.Fprintf(b, "      error: %v\n", t.Err)
		return
	}

	if t.Transport != nil {
		fmt.Fprintf(b, "      transport  %s", t.Transport.Header)
		if t.Transport.Discarded != transport.DiscardNone {
			fmt.Fprintf(b, "  DISCARDED: %s", t.Transport.Discarded)
		}
		b.WriteByte('\n')
	}

	if t.App != nil {
		fmt.Fprintf(b, "      application  %s\n", t.App.Header)
		for i, o := range t.App.Objects {
			fmt.Fprintf(b, "        g%dv%d  %-22s %-14s %d object(s)",
				o.Group, o.Variation, o.Qualifier, o.Range, o.Count())
			if len(o.Data) > 0 {
				fmt.Fprintf(b, "  %d octets", len(o.Data))
			}
			b.WriteByte('\n')

			if i < len(t.App.Values) {
				for _, v := range t.App.Values[i] {
					fmt.Fprintf(b, "          %s\n", v)
				}
			}
		}
		if t.App.Err != nil {
			fmt.Fprintf(b, "        error: %v\n", t.App.Err)
		}
	}

	if showHex {
		writeHex(b, t.Raw, "      ")
	}
}

func (t Trace) linkLine() string {
	l := t.Link
	return fmt.Sprintf("link  %s  %d→%d  len=%d  frame=%dB",
		l.Control, l.Src, l.Dest, l.Length, l.FrameSize)
}

// String renders the trace without the hex dump.
func (t Trace) String() string {
	var b strings.Builder
	t.Render(&b, false)
	return strings.TrimRight(b.String(), "\n")
}

// writeHex writes a classic offset / hex / ASCII dump.
func writeHex(b *strings.Builder, data []byte, indent string) {
	const perLine = 16
	for off := 0; off < len(data); off += perLine {
		end := min(off+perLine, len(data))
		line := data[off:end]

		fmt.Fprintf(b, "%s%04x  ", indent, off)
		for i := range perLine {
			if i < len(line) {
				fmt.Fprintf(b, "%02x ", line[i])
			} else {
				b.WriteString("   ")
			}
			if i == 7 {
				b.WriteByte(' ')
			}
		}
		b.WriteString(" |")
		for _, c := range line {
			if c >= 0x20 && c < 0x7F {
				b.WriteByte(c)
			} else {
				b.WriteByte('.')
			}
		}
		b.WriteString("|\n")
	}
}

// HexDump returns a standalone hex dump of data.
func HexDump(data []byte) string {
	var b strings.Builder
	writeHex(&b, data, "")
	return b.String()
}
