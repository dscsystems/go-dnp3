package conformance

import (
	"encoding/binary"
	"testing"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/internal/link"
	"github.com/dscsystems/go-dnp3/objects"
	"github.com/dscsystems/go-dnp3/outstation"
)

func TestFrozenAnalogLifecycleAndRouting(t *testing.T) {
	h := newHarness(t, outstation.Config{Database: outstation.DatabaseConfig{Analog: 2, FrozenAnalog: 2}}, nil)
	h.out.Update(func(db *outstation.Database) {
		db.Configure(dnp3.TypeFrozenAnalog, 1, outstation.PointConfig{Class: dnp3.Class2, StaticVariation: 8, EventVariation: 8})
		db.UpdateAnalog(1, dnp3.Analog{Value: 12.5, Flags: dnp3.Online})
	})
	waitFor(t, func() bool { v, _, _ := h.out.Database().Analog(1); return v.Value == 12.5 })
	r := h.request(app.FuncFreezeClear, app.ReadRange(30, 0, 1, 1))
	if r.Header.IIN.HasAny(app.RequestErrorMask) {
		t.Fatal(r.Header.IIN)
	}
	v, _, _ := h.out.Database().FrozenAnalog(1)
	if v.Value != 12.5 || v.Time.Time.IsZero() {
		t.Fatal(v)
	}
	a, _, _ := h.out.Database().Analog(1)
	if a.Value != 0 {
		t.Fatal(a)
	}
	r = h.request(app.FuncRead, app.ReadRange(31, 0, 1, 1))
	if len(r.Objects) != 1 || r.Objects[0].Variation != 8 {
		t.Fatal(r)
	}
	c, _ := objects.AnalogCodec(objects.GV(31, 8))
	if got := c.Parse(r.Objects[0].Data, objects.Context{}); got.Value != 12.5 {
		t.Fatal(got)
	}
	r = h.request(app.FuncRead, app.ReadAllObjects(33, 0))
	if len(r.Objects) != 1 || r.Objects[0].Group != 33 || !r.Header.Control.Con {
		t.Fatal(r)
	}
	h.sendConfirm(r.Header.Control.Seq)
	waitFor(t, func() bool { return h.out.Events().Total() == 0 })
}

func TestMixedCounterAnalogFreezeSchedule(t *testing.T) {
	h := newHarness(t, outstation.Config{Database: outstation.DatabaseConfig{Counter: 1, FrozenCounter: 1, Analog: 1, FrozenAnalog: 1}}, nil)
	h.out.Update(func(db *outstation.Database) {
		db.UpdateCounter(0, dnp3.Counter{Value: 19, Flags: dnp3.Online})
		db.UpdateAnalog(0, dnp3.Analog{Value: 7.5, Flags: dnp3.Online})
	})
	waitFor(t, func() bool { v, _, _ := h.out.Database().Analog(0); return v.Value == 7.5 })
	first := time.Now().Add(100 * time.Millisecond)
	data := objects.AppendTime48(nil, dnp3.Now(first))
	data = binary.LittleEndian.AppendUint32(data, 0)
	hdr := app.ObjectHeader{Group: 50, Variation: 2, Qualifier: app.MakeQualifier(app.PrefixNone, app.RangeCount8), Range: app.Range{Spec: app.RangeCount8, Count: 1}, Data: data}
	r := h.request(app.FuncFreezeAtTime, hdr, app.ReadAllObjects(20, 0), app.ReadAllObjects(30, 0))
	if r.Header.IIN.HasAny(app.RequestErrorMask) {
		t.Fatal(r)
	}
	waitFor(t, func() bool { v, _, _ := h.out.Database().FrozenAnalog(0); return v.Value == 7.5 })
	analog, _, _ := h.out.Database().FrozenAnalog(0)
	counter, _, _ := h.out.Database().FrozenCounter(0)
	if counter.Value != 19 || !counter.Time.Time.Equal(analog.Time.Time) {
		t.Fatal(counter, analog)
	}
}

func TestTimeIINAndIndexedIntervals(t *testing.T) {
	h := newHarness(t, outstation.Config{Database: outstation.DatabaseConfig{TimeAndInterval: 2}}, nil)
	r := h.request(app.FuncRead, app.ReadAllObjects(50, 1), app.ReadRange(80, 1, 7, 7))
	if len(r.Objects) != 2 || objects.ParseTime48(r.Objects[0].Data).Time.IsZero() || r.Objects[1].Data[0] != 1 {
		t.Fatal(r)
	}
	at := time.Unix(123456, 0)
	data := []byte{1}
	data = objects.AppendTime48(data, dnp3.Now(at))
	data = binary.LittleEndian.AppendUint32(data, 17)
	data = append(data, 2)
	hdr := app.ObjectHeader{Group: 50, Variation: 4, Qualifier: app.MakeQualifier(app.PrefixIndex1, app.RangeCount8), Range: app.Range{Spec: app.RangeCount8, Count: 1}, Data: data}
	r = h.request(app.FuncWrite, hdr)
	if r.Header.IIN.HasAny(app.RequestErrorMask) {
		t.Fatal(r)
	}
	v, ok := h.out.Database().TimeAndInterval(1)
	if !ok || v.Interval != 17 || !v.Time.Time.Equal(at) {
		t.Fatal(v)
	}
	r = h.request(app.FuncRead, app.ReadRange(50, 4, 1, 1))
	if len(r.Objects) != 1 || r.Objects[0].Data[10] != 2 {
		t.Fatal(r)
	}
	hdr.Data[0] = 3
	r = h.request(app.FuncWrite, hdr)
	if !r.Header.IIN.Has(app.IINParameterError) {
		t.Fatal(r)
	}
}

