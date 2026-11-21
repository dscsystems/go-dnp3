package dnp3_test

import (
	"encoding/binary"
	"net"
	"testing"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/channel"
	"github.com/dscsystems/go-dnp3/outstation"
	"github.com/dscsystems/go-dnp3/secevent"
)

// acceptAll operates any of outputs 0-9.
type acceptAll struct{}

func (acceptAll) SelectCROB(uint16, dnp3.ControlRelayOutputBlock) dnp3.CommandStatus {
	return dnp3.CommandSuccess
}

func (acceptAll) OperateCROB(uint16, dnp3.ControlRelayOutputBlock, outstation.OperateType) dnp3.CommandStatus {
	return dnp3.CommandSuccess
}

func (acceptAll) SelectAnalog(uint16, outstation.AnalogOutput) dnp3.CommandStatus {
	return dnp3.CommandSuccess
}

func (acceptAll) OperateAnalog(uint16, outstation.AnalogOutput, outstation.OperateType) dnp3.CommandStatus {
	return dnp3.CommandSuccess
}

func (acceptAll) HasOutput(_ dnp3.PointType, index uint16) bool { return index < 10 }

// crob returns a g12v1 header for one CROB at index.
func crob(index byte, code dnp3.ControlCode, onMs uint32) []byte {
	b := []byte{0x0C, 0x01, 0x17, 0x01, index, byte(code), 1}
	b = binary.LittleEndian.AppendUint32(b, onMs)
	b = binary.LittleEndian.AppendUint32(b, onMs)
	return append(b, 0)
}

func request(seq byte, fc byte, objs ...[]byte) []byte {
	b := []byte{0xC0 | seq&0x0F, fc}
	for _, o := range objs {
		b = append(b, o...)
	}
	return b
}

func dialCommands(t *testing.T, p *secevent.Policy) (*requester, *eventLog, string) {
	t.Helper()
	log := newEventLog()
	if p == nil {
		p = &secevent.Policy{}
	}
	p.Sink = secevent.SyncSink(log)
	addr := runOutstationWith(t, outstation.Config{LocalAddr: 10, RemoteAddr: 1, Security: p},
		acceptAll{}, channel.TCPServer("127.0.0.1:0"))
	return dialRequester(t, addr), log, addr
}

func dialRequester(t *testing.T, addr string) *requester {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return &requester{t: t, c: c}
}

func TestCommandEvents(t *testing.T) {
	q, log, _ := dialCommands(t, nil)
	pulse := crob(3, dnp3.ControlClose|dnp3.ControlPulseOn, 100)

	q.send(10, request(0, 0x04, pulse)...) // OPERATE, nothing selected
	q.response()
	q.send(10, request(1, 0x03, pulse)...) // SELECT
	q.response()
	q.send(10, request(2, 0x04, crob(3, dnp3.ControlTrip|dnp3.ControlPulseOn, 100))...) // OPERATE something else
	q.response()
	q.send(10, request(3, 0x03, pulse)...) // SELECT
	q.response()
	q.send(10, request(5, 0x04, pulse)...) // OPERATE, skipping a SEQ
	q.response()
	q.send(10, request(6, 0x05, pulse)...) // DIRECT_OPERATE
	q.response()
	q.send(10, request(7, 0x06, pulse)...)                            // DIRECT_OPERATE_NR
	q.send(10, request(8, 0x05, crob(42, dnp3.ControlLatchOn, 0))...) // no such output
	q.response()
	q.send(10, request(9, 0x05, crob(4, dnp3.ControlPulseOn, 0))...) // pulse of no length
	q.response()

	for _, k := range []secevent.Kind{
		secevent.KindCmdOperateWithoutSelect,
		secevent.KindCmdOperateMismatch,
		secevent.KindCmdOperateSeqGap,
		secevent.KindCmdNoAckOperate,
		secevent.KindAccessUnknownPoint,
		secevent.KindProtoFieldInvalid,
	} {
		log.wait(t, k)
	}
	if !log.has(func(e secevent.Event) bool {
		return e.Kind == secevent.KindCmdReceived && e.AppFunc == 5 && e.Index == 3 &&
			e.ControlCode == dnp3.ControlClose|dnp3.ControlPulseOn && e.CommandStatus == dnp3.CommandSuccess
	}) {
		t.Error("no cmd.received for the DIRECT_OPERATE of output 3")
	}
	if e := log.wait(t, secevent.KindAccessUnknownPoint); e.Index != 42 {
		t.Errorf("unknown point %d, want 42", e.Index)
	}
}

