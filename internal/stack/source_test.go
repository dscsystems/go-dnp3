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
