package dnp3_test

import (
	"net"
	"testing"
	"time"

	"github.com/dscsystems/go-dnp3/channel"
	"github.com/dscsystems/go-dnp3/internal/link"
	"github.com/dscsystems/go-dnp3/outstation"
	"github.com/dscsystems/go-dnp3/secevent"
)

// requester sends application fragments to an outstation at link address 10
// from master 1, one single-segment frame each.
type requester struct {
	t    *testing.T
	c    net.Conn
	tseq byte
}

func (q *requester) send(dest uint16, apdu ...byte) {
	q.t.Helper()
	payload := append([]byte{0xC0 | q.tseq&0x3F}, apdu...)
	q.tseq++
	b := linkFrame(q.t, fromMaster(link.FuncUnconfirmedUserData), dest, 1, payload)
	if _, err := q.c.Write(b); err != nil {
		q.t.Fatal(err)
	}
}

// response reads one response and returns its IIN.
func (q *requester) response() (iin1, iin2 byte) {
	q.t.Helper()
	_ = q.c.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 292)
	n, err := q.c.Read(buf)
	if err != nil || n < 15 {
		q.t.Fatalf("no response: %d octets, %v", n, err)
	}
	// header (10) + transport, app control, function, IIN1, IIN2
	return buf[13], buf[14]
}

func TestAppLayerViolationsAreReported(t *testing.T) {
	c, log := dialOutstation(t, outstation.Config{LocalAddr: 10, RemoteAddr: 1})
	q := &requester{t: t, c: c}

	q.send(10, 0xC0, 0x01, 0x3C)                   // truncated object header
	q.send(10, 0x81, 0x01, 0x3C, 0x01, 0x06)       // FIR without FIN
	q.send(10, 0xC2, 0x00, 0x3C, 0x01, 0x06)       // CONFIRM with objects
	q.send(10, 0xE3, 0x01, 0x3C, 0x01, 0x06)       // CON on a request
	q.send(10, 0xC4, 0x03)                         // SELECT without objects
	q.send(10, 0xC5, 0x20)                         // AUTH_REQUEST, no SAv5
	q.send(10, 0xC6, 0x81, 0x00, 0x00)             // a response, to an outstation
	q.send(10, 0xC7, 0x50)                         // unknown function 80
	q.send(10, 0xC8, 0x01, 0x3C, 0x01, 0x06)       // read class 0
	q.send(10, 0xC8, 0x01, 0x3C, 0x01, 0x06)       // the same again
	q.send(10, 0xC8, 0x01, 0x3C, 0x02, 0x06)       // same SEQ, other content
	q.send(10, 0xC9, 0x01, 0xFE, 0x01, 0x00, 0, 1) // unknown object

	for _, k := range []secevent.Kind{
		secevent.KindProtoAppMalformed,
		secevent.KindProtoIncompleteRequest,
		secevent.KindProtoConfirmWithObjects,
		secevent.KindProtoRequestCONUNS,
		secevent.KindProtoNoObjects,
		secevent.KindProtoSecureAuthUnsupported,
		secevent.KindProtoWrongDirection,
		secevent.KindAccessUnsupportedFunction,
		secevent.KindSeqRepeatRequest,
		secevent.KindSeqReusedSeq,
		secevent.KindAccessUnknownObject,
	} {
		e := log.wait(t, k)
		if e.Fields&secevent.HasLinkAddrs == 0 || e.LinkSrc != 1 || e.LinkDest != 10 {
			t.Errorf("%s: link addresses missing: %+v", k, e)
		}
	}
	if !log.has(func(e secevent.Event) bool {
		return e.Kind == secevent.KindAccessUnsupportedFunction && e.AppFunc == 0x50
	}) {
		t.Error("no access.unsupported_function for function 80")
	}
	if e := log.wait(t, secevent.KindProtoAppMalformed); e.Value == "" {
		t.Error("app_malformed carries no reason")
	}
}

func TestFunctionPolicyRefuses(t *testing.T) {
	// No sink: the refusal must happen all the same.
	addr := runOutstationOn(t, outstation.Config{LocalAddr: 10, RemoteAddr: 1,
		Security: &secevent.Policy{Functions: &secevent.FunctionPolicy{DenyRestart: true}}},
		channel.TCPServer("127.0.0.1:0"))
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	q := &requester{t: t, c: c}
	q.send(10, 0xC1, 0x0D) // COLD_RESTART
	if _, iin2 := q.response(); iin2&0x01 == 0 {
		t.Errorf("refused restart answered IIN2 %#02x, want NO_FUNC_CODE_SUPPORT", iin2)
	}
}

func TestFunctionPolicyReports(t *testing.T) {
	log := newEventLog()
	p := log.policy()
	p.Functions = &secevent.FunctionPolicy{ReadOnly: true}
	c, _ := dialOutstation(t, outstation.Config{LocalAddr: 10, RemoteAddr: 1, Security: p})
	q := &requester{t: t, c: c}
	q.send(10, 0xC1, 0x02, 0x22, 0x01, 0x17, 0x01, 0x00, 0x10, 0x00) // WRITE g34v1
	e := log.wait(t, secevent.KindAccessFunctionBlocked)
	if e.Action != secevent.Rejected || e.AppFunc != 2 {
		t.Errorf("event %+v", e)
	}
	if _, iin2 := q.response(); iin2&0x01 == 0 {
		t.Errorf("IIN2 %#02x, want NO_FUNC_CODE_SUPPORT", iin2)
	}
}

func TestBroadcastMisuse(t *testing.T) {
	log := newEventLog()
	p := log.policy()
	p.RejectBroadcastMisuse = true
	c, _ := dialOutstation(t, outstation.Config{LocalAddr: 10, RemoteAddr: 1, Security: p})
	q := &requester{t: t, c: c}
	q.send(0xFFFF, 0xC1, 0x18) // RECORD_CURRENT_TIME: expected
	q.send(0xFFFF, 0xC2, 0x0D) // COLD_RESTART: not
	e := log.wait(t, secevent.KindAccessBroadcastMisuse)
	if e.AppFunc != 0x0D || e.Action != secevent.Rejected {
		t.Errorf("event %+v", e)
	}
	log.mu.Lock()
	defer log.mu.Unlock()
	for _, e := range log.events {
		if e.Kind == secevent.KindAccessBroadcastMisuse && e.AppFunc != 0x0D {
			t.Errorf("broadcast of function %d reported", e.AppFunc)
		}
	}
}

func TestScanDetection(t *testing.T) {
	log := newEventLog()
	p := log.policy()
	p.ScanThreshold = 3
	c, _ := dialOutstation(t, outstation.Config{LocalAddr: 10, RemoteAddr: 1, Security: p})
	q := &requester{t: t, c: c}
	for i := range byte(3) {
		q.send(10, 0xC0|i, 0x50+i)
	}
	log.wait(t, secevent.KindAccessScan)
}

func (l *eventLog) has(match func(secevent.Event) bool) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, e := range l.events {
		if match(e) {
			return true
		}
	}
	return false
}
