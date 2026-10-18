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
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

// point is a measurement as dnp3c parsed it from the outstation's response.
type point struct {
	Kind  kind    `json:"kind"`
	Index int     `json:"index"`
	Value float64 `json:"value"`
	// Flags is the flags octet, absent for a variation without one.
	Flags *int `json:"flags"`
	// Time is milliseconds since 1970, absent for a variation without one.
	Time *int64 `json:"time"`
	// Event is whether the point came from an event variation.
	Event bool `json:"event"`
}

func (p point) String() string {
	s := fmt.Sprintf("%s %d = %v", p.Kind, p.Index, p.Value)
	if p.Flags != nil {
		s += fmt.Sprintf(" (flags 0x%02X)", *p.Flags)
	}
	if p.Time != nil {
		s += " at " + time.UnixMilli(*p.Time).UTC().Format("15:04:05.000")
	}
	return s
}

// timestampTolerance is how far apart two times may be and still be the
// same. The outstation carries milliseconds and the device stamps them, so
// anything wider than rounding would be a fault in one or the other.
const timestampTolerance = 1

// sameValue compares a value as reported with the value held. An analog
// reported in single precision is as close as single precision gets.
func sameValue(k kind, reported, held float64) bool {
	if reported == held {
		return true
	}
	if k == kindAnalog || k == kindAnalogOutput || k == kindFrozenAnalog {
		return math.Abs(reported-held) <= math.Max(1e-6, math.Abs(held)*1e-6)
	}
	return false
}

// matchesHeld checks a reported point against the outstation's database.
func matchesHeld(p point, held *snapshot) error {
	v, flags, ok := held.value(p.Kind, p.Index)
	if !ok {
		return fmt.Errorf("%s: the device has no such point", p)
	}
	if !sameValue(p.Kind, p.Value, v) {
		return fmt.Errorf("%s: the device holds %v", p, v)
	}
	if p.Flags != nil {
		mask := ^p.Kind.stateMask()
		if uint8(*p.Flags)&mask != flags&mask {
			return fmt.Errorf("%s: the device holds flags 0x%02X", p, flags&mask)
		}
	}
	return nil
}

// verifyStatic checks static values: each reported point has the value the
// device holds, and, where every point of the kinds should be there, none is
// missing or repeated.
func verifyStatic(points []point, held *snapshot, complete []kind) error {
	var errs []error
	seen := map[kind]map[int]int{}
	for _, p := range points {
		if p.Event {
			errs = append(errs, fmt.Errorf("%s: an event among static values", p))
			continue
		}
		if seen[p.Kind] == nil {
			seen[p.Kind] = map[int]int{}
		}
		seen[p.Kind][p.Index]++
		if err := matchesHeld(p, held); err != nil {
			errs = append(errs, err)
		}
	}
	for _, k := range complete {
		for i := range held.count(k) {
			switch n := seen[k][i]; {
			case n == 0:
				errs = append(errs, fmt.Errorf("%s %d is missing", k, i))
			case n > 1:
				errs = append(errs, fmt.Errorf("%s %d is reported %d times", k, i, n))
			}
		}
	}
	return errors.Join(errs...)
}

// sameEvent reports whether a reported event is a generated one.
func sameEvent(p point, e event) bool {
	if p.Kind != e.kind || p.Index != e.index || !sameValue(p.Kind, p.Value, e.value) {
		return false
	}
	if p.Flags != nil {
		mask := ^p.Kind.stateMask()
		if uint8(*p.Flags)&mask != e.flags&mask {
			return false
		}
	}
	return true
}

// ordering is how strictly the events' order is checked. The outstation has
// to report a point's events in the order they happened; across points, only
// a binary type's order is meaningful, as a sequence of events.
func inOrderAcrossPoints(k kind) bool {
	return k == kindBinary || k == kindDoubleBit
}

