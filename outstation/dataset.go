package outstation

import (
	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/objects"
)

// DatasetObject configures one prototype, descriptor, or present value. Data
// is the object encoding (without its size prefix). Prototype expansion and
// application-defined element types are the application's responsibility.
type DatasetObject struct {
	Group, Variation uint8
	Index            uint16
	Data             []byte
}

type datasetKey struct {
	group, variation uint8
	index            uint16
}

func datasetVariationKnown(group, variation uint8) bool {
	switch group {
	case 85, 87, 88:
		return variation == 1
	case 86:
		return variation >= 1 && variation <= 3
	}
	return false
}

// UpdateDataset stores a dataset object. For present values, an event snapshot
// is queued when class is an event class. Data is copied so later application
// mutations cannot alter a stored value or an unconfirmed event.
func (db *Database) UpdateDataset(v DatasetObject, class dnp3.Class) error {
	if !datasetVariationKnown(v.Group, v.Variation) || v.Group == 88 || len(v.Data) == 0 || len(v.Data) > app.MaxFreeFormatObject {
		return dnp3.ErrBadConfig
	}
	if v.Group == 86 && v.Variation == 2 && len(v.Data) != 1 {
		return dnp3.ErrBadConfig
	}
	if v.Group == 85 || (v.Group == 86 && v.Variation == 1) {
		if _, err := objects.ParseDatasetDescriptor(v.Data); err != nil {
			return err
		}
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.datasets == nil {
		db.datasets = map[datasetKey][]byte{}
	}
	value := append([]byte(nil), v.Data...)
	db.datasets[datasetKey{v.Group, v.Variation, v.Index}] = value
	if assigned, ok := db.datasetClasses[v.Index]; ok {
		class = assigned
	}
	if v.Group == 87 && class&dnp3.Class123 != 0 && db.events != nil {
		db.events.Add(Event{Type: dnp3.TypeDataset, Index: v.Index, Variation: 1, Class: class & dnp3.Class123, Dataset: value})
	}
	return nil
}

func (s *Session) readDatasets(b *responseBuilder, h app.ObjectHeader) {
	variation := h.Variation
	if variation == 0 {
		variation = 1
	}
	if !datasetVariationKnown(h.Group, variation) {
		s.iin = s.iin.Set(app.IINObjectUnknown)
		return
	}
	if !forEachPointRun(h, func(start, stop uint16) {
		s.db.mu.RLock()
		defer s.db.mu.RUnlock()
		for i := int(start); i <= int(stop); i++ {
			value, ok := s.db.datasets[datasetKey{h.Group, variation, uint16(i)}]
			if !ok {
				continue
			}
			var header app.ObjectHeader
			if h.Group == 86 && variation == 2 {
				header = rangeObjectHeader(objects.GV(86, 2), uint16(i), uint16(i), value)
			} else {
				header, _ = app.FreeFormat(h.Group, variation, value)
			}
			if header.Size()+app.ResponseHeaderSize > s.cfg.MaxTxFragment {
				s.iin = s.iin.Set(app.IINParameterError)
				continue
			}
			b.add(header)
		}
	}) {
		s.iin = s.iin.Set(app.IINParameterError)
	}
}

func (s *Session) writeDatasets(h app.ObjectHeader) {
	if !datasetVariationKnown(h.Group, h.Variation) || (h.Group == 86 && h.Variation == 2) {
		s.iin = s.iin.Set(app.IINObjectUnknown)
		return
	}
	if s.cfg.DatasetWrite == nil {
		s.iin = s.iin.Set(app.IINNoFuncCodeSupport)
		return
	}
	if h.Qualifier != app.FreeFormatQualifier {
		s.iin = s.iin.Set(app.IINParameterError)
		return
	}
	values, err := app.FreeFormatObjects(h)
	if err != nil || uint32(len(values)) != h.Count() {
		s.iin = s.iin.Set(app.IINParameterError)
		return
	}
	for _, v := range values {
		if h.Group == 85 || (h.Group == 86 && h.Variation == 1) {
			if _, err := objects.ParseDatasetDescriptor(v); err != nil {
				s.iin = s.iin.Set(app.IINParameterError)
				continue
			}
		}
		// The backend resolves prototype-defined fields, validates the complete
		// value, and applies the device-specific write. No partial decoding.
		if !s.cfg.DatasetWrite(h.Group, h.Variation, append([]byte(nil), v...)) {
			s.iin = s.iin.Set(app.IINParameterError)
		}
	}
}
