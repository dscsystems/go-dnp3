package conformance

import (
	"testing"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/objects"
	"github.com/dscsystems/go-dnp3/outstation"
)

// eventDB is a database whose binary points report into class 2 and analog
// points into class 1, so a read by class and a read by type differ.
func eventHarness(t *testing.T, maxFrag int) *harness {
	t.Helper()
	h := newHarness(t, outstation.Config{
		Database: outstation.DatabaseConfig{
			Binary: 40, Analog: 10, DefaultClass: dnp3.Class1,
		},
		MaxTxFragment:  maxFrag,
		ConfirmTimeout: 2 * time.Second,
	}, nil)
	h.out.Update(func(db *outstation.Database) {
		for i := range 40 {
			db.Configure(dnp3.TypeBinary, uint16(i), outstation.PointConfig{Class: dnp3.Class2})
		}
	})
	return h
}

// collectResponse sends a read and gathers every fragment of its response,
// confirming each as it goes.
func collectResponse(h *harness, objs ...app.ObjectHeader) []app.Fragment {
	h.t.Helper()
	before := h.count()
	h.send(app.FuncRead, objs...)
	var out []app.Fragment
	for {
		frag := h.await(before + len(out))
		out = append(out, frag)
		if frag.Header.Control.Con {
			h.sendConfirm(frag.Header.Control.Seq)
		}
		if frag.Header.Control.Fin {
			return out
		}
	}
}

func eventGroups(frags []app.Fragment, group uint8) (headers []app.ObjectHeader) {
	for _, f := range frags {
		for _, o := range f.Objects {
			if o.Group == group {
				headers = append(headers, o)
			}
		}
	}
	return headers
}

// A read of an event group returns events in the variation named, and does not
// fall through to the static data of the group with the same number.
func TestReadOfAnEventGroupReturnsEventsInTheRequestedVariation(t *testing.T) {
	h := eventHarness(t, 0)
	now := time.Now()
	h.out.Update(func(db *outstation.Database) {
		db.UpdateBinary(3, dnp3.Binary{Value: true, Flags: dnp3.Online, Time: dnp3.Now(now)})
		db.UpdateAnalog(1, dnp3.Analog{Value: 5, Flags: dnp3.Online, Time: dnp3.Now(now)})
	})
	waitFor(t, func() bool { return h.out.Events().Total() == 2 })

	frags := collectResponse(h, app.ReadAllObjects(2, 1))

	if got := eventGroups(frags, 1); len(got) != 0 {
		t.Errorf("g2v1 read returned %d static g1 headers", len(got))
	}
	ev := eventGroups(frags, 2)
	if len(ev) != 1 || ev[0].Variation != 1 || ev[0].Range.Count != 1 {
		t.Fatalf("g2 headers = %+v, want one g2v1 carrying one event", ev)
	}
	if eventGroups(frags, 32) != nil {
		t.Error("a read of binary events also returned analog events")
	}
	// The analog event, another type in another class, is untouched.
	waitFor(t, func() bool { return h.out.Events().Total() == 1 }) // the confirm is processed asynchronously
}

// A read by type takes the events whatever class they were assigned, and an
// unknown variation says so instead of answering with nothing.
func TestEventReadIgnoresClassAndRejectsUnknownVariations(t *testing.T) {
	h := eventHarness(t, 0)
	h.out.Update(func(db *outstation.Database) {
		db.UpdateBinary(0, dnp3.Binary{Value: true, Flags: dnp3.Online, Time: dnp3.Now(time.Now())})
	})
	waitFor(t, func() bool { return h.out.Events().Total() == 1 })

	frags := collectResponse(h, app.ReadAllObjects(2, 2))
	if ev := eventGroups(frags, 2); len(ev) != 1 || ev[0].Variation != 2 {
		t.Errorf("g2 headers = %+v, want one g2v2", ev)
	}

	resp := h.request(app.FuncRead, app.ReadAllObjects(2, 9))
	if !resp.Header.IIN.Has(app.IINObjectUnknown) {
		t.Errorf("g2v9: IIN = %v, want OBJECT_UNKNOWN", resp.Header.IIN)
	}

	// A start-stop range means nothing for events.
	resp = h.request(app.FuncRead, app.ReadRange(2, 1, 0, 3))
	if !resp.Header.IIN.Has(app.IINParameterError) {
		t.Errorf("g2v1 start-stop: IIN = %v, want PARAMETER_ERROR", resp.Header.IIN)
	}
}

func countObjects(hs []app.ObjectHeader) (n int) {
	for _, h := range hs {
		n += int(h.Range.Count)
	}
	return n
}

func countHeader(group, variation uint8, n uint8) app.ObjectHeader {
	return app.ObjectHeader{
		Group: group, Variation: variation,
		Qualifier: app.MakeQualifier(app.PrefixNone, app.RangeCount8),
		Range:     app.Range{Spec: app.RangeCount8, Count: uint32(n)},
	}
}

// A count on an event read, or on a class read, limits how many events come
// back; the rest stay queued for the next read.
func TestEventReadsHonourTheirCount(t *testing.T) {
	h := eventHarness(t, 0)
	h.out.Update(func(db *outstation.Database) {
		for i := range 10 {
			db.UpdateBinary(uint16(i), dnp3.Binary{Value: true, Flags: dnp3.Online, Time: dnp3.Now(time.Now())})
		}
	})
	waitFor(t, func() bool { return h.out.Events().Total() == 10 })

	frags := collectResponse(h, countHeader(2, 2, 4))
	if got := countObjects(eventGroups(frags, 2)); got != 4 {
		t.Errorf("g2v2 count 4 returned %d events", got)
	}
	waitFor(t, func() bool { return h.out.Events().Total() == 6 }) // 4 of 10 confirmed

	// The same for a class read: class 2 holds the binary events.
	frags = collectResponse(h, countHeader(60, 3, 2))
	if got := countObjects(eventGroups(frags, 2)); got != 2 {
		t.Errorf("class 2 count 2 returned %d events", got)
	}
}