// verifyEvents checks that the reported events are exactly the expected ones:
// each expected event once, nothing else, in an order the standard allows,
// and with timestamps whose gaps are the gaps between when they happened.
func verifyEvents(points []point, expected []event) error {
	var errs []error
	used := make([]bool, len(expected))
	// position[i] is where in expected points[i] matched.
	position := make([]int, len(points))
	for i, p := range points {
		position[i] = -1
		if !p.Event {
			errs = append(errs, fmt.Errorf("%s: a static value among events", p))
			continue
		}
		// The earliest unused match: the order check below then sees the
		// order the events were matched in, not an arbitrary one.
		for j, e := range expected {
			if !used[j] && sameEvent(p, e) {
				used[j], position[i] = true, j
				break
			}
		}
		if position[i] < 0 {
			errs = append(errs, fmt.Errorf("%s: no such event was generated", p))
		}
	}
	for j, e := range expected {
		if !used[j] {
			errs = append(errs, fmt.Errorf("missing event: %s", e))
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}

	// Order: a later report of the same point, or of another point of a
	// binary type, must not be an earlier event.
	for i := range points {
		for j := i + 1; j < len(points); j++ {
			a, b := expected[position[i]], expected[position[j]]
			related := a.kind == b.kind && (a.index == b.index || inOrderAcrossPoints(a.kind))
			if related && position[j] < position[i] {
				errs = append(errs, fmt.Errorf("%s is reported after %s, which happened later", b, a))
			}
		}
	}
	if err := verifyGaps(points, expected, position); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// verifyGaps checks the events' timestamps: none is compared with an
// absolute clock, but each gap between two must be the gap between when
// they were generated.
func verifyGaps(points []point, expected []event, position []int) error {
	first := -1
	for i, p := range points {
		if p.Time == nil {
			continue
		}
		if first < 0 {
			first = i
			continue
		}
		want := expected[position[i]].at.Sub(expected[position[first]].at).Milliseconds()
		got := *p.Time - *points[first].Time
		if d := got - want; d > timestampTolerance || d < -timestampTolerance {
			return fmt.Errorf("%s is %d ms after %s; it was generated %d ms after it",
				p, got, points[first], want)
		}
	}
	return nil
}

// verifyCurrentWithEarlier checks the events of the points changed in the
// latest batch: each point's last event reports its present value, and any
// before it reports one of the values it was given since the procedure began.
func verifyCurrentWithEarlier(points []point, latest, all []event, held *snapshot) error {
	var errs []error
	changed := map[kind]map[int]bool{}
	for _, e := range latest {
		if changed[e.kind] == nil {
			changed[e.kind] = map[int]bool{}
		}
		changed[e.kind][e.index] = true
	}
	last := map[kind]map[int]point{}
	for _, p := range points {
		if !p.Event {
			errs = append(errs, fmt.Errorf("%s: a static value among events", p))
			continue
		}
		generated := false
		for _, e := range all {
			generated = generated || sameEvent(p, e)
		}
		if !generated {
			errs = append(errs, fmt.Errorf("%s: no such event was generated", p))
		}
		if last[p.Kind] == nil {
			last[p.Kind] = map[int]point{}
		}
		last[p.Kind][p.Index] = p
	}
	for k, indices := range changed {
		for i := range indices {
			p, ok := last[k][i]
			if !ok {
				errs = append(errs, fmt.Errorf("%s %d changed, and has no event", k, i))
				continue
			}
			if v, _, _ := held.value(k, i); !sameValue(k, p.Value, v) {
				errs = append(errs, fmt.Errorf("%s: the last event for the point; its present value is %v", p, v))
			}
		}
	}
	return errors.Join(errs...)
}

// verifyGenerated checks only that each reported event was generated.
func verifyGenerated(points []point, all []event) error {
	var errs []error
	for _, p := range points {
		found := false
		for _, e := range all {
			found = found || sameEvent(p, e)
		}
		if !found {
			errs = append(errs, fmt.Errorf("%s: no such event was generated", p))
		}
	}
	return errors.Join(errs...)
}

// ofKinds keeps the events of the kinds given.
func ofKinds(events []event, kinds ...kind) []event {
	var out []event
	for _, e := range events {
		for _, k := range kinds {
			if e.kind == k {
				out = append(out, e)
				break
			}
		}
	}
	return out
}

// describe lists points for a log line.
func describe[T fmt.Stringer](items []T) string {
	if len(items) == 0 {
		return "none"
	}
	s := make([]string, len(items))
	for i, it := range items {
		s[i] = it.String()
	}
	return strings.Join(s, "; ")
}
