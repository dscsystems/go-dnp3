package stack

import (
	"bytes"
	"testing"

	"github.com/dscsystems/go-dnp3/internal/link"
)

// outstationStack is a stack configured as an outstation at address 10.
func outstationStack() *Stack {
	return New(Config{LocalAddr: 10, RemoteAddr: 1, IsMaster: false})
}

func bcastFrame(t *testing.T, fn link.Function, fcb, fcv bool, payload []byte) []byte {
	t.Helper()
	raw, err := link.Encode(nil, link.Header{
		Control: link.Control{Prm: true, Func: fn, Fcb: fcb, Fcv: fcv},
		Dest:    link.BroadcastNoConfirm, Src: 1,
		Length: uint8(link.MinLength + len(payload)),
	}, payload)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return raw
}

// Report: invalid broadcast link frames are accepted and answered. drain
// filters only on addressedToUs, which passes every broadcast address
// through, and then hands any primary frame to the secondary state machine
// without looking at its function code.
//
// Replying to a broadcast is the sharp end of this. A broadcast is by
// definition addressed to every outstation on the line, so a reply means
// every one of them transmits at the same moment — the collision the
// application layer above goes to some length to avoid (see the outstation's
// "a broadcast request is executed but never answered"). The link layer
// underneath it answers anyway.
func TestBroadcastLinkRequestsAreNotAnswered(t *testing.T) {
	tests := []struct {
		name string
		fn   link.Function
		fcv  bool
	}{
		{"reset link states", link.FuncResetLinkStates, false},
		{"request link status", link.FuncRequestLinkStatus, false},
		{"test link states", link.FuncTestLinkStates, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := outstationStack()
			var out bytes.Buffer
			if err := s.Receive(&out, bcastFrame(t, tc.fn, false, tc.fcv, nil), func(Received) {}); err != nil {
				t.Fatalf("receive: %v", err)
			}
			if out.Len() != 0 {
				t.Errorf("the outstation answered a broadcast %v; every station on the "+
					"line would transmit that reply at the same moment", tc.fn)
			}
		})
	}
}

// A broadcast RESET_LINK_STATES also drives the receive state machine: it
// marks the link reset and sets the frame count bit the next confirmed frame
// must carry, all from a frame that was never addressed to this station.
func TestBroadcastDoesNotAlterLinkState(t *testing.T) {
	s := outstationStack()

	var out bytes.Buffer
	if err := s.Receive(&out, bcastFrame(t, link.FuncResetLinkStates, false, false, nil), func(Received) {}); err != nil {
		t.Fatalf("receive: %v", err)
	}

	if s.sec.IsReset() {
		t.Error("a broadcast RESET_LINK_STATES reset this station's link state")
	}
}

// Confirmed user data is the one function that may carry a broadcast and
// still expects an answer in the unicast case. The payload is delivered, but
// the acknowledgement is suppressed: acknowledging would have every station
// answer at once, which is the thing broadcasts must not cause.
func TestBroadcastConfirmedUserDataIsDeliveredButNotAnswered(t *testing.T) {
	s := outstationStack()

	var out bytes.Buffer
	var got []Received
	raw := bcastFrame(t, link.FuncConfirmedUserData, false, true, []byte{0xC0, 0xC1, 0x01})
	if err := s.Receive(&out, raw, func(r Received) { got = append(got, r) }); err != nil {
		t.Fatalf("receive: %v", err)
	}

	if len(got) != 1 {
		t.Errorf("delivered %d fragments, want 1", len(got))
	}
	if out.Len() != 0 {
		t.Error("the outstation acknowledged a broadcast; every station on the line " +
			"would transmit that acknowledgement at the same moment")
	}
}

// A broadcast must not disturb the frame count bit either: that state belongs
// to the confirmed exchange with one station, and a broadcast is no part of
// it. If a broadcast advanced it, the next frame from the real peer would be
// judged against a bit somebody else moved — and silently dropped as a
// duplicate.
func TestBroadcastDoesNotDisturbTheFrameCountBit(t *testing.T) {
	s := outstationStack()

	unicast := func(t *testing.T, fn link.Function, fcb, fcv bool, payload []byte) []byte {
		t.Helper()
		raw, err := link.Encode(nil, link.Header{
			Control: link.Control{Prm: true, Func: fn, Fcb: fcb, Fcv: fcv},
			Dest:    10, Src: 1, Length: uint8(link.MinLength + len(payload)),
		}, payload)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		return raw
	}

	var out bytes.Buffer
	// Establish the link: the first confirmed frame is expected with FCB set.
	if err := s.Receive(&out, unicast(t, link.FuncResetLinkStates, false, false, nil), func(Received) {}); err != nil {
		t.Fatalf("receive (reset): %v", err)
	}

	// A broadcast arrives in between.
	if err := s.Receive(&out, bcastFrame(t, link.FuncConfirmedUserData, true, true, []byte{0xC0, 0x01}), func(Received) {}); err != nil {
		t.Fatalf("receive (broadcast): %v", err)
	}

	// The real peer's first confirmed frame, with the FCB the handshake
	// agreed on. It must still be accepted.
	var got []Received
	if err := s.Receive(&out, unicast(t, link.FuncConfirmedUserData, true, true, []byte{0xC0, 0xC1, 0x01}),
		func(r Received) { got = append(got, r) }); err != nil {
		t.Fatalf("receive (unicast data): %v", err)
	}

	if len(got) != 1 {
		t.Errorf("delivered %d fragments, want 1: the broadcast moved the expected frame "+
			"count bit, so the peer's next frame was taken for a duplicate and dropped",
			len(got))
	}
}

// What must keep working: unconfirmed user data is how a broadcast actually
// carries a request, and its payload has to reach the application.
func TestBroadcastUnconfirmedUserDataIsStillDelivered(t *testing.T) {
	s := outstationStack()

	var out bytes.Buffer
	var got []Received
	raw := bcastFrame(t, link.FuncUnconfirmedUserData, false, false, []byte{0xC0, 0xC1, 0x01})
	if err := s.Receive(&out, raw, func(r Received) { got = append(got, r) }); err != nil {
		t.Fatalf("receive: %v", err)
	}

	if len(got) != 1 {
		t.Fatalf("delivered %d fragments, want 1: a broadcast request must still reach "+
			"the application", len(got))
	}
	if !got[0].Broadcast {
		t.Error("the delivered fragment is not marked as having arrived by broadcast")
	}
	if out.Len() != 0 {
		t.Error("unconfirmed user data was answered at the link layer")
	}
}
