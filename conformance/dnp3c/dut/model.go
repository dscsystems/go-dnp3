// Copyright (C) 2026 Ricardo Olsen / DSC Systems.
//
// This program is free software: you can redistribute it and/or modify it
// under the terms of the GNU General Public License as published by the Free
// Software Foundation, either version 3 of the License, or (at your option)
// any later version. It is distributed WITHOUT ANY WARRANTY; without even the
// implied warranty of MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.
// See the GNU General Public License for more details, in the LICENSE file at
// the root of this repository or at <https://www.gnu.org/licenses/>.

package main

import (
	"fmt"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/outstation"
)

// kind is a type of point, named as the plugin names it on the wire.
type kind string

const (
	kindBinary        kind = "binary"
	kindDoubleBit     kind = "doubleBit"
	kindCounter       kind = "counter"
	kindFrozenCounter kind = "frozenCounter"
	kindAnalog        kind = "analog"
	kindFrozenAnalog  kind = "frozenAnalog"
	kindBinaryOutput  kind = "binaryOutputStatus"
	kindAnalogOutput  kind = "analogOutputStatus"
)

func (k kind) pointType() dnp3.PointType {
	switch k {
	case kindBinary:
		return dnp3.TypeBinary
	case kindDoubleBit:
		return dnp3.TypeDoubleBitBinary
	case kindCounter:
		return dnp3.TypeCounter
	case kindFrozenCounter:
		return dnp3.TypeFrozenCounter
	case kindAnalog:
		return dnp3.TypeAnalog
	case kindFrozenAnalog:
		return dnp3.TypeFrozenAnalog
	case kindBinaryOutput:
		return dnp3.TypeBinaryOutputStatus
	case kindAnalogOutput:
		return dnp3.TypeAnalogOutputStatus
	}
	return dnp3.TypeUnknown
}

// stateMask is the part of a flags octet that is the point's value rather
// than its quality, for the types that carry their state there.
func (k kind) stateMask() uint8 {
	switch k {
	case kindBinary, kindBinaryOutput:
		return 0x80
	case kindDoubleBit:
		return 0xC0
	}
	return 0
}

// snapshot is every point's value at one moment.
type snapshot struct {
	binary        []dnp3.Binary
	doubleBit     []dnp3.DoubleBitBinary
	counter       []dnp3.Counter
	frozenCounter []dnp3.FrozenCounter
	analog        []dnp3.Analog
	binaryOutput  []dnp3.BinaryOutputStatus
	analogOutput  []dnp3.AnalogOutputStatus
}

// value is a point's value and flags as a plain number, how the checks
// compare them: a binary's state as 0 or 1, a double-bit's as 0 to 3.
func (s *snapshot) value(k kind, i int) (v float64, flags uint8, ok bool) {
	b := func(x bool) float64 {
		if x {
			return 1
		}
		return 0
	}
	switch k {
	case kindBinary:
		if i < len(s.binary) {
			return b(s.binary[i].Value), uint8(s.binary[i].Flags), true
		}
	case kindDoubleBit:
		if i < len(s.doubleBit) {
			return float64(s.doubleBit[i].Value), uint8(s.doubleBit[i].Flags), true
		}
	case kindCounter:
		if i < len(s.counter) {
			return float64(s.counter[i].Value), uint8(s.counter[i].Flags), true
		}
	case kindFrozenCounter:
		if i < len(s.frozenCounter) {
			return float64(s.frozenCounter[i].Value), uint8(s.frozenCounter[i].Flags), true
		}
	case kindAnalog:
		if i < len(s.analog) {
			return s.analog[i].Value, uint8(s.analog[i].Flags), true
		}
	case kindBinaryOutput:
		if i < len(s.binaryOutput) {
			return b(s.binaryOutput[i].Value), uint8(s.binaryOutput[i].Flags), true
		}
	case kindAnalogOutput:
		if i < len(s.analogOutput) {
			return s.analogOutput[i].Value, uint8(s.analogOutput[i].Flags), true
		}
	}
	return 0, 0, false
}

// count is how many points of a kind the snapshot holds.
func (s *snapshot) count(k kind) int {
	switch k {
	case kindBinary:
		return len(s.binary)
	case kindDoubleBit:
		return len(s.doubleBit)
	case kindCounter:
		return len(s.counter)
	case kindFrozenCounter:
		return len(s.frozenCounter)
	case kindAnalog:
		return len(s.analog)
	case kindBinaryOutput:
		return len(s.binaryOutput)
	case kindAnalogOutput:
		return len(s.analogOutput)
	}
	return 0
}

