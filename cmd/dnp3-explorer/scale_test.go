package main

import (
	"context"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/channel"
	"github.com/dscsystems/go-dnp3/master"
	"github.com/dscsystems/go-dnp3/outstation"
)

// A real substation gateway reports thousands of points, and an integrity poll
// delivers them in a couple of milliseconds. The interface has to accept that
// or say what it lost — and what it must never do is lose a whole object group
// silently, which is what happens when a queue fills partway through one:
// static data arrives grouped, so a buffer that fills during the binary inputs
// takes the binary outputs behind them with it.
//
// The numbers here are from a device in the field: 3306 binary inputs, 1552
// analogs, 298 binary outputs, 351 analog outputs.
const (
	scaleBinary       = 3306
	scaleAnalog       = 1552
	scaleBinaryOutput = 298
	scaleAnalogOutput = 351
)

// scaleOutstation is a device with a field-sized database.
func scaleOutstation(t *testing.T) (channel.Channel, *outstation.Session) {
	t.Helper()

	out := outstation.New(outstation.Config{
		LocalAddr: 10, RemoteAddr: 1,
		Database: outstation.DatabaseConfig{
			Binary:             scaleBinary,
			Analog:             scaleAnalog,
			BinaryOutputStatus: scaleBinaryOutput,
			AnalogOutputStatus: scaleAnalogOutput,
			DefaultClass:       dnp3.Class1,
		},
	}, nil, nil)

	// Every point carries a value, so a missing one is a point that did not
	// arrive rather than one nobody set.
	out.Update(func(db *outstation.Database) {
		for i := range uint16(scaleBinary) {
			db.UpdateBinary(i, dnp3.Binary{Value: i%2 == 0, Flags: dnp3.Online})
		}
		for i := range uint16(scaleAnalog) {
			db.UpdateAnalog(i, dnp3.Analog{Value: float64(i), Flags: dnp3.Online})
		}
		for i := range uint16(scaleBinaryOutput) {
			db.UpdateBinaryOutputStatus(i, dnp3.BinaryOutputStatus{Value: true, Flags: dnp3.Online})
		}
		for i := range uint16(scaleAnalogOutput) {
			db.UpdateAnalogOutputStatus(i, dnp3.AnalogOutputStatus{Value: float64(i), Flags: dnp3.Online})
		}
	})

	mch, och := channel.Pipe()
	ctx, cancel := context.WithCancel(t.Context())

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = out.Run(ctx, och) }()
	t.Cleanup(func() {
		cancel()
		_ = mch.Close()
		_ = och.Close()
		wg.Wait()
	})

	return mch, out
}

// TestScaleIntegrityPollReachesTheModel is the regression: every point the
// device reports has to reach the table, through the same message path the
// terminal uses.
//
// It fails before the drain was batched. One message per point and one message
// per pass through the event loop means the queue fills long before the poll
// finishes, and the groups that had not yet arrived — binary outputs among
// them — are dropped whole.
func TestScaleIntegrityPollReachesTheModel(t *testing.T) {
	mch, _ := scaleOutstation(t)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	conn := &connection{msgs: make(chan tea.Msg, 2048), ctx: ctx}
	h := &handler{conn: conn}
	sess := master.New(master.Config{
		LocalAddr: 1, RemoteAddr: 10, ResponseTimeout: 10 * time.Second,
	}, h)
	h.sess = sess
	conn.adopt(sess, link{Demo: true, Local: 1, Remote: 10})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = sess.Run(ctx, mch) }()
	t.Cleanup(func() { cancel(); wg.Wait() })

	m := testModel()
	m.conn = conn

	// The model runs the way Bubble Tea runs it: a command produces a message,
	// Update applies it and returns the next command. Nothing here reaches
	// into the channel directly, because the bug being guarded against lives
	// exactly in how much one turn of that loop takes.
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		cmd := m.conn.wait()
		for {
			msg := cmd()
			if _, ok := msg.(statusMsg); ok && msg.(statusMsg).text == "closed" {
				return
			}

			var next tea.Cmd
			switch v := msg.(type) {
			case batchMsg:
				for _, one := range v {
					m.applySessionMsg(one)
				}
				next = m.conn.wait()
			default:
				m.applySessionMsg(msg)
				next = m.conn.wait()
			}
			cmd = next

			if m.pointsSeen() >= scaleBinary+scaleAnalog+scaleBinaryOutput+scaleAnalogOutput {
				return
			}
		}
	}()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && !sess.Connected() {
		time.Sleep(5 * time.Millisecond)
	}
	pollCtx, pollCancel := context.WithTimeout(ctx, 20*time.Second)
	defer pollCancel()
	if err := sess.IntegrityPoll(pollCtx); err != nil {
		t.Fatalf("integrity poll: %v", err)
	}

	select {
	case <-drained:
	case <-time.After(15 * time.Second):
	}

	counts := m.pointCounts()
	want := map[dnp3.PointType]int{
		dnp3.TypeBinary:             scaleBinary,
		dnp3.TypeAnalog:             scaleAnalog,
		dnp3.TypeBinaryOutputStatus: scaleBinaryOutput,
		dnp3.TypeAnalogOutputStatus: scaleAnalogOutput,
	}
	for typ, n := range want {
		if counts[typ] != n {
			t.Errorf("%s: the table holds %d of %d", typeLabel(typ), counts[typ], n)
		}
	}
	if dropped := conn.dropped.Load(); dropped != 0 {
		t.Errorf("%d messages were dropped; the interface fell behind the device", dropped)
	}
}

// TestBatchDrainTakesEverythingQueued is the unit-level guarantee underneath
// it: one turn of the event loop takes what is waiting, not one of it.
func TestBatchDrainTakesEverythingQueued(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	conn := &connection{msgs: make(chan tea.Msg, 4096), ctx: ctx}
	for i := range 1000 {
		conn.push(updateMsg{Type: dnp3.TypeBinary, Index: uint16(i)})
	}
	if dropped := conn.dropped.Load(); dropped != 0 {
		t.Fatalf("%d dropped while filling the queue", dropped)
	}

	msg := conn.wait()()
	batch, ok := msg.(batchMsg)
	if !ok {
		t.Fatalf("wait returned %T, want a batch", msg)
	}
	if len(batch) != 1000 {
		t.Errorf("the batch holds %d of the 1000 queued", len(batch))
	}
}

// A batch must not be bounded away to nothing either: the cap exists so one
// Update cannot run forever, not to throw work away.
func TestBatchDrainIsBounded(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	conn := &connection{msgs: make(chan tea.Msg, maxBatch*2), ctx: ctx}
	for i := range maxBatch + 500 {
		conn.push(updateMsg{Type: dnp3.TypeBinary, Index: uint16(i)})
	}

	batch, ok := conn.wait()().(batchMsg)
	if !ok {
		t.Fatal("wait did not return a batch")
	}
	if len(batch) != maxBatch {
		t.Errorf("the batch holds %d, want it capped at %d", len(batch), maxBatch)
	}
	// The rest is still queued rather than discarded.
	rest, ok := conn.wait()().(batchMsg)
	if !ok {
		t.Fatal("the remainder did not come back")
	}
	if len(rest) != 500 {
		t.Errorf("the remainder holds %d, want 500", len(rest))
	}
}

// pointsSeen is how many distinct points the table holds.
func (m *Model) pointsSeen() int { return len(m.points) }

// pointCounts breaks that down by point type.
func (m *Model) pointCounts() map[dnp3.PointType]int {
	out := map[dnp3.PointType]int{}
	for key := range m.points {
		out[key.Type]++
	}
	return out
}
