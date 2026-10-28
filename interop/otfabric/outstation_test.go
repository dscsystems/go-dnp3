//go:build interop

package otfabric

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/channel"
)

// The reference masters against our outstation, built from the same fixture
// the reference outstations serve. This is the direction that finds the
// interesting problems: a master written by somebody else asks for what the
// standard allows, not for what we know how to answer.

// startDevice runs our outstation on the loopback and returns its port.
func startDevice(t *testing.T, fx *fixture) (*device, string) {
	t.Helper()

	d, err := newDevice(fx)
	if err != nil {
		t.Fatalf("building the outstation from the fixture: %v", err)
	}
	bind, _, _ := hostFor()
	ch := channel.TCPServer(bind + ":0")
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _ = d.session.Run(ctx, ch) }()
	t.Cleanup(func() {
		cancel()
		_ = ch.Close()
		<-done
	})

	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if addr := channel.TCPServerAddr(ch); addr != nil {
			return d, addr.String()[strings.LastIndex(addr.String(), ":")+1:]
		}
		if time.Now().After(deadline) {
			t.Fatal("the outstation is not listening")
		}
	}
}

// eachClient runs the test for every selected reference master, against a
// fresh outstation: the operations change it.
func eachClient(t *testing.T, fn func(t *testing.T, a *adapter, fx *fixture, d *device, port string)) {
	for _, a := range selected() {
		t.Run(a.name, func(t *testing.T) {
			a.load(t)
			if !a.caps.Roles.Client {
				t.Skipf("%s has no master", a.name)
			}
			// dnp3py's master is exercised against the baseline like the
			// others; only its outstation is limited to its own fixture.
			fx := baseline(t)
			d, port := startDevice(t, fx)
			fn(t, a, fx, d, port)
		})
	}
}

// groupOf folds the variations and the event groups of a measurement into the
// static group it belongs to.
func staticGroup(g int) int {
	switch g {
	case 2:
		return 1
	case 4:
		return 3
	case 22:
		return 20
	case 23:
		return 21
	case 32:
		return 30
	}
	return g
}

// numeric converts a result document value, which is a bool for a binary
// point and a number otherwise.
func numeric(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case string:
		return 0, false
	}
	return 0, false
}

// flagSet renders a flag list for comparison, order and ONLINE default aside.
func flagSet(names []string) map[string]bool {
	m := map[string]bool{}
	for _, n := range names {
		m[n] = true
	}
	return m
}

func sameFlags(got, want []string) bool {
	g, w := flagSet(got), flagSet(want)
	if len(g) != len(w) {
		return false
	}
	for n := range w {
		if !g[n] {
			return false
		}
	}
	return true
}

// expected lists the fixture's points as (static group, index) → point.
func expected(fx *fixture) map[[2]int]point {
	m := map[[2]int]point{}
	for group, points := range map[int][]point{
		1: fx.BinaryInputs, 3: fx.DoubleBitInputs, 10: fx.BinaryOutputs,
		20: fx.Counters, 21: fx.FrozenCounters, 30: fx.AnalogInputs,
		40: fx.AnalogOutputs, 110: fx.OctetStrings,
	} {
		for _, p := range points {
			m[[2]int{group, int(p.Index)}] = p
		}
	}
	return m
}

// flagNames lists a point's quality as a document does, ONLINE by default.
func (p point) flagNames() []string {
	if p.Flags == nil {
		return []string{"ONLINE"}
	}
	return p.Flags
}

