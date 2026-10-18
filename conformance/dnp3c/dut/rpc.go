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
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/outstation"
)

// request is one plugin call: IntegrationPlugin's method name, and its
// arguments by name.
type request struct {
	Method string          `json:"method"`
	Args   json.RawMessage `json:"args"`
}

// reply is what the plugin logs, what it returns, and why it throws.
type reply struct {
	Log    []string `json:"log"`
	Result any      `json:"result,omitempty"`
	Error  string   `json:"error,omitempty"`
}

func (r *reply) logf(format string, args ...any) {
	r.Log = append(r.Log, fmt.Sprintf(format, args...))
}

// args is the union of every method's arguments. A method reads the ones it
// takes; decoding them all into one struct keeps the plugin's side a list of
// names rather than a type per method.
type args struct {
	Value      bool     `json:"value"`
	Kind       kind     `json:"kind"`
	Index      int      `json:"index"`
	Indices    []int    `json:"indices"`
	Count      int      `json:"count"`
	Class      int      `json:"class"`
	Expect     string   `json:"expectation"`
	Points     []point  `json:"points"`
	Previous   []point  `json:"previous"`
	IsClass0   bool     `json:"isClass0"`
	Address    int      `json:"address"`
	TimeoutMs  int      `json:"timeoutMs"`
	Retries    *int     `json:"retries"`
	Size       int      `json:"size"`
	Expected   float64  `json:"expected"`
	Actual     float64  `json:"actual"`
	Analog     float64  `json:"analogValue"`
	Flag       int      `json:"flag"`
	DelayMs    int64    `json:"delayMs"`
	SentMs     int64    `json:"sentMs"`
	EventMs    int64    `json:"eventMs"`
	Attributes []attrIn `json:"attributes"`
}

// attrIn is a device attribute as dnp3c decoded it.
type attrIn struct {
	Variation int     `json:"variation"`
	Type      string  `json:"type"`
	Text      string  `json:"text"`
	Number    float64 `json:"number"`
}

type handler func(d *device, a args, r *reply) error

func newRPC(d *device) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var (
			in  request
			out reply
		)
		if err := json.NewDecoder(req.Body).Decode(&in); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var a args
		if len(in.Args) > 0 {
			if err := json.Unmarshal(in.Args, &a); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
		}
		h, ok := methods[in.Method]
		if !ok {
			out.Error = fmt.Sprintf("the device under test does not implement %s", in.Method)
		} else if err := h(d, a, &out); err != nil {
			out.Error = err.Error()
		}
		w.Header().Set("Content-Type", "application/json")
		// The plugin reads the reply or times out; a write that fails has
		// nobody left to tell.
		_ = json.NewEncoder(w).Encode(out)
	})
}

var methods map[string]handler

