// Package conformance drives an outstation with hand-built request fragments
// and checks the fragments it sends back.
//
// The integration tests prove a master and an outstation agree with each
// other, which is necessary but not sufficient: two implementations sharing a
// misreading of the standard agree perfectly. These tests instead assert what
// the standard says an outstation must do, working from octets a master never
// has to construct — an unknown function code, a request for a group the
// device does not have, a broadcast.
//
// They are modelled on the DNP Users Group's Level 2 procedures. They are not
// a substitute for certified conformance testing, and nothing here should be
// read as a conformance claim.
package conformance

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/channel"
	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/internal/stack"
	"github.com/dscsystems/go-dnp3/objects"
	"github.com/dscsystems/go-dnp3/outstation"
)

const (
	masterAddr     = 1
	outstationAddr = 10
)

// harness drives an outstation over a pipe using the raw stack, so a test can
// send exactly the octets it means to.
type harness struct {
	t     *testing.T
	out   *outstation.Session
	stack *stack.Stack
	conn  io.ReadWriteCloser

	mu        sync.Mutex
	fragments [][]byte

	seq uint8
}

func newHarness(t *testing.T, cfg outstation.Config, cmds outstation.CommandHandler) *harness {
	t.Helper()

	if cfg.LocalAddr == 0 {
		cfg.LocalAddr = outstationAddr
	}
	if cfg.RemoteAddr == 0 {
		cfg.RemoteAddr = masterAddr
	}
	if cfg.ConfirmTimeout == 0 {
		cfg.ConfirmTimeout = time.Second
	}

	mch, och := channel.Pipe()
	out := outstation.New(cfg, nil, cmds)

	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = out.Run(ctx, och) }()

	conn, err := mch.Connect(ctx)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	h := &harness{
		t:    t,
		out:  out,
		conn: conn,
		stack: stack.New(stack.Config{
			LocalAddr: masterAddr, RemoteAddr: outstationAddr, IsMaster: true,
		}),
	}

	// A reader goroutine collecting whatever the outstation sends, so a test
	// can assert on unsolicited traffic as well as on answers.
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, stack.ReadChunk)
		rxStack := stack.New(stack.Config{
			LocalAddr: masterAddr, RemoteAddr: outstationAddr, IsMaster: true,
		})
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				_ = rxStack.Receive(io.Discard, buf[:n], func(r stack.Received) {
					frag := make([]byte, len(r.Fragment))
					copy(frag, r.Fragment)
					h.mu.Lock()
					h.fragments = append(h.fragments, frag)
					h.mu.Unlock()
				})
			}
			if err != nil {
				return
			}
		}
	}()

	t.Cleanup(func() {
		cancel()
		_ = conn.Close()
		_ = mch.Close()
		_ = och.Close()
		wg.Wait()
	})
	return h
}

// send transmits a request built from the given function code and objects.
func (h *harness) send(fc app.FuncCode, objs ...app.ObjectHeader) {
	h.t.Helper()
	h.seq = (h.seq + 1) % app.SeqModulus
	frag := app.BuildRequest(nil,
		app.Control{Fir: true, Fin: true, Seq: h.seq}, fc, objs...)
	if err := h.stack.Send(h.conn, frag); err != nil {
		h.t.Fatalf("send: %v", err)
	}
}

// sendTo transmits a request to a specific link address, for broadcast tests.
func (h *harness) sendTo(dest uint16, fc app.FuncCode, objs ...app.ObjectHeader) {
	h.t.Helper()
	h.seq = (h.seq + 1) % app.SeqModulus
	frag := app.BuildRequest(nil,
		app.Control{Fir: true, Fin: true, Seq: h.seq}, fc, objs...)
	if err := h.stack.SendTo(h.conn, dest, frag); err != nil {
		h.t.Fatalf("send: %v", err)
	}
}

// sendConfirm acknowledges a response so the outstation may drop its events.
func (h *harness) sendConfirm(seq uint8) {
	h.t.Helper()
	frag := app.AppendHeader(nil, app.Header{
		Control: app.Control{Fir: true, Fin: true, Seq: seq},
		Func:    app.FuncConfirm,
	})
	if err := h.stack.Send(h.conn, frag); err != nil {
		h.t.Fatalf("confirm: %v", err)
	}
}

