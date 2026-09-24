package outstation

import (
	"encoding/binary"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/objects"
)

// staticTypes is the order a class 0 response reports point types in. It
// matches the group numbering, which is what masters and analysers expect to
// see.
var staticTypes = []dnp3.PointType{
	dnp3.TypeBinary,
	dnp3.TypeDoubleBitBinary,
	dnp3.TypeBinaryOutputStatus,
	dnp3.TypeCounter,
	dnp3.TypeFrozenCounter,
	dnp3.TypeAnalog,
	dnp3.TypeAnalogOutputStatus,
	dnp3.TypeOctetString,
}

// responseBuilder accumulates object headers into fragments, starting a new
// fragment when the current one fills.
//
// Multi-fragment responses are the normal case for an integrity poll: a
// thousand analog points do not fit in 2048 octets, so the response is a
// series of fragments the master confirms one at a time.
type responseBuilder struct {
	max       int
	fragments [][]byte
	cur       []byte
	ctx       objects.Context
}

func newResponseBuilder(maxFragment int, ctx objects.Context) *responseBuilder {
	if maxFragment <= 0 {
		maxFragment = app.DefaultMaxFragment
	}
	return &responseBuilder{max: maxFragment, ctx: ctx}
}

// room returns how many octets remain in the current fragment, leaving space
// for the response header that will be prepended.
func (b *responseBuilder) room() int {
	return b.max - app.ResponseHeaderSize - len(b.cur)
}

// flush ends the current fragment.
func (b *responseBuilder) flush() {
	if len(b.cur) > 0 {
		b.fragments = append(b.fragments, b.cur)
		b.cur = nil
	}
}

// add appends an object header, starting a new fragment if it does not fit.
func (b *responseBuilder) add(h app.ObjectHeader) {
	if h.Size() > b.room() && len(b.cur) > 0 {
		b.flush()
	}
	b.cur = app.AppendObjectHeader(b.cur, h)
}

// done returns every accumulated fragment body. A response with no objects
// still produces one empty body, because an empty response is a real answer.
func (b *responseBuilder) done() [][]byte {
	b.flush()
	if len(b.fragments) == 0 {
		return [][]byte{nil}
	}
	return b.fragments
}

// ---------- Static data ----------

// buildStaticRange appends the points of one type over an index range.
//
// It emits a header per contiguous run that fits the current fragment, so a
// range spanning a fragment boundary is split into two headers rather than
// being truncated.
func (s *Session) buildStaticRange(b *responseBuilder, pt dnp3.PointType, variation uint8, start, stop uint16) {
	counts := s.db.Counts()
	limit := typeCount(counts, pt)
	if limit == 0 {
		return
	}
	if pt == dnp3.TypeOctetString {
		s.buildOctetStrings(b, start, min(stop, uint16(limit-1)))
		return
	}
	if int(stop) >= limit {
		stop = uint16(limit - 1)
	}
	if start > stop {
		return
	}

	if variation != 0 {
		// An explicit variation is the master's choice, and every point in the
		// range is reported as what it asked for.
		s.buildStaticRun(b, pt, variation, start, stop)
		return
	}

	// Variation zero means "each point's own default" — the static variation
	// its configuration set, which can differ from one point to the next. An
	// object header carries exactly one variation, so points that disagree
	// cannot share one: the range is reported as runs of consecutive points
	// that agree, each under its own header.
	//
	// Resolving the variation once from the first point and applying it to the
	// rest reports every later point in a variation it did not ask for. For a
	// float analog following an integer one, that means through the integer
	// codec, and the master reads back a truncated value it has no way to
	// know is wrong.
	//
	// The loop runs on int so a range ending at 0xFFFF cannot wrap.
	for idx := int(start); idx <= int(stop); {
		v, ok := s.staticVariation(pt, uint16(idx))
		if !ok {
			return
		}
		end := idx
		for end < int(stop) {
			next, ok := s.staticVariation(pt, uint16(end+1))
			if !ok || next != v {
				break
			}
			end++
		}
		s.buildStaticRun(b, pt, v, uint16(idx), uint16(end))
		idx = end + 1
	}
}

