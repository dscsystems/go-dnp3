package conformance

import (
	"bytes"
	"testing"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/objects"
	"github.com/dscsystems/go-dnp3/outstation"
)

// The tests in this file each pin an answer the DNP Users Group's IED
// certification procedures expect and the outstation once got wrong, as
// dnp3c (conformance/dnp3c) found. Each names the procedure that found it.

// 8.2.1.2.3: a command to an output the device does not have is refused with
// NOT_SUPPORTED and PARAMETER_ERROR, and reaches no handler.
func TestCommandToAMissingOutputIsAParameterError(t *testing.T) {
	plant := &recordingHandler{}
	h := newHarness(t, outstation.Config{Database: smallDB()}, plant)

	resp := h.request(app.FuncDirectOperate, crobHeader(5, dnp3.ControlLatchOn))
	if got := commandStatus(t, resp); got != dnp3.CommandNotSupported {
		t.Errorf("status = %v, want NOT_SUPPORTED", got)
	}
	if !resp.Header.IIN.Has(app.IINParameterError) {
		t.Errorf("IIN = %v, want PARAMETER_ERROR", resp.Header.IIN)
	}
	if plant.operates != 0 {
		t.Error("the handler was asked to operate an output the device does not have")
	}
}

// 8.2.1.2.4: an operate after the select has timed out is refused with
// TIMEOUT, which tells the operator why, not NO_SELECT.
func TestOperateAfterTheSelectTimeoutIsTimeout(t *testing.T) {
	plant := &recordingHandler{}
	h := newHarness(t, outstation.Config{
		Database:      smallDB(),
		SelectTimeout: 100 * time.Millisecond,
	}, plant)

	crob := crobHeader(0, dnp3.ControlLatchOn)
	if got := commandStatus(t, h.request(app.FuncSelect, crob)); got != dnp3.CommandSuccess {
		t.Fatalf("select status = %v", got)
	}
	time.Sleep(250 * time.Millisecond)
	if got := commandStatus(t, h.request(app.FuncOperate, crob)); got != dnp3.CommandTimeout {
		t.Errorf("operate status = %v, want TIMEOUT", got)
	}
	if plant.operates != 0 {
		t.Error("an operate after the select timed out moved something")
	}
}

// 8.2.4: the controls past MaxControlsPerRequest are refused with
// TOO_MANY_OPS, and the ones within it are carried out.
func TestControlsPastTheCapAreTooManyOps(t *testing.T) {
	plant := &recordingHandler{}
	h := newHarness(t, outstation.Config{
		Database:              smallDB(),
		MaxControlsPerRequest: 1,
	}, plant)

	var data []byte
	for _, index := range []byte{0, 1} {
		data = append(data, index)
		data = objects.AppendCROB(data, dnp3.ControlRelayOutputBlock{Code: dnp3.ControlLatchOn, Count: 1})
	}
	resp := h.request(app.FuncDirectOperate, app.ObjectHeader{
		Group: 12, Variation: 1,
		Qualifier: app.MakeQualifier(app.PrefixIndex1, app.RangeCount8),
		Range:     app.Range{Spec: app.RangeCount8, Count: 2},
		Data:      data,
	})

	got := commandStatuses(t, resp)
	want := []dnp3.CommandStatus{dnp3.CommandSuccess, dnp3.CommandTooManyOps}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("statuses = %v, want %v", got, want)
	}
	if plant.operates != 1 {
		t.Errorf("%d outputs operated, want 1", plant.operates)
	}
}

// 8.5.2.2.1: the response that carries the last events of a class does not
// say the class still has events.
func TestClassBitClearsOnTheResponseThatEmptiesTheClass(t *testing.T) {
	h := newHarness(t, outstation.Config{Database: smallDB()}, nil)
	h.out.Update(func(db *outstation.Database) {
		db.UpdateBinary(0, dnp3.Binary{Value: true, Flags: dnp3.Online})
	})
	waitFor(t, func() bool { return h.out.Events().Total() > 0 })

	resp := h.request(app.FuncRead, app.ReadAllObjects(60, 2))
	if len(resp.Objects) == 0 {
		t.Fatal("the class 1 read returned no events")
	}
	if resp.Header.IIN.Has(app.IINClass1Events) {
		t.Errorf("IIN = %v: CLASS_1_EVENTS on the response carrying every class 1 event", resp.Header.IIN)
	}
}