// snapshot reads every point of a running session.
func (d *device) snapshot(sess *outstation.Session) *snapshot {
	d.mu.Lock()
	st, kept := d.running, d.values
	d.mu.Unlock()

	var s snapshot
	read := func(db *outstation.Database) {
		for i := range st.count(kindBinary) {
			v, _, _ := db.Binary(uint16(i))
			s.binary = append(s.binary, v)
		}
		for i := range st.count(kindDoubleBit) {
			v, _, _ := db.DoubleBit(uint16(i))
			s.doubleBit = append(s.doubleBit, v)
		}
		for i := range st.count(kindCounter) {
			v, _, _ := db.Counter(uint16(i))
			s.counter = append(s.counter, v)
		}
		for i := range st.count(kindFrozenCounter) {
			v, _, _ := db.FrozenCounter(uint16(i))
			s.frozenCounter = append(s.frozenCounter, v)
		}
		for i := range st.count(kindAnalog) {
			v, _, _ := db.Analog(uint16(i))
			s.analog = append(s.analog, v)
		}
		for i := range st.count(kindBinaryOutput) {
			v, _, _ := db.BinaryOutputStatus(uint16(i))
			s.binaryOutput = append(s.binaryOutput, v)
		}
		for i := range st.count(kindAnalogOutput) {
			v, _, _ := db.AnalogOutputStatus(uint16(i))
			s.analogOutput = append(s.analogOutput, v)
		}
	}
	// The getters take the database's own lock, so a read from here cannot
	// tear a value. What it can do is land between two halves of a batch,
	// which is what Update is for, but a stopping session may no longer be
	// applying batches, and the plugin never reads while one is in flight.
	read(sess.Database())
	if st.count(kindBinary) == 0 && kept != nil {
		// Uninstalled points keep their values for when they come back.
		s.binary = kept.binary
	}
	if st.count(kindDoubleBit) == 0 && kept != nil {
		s.doubleBit = kept.doubleBit
	}
	if st.count(kindCounter) == 0 && kept != nil {
		s.counter = kept.counter
	}
	if st.count(kindFrozenCounter) == 0 && kept != nil {
		s.frozenCounter = kept.frozenCounter
	}
	if st.count(kindAnalog) == 0 && kept != nil {
		s.analog = kept.analog
	}
	if st.count(kindBinaryOutput) == 0 && kept != nil {
		s.binaryOutput = kept.binaryOutput
	}
	if st.count(kindAnalogOutput) == 0 && kept != nil {
		s.analogOutput = kept.analogOutput
	}
	return &s
}

// current is a snapshot of the running outstation.
func (d *device) current() (*snapshot, error) {
	d.mu.Lock()
	sess := d.sess
	d.mu.Unlock()
	if sess == nil {
		return nil, fmt.Errorf("the outstation is not running")
	}
	return d.snapshot(sess), nil
}

// ---------- Event tracking ----------

// event is a change the device made that the outstation reports as an event.
type event struct {
	kind  kind
	index int
	value float64
	flags uint8
	at    time.Time
}

func (e event) String() string {
	return fmt.Sprintf("%s %d = %v (flags 0x%02X) at %s",
		e.kind, e.index, e.value, e.flags, e.at.UTC().Format("15:04:05.000"))
}

// tracker remembers the events generated since the procedure started, in
// batches: one per plugin call, which is what the checks compare with.
type tracker struct {
	batches [][]event
	// checked is how many of the latest batch's events a check of its first
	// event has accounted for, so a check of the rest knows where to start.
	checked int
	// counters are the counts recordCurrentCounterValues noted.
	counters *snapshot
	// timeEvent is generateTimeEvent's event.
	timeEvent *event
}

func (t *tracker) add(batch []event) {
	t.batches = append(t.batches, batch)
	t.checked = 0
}

func (t *tracker) latest() []event {
	if len(t.batches) == 0 {
		return nil
	}
	return t.batches[len(t.batches)-1]
}

func (t *tracker) all() []event {
	var out []event
	for _, b := range t.batches {
		out = append(out, b...)
	}
	return out
}

// change is one point to change, and what to change it to.
type change struct {
	kind  kind
	index int
	// next computes the point's new value from its present one.
	next func(v float64) float64
	// flags, if set, replaces the point's flags.
	flags *uint8
}

// apply makes the changes as one batch, recording the events they raise.
// Only points in an event class raise one: a change of an output's status,
// or of a point that is uninstalled, is a change and nothing more.
func (d *device) apply(changes []change) ([]event, error) {
	d.mu.Lock()
	clk := d.clock
	d.mu.Unlock()
	var batch []event
	err := d.update(func(db *outstation.Database, _ dnp3.Timestamp) {
		for _, c := range changes {
			ev, ok := applyOne(db, clk, c)
			if ok {
				batch = append(batch, ev)
			}
		}
	})
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	d.track.add(batch)
	d.mu.Unlock()
	return batch, nil
}

