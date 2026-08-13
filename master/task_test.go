package master

import (
	"testing"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/internal/app"
)

func TestSchedulerOrdersByDueThenPriority(t *testing.T) {
	var s scheduler
	now := time.Now()

	later := &task{name: "later", due: now.Add(time.Second), priority: priorityStartup}
	pollNow := &task{name: "poll", due: now, priority: priorityPoll}
	startupNow := &task{name: "startup", due: now, priority: priorityStartup}

	s.push(later)
	s.push(pollNow)
	s.push(startupNow)

	// Earliest due wins, and among equals the lower priority number wins —
	// clearing the restart indication must precede the poll that would
	// otherwise observe it and restart the sequence.
	for _, want := range []string{"startup", "poll", "later"} {
		got := s.pop()
		if got == nil {
			t.Fatalf("scheduler ran dry expecting %q", want)
		}
		if got.name != want {
			t.Errorf("popped %q, want %q", got.name, want)
		}
	}
	if s.len() != 0 {
		t.Errorf("%d tasks left", s.len())
	}
}

func TestSchedulerPeekDoesNotRemove(t *testing.T) {
	var s scheduler
	s.push(&task{name: "a", due: time.Now()})

	if got, ok := s.peek(); !ok || got.name != "a" {
		t.Fatalf("peek = %v, %v", got, ok)
	}
	if s.len() != 1 {
		t.Error("peek removed the task")
	}
	if _, ok := (&scheduler{}).peek(); ok {
		t.Error("peek on an empty scheduler should report false")
	}
}

// TestSchedulerClearFailsWaiters covers what happens when an outstation
// restarts: the queued work was aimed at a device state that no longer exists,
// and anyone blocked on it must be told rather than left hanging.
func TestSchedulerClearFailsWaiters(t *testing.T) {
	var s scheduler
	done := make(chan error, 1)
	s.push(&task{name: "queued", due: time.Now(), done: done})

	s.clear()

	select {
	case err := <-done:
		if err == nil {
			t.Error("a cleared task reported success")
		}
	default:
		t.Error("clearing the scheduler left a caller waiting forever")
	}
	if s.len() != 0 {
		t.Errorf("%d tasks left after clear", s.len())
	}
}

func TestTaskFinishIsIdempotent(t *testing.T) {
	// A task can be completed and then have a late response arrive. Sending
	// twice on the completion channel would panic or block.
	done := make(chan error, 1)
	tk := &task{done: done}

	tk.finish(nil)
	tk.finish(dnp3.ErrTimeout)

	if err := <-done; err != nil {
		t.Errorf("first outcome = %v, want nil", err)
	}
	select {
	case <-done:
		t.Error("the second finish also sent")
	default:
	}
}

func TestTaskFinishWithoutWaiter(t *testing.T) {
	// A periodic task has no waiter; finishing it must not panic.
	(&task{}).finish(nil)
}

// ---------- Request construction ----------

// buildRequest renders a task's request the way the session does, so the
// octets can be checked against the standard.
func buildRequest(t *testing.T, tk *task, seq uint8) app.Fragment {
	t.Helper()

	b := app.NewBuilder(0)
	if err := b.SetHeader(app.Header{
		Control: app.Control{Fir: true, Fin: true, Seq: seq},
		Func:    tk.funcCode,
	}); err != nil {
		t.Fatal(err)
	}
	if err := tk.build(b); err != nil {
		t.Fatal(err)
	}

	frag, err := app.ParseRequest(nil, b.Bytes())
	if err != nil {
		t.Fatalf("the task built a request its own parser rejects: %v", err)
	}
	return frag
}

// TestIntegrityScanReadsEventsBeforeStatic pins the class ordering.
//
// Events come first so a point that changes during the poll is reported as an
// event and *then* as its current static value. The other order would leave
// the master holding the pre-change value as the newest thing it knows.
func TestIntegrityScanReadsEventsBeforeStatic(t *testing.T) {
	frag := buildRequest(t, newScanTask(dnp3.ClassAll, nil), 0)

	if frag.Header.Func != app.FuncRead {
		t.Errorf("func = %v, want READ", frag.Header.Func)
	}
	if len(frag.Objects) != 4 {
		t.Fatalf("%d object headers, want 4", len(frag.Objects))
	}
	// Group 60 variations 2, 3, 4 are classes 1, 2, 3; variation 1 is class 0.
	for i, want := range []uint8{2, 3, 4, 1} {
		o := frag.Objects[i]
		if o.Group != 60 || o.Variation != want {
			t.Errorf("header %d = g%dv%d, want g60v%d", i, o.Group, o.Variation, want)
		}
		if o.Qualifier.RangeSpec() != app.RangeAllObjects {
			t.Errorf("header %d qualifier = %v, want all-objects", i, o.Qualifier)
		}
	}
}