// 8.5.2.2.4, 8.26.2: a confirmation and the next request arriving together,
// in one read from the connection, as a busy master's often do. The confirm
// is acted on first, so the events it acknowledges are gone by the time the
// request is answered.
func TestConfirmAndRequestInOneReadConfirmsTheEvents(t *testing.T) {
	h := newHarness(t, outstation.Config{Database: smallDB()}, nil)
	h.out.Update(func(db *outstation.Database) {
		db.UpdateBinary(0, dnp3.Binary{Value: true, Flags: dnp3.Online})
	})
	waitFor(t, func() bool { return h.out.Events().Total() > 0 })

	resp := h.request(app.FuncRead, app.ReadAllObjects(60, 2))
	if len(resp.Objects) == 0 || !resp.Header.Control.Con {
		t.Fatalf("CON = %v, %d objects: want the event asking for confirmation",
			resp.Header.Control.Con, len(resp.Objects))
	}

	var both bytes.Buffer
	if err := h.stack.Send(&both, app.AppendHeader(nil, app.Header{
		Control: app.Control{Fir: true, Fin: true, Seq: h.seq},
		Func:    app.FuncConfirm,
	})); err != nil {
		t.Fatal(err)
	}
	h.seq = (h.seq + 1) % app.SeqModulus
	if err := h.stack.Send(&both, app.BuildRequest(nil,
		app.Control{Fir: true, Fin: true, Seq: h.seq}, app.FuncRead, app.ReadAllObjects(60, 2))); err != nil {
		t.Fatal(err)
	}
	before := h.count()
	if _, err := h.conn.Write(both.Bytes()); err != nil {
		t.Fatal(err)
	}
	if resp := h.await(before); len(resp.Objects) != 0 {
		t.Errorf("%v: the confirmed event was reported again", groups(resp))
	}
}

// 8.5.5.2.2: a read of static data and events reports the events first, so
// the static values, which are newer, are what the master is left with.
func TestEventsComeBeforeStaticData(t *testing.T) {
	h := newHarness(t, outstation.Config{Database: smallDB()}, nil)
	h.out.Update(func(db *outstation.Database) {
		db.UpdateBinary(0, dnp3.Binary{Value: true, Flags: dnp3.Online})
	})
	waitFor(t, func() bool { return h.out.Events().Total() > 0 })

	resp := h.request(app.FuncRead, app.ReadAllObjects(60, 1), app.ReadAllObjects(60, 2))
	staticSeen := false
	for _, o := range resp.Objects {
		switch o.Group {
		case 2:
			if staticSeen {
				t.Fatalf("an event object follows static data: %v", groups(resp))
			}
		case 1, 10, 20, 30, 40:
			staticSeen = true
		}
	}
	if !staticSeen {
		t.Fatalf("no static data in the response: %v", groups(resp))
	}
}

// 8.6.5b: a broadcast to 0xFFFE has the response reporting it ask for
// confirmation, and the indication stays set until it is confirmed; one to
// 0xFFFD does not ask, and clears once reported.
func TestBroadcastIndicationIsConfirmedWhenAsked(t *testing.T) {
	h := newHarness(t, outstation.Config{Database: smallDB()}, nil)
	read := func() app.Fragment { return h.request(app.FuncRead, app.ReadAllObjects(60, 2)) }

	h.sendTo(0xFFFE, app.FuncImmedFreezeNR)
	time.Sleep(100 * time.Millisecond)
	for range 2 {
		// Unconfirmed, the second read reports the broadcast again.
		if resp := read(); !resp.Header.IIN.Has(app.IINBroadcast) || !resp.Header.Control.Con {
			t.Fatalf("IIN = %v, CON = %v: want BROADCAST, and a confirmation asked for",
				resp.Header.IIN, resp.Header.Control.Con)
		}
	}
	h.sendConfirm(h.seq)
	if resp := read(); resp.Header.IIN.Has(app.IINBroadcast) {
		t.Errorf("IIN = %v: BROADCAST still set once its response was confirmed", resp.Header.IIN)
	}

	h.sendTo(0xFFFD, app.FuncImmedFreezeNR)
	time.Sleep(100 * time.Millisecond)
	if resp := read(); !resp.Header.IIN.Has(app.IINBroadcast) || resp.Header.Control.Con {
		t.Errorf("IIN = %v, CON = %v: want BROADCAST, and no confirmation asked for",
			resp.Header.IIN, resp.Header.Control.Con)
	}
	if resp := read(); resp.Header.IIN.Has(app.IINBroadcast) {
		t.Errorf("IIN = %v: BROADCAST still set after it was reported", resp.Header.IIN)
	}
}

