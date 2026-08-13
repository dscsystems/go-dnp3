// Package stack couples the link and transport layers to a byte stream.
//
// A master and an outstation differ entirely at the application layer and
// barely at all below it: both frame fragments, both reassemble them, both
// answer link-layer requests. That shared plumbing lives here so neither
// session has to reimplement it, and so the two cannot drift apart.
//
// A Stack is not safe for concurrent use, and is deliberately not made so.
// Sessions call it only from their own goroutine; their read goroutine hands
// raw octets across a channel rather than calling in directly. Locking the
// stack instead would make it possible to interleave a send with the
// processing of an inbound frame, and the link state machines are not
// interleavable — a frame count bit advanced from two places is a lost or
// duplicated fragment.
package stack

import (
	"fmt"
	"io"

	"github.com/dscsystems/go-dnp3/internal/link"
	"github.com/dscsystems/go-dnp3/internal/transport"
)

// Config parameterises a stack.
type Config struct {
	// LocalAddr and RemoteAddr are the link addresses of the two stations.
	LocalAddr  uint16
	RemoteAddr uint16
	// IsMaster sets the direction bit and selects which side answers what.
	IsMaster bool
	// UseConfirms enables link-layer confirmation. Over TCP this is normally
	// off, since the transport already guarantees ordered delivery; over
	// serial it is normally on.
	UseConfirms bool
	// MaxRetries is how many times a confirmed frame is retransmitted.
	MaxRetries int
	// MaxRxFragment caps a reassembled application fragment.
	MaxRxFragment int
}

// Stack owns the link and transport state for one connection.
//
// It performs no I/O of its own and reads no clock: Send writes through a
// caller-supplied writer, Receive is fed octets the caller has already read,
// and link timeouts are reported by the caller through OnTimeout. That keeps
// the blocking, the goroutines and the timers in the session, where they can
// be cancelled.
type Stack struct {
	cfg Config

	parser *link.Parser
	pri    link.Primary
	sec    link.Secondary
	seg    transport.Segmenter
	reasm  *transport.Reassembler

	// awaiting is set while a confirmed frame is unacknowledged, and dest is
	// the address the fragment in flight is going to.
	awaiting bool
	dest     uint16

	// txFrame and txSeg are reused across sends so a steady poll loop does
	// not allocate. They are single-goroutine state, like everything else
	// here.
	txFrame []byte
	txSeg   []byte
	// rxFrame is a separate buffer for link-layer replies, so answering an
	// inbound frame cannot overwrite a fragment mid-transmission.
	rxFrame []byte
}

// New returns a stack for one connection.
func New(cfg Config) *Stack {
	if cfg.MaxRxFragment <= 0 {
		cfg.MaxRxFragment = transport.DefaultMaxFragment
	}
	return &Stack{
		cfg:    cfg,
		parser: link.NewParser(),
		reasm:  transport.NewReassembler(cfg.MaxRxFragment),
		pri: link.Primary{
			LocalAddr:   cfg.LocalAddr,
			RemoteAddr:  cfg.RemoteAddr,
			IsMaster:    cfg.IsMaster,
			UseConfirms: cfg.UseConfirms,
			MaxRetries:  cfg.MaxRetries,
		},
		sec: link.Secondary{
			LocalAddr: cfg.LocalAddr,
			IsMaster:  cfg.IsMaster,
		},
		dest:    cfg.RemoteAddr,
		txFrame: make([]byte, 0, link.MaxFrameSize),
		txSeg:   make([]byte, 0, transport.MaxSegmentSize),
		rxFrame: make([]byte, 0, link.MaxFrameSize),
	}
}

// Reset clears all link and transport state, as when a connection is
// re-established. A fragment cannot span a connection and link state does not
// survive one.
func (s *Stack) Reset() {
	s.parser = link.NewParser()
	s.reasm.Reset()
	s.pri.Reset()
	s.sec.Reset()
	s.awaiting = false
	s.seg.Clear()
}

// Stats returns the link and transport counters.
func (s *Stack) Stats() (link.Stats, transport.Stats) {
	return s.parser.Stats(), s.reasm.Stats()
}

// Pending reports whether a confirmed frame is awaiting acknowledgement.
//
// A session arms its link timer while this is true and calls [Stack.OnTimeout]
// when it fires. With confirmations disabled it is never true.
func (s *Stack) Pending() bool { return s.awaiting }

// Busy reports whether a fragment is still being transmitted, either awaiting
// a confirmation or with segments still to send.
func (s *Stack) Busy() bool { return s.awaiting || s.seg.Pending() }

// Send begins transmitting an application fragment.
//
// With confirmations disabled every segment goes out immediately and the call
// completes the fragment. With them enabled only the first frame is sent;
// [Stack.Pending] then reports true until the peer acknowledges it, and the
// remaining segments follow as each acknowledgement arrives.
func (s *Stack) Send(w io.Writer, fragment []byte) error {
	return s.SendTo(w, s.cfg.RemoteAddr, fragment)
}

// SendTo frames a fragment addressed to a specific station, overriding the
// configured remote address.
//
// An outstation needs this: it must answer whichever master addressed it,
// which is not necessarily the one in its configuration, and a broadcast
// request must be answered to the real source rather than to the broadcast
// address it arrived on.
func (s *Stack) SendTo(w io.Writer, dest uint16, fragment []byte) error {
	if len(fragment) == 0 {
		return fmt.Errorf("stack: refusing to send an empty fragment")
	}
	if s.Busy() {
		return fmt.Errorf("stack: a fragment is already in flight")
	}

	s.dest = dest
	s.seg.Reset(fragment)
	return s.pump(w)
}

