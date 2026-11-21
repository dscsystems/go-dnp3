package dnp3_test

import (
	"testing"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/internal/link"
	"github.com/dscsystems/go-dnp3/outstation"
	"github.com/dscsystems/go-dnp3/secevent"
)

var once = secevent.Rate{N: 1, Per: time.Hour}

func TestFloodThrottlesRequests(t *testing.T) {
	q, log, _ := dialCommands(t, &secevent.Policy{FloodAction: secevent.Throttled,
		Limits: secevent.Limits{Integrity: once}})
	integrity := request(0, 0x01, []byte{0x3C, 0x01, 0x06})
	q.send(10, integrity...)
	if _, iin2 := q.response(); iin2&0x04 != 0 {
		t.Fatalf("first integrity poll refused: IIN2 %#02x", iin2)
	}
	q.send(10, request(1, 0x01, []byte{0x3C, 0x01, 0x06})...)
	if _, iin2 := q.response(); iin2&0x04 == 0 {
		t.Errorf("throttled integrity poll answered IIN2 %#02x, want PARAMETER_ERROR", iin2)
	}
	if e := log.wait(t, secevent.KindFloodIntegrity); e.Action != secevent.Throttled {
		t.Errorf("event %+v", e)
	}
}

func TestFloodThrottlesCommands(t *testing.T) {
	q, log, _ := dialCommands(t, &secevent.Policy{FloodAction: secevent.Throttled,
		Limits: secevent.Limits{Commands: once}})
	q.send(10, request(0, 0x05, crob(1, dnp3.ControlLatchOn, 0))...)
	q.response()
	q.send(10, request(1, 0x05, crob(1, dnp3.ControlLatchOn, 0))...)
	q.response()
	log.wait(t, secevent.KindFloodCommands)
	if !log.has(func(e secevent.Event) bool {
		return e.Kind == secevent.KindAccessFunctionBlocked && e.CommandStatus == dnp3.CommandTooManyOps
	}) && !log.has(func(e secevent.Event) bool {
		return e.Kind == secevent.KindCmdReceived && e.CommandStatus == dnp3.CommandTooManyOps
	}) {
		t.Error("the throttled control was not answered TOO_MANY_OPS")
	}
	if log.has(func(e secevent.Event) bool {
		return e.Kind == secevent.KindCmdReceived && e.AppSeq == 1 && e.CommandStatus.OK()
	}) {
		t.Error("the throttled control was operated")
	}
}

func TestFloodDisconnects(t *testing.T) {
	q, log, _ := dialCommands(t, &secevent.Policy{FloodAction: secevent.Disconnected,
		Limits: secevent.Limits{TimeWrite: once}})
	tw := append([]byte{0x32, 0x01, 0x07, 0x01}, make([]byte, 6)...)
	q.send(10, request(0, 0x02, tw)...)
	q.response()
	q.send(10, request(1, 0x02, tw)...)
	log.wait(t, secevent.KindFloodTimeWrite)
	if e := log.wait(t, secevent.KindConnClosed); e.Detail == "" || e.Detail[:5] != "flood" {
		t.Errorf("closed: %q", e.Detail)
	}
}

func TestLinkStatusFloodIsReportedOnly(t *testing.T) {
	log := newEventLog()
	p := log.policy()
	p.FloodAction = secevent.Disconnected
	p.Limits.LinkStatus = once
	c, _ := dialOutstation(t, outstation.Config{LocalAddr: 10, RemoteAddr: 1, Security: p})
	for range 3 {
		if _, err := c.Write(linkFrame(t, fromMaster(link.FuncRequestLinkStatus), 10, 1, nil)); err != nil {
			t.Fatal(err)
		}
	}
	if e := log.wait(t, secevent.KindFloodLinkStatus); e.Action != secevent.Observed {
		t.Errorf("link status flood action %v, want Observed", e.Action)
	}
	time.Sleep(50 * time.Millisecond)
	if _, closed := log.find(secevent.KindConnClosed); closed {
		t.Error("a link status flood dropped the connection")
	}
}

func TestCommandBruteForce(t *testing.T) {
	log := newEventLog()
	p := log.policy()
	p.Limits.CommandRejects = once
	c, _ := dialOutstation(t, outstation.Config{LocalAddr: 10, RemoteAddr: 1, Security: p})
	q := &requester{t: t, c: c}
	for i := range byte(2) {
		q.send(10, request(i, 0x05, crob(i, dnp3.ControlLatchOn, 0))...)
		q.response()
	}
	log.wait(t, secevent.KindCmdBruteForce)
}

func TestConfirmWithheld(t *testing.T) {
	log := newEventLog()
	p := log.policy()
	p.ConfirmWithheldAfter = 2
	_, _ = dialOutstation(t, outstation.Config{LocalAddr: 10, RemoteAddr: 1, Security: p,
		Unsolicited: outstation.UnsolicitedConfig{Enabled: true, ConfirmTimeout: 30 * time.Millisecond}})
	log.wait(t, secevent.KindDoSConfirmWithheld)
}

func TestOctetFloodDisconnects(t *testing.T) {
	c, log := dialOutstation(t, outstation.Config{LocalAddr: 10, RemoteAddr: 1,
		Security: &secevent.Policy{FloodAction: secevent.Disconnected,
			Limits: secevent.Limits{Octets: secevent.Rate{N: 64, Per: time.Hour}}}})
	if _, err := c.Write(make([]byte, 200)); err != nil {
		t.Fatal(err)
	}
	if e := log.wait(t, secevent.KindFloodFrames); e.Value != "octets" || e.Action != secevent.Disconnected {
		t.Errorf("event %+v", e)
	}
	log.wait(t, secevent.KindConnClosed)
}