func TestClassScanOmitsUnrequestedClasses(t *testing.T) {
	frag := buildRequest(t, newScanTask(dnp3.Class1|dnp3.Class3, nil), 1)

	if len(frag.Objects) != 2 {
		t.Fatalf("%d headers, want 2", len(frag.Objects))
	}
	for i, want := range []uint8{2, 4} { // classes 1 and 3
		if got := frag.Objects[i].Variation; got != want {
			t.Errorf("header %d = variation %d, want %d", i, got, want)
		}
	}
}

// TestClearRestartTaskWritesIndexSeven pins the exact handshake that ends an
// outstation's restart state. Writing the wrong index leaves the indication
// set, and a master that reacts to it re-runs startup forever.
func TestClearRestartTaskWritesIndexSeven(t *testing.T) {
	frag := buildRequest(t, newClearRestartTask(), 0)

	if frag.Header.Func != app.FuncWrite {
		t.Errorf("func = %v, want WRITE", frag.Header.Func)
	}
	if len(frag.Objects) != 1 {
		t.Fatalf("%d headers, want 1", len(frag.Objects))
	}
	o := frag.Objects[0]
	if o.Group != 80 || o.Variation != 1 {
		t.Errorf("object = g%dv%d, want g80v1", o.Group, o.Variation)
	}
	if o.Range.Start != 7 || o.Range.Stop != 7 {
		t.Errorf("range = [%d..%d], want [7..7]", o.Range.Start, o.Range.Stop)
	}
	if len(o.Data) != 1 || o.Data[0] != 0 {
		t.Errorf("data = % x, want a single cleared bit", o.Data)
	}
}

func TestUnsolicitedTaskFunctionCodes(t *testing.T) {
	enable := buildRequest(t, newUnsolicitedTask(true, dnp3.Class123), 0)
	if enable.Header.Func != app.FuncEnableUnsolicited {
		t.Errorf("enable func = %v", enable.Header.Func)
	}
	if len(enable.Objects) != 3 {
		t.Errorf("%d headers for three classes", len(enable.Objects))
	}

	disable := buildRequest(t, newUnsolicitedTask(false, dnp3.Class1), 0)
	if disable.Header.Func != app.FuncDisableUnsolicited {
		t.Errorf("disable func = %v", disable.Header.Func)
	}
	if len(disable.Objects) != 1 {
		t.Errorf("%d headers for one class", len(disable.Objects))
	}
}

func TestRestartTaskFunctionCodes(t *testing.T) {
	cold := buildRequest(t, newRestartTask(dnp3.ColdRestart), 0)
	if cold.Header.Func != app.FuncColdRestart {
		t.Errorf("cold restart func = %v", cold.Header.Func)
	}
	if len(cold.Objects) != 0 {
		t.Errorf("a restart request carries %d objects, want none", len(cold.Objects))
	}

	warm := buildRequest(t, newRestartTask(dnp3.WarmRestart), 0)
	if warm.Header.Func != app.FuncWarmRestart {
		t.Errorf("warm restart func = %v", warm.Header.Func)
	}
}

func TestWriteTimeTaskEncodesTimestamp(t *testing.T) {
	when := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	frag := buildRequest(t, newWriteTimeTask(when), 0)

	if frag.Header.Func != app.FuncWrite {
		t.Errorf("func = %v, want WRITE", frag.Header.Func)
	}
	o := frag.Objects[0]
	if o.Group != 50 || o.Variation != 1 {
		t.Errorf("object = g%dv%d, want g50v1", o.Group, o.Variation)
	}
	if len(o.Data) != 6 {
		t.Fatalf("timestamp is %d octets, want 6", len(o.Data))
	}

	var ms uint64
	for i, b := range o.Data {
		ms |= uint64(b) << (8 * i)
	}
	if got := dnp3.DNP3ToTime(ms); !got.Equal(when) {
		t.Errorf("encoded time = %v, want %v", got, when)
	}
}

func TestRangeScanTaskNarrowsEncoding(t *testing.T) {
	frag := buildRequest(t, newRangeScanTask(30, 1, 5, 9), 0)

	o := frag.Objects[0]
	if o.Group != 30 || o.Variation != 1 {
		t.Errorf("object = g%dv%d, want g30v1", o.Group, o.Variation)
	}
	if o.Range.Start != 5 || o.Range.Stop != 9 {
		t.Errorf("range = [%d..%d], want [5..9]", o.Range.Start, o.Range.Stop)
	}
	if o.Qualifier.RangeSpec() != app.RangeStartStop8 {
		t.Errorf("qualifier = %v, want the 8-bit range for indexes under 256", o.Qualifier)
	}
}

func TestScanTaskPriority(t *testing.T) {
	// An integrity poll outranks a routine event poll, so a re-baseline after
	// a restart is not stuck behind the periodic scans.
	if newScanTask(dnp3.ClassAll, nil).priority >= newScanTask(dnp3.Class123, nil).priority {
		t.Error("an integrity poll should outrank a class poll")
	}
}
