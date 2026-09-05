package outstation

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/channel"
	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/internal/stack"
	"github.com/dscsystems/go-dnp3/objects"
)

// Config parameterises an outstation session.
type Config struct {
	// LocalAddr is this outstation's link address; RemoteAddr is the master's.
	LocalAddr  uint16
	RemoteAddr uint16

	// Database sizes the point database.
	Database DatabaseConfig
	// Events sizes the event buffer.
	Events EventBufferConfig

	// MaxTxFragment caps a response fragment. Zero uses the standard's 2048.
	MaxTxFragment int
	// MaxRxFragment caps a received request fragment.
	MaxRxFragment int

	// ConfirmTimeout is how long to wait for an application confirmation
	// before returning the selected events to the queue for re-sending.
	ConfirmTimeout time.Duration

	// SelectTimeout is how long a select-before-operate reservation stays
	// valid. Zero uses five seconds, which is the conventional default.
	SelectTimeout time.Duration

	// Unsolicited paces unsolicited reporting.
	Unsolicited UnsolicitedConfig

	// Files parameterises file transfer. Without a handler the outstation
	// answers the file function codes the way a device that has no files does.
	Files FileConfig

	// Attributes are what the device says about itself over group 0: vendor,
	// model, serial number, firmware version. The point counts and fragment
	// sizes are derived from this configuration and need not be listed; an
	// entry here overrides a derived one.
	Attributes []dnp3.Attribute

	// UseLinkConfirms enables link-layer confirmation, normally off over TCP.
	UseLinkConfirms bool
	// LinkRetries is how many times a confirmed frame is retransmitted.
	LinkRetries int
	// LinkTimeout is how long to wait for a link-layer acknowledgement before
	// retransmitting. It matters only when UseLinkConfirms is set.
	LinkTimeout time.Duration

	// Log receives protocol and session events. Nil discards them.
	Log *slog.Logger
}

func (c *Config) applyDefaults() {
	if c.MaxTxFragment <= 0 {
		c.MaxTxFragment = app.DefaultMaxFragment
	}
	if c.MaxRxFragment <= 0 {
		c.MaxRxFragment = app.DefaultMaxFragment
	}
	if c.ConfirmTimeout <= 0 {
		c.ConfirmTimeout = 5 * time.Second
	}
	if c.SelectTimeout <= 0 {
		c.SelectTimeout = 5 * time.Second
	}
	if c.LinkTimeout <= 0 {
		c.LinkTimeout = time.Second
	}
	c.Unsolicited.applyDefaults()
	c.Files.applyDefaults()
	if c.Log == nil {
		c.Log = slog.New(slog.DiscardHandler)
	}
}

// Application is the hook an outstation implementation provides for the
// behaviour the stack cannot decide on its own.
//
// Every method has a usable default in [NopApplication], so an embedder
// implements only what it cares about.
type Application interface {
	// Now returns the outstation's idea of the current time. Tests supply a
	// virtual clock through this.
	Now() time.Time
	// WriteAbsoluteTime is called when a master sets the clock. Returning
	// false rejects the request.
	WriteAbsoluteTime(t time.Time) bool
	// ColdRestart and WarmRestart are called for the restart function codes.
	// The returned duration is how long the outstation expects to be
	// unavailable, reported back in a group 52 time delay.
	ColdRestart() time.Duration
	WarmRestart() time.Duration
	// SupportsWriteTime reports whether the outstation accepts clock writes.
	SupportsWriteTime() bool
}

// NopApplication is an Application with sensible defaults.
type NopApplication struct{}

func (NopApplication) Now() time.Time                   { return time.Now() }
func (NopApplication) WriteAbsoluteTime(time.Time) bool { return true }
func (NopApplication) ColdRestart() time.Duration       { return 0 }
func (NopApplication) WarmRestart() time.Duration       { return 0 }
func (NopApplication) SupportsWriteTime() bool          { return true }