// checkObjects compares what a reference master received with the fixture.
func checkObjects(t *testing.T, objects []object, fx *fixture) {
	t.Helper()
	want := expected(fx)
	seen := map[[2]int]bool{}
	for _, o := range objects {
		key := [2]int{staticGroup(o.Group), o.Index}
		p, ok := want[key]
		if !ok {
			t.Errorf("g%dv%d index %d: not in the fixture", o.Group, o.Variation, o.Index)
			continue
		}
		seen[key] = true
		if s, isString := o.Value.(string); isString {
			if !strings.EqualFold(s, p.Value.(string)) {
				t.Errorf("g%dv%d index %d: value %q, want %q", o.Group, o.Variation, o.Index, s, p.Value)
			}
			continue
		}
		got, _ := numeric(o.Value)
		exp, _ := numeric(p.Value)
		// A short variation cannot carry what the point holds; the fixture
		// picks variations that can.
		if got != exp && !(math.IsNaN(got) && math.IsNaN(exp)) {
			t.Errorf("g%dv%d index %d: value %v, want %v", o.Group, o.Variation, o.Index, o.Value, p.Value)
		}
		if !sameFlags(o.Flags, p.flagNames()) {
			t.Errorf("g%dv%d index %d: flags %v, want %v", o.Group, o.Variation, o.Index, o.Flags, p.flagNames())
		}
		if p.StaticVar != 0 && int(p.StaticVar) != o.Variation {
			t.Errorf("g%d index %d: variation %d, the fixture asks for %d", o.Group, o.Index, o.Variation, p.StaticVar)
		}
	}
	for key := range want {
		if !seen[key] {
			t.Errorf("g%d index %d: never received", key[0], key[1])
		}
	}
}

// TestOutstationIntegrityPoll has every reference master read the whole
// database and compares each point with the fixture.
func TestOutstationIntegrityPoll(t *testing.T) {
	eachClient(t, func(t *testing.T, a *adapter, fx *fixture, d *device, port string) {
		res := a.client(t, port, "integrity-poll")
		if !res.OK {
			t.Fatalf("integrity poll failed: %+v", res.Error)
		}
		checkObjects(t, res.Objects, fx)
		if fx.Outstation.NeedTime && !res.hasIIN("NEED_TIME") {
			t.Errorf("IIN %v lacks NEED_TIME, which the fixture sets", res.IIN)
		}
		for _, bad := range []string{"NO_FUNC_CODE_SUPPORT", "OBJECT_UNKNOWN", "PARAMETER_ERROR", "DEVICE_TROUBLE"} {
			if res.hasIIN(bad) {
				t.Errorf("IIN %v has %s", res.IIN, bad)
			}
		}
	})
}

// TestOutstationClassPolls has every reference master poll for events. The
// fixture raises none at load, so the answer is empty, and well-formed.
func TestOutstationClassPolls(t *testing.T) {
	eachClient(t, func(t *testing.T, a *adapter, fx *fixture, d *device, port string) {
		for _, flags := range [][]string{nil, {"--classes", "2"}, {"--classes", "1,3"}} {
			res := a.client(t, port, "event-poll", flags...)
			if !res.OK {
				t.Errorf("event poll %v: %+v", flags, res.Error)
			}
			if len(res.Objects) != 0 {
				t.Errorf("event poll %v returned %d objects from an outstation with no events", flags, len(res.Objects))
			}
		}
		// Class 0 as a plain read, the other way to ask for everything.
		res := a.client(t, port, "read", "--group", "60", "--variation", "1")
		if !res.OK {
			t.Fatalf("read of class 0: %+v", res.Error)
		}
		checkObjects(t, res.Objects, fx)
	})
}

// TestOutstationRangeReads has every reference master read by range and by
// "all objects", the qualifiers a master chooses between.
func TestOutstationRangeReads(t *testing.T) {
	eachClient(t, func(t *testing.T, a *adapter, fx *fixture, d *device, port string) {
		res := a.client(t, port, "read", "--group", "30", "--variation", "0", "--start", "0", "--stop", "1")
		if !res.OK {
			t.Fatalf("range read: %+v", res.Error)
		}
		if len(res.Objects) != 2 {
			t.Errorf("range read of analog inputs 0-1 returned %d objects", len(res.Objects))
		}
		for _, o := range res.Objects {
			if staticGroup(o.Group) != 30 || o.Index > 1 {
				t.Errorf("range read of analog inputs 0-1 returned g%d index %d", o.Group, o.Index)
			}
		}

		res = a.client(t, port, "read", "--group", "1", "--variation", "2", "--start", "0", "--stop", "2")
		if !res.OK || len(res.Objects) != 3 {
			t.Errorf("read of g1v2 0-2: ok=%v, %d objects, %+v", res.OK, len(res.Objects), res.Error)
		}

		if a.caps.has("readAllObjectsClient") {
			for group, count := range map[string]int{
				"1": len(fx.BinaryInputs), "20": len(fx.Counters), "30": len(fx.AnalogInputs),
				"40": len(fx.AnalogOutputs), "110": len(fx.OctetStrings),
			} {
				res := a.client(t, port, "read", "--group", group, "--variation", "0")
				if !res.OK {
					t.Errorf("read of all objects of group %s: %+v", group, res.Error)
				} else if len(res.Objects) != count {
					t.Errorf("read of all objects of group %s returned %d, want %d", group, len(res.Objects), count)
				}
			}
		}

		// A point that does not exist: an error, not silence.
		res = a.client(t, port, "read", "--group", "30", "--variation", "1", "--start", "100", "--stop", "101")
		if res.errCode() == "timeout" {
			t.Errorf("no answer to a read of points that do not exist")
		}
	})
}

