package stack

import (
	"bytes"
	"testing"

	"github.com/dscsystems/go-dnp3/internal/link"
)

// unicastFrom builds a primary frame addressed to this station (10) from an
// arbitrary source address.
func unicastFrom(t *testing.T, src uint16, fn link.Function, payload []byte) []byte {
	t.Helper()
	raw, err := link.Encode(nil, link.Header{
		Control: link.Control{Prm: true, Func: fn},
		Dest:    10, Src: src,
		Length: uint8(link.MinLength + len(payload)),
	}, payload)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return raw
}

// replyDest decodes the frame written to out and returns its destination.
func replyDest(t *testing.T, out *bytes.Buffer) (uint16, bool) {
	t.Helper()
	if out.Len() == 0 {
		return 0, false
	}
	p := link.NewParser()
	if _, err := p.Write(out.Bytes()); err != nil {
		t.Fatalf("parse reply: %v", err)
	}
	f, err := p.Next()
	if err != nil {
		t.Fatalf("decode reply: %v", err)
	}
	return f.Header.Dest, true
}

// Report: reserved source addresses are not rejected. The receive path filters
// on destination alone — addressedToUs looks at Dest, and the source check
// added for secondary frames guards only the primary exchange — so a request
// claiming a source in 0xFFF0-0xFFFF is processed like any other.
//
// The reply is what makes this more than a strictness gap. Secondary.reply
// addresses its answer to the source it was given, so a request claiming a
// broadcast source is answered with a frame addressed to the broadcast
// address: every station on the line sees a reply meant for none of them.
// That is the same collision hazard as answering a broadcast, reached by a
// route the destination check cannot see.
func TestReservedSourceAddressesAreRejected(t *testing.T) {
	tests := []struct {
		name string
		src  uint16
	}{
		{"reserved low", 0xFFF0},
		{"reserved high", 0xFFFB},
		{"self address", link.SelfAddress},
		{"broadcast optional confirm", link.BroadcastOptionalConfirm},
		{"broadcast no confirm", link.BroadcastNoConfirm},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := outstationStack()
			var out bytes.Buffer
			raw := unicastFrom(t, tc.src, link.FuncResetLinkStates, nil)
			if err := s.Receive(&out, raw, func(Received) {}); err != nil {
				t.Fatalf("receive: %v", err)
			}

			if dest, replied := replyDest(t, &out); replied {
				t.Errorf("a request claiming source %#04x was answered, with the reply "+
					"addressed to %#04x; that source cannot belong to any station",
					tc.src, dest)
			}
			if s.sec.IsReset() {
				t.Errorf("a request claiming source %#04x drove the link state machine", tc.src)
			}
		})
	}
}

// Payloads from an impossible source must not reach the application either.
func TestReservedSourceUserDataIsNotDelivered(t *testing.T) {
	s := outstationStack()

	var out bytes.Buffer
	var got []Received
	raw := unicastFrom(t, 0xFFF0, link.FuncUnconfirmedUserData, []byte{0xC0, 0xC1, 0x01})
	if err := s.Receive(&out, raw, func(r Received) { got = append(got, r) }); err != nil {
		t.Fatalf("receive: %v", err)
	}

	if len(got) != 0 {
		t.Errorf("delivered %d fragments from a reserved source address", len(got))
	}
}

// What must keep working: an ordinary request from a real station.
func TestOrdinarySourceAddressesStillWork(t *testing.T) {
	s := outstationStack()

	var out bytes.Buffer
	var got []Received
	if err := s.Receive(&out, unicastFrom(t, 1, link.FuncResetLinkStates, nil), func(Received) {}); err != nil {
		t.Fatalf("receive (reset): %v", err)
	}
	if dest, replied := replyDest(t, &out); !replied || dest != 1 {
		t.Fatalf("reset from station 1 was not acknowledged back to it (replied=%v dest=%#04x)",
			replied, dest)
	}

	raw := unicastFrom(t, 1, link.FuncUnconfirmedUserData, []byte{0xC0, 0xC1, 0x01})
	if err := s.Receive(&out, raw, func(r Received) { got = append(got, r) }); err != nil {
		t.Fatalf("receive (data): %v", err)
	}
	if len(got) != 1 {
		t.Errorf("delivered %d fragments from station 1, want 1", len(got))
	}
}