func init() {
	methods = map[string]handler{
		"startProcedure": func(d *device, _ args, r *reply) error {
			r.logf("restarted the outstation with its default configuration")
			return d.reset()
		},
		"endProcedure": func(*device, args, *reply) error { return nil },

		"cyclePower": func(d *device, _ args, r *reply) error {
			r.logf("restarted the outstation")
			return d.restart()
		},
		"setLinkLayerConfirm": configure(func(s *settings, a args) string {
			s.linkConfirms = a.Value
			return fmt.Sprintf("link-layer confirmation %s", onOff(a.Value))
		}),
		"setSelfAddressSupport": configure(func(s *settings, a args) string {
			s.selfAddress = a.Value
			return fmt.Sprintf("self-address support %s", onOff(a.Value))
		}),
		"enableUnsolicitedResponse": configure(func(s *settings, a args) string {
			s.unsolicited = a.Value
			return fmt.Sprintf("unsolicited responses %s", onOff(a.Value))
		}),
		"setUnsolicitedResponseTimeout": configure(func(s *settings, a args) string {
			s.unsolTimeout = time.Duration(a.TimeoutMs) * time.Millisecond
			return fmt.Sprintf("unsolicited confirm timeout %d ms", a.TimeoutMs)
		}),
		"setMaxUnsolicitedRetries": configure(func(s *settings, a args) string {
			if a.Retries == nil {
				s.unsolRetries = unlimitedRetries
				return "unsolicited retries unlimited"
			}
			s.unsolRetries = *a.Retries
			return fmt.Sprintf("unsolicited retries %d", *a.Retries)
		}),
		"setColdRestartSupport": func(d *device, a args, r *reply) error {
			d.mu.Lock()
			defer d.mu.Unlock()
			d.settings.noColdRestart = !a.Value
			d.running.noColdRestart = !a.Value
			if d.clock != nil {
				d.clock.setColdRestart(a.Value)
			}
			r.logf("cold restart support %s", onOff(a.Value))
			return nil
		},
		"setMasterAddress": configure(func(s *settings, a args) string {
			s.master = uint16(a.Address)
			return fmt.Sprintf("master address %d", a.Address)
		}),
		// The fragment size and the installed points are changed and applied
		// at once: the procedures go on to test them without a restart.
		"setMaxFragmentSize": reconfigure(func(s *settings, a args) string {
			s.fragmentSize = a.Size
			return fmt.Sprintf("maximum transmit fragment %d octets", a.Size)
		}),
		"uninstall": reconfigure(func(s *settings, a args) string {
			s.uninstalled[a.Kind] = true
			return fmt.Sprintf("every %s point uninstalled", a.Kind)
		}),
		"reinstallPoints": reconfigure(func(s *settings, _ args) string {
			s.uninstalled = map[kind]bool{}
			return "every point installed"
		}),

		// ---------- Supervisory control ----------
		"enableRemoteSupervisoryControl": func(d *device, _ args, r *reply) error {
			r.logf("every binary output under remote control")
			return d.setLocal(false, map[uint16]bool{})
		},
		"setGlobalRemoteSupervisoryControl": func(d *device, _ args, r *reply) error {
			r.logf("every binary output under local control")
			return d.setLocal(true, map[uint16]bool{})
		},
		"setIndividualRemoteSupervisoryControl": func(d *device, a args, r *reply) error {
			r.logf("binary output %d under local control", a.Index)
			return d.setLocal(false, map[uint16]bool{uint16(a.Index): true})
		},

		// ---------- Controls ----------
		"checkOperate": func(d *device, a args, r *reply) error {
			ok, ops := d.takeOperation(a.Kind, uint16(a.Index))
			r.logf("operations not yet checked: %s", describeOps(ops))
			if !ok {
				return fmt.Errorf("%s %d did not operate", a.Kind, a.Index)
			}
			return nil
		},
		"checkNotOperate": func(d *device, a args, r *reply) error {
			ops := d.takeOperations()
			r.logf("operations not yet checked: %s", describeOps(ops))
			for _, op := range ops {
				if op.kind == a.Kind {
					return fmt.Errorf("%s %d operated", op.kind, op.index)
				}
			}
			return nil
		},
		"checkAnalogOutputTolerance": func(_ *device, a args, r *reply) error {
			r.logf("sent %v, reported %v; the device keeps the value exactly", a.Expected, a.Actual)
			if !sameValue(kindAnalogOutput, a.Actual, a.Expected) {
				return fmt.Errorf("analog output reported as %v, sent as %v", a.Actual, a.Expected)
			}
			return nil
		},

		// ---------- Event generation ----------
		"generateInputChanges": func(d *device, a args, r *reply) error {
			// Every counter counts, so a freeze of them all finds none at
			// zero; of the other types, three points change.
			n := 3
			if a.Kind == kindCounter {
				n = pointsPerType
			}
			return generate(d, r, a.Kind, n)
		},
		"generateExactInputEvents": func(d *device, a args, r *reply) error {
			return generate(d, r, a.Kind, a.Count)
		},
		"generateClassEvents": func(d *device, a args, r *reply) error {
			return generateClasses(d, r, a.Class, 3)
		},
		"generateExactClassEvents": func(d *device, a args, r *reply) error {
			return generateClasses(d, r, a.Class, a.Count)
		},
		"generatePattern": func(d *device, a args, r *reply) error {
			var changes []change
			for i := range generated(a.Kind) {
				var v float64
				switch a.Kind {
				case kindBinary:
					v = float64(i % 2)
				case kindDoubleBit:
					v = float64(dnp3.DoubleBitDeterminedOff + dnp3.DoubleBit(i%2))
				case kindAnalog:
					v = float64(100 + 10*i)
				default:
					return fmt.Errorf("no pattern for %s points", a.Kind)
				}
				changes = append(changes, change{kind: a.Kind, index: i, next: func(float64) float64 { return v }})
			}
			return record(d, r, changes)
		},
		"generateEventOn": func(d *device, a args, r *reply) error {
			next, err := nextFor(a.Kind)
			if err != nil {
				return err
			}
			return record(d, r, []change{{kind: a.Kind, index: a.Index, next: next}})
		},
		"generateExactFrozenCounterEvents": func(d *device, a args, r *reply) error {
			return generate(d, r, kindFrozenCounter, a.Count)
		},
		"generateExactSmallAnalogInputEvents": func(d *device, a args, r *reply) error {
			// stepAnalog stays inside 16 bits.
			return generate(d, r, kindAnalog, a.Count)
		},
		"generateMultiFragmentResponse": func(d *device, _ args, r *reply) error {
			// Enough events of each class that a read of it does not fit in
			// one 2048-octet fragment: g2v2, g22v5 and g32v3 events take 9,
			// 13 and 13 octets with their indices.
			var changes []change
			for _, c := range []struct {
				k kind
				n int
			}{{kindBinary, 300}, {kindCounter, 200}, {kindAnalog, 200}} {
				cs, err := changesOf(c.k, c.n)
				if err != nil {
					return err
				}
				changes = append(changes, cs...)
			}
			return record(d, r, changes)
		},
		"generateUnsolicitedEvents": func(d *device, _ args, r *reply) error {
			r.Result = "performed"
			return generate(d, r, kindBinary, 1)
		},
		"generateNotEnoughUnsolicitedEvents": func(d *device, _ args, r *reply) error {
			return generate(d, r, kindBinary, 1)
		},
		"generateTimeEvent": func(d *device, _ args, r *reply) error {
			batch, err := d.apply([]change{{kind: kindBinary, index: 0, next: toggle}})
			if err != nil {
				return err
			}
			if len(batch) != 1 {
				return errors.New("the change raised no event")
			}
			d.mu.Lock()
			d.track.timeEvent = &batch[0]
			d.mu.Unlock()
			r.logf("generated %s", batch[0])
			return nil
		},
		"setAnalogInput": func(d *device, a args, r *reply) error {
			v := a.Analog
			return record(d, r, []change{{kind: kindAnalog, index: a.Index, next: func(float64) float64 { return v }}})
		},
		"setFlagsNormal": func(d *device, a args, r *reply) error {
			changes := make([]change, 0, pointsPerType)
			online := uint8(dnp3.Online)
			for i := range pointsPerType {
				changes = append(changes, change{kind: a.Kind, index: i, next: same, flags: &online})
			}
			r.logf("every %s point ONLINE, with no other flag", a.Kind)
			return record(d, r, changes)
		},
		"setFlagAbnormal": func(d *device, a args, r *reply) error {
			if len(a.Indices) == 0 {
				return fmt.Errorf("no %s point to set a flag abnormal on", a.Kind)
			}
			i := a.Indices[0]
			none := uint8(0)
			r.logf("%s %d with ONLINE clear", a.Kind, i)
			if err := record(d, r, []change{{kind: a.Kind, index: i, next: same, flags: &none}}); err != nil {
				return err
			}
			r.Result = map[string]int{"index": i, "flags": 0}
			return nil
		},
		"changeOutputFlag": func(d *device, a args, r *reply) error {
			held, err := d.current()
			if err != nil {
				return err
			}
			_, flags, ok := held.value(a.Kind, a.Index)
			if !ok {
				return fmt.Errorf("no %s %d", a.Kind, a.Index)
			}
			flags ^= 1 << a.Flag
			r.logf("%s %d flag bit %d toggled", a.Kind, a.Index, a.Flag)
			return record(d, r, []change{{kind: a.Kind, index: a.Index, next: same, flags: &flags}})
		},
		"changeBinaryOutputState": func(d *device, a args, r *reply) error {
			return record(d, r, []change{{kind: kindBinaryOutput, index: a.Index, next: toggle}})
		},
		"setAnalogOutputValue": func(d *device, a args, r *reply) error {
			v := a.Analog
			return record(d, r, []change{{kind: kindAnalogOutput, index: a.Index, next: func(float64) float64 { return v }}})
		},
		"recordCurrentCounterValues": func(d *device, _ args, r *reply) error {
			held, err := d.current()
			if err != nil {
				return err
			}
			d.mu.Lock()
			d.track.counters = held
			d.mu.Unlock()
			r.logf("counters %v, frozen %v", counterValues(held.counter), frozenValues(held.frozenCounter))
			return nil
		},

		// ---------- Checks of static values ----------
		"verifyAllStaticCurrentValues": func(d *device, a args, r *reply) error {
			held, err := d.current()
			if err != nil {
				return err
			}
			r.logf("checked %d points against the database", len(a.Points))
			var complete []kind
			if a.IsClass0 {
				complete = []kind{kindBinary, kindDoubleBit, kindCounter, kindFrozenCounter,
					kindAnalog, kindBinaryOutput, kindAnalogOutput}
			}
			return verifyStatic(a.Points, held, complete)
		},
		"verifyInputCurrentValues": func(d *device, a args, r *reply) error {
			held, err := d.current()
			if err != nil {
				return err
			}
			r.logf("checked %d points against the database", len(a.Points))
			return verifyStatic(a.Points, held, nil)
		},
		"verifyRecordedCounterValues": func(d *device, a args, r *reply) error {
			d.mu.Lock()
			rec := d.track.counters
			d.mu.Unlock()
			if rec == nil {
				return errors.New("no counter values were recorded")
			}
			r.logf("checked %d counters against the values recorded", len(a.Points))
			return verifyStatic(a.Points, rec, nil)
		},

		// ---------- Checks of events ----------
		"verifyInputEvents": func(d *device, a args, r *reply) error {
			return d.verifyExpectation(r, a.Points, []kind{a.Kind}, a.Expect)
		},
		"verifyFirstInputEvent": func(d *device, a args, r *reply) error {
			return d.verifyFirst(r, a.Points, []kind{a.Kind})
		},
		"verifyClassEvents": func(d *device, a args, r *reply) error {
			return d.verifyExpectation(r, a.Points, classKinds(classMask(a.Class)), a.Expect)
		},
		"verifyFirstClassEvent": func(d *device, a args, r *reply) error {
			return d.verifyFirst(r, a.Points, classKinds(classMask(a.Class)))
		},
		"verifyFrozenCounterEvents": func(d *device, a args, r *reply) error {
			return d.verifyExpectation(r, a.Points, []kind{kindFrozenCounter}, "LatestBatch")
		},
		"verifyPolledDataDoNotRepeatUnsolicitedData": func(_ *device, a args, r *reply) error {
			r.logf("unsolicited: %s", describe(a.Previous))
			r.logf("polled: %s", describe(a.Points))
			for _, u := range a.Previous {
				for _, p := range a.Points {
					if samePoint(u, p) {
						return fmt.Errorf("%s was reported unsolicited and then polled", p)
					}
				}
			}
			return nil
		},
		"verifyResponseRetainsEvents": func(_ *device, a args, r *reply) error {
			r.logf("earlier: %s", describe(a.Previous))
			r.logf("now: %s", describe(a.Points))
			for _, u := range a.Previous {
				found := false
				for _, p := range a.Points {
					found = found || samePoint(u, p)
				}
				if !found {
					return fmt.Errorf("%s was reported earlier and is missing now", u)
				}
			}
			return nil
		},
		"verifyTimestamp": func(d *device, a args, r *reply) error {
			d.mu.Lock()
			te := d.track.timeEvent
			d.mu.Unlock()
			if te == nil {
				return errors.New("no time event was generated")
			}
			r.logf("generated %s", te)
			for _, p := range a.Points {
				if p.Time != nil && sameEvent(p, *te) {
					diff := *p.Time - te.at.UnixMilli()
					r.logf("reported %s, %d ms from when it happened", p, diff)
					if diff > timestampTolerance || diff < -timestampTolerance {
						return fmt.Errorf("%s: the event happened at %s", p, te.at.UTC().Format("15:04:05.000"))
					}
					return nil
				}
			}
			return fmt.Errorf("the event is not among %s", describe(a.Points))
		},
		"verifyCommandEventTime": func(_ *device, a args, r *reply) error {
			diff := a.EventMs - a.SentMs
			r.logf("the event is %d ms after the command was sent", diff)
			if diff < -1000 || diff > 1000 {
				return fmt.Errorf("the command event is %d ms from when the command was sent", diff)
			}
			return nil
		},
		"verifyDelayMeasurementAccuracy": func(_ *device, a args, r *reply) error {
			// The outstation reports the time it held the request, which on
			// a host answering over loopback is a few milliseconds at most.
			r.logf("reported delay %d ms", a.DelayMs)
			if a.DelayMs < 0 || a.DelayMs > 100 {
				return fmt.Errorf("a delay of %d ms is not this device's processing time", a.DelayMs)
			}
			return nil
		},
		"verifyDeviceAttributes": func(d *device, a args, r *reply) error {
			d.mu.Lock()
			want := make(map[uint8]dnp3.Attribute, len(d.attributes))
			for k, v := range d.attributes {
				want[k] = v
			}
			d.mu.Unlock()
			return verifyAttributes(r, a.Attributes, want)
		},
	}
}