// await waits for the next response fragment beyond those already seen.
func (h *harness) await(after int) app.Fragment {
	h.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		n := len(h.fragments)
		var raw []byte
		if n > after {
			raw = h.fragments[after]
		}
		h.mu.Unlock()

		if raw != nil {
			frag, err := app.ParseResponse(nil, raw)
			if err != nil {
				h.t.Fatalf("the outstation sent a fragment its own parser rejects: %v", err)
			}
			return frag
		}
		time.Sleep(2 * time.Millisecond)
	}
	h.t.Fatal("no response within 3s")
	return app.Fragment{}
}

// count returns how many fragments have arrived.
func (h *harness) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.fragments)
}

// request sends and returns the single response it produces.
func (h *harness) request(fc app.FuncCode, objs ...app.ObjectHeader) app.Fragment {
	h.t.Helper()
	before := h.count()
	h.send(fc, objs...)
	return h.await(before)
}

func smallDB() outstation.DatabaseConfig {
	return outstation.DatabaseConfig{
		Binary: 4, Analog: 3, Counter: 2, BinaryOutputStatus: 2, AnalogOutputStatus: 2,
		DefaultClass: dnp3.Class1,
	}
}

// ---------- Procedures ----------

// A fresh outstation asserts DEVICE_RESTART until a master clears it.
func TestRestartIndicationAssertedUntilCleared(t *testing.T) {
	h := newHarness(t, outstation.Config{Database: smallDB()}, nil)

	resp := h.request(app.FuncRead, app.ReadAllObjects(60, 1))
	if !resp.Header.IIN.Has(app.IINDeviceRestart) {
		t.Errorf("a fresh outstation must assert DEVICE_RESTART; IIN = %v", resp.Header.IIN)
	}

	// Writing zero to internal indication index 7 clears it.
	resp = h.request(app.FuncWrite, app.ObjectHeader{
		Group: 80, Variation: 1,
		Qualifier: app.MakeQualifier(app.PrefixNone, app.RangeStartStop8),
		Range:     app.Range{Spec: app.RangeStartStop8, Start: 7, Stop: 7, Count: 1},
		Data:      []byte{0x00},
	})
	if resp.Header.IIN.Has(app.IINDeviceRestart) {
		t.Errorf("DEVICE_RESTART survived the write that clears it; IIN = %v", resp.Header.IIN)
	}
}

// A class 0 read returns every static point the device has.
func TestClassZeroReturnsAllStaticData(t *testing.T) {
	h := newHarness(t, outstation.Config{Database: smallDB()}, nil)
	h.out.Update(func(db *outstation.Database) {
		db.UpdateBinary(2, dnp3.Binary{Value: true, Flags: dnp3.Online})
		db.UpdateAnalog(1, dnp3.Analog{Value: 77, Flags: dnp3.Online})
	})

	resp := h.request(app.FuncRead, app.ReadAllObjects(60, 1))
	if resp.Header.Func != app.FuncResponse {
		t.Fatalf("func = %v, want RESPONSE", resp.Header.Func)
	}

	seen := map[uint8]uint32{}
	for _, o := range resp.Objects {
		seen[o.Group] += o.Count()
	}
	cfg := smallDB()
	for group, want := range map[uint8]uint32{
		1:  uint32(cfg.Binary),
		10: uint32(cfg.BinaryOutputStatus),
		20: uint32(cfg.Counter),
		30: uint32(cfg.Analog),
		40: uint32(cfg.AnalogOutputStatus),
	} {
		if seen[group] != want {
			t.Errorf("class 0 returned %d objects of group %d, want %d", seen[group], group, want)
		}
	}
}

// An unknown function code is refused with the matching indication, not
// ignored.
func TestUnknownFunctionCode(t *testing.T) {
	h := newHarness(t, outstation.Config{Database: smallDB()}, nil)

	resp := h.request(app.FuncCode(0x70))
	if !resp.Header.IIN.Has(app.IINNoFuncCodeSupport) {
		t.Errorf("IIN = %v, want NO_FUNC_CODE_SUPPORT", resp.Header.IIN)
	}
}

// A request for a group the device does not implement sets OBJECT_UNKNOWN.
func TestUnknownObjectGroup(t *testing.T) {
	h := newHarness(t, outstation.Config{Database: smallDB()}, nil)

	resp := h.request(app.FuncRead, app.ReadRange(88, 1, 0, 1))
	if !resp.Header.IIN.Has(app.IINObjectUnknown) {
		t.Errorf("IIN = %v, want OBJECT_UNKNOWN", resp.Header.IIN)
	}
}

