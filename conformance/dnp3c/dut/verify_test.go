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
	"testing"
	"time"

	"github.com/dscsystems/go-dnp3"
)

// The checks are what the conformance run's verdicts rest on. One that passes
// what it should not turns a failing outstation into a passing report, so
// each is tested here against the reports it must reject.

var t0 = time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

func ev(k kind, i int, v float64, ms int) event {
	return event{kind: k, index: i, value: v, flags: uint8(dnp3.Online), at: t0.Add(time.Duration(ms) * time.Millisecond)}
}

func rep(k kind, i int, v float64, ms int) point {
	f, t := int(dnp3.Online), t0.Add(time.Duration(ms)*time.Millisecond).UnixMilli()
	return point{Kind: k, Index: i, Value: v, Flags: &f, Time: &t, Event: true}
}

func TestVerifyEvents(t *testing.T) {
	generated := []event{
		ev(kindBinary, 0, 1, 0),
		ev(kindBinary, 1, 1, 1),
		ev(kindBinary, 0, 0, 2),
	}
	in := []point{rep(kindBinary, 0, 1, 0), rep(kindBinary, 1, 1, 1), rep(kindBinary, 0, 0, 2)}

	if err := verifyEvents(in, generated); err != nil {
		t.Fatalf("the events as generated: %v", err)
	}

	cases := map[string][]point{
		"one missing":   in[:2],
		"one extra":     append(append([]point{}, in...), rep(kindBinary, 2, 1, 3)),
		"wrong value":   {in[0], in[1], rep(kindBinary, 0, 1, 2)},
		"out of order":  {in[1], in[0], in[2]},
		"same point":    {in[2], in[0], in[1]},
		"gap stretched": {in[0], in[1], rep(kindBinary, 0, 0, 50)},
		"static value":  {in[0], in[1], {Kind: kindBinary, Index: 0, Value: 0}},
	}
	for name, points := range cases {
		if err := verifyEvents(points, generated); err == nil {
			t.Errorf("%s: passed", name)
		}
	}
}

func TestVerifyEventsOrderAcrossAnalogPoints(t *testing.T) {
	// An analog's events need only be in order point by point: an
	// outstation may report point 1's before point 0's.
	generated := []event{ev(kindAnalog, 0, 7, 0), ev(kindAnalog, 1, 7, 1), ev(kindAnalog, 0, 14, 2)}
	in := []point{rep(kindAnalog, 1, 7, 1), rep(kindAnalog, 0, 7, 0), rep(kindAnalog, 0, 14, 2)}
	if err := verifyEvents(in, generated); err != nil {
		t.Fatalf("reordered across points: %v", err)
	}
	if err := verifyEvents([]point{in[0], in[2], in[1]}, generated); err == nil {
		t.Fatal("reordered within a point: passed")
	}
}

func TestVerifyEventsFlagsIgnoreState(t *testing.T) {
	// A binary's flags octet carries its state in bit 7; the device's flags
	// do not, and the two must still compare equal.
	generated := []event{ev(kindBinary, 0, 1, 0)}
	p := rep(kindBinary, 0, 1, 0)
	f := 0x81
	p.Flags = &f
	if err := verifyEvents([]point{p}, generated); err != nil {
		t.Fatalf("state bit in the flags: %v", err)
	}
	f = 0x83
	if err := verifyEvents([]point{p}, generated); err == nil {
		t.Fatal("a different flag: passed")
	}
}

func TestVerifyStatic(t *testing.T) {
	held := &snapshot{
		binary: []dnp3.Binary{{Value: true, Flags: dnp3.Online}, {Flags: dnp3.Online}},
		analog: []dnp3.Analog{{Value: 1.5, Flags: dnp3.Online}},
	}
	f := 0x81
	ok := []point{
		{Kind: kindBinary, Index: 0, Value: 1, Flags: &f},
		{Kind: kindBinary, Index: 1, Value: 0},
		{Kind: kindAnalog, Index: 0, Value: 1.5},
	}
	if err := verifyStatic(ok, held, []kind{kindBinary, kindAnalog}); err != nil {
		t.Fatalf("the values held: %v", err)
	}
	cases := map[string][]point{
		"wrong value": {ok[0], {Kind: kindBinary, Index: 1, Value: 1}, ok[2]},
		"missing":     {ok[0], ok[2]},
		"repeated":    {ok[0], ok[1], ok[1], ok[2]},
		"no point":    {ok[0], ok[1], ok[2], {Kind: kindBinary, Index: 7}},
		"an event":    {ok[0], ok[1], {Kind: kindAnalog, Index: 0, Value: 1.5, Event: true}},
	}
	for name, points := range cases {
		if err := verifyStatic(points, held, []kind{kindBinary, kindAnalog}); err == nil {
			t.Errorf("%s: passed", name)
		}
	}
}

func TestVerifyCurrentWithEarlier(t *testing.T) {
	all := []event{ev(kindCounter, 0, 1, 0), ev(kindCounter, 0, 2, 1)}
	latest := all[1:]
	held := &snapshot{counter: []dnp3.Counter{{Value: 2, Flags: dnp3.Online}}}

	for name, points := range map[string][]point{
		"current only":        {rep(kindCounter, 0, 2, 1)},
		"earlier and current": {rep(kindCounter, 0, 1, 0), rep(kindCounter, 0, 2, 1)},
	} {
		if err := verifyCurrentWithEarlier(points, latest, all, held); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, points := range map[string][]point{
		"none":          nil,
		"earlier last":  {rep(kindCounter, 0, 2, 1), rep(kindCounter, 0, 1, 0)},
		"never a value": {rep(kindCounter, 0, 9, 1), rep(kindCounter, 0, 2, 1)},
	} {
		if err := verifyCurrentWithEarlier(points, latest, all, held); err == nil {
			t.Errorf("%s: passed", name)
		}
	}
}

func TestChangesStayInTheirClass(t *testing.T) {
	// Binary inputs from binaryClassOthers up are in classes 2 and 3; a
	// generator that changed them would put binary events where device.ini
	// says there are none.
	changes, err := changesOf(kindBinary, 3*pointsPerType)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range changes {
		if pointClass(kindBinary, c.index) != dnp3.Class1 {
			t.Fatalf("binary input %d changed, in class %v", c.index, pointClass(kindBinary, c.index))
		}
	}
}

func TestStampsAreOrdered(t *testing.T) {
	var c clock
	last := c.stamp().Time
	for range 100 {
		next := c.stamp().Time
		if !next.After(last) {
			t.Fatalf("stamp %s after %s", next, last)
		}
		last = next
	}
}
