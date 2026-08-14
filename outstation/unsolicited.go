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

	// The events go back in the queue whether or not we retry. If we give up,
	// the master's next poll collects them — losing them because unsolicited
	// delivery failed would defeat the point of the confirmation.
	requeued := s.db.events.Unselect()

	if s.unsol.retries > s.cfg.Unsolicited.MaxRetries {
		s.log.Warn("giving up on unsolicited reporting until the master polls",
			"retries", s.unsol.retries, "events_requeued", requeued)
		s.unsol.retries = 0
		s.unsol.nextAllowed = now.Add(s.cfg.Unsolicited.ConfirmTimeout)
		return nil
	}

	s.log.Debug("unsolicited response unconfirmed; retrying",
		"attempt", s.unsol.retries, "events_requeued", requeued)
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

	if !s.unsol.nullConfirmed {
		// The master has acknowledged our existence; data may now flow.
		s.unsol.nullConfirmed = true
		s.log.Debug("null unsolicited response confirmed")
		return
	}

	n := s.db.events.Confirm()
	s.log.Debug("unsolicited events confirmed", "count", n)
}
