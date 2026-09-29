package link

import "testing"

// P2 report: DFC is recorded but ignored. Primary.OnFrame records the peer's
// Data Flow Control flag into p.dfc (see DataFlowControl), but nothing in
// Send or sendPending ever consults it before queuing more confirmed user
// data. A peer that raises DFC to say "my buffers are full, stop sending" is
// therefore not actually slowed down at all.
func TestPrimaryHonoursDFC(t *testing.T) {
	p := &Primary{LocalAddr: 1, RemoteAddr: 10, UseConfirms: true, MaxRetries: 3}

	if _, _, err := p.Send([]byte{0xC0}); err != nil {
		t.Fatalf("first send: %v", err)
	}
	// Completes the link-reset handshake, which immediately queues the
	// pending payload as confirmed user data (hence ActionTransmit again,
	// not ActionComplete).
	if _, action := p.OnFrame(secondaryFrame(FuncAck, 10, 1)); action != ActionTransmit {
		t.Fatalf("link-reset ack action = %s, want transmit", action)
	}

	// The peer's ack for that data frame also raises DFC.
	dataAck := secondaryFrame(FuncAck, 10, 1)
	dataAck.Header.Control.Fcv = true
	if _, action := p.OnFrame(dataAck); action != ActionComplete {
		t.Fatalf("data ack action = %s, want complete", action)
	}
	if !p.DataFlowControl() {
		t.Fatal("DFC from the peer was not recorded; the rest of this test proves nothing")
	}

	if _, _, err := p.Send([]byte{0xC1}); err == nil {
		t.Error("Send proceeded while the peer's DFC flag was still set; " +
			"user data must wait until DFC clears")
	}
}

// DFC answers something we sent. A stray secondary frame on an idle link used
// to latch it, and nothing on an idle link ever cleared it again.
func TestPrimaryIgnoresDFCFromAnIdleLink(t *testing.T) {
	p := &Primary{LocalAddr: 1, RemoteAddr: 10, UseConfirms: true, MaxRetries: 3}

	stray := secondaryFrame(FuncAck, 10, 1)
	stray.Header.Control.Fcv = true // DFC
	p.OnFrame(stray)

	if p.DataFlowControl() {
		t.Fatal("DFC from a frame nothing was waiting for latched flow control")
	}
	if _, _, err := p.Send([]byte{0xC0}); err != nil {
		t.Errorf("send refused after a stray frame: %v", err)
	}
}
