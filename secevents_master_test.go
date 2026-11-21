package dnp3_test

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/dscsystems/go-dnp3/channel"
	"github.com/dscsystems/go-dnp3/internal/link"
	"github.com/dscsystems/go-dnp3/master"
	"github.com/dscsystems/go-dnp3/secevent"
)

// fakeOutstation answers a master by hand, so a test can send it what no
// real outstation would.
type fakeOutstation struct {
	t    *testing.T
	c    net.Conn
	p    *link.Parser
	tseq byte
}

// request reads the master's next request and returns its sequence number
// and function code.
func (o *fakeOutstation) request() (seq, fc byte) {
	o.t.Helper()
	buf := make([]byte, 512)
	deadline := time.Now().Add(3 * time.Second)
	for {
		if f, err := o.p.Next(); err == nil {
			if len(f.Payload) >= 3 && f.Header.Control.Func == link.FuncUnconfirmedUserData {
				return f.Payload[1] & 0x0F, f.Payload[2]
			}
			continue
		}
		_ = o.c.SetReadDeadline(deadline)
		n, err := o.c.Read(buf)
		if err != nil {
			o.t.Fatalf("no request from the master: %v", err)
		}
		_, _ = o.p.Write(buf[:n])
	}
}

// send sends one application fragment to the master.
func (o *fakeOutstation) send(apdu ...byte) {
	o.t.Helper()
	payload := append([]byte{0xC0 | o.tseq&0x3F}, apdu...)
	o.tseq++
	b := linkFrame(o.t, link.Control{Prm: true, Func: link.FuncUnconfirmedUserData}, 1, 10, payload)
	if _, err := o.c.Write(b); err != nil {
		o.t.Fatal(err)
	}
}

// startMaster runs a master against a fake outstation and returns both.
func startMaster(t *testing.T, cfg master.Config) (*fakeOutstation, *eventLog) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	log := newEventLog()
	cfg.LocalAddr, cfg.RemoteAddr, cfg.Security = 1, 10, log.policy()
	m := master.New(cfg, nil)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _ = m.Run(ctx, channel.TCPClient(l.Addr().String(), channel.NoRetry)) }()
	t.Cleanup(func() { cancel(); <-done })
	c, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return &fakeOutstation{t: t, c: c, p: link.NewParser()}, log
}

func TestMasterResponseEvents(t *testing.T) {
	o, log := startMaster(t, master.Config{})
	seq, fc := o.request() // the startup write clearing the restart indication
	if fc != 0x02 {
		t.Fatalf("first request function %d, want WRITE", fc)
	}
	o.send(0xC0|(seq+5)&0x0F, 0x81, 0x00, 0x00) // stale SEQ
	o.send(0xC0|seq, 0x81, 0x40, 0x00)          // the answer, with DEVICE_TROUBLE
	o.send(0xC0|seq, 0x81, 0x40, 0x00)          // and again, once complete
	o.send(0xC0|(seq+9)&0x0F, 0x81, 0x40, 0x00) // nothing asked for this
	o.send(0xD0|seq, 0x81, 0x40, 0x00)          // UNS on a solicited response
	o.send(0xC0, 0x01)                          // a READ request, to the master
	o.send(0xF3, 0x82, 0x40, 0x00)              // unsolicited, SEQ 3
	o.send(0xF3, 0x82, 0x40, 0x00)              // and again
	o.send(0xF4, 0x82, 0x80, 0x00)              // restarted

	for _, k := range []secevent.Kind{
		secevent.KindSeqResponseStale,
		secevent.KindRemoteDeviceTrouble,
		secevent.KindSeqResponseRepeat,
		secevent.KindUnsolResponseWithoutRequest,
		secevent.KindProtoInvalidResponse,
		secevent.KindProtoWrongDirection,
		secevent.KindSeqUnsolDuplicate,
		secevent.KindRemoteRestart,
	} {
		log.wait(t, k)
	}
	log.mu.Lock()
	defer log.mu.Unlock()
	n := 0
	for _, e := range log.events {
		if e.Kind == secevent.KindRemoteDeviceTrouble {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d remote.device_trouble events, want 1 (rising edge only)", n)
	}
}

func TestMasterUnsolicitedWhileDisabled(t *testing.T) {
	o, log := startMaster(t, master.Config{DisableUnsolOnStartup: true})
	seq, _ := o.request() // clear restart
	o.send(0xC0|seq, 0x81, 0x00, 0x00)
	seq, fc := o.request()
	if fc != 0x15 {
		t.Fatalf("second request function %d, want DISABLE_UNSOLICITED", fc)
	}
	o.send(0xC0|seq, 0x81, 0x00, 0x00)
	o.send(0xF1, 0x82, 0x00, 0x00) // null unsolicited: exempt
	// unsolicited binary input event: g2v1, index 0, value 1
	o.send(0xF2, 0x82, 0x00, 0x00, 0x02, 0x01, 0x17, 0x01, 0x00, 0x81)
	e := log.wait(t, secevent.KindUnsolWhileDisabled)
	if e.AppSeq != 2 {
		t.Errorf("while-disabled event for SEQ %d, want 2", e.AppSeq)
	}
}

func TestMasterReportsRefusal(t *testing.T) {
	o, log := startMaster(t, master.Config{})
	seq, _ := o.request()
	o.send(0xC0|seq, 0x81, 0x00, 0x04) // PARAMETER_ERROR
	if e := log.wait(t, secevent.KindRemoteRejected); e.Value == "" {
		t.Errorf("event %+v", e)
	}
}