// Session is an outstation.
//
// All protocol state lives in the session goroutine started by [Session.Run].
// Database updates arrive through [Session.Update], which is safe to call from
// anywhere.
type Session struct {
	cfg   Config
	appl  Application
	db    *Database
	stack *stack.Stack
	log   *slog.Logger

	// iin holds the internal indications the outstation reports. It is
	// touched only from the session goroutine.
	iin app.IIN

	// synchronized tracks whether the master has set our clock, which decides
	// the quality stamped on the timestamps we report.
	synchronized bool

	// unsolClasses is the mask of classes the master has enabled for
	// unsolicited reporting. Unsolicited transmission itself is not
	// implemented yet; the flag is tracked so the enable and disable requests
	// are answered truthfully rather than silently ignored.
	unsolClasses dnp3.Class

	// pendingConfirm is the sequence number of a response awaiting an
	// application confirmation, and confirmDeadline when it expires.
	awaitingConfirm bool
	confirmSeq      uint8
	confirmDeadline time.Time

	// pendingBodies are the fragments still to send for a response that spans
	// more than one, and pendingIndex is the next to go out. Only one is ever
	// truly in flight at a time, on purpose: every fragment in the response
	// shares the request's own sequence number, the only field a confirm is
	// matched against, so a confirm for the first fragment cannot be told
	// apart from one for a later one unless the outstation never has more
	// than one outstanding. pendingDest and pendingSeq are constant across the
	// response; pendingHasEvents says whether it carries events at all, which
	// decides whether the last fragment needs a confirmation of its own.
	pendingBodies    [][]byte
	pendingIndex     int
	pendingDest      uint16
	pendingSeq       uint8
	pendingHasEvents bool

	// lastReq is the request we last acted on, and lastResp what we answered
	// it with. A master retransmits a request whenever it does not see the
	// response, reusing the sequence number precisely so the outstation can
	// recognise the repeat; answering it from here rather than running it
	// again is what keeps one operator action from operating a point twice.
	lastReqValid   bool
	lastReqSource  uint16
	lastReqSeq     uint8
	lastReqFrag    []byte
	lastRespBodies [][]byte
	lastRespEvents bool

	// sel is the live select-before-operate reservation, and cmds executes
	// the controls themselves.
	sel  selection
	cmds CommandHandler

	// unsol is the unsolicited reporting state, and connected gates it: an
	// outstation with no master attached has nowhere to send.
	unsol     unsolState
	connected bool

	// linkDeadline is when an unacknowledged link frame should be retried.
	linkDeadline time.Time

	// attributes is what this device answers a group 0 read with, assembled
	// once at construction because neither half of it changes.
	attributes attributeStore

	// file is the transfer in flight, and handleSeq issues the handles. A
	// handle is never reused within a session, so a master holding a stale one
	// is told it is invalid rather than being given somebody else's file.
	file      *transfer
	handleSeq uint32

	// recordedTime is when the last RECORD_CURRENT_TIME request arrived, which
	// a master reads back as group 50 variation 3 to work out the transit
	// delay before setting the clock.
	recordedTime time.Time

	updates chan func(*Database)
	mu      sync.Mutex
	stats   Stats
}

// Stats counts what a session has done.
type Stats struct {
	RequestsReceived   uint64
	ResponsesSent      uint64
	FragmentsSent      uint64
	ConfirmsReceived   uint64
	ConfirmTimeouts    uint64
	UnknownFunction    uint64
	RepeatedRequests   uint64
	IncompleteRequests uint64
	MalformedRequests  uint64
	Connections        uint64

	CommandsExecuted    uint64
	CommandsRejected    uint64
	UnsolicitedSent     uint64
	UnsolicitedTimeouts uint64

	// File transfer. FileErrors counts the operations refused with a status
	// code — a missing file, a denied write — which is a configuration
	// problem far more often than a protocol one.
	// AttributesRead counts group 0 reads answered.
	AttributesRead uint64

	FilesOpened        uint64
	FileBlocksSent     uint64
	FileBlocksReceived uint64
	FileErrors         uint64
	FileTimeouts       uint64
	FilesAborted       uint64
}

// New returns an outstation session.
//
// A nil Application uses [NopApplication]. A nil CommandHandler uses
// [RejectingCommandHandler], which refuses every control — an outstation whose
// controls are not wired up must say so rather than silently report success.
func New(cfg Config, appl Application, cmds CommandHandler) *Session {
	cfg.applyDefaults()
	if appl == nil {
		appl = NopApplication{}
	}
	if cmds == nil {
		cmds = RejectingCommandHandler{}
	}

	events := NewEventBuffer(cfg.Events)
	return &Session{
		attributes: buildAttributes(cfg),
		cfg:        cfg,
		appl:       appl,
		cmds:       cmds,
		db:         NewDatabase(cfg.Database, events),
		log:        cfg.Log.With("role", "outstation", "addr", cfg.LocalAddr),
		// A fresh outstation reports a restart until the master clears it.
		// Suppressing that would deny the master the one signal that says
		// "my event history is gone, re-poll everything".
		iin:     app.IINDeviceRestart,
		updates: make(chan func(*Database), 64),
	}
}

// Restart makes the outstation report a restart to its master.
//
// It is what a device calls when it has genuinely restarted, and what a
// simulator calls to produce the condition on demand. The restart indication
// is the only signal that tells a master its whole picture is stale — the
// event history is gone, so no incremental poll can recover it and only a full
// re-baseline will do.
func (s *Session) Restart() {
	s.Update(func(*Database) {
		s.iin = s.iin.Set(app.IINDeviceRestart)
		s.synchronized = false
		s.db.events.Reset()
		s.unsol.reset()
	})
}

// Database returns the point database. Prefer [Session.Update] for
// modifications, which serialises them with the session goroutine.
func (s *Session) Database() *Database { return s.db }

// Events returns the event buffer.
func (s *Session) Events() *EventBuffer { return s.db.events }

// Stats returns a snapshot of the session counters.
func (s *Session) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}

// Update applies fn to the database from the session goroutine.
//
// Batching changes in one call is what makes a set of related updates — a
// breaker opening and its alarm asserting — produce one consistent set of
// events rather than a torn read.
func (s *Session) Update(fn func(*Database)) {
	select {
	case s.updates <- fn:
	default:
		// The queue is full, which means the session is not running or is
		// wedged. Apply directly rather than dropping the update: the
		// database takes its own lock.
		fn(s.db)
	}
}

