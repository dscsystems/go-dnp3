package outstation

import (
	"io"
	"time"

	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/objects"
)

// unsolState is the outstation's unsolicited reporting state.
//
// Unsolicited responses have their own sequence space, separate from the
// solicited one, and their own confirmation: an outstation that mixed the two
// would confirm a poll with an event acknowledgement and drop data.
type unsolState struct {
	// seq is the next unsolicited sequence number to use.
	seq uint8

	// nullSent records that the initial null unsolicited response has been
	// sent, and nullConfirmed that the master answered it.
	nullSent      bool
	nullConfirmed bool

	// awaiting is set while a response is unconfirmed, with its sequence
	// number and deadline.
	awaiting bool
	awaitSeq uint8
	deadline time.Time

	// retries counts consecutive unconfirmed attempts.
	retries int

	// lastFrag is the exact fragment last transmitted, kept so a retry can
	// repeat it octet for octet. A master tells a retransmission apart from
	// new data purely by the sequence number, so a retry that keeps the
	// sequence number — as it must — has to carry the same data too, or the
	// difference is discarded as a duplicate and never delivered.
	lastFrag []byte

	// nextAllowed is the earliest a further unsolicited response may be sent,
	// which is how a device that has given up retrying backs off.
	nextAllowed time.Time

	// firstEventAt is when the oldest unreported event appeared, which starts
	// the hold-time clock. It is zero when nothing is waiting.
	firstEventAt time.Time
}

// reset returns the state to what it is after a restart.
func (u *unsolState) reset() {
	*u = unsolState{}
}

// UnsolicitedConfig paces unsolicited reporting.
type UnsolicitedConfig struct {
	// Enabled allows the outstation to send unsolicited responses at all.
	// The master still has to enable the individual classes with
	// ENABLE_UNSOLICITED; this is the device-level switch that says the
	// outstation is capable of it.
	Enabled bool

	// HoldTime is how long to wait after an event before transmitting, so a
	// burst of changes becomes one response rather than twenty. Zero sends as
	// soon as an event appears.
	HoldTime time.Duration

	// MaxEvents transmits as soon as this many events are queued, regardless
	// of the hold time. Zero means no threshold.
	MaxEvents int

	// ConfirmTimeout is how long to wait for the master's confirmation before
	// retrying.
	ConfirmTimeout time.Duration

	// MaxRetries is how many times an unconfirmed response is re-sent before
	// the outstation gives up and waits for the master to poll instead.
	MaxRetries int
}

func (c *UnsolicitedConfig) applyDefaults() {
	if c.ConfirmTimeout <= 0 {
		c.ConfirmTimeout = 5 * time.Second
	}
	if c.MaxRetries <= 0 {
		c.MaxRetries = 3
	}
}

// pollUnsolicited decides whether to transmit an unsolicited response and does
// so if the moment is right.
//
// It runs on the session's tick, which is what lets the hold time and the
// confirm timeout be enforced without a timer per event.
func (s *Session) pollUnsolicited(w io.Writer, now time.Time) error {
	if !s.cfg.Unsolicited.Enabled || !s.connected {
		return nil
	}

	// An unconfirmed response is either still in its window or has run out.
	if s.unsol.awaiting {
		if now.Before(s.unsol.deadline) {
			return nil
		}
		return s.onUnsolicitedTimeout(w, now)
	}

	// A retry repeats the fragment the master did not confirm, exactly as it
	// went out the first time. Building a fresh one from whatever is queued
	// now would put events the master has never seen behind the sequence
	// number of a transmission it has already seen, so it would match them as
	// a duplicate, deliver none of them, and confirm them away regardless.
	if s.unsol.retries > 0 && len(s.unsol.lastFrag) > 0 {
		return s.retryUnsolicited(w, now)
	}

	// The null unsolicited response comes first and comes before any data.
	//
	// Its job is to tell a master that has just connected — or reconnected to
	// an outstation that restarted — that this outstation exists and is
	// asserting DEVICE_RESTART, without gambling event data on a session the
	// master may not be ready for.
	if !s.unsol.nullConfirmed {
		if !s.unsol.nullSent || now.After(s.unsol.nextAllowed) {
			return s.sendUnsolicited(w, nil, now, true)
		}
		return nil
	}

	if s.unsolClasses == 0 || now.Before(s.unsol.nextAllowed) {
		return nil
	}

	pending := s.db.events.Count(s.unsolClasses)
	if pending == 0 {
		return nil
	}

	// Hold briefly so a burst of changes becomes one response, unless enough
	// events have piled up that waiting no longer helps.
	if s.cfg.Unsolicited.HoldTime > 0 && s.unsol.firstEventAt.IsZero() {
		s.unsol.firstEventAt = now
	}
	if s.cfg.Unsolicited.HoldTime > 0 {
		enough := s.cfg.Unsolicited.MaxEvents > 0 && pending >= s.cfg.Unsolicited.MaxEvents
		if !enough && now.Sub(s.unsol.firstEventAt) < s.cfg.Unsolicited.HoldTime {
			return nil
		}
	}
	s.unsol.firstEventAt = time.Time{}

	events := s.db.events.Select(s.unsolClasses, 64)
	if len(events) == 0 {
		return nil
	}
	return s.sendUnsolicited(w, events, now, false)
}