// 8.6.6.2.1: a read that replaces an unfinished multi-fragment response keeps
// the events it selected, and its confirmation removes them.
func TestReadReplacingAnUnfinishedResponseKeepsItsEvents(t *testing.T) {
	h := newHarness(t, outstation.Config{
		Database:      smallDB(),
		MaxTxFragment: 128,
	}, nil)
	const n = 60
	h.out.Update(func(db *outstation.Database) {
		for i := range n {
			db.UpdateBinary(0, dnp3.Binary{Value: i%2 == 0, Flags: dnp3.Online})
		}
	})
	waitFor(t, func() bool { return h.out.Events().Total() == n })

	if first := h.request(app.FuncRead, app.ReadAllObjects(60, 2)); first.Header.Control.Fin {
		t.Fatal("the class 1 read fitted one fragment; the test needs more events")
	}

	// Left unconfirmed, and replaced by a read of one event.
	resp := h.request(app.FuncRead, app.ObjectHeader{
		Group: 2, Variation: 0,
		Qualifier: app.MakeQualifier(app.PrefixNone, app.RangeCount8),
		Range:     app.Range{Spec: app.RangeCount8, Count: 1},
	})
	h.sendConfirm(resp.Header.Control.Seq)
	waitFor(t, func() bool { return h.out.Events().Total() == n-1 })
}

// 8.17.2c: a read that arrives while a response carrying events awaits
// confirmation is answered, not left behind the confirmation.
func TestReadWhileAwaitingConfirmationIsAnswered(t *testing.T) {
	h := newHarness(t, outstation.Config{
		Database:       smallDB(),
		ConfirmTimeout: 2 * time.Second,
	}, nil)
	h.out.Update(func(db *outstation.Database) {
		db.UpdateBinary(0, dnp3.Binary{Value: true, Flags: dnp3.Online})
	})
	waitFor(t, func() bool { return h.out.Events().Total() > 0 })

	if first := h.request(app.FuncRead, app.ReadAllObjects(60, 2)); !first.Header.Control.Con {
		t.Fatal("the response carrying events did not ask for confirmation")
	}
	before := h.count()
	h.send(app.FuncRead, app.ReadAllObjects(60, 2))
	deadline := time.Now().Add(time.Second)
	for h.count() == before {
		if time.Now().After(deadline) {
			t.Fatal("the second read was not answered while the first response awaited confirmation")
		}
		time.Sleep(2 * time.Millisecond)
	}
	if resp := h.await(before); resp.Header.Control.Seq != h.seq {
		t.Errorf("answered with sequence %d, want the second read's %d", resp.Header.Control.Seq, h.seq)
	}
}

// restartSupport is an application that can turn cold restart off.
type restartSupport struct {
	outstation.NopApplication
	cold bool
}

func (a *restartSupport) SupportsColdRestart() bool { return a.cold }
func (a *restartSupport) SupportsWarmRestart() bool { return true }

// 8.8.2: a device that does not support cold restart refuses it with
// NO_FUNC_CODE_SUPPORT, and does not restart.
func TestColdRestartRefusedWhenUnsupported(t *testing.T) {
	appl := &restartSupport{}
	h := newHarnessWithApp(t, outstation.Config{Database: smallDB()}, appl)
	h.request(app.FuncWrite, clearRestartHeader())

	resp := h.request(app.FuncColdRestart)
	if !resp.Header.IIN.Has(app.IINNoFuncCodeSupport) || resp.Header.IIN.Has(app.IINDeviceRestart) {
		t.Errorf("IIN = %v: want NO_FUNC_CODE_SUPPORT, and no restart", resp.Header.IIN)
	}
	if len(resp.Objects) != 0 {
		t.Errorf("an unsupported restart answered with a delay: %v", groups(resp))
	}

	appl.cold = true
	if resp := h.request(app.FuncColdRestart); !resp.Header.IIN.Has(app.IINDeviceRestart) {
		t.Errorf("IIN = %v: a supported cold restart did not restart", resp.Header.IIN)
	}
}

