package link

import (
	"errors"
	"fmt"
)

// ErrDataFlowControl means the peer's last reply signalled DFC — its
// buffers are full — so Send refuses to queue more user data until a later
// reply clears it.
var ErrDataFlowControl = errors.New("link: peer has signalled data flow control")

// Action tells a session what the [Primary] state machine wants next.
type Action uint8

// Actions returned by the Primary state machine.
const (
	// ActionNone means nothing to do; the frame was not for this half, or was
	// absorbed without changing state.
	ActionNone Action = iota
	// ActionTransmit means transmit the returned frame and arm the response
	// timer. The session must call OnTimeout if the timer expires.
	ActionTransmit
	// ActionComplete means the queued payload was delivered and acknowledged.
	// Any pending timer should be cancelled.
	ActionComplete
	// ActionFailed means the transmission failed after exhausting retries.
	// The link is left unreset so the next send re-establishes it.
	ActionFailed
)

func (a Action) String() string {
	switch a {
	case ActionNone:
		return "none"
	case ActionTransmit:
		return "transmit"
	case ActionComplete:
		return "complete"
	case ActionFailed:
		return "failed"
	default:
		return "Action(?)"
	}
}

// Primary is the transmitting half of the link layer.
//
// With confirmations enabled it runs the handshake the standard requires:
// reset the link, then send each frame with an alternating frame count bit and
// wait for an ACK, retransmitting on timeout. With confirmations disabled it
// is a thin pass-through, which is the normal configuration over TCP where the
// transport already guarantees ordered delivery.
//
// It owns no timer. The session arms one when Primary returns ActionTransmit
// and calls OnTimeout when it fires.
type Primary struct {
	// LocalAddr and RemoteAddr are the link addresses of the two stations.
	LocalAddr  uint16
	RemoteAddr uint16
	// IsMaster sets the DIR bit on transmitted frames.
	IsMaster bool
	// UseConfirms enables the confirmed handshake. Over TCP this is normally
	// false; over serial it is normally true.
	UseConfirms bool
	// MaxRetries is how many times a frame is retransmitted after a timeout
	// before the transmission fails.
	MaxRetries int

	state    priState
	linkUp   bool
	fcb      bool
	retries  int
	pending  []byte
	lastSent Frame
	dfc      bool
}

type priState uint8

const (
	priIdle priState = iota
	priWaitLinkReset
	priWaitConfirm
	priWaitStatus
)

func (s priState) String() string {
	switch s {
	case priIdle:
		return "idle"
	case priWaitLinkReset:
		return "wait-link-reset"
	case priWaitConfirm:
		return "wait-confirm"
	case priWaitStatus:
		return "wait-status"
	default:
		return "priState(?)"
	}
}

// Reset returns the primary to its initial state and drops any queued payload.
// A session calls this when the connection is re-established.
func (p *Primary) Reset() {
	p.state = priIdle
	p.linkUp = false
	p.fcb = false
	p.retries = 0
	p.pending = nil
	p.dfc = false
}

// Busy reports whether a transmission is in flight. Callers must not call Send
// while it is true.
func (p *Primary) Busy() bool { return p.state != priIdle }

// LinkUp reports whether the link has been reset and confirmed.
func (p *Primary) LinkUp() bool { return p.linkUp }

// DataFlowControl reports whether the peer last signalled DFC, meaning its
// buffers are full and user data must not be sent until it clears.
func (p *Primary) DataFlowControl() bool { return p.dfc }

// Send queues payload for transmission and returns the first frame to put on
// the wire.
//
// When confirmations are disabled the returned action is ActionComplete
// alongside the frame: transmit it and consider the send finished. When they
// are enabled the action is ActionTransmit, and the send is not finished until
// a later call returns ActionComplete or ActionFailed.
func (p *Primary) Send(payload []byte) (Frame, Action, error) {
	if len(payload) > MaxPayload {
		return Frame{}, ActionFailed, fmt.Errorf("%w: %d octets", ErrPayloadTooLong, len(payload))
	}
	if p.Busy() {
		return Frame{}, ActionFailed, fmt.Errorf("link: primary busy in state %s", p.state)
	}
	if p.dfc {
		// The peer's last reply said its buffers are full; sending more user
		// data now would be exactly what data flow control exists to
		// prevent. Stack.pump checks DataFlowControl itself before ever
		// reaching here for a queued continuation segment, so this only
		// turns away a caller starting something new while paused.
		return Frame{}, ActionFailed, ErrDataFlowControl
	}

	p.retries = 0

	if !p.UseConfirms {
		f := p.frame(FuncUnconfirmedUserData, false, false, payload)
		p.lastSent = f
		return f, ActionComplete, nil
	}

	p.pending = payload
	if !p.linkUp {
		p.state = priWaitLinkReset
		f := p.frame(FuncResetLinkStates, false, false, nil)
		p.lastSent = f
		return f, ActionTransmit, nil
	}
	return p.sendPending()
}

