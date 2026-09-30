package link

import "testing"

// LEN has to agree with the function: user data functions carry some, the
// rest carry none.
func TestFramesWhoseLengthContradictsTheirFunctionAreDiscarded(t *testing.T) {
	tests := []struct {
		name    string
		fn      Function
		fcb     bool
		fcv     bool
		payload []byte
	}{
		{"reset with user data", FuncResetLinkStates, false, false, make([]byte, 200)},
		{"test link with user data", FuncTestLinkStates, true, true, []byte{1}},
		{"request status with user data", FuncRequestLinkStatus, false, false, []byte{1}},
		{"confirmed user data with none", FuncConfirmedUserData, true, true, nil},
		{"unconfirmed user data with none", FuncUnconfirmedUserData, false, false, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &Secondary{LocalAddr: 10}
			s.OnFrame(primaryFrame(FuncResetLinkStates, false, false, 1, 10, nil))

			res := s.OnFrame(primaryFrame(tc.fn, tc.fcb, tc.fcv, 1, 10, tc.payload))

			if res.Reply != nil || res.Payload != nil || !res.Discarded {
				t.Errorf("reply=%v payload=%v discarded=%v, want the frame dropped unanswered",
					res.Reply != nil, res.Payload != nil, res.Discarded)
			}
		})
	}
}

// A secondary function carries no user data either, so the primary ignores a
// reply that does.
func TestPrimaryIgnoresRepliesCarryingUserData(t *testing.T) {
	p := &Primary{LocalAddr: 1, RemoteAddr: 10, UseConfirms: true, MaxRetries: 3}
	p.Send([]byte{0xC0})

	reply := secondaryFrame(FuncAck, 10, 1)
	reply.Payload = []byte{1, 2, 3}
	if _, action := p.OnFrame(reply); action != ActionNone {
		t.Errorf("action = %s, want none", action)
	}
	if !p.Busy() {
		t.Error("the malformed reply completed the exchange")
	}
}