// Run connects and serves until the context is cancelled.
func (s *Session) Run(ctx context.Context, ch channel.Channel) error {
	s.stack = stack.New(stack.Config{
		LocalAddr:     s.cfg.LocalAddr,
		RemoteAddr:    s.cfg.RemoteAddr,
		IsMaster:      false,
		UseConfirms:   s.cfg.UseLinkConfirms,
		MaxRetries:    s.cfg.LinkRetries,
		MaxRxFragment: s.cfg.MaxRxFragment,
	})

	// Cancelling the context is how Run is asked to stop, and a closed channel
	// is the same instruction arriving from the other direction. Both end the
	// loop by returning nil: a shutdown that reports itself as a failure makes
	// every caller write the same "unless I asked for it" check.
	for {
		if ctx.Err() != nil {
			return nil //nolint:nilerr // cancellation is a clean shutdown
		}

		conn, err := ch.Connect(ctx)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, channel.ErrClosed) {
				return nil //nolint:nilerr // cancellation is a clean shutdown
			}
			return fmt.Errorf("outstation: connect: %w", err)
		}

		s.bump(func(st *Stats) { st.Connections++ })
		s.log.Info("connected", "channel", ch.String())

		s.stack.Reset()
		s.serve(ctx, conn)
		_ = conn.Close()

		s.log.Info("disconnected")
	}
}

// serve runs one connection until it fails or the context ends.
func (s *Session) serve(ctx context.Context, conn io.ReadWriteCloser) {
	// The read goroutine only moves octets. Everything that touches protocol
	// state runs on this goroutine, so the stack needs no locking and a
	// response can never interleave with the processing of an inbound frame.
	rx := make(chan []byte, 8)
	readErr := make(chan error, 1)
	go readInto(ctx, conn, rx, readErr)

	s.connected = true
	s.unsol.reset()
	defer func() {
		s.connected = false
		// A transfer belongs to the connection it started on: the master that
		// opened it cannot come back to the same handle, and holding the file
		// open would deny the next one.
		if err := s.closeFile(); err != nil {
			s.log.Warn("closing a transfer on disconnect failed", "err", err)
		}
	}()

	// Announce ourselves before servicing anything. The null unsolicited
	// response exists to tell a master "I am here and I have restarted", and
	// waiting for the first tick would let the master's own startup sequence
	// clear the restart indication first — leaving the announcement carrying
	// nothing worth announcing.
	if err := s.pollUnsolicited(conn, s.appl.Now()); err != nil {
		s.log.Warn("initial unsolicited transmission failed", "err", err)
	}

	// The tick drives the confirm timeout, the select timeout and the
	// unsolicited hold time, so it has to be short relative to all three.
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case err := <-readErr:
			if err != nil && !errors.Is(err, io.EOF) {
				s.log.Warn("read loop ended", "err", err)
			}
			return

		case fn := <-s.updates:
			fn(s.db)

		case data := <-rx:
			var handleErr error
			if err := s.stack.Receive(conn, data, func(r stack.Received) {
				if handleErr == nil {
					handleErr = s.handle(conn, r)
				}
			}); err != nil {
				s.log.Warn("receive failed", "err", err)
				return
			}
			if handleErr != nil {
				s.log.Warn("request handling failed", "err", handleErr)
				return
			}
			if err := s.advanceResponse(conn); err != nil {
				s.log.Warn("sending a queued response fragment failed", "err", err)
				return
			}

		case <-ticker.C:
			now := s.appl.Now()
			s.checkLinkTimeout(conn)
			s.checkConfirmTimeout()
			s.checkSelectTimeout(now)
			s.checkFileTimeout(now)
			if err := s.pollUnsolicited(conn, now); err != nil {
				s.log.Warn("unsolicited transmission failed", "err", err)
				return
			}
		}
	}
}

// readInto moves octets from the connection to the session goroutine.
func readInto(ctx context.Context, r io.Reader, out chan<- []byte, errc chan<- error) {
	buf := make([]byte, stack.ReadChunk)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			select {
			case out <- chunk:
			case <-ctx.Done():
				errc <- ctx.Err()
				return
			}
		}
		if err != nil {
			errc <- err
			return
		}
	}
}

// checkLinkTimeout retransmits an unacknowledged link frame.
func (s *Session) checkLinkTimeout(w io.Writer) {
	if !s.stack.Pending() || time.Now().Before(s.linkDeadline) {
		return
	}
	failed, err := s.stack.OnTimeout(w)
	if err != nil {
		s.log.Warn("link retransmission failed", "err", err)
		return
	}
	s.linkDeadline = time.Now().Add(s.cfg.LinkTimeout)
	if failed {
		s.log.Warn("link layer gave up on a response")
	}
}