// configure changes a setting that takes effect at the next restart.
func configure(fn func(*settings, args) string) handler {
	return func(d *device, a args, r *reply) error {
		d.mu.Lock()
		what := fn(&d.settings, a)
		d.mu.Unlock()
		r.logf("configured %s, from the next restart", what)
		return nil
	}
}

// reconfigure changes a setting and restarts the outstation with it.
func reconfigure(fn func(*settings, args) string) handler {
	return func(d *device, a args, r *reply) error {
		d.mu.Lock()
		s := d.settings.clone()
		what := fn(&s, a)
		d.settings = s
		d.mu.Unlock()
		r.logf("configured %s, and restarted the outstation", what)
		return d.restart()
	}
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

var same = func(v float64) float64 { return v }

// generate makes n changes of a kind as one batch.
func generate(d *device, r *reply, k kind, n int) error {
	changes, err := changesOf(k, n)
	if err != nil {
		return err
	}
	return record(d, r, changes)
}

// record applies changes and logs the events they raised.
func record(d *device, r *reply, changes []change) error {
	batch, err := d.apply(changes)
	if err != nil {
		return err
	}
	if len(batch) <= 20 {
		r.logf("generated %d event(s): %s", len(batch), describe(batch))
	} else {
		r.logf("generated %d events", len(batch))
	}
	return nil
}

// classMask turns the plugin's class number into a mask, 0 being all three.
func classMask(c int) dnp3.Class {
	switch c {
	case 1:
		return dnp3.Class1
	case 2:
		return dnp3.Class2
	case 3:
		return dnp3.Class3
	}
	return dnp3.Class123
}

// generateClasses makes n events in a class, or n in each of classes 1, 2
// and 3 for class 0, as one batch. A class's events are of the first input
// type in it: binary inputs, counters and analog inputs.
func generateClasses(d *device, r *reply, class, n int) error {
	classes := []int{class}
	if class == 0 {
		classes = []int{1, 2, 3}
	}
	var changes []change
	for _, c := range classes {
		kinds := classKinds(classMask(c))
		if len(kinds) == 0 {
			return fmt.Errorf("no input is in class %d", c)
		}
		cs, err := changesOf(kinds[0], n)
		if err != nil {
			return err
		}
		changes = append(changes, cs...)
	}
	return record(d, r, changes)
}

// verifyExpectation checks events against the generated ones, as an
// EventExpectation says.
func (d *device) verifyExpectation(r *reply, points []point, kinds []kind, expectation string) error {
	d.mu.Lock()
	t := d.track
	d.mu.Unlock()
	r.logf("reported: %s", describe(points))

	latest := ofKinds(t.latest(), kinds...)
	all := ofKinds(t.all(), kinds...)
	switch expectation {
	case "LatestBatch":
		r.logf("expected the latest batch: %s", describe(latest))
		return verifyEvents(points, latest)
	case "RemainingLatestBatch":
		rest := latest[min(t.checked, len(latest)):]
		r.logf("expected the rest of the latest batch: %s", describe(rest))
		return verifyEvents(points, rest)
	case "AllGeneratedBatches":
		r.logf("expected every event generated: %s", describe(all))
		return verifyEvents(points, all)
	case "CurrentValuesWithOptionalEarlier":
		held, err := d.current()
		if err != nil {
			return err
		}
		r.logf("expected the present value of each point changed in: %s", describe(latest))
		return verifyCurrentWithEarlier(points, latest, all, held)
	case "ReturnedEventsOnly":
		return verifyGenerated(points, all)
	}
	return fmt.Errorf("the device under test cannot check %q", expectation)
}

// verifyFirst checks a response holding only the first event of the latest
// batch.
func (d *device) verifyFirst(r *reply, points []point, kinds []kind) error {
	d.mu.Lock()
	latest := ofKinds(d.track.latest(), kinds...)
	d.mu.Unlock()
	if len(latest) == 0 {
		return errors.New("no event of the type was generated")
	}
	r.logf("reported: %s; expected %s", describe(points), latest[0])
	if err := verifyEvents(points, latest[:1]); err != nil {
		return err
	}
	d.mu.Lock()
	d.track.checked = 1
	d.mu.Unlock()
	return nil
}

// samePoint is whether two reports are of the same event.
func samePoint(a, b point) bool {
	if a.Kind != b.Kind || a.Index != b.Index || !sameValue(a.Kind, a.Value, b.Value) {
		return false
	}
	if a.Time != nil && b.Time != nil {
		return *a.Time == *b.Time
	}
	return true
}

func describeOps(ops []operation) string {
	if len(ops) == 0 {
		return "none"
	}
	s := make([]string, len(ops))
	for i, op := range ops {
		s[i] = fmt.Sprintf("%s %d", op.kind, op.index)
	}
	return strings.Join(s, ", ")
}

func counterValues(cs []dnp3.Counter) []uint32 {
	out := make([]uint32, len(cs))
	for i, c := range cs {
		out[i] = c.Value
	}
	return out
}

func frozenValues(cs []dnp3.FrozenCounter) []uint32 {
	out := make([]uint32, len(cs))
	for i, c := range cs {
		out[i] = c.Value
	}
	return out
}

// verifyAttributes checks a read of the standard attribute set against what
// the device reports about itself.
func verifyAttributes(r *reply, got []attrIn, want map[uint8]dnp3.Attribute) error {
	var errs []error
	for _, a := range got {
		v := uint8(a.Variation)
		r.logf("g0v%d: %s %q %v", v, a.Type, a.Text, a.Number)
		if v == 254 || v == 255 {
			continue
		}
		w, ok := want[v]
		if !ok {
			errs = append(errs, fmt.Errorf("g0v%d is not an attribute the device has", v))
			continue
		}
		switch w.Type {
		case dnp3.AttrVisibleString:
			if a.Type != "vstr" || a.Text != w.Text {
				errs = append(errs, fmt.Errorf("g0v%d is %s %q; the device has %q", v, a.Type, a.Text, w.Text))
			}
		case dnp3.AttrUnsignedInt, dnp3.AttrSignedInt:
			if (a.Type != "uint" && a.Type != "int") || a.Number != float64(w.Number) {
				errs = append(errs, fmt.Errorf("g0v%d is %s %v; the device has %d", v, a.Type, a.Number, w.Number))
			}
		case dnp3.AttrFloat:
			if a.Type != "flt" || math.Abs(a.Number-w.Real) > 1e-6 {
				errs = append(errs, fmt.Errorf("g0v%d is %s %v; the device has %v", v, a.Type, a.Number, w.Real))
			}
		}
	}
	return errors.Join(errs...)
}

// The device's checks use the outstation's own types; this keeps the
// compiler honest that the handler satisfies the interface it is given as.
var _ outstation.CommandHandler = (*device)(nil)
