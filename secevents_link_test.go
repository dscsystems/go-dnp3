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

func linkFrame(t *testing.T, c link.Control, dest, src uint16, payload []byte) []byte {
	t.Helper()
	b, err := link.Encode(nil, link.Header{Control: c, Dest: dest, Src: src}, payload)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func fromMaster(f link.Function) link.Control { return link.Control{Dir: true, Prm: true, Func: f} }

func dialOutstation(t *testing.T, cfg outstation.Config) (net.Conn, *eventLog) {
	t.Helper()
	log := newEventLog()
	if cfg.Security == nil {
		cfg.Security = log.policy()
	} else {
		cfg.Security.Sink = secevent.SyncSink(log)
	}
	addr := runOutstationOn(t, cfg, channel.TCPServer("127.0.0.1:0"))
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	log.wait(t, secevent.KindConnAccepted)
	return c, log
}

func TestLinkAnomaliesAreReported(t *testing.T) {
	c, log := dialOutstation(t, outstation.Config{LocalAddr: 10, RemoteAddr: 1})

	crc := linkFrame(t, fromMaster(link.FuncRequestLinkStatus), 10, 1, nil)
	crc[8] ^= 0xFF
	for _, b := range [][]byte{
		{'G', 'E', 'T', ' '},
		crc,
		linkFrame(t, fromMaster(link.FuncRequestLinkStatus), 99, 1, nil),
		linkFrame(t, link.Control{Prm: true, Func: link.FuncRequestLinkStatus}, 10, 1, nil),
		linkFrame(t, fromMaster(link.FuncRequestLinkStatus), 10, 0xFFF5, nil),
		linkFrame(t, fromMaster(7), 10, 1, nil),
		linkFrame(t, link.Control{Dir: true, Prm: true, Fcv: true, Func: link.FuncConfirmedUserData}, 10, 1, []byte{0xC0, 0xC0, 1}),
		linkFrame(t, fromMaster(link.FuncUnconfirmedUserData), 10, 1, []byte{0x01, 0xC0, 1}),
		linkFrame(t, fromMaster(link.FuncUnconfirmedUserData), 0xFFFF, 1, []byte{0xC0, 0xC0, 1}),
	} {
		if _, err := c.Write(b); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond) // one read per write, so the parser deltas stay apart
	}

	for k, sev := range map[secevent.Kind]secevent.Severity{
		secevent.KindLinkGarbage:             secevent.Warning,
		secevent.KindLinkCRCError:            secevent.Warning,
		secevent.KindLinkForeignDest:         secevent.Notice,
		secevent.KindLinkWrongDir:            secevent.Warning,
		secevent.KindLinkInvalidSource:       secevent.Warning,
		secevent.KindLinkUnsupportedFunction: secevent.Warning,
		secevent.KindLinkNotReset:            secevent.Notice,
		secevent.KindTransportDiscard:        secevent.Notice,
		secevent.KindLinkBroadcast:           secevent.Notice,
	} {
		e := log.wait(t, k)
		if e.Severity != sev {
			t.Errorf("%s: severity %v, want %v", k, e.Severity, sev)
		}
		if e.Transport != "tcp" || e.ConnID == 0 {
			t.Errorf("%s: transport %q conn %d", k, e.Transport, e.ConnID)
		}
	}
	if e := log.wait(t, secevent.KindTransportDiscard); e.Value != "no_fir" {
		t.Errorf("transport.discard reason %q, want no_fir", e.Value)
	}
	if e := log.wait(t, secevent.KindLinkInvalidSource); e.Fields&secevent.HasLinkAddrs == 0 || e.LinkSrc != 0xFFF5 {
		t.Errorf("invalid source event %+v", e)
	}
}

func TestForeignDestSilentOnSharedBus(t *testing.T) {
	c, log := dialOutstation(t, outstation.Config{LocalAddr: 10, RemoteAddr: 1,
		Security: &secevent.Policy{LinkTopology: secevent.SharedBus}})
	for _, b := range [][]byte{
		linkFrame(t, fromMaster(link.FuncRequestLinkStatus), 99, 1, nil),
		linkFrame(t, link.Control{Prm: true, Func: link.FuncRequestLinkStatus}, 10, 1, nil),
	} {
		if _, err := c.Write(b); err != nil {
			t.Fatal(err)
		}
	}
	log.wait(t, secevent.KindLinkWrongDir)
	if _, ok := log.find(secevent.KindLinkForeignDest); ok {
		t.Error("link.foreign_dest reported on a shared bus")
	}
}

func TestUnexpectedMaster(t *testing.T) {
	for _, reject := range []bool{false, true} {
		t.Run(map[bool]string{false: "observe", true: "reject"}[reject], func(t *testing.T) {
			c, log := dialOutstation(t, outstation.Config{LocalAddr: 10, RemoteAddr: 1,
				Security: &secevent.Policy{AllowedMasters: []uint16{1}, RejectUnexpectedMaster: reject}})
			if _, err := c.Write(linkFrame(t, fromMaster(link.FuncRequestLinkStatus), 10, 7, nil)); err != nil {
				t.Fatal(err)
			}
			e := log.wait(t, secevent.KindLinkUnexpectedMaster)
			if e.LinkSrc != 7 || (e.Action == secevent.Rejected) != reject {
				t.Errorf("event %+v", e)
			}
			_ = c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
			buf := make([]byte, 64)
			n, _ := c.Read(buf)
			if reject && n > 0 {
				t.Errorf("rejected master was answered: % x", buf[:n])
			}
			if !reject && n == 0 {
				t.Error("observed master was not answered")
			}
			// The allowed master is served and reported on neither path.
			if _, err := c.Write(linkFrame(t, fromMaster(link.FuncRequestLinkStatus), 10, 1, nil)); err != nil {
				t.Fatal(err)
			}
			_ = c.SetReadDeadline(time.Now().Add(time.Second))
			if n, _ := c.Read(buf); n == 0 {
				t.Error("allowed master not answered")
			}
		})
	}
}

func TestRejectUnexpectedMasterNeedsList(t *testing.T) {
	if err := (&secevent.Policy{RejectUnexpectedMaster: true}).Valid(); err == nil {
		t.Error("RejectUnexpectedMaster without AllowedMasters accepted")
	}
}