// 8.11.2a: an unsolicited retry reports the indications as they are now, not
// as they were on the first attempt.
func TestUnsolicitedRetryReportsCurrentIndications(t *testing.T) {
	h := newHarness(t, outstation.Config{
		Database: smallDB(),
		Unsolicited: outstation.UnsolicitedConfig{
			Enabled:        true,
			ConfirmTimeout: 300 * time.Millisecond,
			MaxRetries:     5,
		},
	}, nil)

	null := h.await(0)
	if !null.Header.IIN.Has(app.IINDeviceRestart) {
		t.Fatalf("the null unsolicited response does not report the restart: %v", null.Header.IIN)
	}
	if resp := h.request(app.FuncWrite, clearRestartHeader()); resp.Header.IIN.Has(app.IINDeviceRestart) {
		t.Fatalf("IIN = %v after clearing DEVICE_RESTART", resp.Header.IIN)
	}

	retry := awaitUnsolicited(t, h, h.count())
	if retry.Header.Control.Seq != null.Header.Control.Seq {
		t.Fatalf("retry sequence = %d, want %d", retry.Header.Control.Seq, null.Header.Control.Seq)
	}
	if retry.Header.IIN.Has(app.IINDeviceRestart) {
		t.Errorf("IIN = %v: the retry still reports the restart the master cleared", retry.Header.IIN)
	}
}

// 8.11.2.5.4: a read that arrives while an unsolicited response awaits
// confirmation is answered once the confirmation arrives.
func TestReadWaitsForTheUnsolicitedConfirmation(t *testing.T) {
	h := newHarness(t, outstation.Config{
		Database: smallDB(),
		Unsolicited: outstation.UnsolicitedConfig{
			Enabled:        true,
			ConfirmTimeout: 2 * time.Second,
		},
	}, nil)
	null := h.await(0)

	before := h.count()
	h.send(app.FuncRead, app.ReadAllObjects(60, 1))
	time.Sleep(300 * time.Millisecond)
	if h.count() != before {
		t.Fatal("the read was answered while the unsolicited response awaited confirmation")
	}

	sendUnsolConfirm(h, null.Header.Control.Seq)
	if resp := h.await(before); resp.Header.Control.Uns || resp.Header.Control.Seq != h.seq {
		t.Errorf("after the confirmation: %+v, want the answer to the read", resp.Header)
	}
}

// 8.11.2a: a read deferred behind the null unsolicited response is answered
// when its confirmation times out, and the null response is still retried.
func TestNullUnsolicitedIsRetriedAfterADeferredRead(t *testing.T) {
	h := newHarness(t, outstation.Config{
		Database: smallDB(),
		Unsolicited: outstation.UnsolicitedConfig{
			Enabled:        true,
			ConfirmTimeout: 300 * time.Millisecond,
			MaxRetries:     5,
		},
	}, nil)
	null := h.await(0)

	before := h.count()
	h.send(app.FuncRead, app.ReadAllObjects(60, 1))
	if resp := h.await(before); resp.Header.Control.Uns || resp.Header.Control.Seq != h.seq {
		t.Fatalf("%+v: want the answer to the read first", resp.Header)
	}
	if retry := awaitUnsolicited(t, h, before+1); retry.Header.Control.Seq != null.Header.Control.Seq {
		t.Errorf("unsolicited sequence %d, want the null response's %d retried",
			retry.Header.Control.Seq, null.Header.Control.Seq)
	}
}

// outputPoints is a command handler that says it has binary outputs 0 to 9,
// whatever the database holds.
type outputPoints struct{ recordingHandler }

func (*outputPoints) HasOutput(pt dnp3.PointType, index uint16) bool {
	return pt == dnp3.TypeBinaryOutputStatus && index < 10
}

// 8.2.1.2.10: a handler implementing OutputPoints decides which outputs
// exist, for a device whose database has no output status to go by.
func TestOutputPointsDecideWhichOutputsExist(t *testing.T) {
	plant := &outputPoints{}
	h := newHarness(t, outstation.Config{Database: outstation.DatabaseConfig{Binary: 1}}, plant)

	if resp := h.request(app.FuncDirectOperate, crobHeader(7, dnp3.ControlLatchOn)); commandStatus(t, resp) != dnp3.CommandSuccess {
		t.Errorf("output 7: %v, IIN %v; want SUCCESS", commandStatus(t, resp), resp.Header.IIN)
	}
	resp := h.request(app.FuncDirectOperate, crobHeader(12, dnp3.ControlLatchOn))
	if commandStatus(t, resp) != dnp3.CommandNotSupported || !resp.Header.IIN.Has(app.IINParameterError) {
		t.Errorf("output 12: %v, IIN %v; want NOT_SUPPORTED and PARAMETER_ERROR", commandStatus(t, resp), resp.Header.IIN)
	}
	if plant.operates != 1 {
		t.Errorf("%d operates reached the handler, want 1", plant.operates)
	}
}