// checkConfirmTimeout returns selected events to the queue when the master
// never confirmed the response that carried them.
func (s *Session) checkConfirmTimeout() {
	if !s.awaitingConfirm || time.Now().Before(s.confirmDeadline) {
		return
	}
	s.awaitingConfirm = false
	// The master's silence on one fragment means it cannot be paced through
	// the rest of the response either, so the whole thing is abandoned rather
	// than pressing on: the events it carried, wherever in the series they
	// actually landed, go back in the queue for the master's next poll.
	s.pendingBodies = nil
	s.pendingIndex = 0
	n := s.db.events.Unselect()
	s.bump(func(st *Stats) { st.ConfirmTimeouts++ })
	s.log.Warn("application confirm timed out; events requeued", "events", n)
}

// handle dispatches one request fragment.
func (s *Session) handle(w io.Writer, r stack.Received) error {
	s.bump(func(st *Stats) { st.RequestsReceived++ })

	frag, err := app.ParseFragment(nil, r.Fragment)
	if err != nil {
		s.bump(func(st *Stats) { st.MalformedRequests++ })
		s.log.Warn("malformed request", "err", err)
		// A fragment we cannot parse cannot be answered meaningfully: we do
		// not know its sequence number's validity or what it asked for. The
		// parameter-error indication rides on the next response instead.
		s.iin = s.iin.Set(app.IINParameterError)
		return nil
	}

	// Only a fragment carrying both FIR and FIN is a complete request. A
	// multi-fragment request is not something this outstation reassembles,
	// and a fragment with FIR clear is a continuation of a series whose
	// beginning it never saw — acting on either means acting on a request it
	// cannot know the whole of, which for a control means operating a point
	// on the strength of half a message.
	if !frag.Header.Control.Fir || !frag.Header.Control.Fin {
		s.bump(func(st *Stats) { st.IncompleteRequests++ })
		s.log.Warn("discarding a fragment that is not a complete request",
			"fir", frag.Header.Control.Fir, "fin", frag.Header.Control.Fin,
			"seq", frag.Header.Control.Seq)
		// As with a fragment we cannot parse: there is nothing meaningful to
		// answer, so the indication rides on the next response instead.
		s.iin = s.iin.Set(app.IINParameterError)
		return nil
	}

	// A confirm is an acknowledgement of our own response, not a request: it
	// has its own sequence space and nothing to replay, so it is dispatched
	// without going near the repeat-detection below.
	if frag.Header.Func != app.FuncConfirm {
		if s.isRepeatRequest(r, frag) {
			s.bump(func(st *Stats) { st.RepeatedRequests++ })
			s.log.Debug("repeated request; re-sending the previous response",
				"seq", frag.Header.Control.Seq)
			return s.replayResponse(w, r, frag.Header)
		}
		s.rememberRequest(r, frag)
	}

	if r.Broadcast {
		// A broadcast request is executed but never answered — every
		// outstation answering at once would collide. The next response
		// carries the broadcast indication instead.
		s.iin = s.iin.Set(app.IINBroadcast)
	}

	switch frag.Header.Func {
	case app.FuncConfirm:
		// Solicited and unsolicited responses have separate sequence spaces,
		// so the UNS bit decides which one this acknowledges. Confusing them
		// would drop events the master never received.
		if frag.Header.Control.Uns {
			s.onUnsolicitedConfirm(frag.Header)
		} else {
			s.onConfirm(frag.Header)
		}
		return nil

	case app.FuncRead:
		return s.onRead(w, r, frag)

	case app.FuncWrite:
		return s.onWrite(w, r, frag)

	case app.FuncDelayMeasure:
		return s.onDelayMeasure(w, r, frag)

	case app.FuncRecordCurrentTime:
		return s.onRecordCurrentTime(w, r, frag)

	case app.FuncOpenFile:
		return s.onOpenFile(w, r, frag)

	case app.FuncCloseFile:
		return s.onCloseFile(w, r, frag)

	case app.FuncDeleteFile:
		return s.onDeleteFile(w, r, frag)

	case app.FuncGetFileInfo:
		return s.onGetFileInfo(w, r, frag)

	case app.FuncAbortFile:
		return s.onAbortFile(w, r, frag)

	case app.FuncColdRestart, app.FuncWarmRestart:
		return s.onRestart(w, r, frag)

	case app.FuncEnableUnsolicited, app.FuncDisableUnsolicited:
		return s.onUnsolicitedControl(w, r, frag)

	case app.FuncAssignClass:
		return s.onAssignClass(w, r, frag)

	case app.FuncSelect, app.FuncOperate,
		app.FuncDirectOperate, app.FuncDirectOperateNR:
		return s.onCommand(w, r, frag)

	case app.FuncImmedFreeze, app.FuncImmedFreezeNR:
		s.db.FreezeCounters()
		if frag.Header.Func.NoReply() || r.Broadcast {
			return nil
		}
		return s.respond(w, r, frag.Header, nil)

	default:
		s.bump(func(st *Stats) { st.UnknownFunction++ })
		s.iin = s.iin.Set(app.IINNoFuncCodeSupport)
		if r.Broadcast {
			return nil
		}
		return s.respond(w, r, frag.Header, nil)
	}
}

