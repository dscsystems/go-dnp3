package outstation

import (
	"encoding/binary"
	"io"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/internal/stack"
	"github.com/dscsystems/go-dnp3/objects"
)

// AttributeID identifies an attribute allowed to accept writes.
type AttributeID struct{ Set, Variation uint8 }

func (s *Session) attributeWritable(set, variation uint8) bool {
	if variation == 0 || variation >= dnp3.AttrAll {
		return false
	}
	for _, id := range s.cfg.WritableAttributes {
		if id.Set == set && id.Variation == variation {
			return true
		}
	}
	return false
}

func (s *Session) writeAttribute(h app.ObjectHeader) {
	// An attribute range names its set. Require one set and one value.
	if !h.Range.Spec.IsStartStop() || h.Range.Start != h.Range.Stop || h.Range.Start > 255 || h.Count() != 1 || h.Qualifier.IndexPrefix() != app.PrefixNone {
		s.iin = s.iin.Set(app.IINParameterError)
		return
	}
	set := uint8(h.Range.Start)
	key := attributeKey{set, h.Variation}
	old, exists := s.attributes[key]
	if !exists {
		s.iin = s.iin.Set(app.IINObjectUnknown)
		return
	}
	v, n, err := objects.ParseAttribute(set, h.Variation, h.Data)
	if err != nil || n != len(h.Data) || old.Type != v.Type || !s.attributeWritable(set, h.Variation) {
		s.iin = s.iin.Set(app.IINParameterError)
		return
	}
	if s.cfg.AttributeWrite != nil {
		callbackValue := v
		callbackValue.Octets = append([]byte(nil), v.Octets...)
		if !s.cfg.AttributeWrite(callbackValue) {
			s.iin = s.iin.Set(app.IINParameterError)
			return
		}
	}
	s.attributes[key] = v
}

func (s *Session) readCurrentTime(b *responseBuilder, h app.ObjectHeader) {
	if h.Range.Spec != app.RangeAllObjects && !(h.Range.Spec.IsCount() && h.Count() == 1 && h.Qualifier.IndexPrefix() == app.PrefixNone) {
		s.iin = s.iin.Set(app.IINParameterError)
		return
	}
	b.add(app.ObjectHeader{Group: 50, Variation: 1,
		Qualifier: app.MakeQualifier(app.PrefixNone, app.RangeCount8),
		Range:     app.Range{Spec: app.RangeCount8, Count: 1}, Data: objects.AppendTime48(nil, dnp3.Now(s.appl.Now()))})
}

func (s *Session) readIIN(b *responseBuilder, h app.ObjectHeader) {
	start, stop := uint32(0), uint32(15)
	if h.Range.Spec.IsStartStop() {
		start, stop = h.Range.Start, h.Range.Stop
	} else if h.Range.Spec != app.RangeAllObjects {
		s.iin = s.iin.Set(app.IINParameterError)
		return
	}
	if start > stop || stop > 15 {
		s.iin = s.iin.Set(app.IINParameterError)
		return
	}
	bits := uint16(s.currentIIN()) >> start
	data := make([]byte, (stop-start+8)/8)
	for i := range data {
		data[i] = byte(bits >> (8 * i))
	}
	if n := (stop - start + 1) % 8; n != 0 {
		data[len(data)-1] &= byte((1 << n) - 1)
	}
	b.add(rangeObjectHeader(objects.GV(80, 1), uint16(start), uint16(stop), data))
}

// eachIndexedValue walks a data-bearing header, respecting index prefixes.
func eachIndexedValue(h app.ObjectHeader, size int, fn func(uint32, []byte)) bool {
	prefix := h.Qualifier.IndexPrefix().Octets()
	if size <= 0 || (!h.Range.Spec.IsStartStop() && !(h.Range.Spec.IsCount() && h.Qualifier.IndexPrefix().IsIndex())) {
		return false
	}
	if uint64(h.Count())*uint64(size+prefix) != uint64(len(h.Data)) {
		return false
	}
	for i := uint32(0); i < h.Count(); i++ {
		off := int(i) * (size + prefix)
		index := h.Range.Start + i
		if prefix > 0 {
			index = readPrefix(h.Data[off:], prefix)
		}
		fn(index, h.Data[off+prefix:off+prefix+size])
	}
	return true
}

func (s *Session) readTimeIntervals(b *responseBuilder, h app.ObjectHeader) {
	if !forEachPointRun(h, func(start, stop uint16) {
		for i := int(start); i <= int(stop) && i < s.db.Counts().TimeAndInterval; i++ {
			v, _ := s.db.TimeAndInterval(uint16(i))
			data := objects.AppendTime48(nil, v.Time)
			data = binary.LittleEndian.AppendUint32(data, v.Interval)
			data = append(data, v.Units)
			b.add(rangeObjectHeader(objects.GV(50, 4), uint16(i), uint16(i), data))
		}
	}) {
		s.iin = s.iin.Set(app.IINParameterError)
	}
}

