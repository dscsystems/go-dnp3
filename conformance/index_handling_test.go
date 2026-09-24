package conformance

import (
	"encoding/binary"
	"sync"
	"testing"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/objects"
	"github.com/dscsystems/go-dnp3/outstation"
)

// indexHandler records which point every operate was addressed to.
type indexHandler struct {
	mu  sync.Mutex
	ops []uint16
}

func (h *indexHandler) operated() []uint16 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]uint16(nil), h.ops...)
}

func (h *indexHandler) SelectCROB(uint16, dnp3.ControlRelayOutputBlock) dnp3.CommandStatus {
	return dnp3.CommandSuccess
}

func (h *indexHandler) OperateCROB(i uint16, _ dnp3.ControlRelayOutputBlock, _ outstation.OperateType) dnp3.CommandStatus {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ops = append(h.ops, i)
	return dnp3.CommandSuccess
}

func (h *indexHandler) SelectAnalog(uint16, outstation.AnalogOutput) dnp3.CommandStatus {
	return dnp3.CommandSuccess
}

func (h *indexHandler) OperateAnalog(uint16, outstation.AnalogOutput, outstation.OperateType) dnp3.CommandStatus {
	return dnp3.CommandSuccess
}

// indexList builds a request header naming individual points by a two-octet
// index prefix, qualifier 0x28.
func indexList(group, variation uint8, indexes ...uint16) app.ObjectHeader {
	var data []byte
	for _, i := range indexes {
		data = binary.LittleEndian.AppendUint16(data, i)
	}
	return app.ObjectHeader{
		Group: group, Variation: variation,
		Qualifier: app.MakeQualifier(app.PrefixIndex2, app.RangeCount16),
		Range:     app.Range{Spec: app.RangeCount16, Count: uint32(len(indexes))},
		Data:      data,
	}
}

// eventIndexes decodes the point index of every event in a response.
func eventIndexes(t *testing.T, resp app.Fragment) []uint32 {
	t.Helper()
	var out []uint32
	for _, o := range resp.Objects {
		d, ok := objects.Lookup(objects.GV(o.Group, o.Variation))
		if !ok {
			continue
		}
		size, _ := d.SizeOctets()
		width := o.Qualifier.IndexPrefix().Octets()
		for off := 0; off+width+size <= len(o.Data); off += width + size {
			switch width {
			case 1:
				out = append(out, uint32(o.Data[off]))
			case 2:
				out = append(out, uint32(binary.LittleEndian.Uint16(o.Data[off:])))
			}
		}
	}
	return out
}

// Events carry their point index as a prefix, and one octet holds only
// 0-255. Encoding a higher index in it reports the event against a different
// point — analog 300 as analog 44 — and the master has no way to know.
func TestEventsAboveIndex255KeepTheirIndex(t *testing.T) {
	h := newHarness(t, outstation.Config{
		Database: outstation.DatabaseConfig{Analog: 400, DefaultClass: dnp3.Class1},
	}, nil)
	h.out.Update(func(db *outstation.Database) {
		db.UpdateAnalog(300, dnp3.Analog{Value: 42, Flags: dnp3.Online})
	})

	resp := h.request(app.FuncRead, app.ReadAllObjects(60, 2))

	got := eventIndexes(t, resp)
	if len(got) != 1 || got[0] != 300 {
		t.Errorf("event indexes = %v, want [300]", got)
	}
}

// The narrow prefix is still used when every index fits it.
func TestEventsAtLowIndexesUseTheOneOctetPrefix(t *testing.T) {
	h := newHarness(t, outstation.Config{
		Database: outstation.DatabaseConfig{Analog: 10, DefaultClass: dnp3.Class1},
	}, nil)
	h.out.Update(func(db *outstation.Database) {
		db.UpdateAnalog(7, dnp3.Analog{Value: 42, Flags: dnp3.Online})
	})

	resp := h.request(app.FuncRead, app.ReadAllObjects(60, 2))

	for _, o := range resp.Objects {
		if o.Qualifier.IndexPrefix() != app.PrefixIndex1 {
			t.Errorf("qualifier %v for an event at index 7; the one-octet prefix fits", o.Qualifier)
		}
	}
	if got := eventIndexes(t, resp); len(got) != 1 || got[0] != 7 {
		t.Errorf("event indexes = %v, want [7]", got)
	}
}

