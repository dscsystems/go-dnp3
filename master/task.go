package master

import (
	"container/heap"
	"math"
	"slices"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/objects"
)

// Task priorities. Lower runs first when two tasks are due at the same moment.
const (
	priorityStartup   = 0 // clearing the restart indication comes before anything
	priorityCommand   = 1 // an operator waiting on a control beats a poll
	priorityIntegrity = 2
	priorityPoll      = 3
)

// task is one request the master wants to send.
//
// A task is built when it is sent, not when it is queued, so a periodic poll
// queued once produces a fresh request with a current sequence number every
// time it runs.
type task struct {
	name     string
	funcCode app.FuncCode
	priority int

	// build appends the task's object headers to the request.
	build func(*app.Builder) error
	// onFragment runs for each response fragment, before the confirm.
	onFragment func(app.Fragment)
	// onDone runs when the final response fragment arrives.
	onDone func(app.IIN)

	// noResponse marks a request the outstation will not answer, so the task
	// completes as soon as it is on the wire.
	noResponse bool

	// startup marks the steps of the startup sequence, so the session can
	// tell when that sequence is in flight.
	startup bool

	// next, when set, returns a task to run immediately after this one
	// succeeds — bypassing the scheduler so nothing can be interleaved.
	//
	// Select-before-operate needs this. The standard requires the OPERATE to
	// carry the sequence number one above the SELECT, so a periodic poll
	// slipping between them would make the outstation reject the operate with
	// NO_SELECT. Chaining is what guarantees they stay adjacent.
	next func() *task

	// period, when non-zero, reschedules the task after each run.
	period time.Duration

	due      time.Time
	deadline time.Time
	seq      uint8

	// started records that the first fragment of the response has been
	// accepted, which is what tells a continuation apart from a repeat: every
	// fragment of a solicited response carries the request's own sequence
	// number, so the sequence number alone cannot distinguish them.
	started bool
	// respSeq is the sequence number of the last fragment accepted, from
	// which the next in the series is expected to follow.
	respSeq uint8

	// done receives the outcome, for callers waiting on a one-shot task.
	done chan error
	// index is the task's position in the scheduler heap, and order is the
	// sequence it was pushed in.
	index int
	order uint64
}

// finish reports the outcome to a waiting caller exactly once.
func (t *task) finish(err error) {
	if t.done == nil {
		return
	}
	select {
	case t.done <- err:
	default:
	}
	t.done = nil
}

// ---------- Task constructors ----------

// newClearRestartTask writes zero to internal indication index 7, which is how
// a master tells an outstation it has seen the restart.
//
// Until this is done the outstation keeps asserting DEVICE_RESTART on every
// response, and a master that reacts to that indication would re-run its
// startup sequence forever.
func newClearRestartTask() *task {
	return &task{
		name:     "clear-restart",
		funcCode: app.FuncWrite,
		priority: priorityStartup,
		build: func(b *app.Builder) error {
			return b.AddObject(app.ObjectHeader{
				Group: 80, Variation: 1,
				Qualifier: app.MakeQualifier(app.PrefixNone, app.RangeStartStop8),
				Range:     app.Range{Spec: app.RangeStartStop8, Start: 7, Stop: 7, Count: 1},
				Data:      []byte{0x00}, // one packed bit, cleared
			})
		},
	}
}

