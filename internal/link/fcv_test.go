package link

import "testing"

// Report: invalid link control combinations are processed. The control field's
// FCV bit says whether the frame count bit means anything, so the standard's
// control-code validity matrix fixes it per function code: set for the
// functions that use the FCB (TEST_LINK_STATES, CONFIRMED_USER_DATA), clear
// for those that do not (RESET_LINK_STATES, UNCONFIRMED_USER_DATA,
// REQUEST_LINK_STATUS). Secondary.OnFrame never reads Fcv at all.
//
// Confirmed user data with FCV clear is the consequential one. The sender has
// declared its frame count bit meaningless, and this secondary nonetheless
// compares that bit against expectFCB and decides what to do on the strength
// of it — accepting the payload, or dropping it as a duplicate while
// answering ACK. That last combination loses data silently: the peer is told
// its frame arrived and the payload never reaches the transport function.
func TestConfirmedUserDataRequiresFcv(t *testing.T) {
	s := &Secondary{LocalAddr: 10}
	s.OnFrame(primaryFrame(FuncResetLinkStates, false, false, 1, 10, nil))

	// FCB matches what the secondary expects, but FCV says to ignore it.
	res := s.OnFrame(primaryFrame(FuncConfirmedUserData, true, false, 1, 10, []byte{0xC0, 0x01}))

	if res.Payload != nil {
		t.Error("confirmed user data with FCV clear was accepted and its payload delivered; " +
			"the combination is not valid and the frame should have been discarded")
	}
}

// The same frame with FCB clear is the silent-loss case: the secondary
// compares a bit the sender declared meaningless, decides it is looking at a
// retransmission, and answers ACK while dropping the payload.
func TestConfirmedUserDataWithoutFcvDoesNotAckDiscardedData(t *testing.T) {
	s := &Secondary{LocalAddr: 10}
	s.OnFrame(primaryFrame(FuncResetLinkStates, false, false, 1, 10, nil))

	res := s.OnFrame(primaryFrame(FuncConfirmedUserData, false, false, 1, 10, []byte{0xC0, 0x01}))

	acked := res.Reply != nil && res.Reply.Header.Control.Func == FuncAck
	if acked && res.Payload == nil {
		t.Error("the payload was discarded and the frame acknowledged anyway: the peer is " +
			"told its data arrived when it never reached the transport function")
	}
}

// Unconfirmed user data carries no frame count bit, so FCV must be clear.
func TestUnconfirmedUserDataRejectsFcv(t *testing.T) {
	s := &Secondary{LocalAddr: 10}

	res := s.OnFrame(primaryFrame(FuncUnconfirmedUserData, false, true, 1, 10, []byte{0xC0, 0x01}))

	if res.Payload != nil {
		t.Error("unconfirmed user data with FCV set was accepted and its payload delivered; " +
			"the combination is not valid and the frame should have been discarded")
	}
}

// The rest of the matrix, which is the same omission: none of these functions
// uses the frame count bit, so all of them require FCV clear, and
// TEST_LINK_STATES does use it and so requires FCV set.
func TestOtherInvalidControlCombinationsAreDiscarded(t *testing.T) {
	tests := []struct {
		name  string
		frame Frame
	}{
		{"reset with FCV set", primaryFrame(FuncResetLinkStates, false, true, 1, 10, nil)},
		{"link status with FCV set", primaryFrame(FuncRequestLinkStatus, false, true, 1, 10, nil)},
		{"test link states with FCV clear", primaryFrame(FuncTestLinkStates, true, false, 1, 10, nil)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &Secondary{LocalAddr: 10}
			// Reset first so TEST_LINK_STATES gets past the unreset check and
			// the combination itself is what decides the outcome.
			s.OnFrame(primaryFrame(FuncResetLinkStates, false, false, 1, 10, nil))

			res := s.OnFrame(tc.frame)
			if res.Reply != nil || res.Payload != nil {
				t.Errorf("an invalid combination was processed rather than discarded: "+
					"reply = %v", res.Reply)
			}
		})
	}
}