// forEachPointRun calls fn for each run of consecutive point indexes a
// request header names, and reports whether the header names points in a
// form the outstation understands:
//
//   - all objects: every point;
//   - a start-stop range: that range;
//   - a count with an index prefix: the listed indexes, consecutive ones
//     merged into a single run.
//
// A count with no index prefix says how many points but not which, and is
// left to the caller. Indexes above the 16-bit point space name no point: a
// range starting there is empty, one ending there stops at the top, and a
// listed one is skipped. Narrowing them to uint16 instead would wrap each onto
// a point that does exist.
func forEachPointRun(h app.ObjectHeader, fn func(start, stop uint16)) bool {
	spec := h.Range.Spec
	switch {
	case spec == app.RangeAllObjects:
		fn(0, 0xFFFF)

	case spec.IsStartStop():
		if h.Range.Start <= 0xFFFF {
			fn(uint16(h.Range.Start), uint16(min(h.Range.Stop, 0xFFFF)))
		}

	case spec.IsCount() && h.Qualifier.IndexPrefix().IsIndex():
		width := h.Qualifier.IndexPrefix().Octets()
		var first, last uint32
		open := false
		for off := 0; off+width <= len(h.Data); off += width {
			idx := readPrefix(h.Data[off:], width)
			switch {
			case idx > 0xFFFF:
				continue
			case open && idx == last+1:
				last = idx
				continue
			}
			if open {
				fn(uint16(first), uint16(last))
			}
			first, last, open = idx, idx, true
		}
		if open {
			fn(uint16(first), uint16(last))
		}

	default:
		return false
	}
	return true
}

// staticVariation returns the static variation a point is configured to be
// reported in.
func (s *Session) staticVariation(pt dnp3.PointType, index uint16) (uint8, bool) {
	_, cfg, ok := s.pointConfig(pt, index)
	return cfg.StaticVariation, ok
}

// buildStaticRun reports points start through stop, all in one variation,
// splitting across fragments as the space in each allows.
func (s *Session) buildStaticRun(b *responseBuilder, pt dnp3.PointType, variation uint8, start, stop uint16) {
	gv := staticGroupVar(pt, variation)
	d, ok := objects.Lookup(gv)
	if !ok {
		return
	}
	size, ok := d.SizeOctets()
	if !ok || size == 0 {
		return
	}

	// Indexes are carried as int: stepping a uint16 past a run that ends at
	// 0xFFFF wraps it to zero, and the loop never terminates.
	for idx := int(start); idx <= int(stop); {
		// How many points fit in what is left of the fragment, after the
		// header and its range field.
		const headerOverhead = app.ObjectHeaderSize + 4 // worst-case 16-bit range
		avail := b.room() - headerOverhead
		if avail < size {
			b.flush()
			avail = b.room() - headerOverhead
			if avail < size {
				return // a single object does not fit an empty fragment
			}
		}

		runLen := min(avail/size, int(stop)-idx+1)
		last := idx + runLen - 1

		data := make([]byte, 0, runLen*size)
		for i := idx; i <= last; i++ {
			data = s.encodeStatic(data, pt, gv, uint16(i), b.ctx)
		}

		b.add(rangeObjectHeader(gv, uint16(idx), uint16(last), data))
		idx = last + 1
	}
}

// encodeStatic appends one point's static encoding.
func (s *Session) encodeStatic(dst []byte, pt dnp3.PointType, gv objects.GroupVar, index uint16, ctx objects.Context) []byte {
	switch pt {
	case dnp3.TypeBinary:
		v, _, _ := s.db.Binary(index)
		if c, ok := objects.BinaryCodec(gv); ok {
			return c.Write(dst, v, ctx)
		}
	case dnp3.TypeDoubleBitBinary:
		v, _, _ := s.db.DoubleBit(index)
		if c, ok := objects.DoubleBitCodec(gv); ok {
			return c.Write(dst, v, ctx)
		}
	case dnp3.TypeCounter:
		v, _, _ := s.db.Counter(index)
		if c, ok := objects.CounterCodec(gv); ok {
			return c.Write(dst, v, ctx)
		}
	case dnp3.TypeFrozenCounter:
		v, _, _ := s.db.FrozenCounter(index)
		if c, ok := objects.FrozenCounterCodec(gv); ok {
			return c.Write(dst, v, ctx)
		}
	case dnp3.TypeAnalog:
		v, _, _ := s.db.Analog(index)
		if c, ok := objects.AnalogCodec(gv); ok {
			return c.Write(dst, v, ctx)
		}
	case dnp3.TypeBinaryOutputStatus:
		v, _, _ := s.db.BinaryOutputStatus(index)
		if c, ok := objects.BinaryOutputCodec(gv); ok {
			return c.Write(dst, v, ctx)
		}
	case dnp3.TypeAnalogOutputStatus:
		v, _, _ := s.db.AnalogOutputStatus(index)
		if c, ok := objects.AnalogOutputCodec(gv); ok {
			return c.Write(dst, v, ctx)
		}
	}
	return dst
}

