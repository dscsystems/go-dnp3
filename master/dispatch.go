package master

import (
	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/objects"
)

// dispatch decodes one object header into typed measurements and hands them to
// the handler.
//
// The headers a master must cope with come in two shapes: a contiguous index
// range with no per-object prefix, which is how static data arrives, and a
// count with a per-object index prefix, which is how events arrive because the
// points that changed are not adjacent.
func (s *Session) dispatch(h app.ObjectHeader, ctx objects.Context) {
	gv := objects.GV(h.Group, h.Variation)
	if len(h.Data) == 0 {
		return
	}

	// Octet strings are checked before the registry lookup, not after: their
	// length lives in the variation number, so there is no descriptor row for
	// g110v5 to find. Looking them up first would silently drop every string a
	// device reports.
	if h.Group == groupOctetString || h.Group == groupOctetStringEvent {
		kind := objects.KindString
		s.dispatchOctetStrings(h, HeaderInfo{GV: gv, Kind: kind}, h.Group == groupOctetStringEvent)
		return
	}

	d, ok := objects.Lookup(gv)
	if !ok || d.Measurement == dnp3.TypeUnknown {
		return
	}

	info := HeaderInfo{GV: gv, Kind: d.Kind}

	if d.Packed {
		s.dispatchPacked(h, d, info)
		return
	}

	size, ok := d.SizeOctets()
	if !ok || size == 0 {
		return
	}

	prefixLen := 0
	if p := h.Qualifier.IndexPrefix(); p.IsIndex() {
		prefixLen = p.Octets()
	}

	switch d.Measurement {
	case dnp3.TypeBinary:
		c, _ := objects.BinaryCodec(gv)
		vals := decodeRun(h, size, prefixLen, ctx, c.Parse)
		s.handler.HandleBinary(info, vals)
	case dnp3.TypeDoubleBitBinary:
		c, _ := objects.DoubleBitCodec(gv)
		vals := decodeRun(h, size, prefixLen, ctx, c.Parse)
		s.handler.HandleDoubleBit(info, vals)
	case dnp3.TypeCounter:
		c, _ := objects.CounterCodec(gv)
		vals := decodeRun(h, size, prefixLen, ctx, c.Parse)
		s.handler.HandleCounter(info, vals)
	case dnp3.TypeFrozenCounter:
		c, _ := objects.FrozenCounterCodec(gv)
		vals := decodeRun(h, size, prefixLen, ctx, c.Parse)
		s.handler.HandleFrozenCounter(info, vals)
	case dnp3.TypeAnalog:
		c, _ := objects.AnalogCodec(gv)
		vals := decodeRun(h, size, prefixLen, ctx, c.Parse)
		s.handler.HandleAnalog(info, vals)
	case dnp3.TypeBinaryOutputStatus:
		c, _ := objects.BinaryOutputCodec(gv)
		vals := decodeRun(h, size, prefixLen, ctx, c.Parse)
		s.handler.HandleBinaryOutputStatus(info, vals)
	case dnp3.TypeAnalogOutputStatus:
		c, _ := objects.AnalogOutputCodec(gv)
		vals := decodeRun(h, size, prefixLen, ctx, c.Parse)
		s.handler.HandleAnalogOutputStatus(info, vals)
	}
}

// decodeRun walks the objects a header introduces, taking each index either
// from the range or from a per-object prefix.
func decodeRun[T any](
	h app.ObjectHeader, size, prefixLen int, ctx objects.Context,
	parse func([]byte, objects.Context) T,
) []dnp3.Indexed[T] {
	if parse == nil {
		return nil
	}

	count := int(h.Count())
	out := make([]dnp3.Indexed[T], 0, count)

	off := 0
	for i := range count {
		if off+prefixLen+size > len(h.Data) {
			// The framing layer already validated the header's arithmetic, so
			// this should not happen; stopping rather than indexing past the
			// buffer keeps a malformed peer from crashing the session.
			break
		}

		index := uint16(h.Range.IndexOf(uint32(i)))
		if prefixLen > 0 {
			index = uint16(readPrefix(h.Data[off:], prefixLen))
			off += prefixLen
		}

		out = append(out, dnp3.Indexed[T]{
			Index: index,
			Value: parse(h.Data[off:off+size], ctx),
		})
		off += size
	}
	return out
}

