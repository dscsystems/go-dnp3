package conformance

import (
	"testing"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/objects"
	"github.com/dscsystems/go-dnp3/outstation"
)

func freezeHarness(t *testing.T) *harness {
	t.Helper()
	db := smallDB()
	db.Counter, db.FrozenCounter = 3, 3
	h := newHarness(t, outstation.Config{Database: db}, nil)
	h.out.Update(func(d *outstation.Database) {
		for i := range 3 {
			d.UpdateCounter(uint16(i), dnp3.Counter{Value: uint32(100 * (i + 1)), Flags: dnp3.Online})
		}
	})
	waitFor(t, func() bool {
		c, _, _ := h.out.Database().Counter(2)
		return c.Value == 300
	})
	return h
}

func frozen(h *harness, i uint16) uint32 {
	f, _, _ := h.out.Database().FrozenCounter(i)
	return f.Value
}

// freezeAt builds the group 50 variation 2 object that leads a FREEZE_AT_TIME.
func freezeAt(at time.Time, interval time.Duration) app.ObjectHeader {
	data := objects.AppendTime48(nil, dnp3.Now(at))
	ms := uint32(interval / time.Millisecond)
	data = append(data, byte(ms), byte(ms>>8), byte(ms>>16), byte(ms>>24))
	return app.ObjectHeader{
		Group: 50, Variation: 2,
		Qualifier: app.MakeQualifier(app.PrefixNone, app.RangeCount8),
		Range:     app.Range{Spec: app.RangeCount8, Count: 1},
		Data:      data,
	}
}

// FREEZE_CLEAR freezes and then zeroes the running counter.
func TestFreezeClearResetsTheRunningCounter(t *testing.T) {
	h := freezeHarness(t)

	resp := h.request(app.FuncFreezeClear, app.ReadRange(20, 0, 1, 1))
	if resp.Header.IIN.Has(app.IINNoFuncCodeSupport) {
		t.Fatal("FREEZE_CLEAR is not supported")
	}

	if got := frozen(h, 1); got != 200 {
		t.Errorf("frozen[1] = %d, want 200", got)
	}
	if c, _, _ := h.out.Database().Counter(1); c.Value != 0 {
		t.Errorf("counter[1] = %d after FREEZE_CLEAR, want 0", c.Value)
	}
	if c, _, _ := h.out.Database().Counter(0); c.Value != 100 {
		t.Errorf("counter[0] = %d, but only counter 1 was named", c.Value)
	}
	if got := frozen(h, 0); got != 0 {
		t.Errorf("frozen[0] = %d, but only counter 1 was named", got)
	}
}

// FREEZE_AT_TIME freezes when the time arrives, not before, and only the
// counters its trailing headers name.
func TestFreezeAtTimeFreezesWhenTheTimeArrives(t *testing.T) {
	h := freezeHarness(t)

	at := time.Now().Add(300 * time.Millisecond)
	// A trailing header with a non-zero variation and a range qualifier: with
	// the per-fragment rule this was walked as though it carried data.
	resp := h.request(app.FuncFreezeAtTime, freezeAt(at, 0), app.ReadRange(20, 1, 0, 0))
	if resp.Header.IIN.Has(app.IINNoFuncCodeSupport) || resp.Header.IIN.Has(app.IINParameterError) {
		t.Fatalf("IIN = %v", resp.Header.IIN)
	}

	if got := frozen(h, 0); got != 0 {
		t.Fatalf("frozen[0] = %d before the time", got)
	}
	waitFor(t, func() bool { return h.out.Stats().ScheduledFreezes == 1 })

	if got := frozen(h, 0); got != 100 {
		t.Errorf("frozen[0] = %d, want 100", got)
	}
	if got := frozen(h, 1); got != 0 {
		t.Errorf("frozen[1] = %d, but only counter 0 was named", got)
	}
	if f, _, _ := h.out.Database().FrozenCounter(0); f.Time.Time.Sub(at).Abs() > time.Millisecond {
		t.Errorf("the frozen value is stamped %v, want the scheduled %v", f.Time.Time, at)
	}
}

// An interval repeats the freeze, and each freeze takes the counter's value
// at that moment.
func TestFreezeAtTimeRepeatsOnItsInterval(t *testing.T) {
	h := freezeHarness(t)

	h.request(app.FuncFreezeAtTime,
		freezeAt(time.Now().Add(100*time.Millisecond), 200*time.Millisecond), app.ReadAllObjects(20, 0))

	waitFor(t, func() bool { return h.out.Stats().ScheduledFreezes >= 1 })
	h.out.Update(func(d *outstation.Database) {
		d.UpdateCounter(0, dnp3.Counter{Value: 555, Flags: dnp3.Online})
	})
	waitFor(t, func() bool { return frozen(h, 0) == 555 })
}

// A time that has already passed, with nothing to repeat, is refused.
func TestFreezeAtTimeInThePastIsRefused(t *testing.T) {
	h := freezeHarness(t)

	resp := h.request(app.FuncFreezeAtTime,
		freezeAt(time.Now().Add(-time.Minute), 0), app.ReadAllObjects(20, 0))
	if !resp.Header.IIN.Has(app.IINParameterError) {
		t.Errorf("IIN = %v, want PARAMETER_ERROR", resp.Header.IIN)
	}
	time.Sleep(120 * time.Millisecond)
	if got := h.out.Stats().ScheduledFreezes; got != 0 {
		t.Errorf("%d freezes ran for a refused request", got)
	}
}

// FREEZE_AT_TIME without its leading time object names nothing to wait for.
func TestFreezeAtTimeWithoutATimeIsRefused(t *testing.T) {
	h := freezeHarness(t)

	resp := h.request(app.FuncFreezeAtTime, app.ReadAllObjects(20, 0))
	if !resp.Header.IIN.Has(app.IINParameterError) {
		t.Errorf("IIN = %v, want PARAMETER_ERROR", resp.Header.IIN)
	}
}