// The response's sequence number echoes the request's, or a master cannot
// match answers to questions.
func TestResponseEchoesRequestSequence(t *testing.T) {
	h := newHarness(t, outstation.Config{Database: smallDB()}, nil)

	for range 4 {
		before := h.count()
		h.send(app.FuncRead, app.ReadAllObjects(60, 1))
		resp := h.await(before)
		if resp.Header.Control.Seq != h.seq {
			t.Errorf("response seq = %d, want %d", resp.Header.Control.Seq, h.seq)
		}
	}
}

// A broadcast request is executed but never answered — every outstation
// answering at once would collide — and the next response says so.
func TestBroadcastIsExecutedButNotAnswered(t *testing.T) {
	h := newHarness(t, outstation.Config{Database: smallDB()}, nil)

	before := h.count()
	h.sendTo(0xFFFF, app.FuncRead, app.ReadAllObjects(60, 1))
	time.Sleep(200 * time.Millisecond)
	if got := h.count(); got != before {
		t.Fatalf("the outstation answered a broadcast: %d new fragments", got-before)
	}

	resp := h.request(app.FuncRead, app.ReadAllObjects(60, 1))
	if !resp.Header.IIN.Has(app.IINBroadcast) {
		t.Errorf("IIN = %v, want the BROADCAST indication on the next response", resp.Header.IIN)
	}
}

// DELAY_MEASURE is answered with a group 52 variation 2 time delay.
func TestDelayMeasure(t *testing.T) {
	h := newHarness(t, outstation.Config{Database: smallDB()}, nil)

	resp := h.request(app.FuncDelayMeasure)
	if len(resp.Objects) != 1 {
		t.Fatalf("%d objects, want 1", len(resp.Objects))
	}
	o := resp.Objects[0]
	if o.Group != 52 || o.Variation != 2 {
		t.Errorf("object = g%dv%d, want g52v2", o.Group, o.Variation)
	}
	if len(o.Data) != 2 {
		t.Errorf("delay is %d octets, want 2", len(o.Data))
	}
}

// A cold restart is answered with the time the device expects to be away, and
// re-asserts the restart indication.
func TestColdRestart(t *testing.T) {
	h := newHarness(t, outstation.Config{Database: smallDB()}, nil)

	// Clear the initial indication so the re-assertion is unambiguous.
	h.request(app.FuncWrite, app.ObjectHeader{
		Group: 80, Variation: 1,
		Qualifier: app.MakeQualifier(app.PrefixNone, app.RangeStartStop8),
		Range:     app.Range{Spec: app.RangeStartStop8, Start: 7, Stop: 7, Count: 1},
		Data:      []byte{0x00},
	})

	resp := h.request(app.FuncColdRestart)
	if !resp.Header.IIN.Has(app.IINDeviceRestart) {
		t.Errorf("IIN = %v, want DEVICE_RESTART re-asserted", resp.Header.IIN)
	}
	if len(resp.Objects) != 1 || resp.Objects[0].Group != 52 {
		t.Errorf("a restart must answer with a group 52 time delay, got %v", resp.Objects)
	}
}

// Writing the time clears NEED_TIME.
func TestWriteTimeClearsNeedTime(t *testing.T) {
	h := newHarness(t, outstation.Config{Database: smallDB()}, nil)

	resp := h.request(app.FuncRead, app.ReadAllObjects(60, 1))
	if !resp.Header.IIN.Has(app.IINNeedTime) {
		t.Fatalf("a fresh outstation should ask for the time; IIN = %v", resp.Header.IIN)
	}

	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	ms := dnp3.TimeToDNP3(now)
	resp = h.request(app.FuncWrite, app.ObjectHeader{
		Group: 50, Variation: 1,
		Qualifier: app.MakeQualifier(app.PrefixNone, app.RangeCount8),
		Range:     app.Range{Spec: app.RangeCount8, Count: 1},
		Data: []byte{
			byte(ms), byte(ms >> 8), byte(ms >> 16),
			byte(ms >> 24), byte(ms >> 32), byte(ms >> 40),
		},
	})
	if resp.Header.IIN.Has(app.IINNeedTime) {
		t.Errorf("NEED_TIME survived a successful clock write; IIN = %v", resp.Header.IIN)
	}
}