// onConfirm clears the wait on the fragment just confirmed.
//
// It does not touch the event buffer: a multi-fragment response may still
// have fragments left to send, sharing this same sequence number, so this
// confirm cannot be told apart from one for a later fragment. The events are
// only actually cleared once the whole response is done — see
// finishResponse, reached through advanceResponse once every fragment has
// both gone out and, where it needed one, been confirmed.
func (s *Session) onConfirm(h app.Header) {
	s.bump(func(st *Stats) { st.ConfirmsReceived++ })

	if !s.awaitingConfirm || h.Control.Seq != s.confirmSeq {
		// A confirm for a response we are not waiting on. Ignoring it is
		// right: acting on it would drop events the master never received.
		s.log.Debug("unexpected confirm", "seq", h.Control.Seq, "awaiting", s.awaitingConfirm)
		return
	}
	s.awaitingConfirm = false
}

// onRead answers a read request.
func (s *Session) onRead(w io.Writer, r stack.Received, frag app.Fragment) error {
	// A read naming a group 70 object is asking for the next block of a file,
	// not for measurements. It cannot be both: the object header says which.
	if h, ok := fileObject(frag); ok {
		return s.onFileRead(w, r, frag, h)
	}
	// Group 0 is the same story: an attribute read asks what the device is,
	// not what it is measuring.
	if h, ok := attributeHeader(frag); ok {
		return s.onAttributeRead(w, r, frag, h)
	}

	ctx := objects.Context{Synchronized: s.synchronized}
	b := newResponseBuilder(s.cfg.MaxTxFragment, ctx)

	var selected []Event

	for _, h := range frag.Objects {
		switch {
		case h.Group == 60:
			switch h.Variation {
			case 1: // class 0: all static data
				for _, pt := range staticTypes {
					s.buildStaticRange(b, pt, 0, 0, 0xFFFF)
				}
			case 2, 3, 4: // event classes 1, 2 and 3
				mask := dnp3.Class1 << (h.Variation - 2)
				selected = append(selected, s.db.events.Select(mask, 512)...)
			}

		case h.Group == 50 && h.Variation == 3:
			// The second half of the LAN time-sync procedure: hand back the
			// time the RECORD_CURRENT_TIME request arrived.
			if s.recordedTime.IsZero() {
				s.iin = s.iin.Set(app.IINParameterError)
				continue
			}
			b.add(app.ObjectHeader{
				Group: 50, Variation: 3,
				Qualifier: app.MakeQualifier(app.PrefixNone, app.RangeCount8),
				Range:     app.Range{Spec: app.RangeCount8, Count: 1},
				Data:      objects.AppendTime48(nil, dnp3.Now(s.recordedTime)),
			})

		default:
			pt, ok := pointTypeForGroup(h.Group)
			if !ok {
				s.iin = s.iin.Set(app.IINObjectUnknown)
				continue
			}
			start, stop := uint16(0), uint16(0xFFFF)
			if h.Range.Spec.IsStartStop() {
				start, stop = uint16(h.Range.Start), uint16(h.Range.Stop)
			}
			s.buildStaticRange(b, pt, h.Variation, start, stop)
		}
	}

	if len(selected) > 0 {
		s.buildEvents(b, selected)
	}
	return s.sendFragments(w, r, frag.Header, b.done(), len(selected) > 0)
}

// onWrite handles the write function code.
func (s *Session) onWrite(w io.Writer, r stack.Received, frag app.Fragment) error {
	if h, ok := fileObject(frag); ok {
		return s.onFileWrite(w, r, frag, h)
	}

	for _, h := range frag.Objects {
		switch {
		case h.Group == 80 && h.Variation == 1:
			// A master clears DEVICE_RESTART by writing zero to index 7.
			// This is the handshake that ends the restart sequence.
			s.iin = s.iin.Clear(app.IINDeviceRestart)
			s.log.Debug("device restart indication cleared by master")

		case h.Group == 50 && h.Variation == 3:
			// The second half of the LAN time-synchronisation procedure.
			//
			// The master sent RECORD_CURRENT_TIME, we noted when it arrived,
			// and it is now telling us what its own clock read at that moment.
			// The correction is that value plus however long we have taken
			// since — which is what makes this procedure better than a plain
			// clock write: the transit delay is measured rather than assumed.
			if !s.appl.SupportsWriteTime() {
				s.iin = s.iin.Set(app.IINNoFuncCodeSupport)
				continue
			}
			if s.recordedTime.IsZero() {
				// No RECORD_CURRENT_TIME preceded this, so there is no
				// reference to correct against.
				s.iin = s.iin.Set(app.IINParameterError)
				continue
			}
			if len(h.Data) < objects.Time48Size {
				s.iin = s.iin.Set(app.IINParameterError)
				continue
			}
			recorded := objects.ParseTime48(h.Data)
			elapsed := s.appl.Now().Sub(s.recordedTime)
			if s.appl.WriteAbsoluteTime(recorded.Time.Add(elapsed)) {
				s.synchronized = true
				s.iin = s.iin.Clear(app.IINNeedTime)
				s.recordedTime = time.Time{}
				s.log.Debug("clock set by the recorded-time procedure",
					"recorded_at", recorded.Time, "elapsed", elapsed)
			} else {
				s.iin = s.iin.Set(app.IINParameterError)
			}

		case h.Group == 50 && h.Variation == 1:
			if !s.appl.SupportsWriteTime() {
				s.iin = s.iin.Set(app.IINNoFuncCodeSupport)
				continue
			}
			if len(h.Data) < objects.Time48Size {
				s.iin = s.iin.Set(app.IINParameterError)
				continue
			}
			ts := objects.ParseTime48(h.Data)
			if s.appl.WriteAbsoluteTime(ts.Time) {
				s.synchronized = true
				s.iin = s.iin.Clear(app.IINNeedTime)
				s.log.Debug("clock set by master", "time", ts.Time)
			} else {
				s.iin = s.iin.Set(app.IINParameterError)
			}

		case h.Group == 34:
			s.writeDeadbands(h)

		default:
			s.iin = s.iin.Set(app.IINObjectUnknown)
		}
	}
	if r.Broadcast {
		return nil
	}
	return s.respond(w, r, frag.Header, nil)
}