// operateCase is one control and what it must do.
type operateCase struct {
	name    string
	flags   []string
	status  string // the status the master reports; empty for a refusal by IIN
	errCode string // empty on success
	// executed says whether the control must have reached the handler as an
	// operate, and what it changed.
	function string
	kind     string
	index    uint16
	code     uint8
	value    float64
}

// TestOutstationControls has every reference master issue controls: each
// relay code, each analog encoding, both sequences, and the refusals.
func TestOutstationControls(t *testing.T) {
	eachClient(t, func(t *testing.T, a *adapter, fx *fixture, d *device, port string) {
		direct := firstOutput(fx.BinaryOutputs, false)
		sbo := firstOutput(fx.BinaryOutputs, true)
		adirect := firstOutput(fx.AnalogOutputs, false)
		asbo := firstOutput(fx.AnalogOutputs, true)
		if direct == nil || sbo == nil || adirect == nil || asbo == nil {
			t.Skip("the fixture lacks outputs of both kinds")
		}
		ix := func(p *point) string { return strconv.Itoa(int(p.Index)) }

		cases := []operateCase{
			{name: "latch on", flags: []string{"--type", "g12v1", "--index", ix(direct), "--code", "LATCH_ON"},
				status: "SUCCESS", function: "direct-operate", kind: "crob", index: direct.Index, code: 0x03},
			{name: "latch off", flags: []string{"--type", "g12v1", "--index", ix(direct), "--code", "LATCH_OFF"},
				status: "SUCCESS", function: "direct-operate", kind: "crob", index: direct.Index, code: 0x04},
			{name: "close", flags: []string{"--type", "g12v1", "--index", ix(direct), "--code", "CLOSE"},
				status: "SUCCESS", function: "direct-operate", kind: "crob", index: direct.Index, code: 0x41},
			{name: "trip, select before operate", flags: []string{"--type", "g12v1", "--index", ix(direct), "--code", "TRIP", "--mode", "sbo"},
				status: "SUCCESS", function: "operate", kind: "crob", index: direct.Index, code: 0x81},
			{name: "latch on, output that requires a select", flags: []string{"--type", "g12v1", "--index", ix(sbo), "--code", "LATCH_ON", "--mode", "sbo"},
				status: "SUCCESS", function: "operate", kind: "crob", index: sbo.Index, code: 0x03},
			{name: "analog int32", flags: []string{"--type", "g41v1", "--index", ix(adirect), "--value", "-12345"},
				status: "SUCCESS", function: "direct-operate", kind: "analog", index: adirect.Index, value: -12345},
			{name: "analog int16, select before operate", flags: []string{"--type", "g41v2", "--index", ix(adirect), "--value", "42", "--mode", "sbo"},
				status: "SUCCESS", function: "operate", kind: "analog", index: adirect.Index, value: 42},
			{name: "analog float32, output that requires a select", flags: []string{"--type", "g41v3", "--index", ix(asbo), "--value", "49.5", "--mode", "sbo"},
				status: "SUCCESS", function: "operate", kind: "analog", index: asbo.Index, value: 49.5},
			{name: "analog float64", flags: []string{"--type", "g41v4", "--index", ix(adirect), "--value", "1000"},
				status: "SUCCESS", function: "direct-operate", kind: "analog", index: adirect.Index, value: 1000},
		}
		for _, c := range cases {
			before := len(d.commandsWith(c.function))
			res := a.client(t, port, "operate", c.flags...)
			if !res.OK || res.CommandStatus == nil || *res.CommandStatus != c.status {
				t.Errorf("%s: ok=%v status=%v error=%+v, want %s", c.name, res.OK, deref(res.CommandStatus), res.Error, c.status)
				continue
			}
			got := d.commandsWith(c.function)
			if len(got) != before+1 {
				t.Errorf("%s: the outstation saw %d %s requests, want %d", c.name, len(got), c.function, before+1)
				continue
			}
			rec := got[len(got)-1]
			if rec.kind != c.kind || rec.index != c.index || rec.code != c.code || rec.value != c.value || rec.status != dnp3.CommandSuccess {
				t.Errorf("%s: the outstation saw %+v, want %+v", c.name, rec, c)
			}
		}

		// What the controls did to the outstation: the last relay command to
		// the output that drives an input, and the last analog output.
		res := a.client(t, port, "integrity-poll")
		if !res.OK {
			t.Fatalf("integrity poll after the controls: %+v", res.Error)
		}
		for _, o := range res.Objects {
			switch {
			case staticGroup(o.Group) == 40 && o.Index == int(adirect.Index):
				if v, _ := numeric(o.Value); v != 1000 {
					t.Errorf("analog output %d holds %v after operating it to 1000", o.Index, o.Value)
				}
			case staticGroup(o.Group) == 10 && o.Index == int(sbo.Index):
				if v, _ := numeric(o.Value); v != 1 {
					t.Errorf("binary output %d holds %v after latching it on", o.Index, o.Value)
				}
			}
		}

		// Refusals: a value outside the output's range, a point that is not
		// there, and a direct operate on an output that requires a select.
		// Whether a master reports the status or the PARAMETER_ERROR that
		// goes with it depends on the master; either is a refusal.
		refusals := []struct {
			name  string
			flags []string
			// status is what a master that reports it must say; "" means
			// any refusal will do.
			status string
		}{
			{"analog out of range", []string{"--type", "g41v1", "--index", ix(adirect), "--value", "2000000"}, "OUT_OF_RANGE"},
			{"analog out of range, select before operate", []string{"--type", "g41v3", "--index", ix(asbo), "--value", "500", "--mode", "sbo"}, "OUT_OF_RANGE"},
			{"output that is not there", []string{"--type", "g41v2", "--index", "5", "--value", "1"}, ""},
			{"relay that is not there", []string{"--type", "g12v1", "--index", "9", "--code", "LATCH_ON"}, ""},
			{"direct operate on an output that requires a select", []string{"--type", "g12v1", "--index", ix(sbo), "--code", "LATCH_ON"}, "NOT_SUPPORTED"},
		}
		for _, r := range refusals {
			res := a.client(t, port, "operate", r.flags...)
			if res.OK {
				t.Errorf("%s: accepted", r.name)
				continue
			}
			switch res.errCode() {
			case "command-failed":
				if r.status != "" && deref(res.CommandStatus) != r.status {
					t.Errorf("%s: status %s, want %s", r.name, deref(res.CommandStatus), r.status)
				}
			case "rejected":
			default:
				t.Errorf("%s: %s (%s), want a refusal", r.name, res.errCode(), res.Error.Message)
			}
		}
		for _, c := range d.commandsWith("operate") {
			if c.status == dnp3.CommandSuccess {
				continue
			}
			t.Errorf("an operate that should not have executed reached the handler: %+v", c)
		}
	})
}