// The recorded-time procedure is the LAN clock synchronisation the standard describes.
// An outstation that refuses it leaves that master unable to set the clock at all.
//
// The master sends RECORD_CURRENT_TIME, the outstation notes when it arrived,
// and the master then *writes* group 50 variation 3 with what its own clock
// read at that moment. The outstation adds however long it has taken since,
// which is what makes this better than a plain clock write: the transit delay
// is measured rather than assumed.
func TestRecordCurrentTimeProcedure(t *testing.T) {
	h := newHarness(t, outstation.Config{Database: smallDB()}, nil)

	resp := h.request(app.FuncRead, app.ReadAllObjects(60, 1))
	if !resp.Header.IIN.Has(app.IINNeedTime) {
		t.Fatalf("a fresh outstation should be asking for the time; IIN = %v", resp.Header.IIN)
	}

	// Step one: the outstation records when this arrived.
	resp = h.request(app.FuncRecordCurrentTime)
	if resp.Header.IIN.HasAny(app.IINNoFuncCodeSupport) {
		t.Fatalf("RECORD_CURRENT_TIME was refused; IIN = %v", resp.Header.IIN)
	}

	// Step two: the master writes what its clock read at that moment.
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	ms := dnp3.TimeToDNP3(now)
	resp = h.request(app.FuncWrite, app.ObjectHeader{
		Group: 50, Variation: 3,
		Qualifier: app.MakeQualifier(app.PrefixNone, app.RangeCount8),
		Range:     app.Range{Spec: app.RangeCount8, Count: 1},
		Data: []byte{
			byte(ms), byte(ms >> 8), byte(ms >> 16),
			byte(ms >> 24), byte(ms >> 32), byte(ms >> 40),
		},
	})
	if resp.Header.IIN.HasError() {
		t.Fatalf("the recorded-time write was refused; IIN = %v", resp.Header.IIN)
	}
	if resp.Header.IIN.Has(app.IINNeedTime) {
		t.Error("NEED_TIME survived a successful recorded-time synchronisation")
	}
}

// A group 50 variation 3 write with no RECORD_CURRENT_TIME before it has no
// reference to correct against, so it must be refused rather than silently
// treated as a plain clock write.
func TestRecordedTimeWriteWithoutRecord(t *testing.T) {
	h := newHarness(t, outstation.Config{Database: smallDB()}, nil)

	ms := dnp3.TimeToDNP3(time.Now())
	resp := h.request(app.FuncWrite, app.ObjectHeader{
		Group: 50, Variation: 3,
		Qualifier: app.MakeQualifier(app.PrefixNone, app.RangeCount8),
		Range:     app.Range{Spec: app.RangeCount8, Count: 1},
		Data: []byte{
			byte(ms), byte(ms >> 8), byte(ms >> 16),
			byte(ms >> 24), byte(ms >> 32), byte(ms >> 40),
		},
	})
	if !resp.Header.IIN.Has(app.IINParameterError) {
		t.Errorf("IIN = %v, want PARAMETER_ERROR", resp.Header.IIN)
	}
	if !resp.Header.IIN.Has(app.IINNeedTime) {
		t.Error("the clock should not have been set")
	}
}

// Events are reported on a class poll and only dropped once confirmed.
func TestEventsRequireConfirmation(t *testing.T) {
	h := newHarness(t, outstation.Config{Database: smallDB()}, nil)

	h.out.Update(func(db *outstation.Database) {
		db.UpdateBinary(1, dnp3.Binary{Value: true, Flags: dnp3.Online})
	})
	waitFor(t, func() bool { return h.out.Events().Total() >= 1 })

	resp := h.request(app.FuncRead, app.ReadAllObjects(60, 2))
	if len(resp.Objects) == 0 {
		t.Fatal("the class 1 poll returned no events")
	}
	if !resp.Header.Control.Con {
		t.Error("a response carrying events must ask to be confirmed")
	}

	// Until the confirm arrives, the events stay put.
	if h.out.Events().Total() == 0 {
		t.Error("events were dropped before being confirmed")
	}

	h.sendConfirm(resp.Header.Control.Seq)
	waitFor(t, func() bool { return h.out.Events().Total() == 0 })
}