// writeDeadbands applies a group 34 analog deadband write.
//
// A deadband is how a master tells an outstation how much a point must move
// before it is worth an event, which is the only lever it has over a chattering
// analog short of dropping the point from its class.
func (s *Session) writeDeadbands(h app.ObjectHeader) {
	d, ok := objects.Lookup(objects.GV(h.Group, h.Variation))
	if !ok {
		s.iin = s.iin.Set(app.IINObjectUnknown)
		return
	}
	size, ok := d.SizeOctets()
	if !ok || size == 0 {
		s.iin = s.iin.Set(app.IINObjectUnknown)
		return
	}

	prefixLen := 0
	if p := h.Qualifier.IndexPrefix(); p.IsIndex() {
		prefixLen = p.Octets()
	}

	off := 0
	for i := range int(h.Count()) {
		if off+prefixLen+size > len(h.Data) {
			s.iin = s.iin.Set(app.IINParameterError)
			return
		}

		index := uint16(h.Range.IndexOf(uint32(i)))
		if prefixLen > 0 {
			index = uint16(readPrefix(h.Data[off:], prefixLen))
			off += prefixLen
		}

		value := decodeDeadband(h.Variation, h.Data[off:off+size])
		off += size

		_, cfg, exists := s.db.Analog(index)
		if !exists {
			s.iin = s.iin.Set(app.IINParameterError)
			continue
		}
		cfg.Deadband = value
		s.db.Configure(dnp3.TypeAnalog, index, cfg)
		s.log.Debug("deadband written", "index", index, "value", value)
	}
}

// decodeDeadband reads one group 34 value.
func decodeDeadband(variation uint8, buf []byte) float64 {
	switch variation {
	case 1:
		return float64(uint16(buf[0]) | uint16(buf[1])<<8)
	case 2:
		return float64(uint32(buf[0]) | uint32(buf[1])<<8 | uint32(buf[2])<<16 | uint32(buf[3])<<24)
	case 3:
		return float64(math.Float32frombits(
			uint32(buf[0]) | uint32(buf[1])<<8 | uint32(buf[2])<<16 | uint32(buf[3])<<24))
	}
	return 0
}

// onDelayMeasure answers with the fine time delay, which a master uses to
// estimate the round trip before setting the clock.
func (s *Session) onDelayMeasure(w io.Writer, r stack.Received, frag app.Fragment) error {
	body := app.AppendObjectHeader(nil, app.ObjectHeader{
		Group: 52, Variation: 2,
		Qualifier: app.MakeQualifier(app.PrefixNone, app.RangeCount8),
		Range:     app.Range{Spec: app.RangeCount8, Count: 1},
		Data:      []byte{0, 0}, // no processing delay to declare
	})
	return s.respond(w, r, frag.Header, body)
}

// onRecordCurrentTime notes when the request arrived.
//
// This is the first half of the standard's LAN time-synchronisation procedure:
// the master sends it, the outstation records the arrival time, and the master
// then reads that time back as group 50 variation 3 to work out how long the
// message took to get there. An outstation that refuses it leaves that master
// unable to set the clock at all.
//
// The standard says to record the time the *first octet* arrived. This records
// the time the fragment was dispatched, which is later by the time it took to
// receive and reassemble the frame — negligible over Ethernet, and on a slow
// serial link the delay-measure procedure is the right one to use anyway.
func (s *Session) onRecordCurrentTime(w io.Writer, r stack.Received, frag app.Fragment) error {
	s.recordedTime = s.appl.Now()
	s.log.Debug("current time recorded", "time", s.recordedTime)

	if r.Broadcast {
		return nil
	}
	return s.respond(w, r, frag.Header, nil)
}

