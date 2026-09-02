package main

import (
	"context"
	"log/slog"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/channel"
	"github.com/dscsystems/go-dnp3/master"
	"github.com/dscsystems/go-dnp3/outstation"
)

// A setpoint a device accepts and then cannot report back is half a feature:
// writing one and reading it back is how an operator knows the write landed.
// So these tests write over a real session and read the analog output status,
// rather than checking the simulator's own memory of what it was told.

// collector keeps the analog output statuses a master is given.
type collector struct {
	master.NopHandler

	mu      sync.Mutex
	outputs map[uint16]float64
}

func newCollector() *collector {
	return &collector{outputs: map[uint16]float64{}}
}

func (c *collector) HandleAnalogOutputStatus(_ master.HeaderInfo, vs []dnp3.Indexed[dnp3.AnalogOutputStatus]) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, v := range vs {
		c.outputs[v.Index] = v.Value.Value
	}
}

func (c *collector) output(index uint16) (float64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.outputs[index]
	return v, ok
}

// simPair starts the simulated outstation and a master against it.
func simPair(t *testing.T) (*master.Session, *collector, *Simulator) {
	t.Helper()

	cfg := DefaultConfig()
	cfg.injection = Injection{}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("the default configuration does not validate: %v", err)
	}

	sim := NewSimulator(cfg)
	ocfg := cfg.outstationConfig()
	ocfg.LocalAddr, ocfg.RemoteAddr = 10, 1

	plant := &plantHandler{sim: sim, log: discardLogger()}
	out := outstation.New(ocfg, &clock{}, plant)
	cfg.applyPointConfig(out.Database())
	plant.sess = out

	coll := newCollector()
	m := master.New(master.Config{
		LocalAddr: 1, RemoteAddr: 10, ResponseTimeout: 2 * time.Second,
	}, coll)

	mch, och := channel.Pipe()
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); _ = out.Run(ctx, och) }()
	go func() { defer wg.Done(); _ = m.Run(ctx, mch) }()
	go func() { defer wg.Done(); simulate(ctx, out, sim, Injection{}, discardLogger()) }()

	t.Cleanup(func() {
		cancel()
		_ = mch.Close()
		_ = och.Close()
		wg.Wait()
	})

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !m.Connected() {
		time.Sleep(2 * time.Millisecond)
	}
	if !m.Connected() {
		t.Fatal("the master never connected")
	}
	return m, coll, sim
}

// TestSetpointIsAssumedAndReported is the whole point: what a master writes is
// what it reads back.
func TestSetpointIsAssumedAndReported(t *testing.T) {
	m, coll, _ := simPair(t)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	const index, want = uint16(0), 11.05
	if err := operate(ctx, m, index, want); err != nil {
		t.Fatalf("operate: %v", err)
	}

	// Let the simulation write the new value into the database, then read it
	// the way a master confirms a setpoint: by polling it back.
	waitFor(t, 3*time.Second, func() bool {
		if err := m.IntegrityPoll(ctx); err != nil {
			return false
		}
		got, ok := coll.output(index)
		return ok && math.Abs(got-want) < 0.001
	})

	got, _ := coll.output(index)
	if math.Abs(got-want) > 0.001 {
		t.Errorf("the outstation reports %v, want the %v it was written", got, want)
	}
}

// The value has to survive: a device that forgets a setpoint between polls has
// not assumed it at all.
func TestSetpointIsHeld(t *testing.T) {
	m, coll, _ := simPair(t)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	const index, want = uint16(4), 9.0
	if err := operate(ctx, m, index, want); err != nil {
		t.Fatalf("operate: %v", err)
	}
	waitFor(t, 3*time.Second, func() bool {
		_ = m.IntegrityPoll(ctx)
		got, ok := coll.output(index)
		return ok && got == want
	})

	// Several polls later, across many simulation ticks, it is still what was
	// written rather than drifting back to where it started.
	time.Sleep(300 * time.Millisecond)
	for range 3 {
		if err := m.IntegrityPoll(ctx); err != nil {
			t.Fatalf("poll: %v", err)
		}
		if got, _ := coll.output(index); got != want {
			t.Fatalf("the setpoint drifted to %v", got)
		}
	}
}

// An output nobody has written still answers a poll, because the point exists
// whether or not anyone has commanded it.
func TestUnwrittenSetpointsStillReport(t *testing.T) {
	m, coll, _ := simPair(t)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	waitFor(t, 3*time.Second, func() bool {
		_ = m.IntegrityPoll(ctx)
		got, ok := coll.output(0)
		return ok && got == 11.0
	})

	// An output the configuration never named still answers, because the point
	// exists whether or not anybody described it.
	if _, ok := coll.output(1); !ok {
		t.Error("an unnamed analog output did not come back from a class 0 poll")
	}
	// And a named one starts where the configuration says, not at zero.
	if got, _ := coll.output(0); got != 11.0 {
		t.Errorf("setpoint 0 starts at %v, want its configured 11", got)
	}
}