// onUnsolicitedTimeout retries or gives up on an unconfirmed response.
func (s *Session) onUnsolicitedTimeout(w io.Writer, now time.Time) error {
	s.unsol.awaiting = false
	s.unsol.retries++
	s.bump(func(st *Stats) { st.UnsolicitedTimeouts++ })

	if s.unsol.retries > s.cfg.Unsolicited.MaxRetries {
		// Only now do the events go back in the queue, for the master's next
		// poll to collect: losing them because unsolicited delivery failed
		// would defeat the point of the confirmation. Returning them any
		// earlier, while a retry still owes them, would let the retry sweep
		// up whatever else has arrived since and send the lot under the
		// original sequence number.
		requeued := s.db.events.Unselect()
		s.log.Warn("giving up on unsolicited reporting until the master polls",
			"retries", s.unsol.retries, "events_requeued", requeued)
		s.unsol.retries = 0
		s.unsol.lastFrag = nil
		s.unsol.nextAllowed = now.Add(s.cfg.Unsolicited.ConfirmTimeout)
		return nil
	}

	s.log.Debug("unsolicited response unconfirmed; retrying",
		"attempt", s.unsol.retries)
	return nil
}

// retryUnsolicited retransmits the unconfirmed response verbatim.
func (s *Session) retryUnsolicited(w io.Writer, now time.Time) error {
	if err := s.stack.Send(w, s.unsol.lastFrag); err != nil {
		// The events stay selected; the retry path keeps hold of them until
		// the transmission is either confirmed or given up on.
		return err
	}

	s.unsol.awaiting = true
	s.unsol.awaitSeq = s.unsol.seq
	s.unsol.deadline = now.Add(s.cfg.Unsolicited.ConfirmTimeout)

	s.bump(func(st *Stats) { st.UnsolicitedSent++ })
	s.log.Debug("unsolicited response retransmitted",
		"seq", s.unsol.seq, "attempt", s.unsol.retries)
	return nil
}

// sendUnsolicited transmits one unsolicited response.
func (s *Session) sendUnsolicited(w io.Writer, events []Event, now time.Time, null bool) error {
	ctx := objects.Context{Synchronized: s.synchronized}
	b := newResponseBuilder(s.cfg.MaxTxFragment, ctx)
	if !null {
		s.buildEvents(b, events)
	}
	bodies := b.done()

	// An unsolicited response is a single fragment. If the events do not fit,
	// the rest stay queued for the next one rather than being split across a
	// series the master would have to reassemble without having asked for it.
	body := bodies[0]

	// Every transmission through here is a new one, carrying data the master
	// has not been offered before, so it takes the next sequence number. A
	// retry does not come through here at all: it goes through
	// retryUnsolicited, which repeats the stored fragment under the sequence
	// number it already had, because that is the only thing a master's
	// duplicate detection matches on.
	s.unsol.seq = (s.unsol.seq + 1) % app.SeqModulus
	frag := app.AppendHeader(nil, app.Header{
		Control: app.Control{
			Fir: true, Fin: true, Con: true, Uns: true,
			Seq: s.unsol.seq,
		},
		Func: app.FuncUnsolicitedResponse,
		IIN:  s.currentIIN(),
	})
	frag = append(frag, body...)

	if err := s.stack.Send(w, frag); err != nil {
		// The events are still selected; the retry path requeues them.
		return err
	}

	s.unsol.awaiting = true
	s.unsol.awaitSeq = s.unsol.seq
	s.unsol.deadline = now.Add(s.cfg.Unsolicited.ConfirmTimeout)
	s.unsol.nullSent = s.unsol.nullSent || null
	// Kept whole for a retry to repeat. frag is freshly allocated here and
	// the body is copied into it, so holding the reference is enough.
	s.unsol.lastFrag = frag

	s.bump(func(st *Stats) { st.UnsolicitedSent++ })
	s.log.Debug("unsolicited response sent",
		"seq", s.unsol.seq, "events", len(events), "null", null)
	return nil
}

// onUnsolicitedConfirm handles a confirmation of an unsolicited response.
func (s *Session) onUnsolicitedConfirm(h app.Header) {
	if !s.unsol.awaiting || h.Control.Seq != s.unsol.awaitSeq {
		s.log.Debug("unexpected unsolicited confirm",
			"seq", h.Control.Seq, "awaiting", s.unsol.awaiting)
		return
	}

	s.unsol.awaiting = false
	s.unsol.retries = 0
	s.unsol.lastFrag = nil

	if !s.unsol.nullConfirmed {
		// The master has acknowledged our existence; data may now flow.
		s.unsol.nullConfirmed = true
		s.log.Debug("null unsolicited response confirmed")
		return
	}

	n := s.db.events.Confirm()
	s.log.Debug("unsolicited events confirmed", "count", n)
}