// 8.13.2d, 8.23.2.1.3: a read naming points the device does not have says so,
// and still reports the ones it does.
func TestReadOfMissingPointsIsAnError(t *testing.T) {
	h := newHarness(t, outstation.Config{Database: smallDB()}, nil)

	resp := h.request(app.FuncRead, app.ReadRange(1, 2, 2, 5))
	if !resp.Header.IIN.Has(app.IINParameterError) {
		t.Errorf("IIN = %v: a range past the last binary input, want PARAMETER_ERROR", resp.Header.IIN)
	}
	if len(resp.Objects) != 1 || resp.Objects[0].Range.Start != 2 || resp.Objects[0].Range.Stop != 3 {
		t.Errorf("objects = %v, want binary inputs 2 to 3", resp.Objects)
	}

	resp = h.request(app.FuncRead, app.ReadAllObjects(21, 0))
	if !resp.Header.IIN.HasAny(app.IINObjectUnknown | app.IINParameterError) {
		t.Errorf("IIN = %v: a read of frozen counters on a device with none", resp.Header.IIN)
	}

	resp = h.request(app.FuncRead, app.ReadAllObjects(4, 0))
	if !resp.Header.IIN.HasAny(app.IINObjectUnknown | app.IINParameterError) {
		t.Errorf("IIN = %v: a read of double-bit events on a device with no double-bit inputs", resp.Header.IIN)
	}

	if resp := h.request(app.FuncRead, app.ReadAllObjects(1, 0)); resp.Header.IIN.HasAny(app.RequestErrorMask) {
		t.Errorf("IIN = %v: a read of every binary input is not an error", resp.Header.IIN)
	}
}

// 8.15.3, 8.14.2m: a clock set by the master is asked for again after the sync
// interval, and stops being trusted after the holdover.
func TestClockIsAskedForAgainAndExpires(t *testing.T) {
	h := newHarness(t, outstation.Config{
		Database: smallDB(),
		Clock:    outstation.ClockConfig{SyncInterval: 200 * time.Millisecond, Holdover: 200 * time.Millisecond},
	}, nil)
	h.request(app.FuncWrite, clearRestartHeader())

	if resp := h.request(app.FuncWrite, timeHeader(time.Now())); resp.Header.IIN.Has(app.IINNeedTime) {
		t.Fatalf("IIN = %v after the clock was set", resp.Header.IIN)
	}
	if cto := relativeEventCTO(t, h); cto != 1 {
		t.Errorf("CTO g51v%d while synchronized, want g51v1", cto)
	}

	time.Sleep(250 * time.Millisecond)
	if resp := h.request(app.FuncDelayMeasure); !resp.Header.IIN.Has(app.IINNeedTime) {
		t.Errorf("IIN = %v after the sync interval, want NEED_TIME", resp.Header.IIN)
	}
	if cto := relativeEventCTO(t, h); cto != 1 {
		t.Errorf("CTO g51v%d within the holdover, want g51v1", cto)
	}

	time.Sleep(250 * time.Millisecond)
	if cto := relativeEventCTO(t, h); cto != 2 {
		t.Errorf("CTO g51v%d after the holdover, want the unsynchronized g51v2", cto)
	}
}

// 8.24.2a: a read of the packed variation g1v1 reports the points, a bit
// each.
func TestPackedBinaryRead(t *testing.T) {
	h := newHarness(t, outstation.Config{Database: smallDB()}, nil)
	h.out.Update(func(db *outstation.Database) {
		for i, v := range []bool{true, false, true, true} {
			db.UpdateBinary(uint16(i), dnp3.Binary{Value: v, Flags: dnp3.Online})
		}
	})

	resp := h.request(app.FuncRead, app.ReadAllObjects(1, 1))
	if len(resp.Objects) != 1 {
		t.Fatalf("objects = %v, want one g1v1 header", groups(resp))
	}
	o := resp.Objects[0]
	if o.Group != 1 || o.Variation != 1 || o.Count() != 4 || len(o.Data) != 1 || o.Data[0] != 0x0D {
		t.Errorf("g%dv%d, %d points, data % x: want g1v1, 4 points, 0d", o.Group, o.Variation, o.Count(), o.Data)
	}
}

