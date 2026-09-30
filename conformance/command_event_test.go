package conformance

import (
	"testing"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/objects"
	"github.com/dscsystems/go-dnp3/outstation"
)

func commandEventHarness(t *testing.T, class dnp3.Class) *harness {
	t.Helper()
	h := newHarness(t, outstation.Config{Database: smallDB()}, &recordingHandler{})
	h.out.Update(func(db *outstation.Database) {
		for i := range 2 {
			db.Configure(dnp3.TypeBinaryOutputStatus, uint16(i), outstation.PointConfig{
				Class: dnp3.ClassNone, CommandEventClass: class})
			db.Configure(dnp3.TypeAnalogOutputStatus, uint16(i), outstation.PointConfig{
				Class: dnp3.ClassNone, CommandEventClass: class})
		}
	})
	return h
}

// commandEventsIn returns the group's events from a response, by index.
func commandEventsIn(t *testing.T, frag app.Fragment, group uint8) map[uint32]dnp3.CommandEvent {
	t.Helper()
	out := map[uint32]dnp3.CommandEvent{}
	for _, o := range frag.Objects {
		if o.Group != group {
			continue
		}
		size, ok := objects.CommandEventSize(o.Group, o.Variation)
		if !ok {
			t.Fatalf("g%dv%d is not a command event", o.Group, o.Variation)
		}
		prefix := o.Qualifier.IndexPrefix().Octets()
		for k := 0; k < int(o.Range.Count); k++ {
			off := k * (prefix + size)
			idx := uint32(o.Data[off])
			if prefix == 2 {
				idx |= uint32(o.Data[off+1]) << 8
			}
			e, ok := objects.ParseCommandEvent(o.Group, o.Variation, o.Data[off+prefix:])
			if !ok {
				t.Fatalf("undecodable g%dv%d event", o.Group, o.Variation)
			}
			out[idx] = e
		}
	}
	return out
}

// An operated control is recorded as a command event in the class the point
// names, and a master polling that class collects it.
func TestOperatedControlsRaiseCommandEvents(t *testing.T) {
	h := commandEventHarness(t, dnp3.Class2)

	if got := commandStatus(t, h.request(app.FuncDirectOperate, crobHeader(1, dnp3.ControlLatchOn))); got != dnp3.CommandSuccess {
		t.Fatalf("direct operate status = %v", got)
	}
	h.request(app.FuncDirectOperate, app.ObjectHeader{
		Group: 41, Variation: 3,
		Qualifier: app.MakeQualifier(app.PrefixIndex1, app.RangeCount8),
		Range:     app.Range{Spec: app.RangeCount8, Count: 1},
		Data:      objects.AppendAnalogOutputFloat32([]byte{0}, dnp3.AnalogOutputFloat32{Value: 12.5}),
	})

	resp := h.request(app.FuncRead, app.ReadAllObjects(60, 3)) // class 2 events

	bin := commandEventsIn(t, resp, 13)
	if e, ok := bin[1]; !ok || !e.State || e.Status != dnp3.CommandSuccess {
		t.Errorf("binary command events = %+v, want index 1 latched on with success", bin)
	}
	an := commandEventsIn(t, resp, 43)
	if e, ok := an[0]; !ok || !e.Analog || e.Value != 12.5 || e.Status != dnp3.CommandSuccess {
		t.Errorf("analog command events = %+v, want index 0 = 12.5 with success", an)
	}
	if e := bin[1]; e.Time.Time.IsZero() {
		t.Error("the command event carries no time")
	}
}

// A SELECT operates nothing, and a point with no command event class records
// nothing.
func TestSelectAndUnconfiguredPointsRaiseNoCommandEvents(t *testing.T) {
	h := commandEventHarness(t, dnp3.Class2)
	h.request(app.FuncSelect, crobHeader(0, dnp3.ControlLatchOn))
	if got := h.out.Events().Total(); got != 0 {
		t.Errorf("a select raised %d events", got)
	}

	off := commandEventHarness(t, dnp3.ClassNone)
	off.request(app.FuncDirectOperate, crobHeader(0, dnp3.ControlLatchOn))
	if got := off.out.Events().Total(); got != 0 {
		t.Errorf("a point without a command event class raised %d events", got)
	}
}

// A pattern control block is not a CROB. Neither g12v2 nor its g12v3 mask may
// reach the handler, whichever function carries them.
func TestPatternControlsAreRefusedWithoutReachingTheHandler(t *testing.T) {
	for _, variation := range []uint8{2, 3} {
		for _, fc := range []app.FuncCode{app.FuncDirectOperate, app.FuncSelect} {
			rec := &recordingHandler{}
			h := newHarness(t, outstation.Config{Database: smallDB()}, rec)

			data := append([]byte{1}, make([]byte, 11)...) // an index and a block
			data[1] = byte(dnp3.ControlLatchOn)
			data[2] = 1
			resp := h.request(fc, app.ObjectHeader{
				Group: 12, Variation: variation,
				Qualifier: app.MakeQualifier(app.PrefixIndex1, app.RangeCount8),
				Range:     app.Range{Spec: app.RangeCount8, Count: 1},
				Data:      data,
			})

			// The packed mask is refused while the fragment is parsed, the
			// block while the command is executed; both are refusals.
			if !resp.Header.IIN.Has(app.IINObjectUnknown) && !resp.Header.IIN.Has(app.IINParameterError) {
				t.Errorf("g12v%d %v: IIN = %v, want the request refused", variation, fc, resp.Header.IIN)
			}
			if rec.selects != 0 || rec.operates != 0 {
				t.Errorf("g12v%d %v reached the handler (%d selects, %d operates)",
					variation, fc, rec.selects, rec.operates)
			}
		}
	}
}
