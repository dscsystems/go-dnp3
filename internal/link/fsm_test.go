package link

import (
	"bytes"
	"testing"
)

func primaryFrame(fn Function, fcb, fcv bool, src, dest uint16, payload []byte) Frame {
	return Frame{
		Header: Header{
			Control: Control{Dir: true, Prm: true, Fcb: fcb, Fcv: fcv, Func: fn},
			Dest:    dest, Src: src, Length: uint8(MinLength + len(payload)),
		},
		Payload: payload,
	}
}

func secondaryFrame(fn Function, src, dest uint16) Frame {
	return Frame{
		Header: Header{
			Control: Control{Prm: false, Func: fn},
			Dest:    dest, Src: src, Length: MinLength,
		},
	}
}

// ---------- Secondary ----------

func TestSecondaryResetLinkStates(t *testing.T) {
	s := &Secondary{LocalAddr: 10}
	if s.IsReset() {
		t.Fatal("a fresh secondary should not be reset")
	}

	res := s.OnFrame(primaryFrame(FuncResetLinkStates, false, false, 1, 10, nil))
	if res.Reply == nil || res.Reply.Header.Control.Func != FuncAck {
		t.Fatalf("reply = %v, want ACK", res.Reply)
	}
	if res.Reply.Header.Dest != 1 || res.Reply.Header.Src != 10 {
		t.Errorf("reply addresses = %d→%d, want 10→1", res.Reply.Header.Src, res.Reply.Header.Dest)
	}
	if res.Reply.Header.Control.Prm {
		t.Error("a secondary reply must have PRM clear")
	}
	if !s.IsReset() {
		t.Error("link should be reset after RESET_LINK_STATES")
	}
}

func TestSecondaryRejectsConfirmedDataBeforeReset(t *testing.T) {
	s := &Secondary{LocalAddr: 10}
	res := s.OnFrame(primaryFrame(FuncConfirmedUserData, true, true, 1, 10, []byte{0xC0, 0x01}))

	if res.Reply == nil || res.Reply.Header.Control.Func != FuncNack {
		t.Errorf("reply = %v, want NACK", res.Reply)
	}
	if res.Payload != nil {
		t.Error("payload must not be delivered before the link is reset")
	}
	if !res.Discarded {
		t.Error("Discarded should be set")
	}
}

// TestSecondaryFCBSequence walks the frame count bit through the sequence a
// conforming primary produces, then repeats a bit to simulate a lost ACK.
func TestSecondaryFCBSequence(t *testing.T) {
	s := &Secondary{LocalAddr: 10}
	s.OnFrame(primaryFrame(FuncResetLinkStates, false, false, 1, 10, nil))

	// After a reset the first confirmed frame carries FCB=1.
	first := []byte{0xC0, 0xC1, 0x01}
	res := s.OnFrame(primaryFrame(FuncConfirmedUserData, true, true, 1, 10, first))
	if res.Reply.Header.Control.Func != FuncAck {
		t.Fatal("want ACK for the first confirmed frame")
	}
	if !bytes.Equal(res.Payload, first) {
		t.Errorf("payload = % x, want % x", res.Payload, first)
	}

	// The next carries FCB=0.
	second := []byte{0xC1, 0xC2, 0x02}
	res = s.OnFrame(primaryFrame(FuncConfirmedUserData, false, true, 1, 10, second))
	if !bytes.Equal(res.Payload, second) {
		t.Errorf("payload = % x, want % x", res.Payload, second)
	}

	// A repeat of FCB=0 is a retransmission: ACK it again, but do not deliver
	// the payload a second time. Delivering it would duplicate an application
	// fragment, which is exactly the failure the FCB exists to prevent.
	res = s.OnFrame(primaryFrame(FuncConfirmedUserData, false, true, 1, 10, second))
	if res.Reply == nil || res.Reply.Header.Control.Func != FuncAck {
		t.Error("a retransmission must still be ACKed")
	}
	if res.Payload != nil {
		t.Error("a retransmitted payload must not be delivered twice")
	}
	if !res.Discarded {
		t.Error("Discarded should be set for the duplicate")
	}

	// The sequence then continues with FCB=1, undisturbed by the duplicate.
	third := []byte{0xC2, 0xC3, 0x03}
	res = s.OnFrame(primaryFrame(FuncConfirmedUserData, true, true, 1, 10, third))
	if !bytes.Equal(res.Payload, third) {
		t.Errorf("payload = % x, want % x — the FCB advanced on a duplicate", res.Payload, third)
	}
}