// A value outside the output's range is refused, and refusing it leaves the
// old one alone.
func TestSetpointOutOfRangeRefused(t *testing.T) {
	m, coll, sim := simPair(t)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	// Wait until the starting value is on the wire, so "unchanged" is being
	// checked against something the master has actually seen.
	waitFor(t, 3*time.Second, func() bool {
		_ = m.IntegrityPoll(ctx)
		got, ok := coll.output(0)
		return ok && got == 11.0
	})
	before, _ := sim.Setpoint(0)

	err := operate(ctx, m, 0, 99)
	if err == nil {
		t.Fatal("a setpoint of 99 was accepted on a 10.8..11.2 output")
	}

	if got, _ := sim.Setpoint(0); got != before {
		t.Errorf("the refused write changed the setpoint to %v", got)
	}
	_ = m.IntegrityPoll(ctx)
	if got, ok := coll.output(0); ok && got != before {
		t.Errorf("the outstation reports %v after a refused write, want %v", got, before)
	}
}

// A select must answer whether the operate would be accepted without moving
// anything — the whole reason select-before-operate exists.
func TestSetpointSelectDoesNotMoveIt(t *testing.T) {
	_, _, sim := simPair(t)

	before, _ := sim.Setpoint(0)

	if st := sim.wouldAcceptAnalog(0, 11.2); st != dnp3.CommandSuccess {
		t.Errorf("a value in range was refused: %s", st)
	}
	if st := sim.wouldAcceptAnalog(0, 99); st != dnp3.CommandOutOfRange {
		t.Errorf("a value out of range gave %s", st)
	}
	if st := sim.wouldAcceptAnalog(9000, 1); st != dnp3.CommandNotSupported {
		t.Errorf("an index that does not exist gave %s", st)
	}

	if got, _ := sim.Setpoint(0); got != before {
		t.Errorf("asking moved the setpoint to %v", got)
	}
}

// The unit-level rules, without a session in the way.
func TestSetpointAccepts(t *testing.T) {
	unbounded := SetpointSim{Index: 0}
	bounded := SetpointSim{Index: 1, Min: -5, Max: 5}

	cases := []struct {
		name string
		sp   SetpointSim
		v    float64
		want bool
	}{
		{"unbounded takes anything", unbounded, 1e9, true},
		{"unbounded takes a negative", unbounded, -1e9, true},
		{"in range", bounded, 4.999, true},
		{"on the lower bound", bounded, -5, true},
		{"on the upper bound", bounded, 5, true},
		{"above the range", bounded, 5.001, false},
		{"below the range", bounded, -5.001, false},
		// A NaN compares false against every bound, so a device checking only
		// the limits would take it and then report a value nobody can use.
		{"not a number", unbounded, math.NaN(), false},
		{"infinite", unbounded, math.Inf(1), false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.sp.accepts(c.v); got != c.want {
				t.Errorf("accepts(%v) = %v, want %v", c.v, got, c.want)
			}
		})
	}
}

// A setpoint an analog input shares an index with drags the measurement with
// it, which is the simulator standing in for the plant.
func TestSetpointDrivesTheMeasurement(t *testing.T) {
	m, _, sim := simPair(t)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	// Analog input 0 is the bus voltage, 10.8..11.2, and setpoint 0 overlaps
	// it. Writing inside both ranges moves the measurement.
	if err := operate(ctx, m, 0, 11.1); err != nil {
		t.Fatalf("operate: %v", err)
	}

	sim.mu.Lock()
	value, signal := sim.analogs[0].value, sim.analogs[0].Signal
	sim.mu.Unlock()

	if signal != SignalFixed {
		t.Errorf("the measurement is still %s, want it pinned by the setpoint", signal)
	}
	// Checked before the next simulation tick, which would add the point's
	// configured noise on top: the setpoint pins where the measurement sits,
	// not how steady it is.
	if math.Abs(value-11.1) > 0.001 {
		t.Errorf("the measurement is %v, want it to follow the setpoint to 11.1", value)
	}

	// A setpoint outside the measurement's own range moves the output without
	// dragging the measurement somewhere it could never have been. The default
	// configuration keeps the two ranges aligned, so this needs an output that
	// is deliberately wider than its input.
	wide := &SetpointSim{Index: 1, Min: -1000, Max: 1000}
	sim.mu.Lock()
	sim.setpoints[1] = wide
	inputMax := sim.analogs[1].Max
	sim.mu.Unlock()

	if err := operate(ctx, m, 1, 900); err != nil {
		t.Fatalf("operate: %v", err)
	}
	if got, _ := sim.Setpoint(1); got != 900 {
		t.Errorf("the setpoint is %v, want 900", got)
	}

	sim.mu.Lock()
	measured := sim.analogs[1].value
	sim.mu.Unlock()
	if measured > inputMax {
		t.Errorf("the measurement followed a setpoint to %v, past its own maximum of %v",
			measured, inputMax)
	}
}

// operate writes one setpoint and reports what the outstation said about it,
// so a refusal is an error rather than a successful exchange carrying a
// failure nobody looked at.
func operate(ctx context.Context, m *master.Session, index uint16, v float64) error {
	res, err := m.SelectAndOperate(ctx, master.AnalogOutputFloat32(index, float32(v)))
	if err != nil {
		return err
	}
	return res.Err()
}

// discardLogger keeps the simulator quiet during a test.
func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within %v", d)
}