func deref(s *string) string {
	if s == nil {
		return "<none>"
	}
	return *s
}

// TestOutstationTimeSync has every reference master set the clock, by both
// procedures, and checks the outstation stops asking for it.
func TestOutstationTimeSync(t *testing.T) {
	eachClient(t, func(t *testing.T, a *adapter, fx *fixture, d *device, port string) {
		if !a.caps.has("timeSyncLan") {
			t.Skip("the master does not take the LAN procedure")
		}
		start := time.Now()
		res := a.client(t, port, "time-sync")
		if !res.OK {
			t.Fatalf("LAN time sync: %+v", res.Error)
		}
		writes := d.timeWrites()
		if len(writes) != 1 {
			t.Fatalf("the outstation saw %d clock writes, want 1", len(writes))
		}
		if skew := writes[0].Sub(start); skew < -time.Minute || skew > time.Minute {
			t.Errorf("the clock was set to %s, %s away from now", writes[0], skew)
		}

		if res := a.client(t, port, "integrity-poll"); !res.OK {
			t.Fatalf("integrity poll: %+v", res.Error)
		} else if res.hasIIN("NEED_TIME") {
			t.Errorf("NEED_TIME still set after the clock was written: %v", res.IIN)
		}

		// A master that synchronises only when asked to (opendnp3) has
		// nothing to do with an outstation that has just been set, so the
		// second procedure gets an outstation of its own.
		if a.caps.has("timeSyncNonLan") {
			d, port := startDevice(t, fx)
			if res := a.client(t, port, "time-sync", "--procedure", "non-lan"); !res.OK {
				t.Errorf("non-LAN time sync: %+v", res.Error)
			} else if n := len(d.timeWrites()); n != 1 {
				t.Errorf("the outstation saw %d clock writes after the non-LAN procedure, want 1", n)
			}
		}
	})
}