// An event buffer that overflows reports it, which is the only way a master
// learns its record has a hole in it.
func TestEventBufferOverflowIndication(t *testing.T) {
	h := newHarness(t, outstation.Config{
		Database: smallDB(),
		Events:   outstation.EventBufferConfig{MaxEvents: 2},
	}, nil)

	h.out.Update(func(db *outstation.Database) {
		for i := range 20 {
			db.UpdateBinary(0, dnp3.Binary{Value: i%2 == 0, Flags: dnp3.Online})
		}
	})
	waitFor(t, func() bool { return h.out.Events().Overflowed() })

	resp := h.request(app.FuncRead, app.ReadAllObjects(60, 2))
	if !resp.Header.IIN.Has(app.IINEventBufferOverflow) {
		t.Errorf("IIN = %v, want EVENT_BUFFER_OVERFLOW", resp.Header.IIN)
	}
}

// A select reserves and an operate executes; the outstation echoes each
// command with its status.
func TestSelectBeforeOperate(t *testing.T) {
	plant := &recordingHandler{}
	h := newHarness(t, outstation.Config{
		Database:      smallDB(),
		SelectTimeout: 5 * time.Second,
	}, plant)

	crob := crobHeader(3, dnp3.ControlLatchOn)

	resp := h.request(app.FuncSelect, crob)
	if got := commandStatus(t, resp); got != dnp3.CommandSuccess {
		t.Fatalf("select status = %v", got)
	}
	if plant.operates != 0 {
		t.Fatal("the select operated something")
	}

	resp = h.request(app.FuncOperate, crob)
	if got := commandStatus(t, resp); got != dnp3.CommandSuccess {
		t.Fatalf("operate status = %v", got)
	}
	if plant.operates != 1 {
		t.Errorf("%d operates, want 1", plant.operates)
	}
}

// An operate with no live selection is refused with NO_SELECT.
func TestOperateWithoutSelect(t *testing.T) {
	plant := &recordingHandler{}
	h := newHarness(t, outstation.Config{Database: smallDB()}, plant)

	resp := h.request(app.FuncOperate, crobHeader(1, dnp3.ControlLatchOn))
	if got := commandStatus(t, resp); got != dnp3.CommandNoSelect {
		t.Errorf("status = %v, want NO_SELECT", got)
	}
	if plant.operates != 0 {
		t.Error("an unselected operate moved something")
	}
}

// An operate naming different objects from the select is refused. The whole
// point of the two-pass sequence is that the operator confirms exactly what
// was proposed.
func TestOperateMustMatchSelect(t *testing.T) {
	plant := &recordingHandler{}
	h := newHarness(t, outstation.Config{
		Database:      smallDB(),
		SelectTimeout: 5 * time.Second,
	}, plant)

	h.request(app.FuncSelect, crobHeader(0, dnp3.ControlLatchOn))

	// A different point, and then a different operation on the right point.
	resp := h.request(app.FuncOperate, crobHeader(1, dnp3.ControlLatchOn))
	if got := commandStatus(t, resp); got == dnp3.CommandSuccess {
		t.Error("an operate on a different point than the select was accepted")
	}
	if plant.operates != 0 {
		t.Error("a mismatched operate moved something")
	}
}

// Enable and disable unsolicited are accepted for the event classes.
func TestUnsolicitedControlAccepted(t *testing.T) {
	h := newHarness(t, outstation.Config{Database: smallDB()}, nil)

	for _, fc := range []app.FuncCode{app.FuncEnableUnsolicited, app.FuncDisableUnsolicited} {
		resp := h.request(fc,
			app.ReadAllObjects(60, 2), app.ReadAllObjects(60, 3), app.ReadAllObjects(60, 4))
		if resp.Header.IIN.HasAny(app.IINNoFuncCodeSupport | app.IINObjectUnknown) {
			t.Errorf("%v was refused; IIN = %v", fc, resp.Header.IIN)
		}
	}
}

// A range read returns exactly the points asked for.
func TestRangeReadReturnsRequestedPoints(t *testing.T) {
	h := newHarness(t, outstation.Config{Database: smallDB()}, nil)

	resp := h.request(app.FuncRead, app.ReadRange(30, 1, 1, 2))
	if len(resp.Objects) != 1 {
		t.Fatalf("%d object headers, want 1", len(resp.Objects))
	}
	o := resp.Objects[0]
	if o.Count() != 2 {
		t.Errorf("returned %d points, want 2", o.Count())
	}
	if o.Range.Start != 1 || o.Range.Stop != 2 {
		t.Errorf("range = [%d..%d], want [1..2]", o.Range.Start, o.Range.Stop)
	}
}