// RequestLinkStatus builds a keep-alive frame. The session sends it on an idle
// link to detect a peer that has gone away without closing the connection.
func (p *Primary) RequestLinkStatus() (Frame, Action, error) {
	if p.Busy() {
		return Frame{}, ActionNone, fmt.Errorf("link: primary busy in state %s", p.state)
	}
	p.state = priWaitStatus
	p.retries = 0
	f := p.frame(FuncRequestLinkStatus, false, false, nil)
	p.lastSent = f
	return f, ActionTransmit, nil
}

// sendPending emits the queued payload as confirmed user data.
func (p *Primary) sendPending() (Frame, Action, error) {
	p.state = priWaitConfirm
	f := p.frame(FuncConfirmedUserData, p.fcb, true, p.pending)
	p.lastSent = f
	return f, ActionTransmit, nil
}

// OnFrame processes a reply from the peer's secondary station.
//
// Frames that are primary messages are ignored: those belong to the
// [Secondary] half and are routed there by the session.
func (p *Primary) OnFrame(f Frame) (Frame, Action) {
	if f.Header.Control.Prm {
		return Frame{}, ActionNone
	}
	p.dfc = f.Header.Control.Dfc()

	switch p.state {

	case priWaitLinkReset:
		switch f.Header.Control.Func {
		case FuncAck:
			// The peer's secondary has reset. Its expected frame count bit is
			// now 1, so ours must start there too.
			p.linkUp = true
			p.fcb = true
			p.retries = 0
			next, action, err := p.sendPending()
			if err != nil {
				return Frame{}, ActionFailed
			}
			return next, action
		case FuncNack, FuncNotSupported:
			return p.fail()
		default:
			return Frame{}, ActionNone
		}

	case priWaitConfirm:
		switch f.Header.Control.Func {
		case FuncAck:
			p.fcb = !p.fcb
			p.state = priIdle
			p.pending = nil
			p.retries = 0
			return Frame{}, ActionComplete
		case FuncNack:
			// The peer says its link is not reset. Start the handshake again
			// rather than retrying the data frame, which would only be NACKed
			// once more.
			p.linkUp = false
			p.state = priWaitLinkReset
			p.retries = 0
			next := p.frame(FuncResetLinkStates, false, false, nil)
			p.lastSent = next
			return next, ActionTransmit
		case FuncNotSupported:
			return p.fail()
		default:
			return Frame{}, ActionNone
		}

	case priWaitStatus:
		switch f.Header.Control.Func {
		case FuncLinkStatus, FuncAck:
			p.state = priIdle
			p.retries = 0
			return Frame{}, ActionComplete
		case FuncNack, FuncNotSupported:
			return p.fail()
		default:
			return Frame{}, ActionNone
		}

	default:
		return Frame{}, ActionNone
	}
}

// OnTimeout is called by the session when the response timer expires. It
// retransmits the last frame until MaxRetries is exhausted.
func (p *Primary) OnTimeout() (Frame, Action) {
	if p.state == priIdle {
		return Frame{}, ActionNone
	}
	if p.retries >= p.MaxRetries {
		return p.fail()
	}
	p.retries++
	// Retransmit verbatim: the frame count bit must not advance, or the peer
	// will treat the retry as new data.
	return p.lastSent, ActionTransmit
}

// Retries returns how many times the in-flight frame has been retransmitted.
func (p *Primary) Retries() int { return p.retries }

// fail abandons the transmission and tears down link state, so the next Send
// re-runs the reset handshake.
func (p *Primary) fail() (Frame, Action) {
	p.state = priIdle
	p.linkUp = false
	p.pending = nil
	p.retries = 0
	return Frame{}, ActionFailed
}

func (p *Primary) frame(fn Function, fcb, fcv bool, payload []byte) Frame {
	return Frame{
		Header: Header{
			Control: Control{
				Dir:  p.IsMaster,
				Prm:  true,
				Fcb:  fcb,
				Fcv:  fcv,
				Func: fn,
			},
			Dest:   p.RemoteAddr,
			Src:    p.LocalAddr,
			Length: uint8(MinLength + len(payload)),
		},
		Payload: payload,
	}
}