func (s *Session) writeTimeIntervals(h app.ObjectHeader) {
	if !eachIndexedValue(h, 11, func(index uint32, data []byte) {
		if index > 65535 || data[10] > 9 || !s.db.UpdateTimeAndInterval(uint16(index), dnp3.TimeAndInterval{
			Time: objects.ParseTime48(data), Interval: binary.LittleEndian.Uint32(data[6:10]), Units: data[10]}) {
			s.iin = s.iin.Set(app.IINParameterError)
		}
	}) {
		s.iin = s.iin.Set(app.IINParameterError)
	}
}

func (s *Session) readTerminals(b *responseBuilder, h app.ObjectHeader) {
	if !forEachPointRun(h, func(start, stop uint16) {
		s.db.mu.RLock()
		defer s.db.mu.RUnlock()
		for i := int(start); i <= int(stop) && i < len(s.db.terminal); i++ {
			v := s.db.terminal[i].value
			if len(v) > 0 {
				b.add(rangeObjectHeader(objects.GV(112, uint8(len(v))), uint16(i), uint16(i), v))
			}
		}
	}) {
		s.iin = s.iin.Set(app.IINParameterError)
	}
}

func (s *Session) writeTerminals(h app.ObjectHeader) {
	if s.cfg.TerminalWrite == nil {
		s.iin = s.iin.Set(app.IINNoFuncCodeSupport)
		return
	}
	if !eachIndexedValue(h, int(h.Variation), func(index uint32, data []byte) {
		if index >= uint32(s.db.Counts().VirtualTerminal) || !s.cfg.TerminalWrite(uint16(index), append([]byte(nil), data...)) {
			s.iin = s.iin.Set(app.IINParameterError)
		}
	}) {
		s.iin = s.iin.Set(app.IINParameterError)
	}
}

// ManagementOperation is an application/configuration function code.
type ManagementOperation uint8

const (
	InitializeData        ManagementOperation = 15
	InitializeApplication ManagementOperation = 16
	StartApplication      ManagementOperation = 17
	StopApplication       ManagementOperation = 18
	SaveConfiguration     ManagementOperation = 19
	ActivateConfiguration ManagementOperation = 31
)

// ManagementHandler performs a device-specific operation. The payload contains
// the complete object section, copied for the handler. A false result refuses
// the operation with PARAMETER_ERROR. Nil Config.Management reports unsupported.
type ManagementHandler interface {
	Manage(ManagementOperation, []byte) bool
}

func (s *Session) onManagement(w io.Writer, r stack.Received, frag app.Fragment) error {
	if frag.Header.Func == app.FuncActivateConfig && s.cfg.ActivateConfig != nil {
		var names []string
		for _, h := range frag.Objects {
			if h.Group != 70 || h.Variation != 8 || h.Qualifier != app.FreeFormatQualifier {
				s.iin = s.iin.Set(app.IINParameterError)
				return s.respond(w, r, frag.Header, nil)
			}
			data, err := app.FreeFormatObjects(h)
			if err != nil {
				s.iin = s.iin.Set(app.IINParameterError)
				return s.respond(w, r, frag.Header, nil)
			}
			for _, v := range data {
				if len(v) == 0 {
					s.iin = s.iin.Set(app.IINParameterError)
					return s.respond(w, r, frag.Header, nil)
				}
				names = append(names, string(v))
			}
		}
		if len(names) == 0 {
			s.iin = s.iin.Set(app.IINParameterError)
			return s.respond(w, r, frag.Header, nil)
		}
		value, err := objects.AppendActivationResult(nil, s.cfg.ActivateConfig(names))
		if err != nil {
			s.iin = s.iin.Set(app.IINParameterError)
			return s.respond(w, r, frag.Header, nil)
		}
		h := app.ObjectHeader{Group: 91, Variation: 1, Qualifier: app.MakeQualifier(app.PrefixNone, app.RangeCount8), Range: app.Range{Spec: app.RangeCount8, Count: 1}, Data: value}
		if h.Size()+app.ResponseHeaderSize > s.cfg.MaxTxFragment {
			s.iin = s.iin.Set(app.IINParameterError)
			return s.respond(w, r, frag.Header, nil)
		}
		return s.respond(w, r, frag.Header, app.AppendObjectHeader(nil, h))
	}

	if s.cfg.Management == nil {
		s.iin = s.iin.Set(app.IINNoFuncCodeSupport)
	} else if !s.cfg.Management.Manage(ManagementOperation(frag.Header.Func), append([]byte(nil), frag.Raw[frag.Header.Size():]...)) {
		s.iin = s.iin.Set(app.IINParameterError)
	}
	if r.Broadcast {
		return nil
	}
	return s.respond(w, r, frag.Header, nil)
}