func TestSecondaryUnconfirmedDataNeedsNoReset(t *testing.T) {
	s := &Secondary{LocalAddr: 10}
	data := []byte{0xC0, 0x01, 0x3C, 0x01, 0x06}
	res := s.OnFrame(primaryFrame(FuncUnconfirmedUserData, false, false, 1, 10, data))

	if res.Reply != nil {
		t.Errorf("unconfirmed data must not be answered, got %v", res.Reply)
	}
	if !bytes.Equal(res.Payload, data) {
		t.Errorf("payload = % x, want % x", res.Payload, data)
	}
}

func TestSecondaryRequestLinkStatus(t *testing.T) {
	s := &Secondary{LocalAddr: 10}
	res := s.OnFrame(primaryFrame(FuncRequestLinkStatus, false, false, 1, 10, nil))
	if res.Reply == nil || res.Reply.Header.Control.Func != FuncLinkStatus {
		t.Errorf("reply = %v, want LINK_STATUS", res.Reply)
	}
}

func TestSecondaryTestLinkStates(t *testing.T) {
	s := &Secondary{LocalAddr: 10}

	// Before a reset, TEST_LINK_STATES must be refused.
	res := s.OnFrame(primaryFrame(FuncTestLinkStates, true, true, 1, 10, nil))
	if res.Reply.Header.Control.Func != FuncNack {
		t.Error("want NACK before reset")
	}

	s.OnFrame(primaryFrame(FuncResetLinkStates, false, false, 1, 10, nil))

	res = s.OnFrame(primaryFrame(FuncTestLinkStates, true, true, 1, 10, nil))
	if res.Reply.Header.Control.Func != FuncAck {
		t.Error("want ACK for a matching FCB")
	}
	// The FCB advanced, so the same bit again must be refused.
	res = s.OnFrame(primaryFrame(FuncTestLinkStates, true, true, 1, 10, nil))
	if res.Reply.Header.Control.Func != FuncNack {
		t.Error("want NACK for a stale FCB")
	}
}

func TestSecondaryUnknownFunction(t *testing.T) {
	s := &Secondary{LocalAddr: 10}
	res := s.OnFrame(primaryFrame(Function(7), false, false, 1, 10, nil))
	if res.Reply == nil || res.Reply.Header.Control.Func != FuncNotSupported {
		t.Errorf("reply = %v, want NOT_SUPPORTED", res.Reply)
	}
}

func TestSecondaryIgnoresSecondaryFrames(t *testing.T) {
	s := &Secondary{LocalAddr: 10}
	res := s.OnFrame(secondaryFrame(FuncAck, 1, 10))
	if res.Reply != nil || res.Payload != nil {
		t.Error("a secondary frame belongs to the Primary half and must be ignored")
	}
}

func TestSecondaryResetClearsState(t *testing.T) {
	s := &Secondary{LocalAddr: 10}
	s.OnFrame(primaryFrame(FuncResetLinkStates, false, false, 1, 10, nil))
	s.Reset()
	if s.IsReset() {
		t.Error("Reset should return the secondary to unreset")
	}
}

// ---------- Primary ----------

func TestPrimaryUnconfirmedIsPassThrough(t *testing.T) {
	p := &Primary{LocalAddr: 1, RemoteAddr: 10, IsMaster: true, UseConfirms: false}
	data := []byte{0xC0, 0x01}

	f, action, err := p.Send(data)
	if err != nil {
		t.Fatal(err)
	}
	if action != ActionComplete {
		t.Errorf("action = %s, want complete", action)
	}
	if f.Header.Control.Func != FuncUnconfirmedUserData {
		t.Errorf("func = %v, want UNCONFIRMED_USER_DATA", f.Header.Control.Func)
	}
	if f.Header.Control.Fcv {
		t.Error("unconfirmed frames must have FCV clear")
	}
	if p.Busy() {
		t.Error("an unconfirmed send leaves nothing in flight")
	}
}