// Every fragment of a relative-time response carries its own base, and each
// event resolves to the time it was stamped with.
func TestRelativeTimeEventsCarryACTOInEveryFragment(t *testing.T) {
	h := eventHarness(t, 60) // a few events per fragment
	base := time.Now().Truncate(time.Millisecond)
	want := map[uint32]time.Time{}
	h.out.Update(func(db *outstation.Database) {
		for i := range 12 {
			at := base.Add(time.Duration(i) * 1500 * time.Millisecond)
			want[uint32(i)] = at
			db.UpdateBinary(uint16(i), dnp3.Binary{Value: true, Flags: dnp3.Online, Time: dnp3.Now(at)})
		}
	})
	waitFor(t, func() bool { return h.out.Events().Total() == 12 })

	frags := collectResponse(h, app.ReadAllObjects(2, 3))
	if len(frags) < 2 {
		t.Fatalf("%d fragment(s); the test needs several", len(frags))
	}

	got := map[uint32]time.Time{}
	for n, f := range frags {
		ctx := objects.Context{}
		for _, o := range f.Objects {
			switch o.Group {
			case 51:
				ctx = ctx.WithGroup51(objects.ParseTime48(o.Data).Time, o.Variation)
			case 2:
				if !ctx.HasCTO {
					t.Fatalf("fragment %d: g2v3 with no CTO before it in the same fragment", n)
				}
				c, _ := objects.BinaryCodec(objects.GV(2, 3))
				prefix := o.Qualifier.IndexPrefix().Octets()
				step := prefix + 3
				for k := 0; k < int(o.Range.Count); k++ {
					idx := uint32(o.Data[k*step])
					got[idx] = c.Parse(o.Data[k*step+prefix:], ctx).Time.Time
				}
			}
		}
	}

	if len(got) != 12 {
		t.Fatalf("resolved %d events, want 12", len(got))
	}
	for i, w := range want {
		if d := got[i].Sub(w).Abs(); d > time.Millisecond {
			t.Errorf("event %d resolves to %v, want %v", i, got[i], w)
		}
	}
}

// An offset is sixteen bits of milliseconds, so events more than 65.535 s past
// the base start a new one.
func TestRelativeTimeEventsRenewTheCTOPastSixteenBits(t *testing.T) {
	h := eventHarness(t, 0)
	base := time.Now().Truncate(time.Millisecond)
	h.out.Update(func(db *outstation.Database) {
		for i, offset := range []time.Duration{0, 30 * time.Second, 70 * time.Second, 71 * time.Second} {
			db.UpdateBinary(uint16(i), dnp3.Binary{Value: true, Flags: dnp3.Online,
				Time: dnp3.Now(base.Add(offset))})
		}
	})
	waitFor(t, func() bool { return h.out.Events().Total() == 4 })

	frags := collectResponse(h, app.ReadAllObjects(2, 3))
	ctos := eventGroups(frags, 51)
	if len(ctos) != 2 {
		t.Fatalf("%d CTO objects, want 2 (events at 0s and 30s share one, 70s and 71s the next)", len(ctos))
	}
	second := objects.ParseTime48(ctos[1].Data).Time
	if d := second.Sub(base.Add(70 * time.Second)).Abs(); d > time.Millisecond {
		t.Errorf("second CTO = %v, want the 70s event's time", second)
	}
}

// The variation of the CTO says whether the clock behind it was synchronised.
func TestCTOVariationReflectsClockSynchronisation(t *testing.T) {
	h := eventHarness(t, 0)
	h.out.Update(func(db *outstation.Database) {
		db.UpdateBinary(0, dnp3.Binary{Value: true, Flags: dnp3.Online, Time: dnp3.Now(time.Now())})
	})
	waitFor(t, func() bool { return h.out.Events().Total() == 1 })

	frags := collectResponse(h, app.ReadAllObjects(2, 3))
	if c := eventGroups(frags, 51); len(c) != 1 || c[0].Variation != 2 {
		t.Fatalf("CTO headers = %+v, want one g51v2 while the clock is unset", c)
	}

	// Set the clock, and give it something new to report.
	ms := dnp3.TimeToDNP3(time.Now())
	h.request(app.FuncWrite, app.ObjectHeader{
		Group: 50, Variation: 1,
		Qualifier: app.MakeQualifier(app.PrefixNone, app.RangeCount8),
		Range:     app.Range{Spec: app.RangeCount8, Count: 1},
		Data:      []byte{byte(ms), byte(ms >> 8), byte(ms >> 16), byte(ms >> 24), byte(ms >> 32), byte(ms >> 40)},
	})
	h.out.Update(func(db *outstation.Database) {
		db.UpdateBinary(1, dnp3.Binary{Value: true, Flags: dnp3.Online, Time: dnp3.Now(time.Now())})
	})
	waitFor(t, func() bool { return h.out.Events().Total() == 1 })
	frags = collectResponse(h, app.ReadAllObjects(2, 3))
	if c := eventGroups(frags, 51); len(c) != 1 || c[0].Variation != 1 {
		t.Errorf("CTO headers = %+v, want one g51v1 once the clock is set", c)
	}
}