// A four-octet index prefix can name a point no database has. Narrowing it to
// 16 bits wraps it onto one that does exist, so a control addressed to point
// 65541 operated point 5.
func TestCommandForAnIndexAbove16BitsOperatesNothing(t *testing.T) {
	hd := &indexHandler{}
	h := newHarness(t, outstation.Config{
		Database: outstation.DatabaseConfig{BinaryOutputStatus: 10},
	}, hd)

	data := binary.LittleEndian.AppendUint32(nil, 65536+5)
	data = objects.AppendCROB(data, dnp3.ControlRelayOutputBlock{Code: dnp3.ControlLatchOn, Count: 1})
	resp := h.request(app.FuncDirectOperate, app.ObjectHeader{
		Group: 12, Variation: 1,
		Qualifier: app.MakeQualifier(app.PrefixIndex4, app.RangeCount32),
		Range:     app.Range{Spec: app.RangeCount32, Count: 1},
		Data:      data,
	})

	if ops := hd.operated(); len(ops) != 0 {
		t.Errorf("a command for index 65541 operated point(s) %v", ops)
	}
	if got := commandStatus(t, resp); got != dnp3.CommandNotSupported {
		t.Errorf("status = %v, want NOT_SUPPORTED", got)
	}
}

// The same wrap on the read side: a 32-bit range beyond the point space is
// answered with points from the bottom of it.
func TestReadAboveThe16BitIndexSpaceReturnsNothing(t *testing.T) {
	h := newHarness(t, outstation.Config{
		Database: outstation.DatabaseConfig{Analog: 10},
	}, nil)

	resp := h.request(app.FuncRead, app.ObjectHeader{
		Group: 30, Variation: 0,
		Qualifier: app.MakeQualifier(app.PrefixNone, app.RangeStartStop32),
		Range:     app.Range{Spec: app.RangeStartStop32, Start: 65536, Stop: 65537},
	})

	for _, o := range resp.Objects {
		if o.Group == 30 {
			t.Errorf("a read of 65536..65537 was answered with %s", o)
		}
	}
}

// A read may name individual points rather than a range. The index list is on
// the wire even though a read carries no object data, and has to be both
// parsed and honoured: before, the request failed to parse at all.
func TestReadOfIndividualPoints(t *testing.T) {
	h := newHarness(t, outstation.Config{
		Database: outstation.DatabaseConfig{Analog: 10},
	}, nil)
	h.out.Update(func(db *outstation.Database) {
		for i := range uint16(10) {
			db.UpdateAnalog(i, dnp3.Analog{Value: float64(i * 10), Flags: dnp3.Online})
		}
	})

	resp := h.request(app.FuncRead, indexList(30, 0, 3, 4, 7))

	if resp.Header.IIN.Has(app.IINParameterError) {
		t.Fatalf("the read was rejected: IIN = %v", resp.Header.IIN)
	}
	var got []uint32
	for _, o := range resp.Objects {
		for i := o.Range.Start; i <= o.Range.Stop; i++ {
			got = append(got, i)
		}
	}
	if want := []uint32{3, 4, 7}; len(got) != len(want) || got[0] != 3 || got[1] != 4 || got[2] != 7 {
		t.Errorf("points returned = %v, want %v", got, want)
	}
	if v, _ := decodeAnalog(t, resp, 7); v != 70 {
		t.Errorf("analog 7 = %v, want 70", v)
	}
}

// ASSIGN_CLASS names the points it applies to. Assigning the class to every
// point of the type instead silently reclassifies all the others.
func TestAssignClassChangesOnlyTheNamedPoints(t *testing.T) {
	tests := []struct {
		name   string
		points app.ObjectHeader
	}{
		{"range", app.ReadRange(30, 0, 0, 1)},
		{"index list", indexList(30, 0, 0, 1)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, outstation.Config{
				Database: outstation.DatabaseConfig{Analog: 10, DefaultClass: dnp3.Class2},
			}, nil)

			h.request(app.FuncAssignClass, app.ReadAllObjects(60, 2), tc.points)

			for _, c := range []struct {
				index uint16
				want  dnp3.Class
			}{{0, dnp3.Class1}, {1, dnp3.Class1}, {5, dnp3.Class2}, {9, dnp3.Class2}} {
				if _, cfg, _ := h.out.Database().Analog(c.index); cfg.Class != c.want {
					t.Errorf("analog %d class = %v, want %v", c.index, cfg.Class, c.want)
				}
			}
		})
	}
}

// A freeze naming counters freezes those counters, and leaves every other
// frozen value alone.
func TestFreezeAffectsOnlyTheNamedCounters(t *testing.T) {
	h := newHarness(t, outstation.Config{
		Database: outstation.DatabaseConfig{Counter: 4, FrozenCounter: 4},
	}, nil)
	h.out.Update(func(db *outstation.Database) {
		for i := range uint16(4) {
			db.UpdateCounter(i, dnp3.Counter{Value: 100 + uint32(i), Flags: dnp3.Online})
		}
	})

	h.request(app.FuncImmedFreeze, app.ReadRange(20, 0, 0, 1))

	for i, want := range []uint32{100, 101, 0, 0} {
		if v, _, _ := h.out.Database().FrozenCounter(uint16(i)); v.Value != want {
			t.Errorf("frozen counter %d = %d, want %d", i, v.Value, want)
		}
	}
}