// onRestart answers a restart request with how long the outstation expects to
// be unavailable.
func (s *Session) onRestart(w io.Writer, r stack.Received, frag app.Fragment) error {
	var d time.Duration
	if frag.Header.Func == app.FuncColdRestart {
		d = s.appl.ColdRestart()
		s.db.events.Reset()
	} else {
		d = s.appl.WarmRestart()
	}
	s.iin = s.iin.Set(app.IINDeviceRestart)
	s.synchronized = false

	ms := d.Milliseconds()
	if ms > 0xFFFF {
		ms = 0xFFFF
	}
	body := app.AppendObjectHeader(nil, app.ObjectHeader{
		Group: 52, Variation: 2,
		Qualifier: app.MakeQualifier(app.PrefixNone, app.RangeCount8),
		Range:     app.Range{Spec: app.RangeCount8, Count: 1},
		Data:      []byte{byte(ms), byte(ms >> 8)},
	})
	if r.Broadcast {
		return nil
	}
	return s.respond(w, r, frag.Header, body)
}

// onUnsolicitedControl records the enable or disable, answering truthfully
// that the request was understood.
func (s *Session) onUnsolicitedControl(w io.Writer, r stack.Received, frag app.Fragment) error {
	enable := frag.Header.Func == app.FuncEnableUnsolicited
	for _, h := range frag.Objects {
		if h.Group != 60 || h.Variation < 2 || h.Variation > 4 {
			s.iin = s.iin.Set(app.IINObjectUnknown)
			continue
		}
		class := dnp3.Class1 << (h.Variation - 2)
		if enable {
			s.unsolClasses |= class
		} else {
			s.unsolClasses &^= class
		}
	}
	if r.Broadcast {
		return nil
	}
	return s.respond(w, r, frag.Header, nil)
}

// onAssignClass moves point types between event classes.
func (s *Session) onAssignClass(w io.Writer, r stack.Received, frag app.Fragment) error {
	class := dnp3.ClassNone
	for _, h := range frag.Objects {
		if h.Group == 60 {
			switch h.Variation {
			case 1:
				class = dnp3.ClassNone // class 0 means "no events"
			case 2, 3, 4:
				class = dnp3.Class1 << (h.Variation - 2)
			}
			continue
		}
		if pt, ok := pointTypeForGroup(h.Group); ok {
			s.db.AssignClass(pt, class)
		} else {
			s.iin = s.iin.Set(app.IINObjectUnknown)
		}
	}
	if r.Broadcast {
		return nil
	}
	return s.respond(w, r, frag.Header, nil)
}

// isRepeatRequest reports whether this is the request we last acted on,
// arriving again because the master did not see the response.
//
// The whole fragment is compared, not just the sequence number: a master that
// reuses a sequence number for genuinely different content is asking for
// something new, and suppressing that would be far worse than answering a
// repeat twice.
func (s *Session) isRepeatRequest(r stack.Received, frag app.Fragment) bool {
	return s.lastReqValid &&
		r.Source == s.lastReqSource &&
		frag.Header.Control.Seq == s.lastReqSeq &&
		bytes.Equal(r.Fragment, s.lastReqFrag)
}

// rememberRequest records a request as the one to compare repeats against.
func (s *Session) rememberRequest(r stack.Received, frag app.Fragment) {
	s.lastReqValid = true
	s.lastReqSource = r.Source
	s.lastReqSeq = frag.Header.Control.Seq
	// r.Fragment aliases the stack's reassembly buffer and is valid only for
	// this call, so it has to be copied to outlive it.
	s.lastReqFrag = append(s.lastReqFrag[:0], r.Fragment...)

	// Whatever we answered the previous request with no longer belongs to
	// this one; it is replaced when this request produces its own response.
	s.lastRespBodies = nil
	s.lastRespEvents = false
}

// replayResponse re-sends the response the identical previous request
// produced, without executing anything a second time.
func (s *Session) replayResponse(w io.Writer, r stack.Received, req app.Header) error {
	if r.Broadcast || len(s.lastRespBodies) == 0 {
		// A broadcast is executed but never answered, and a request that
		// produced no response has nothing to repeat.
		return nil
	}

	// This supersedes any part of that same response still in flight with an
	// identical send from its first fragment. The events it carries stay
	// selected rather than going back to the queue, because they are the very
	// events this replay is about to carry again.
	s.awaitingConfirm = false
	s.pendingBodies = nil
	s.pendingIndex = 0

	return s.sendFragments(w, r, req, s.lastRespBodies, s.lastRespEvents)
}

// respond sends a single-fragment response carrying body.
func (s *Session) respond(w io.Writer, r stack.Received, req app.Header, body []byte) error {
	return s.sendFragments(w, r, req, [][]byte{body}, false)
}

// sendFragments starts a response, splitting it across fragments as needed,
// and sends as much of it as the link and the master's pacing allow right
// now. The rest, if any, is queued and driven forward by advanceResponse as
// each fragment's acknowledgement arrives — see the pendingBodies field.
//
// Every fragment but the last carries FIN clear. A fragment carrying events
// sets CON, because only a confirmation lets the outstation drop them.
func (s *Session) sendFragments(w io.Writer, r stack.Received, req app.Header, bodies [][]byte, hasEvents bool) error {
	if r.Broadcast {
		return nil
	}

	if s.pendingIndex < len(s.pendingBodies) {
		// A well-behaved master confirms (or lets confirm time out) before
		// asking anything else, so this should not happen; abandon the
		// response still in flight rather than silently losing track of
		// whatever events it was holding.
		s.log.Warn("a new response is replacing one still in flight")
		if s.awaitingConfirm {
			s.awaitingConfirm = false
			s.db.events.Unselect()
		}
	}

	s.pendingBodies = bodies
	s.pendingIndex = 0
	s.pendingDest = r.Source
	s.pendingSeq = req.Control.Seq
	s.pendingHasEvents = hasEvents

	// Kept so a retransmission of the request that produced it is answered
	// with this same response rather than by running the request again.
	s.lastRespBodies = bodies
	s.lastRespEvents = hasEvents

	return s.advanceResponse(w)
}