// pointConfig returns a point's configuration.
func (s *Session) pointConfig(pt dnp3.PointType, index uint16) (any, PointConfig, bool) {
	switch pt {
	case dnp3.TypeBinary:
		v, c, ok := s.db.Binary(index)
		return v, c, ok
	case dnp3.TypeDoubleBitBinary:
		v, c, ok := s.db.DoubleBit(index)
		return v, c, ok
	case dnp3.TypeCounter:
		v, c, ok := s.db.Counter(index)
		return v, c, ok
	case dnp3.TypeFrozenCounter:
		v, c, ok := s.db.FrozenCounter(index)
		return v, c, ok
	case dnp3.TypeAnalog:
		v, c, ok := s.db.Analog(index)
		return v, c, ok
	case dnp3.TypeBinaryOutputStatus:
		v, c, ok := s.db.BinaryOutputStatus(index)
		return v, c, ok
	case dnp3.TypeAnalogOutputStatus:
		v, c, ok := s.db.AnalogOutputStatus(index)
		return v, c, ok
	}
	return nil, PointConfig{}, false
}

func typeCount(c DatabaseConfig, pt dnp3.PointType) int {
	switch pt {
	case dnp3.TypeBinary:
		return c.Binary
	case dnp3.TypeDoubleBitBinary:
		return c.DoubleBitBinary
	case dnp3.TypeCounter:
		return c.Counter
	case dnp3.TypeFrozenCounter:
		return c.FrozenCounter
	case dnp3.TypeAnalog:
		return c.Analog
	case dnp3.TypeBinaryOutputStatus:
		return c.BinaryOutputStatus
	case dnp3.TypeAnalogOutputStatus:
		return c.AnalogOutputStatus
	case dnp3.TypeOctetString:
		return c.OctetString
	}
	return 0
}

// rangeObjectHeader builds a header addressing an inclusive index range,
// choosing the narrowest range encoding that fits.
func rangeObjectHeader(gv objects.GroupVar, start, stop uint16, data []byte) app.ObjectHeader {
	spec := app.RangeStartStop16
	if stop <= 0xFF {
		spec = app.RangeStartStop8
	}
	return app.ObjectHeader{
		Group:     gv.Group,
		Variation: gv.Variation,
		Qualifier: app.MakeQualifier(app.PrefixNone, spec),
		Range: app.Range{
			Spec:  spec,
			Start: uint32(start),
			Stop:  uint32(stop),
			Count: uint32(stop-start) + 1,
		},
		Data: data,
	}
}

// buildOctetStrings appends octet string points.
//
// These need their own path because the variation number *is* the string's
// length: two points of different lengths cannot share an object header, so a
// range is emitted as one header per run of equal-length strings. Forcing them
// through the fixed-size path would report every string at one length and
// truncate or pad the rest.
func (s *Session) buildOctetStrings(b *responseBuilder, start, stop uint16) {
	if start > stop {
		return
	}

	// Indexes are carried as int for the same reason as in buildStaticRun:
	// stepping a uint16 past 0xFFFF wraps it to zero and the loop never ends.
	// Here the run arithmetic can also overflow well before the top — idx plus
	// a fragment's worth of strings passes 0xFFFF once idx is past about
	// 63,500 — which would put the end of a run before its start.
	for idx := int(start); idx <= int(stop); {
		v, _, ok := s.db.OctetString(uint16(idx))
		if !ok {
			return
		}
		// A zero-length string cannot be encoded: variation zero means "any
		// length" in a request and is not a valid response variation.
		length := max(len(v), 1)

		// Collect the run of following points with the same length.
		last := idx
		for last < int(stop) {
			next, _, ok := s.db.OctetString(uint16(last + 1))
			if !ok || max(len(next), 1) != length {
				break
			}
			last++
		}

		const headerOverhead = app.ObjectHeaderSize + 4
		for idx <= last {
			avail := b.room() - headerOverhead
			if avail < length {
				b.flush()
				avail = b.room() - headerOverhead
				if avail < length {
					return
				}
			}
			runEnd := min(idx+avail/length-1, last)

			data := make([]byte, 0, (runEnd-idx+1)*length)
			for i := idx; i <= runEnd; i++ {
				str, _, _ := s.db.OctetString(uint16(i))
				data = appendOctetString(data, str, length)
			}

			b.add(rangeObjectHeader(objects.GV(110, uint8(length)), uint16(idx), uint16(runEnd), data))
			idx = runEnd + 1
		}
	}
}

// appendOctetString writes one string padded or truncated to length, which the
// fixed-length variation requires.
func appendOctetString(dst []byte, v dnp3.OctetString, length int) []byte {
	if len(v) > length {
		v = v[:length]
	}
	dst = append(dst, v...)
	for range length - len(v) {
		dst = append(dst, 0)
	}
	return dst
}

// ---------- Events ----------

