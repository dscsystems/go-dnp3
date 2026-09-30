package stack

import (
	"bytes"
	"testing"

	"github.com/dscsystems/go-dnp3/internal/link"
)

// P2 report: link-layer replies are accepted from any source. drain filters
// an inbound frame only by addressedToUs, which checks Dest (and broadcast)
// but never Src against the peer this stack is actually configured to talk
// to. A secondary-style reply — an ACK, say — forged or misrouted from some
// other station therefore still reaches Primary.OnFrame and is processed
// exactly as if the real peer had sent it, completing or advancing whatever
// exchange is in flight.
func TestReceiveIgnoresRepliesFromUnexpectedSource(t *testing.T) {
	s := New(Config{
		LocalAddr:   1,
		RemoteAddr:  10,
		IsMaster:    true,
		UseConfirms: true,
		MaxRetries:  3,
	})

	ackFrom := func(src uint16) []byte {
		raw, err := link.Encode(nil, link.Header{
			Control: link.Control{Prm: false, Func: link.FuncAck},
			Dest:    1, Src: src, Length: link.MinLength,
		}, nil)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		return raw
	}

	if err := s.Send(&bytes.Buffer{}, []byte{0xC0}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if !s.Busy() {
		t.Fatal("a confirmed send did not leave the stack awaiting an ACK; the test setup is broken")
	}

	// The real peer (10) completes the link-reset handshake this first send
	// triggers, which immediately queues the payload itself as confirmed user
	// data — still Busy(), now waiting on an ACK for that data frame.
	var out bytes.Buffer
	if err := s.Receive(&out, ackFrom(10), func(Received) {}); err != nil {
		t.Fatalf("receive (legitimate reset ack): %v", err)
	}
	if !s.Busy() {
		t.Fatal("the link-reset ack also completed the data send; the test setup is broken")
	}

	// An ACK addressed to us, but from station 99 — not RemoteAddr (10), the
	// station whose acknowledgement of the data frame we are actually
	// waiting on.
	if err := s.Receive(&out, ackFrom(99), func(Received) {}); err != nil {
		t.Fatalf("receive (forged ack): %v", err)
	}

	if !s.Busy() {
		t.Error("an ACK from an unexpected source (99, not the configured peer 10) completed " +
			"the in-flight send; the source address of a link-layer reply is not validated")
	}
}

// A frame whose DIR bit matches our own role did not come from the other end
// of the link, so it is not acted on: an outstation ignores a primary frame
// that claims to be from an outstation.
func TestFrameFromTheSameRoleIsIgnored(t *testing.T) {
	st := outstationStack()

	raw, err := link.Encode(nil, link.Header{
		Control: link.Control{Dir: false, Prm: true, Func: link.FuncResetLinkStates},
		Dest:    10, Src: 1,
		Length: link.MinLength,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := st.Receive(&out, raw, func(Received) {}); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Error("a frame with the wrong direction bit was answered")
	}
}

// A request sent to the self address is answered from the outstation's own, so
// the first valid reply names it. That is the only time a reply from an address
// other than the one sent to is accepted, and once the station has named
// itself it is held to that address like any other.
func TestSelfAddressDiscoveryAdoptsTheFirstValidReplierOnly(t *testing.T) {
	newStack := func() *Stack {
		s := New(Config{LocalAddr: 1, RemoteAddr: link.SelfAddress, IsMaster: true, UseConfirms: true, MaxRetries: 3})
		if err := s.Send(&bytes.Buffer{}, []byte{0xC0}); err != nil {
			t.Fatalf("send: %v", err)
		}
		return s
	}
	ackFrom := func(src uint16) []byte {
		raw, err := link.Encode(nil, link.Header{
			Control: link.Control{Prm: false, Func: link.FuncAck},
			Dest:    1, Src: src, Length: link.MinLength,
		}, nil)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		return raw
	}
	recv := func(s *Stack, src uint16) {
		t.Helper()
		if err := s.Receive(&bytes.Buffer{}, ackFrom(src), func(Received) {}); err != nil {
			t.Fatalf("receive: %v", err)
		}
	}

	// An address no station may hold cannot name itself.
	s := newStack()
	if s.dest != link.SelfAddress {
		t.Fatalf("dest = %#x after sending to the self address", s.dest)
	}
	recv(s, link.BroadcastNoConfirm)
	if s.dest != link.SelfAddress {
		t.Errorf("a reply from a reserved address was adopted: dest = %#x", s.dest)
	}

	// A real station answers and is adopted; nobody else is accepted after it.
	recv(s, 10)
	if s.dest != 10 {
		t.Fatalf("dest = %d after a reply from 10, want 10", s.dest)
	}
	busy := s.Busy()
	recv(s, 99)
	if s.dest != 10 {
		t.Errorf("a later reply from 99 replaced the adopted station: dest = %d", s.dest)
	}
	if s.Busy() != busy {
		t.Error("a reply from a station other than the adopted one advanced the exchange")
	}

	// Discovery is for the self address alone: an ordinary destination is
	// never rewritten by a reply from somewhere else.
	plain := New(Config{LocalAddr: 1, RemoteAddr: 10, IsMaster: true, UseConfirms: true, MaxRetries: 3})
	_ = plain.Send(&bytes.Buffer{}, []byte{0xC0})
	recv(plain, 99)
	if plain.dest != 10 {
		t.Errorf("an ordinary destination was rewritten by a reply from 99: dest = %d", plain.dest)
	}
}