// advanceResponse sends the next queued fragment of a response in progress,
// if nothing is holding it back, and finishes the response once every
// fragment has been both sent and, where required, confirmed.
//
// Two independent things can hold the next fragment back, and only one is
// ever outstanding for long: the link layer, when link confirms are in use
// and the peer has not yet acknowledged the last frame (stack.Busy), and the
// application layer, when the fragment just sent asked for a confirmation of
// its own (awaitingConfirm). Both gate on state the rest of the session
// already maintains — the stack for the first, onConfirm and
// checkConfirmTimeout for the second — so this only needs to check them.
func (s *Session) advanceResponse(w io.Writer) error {
	if len(s.pendingBodies) == 0 {
		return nil
	}
	if s.awaitingConfirm || s.stack.Busy() {
		return nil
	}
	if s.pendingIndex >= len(s.pendingBodies) {
		return s.finishResponse()
	}

	i := s.pendingIndex
	last := i == len(s.pendingBodies)-1
	// Intermediate fragments must be confirmed or the master cannot pace the
	// series; the final one only needs it when it carries events.
	needConfirm := !last || s.pendingHasEvents

	ctrl := app.Control{
		Fir: i == 0,
		Fin: last,
		Con: needConfirm,
		Seq: s.pendingSeq,
	}

	frag := app.AppendHeader(nil, app.Header{
		Control: ctrl,
		Func:    app.FuncResponse,
		IIN:     s.currentIIN(),
	})
	frag = append(frag, s.pendingBodies[i]...)

	if err := s.stack.SendTo(w, s.pendingDest, frag); err != nil {
		return err
	}
	s.bump(func(st *Stats) { st.FragmentsSent++ })
	if s.stack.Pending() {
		s.linkDeadline = time.Now().Add(s.cfg.LinkTimeout)
	}
	s.pendingIndex++

	if needConfirm {
		s.awaitingConfirm = true
		s.confirmSeq = ctrl.Seq
		s.confirmDeadline = time.Now().Add(s.cfg.ConfirmTimeout)
		return nil
	}
	return s.finishResponse()
}

// finishResponse closes out a response once every fragment has gone out and
// none is still awaiting confirmation: only now is it safe to drop the
// events it carried, however many trailing fragments they ended up spread
// across, since only now do we know the master has everything.
func (s *Session) finishResponse() error {
	hasEvents := s.pendingHasEvents
	s.pendingBodies = nil
	s.pendingIndex = 0

	if hasEvents {
		n := s.db.events.Confirm()
		s.log.Debug("events confirmed", "count", n)
	}
	s.bump(func(st *Stats) { st.ResponsesSent++ })
	// The broadcast indication reports only the request that arrived by
	// broadcast, so it is cleared once reported.
	s.iin = s.iin.Clear(app.IINBroadcast)
	return nil
}

// currentIIN assembles the indications to report, folding in the event state
// that changes between responses.
func (s *Session) currentIIN() app.IIN {
	iin := s.iin

	classes := s.db.events.Classes()
	if classes&dnp3.Class1 != 0 {
		iin = iin.Set(app.IINClass1Events)
	}
	if classes&dnp3.Class2 != 0 {
		iin = iin.Set(app.IINClass2Events)
	}
	if classes&dnp3.Class3 != 0 {
		iin = iin.Set(app.IINClass3Events)
	}
	if s.db.events.Overflowed() {
		iin = iin.Set(app.IINEventBufferOverflow)
	}
	if !s.synchronized {
		iin = iin.Set(app.IINNeedTime)
	}
	return iin
}

func (s *Session) bump(fn func(*Stats)) {
	s.mu.Lock()
	fn(&s.stats)
	s.mu.Unlock()
}

// pointTypeForGroup maps a static object group to its measurement type.
func pointTypeForGroup(group uint8) (dnp3.PointType, bool) {
	switch group {
	case 1, 2:
		return dnp3.TypeBinary, true
	case 3, 4:
		return dnp3.TypeDoubleBitBinary, true
	case 10, 11:
		return dnp3.TypeBinaryOutputStatus, true
	case 20, 22:
		return dnp3.TypeCounter, true
	case 21, 23:
		return dnp3.TypeFrozenCounter, true
	case 30, 32:
		return dnp3.TypeAnalog, true
	case 40, 42:
		return dnp3.TypeAnalogOutputStatus, true
	}
	return dnp3.TypeUnknown, false
}