func TestCommandGuard(t *testing.T) {
	guard := func(_ secevent.PeerInfo, _ uint8, _ uint8, index uint16, _ dnp3.ControlCode) dnp3.CommandStatus {
		if index == 5 {
			return dnp3.CommandBlocked
		}
		return dnp3.CommandSuccess
	}
	q, log, _ := dialCommands(t, &secevent.Policy{CommandGuard: guard})
	q.send(10, request(0, 0x05, crob(5, dnp3.ControlLatchOn, 0))...)
	q.response()
	e := log.wait(t, secevent.KindAccessFunctionBlocked)
	if e.Index != 5 || e.CommandStatus != dnp3.CommandBlocked || e.Action != secevent.Rejected {
		t.Errorf("event %+v", e)
	}
}

func TestReadOnlyRefusesControls(t *testing.T) {
	q, log, _ := dialCommands(t, &secevent.Policy{Functions: &secevent.FunctionPolicy{ReadOnly: true}})
	q.send(10, request(0, 0x05, crob(1, dnp3.ControlLatchOn, 0))...)
	q.response()
	if e := log.wait(t, secevent.KindAccessFunctionBlocked); e.CommandStatus != dnp3.CommandNotAuthorized {
		t.Errorf("status %v, want NOT_AUTHORIZED", e.CommandStatus)
	}
	if log.has(func(e secevent.Event) bool { return e.Kind == secevent.KindCmdReceived && e.CommandStatus.OK() }) {
		t.Error("a refused control was operated")
	}
}

func TestCommandReplayedOnAnotherConnection(t *testing.T) {
	q, log, addr := dialCommands(t, nil)
	do := request(4, 0x05, crob(2, dnp3.ControlLatchOn, 0))
	q.send(10, do...)
	q.response()
	_ = q.c.Close()
	log.wait(t, secevent.KindConnClosed)

	q2 := dialRequester(t, addr)
	q2.send(10, do...)
	q2.response()
	log.wait(t, secevent.KindCmdDuplicate)
}

func TestControlAudit(t *testing.T) {
	q, log, _ := dialCommands(t, nil)
	tw := []byte{0x32, 0x01, 0x07, 0x01}
	tw = append(tw, make([]byte, 6)...) // 1970: a long way from now
	q.send(10, request(0, 0x02, tw)...)
	q.response()
	q.send(10, request(1, 0x02, []byte{0x50, 0x01, 0x00, 0x07, 0x07, 0x00})...) // clear restart IIN
	q.response()
	q.send(10, request(2, 0x16, []byte{0x3C, 0x01, 0x06}, []byte{0x1E, 0x00, 0x06})...) // analogs to class 0
	q.response()
	q.send(10, request(3, 0x0D)...) // COLD_RESTART
	q.response()

	if e := log.wait(t, secevent.KindCtrlClockStep); e.Severity != secevent.Alert {
		t.Errorf("clock step severity %v", e.Severity)
	}
	log.wait(t, secevent.KindCtrlTimeWrite)
	log.wait(t, secevent.KindCtrlRestartIINCleared)
	if e := log.wait(t, secevent.KindCtrlAssignClass); e.Severity != secevent.Alert {
		t.Errorf("class 0 assignment severity %v, want Alert", e.Severity)
	}
	log.wait(t, secevent.KindCtrlColdRestart)
}

func TestControlAuditOff(t *testing.T) {
	q, log, _ := dialCommands(t, &secevent.Policy{DisableAudit: true})
	q.send(10, request(0, 0x05, crob(1, dnp3.ControlLatchOn, 0))...)
	q.response()
	q.send(10, request(1, 0x0D)...)
	q.response()
	log.wait(t, secevent.KindCtrlColdRestart) // always reported
	time.Sleep(20 * time.Millisecond)
	if _, ok := log.find(secevent.KindCmdReceived); ok {
		t.Error("cmd.received with audit off")
	}
}