// pump sends segments until one needs acknowledging or the fragment is done.
func (s *Stack) pump(w io.Writer) error {
	for s.seg.Pending() {
		seg, ok := s.seg.Next(s.txSeg[:0])
		if !ok {
			break
		}
		s.txSeg = seg

		saved := s.pri.RemoteAddr
		s.pri.RemoteAddr = s.dest
		f, action, err := s.pri.Send(seg)
		s.pri.RemoteAddr = saved

		if err != nil {
			return fmt.Errorf("stack: link send: %w", err)
		}
		if action == link.ActionFailed {
			return fmt.Errorf("stack: link refused the segment")
		}
		if err := s.write(w, &s.txFrame, f); err != nil {
			return err
		}

		if action == link.ActionTransmit {
			// A confirmed frame: nothing more goes out until the peer answers.
			s.awaiting = true
			return nil
		}
	}
	s.awaiting = false
	return nil
}

// OnTimeout is called by the session when its link timer expires.
//
// It retransmits the unacknowledged frame, or gives up once the retry budget
// is spent. The returned bool reports whether the transmission failed for
// good, which the session surfaces as a failed request rather than a silent
// stall.
func (s *Stack) OnTimeout(w io.Writer) (failed bool, err error) {
	if !s.awaiting {
		return false, nil
	}

	f, action := s.pri.OnTimeout()
	switch action {
	case link.ActionTransmit:
		return false, s.write(w, &s.txFrame, f)
	case link.ActionFailed:
		s.awaiting = false
		s.seg.Clear()
		return true, nil
	default:
		return false, nil
	}
}

// Received is what one completed fragment looks like.
type Received struct {
	// Fragment is a completed application fragment. It aliases the stack's
	// reassembly buffer and is valid only for the duration of the callback.
	Fragment []byte
	// Source is the link address the fragment came from.
	Source uint16
	// Dest is the link address it was sent to, which may be a broadcast
	// address. An outstation must answer a broadcast without echoing it.
	Dest uint16
	// Broadcast reports whether Dest was a broadcast address.
	Broadcast bool
}

// Receive feeds received octets, calling fn for each completed application
// fragment.
//
// Link-layer replies — acknowledgements, link status — are written to w as
// they are produced, and an acknowledgement of our own confirmed frame
// releases the next segment.
func (s *Stack) Receive(w io.Writer, data []byte, fn func(Received)) error {
	for len(data) > 0 {
		n, werr := s.parser.Write(data)
		data = data[n:]

		if err := s.drain(w, fn); err != nil {
			return err
		}
		if werr != nil && n == 0 {
			return fmt.Errorf("stack: %w", werr)
		}
	}
	return nil
}

func (s *Stack) drain(w io.Writer, fn func(Received)) error {
	for {
		f, err := s.parser.Next()
		if err != nil {
			return nil // ErrNeedMore; nothing more to decode yet
		}

		if !s.addressedToUs(f.Header.Dest) {
			continue
		}

		if f.Header.Control.Prm {
			res := s.sec.OnFrame(f)
			if res.Reply != nil {
				// A separate buffer from the transmit path: answering an
				// inbound frame must not disturb a fragment in flight.
				if err := s.write(w, &s.rxFrame, *res.Reply); err != nil {
					return err
				}
			}
			if res.Payload != nil {
				s.deliver(f, res.Payload, fn)
			}
			continue
		}

		// A secondary frame is a reply to something we sent.
		next, action := s.pri.OnFrame(f)
		switch action {
		case link.ActionTransmit:
			if err := s.write(w, &s.txFrame, next); err != nil {
				return err
			}
		case link.ActionComplete:
			s.awaiting = false
			if err := s.pump(w); err != nil {
				return err
			}
		case link.ActionFailed:
			s.awaiting = false
			s.seg.Clear()
		}
	}
}

// deliver runs a segment through reassembly and reports any completed
// fragment.
func (s *Stack) deliver(f link.Frame, payload []byte, fn func(Received)) {
	res := s.reasm.Accept(payload)
	if !res.Complete() {
		return
	}
	fn(Received{
		Fragment:  res.Fragment,
		Source:    f.Header.Src,
		Dest:      f.Header.Dest,
		Broadcast: link.IsBroadcast(f.Header.Dest),
	})
}

// addressedToUs reports whether a frame is ours to process.
func (s *Stack) addressedToUs(dest uint16) bool {
	return dest == s.cfg.LocalAddr || link.IsBroadcast(dest)
}

// write encodes a frame into buf and sends it.
func (s *Stack) write(w io.Writer, buf *[]byte, f link.Frame) error {
	out, err := link.Encode((*buf)[:0], f.Header, f.Payload)
	if err != nil {
		return err
	}
	*buf = out
	_, err = w.Write(out)
	return err
}

// SendLinkStatusRequest sends a keep-alive.
//
// An idle TCP connection tells you nothing: a peer that has gone away, or a
// firewall that has quietly dropped the flow, looks exactly like a peer with
// nothing to report. Asking for link status is how a session finds out before
// the next poll is due.
func (s *Stack) SendLinkStatusRequest(w io.Writer) error {
	if s.Busy() {
		return nil // there is traffic in flight; the link is demonstrably alive
	}
	f, action, err := s.pri.RequestLinkStatus()
	if err != nil {
		return err
	}
	if action != link.ActionTransmit {
		return nil
	}
	s.awaiting = true
	return s.write(w, &s.txFrame, f)
}

// ReadChunk is the size a session's read goroutine should use.
const ReadChunk = link.MaxFrameSize