// TestPrimaryConfirmedHandshake walks the full sequence: reset, data, ACK,
// then a second send that skips the reset and toggles the FCB.
func TestPrimaryConfirmedHandshake(t *testing.T) {
	p := &Primary{LocalAddr: 1, RemoteAddr: 10, IsMaster: true, UseConfirms: true, MaxRetries: 3}

	f, action, err := p.Send([]byte{0xC0, 0x01})
	if err != nil {
		t.Fatal(err)
	}
	if action != ActionTransmit || f.Header.Control.Func != FuncResetLinkStates {
		t.Fatalf("first frame = %v/%s, want RESET_LINK_STATES/transmit", f.Header.Control.Func, action)
	}
	if !p.Busy() {
		t.Error("primary should be busy after Send")
	}

	// The peer ACKs the reset; the data frame follows with FCB set.
	f, action = p.OnFrame(secondaryFrame(FuncAck, 10, 1))
	if action != ActionTransmit || f.Header.Control.Func != FuncConfirmedUserData {
		t.Fatalf("after reset ACK: %v/%s, want CONFIRMED_USER_DATA/transmit", f.Header.Control.Func, action)
	}
	if !f.Header.Control.Fcb {
		t.Error("the first confirmed frame after a reset must carry FCB=1")
	}
	if !f.Header.Control.Fcv {
		t.Error("confirmed frames must carry FCV")
	}

	// The peer ACKs the data.
	_, action = p.OnFrame(secondaryFrame(FuncAck, 10, 1))
	if action != ActionComplete {
		t.Fatalf("action = %s, want complete", action)
	}
	if p.Busy() {
		t.Error("primary should be idle after completion")
	}
	if !p.LinkUp() {
		t.Error("link should be up")
	}

	// A second send skips the reset and flips the FCB.
	f, action, err = p.Send([]byte{0xC1, 0x01})
	if err != nil {
		t.Fatal(err)
	}
	if action != ActionTransmit || f.Header.Control.Func != FuncConfirmedUserData {
		t.Fatalf("second send = %v/%s", f.Header.Control.Func, action)
	}
	if f.Header.Control.Fcb {
		t.Error("the second confirmed frame must carry FCB=0")
	}
}

func TestPrimaryRetriesThenFails(t *testing.T) {
	p := &Primary{LocalAddr: 1, RemoteAddr: 10, IsMaster: true, UseConfirms: true, MaxRetries: 2}

	first, _, err := p.Send([]byte{0xC0})
	if err != nil {
		t.Fatal(err)
	}

	for i := 1; i <= 2; i++ {
		f, action := p.OnTimeout()
		if action != ActionTransmit {
			t.Fatalf("retry %d: action = %s, want transmit", i, action)
		}
		// A retransmission must be byte-identical. Advancing the frame count
		// bit here would make the peer treat the retry as a new fragment.
		if f.Header.Control != first.Header.Control {
			t.Errorf("retry %d changed the control octet: %v vs %v",
				i, f.Header.Control, first.Header.Control)
		}
		if p.Retries() != i {
			t.Errorf("Retries = %d, want %d", p.Retries(), i)
		}
	}

	if _, action := p.OnTimeout(); action != ActionFailed {
		t.Errorf("action = %s, want failed after exhausting retries", action)
	}
	if p.Busy() {
		t.Error("a failed primary should be idle")
	}
	if p.LinkUp() {
		t.Error("a failed transmission must tear down link state so the next send re-resets")
	}
}

func TestPrimaryNackRestartsHandshake(t *testing.T) {
	p := &Primary{LocalAddr: 1, RemoteAddr: 10, IsMaster: true, UseConfirms: true, MaxRetries: 3}
	p.Send([]byte{0xC0})
	p.OnFrame(secondaryFrame(FuncAck, 10, 1)) // reset ACKed, data sent

	// The peer NACKs the data: its link is not reset after all.
	f, action := p.OnFrame(secondaryFrame(FuncNack, 10, 1))
	if action != ActionTransmit {
		t.Fatalf("action = %s, want transmit", action)
	}
	if f.Header.Control.Func != FuncResetLinkStates {
		t.Errorf("func = %v, want RESET_LINK_STATES — a NACK means redo the handshake",
			f.Header.Control.Func)
	}
	if p.LinkUp() {
		t.Error("link should be marked down after a NACK")
	}
}