// dispatchPacked handles the bit-packed variations, whose unit of encoding is
// the range rather than the object.
func (s *Session) dispatchPacked(h app.ObjectHeader, d objects.Descriptor, info HeaderInfo) {
	count := int(h.Count())
	start := uint32(h.Range.Start)

	switch d.Measurement {
	case dnp3.TypeBinary:
		raw := objects.ParsePackedBinary(h.Data, count, nil)
		out := make([]dnp3.Indexed[dnp3.Binary], len(raw))
		for i, v := range raw {
			out[i] = dnp3.Indexed[dnp3.Binary]{Index: uint16(start + uint32(i)), Value: v}
		}
		s.handler.HandleBinary(info, out)

	case dnp3.TypeDoubleBitBinary:
		raw := objects.ParsePackedDoubleBit(h.Data, count, nil)
		out := make([]dnp3.Indexed[dnp3.DoubleBitBinary], len(raw))
		for i, v := range raw {
			out[i] = dnp3.Indexed[dnp3.DoubleBitBinary]{Index: uint16(start + uint32(i)), Value: v}
		}
		s.handler.HandleDoubleBit(info, out)

	case dnp3.TypeBinaryOutputStatus:
		raw := objects.ParsePackedBinaryOutput(h.Data, count, nil)
		out := make([]dnp3.Indexed[dnp3.BinaryOutputStatus], len(raw))
		for i, v := range raw {
			out[i] = dnp3.Indexed[dnp3.BinaryOutputStatus]{Index: uint16(start + uint32(i)), Value: v}
		}
		s.handler.HandleBinaryOutputStatus(info, out)
	}
}

// dispatchOctetStrings decodes group 110 and 111 objects.
//
// The variation is the string's length, which is why a range of strings of
// differing lengths arrives as several headers rather than one.
func (s *Session) dispatchOctetStrings(h app.ObjectHeader, info HeaderInfo, isEvent bool) {
	if isEvent {
		info.Kind = objects.KindEvent
	}
	size := int(h.Variation)
	if size == 0 {
		return // variation zero means "any length" and appears only in requests
	}

	prefixLen := 0
	if p := h.Qualifier.IndexPrefix(); p.IsIndex() {
		prefixLen = p.Octets()
	}

	count := int(h.Count())
	out := make([]dnp3.Indexed[dnp3.OctetString], 0, count)

	off := 0
	for i := range count {
		if off+prefixLen+size > len(h.Data) {
			break
		}
		index := uint16(h.Range.IndexOf(uint32(i)))
		if prefixLen > 0 {
			index = uint16(readPrefix(h.Data[off:], prefixLen))
			off += prefixLen
		}
		// Copied: the header aliases the session's receive buffer, and a
		// handler that keeps the string would otherwise see it change.
		v := make(dnp3.OctetString, size)
		copy(v, h.Data[off:off+size])
		off += size

		out = append(out, dnp3.Indexed[dnp3.OctetString]{Index: index, Value: v})
	}
	s.handler.HandleOctetString(info, out)
}

// The octet string groups: 110 carries static strings, 111 their events.
const (
	groupOctetString      = 110
	groupOctetStringEvent = 111
)

func readPrefix(buf []byte, width int) uint32 {
	switch width {
	case 1:
		return uint32(buf[0])
	case 2:
		return uint32(buf[0]) | uint32(buf[1])<<8
	case 4:
		return uint32(buf[0]) | uint32(buf[1])<<8 | uint32(buf[2])<<16 | uint32(buf[3])<<24
	}
	return 0
}
