package link

// Secondary is the receiving half of the link layer: the side that answers
// RESET_LINK_STATES, validates the frame count bit on confirmed user data, and
// hands accepted payloads up to the transport function.
//
// It holds no timers and performs no I/O. Feed it frames, transmit whatever
// reply it returns, and pass whatever payload it accepts upward.
type Secondary struct {
	// LocalAddr is this station's link address, used as the source of replies.
	LocalAddr uint16
	// IsMaster sets the DIR bit on replies.
	IsMaster bool

	reset     bool
	expectFCB bool
}

// SecResult is what a Secondary wants done with a received frame.
type SecResult struct {
	// Reply is a frame to transmit, or nil when the frame needs no answer.
	Reply *Frame
	// Payload is user data to pass to the transport function, or nil. It
	// aliases the input frame's payload.
	Payload []byte
	// Discarded is set when a received frame was dropped. The usual cause is a
	// frame count bit mismatch on user data, meaning the peer retransmitted
	// something already accepted, which is correct behaviour rather than an
	// error — but worth counting. It is also set for a frame whose control
	// field is not a valid combination.
	Discarded bool
}

// validControl reports whether a primary frame's control field is one of the
// combinations the standard's control-code validity matrix allows.
//
// FCV says whether the frame count bit carries meaning, so it is fixed by the
// function code: set for the two functions that use the FCB, clear for those
// that do not. A frame whose FCV contradicts its function code is not safe to
// act on — most sharply for confirmed user data, where the FCB is what
// decides whether a payload is new or a duplicate of one already delivered.
// Acting on a bit the sender has declared meaningless can drop live data and
// acknowledge it as received.
func validControl(c Control) bool {
	switch c.Func {
	case FuncTestLinkStates, FuncConfirmedUserData:
		return c.Fcv
	case FuncResetLinkStates, FuncUnconfirmedUserData, FuncRequestLinkStatus:
		return !c.Fcv
	default:
		// Not an FCV question: an unrecognised function code is answered with
		// NOT_SUPPORTED rather than discarded.
		return true
	}
}

// Reset returns the secondary to its unreset state. A session calls this when
// the underlying connection is re-established, because link state does not
// survive a socket.
func (s *Secondary) Reset() {
	s.reset = false
	s.expectFCB = false
}

// IsReset reports whether the link has been reset by the peer, which is the
// precondition for accepting confirmed user data.
func (s *Secondary) IsReset() bool { return s.reset }

// OnFrame processes a frame addressed to this station.
//
// Frames that are not primary messages are ignored — those are replies meant
// for the [Primary] half and are routed there by the session.
func (s *Secondary) OnFrame(f Frame) SecResult {
	if !f.Header.Control.Prm {
		return SecResult{}
	}

	// A control field that is not a valid combination is discarded outright,
	// without a reply: answering it would confirm a frame we are refusing to
	// act on, and the peer's own timeout is what should tell it something is
	// wrong.
	if !validControl(f.Header.Control) {
		return SecResult{Discarded: true}
	}

	src := f.Header.Src
	switch f.Header.Control.Func {

	case FuncResetLinkStates:
		// The peer is establishing the link. Its first confirmed frame will
		// carry FCB=1, so that is what we expect next.
		s.reset = true
		s.expectFCB = true
		return SecResult{Reply: s.reply(FuncAck, src)}

	case FuncTestLinkStates:
		if !s.reset {
			return SecResult{Reply: s.reply(FuncNack, src)}
		}
		if f.Header.Control.Fcb != s.expectFCB {
			return SecResult{Reply: s.reply(FuncNack, src)}
		}
		s.expectFCB = !s.expectFCB
		return SecResult{Reply: s.reply(FuncAck, src)}

	case FuncConfirmedUserData:
		if !s.reset {
			// Confirmed data before a reset is a protocol error on the peer's
			// side. NACK tells it to reset the link and start over.
			return SecResult{Reply: s.reply(FuncNack, src), Discarded: true}
		}
		if f.Header.Control.Fcb != s.expectFCB {
			// A retransmission of a frame we already accepted: our ACK was
			// lost, not the data. Re-ACK and drop the duplicate payload,
			// without touching the expected FCB.
			return SecResult{Reply: s.reply(FuncAck, src), Discarded: true}
		}
		s.expectFCB = !s.expectFCB
		return SecResult{Reply: s.reply(FuncAck, src), Payload: f.Payload}

	case FuncUnconfirmedUserData:
		// No link-layer handshake, no frame count bit, no reply.
		return SecResult{Payload: f.Payload}

	case FuncRequestLinkStatus:
		return SecResult{Reply: s.reply(FuncLinkStatus, src)}

	default:
		return SecResult{Reply: s.reply(FuncNotSupported, src)}
	}
}

// reply builds a secondary-to-primary frame back to dest.
func (s *Secondary) reply(fn Function, dest uint16) *Frame {
	return &Frame{
		Header: Header{
			Control: Control{
				Dir:  s.IsMaster,
				Prm:  false,
				Func: fn,
				// DFC is left clear: this implementation never asks a peer to
				// stop sending, because the transport function above it always
				// has somewhere to put a frame.
			},
			Dest:   dest,
			Src:    s.LocalAddr,
			Length: MinLength,
		},
	}
}