func TestPrimaryNotSupportedFails(t *testing.T) {
	p := &Primary{LocalAddr: 1, RemoteAddr: 10, UseConfirms: true, MaxRetries: 3}
	p.Send([]byte{0xC0})
	if _, action := p.OnFrame(secondaryFrame(FuncNotSupported, 10, 1)); action != ActionFailed {
		t.Errorf("action = %s, want failed", action)
	}
}

func TestPrimaryRejectsConcurrentSend(t *testing.T) {
	p := &Primary{LocalAddr: 1, RemoteAddr: 10, UseConfirms: true, MaxRetries: 3}
	p.Send([]byte{0xC0})
	if _, _, err := p.Send([]byte{0xC1}); err == nil {
		t.Error("a second Send while busy should fail")
	}
}

func TestPrimaryRejectsOversizePayload(t *testing.T) {
	p := &Primary{LocalAddr: 1, RemoteAddr: 10}
	if _, _, err := p.Send(make([]byte, MaxPayload+1)); err == nil {
		t.Error("want an error for an oversize payload")
	}
}

func TestPrimaryRequestLinkStatus(t *testing.T) {
	p := &Primary{LocalAddr: 1, RemoteAddr: 10, UseConfirms: true, MaxRetries: 3}

	f, action, err := p.RequestLinkStatus()
	if err != nil {
		t.Fatal(err)
	}
	if action != ActionTransmit || f.Header.Control.Func != FuncRequestLinkStatus {
		t.Fatalf("frame = %v/%s", f.Header.Control.Func, action)
	}
	if _, action := p.OnFrame(secondaryFrame(FuncLinkStatus, 10, 1)); action != ActionComplete {
		t.Errorf("action = %s, want complete", action)
	}
}

func TestPrimaryTracksDFC(t *testing.T) {
	p := &Primary{LocalAddr: 1, RemoteAddr: 10, UseConfirms: true, MaxRetries: 3}
	p.Send([]byte{0xC0})

	busy := secondaryFrame(FuncAck, 10, 1)
	busy.Header.Control.Fcv = true // DFC on a secondary frame
	p.OnFrame(busy)
	if !p.DataFlowControl() {
		t.Error("DFC from the peer was not recorded")
	}
}

func TestPrimaryIgnoresPrimaryFrames(t *testing.T) {
	p := &Primary{LocalAddr: 1, RemoteAddr: 10, UseConfirms: true, MaxRetries: 3}
	p.Send([]byte{0xC0})
	if _, action := p.OnFrame(primaryFrame(FuncResetLinkStates, false, false, 10, 1, nil)); action != ActionNone {
		t.Error("a primary frame belongs to the Secondary half and must be ignored")
	}
}

func TestPrimaryResetClearsState(t *testing.T) {
	p := &Primary{LocalAddr: 1, RemoteAddr: 10, UseConfirms: true, MaxRetries: 3}
	p.Send([]byte{0xC0})
	p.OnFrame(secondaryFrame(FuncAck, 10, 1))
	p.Reset()
	if p.Busy() || p.LinkUp() {
		t.Error("Reset should clear both the in-flight state and the link state")
	}
}

// TestPrimarySecondaryInterop drives the two halves against each other over
// several exchanges, which is the closest thing to a real link without a
// socket, and catches FCB conventions that disagree between the two.
func TestPrimarySecondaryInterop(t *testing.T) {
	pri := &Primary{LocalAddr: 1, RemoteAddr: 10, IsMaster: true, UseConfirms: true, MaxRetries: 3}
	sec := &Secondary{LocalAddr: 10}

	for i := range 6 {
		payload := []byte{byte(0xC0 + i), 0x01}

		f, action, err := pri.Send(payload)
		if err != nil {
			t.Fatalf("exchange %d: Send: %v", i, err)
		}

		var delivered []byte
		for range 4 { // bounded: reset + data is at most two round trips
			if action != ActionTransmit {
				break
			}
			res := sec.OnFrame(f)
			if res.Payload != nil {
				delivered = append([]byte(nil), res.Payload...)
			}
			if res.Reply == nil {
				t.Fatalf("exchange %d: secondary sent no reply to %v", i, f.Header.Control.Func)
			}
			f, action = pri.OnFrame(*res.Reply)
		}

		if action != ActionComplete {
			t.Fatalf("exchange %d: action = %s, want complete", i, action)
		}
		if !bytes.Equal(delivered, payload) {
			t.Fatalf("exchange %d: delivered % x, want % x", i, delivered, payload)
		}
	}
}