// With no objects, a freeze still freezes every counter.
func TestFreezeWithNoObjectsFreezesEveryCounter(t *testing.T) {
	h := newHarness(t, outstation.Config{
		Database: outstation.DatabaseConfig{Counter: 3, FrozenCounter: 3},
	}, nil)
	h.out.Update(func(db *outstation.Database) {
		for i := range uint16(3) {
			db.UpdateCounter(i, dnp3.Counter{Value: 50, Flags: dnp3.Online})
		}
	})

	h.request(app.FuncImmedFreeze)

	for i := range uint16(3) {
		if v, _, _ := h.out.Database().FrozenCounter(i); v.Value != 50 {
			t.Errorf("frozen counter %d = %d, want 50", i, v.Value)
		}
	}
}

// End to end: a master that freezes counters and then polls for events is
// told what the freeze captured.
func TestFreezeIsReportedToAnEventPoll(t *testing.T) {
	h := newHarness(t, outstation.Config{
		Database: outstation.DatabaseConfig{Counter: 2, FrozenCounter: 2},
	}, nil)
	h.out.Update(func(db *outstation.Database) {
		db.Configure(dnp3.TypeFrozenCounter, 0, outstation.PointConfig{Class: dnp3.Class1})
		db.Configure(dnp3.TypeFrozenCounter, 1, outstation.PointConfig{Class: dnp3.Class1})
		db.UpdateCounter(0, dnp3.Counter{Value: 100, Flags: dnp3.Online})
		db.UpdateCounter(1, dnp3.Counter{Value: 200, Flags: dnp3.Online})
	})

	h.request(app.FuncImmedFreeze)
	resp := h.request(app.FuncRead, app.ReadAllObjects(60, 2))

	var frozenEvents uint32
	for _, o := range resp.Objects {
		if o.Group == 23 {
			frozenEvents += o.Range.Count
		}
	}
	if frozenEvents != 2 {
		t.Errorf("the event poll after a freeze carried %d frozen counter events, want 2: %v",
			frozenEvents, resp.Objects)
	}
}

// crobHandler records the control code of every CROB operated.
type crobHandler struct {
	mu    sync.Mutex
	codes []dnp3.ControlCode
}

func (h *crobHandler) seen() []dnp3.ControlCode {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]dnp3.ControlCode(nil), h.codes...)
}

func (h *crobHandler) SelectCROB(uint16, dnp3.ControlRelayOutputBlock) dnp3.CommandStatus {
	return dnp3.CommandSuccess
}

func (h *crobHandler) OperateCROB(_ uint16, c dnp3.ControlRelayOutputBlock, _ outstation.OperateType) dnp3.CommandStatus {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.codes = append(h.codes, c.Code)
	return dnp3.CommandSuccess
}

func (h *crobHandler) SelectAnalog(uint16, outstation.AnalogOutput) dnp3.CommandStatus {
	return dnp3.CommandSuccess
}

func (h *crobHandler) OperateAnalog(uint16, outstation.AnalogOutput, outstation.OperateType) dnp3.CommandStatus {
	return dnp3.CommandSuccess
}

// A conforming master sends a pulsed trip as 0x81 and a pulsed close as 0x41
// (trip-close code 2 and 1 in bits 7-6). The outstation has to hand the
// application the coil the master meant: with the codes transposed, a trip
// from any other vendor's master reached the application as a close.
func TestOutstationReadsTripAndCloseAsTheStandardDefines(t *testing.T) {
	for _, tc := range []struct {
		name    string
		raw     byte
		trip    bool
		closing bool
	}{
		{"pulse trip 0x81", 0x81, true, false},
		{"pulse close 0x41", 0x41, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hd := &crobHandler{}
			h := newHarness(t, outstation.Config{
				Database: outstation.DatabaseConfig{BinaryOutputStatus: 4},
			}, hd)

			h.request(app.FuncDirectOperate, app.ObjectHeader{
				Group: 12, Variation: 1,
				Qualifier: app.MakeQualifier(app.PrefixIndex1, app.RangeCount8),
				Range:     app.Range{Spec: app.RangeCount8, Count: 1},
				Data:      []byte{0, tc.raw, 1, 0xE8, 0x03, 0, 0, 0, 0, 0, 0, 0},
			})

			codes := hd.seen()
			if len(codes) != 1 {
				t.Fatalf("handler saw %d operates, want 1", len(codes))
			}
			if codes[0].IsTrip() != tc.trip || codes[0].IsClose() != tc.closing {
				t.Errorf("code %#02x reached the handler as trip=%v close=%v, want trip=%v close=%v",
					tc.raw, codes[0].IsTrip(), codes[0].IsClose(), tc.trip, tc.closing)
			}
		})
	}
}