// eventGroup returns the group an event of a point type is reported in.
func eventGroup(pt dnp3.PointType) uint8 {
	switch pt {
	case dnp3.TypeBinary:
		return 2
	case dnp3.TypeDoubleBitBinary:
		return 4
	case dnp3.TypeBinaryOutputStatus:
		return 11
	case dnp3.TypeCounter:
		return 22
	case dnp3.TypeFrozenCounter:
		return 23
	case dnp3.TypeAnalog:
		return 32
	case dnp3.TypeAnalogOutputStatus:
		return 42
	case dnp3.TypeOctetString:
		return 111
	}
	return 0
}

// buildEvents appends event objects for the selected events.
//
// Events carry per-object index prefixes because the points that changed are
// not contiguous, and they are grouped into runs sharing a group and variation
// so a burst of analog changes becomes one header rather than fifty.
func (s *Session) buildEvents(b *responseBuilder, events []Event) {
	for i := 0; i < len(events); {
		gv := objects.GV(eventGroup(events[i].Type), events[i].Variation)

		// An octet string's size is its variation, not a table lookup: group
		// 111 has no descriptor row for a length to find. Consulting the
		// registry first would silently drop every string event.
		var size int
		if events[i].Type == dnp3.TypeOctetString {
			size = int(gv.Variation)
		} else {
			d, ok := objects.Lookup(gv)
			if !ok {
				i++
				continue
			}
			var okSize bool
			size, okSize = d.SizeOctets()
			if !okSize {
				i++
				continue
			}
		}
		if size == 0 {
			i++
			continue
		}

		// Collect the run of consecutive events sharing this encoding.
		j := i
		for j < len(events) &&
			eventGroup(events[j].Type) == gv.Group &&
			events[j].Variation == gv.Variation {
			j++
		}

		// Each event carries its point index as a prefix, and the prefix has to
		// be wide enough for every index in the run. A one-octet prefix holds
		// only 0-255: writing index 300 into it reports the event against
		// point 44, and the master has no way to know. One octet is kept where
		// it fits, which is the common case and the smaller encoding; a run
		// reaching past 255 moves to two octets, and so to a two-octet count.
		prefix, spec, maxCount := app.PrefixIndex1, app.RangeCount8, 0xFF
		for k := i; k < j; k++ {
			if events[k].Index > 0xFF {
				prefix, spec, maxCount = app.PrefixIndex2, app.RangeCount16, 0xFFFF
				break
			}
		}
		prefixLen := prefix.Octets()
		perObject := prefixLen + size
		headerOverhead := app.ObjectHeaderSize + spec.Octets()

		for i < j {
			avail := b.room() - headerOverhead
			if avail < perObject {
				b.flush()
				avail = b.room() - headerOverhead
				if avail < perObject {
					return
				}
			}

			runLen := min(avail/perObject, j-i, maxCount)
			data := make([]byte, 0, runLen*perObject)
			for k := range runLen {
				e := events[i+k]
				if prefixLen == 1 {
					data = append(data, byte(e.Index))
				} else {
					data = binary.LittleEndian.AppendUint16(data, e.Index)
				}
				data = s.encodeEvent(data, gv, e, b.ctx)
			}

			b.add(app.ObjectHeader{
				Group:     gv.Group,
				Variation: gv.Variation,
				Qualifier: app.MakeQualifier(prefix, spec),
				Range:     app.Range{Spec: spec, Count: uint32(runLen)},
				Data:      data,
			})
			i += runLen
		}
	}
}

// encodeEvent appends one event's object encoding.
func (s *Session) encodeEvent(dst []byte, gv objects.GroupVar, e Event, ctx objects.Context) []byte {
	switch e.Type {
	case dnp3.TypeBinary:
		if c, ok := objects.BinaryCodec(gv); ok {
			return c.Write(dst, e.Binary, ctx)
		}
	case dnp3.TypeDoubleBitBinary:
		if c, ok := objects.DoubleBitCodec(gv); ok {
			return c.Write(dst, e.DoubleBit, ctx)
		}
	case dnp3.TypeCounter:
		if c, ok := objects.CounterCodec(gv); ok {
			return c.Write(dst, e.Counter, ctx)
		}
	case dnp3.TypeFrozenCounter:
		if c, ok := objects.FrozenCounterCodec(gv); ok {
			return c.Write(dst, e.FrozenCounter, ctx)
		}
	case dnp3.TypeAnalog:
		if c, ok := objects.AnalogCodec(gv); ok {
			return c.Write(dst, e.Analog, ctx)
		}
	case dnp3.TypeBinaryOutputStatus:
		if c, ok := objects.BinaryOutputCodec(gv); ok {
			return c.Write(dst, e.BinaryOutput, ctx)
		}
	case dnp3.TypeAnalogOutputStatus:
		if c, ok := objects.AnalogOutputCodec(gv); ok {
			return c.Write(dst, e.AnalogOutput, ctx)
		}
	case dnp3.TypeOctetString:
		return appendOctetString(dst, e.OctetString, int(gv.Variation))
	}
	return dst
}