// TestOutstationRestart has every reference master restart the outstation and
// checks the delay it announced is the fixture's. A second restart while the
// first is under way is answered with the time left, not carried out, so each
// kind gets an outstation of its own.
func TestOutstationRestart(t *testing.T) {
	eachClient(t, func(t *testing.T, a *adapter, fx *fixture, d *device, port string) {
		if !a.caps.has("restart") {
			t.Skip("the master does not restart outstations")
		}
		for _, c := range []struct {
			kind  string
			mode  dnp3.RestartMode
			delay *int
		}{
			{"cold", dnp3.ColdRestart, fx.Outstation.ColdDelayMs},
			{"warm", dnp3.WarmRestart, fx.Outstation.WarmDelayMs},
		} {
			d, port := startDevice(t, fx)
			res := a.client(t, port, "restart", "--type", c.kind)
			if !res.OK {
				t.Errorf("%s restart: %+v", c.kind, res.Error)
				continue
			}
			if a.caps.has("restartDelayClient") && c.delay != nil {
				if res.RestartDelayMs == nil || int(*res.RestartDelayMs) != *c.delay {
					t.Errorf("%s restart: delay %v ms, want %d", c.kind, fmtPtr(res.RestartDelayMs), *c.delay)
				}
			}
			reqs := d.restartRequests()
			if len(reqs) == 0 || reqs[len(reqs)-1] != c.mode {
				t.Errorf("%s restart: the outstation saw %v", c.kind, reqs)
			}
		}
	})
}

func fmtPtr(f *float64) string {
	if f == nil {
		return "<none>"
	}
	return fmt.Sprint(*f)
}

// TestOutstationUnsolicitedAndFreeze covers the requests that change the
// outstation's behaviour rather than its data.
func TestOutstationUnsolicitedAndFreeze(t *testing.T) {
	eachClient(t, func(t *testing.T, a *adapter, fx *fixture, d *device, port string) {
		for _, op := range []string{"disable-unsolicited", "enable-unsolicited"} {
			if !a.caps.has("unsolicited") {
				break
			}
			if res := a.client(t, port, op); !res.OK {
				t.Errorf("%s: %+v", op, res.Error)
			}
		}
		if a.caps.has("freeze") {
			for _, flags := range [][]string{nil, {"--clear"}} {
				if res := a.client(t, port, "freeze", flags...); !res.OK {
					t.Errorf("freeze %v: %+v", flags, res.Error)
				}
			}
		}
		if res := a.client(t, port, "integrity-poll"); !res.OK {
			t.Errorf("integrity poll afterwards: %+v", res.Error)
		}
	})
}

// TestOutstationAddressing checks that frames for another address, or from
// another master, are not answered, as the contract has every outstation do.
func TestOutstationAddressing(t *testing.T) {
	eachClient(t, func(t *testing.T, a *adapter, fx *fixture, d *device, port string) {
		for _, flags := range [][]string{
			{"--remote-addr", "99", "--timeout-ms", "1000"},
			{"--local-addr", "7", "--timeout-ms", "1000"},
		} {
			res := a.client(t, port, "integrity-poll", flags...)
			if res.OK || res.errCode() != "timeout" {
				t.Errorf("integrity poll %v: ok=%v error=%+v, want a timeout", flags, res.OK, res.Error)
			}
		}
		if res := a.client(t, port, "integrity-poll"); !res.OK {
			t.Errorf("the outstation stopped answering its master after the strays: %+v", res.Error)
		}
	})
}