// 8.24.2d: a read of a variation without flags reports a point whose flags
// are abnormal with them.
func TestFlaglessReadReportsAnAbnormalPointWithFlags(t *testing.T) {
	h := newHarness(t, outstation.Config{Database: smallDB()}, nil)
	h.out.Update(func(db *outstation.Database) {
		db.UpdateAnalog(0, dnp3.Analog{Value: 1, Flags: dnp3.Online | dnp3.LocalForced})
		db.UpdateAnalog(1, dnp3.Analog{Value: 2, Flags: dnp3.Online})
		db.UpdateAnalog(2, dnp3.Analog{Value: 3, Flags: dnp3.Online})
	})

	resp := h.request(app.FuncRead, app.ReadAllObjects(30, 3))
	var got []string
	for _, o := range resp.Objects {
		got = append(got, gvRange(o))
	}
	want := []string{"g30v1 0-0", "g30v3 1-2"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("objects = %v, want %v", got, want)
	}
}

// ---------- Helpers ----------

// commandStatuses reads every status out of a command echo.
func commandStatuses(t *testing.T, resp app.Fragment) []dnp3.CommandStatus {
	t.Helper()
	var out []dnp3.CommandStatus
	for _, o := range resp.Objects {
		d, ok := objects.Lookup(objects.GV(o.Group, o.Variation))
		if !ok || (o.Group != 12 && o.Group != 41) {
			continue
		}
		size, _ := d.SizeOctets()
		step := o.Qualifier.IndexPrefix().Octets() + size
		for off := 0; off+step <= len(o.Data); off += step {
			out = append(out, dnp3.CommandStatus(o.Data[off+step-1]))
		}
	}
	return out
}

func groups(resp app.Fragment) []string {
	var out []string
	for _, o := range resp.Objects {
		out = append(out, gvRange(o))
	}
	return out
}

func gvRange(o app.ObjectHeader) string {
	s := "g" + itoa(int(o.Group)) + "v" + itoa(int(o.Variation))
	if o.Range.Spec.IsStartStop() {
		s += " " + itoa(int(o.Range.Start)) + "-" + itoa(int(o.Range.Stop))
	}
	return s
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

// clearRestartHeader is the write that clears DEVICE_RESTART.
func clearRestartHeader() app.ObjectHeader {
	return app.ObjectHeader{
		Group: 80, Variation: 1,
		Qualifier: app.MakeQualifier(app.PrefixNone, app.RangeStartStop8),
		Range:     app.Range{Spec: app.RangeStartStop8, Start: 7, Stop: 7, Count: 1},
		Data:      []byte{0x00},
	}
}

// timeHeader is a g50v1 clock write.
func timeHeader(at time.Time) app.ObjectHeader {
	return app.ObjectHeader{
		Group: 50, Variation: 1,
		Qualifier: app.MakeQualifier(app.PrefixNone, app.RangeCount8),
		Range:     app.Range{Spec: app.RangeCount8, Count: 1},
		Data:      objects.AppendTime48(nil, dnp3.Now(at)),
	}
}

// relativeEventCTO makes a binary input event, reads it as g2v3, and returns
// the variation of the common time of occurrence it is measured from.
func relativeEventCTO(t *testing.T, h *harness) uint8 {
	t.Helper()
	total := h.out.Events().Total()
	h.out.Update(func(db *outstation.Database) {
		v, _, _ := db.Binary(0)
		db.UpdateBinary(0, dnp3.Binary{Value: !v.Value, Flags: dnp3.Online, Time: dnp3.Now(time.Now())})
	})
	waitFor(t, func() bool { return h.out.Events().Total() > total })

	resp := h.request(app.FuncRead, app.ReadAllObjects(2, 3))
	h.sendConfirm(resp.Header.Control.Seq)
	for _, o := range resp.Objects {
		if o.Group == 51 {
			return o.Variation
		}
	}
	t.Fatalf("no common time of occurrence in the g2v3 response: %v", groups(resp))
	return 0
}

// awaitUnsolicited waits for the next unsolicited fragment from after on.
func awaitUnsolicited(t *testing.T, h *harness, after int) app.Fragment {
	t.Helper()
	for i := after; ; i++ {
		if f := h.await(i); f.Header.Control.Uns {
			return f
		}
	}
}