// applyOne makes one change and reports the event it raised, if any.
func applyOne(db *outstation.Database, clk *clock, c change) (event, bool) {
	at := clk.stamp()
	i := uint16(c.index)
	flagsOr := func(f dnp3.Flags) dnp3.Flags {
		if c.flags != nil {
			return dnp3.Flags(*c.flags)
		}
		return f
	}
	ev := event{kind: c.kind, index: c.index, at: at.Time}
	var changed bool
	switch c.kind {
	case kindBinary:
		v, _, ok := db.Binary(i)
		if !ok {
			return ev, false
		}
		n := dnp3.Binary{Value: c.next(boolNum(v.Value)) != 0, Flags: flagsOr(v.Flags), Time: at}
		changed = n.Value != v.Value || n.Flags != v.Flags
		db.UpdateBinary(i, n)
		ev.value, ev.flags = boolNum(n.Value), uint8(n.Flags)
	case kindDoubleBit:
		v, _, ok := db.DoubleBit(i)
		if !ok {
			return ev, false
		}
		n := dnp3.DoubleBitBinary{Value: dnp3.DoubleBit(c.next(float64(v.Value))), Flags: flagsOr(v.Flags), Time: at}
		changed = n.Value != v.Value || n.Flags != v.Flags
		db.UpdateDoubleBit(i, n)
		ev.value, ev.flags = float64(n.Value), uint8(n.Flags)
	case kindCounter:
		v, _, ok := db.Counter(i)
		if !ok {
			return ev, false
		}
		n := dnp3.Counter{Value: uint32(c.next(float64(v.Value))), Flags: flagsOr(v.Flags), Time: at}
		changed = n.Value != v.Value || n.Flags != v.Flags
		db.UpdateCounter(i, n)
		ev.value, ev.flags = float64(n.Value), uint8(n.Flags)
	case kindFrozenCounter:
		v, _, ok := db.FrozenCounter(i)
		if !ok {
			return ev, false
		}
		n := dnp3.FrozenCounter{Value: uint32(c.next(float64(v.Value))), Flags: flagsOr(v.Flags), Time: at}
		changed = n.Value != v.Value || n.Flags != v.Flags
		db.UpdateFrozenCounter(i, n)
		ev.value, ev.flags = float64(n.Value), uint8(n.Flags)
	case kindAnalog:
		v, _, ok := db.Analog(i)
		if !ok {
			return ev, false
		}
		n := dnp3.Analog{Value: c.next(v.Value), Flags: flagsOr(v.Flags), Time: at}
		changed = n.Value != v.Value || n.Flags != v.Flags
		db.UpdateAnalog(i, n)
		ev.value, ev.flags = n.Value, uint8(n.Flags)
	case kindBinaryOutput:
		v, _, ok := db.BinaryOutputStatus(i)
		if !ok {
			return ev, false
		}
		n := dnp3.BinaryOutputStatus{Value: c.next(boolNum(v.Value)) != 0, Flags: flagsOr(v.Flags), Time: at}
		db.UpdateBinaryOutputStatus(i, n)
		return ev, false
	case kindAnalogOutput:
		v, _, ok := db.AnalogOutputStatus(i)
		if !ok {
			return ev, false
		}
		n := dnp3.AnalogOutputStatus{Value: c.next(v.Value), Flags: flagsOr(v.Flags), Time: at}
		db.UpdateAnalogOutputStatus(i, n)
		return ev, false
	}
	_, inClass := eventClass[c.kind]
	return ev, changed && inClass
}

func boolNum(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// Value changes the generators use.
var (
	toggle = func(v float64) float64 { return 1 - v }
	// flipDoubleBit moves a double-bit input between its two determined
	// states, which is what a breaker does.
	flipDoubleBit = func(v float64) float64 {
		if dnp3.DoubleBit(v) == dnp3.DoubleBitDeterminedOn {
			return float64(dnp3.DoubleBitDeterminedOff)
		}
		return float64(dnp3.DoubleBitDeterminedOn)
	}
	countUp = func(v float64) float64 { return v + 1 }
	// stepAnalog moves an analog input by more than its deadband, staying in
	// the range of a 16-bit variation so every variation can carry it.
	stepAnalog = func(v float64) float64 {
		if v >= 30000 {
			return 1
		}
		return v + 7
	}
)

// nextFor is the generators' change for a kind of input.
func nextFor(k kind) (func(float64) float64, error) {
	switch k {
	case kindBinary:
		return toggle, nil
	case kindDoubleBit:
		return flipDoubleBit, nil
	case kindCounter, kindFrozenCounter:
		return countUp, nil
	case kindAnalog:
		return stepAnalog, nil
	}
	return nil, fmt.Errorf("cannot generate events for %s points", k)
}

// changesOf is n changes of a kind, spread over its points in index order and
// wrapping round, so a point changes again only once every other has.
func changesOf(k kind, n int) ([]change, error) {
	next, err := nextFor(k)
	if err != nil {
		return nil, err
	}
	out := make([]change, 0, n)
	for i := range n {
		out = append(out, change{kind: k, index: i % generated(k), next: next})
	}
	return out, nil
}

// classKinds are the input types in an event class, the first being what
// the class generators change.
func classKinds(class dnp3.Class) []kind {
	var out []kind
	for _, k := range []kind{kindBinary, kindDoubleBit, kindCounter, kindFrozenCounter, kindAnalog} {
		if eventClass[k]&class != 0 {
			out = append(out, k)
		}
	}
	return out
}