func TestWritableAttributePropertiesAndTypeValidation(t *testing.T) {
	a := objects.StringAttribute(247, "before")
	h := newHarness(t, outstation.Config{Attributes: []dnp3.Attribute{a}, WritableAttributes: []outstation.AttributeID{{Set: 0, Variation: 247}}}, nil)
	r := h.request(app.FuncRead, app.ReadRange(0, 255, 0, 0))
	attrs := attributesIn(t, r)
	found := false
	for _, it := range attrs[255].List() {
		if it.Variation == 247 && it.Writable {
			found = true
		}
	}
	if !found {
		t.Fatal(attrs)
	}
	data, _ := objects.AppendAttribute(nil, objects.StringAttribute(247, "after"))
	hdr := app.ReadRange(0, 247, 0, 0)
	hdr.Data = data
	r = h.request(app.FuncWrite, hdr)
	if r.Header.IIN.HasAny(app.RequestErrorMask) {
		t.Fatal(r)
	}
	r = h.request(app.FuncRead, app.ReadRange(0, 247, 0, 0))
	if attributesIn(t, r)[247].Text != "after" {
		t.Fatal(r)
	}
	hdr.Data, _ = objects.AppendAttribute(nil, objects.UintAttribute(247, 5))
	r = h.request(app.FuncWrite, hdr)
	if !r.Header.IIN.Has(app.IINParameterError) {
		t.Fatal(r)
	}
}

func TestSelfAddressDiscovery(t *testing.T) {
	h := newHarness(t, outstation.Config{SelfAddress: true, Database: smallDB()}, nil)
	before := h.count()
	h.sendTo(link.SelfAddress, app.FuncRead, app.ReadAllObjects(60, 1))
	r := h.await(before)
	if r.Header.IIN.HasAny(app.RequestErrorMask) || len(r.Objects) == 0 {
		t.Fatal(r)
	}
}

func TestVirtualTerminalWriteAndEvent(t *testing.T) {
	var got []byte
	h := newHarness(t, outstation.Config{Database: outstation.DatabaseConfig{VirtualTerminal: 1, DefaultClass: dnp3.Class1}, TerminalWrite: func(_ uint16, data []byte) bool { got = data; return true }}, nil)
	hdr := app.ReadRange(112, 3, 0, 0)
	hdr.Data = []byte("cmd")
	r := h.request(app.FuncWrite, hdr)
	if r.Header.IIN.HasAny(app.RequestErrorMask) || string(got) != "cmd" {
		t.Fatal(r)
	}
	h.out.Update(func(db *outstation.Database) { db.UpdateVirtualTerminal(0, []byte("reply")) })
	waitFor(t, func() bool { return h.out.Events().Total() == 1 })
	h.out.Update(func(db *outstation.Database) { db.UpdateVirtualTerminal(0, []byte("reply")) })
	waitFor(t, func() bool { return h.out.Events().Total() == 2 })
	r = h.request(app.FuncRead, app.ReadAllObjects(113, 0))
	if len(r.Objects) != 1 || r.Objects[0].Count() != 2 || string(r.Objects[0].Data[1:6]) != "reply" || !r.Header.Control.Con {
		t.Fatal(r)
	}
}

// A static read of a virtual terminal returns what it last held, in the
// variation that is its length.
func TestVirtualTerminalStaticReadReturnsTheLastInput(t *testing.T) {
	h := newHarness(t, outstation.Config{Database: outstation.DatabaseConfig{VirtualTerminal: 2, DefaultClass: dnp3.ClassNone}}, nil)

	if r := h.request(app.FuncRead, app.ReadRange(112, 0, 0, 1)); len(r.Objects) != 0 {
		t.Fatalf("empty terminals returned %d objects", len(r.Objects))
	}
	h.out.Update(func(db *outstation.Database) { db.UpdateVirtualTerminal(1, []byte("hello")) })
	waitFor(t, func() bool { return h.out.Database().Counts().VirtualTerminal == 2 })

	var r app.Fragment
	waitFor(t, func() bool {
		r = h.request(app.FuncRead, app.ReadRange(112, 0, 0, 1))
		return len(r.Objects) == 1
	})
	o := r.Objects[0]
	if o.Variation != 5 || string(o.Data) != "hello" || o.Range.Start != 1 {
		t.Errorf("terminal read = g112v%d at %d holding %q, want g112v5 at 1 holding hello", o.Variation, o.Range.Start, o.Data)
	}
}