// newUnsolicitedTask enables or disables unsolicited reporting for a set of
// classes.
func newUnsolicitedTask(enable bool, mask dnp3.Class) *task {
	fc := app.FuncDisableUnsolicited
	name := "disable-unsolicited"
	if enable {
		fc = app.FuncEnableUnsolicited
		name = "enable-unsolicited"
	}

	return &task{
		name:     name,
		funcCode: fc,
		priority: priorityStartup,
		build: func(b *app.Builder) error {
			for _, c := range []struct {
				class     dnp3.Class
				variation uint8
			}{{dnp3.Class1, 2}, {dnp3.Class2, 3}, {dnp3.Class3, 4}} {
				if mask&c.class == 0 {
					continue
				}
				if err := b.AddObject(app.ReadAllObjects(60, c.variation)); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

// newScanTask reads a set of classes.
//
// The class order matters. Events are read before static data so that a value
// which changed during the poll is reported as an event *and* then as its
// current static value, rather than the other way round — which would leave
// the master holding the pre-change value as the latest.
func newScanTask(mask dnp3.Class, _ Handler) *task {
	name := "scan-" + mask.String()
	return &task{
		name:     name,
		funcCode: app.FuncRead,
		priority: scanPriority(mask),
		build: func(b *app.Builder) error {
			for _, c := range []struct {
				class     dnp3.Class
				variation uint8
			}{
				{dnp3.Class1, 2},
				{dnp3.Class2, 3},
				{dnp3.Class3, 4},
				{dnp3.Class0, 1},
			} {
				if mask&c.class == 0 {
					continue
				}
				if err := b.AddObject(app.ReadAllObjects(60, c.variation)); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func scanPriority(mask dnp3.Class) int {
	if mask&dnp3.Class0 != 0 {
		return priorityIntegrity
	}
	return priorityPoll
}

// newRangeScanTask reads a contiguous index range of one group and variation.
func newRangeScanTask(group, variation uint8, start, stop uint16) *task {
	return &task{
		name:     "scan-range",
		funcCode: app.FuncRead,
		priority: priorityPoll,
		build: func(b *app.Builder) error {
			return b.AddObject(app.ReadRange(group, variation, uint32(start), uint32(stop)))
		},
	}
}

// newRestartTask asks the outstation to restart.
func newRestartTask(mode dnp3.RestartMode) *task {
	fc := app.FuncColdRestart
	if mode == dnp3.WarmRestart {
		fc = app.FuncWarmRestart
	}
	return &task{
		name:     "restart-" + mode.String(),
		funcCode: fc,
		priority: priorityCommand,
		build:    func(*app.Builder) error { return nil },
	}
}

// newWriteTimeTask sets the outstation's clock.
func newWriteTimeTask(t time.Time) *task { return newWriteTimeObjectTask("write-time", 1, t) }

// newWriteRecordedTimeTask writes what the master's clock read when it sent
// RECORD_CURRENT_TIME, as group 50 variation 3: the second half of the LAN
// procedure. The outstation adds the time it has held the request since.
func newWriteRecordedTimeTask(t time.Time) *task {
	return newWriteTimeObjectTask("write-recorded-time", 3, t)
}

func newWriteTimeObjectTask(name string, variation uint8, t time.Time) *task {
	ms := dnp3.TimeToDNP3(t)
	return &task{
		name:     name,
		funcCode: app.FuncWrite,
		priority: priorityStartup,
		build: func(b *app.Builder) error {
			return b.AddObject(app.ObjectHeader{
				Group: 50, Variation: variation,
				Qualifier: app.MakeQualifier(app.PrefixNone, app.RangeCount8),
				Range:     app.Range{Spec: app.RangeCount8, Count: 1},
				Data: []byte{
					byte(ms), byte(ms >> 8), byte(ms >> 16),
					byte(ms >> 24), byte(ms >> 32), byte(ms >> 40),
				},
			})
		},
	}
}

// newDelayMeasureTask asks the outstation how long it takes to turn a request
// around, which is the first half of the serial time-synchronisation
// procedure.
//
// delayMillis receives the outstation's answer.
func newDelayMeasureTask(delayMillis *uint32) *task {
	return &task{
		name:     "delay-measure",
		funcCode: app.FuncDelayMeasure,
		priority: priorityStartup,
		build:    func(*app.Builder) error { return nil },
		onFragment: func(frag app.Fragment) {
			for _, h := range frag.Objects {
				if h.Group == 52 && len(h.Data) >= 2 {
					*delayMillis = objects.ParseTimeDelay(h.Variation, h.Data)
					return
				}
			}
		},
	}
}

// newWriteDeadbandTask sets the analog deadbands of a set of points.
func newWriteDeadbandTask(deadbands map[uint16]float32) *task {
	// Sorted so the request is deterministic, which matters when comparing
	// captures and when an outstation logs what it was told.
	indexes := make([]uint16, 0, len(deadbands))
	for i := range deadbands {
		indexes = append(indexes, i)
	}
	slices.Sort(indexes)

	return &task{
		name:     "write-deadband",
		funcCode: app.FuncWrite,
		priority: priorityCommand,
		build: func(b *app.Builder) error {
			// The index prefix is widened when any index passes 255; a
			// one-octet prefix would set the deadband of the wrong point.
			// indexes is sorted, so the last one is the largest.
			prefix, spec := app.PrefixIndex1, app.RangeCount8
			if len(indexes) > 0 && indexes[len(indexes)-1] > 0xFF {
				prefix, spec = app.PrefixIndex2, app.RangeCount16
			}
			data := make([]byte, 0, (prefix.Octets()+4)*len(indexes))
			for _, i := range indexes {
				bits := math.Float32bits(deadbands[i])
				if prefix == app.PrefixIndex2 {
					data = append(data, byte(i), byte(i>>8))
				} else {
					data = append(data, byte(i))
				}
				data = append(data,
					byte(bits), byte(bits>>8), byte(bits>>16), byte(bits>>24))
			}
			return b.AddObject(app.ObjectHeader{
				Group: 34, Variation: 3, // single precision
				Qualifier: app.MakeQualifier(prefix, spec),
				Range:     app.Range{Spec: spec, Count: uint32(len(indexes))},
				Data:      data,
			})
		},
	}
}

// ---------- Scheduler ----------

// scheduler orders pending tasks by when they are due, then by priority.
//
// A heap rather than a sorted slice because a busy master with several
// periodic polls re-queues a task on every run, and re-sorting each time is
// the one place this loop could get quadratic.
type scheduler struct {
	q     taskQueue
	pushN uint64
}

func (s *scheduler) push(t *task) {
	// The push order is the final tiebreaker, so tasks that are due at the
	// same instant with the same priority run in the order they were queued.
	// Without it a heap orders equal keys arbitrarily, and a startup sequence
	// whose steps share a priority would run in a different order each time.
	s.pushN++
	t.order = s.pushN
	heap.Push(&s.q, t)
}

func (s *scheduler) pop() *task {
	if s.q.Len() == 0 {
		return nil
	}
	return heap.Pop(&s.q).(*task)
}

func (s *scheduler) peek() (*task, bool) {
	if s.q.Len() == 0 {
		return nil, false
	}
	return s.q[0], true
}

func (s *scheduler) len() int { return s.q.Len() }

// clear drops every pending one-shot task, failing anything a caller is
// waiting on.
//
// This runs when the outstation reports a restart: the queued work was aimed
// at a device state that no longer exists. Periodic tasks are kept, because a
// periodic task is only re-queued after it runs and would otherwise be lost
// for good; they poll the restarted device as they always did.
func (s *scheduler) clear() {
	kept := s.q[:0]
	for _, t := range s.q {
		if t.period > 0 {
			kept = append(kept, t)
			continue
		}
		t.finish(dnp3.ErrTaskFailed)
	}
	for i := len(kept); i < len(s.q); i++ {
		s.q[i] = nil
	}
	s.q = kept
	heap.Init(&s.q)
}

type taskQueue []*task

func (q taskQueue) Len() int { return len(q) }

func (q taskQueue) Less(i, j int) bool {
	if !q[i].due.Equal(q[j].due) {
		return q[i].due.Before(q[j].due)
	}
	if q[i].priority != q[j].priority {
		return q[i].priority < q[j].priority
	}
	return q[i].order < q[j].order
}

func (q taskQueue) Swap(i, j int) {
	q[i], q[j] = q[j], q[i]
	q[i].index, q[j].index = i, j
}

func (q *taskQueue) Push(x any) {
	t := x.(*task)
	t.index = len(*q)
	*q = append(*q, t)
}

func (q *taskQueue) Pop() any {
	old := *q
	n := len(old)
	t := old[n-1]
	old[n-1] = nil
	*q = old[:n-1]
	return t
}

// newFreezeTask asks the outstation to freeze every counter now, clearing the
// running counters afterwards when clear is set.
func newFreezeTask(clear bool) *task {
	fc := app.FuncImmedFreeze
	name := "freeze"
	if clear {
		fc, name = app.FuncFreezeClear, "freeze-clear"
	}
	return &task{
		name:     name,
		funcCode: fc,
		priority: priorityCommand,
		build: func(b *app.Builder) error {
			return b.AddObject(app.ReadAllObjects(20, 0))
		},
	}
}

// newFreezeAtTimeTask asks the outstation to freeze every counter at a time,
// and again every interval when that is non-zero.
func newFreezeAtTimeTask(at time.Time, interval time.Duration) *task {
	data := objects.AppendTime48(nil, dnp3.Now(at))
	ms := uint32(interval / time.Millisecond)
	data = append(data, byte(ms), byte(ms>>8), byte(ms>>16), byte(ms>>24))
	return &task{
		name:     "freeze-at-time",
		funcCode: app.FuncFreezeAtTime,
		priority: priorityCommand,
		build: func(b *app.Builder) error {
			if err := b.AddObject(app.ObjectHeader{
				Group: 50, Variation: 2,
				Qualifier: app.MakeQualifier(app.PrefixNone, app.RangeCount8),
				Range:     app.Range{Spec: app.RangeCount8, Count: 1},
				Data:      data,
			}); err != nil {
				return err
			}
			return b.AddObject(app.ReadAllObjects(20, 0))
		},
	}
}