// A read of a range beyond the database is clamped rather than refused, and
// returns the points that do exist.
func TestRangeBeyondDatabaseIsClamped(t *testing.T) {
	h := newHarness(t, outstation.Config{Database: smallDB()}, nil)

	resp := h.request(app.FuncRead, app.ReadRange(30, 1, 0, 200))
	if len(resp.Objects) != 1 {
		t.Fatalf("%d object headers, want 1", len(resp.Objects))
	}
	if got := resp.Objects[0].Count(); got != uint32(smallDB().Analog) {
		t.Errorf("returned %d points, want the %d that exist", got, smallDB().Analog)
	}
}

// Deadbands written by a master take effect.
func TestWriteAnalogDeadband(t *testing.T) {
	h := newHarness(t, outstation.Config{Database: smallDB()}, nil)

	// 100.0 as a single-precision float.
	resp := h.request(app.FuncWrite, app.ObjectHeader{
		Group: 34, Variation: 3,
		Qualifier: app.MakeQualifier(app.PrefixIndex1, app.RangeCount8),
		Range:     app.Range{Spec: app.RangeCount8, Count: 1},
		Data:      []byte{0, 0x00, 0x00, 0xC8, 0x42},
	})
	if resp.Header.IIN.HasError() {
		t.Fatalf("the deadband write was refused; IIN = %v", resp.Header.IIN)
	}

	_, cfg, ok := h.out.Database().Analog(0)
	if !ok {
		t.Fatal("analog 0 is missing")
	}
	if cfg.Deadband != 100 {
		t.Errorf("deadband = %v, want 100", cfg.Deadband)
	}

	// And it is honoured: a move inside it generates no event.
	h.out.Update(func(db *outstation.Database) {
		db.UpdateAnalog(0, dnp3.Analog{Value: 0, Flags: dnp3.Online})
	})
	waitFor(t, func() bool { return h.out.Events().Total() >= 1 })
	before := h.out.Events().Total()

	h.out.Update(func(db *outstation.Database) {
		db.UpdateAnalog(0, dnp3.Analog{Value: 50, Flags: dnp3.Online})
	})
	time.Sleep(150 * time.Millisecond)
	if got := h.out.Events().Total(); got != before {
		t.Errorf("a move of 50 inside a deadband of 100 produced an event")
	}
}

// ---------- Helpers ----------

func crobHeader(index uint8, code dnp3.ControlCode) app.ObjectHeader {
	data := []byte{index}
	data = objects.AppendCROB(data, dnp3.ControlRelayOutputBlock{Code: code, Count: 1})
	return app.ObjectHeader{
		Group: 12, Variation: 1,
		Qualifier: app.MakeQualifier(app.PrefixIndex1, app.RangeCount8),
		Range:     app.Range{Spec: app.RangeCount8, Count: 1},
		Data:      data,
	}
}

// commandStatus reads the status out of the outstation's command echo.
func commandStatus(t *testing.T, resp app.Fragment) dnp3.CommandStatus {
	t.Helper()
	for _, o := range resp.Objects {
		if o.Group != 12 && o.Group != 41 {
			continue
		}
		d, ok := objects.Lookup(objects.GV(o.Group, o.Variation))
		if !ok {
			continue
		}
		size, _ := d.SizeOctets()
		prefix := o.Qualifier.IndexPrefix().Octets()
		if len(o.Data) < prefix+size {
			continue
		}
		return dnp3.CommandStatus(o.Data[prefix+size-1])
	}
	t.Fatalf("the response carried no command echo: %v", resp.Objects)
	return 0
}

type recordingHandler struct {
	selects  int
	operates int
}

func (r *recordingHandler) SelectCROB(uint16, dnp3.ControlRelayOutputBlock) dnp3.CommandStatus {
	r.selects++
	return dnp3.CommandSuccess
}

func (r *recordingHandler) OperateCROB(uint16, dnp3.ControlRelayOutputBlock, outstation.OperateType) dnp3.CommandStatus {
	r.operates++
	return dnp3.CommandSuccess
}

func (r *recordingHandler) SelectAnalog(uint16, outstation.AnalogOutput) dnp3.CommandStatus {
	r.selects++
	return dnp3.CommandSuccess
}

func (r *recordingHandler) OperateAnalog(uint16, outstation.AnalogOutput, outstation.OperateType) dnp3.CommandStatus {
	r.operates++
	return dnp3.CommandSuccess
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met within 3s")
}
