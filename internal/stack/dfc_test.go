package stack

import (
	"bytes"
	"testing"

	"github.com/dscsystems/go-dnp3/internal/link"
)

// P2 report: DFC is recorded but ignored. This exercises the fix at the
// level that actually matters operationally: a fragment split into several
// link-layer segments, where the peer raises DFC on the ACK to the first
// one. pump must not push the second segment through — but it also must not
// lose it or fail the whole transmission, since DFC is a pause, not an
// error. Sending must resume on its own once a later reply (here, the ACK to
// a keep-alive status probe, which SendLinkStatusRequest must still be able
// to send while paused) shows DFC has cleared.
func TestPumpPausesForDFCAndResumesAfterItClears(t *testing.T) {
	s := New(Config{
		LocalAddr: 1, RemoteAddr: 10, IsMaster: true,
		UseConfirms: true, MaxRetries: 3,
	})

	ack := func(src uint16, dfc bool) []byte {
		raw, err := link.Encode(nil, link.Header{
			Control: link.Control{Prm: false, Func: link.FuncAck, Fcv: dfc},
			Dest:    1, Src: src, Length: link.MinLength,
		}, nil)
		if err != nil {
			t.Fatalf("encode ack: %v", err)
		}
		return raw
	}
	status := func(src uint16, dfc bool) []byte {
		raw, err := link.Encode(nil, link.Header{
			Control: link.Control{Prm: false, Func: link.FuncLinkStatus, Fcv: dfc},
			Dest:    1, Src: src, Length: link.MinLength,
		}, nil)
		if err != nil {
			t.Fatalf("encode status: %v", err)
		}
		return raw
	}

	// Bigger than one transport segment (249 octets), so the fragment needs
	// two: the second is exactly what must wait for DFC.
	fragment := bytes.Repeat([]byte{0xAB}, 300)

	var out bytes.Buffer
	if err := s.SendTo(&out, 10, fragment); err != nil {
		t.Fatalf("send: %v", err)
	}
	if !s.Busy() {
		t.Fatal("sending a confirmed fragment did not leave the stack awaiting an ACK")
	}

	// The real peer completes the link-reset handshake this first send
	// triggers, which queues segment 1 as confirmed user data.
	if err := s.Receive(&out, ack(10, false), func(Received) {}); err != nil {
		t.Fatalf("receive (reset ack): %v", err)
	}
	if !s.Busy() {
		t.Fatal("the reset ack did not lead to segment 1 being sent")
	}

	// Segment 1's own ACK raises DFC. Segment 2 must not go out for it.
	before := out.Len()
	if err := s.Receive(&out, ack(10, true), func(Received) {}); err != nil {
		t.Fatalf("receive (segment 1 ack, DFC set): %v", err)
	}
	if out.Len() != before {
		t.Error("segment 2 was sent even though the peer's ack raised DFC")
	}
	if !s.Busy() {
		t.Error("pausing for DFC must not look like the transmission finished")
	}

	// A keep-alive probe must still be able to go out while paused like
	// this — there is otherwise no way to ever learn DFC has cleared.
	if err := s.SendLinkStatusRequest(&out); err != nil {
		t.Fatalf("status request while paused for DFC: %v", err)
	}
	if out.Len() == before {
		t.Fatal("SendLinkStatusRequest did not actually send anything while paused")
	}

	// The status reply clears DFC, and drain's own post-ACK pump resumes
	// sending the segment that had been left waiting.
	before = out.Len()
	if err := s.Receive(&out, status(10, false), func(Received) {}); err != nil {
		t.Fatalf("receive (status reply, DFC clear): %v", err)
	}
	if out.Len() == before {
		t.Fatal("segment 2 was not sent once DFC cleared")
	}
	if !s.Busy() {
		t.Fatal("segment 2 was sent as a confirmed frame; the stack should be awaiting its ACK")
	}

	// Finish the transmission off, for good measure.
	if err := s.Receive(&out, ack(10, false), func(Received) {}); err != nil {
		t.Fatalf("receive (segment 2 ack): %v", err)
	}
	if s.Busy() {
		t.Error("the fragment is fully sent and acknowledged; the stack should be idle")
	}
}
