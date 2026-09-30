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
	"sync/atomic"
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

	// WritableAttributes lists attributes that accept same-type writes.
	WritableAttributes []AttributeID
	// AttributeWrite may reject or persist an attribute before it is stored.
	AttributeWrite func(dnp3.Attribute) bool
	// SelfAddress enables destination 0xFFFC discovery.
	SelfAddress bool
	// Management implements application and configuration operations.
	Management ManagementHandler
	// ActivateConfig applies named configuration files and reports g91v1 results.
	ActivateConfig func([]string) objects.ActivationResult
	// SecureAuthentication enables DNP3 symmetric authentication independently of TLS.
	SecureAuthentication *SecureAuthenticationConfig
	// TerminalWrite consumes master virtual-terminal output. Nil refuses writes.
	TerminalWrite func(uint16, []byte) bool
	// Datasets supplies prototype, descriptor and present-value objects.
	Datasets []DatasetObject
	// DatasetWrite validates and applies prototype-dependent dataset writes.
	DatasetWrite func(group, variation uint8, object []byte) bool

	// UseLinkConfirms enables link-layer confirmation, normally off over TCP.
	UseLinkConfirms bool
	// LinkRetries is how many times a confirmed frame is retransmitted.
	LinkRetries int
	// LinkTimeout is how long to wait for a link-layer acknowledgement before
	// retransmitting. It matters only when UseLinkConfirms is set.
	LinkTimeout time.Duration

	// Indications are the device-controlled internal indications asserted from
	// the start, for a device that already knows its configuration is bad or a
	// point is in local control when it comes up. They can be changed while
	// running with [Session.SetIndication].
	Indications Indication

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

// Indication is a device-controlled internal indication: one the library
// cannot know for itself, because it depends on the device rather than on the
// protocol. Set them with [Session.SetIndication].
type Indication uint16

// Device-controlled indications.
const (
	// IndicationLocalControl reports that one or more points are in local
	// control and will not accept commands from the master.
	IndicationLocalControl = Indication(app.IINLocalControl)
	// IndicationDeviceTrouble reports a device-specific fault. What it means
	// is the device's to define, and the master's to look up.
	IndicationDeviceTrouble = Indication(app.IINDeviceTrouble)
	// IndicationConfigCorrupt reports that the device's configuration is not
	// valid and its answers cannot be relied on.
	IndicationConfigCorrupt = Indication(app.IINConfigCorrupt)

	settableIndications = IndicationLocalControl | IndicationDeviceTrouble | IndicationConfigCorrupt
)

// SetIndication asserts or clears device-controlled indications in every
// response from now on. It is safe to call from any goroutine.
//
// Only [IndicationLocalControl], [IndicationDeviceTrouble] and
// [IndicationConfigCorrupt] can be set; anything else is ignored, since the
// rest describe the protocol and are the library's to assert.
func (s *Session) SetIndication(ind Indication, on bool) {
	ind &= settableIndications
	for {
		old := s.indications.Load()
		next := old &^ uint32(ind)
		if on {
			next = old | uint32(ind)
		}
		if s.indications.CompareAndSwap(old, next) {
			return
		}
	}
}

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
	// truly in flight at a time, on purpose: the master paces the series with
	// its confirms. pendingDest and pendingSeq, the request's sequence number
	// and the first fragment's, are constant across the response; each later
	// fragment adds its index to it. pendingHasEvents says whether it carries events at all, which
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
	// freezes are the FREEZE_AT_TIME schedules waiting for their moment.
	freezes []freezeSchedule

	// indications are the bits the application controls: LOCAL_CONTROL,
	// DEVICE_TROUBLE and CONFIG_CORRUPT. Atomic because they are set from the
	// application's goroutines and read on the session's.
	indications atomic.Uint32

	// restartUntil is when the last restart the application announced is
	// expected to finish, so a second request for it while it is still under
	// way is answered ALREADY_EXECUTING instead of being carried out twice.
	restartUntil time.Time

	lastReqValid   bool
	lastReqSource  uint16
	lastReqSeq     uint8
	lastReqFrag    []byte
	lastRespBodies [][]byte
	lastRespEvents bool
	// lastRespErrors is the request error indications that response carried.
	// They are cleared once reported, so a replay has to put them back: the
	// repeat is owed the same answer, refusal and all.
	lastRespErrors app.IIN

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
	// at construction and updated by validated attribute writes.
	attributes attributeStore

	// file is the transfer in flight, and handleSeq issues the handles. A
	// handle is never reused within a session, so a master holding a stale one
	// is told it is invalid rather than being given somebody else's file.
	file      *transfer
	handleSeq uint32
	fileAuth  fileAuthorization
	security  securityState

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
	// ScheduledFreezes counts FREEZE_AT_TIME freezes that have been performed.
	ScheduledFreezes uint64
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
	s := &Session{
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
	for _, v := range cfg.Datasets {
		if err := s.db.UpdateDataset(v, dnp3.ClassNone); err != nil {
			s.iin = s.iin.Set(app.IINConfigCorrupt)
		}
	}
	s.resetSecurity()
	s.indications.Store(uint32(cfg.Indications & settableIndications))
	return s
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

// drainUpdates applies every update already queued, without waiting for more.
func (s *Session) drainUpdates() {
	for {
		select {
		case fn := <-s.updates:
			fn(s.db)
		default:
			return
		}
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
		SelfAddress:   s.cfg.SelfAddress,
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
	// Cached application replies belong to this connection, including file
	// authentication keys and requests with device side effects.
	s.lastReqValid = false
	s.unsol.reset()
	defer func() {
		s.connected = false
		// A transfer belongs to the connection it started on: the master that
		// opened it cannot come back to the same handle, and holding the file
		// open would deny the next one.
		s.resetSecurity()
		s.fileAuth = fileAuthorization{}
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
			// Updates the application submitted before this request arrived
			// belong in its answer. Select picks among ready cases at random,
			// so without this a request could overtake an update queued
			// ahead of it and be answered with the state from before.
			s.drainUpdates()

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
			s.runFreezes(now)
			s.expireSecurity()
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
	return s.handleAuthenticated(w, r, false)
}

func (s *Session) handleAuthenticated(w io.Writer, r stack.Received, authenticated bool) error {
	s.bump(func(st *Stats) { st.RequestsReceived++ })

	if s.cfg.SecureAuthentication != nil {
		s.countSecurity(6)
		s.expireSecurity()
	}
	frag, err := app.ParseFragment(nil, r.Fragment)
	if err != nil {
		s.bump(func(st *Stats) { st.MalformedRequests++ })
		s.log.Warn("malformed request", "err", err)
		return s.rejectMalformed(w, r, err)
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

	// Two things a well-formed message never does, whatever it is asking for.
	// A CONFIRM is a bare header: objects after it are not an acknowledgement
	// of anything. And CON and UNS belong to responses — a request asks the
	// outstation for nothing by setting them, so one that does is discarded
	// unanswered rather than acted on as though it meant something else.
	if frag.Header.Func == app.FuncConfirm && len(frag.Objects) > 0 {
		s.bump(func(st *Stats) { st.MalformedRequests++ })
		s.log.Warn("discarding a confirm that carries objects", "seq", frag.Header.Control.Seq)
		return nil
	}
	if frag.Header.Func != app.FuncConfirm && (frag.Header.Control.Con || frag.Header.Control.Uns) {
		s.bump(func(st *Stats) { st.MalformedRequests++ })
		s.log.Warn("discarding a request with CON or UNS set",
			"func", frag.Header.Func, "con", frag.Header.Control.Con, "uns", frag.Header.Control.Uns)
		return nil
	}

	// A prefix and a range that do not go together — an index prefix on a
	// start-stop range, say — are not a qualifier the standard defines, and
	// which points they name is anyone's guess.
	for _, h := range frag.Objects {
		if !h.Qualifier.Consistent() {
			s.bump(func(st *Stats) { st.MalformedRequests++ })
			s.log.Warn("request carries an inconsistent qualifier",
				"group", h.Group, "variation", h.Variation, "qualifier", h.Qualifier)
			return s.rejectMalformed(w, r, fmt.Errorf("%w: %s", app.ErrBadQualifier, h.Qualifier))
		}
	}

	if frag.Header.Func == app.FuncAuthRequest || frag.Header.Func == app.FuncAuthRequestNoAck {
		return s.onAuthentication(w, r, frag)
	}
	if !authenticated && s.cfg.SecureAuthentication != nil && (criticalFunction(frag.Header.Func) || hasAuthenticationObject(frag)) {
		return s.challengeRequest(w, r, frag)
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

	// A request whose function code is meaningless without objects asked for
	// nothing, which is not the same as having nothing to do. Answering it
	// with an empty success would tell the master its request was carried
	// out: for a control, that its point was operated.
	if frag.Header.Func.RequiresObjects() && len(frag.Objects) == 0 {
		s.bump(func(st *Stats) { st.MalformedRequests++ })
		s.log.Warn("request carried no objects but requires them",
			"func", frag.Header.Func, "seq", frag.Header.Control.Seq)
		s.iin = s.iin.Set(app.IINParameterError)
		if frag.Header.Func.NoReply() || r.Broadcast {
			// Nothing to answer to; the indication rides on the next response.
			return nil
		}
		// Unlike a fragment we cannot parse, this one has a valid header and a
		// sequence number to answer on, and the master is waiting: it gets a
		// null response carrying the error rather than silence.
		return s.respond(w, r, frag.Header, nil)
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

	case app.FuncAuthenticateFile:
		return s.onAuthenticateFile(w, r, frag)
	case app.FuncInitializeData, app.FuncInitializeAppl, app.FuncStartAppl, app.FuncStopAppl, app.FuncSaveConfig, app.FuncActivateConfig:
		return s.onManagement(w, r, frag)
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

	case app.FuncImmedFreeze, app.FuncImmedFreezeNR,
		app.FuncFreezeClear, app.FuncFreezeClearNR:
		clear := frag.Header.Func == app.FuncFreezeClear || frag.Header.Func == app.FuncFreezeClearNR
		s.onFreeze(frag, clear)
		if frag.Header.Func.NoReply() || r.Broadcast {
			return nil
		}
		return s.respond(w, r, frag.Header, nil)

	case app.FuncFreezeAtTime, app.FuncFreezeAtTimeNR:
		s.onFreezeAtTime(frag)
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

// timeWriteData returns the octets of the first time object in a write,
// skipping the object's index prefix when the qualifier carries one.
//
// ObjectHeader.Data includes per-object index prefixes, and both qualifiers
// 0x17 and 0x28 are legal for a write: reading Data from its first octet
// would take the index for the top of the timestamp and set a garbage clock.
func timeWriteData(h app.ObjectHeader) ([]byte, bool) {
	skip := h.Qualifier.IndexPrefix().Octets()
	if len(h.Data) < skip+objects.Time48Size {
		return nil, false
	}
	return h.Data[skip : skip+objects.Time48Size], true
}

// rejectMalformed answers a request whose object section could not be parsed.
//
// When the fragment header itself was readable there is a sequence number to
// answer on and a master waiting, so it gets a null response carrying the
// indication rather than silence: an object this outstation does not know is
// OBJECT_UNKNOWN, a function it does not implement is NO_FUNC_CODE_SUPPORT
// (checked first, since it says more than any object could), and anything else
// is PARAMETER_ERROR. Where nothing can be answered — an unreadable header, a
// fragment that is not a complete request, a request that takes no reply — the
// indication rides on the next response instead.
func (s *Session) rejectMalformed(w io.Writer, r stack.Received, perr error) error {
	hdr, _, herr := app.ParseHeader(r.Fragment)
	if herr != nil || hdr.IsResponse() {
		// Not answerable, so the failure is the indication and not an error to
		// end the session with: the stream that carried it is still good.
		s.iin = s.iin.Set(app.IINParameterError)
		return nil //nolint:nilerr // deliberate: the malformed request is reported, not returned
	}

	switch {
	case !handlesFunc(hdr.Func):
		s.bump(func(st *Stats) { st.UnknownFunction++ })
		s.iin = s.iin.Set(app.IINNoFuncCodeSupport)
	case errors.Is(perr, app.ErrUnknownObject):
		s.iin = s.iin.Set(app.IINObjectUnknown)
	default:
		s.iin = s.iin.Set(app.IINParameterError)
	}

	if hdr.Func == app.FuncConfirm || hdr.Func.NoReply() || r.Broadcast ||
		!hdr.Control.Fir || !hdr.Control.Fin {
		return nil
	}
	return s.respond(w, r, hdr, nil)
}

// handlesFunc reports whether handle dispatches a function code to something
// other than its "not supported" default. It mirrors the switch in handle.
func handlesFunc(f app.FuncCode) bool {
	switch f {
	case app.FuncConfirm, app.FuncRead, app.FuncWrite, app.FuncDelayMeasure,
		app.FuncAuthRequest, app.FuncAuthRequestNoAck, app.FuncAuthenticateFile, app.FuncInitializeData, app.FuncInitializeAppl, app.FuncStartAppl, app.FuncStopAppl, app.FuncSaveConfig, app.FuncActivateConfig,
		app.FuncRecordCurrentTime, app.FuncOpenFile, app.FuncCloseFile,
		app.FuncDeleteFile, app.FuncGetFileInfo, app.FuncAbortFile,
		app.FuncColdRestart, app.FuncWarmRestart,
		app.FuncEnableUnsolicited, app.FuncDisableUnsolicited,
		app.FuncAssignClass, app.FuncSelect, app.FuncOperate,
		app.FuncDirectOperate, app.FuncDirectOperateNR,
		app.FuncImmedFreeze, app.FuncImmedFreezeNR,
		app.FuncFreezeClear, app.FuncFreezeClearNR,
		app.FuncFreezeAtTime, app.FuncFreezeAtTimeNR:
		return true
	}
	return false
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
				s.readDatasets(b, app.ReadAllObjects(87, 1))
			case 2, 3, 4: // event classes 1, 2 and 3
				// A class read names how many events it wants when its range
				// is a count; without one it wants every event in the class.
				limit, ok := eventReadLimit(h)
				if !ok {
					s.iin = s.iin.Set(app.IINParameterError)
					continue
				}
				mask := dnp3.Class1 << (h.Variation - 2)
				selected = append(selected, s.db.events.Select(mask, limit)...)
			}

		case isEventGroup(h.Group):
			// A read of an event group asks for the buffered events of that
			// kind, in the variation named — not the static values of the
			// group's namesake, which is what falling into the static path
			// below would have answered.
			pt, _ := eventTypeForGroup(h.Group)
			if h.Variation != 0 && !eventVariationKnown(pt, h.Variation) {
				s.iin = s.iin.Set(app.IINObjectUnknown)
				continue
			}
			limit, ok := eventReadLimit(h)
			if !ok {
				s.iin = s.iin.Set(app.IINParameterError)
				continue
			}
			evs := s.db.events.SelectType(pt, limit)
			if h.Variation != 0 && pt != dnp3.TypeOctetString {
				for k := range evs {
					evs[k].Variation = h.Variation
				}
			}
			selected = append(selected, evs...)

		case h.Group == 50 && h.Variation == 1:
			s.readCurrentTime(b, h)
		case h.Group == 80 && h.Variation == 1:
			s.readIIN(b, h)
		case h.Group == 50 && h.Variation == 4:
			s.readTimeIntervals(b, h)
		case h.Group >= 85 && h.Group <= 87:
			s.readDatasets(b, h)
		case h.Group == 121:
			s.readSecurityStatistics(b, h)
		case h.Group == 112:
			s.readTerminals(b, h)
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
			// A variation the group does not have would otherwise read as an
			// empty database, and a master could not tell the two apart. An
			// octet string's variation is its length, so it has no table row.
			if h.Variation != 0 && pt != dnp3.TypeOctetString {
				if _, known := objects.Lookup(staticGroupVar(pt, h.Variation)); !known {
					s.iin = s.iin.Set(app.IINObjectUnknown)
					continue
				}
			}
			build := func(start, stop uint16) {
				s.buildStaticRange(b, pt, h.Variation, start, stop)
			}
			if !forEachPointRun(h, build) {
				// A count with no index prefix says how many points but not
				// which. It has always been answered with every point, and
				// still is.
				build(0, 0xFFFF)
			}
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
		case h.Group >= 85 && h.Group <= 87:
			s.writeDatasets(h)
		case h.Group == 0:
			s.writeAttribute(h)
		case h.Group == 50 && h.Variation == 4:
			s.writeTimeIntervals(h)
		case h.Group == 112:
			s.writeTerminals(h)
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
			data, ok := timeWriteData(h)
			if !ok {
				s.iin = s.iin.Set(app.IINParameterError)
				continue
			}
			recorded := objects.ParseTime48(data)
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
			data, ok := timeWriteData(h)
			if !ok {
				s.iin = s.iin.Set(app.IINParameterError)
				continue
			}
			ts := objects.ParseTime48(data)
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
	if now := s.appl.Now(); now.Before(s.restartUntil) {
		// A restart already under way. Doing it again would restart what has
		// not finished restarting, so the request is understood but not
		// repeated, and the delay reported is what is left of the first.
		d = s.restartUntil.Sub(now)
		s.iin = s.iin.Set(app.IINAlreadyExecuting)
	} else {
		if frag.Header.Func == app.FuncColdRestart {
			d = s.appl.ColdRestart()
			s.db.events.Reset()
		} else {
			d = s.appl.WarmRestart()
		}
		if d > 0 {
			s.restartUntil = s.appl.Now().Add(d)
		}
		s.iin = s.iin.Set(app.IINDeviceRestart)
		s.synchronized = false
	}

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
		pt, ok := pointTypeForGroup(h.Group)
		if !ok {
			s.iin = s.iin.Set(app.IINObjectUnknown)
			continue
		}
		// Only the points the header names change class. Assigning the whole
		// type regardless would move every other point too — a request to put
		// two analogs in class 1 silently reclassifying all of them.
		if !forEachPointRun(h, func(start, stop uint16) {
			s.db.assignClassRange(pt, class, start, stop)
		}) {
			// A count with no index prefix does not say which points.
			s.iin = s.iin.Set(app.IINParameterError)
		}
	}
	if r.Broadcast {
		return nil
	}
	return s.respond(w, r, frag.Header, nil)
}

// onFreeze copies the counters a freeze request names into their frozen
// counterparts.
//
// A request with no objects freezes every counter. One naming counters by
// group 20 header freezes only those: freezing the lot regardless overwrites
// frozen values the master never asked to change.
func (s *Session) onFreeze(frag app.Fragment, clear bool) {
	// Every counter frozen by one request shares the one moment of the freeze,
	// taken from the application's clock and only as trustworthy as that
	// clock: synchronized once the master has set it, unsynchronized until.
	at := dnp3.Unsynchronized(s.appl.Now())
	if s.synchronized {
		at = dnp3.Now(at.Time)
	}
	freeze := func(start, stop uint16) { s.db.freezeCountersRange(start, stop, at) }
	if clear {
		freeze = func(start, stop uint16) { s.db.freezeClearCountersRange(start, stop, at) }
	}

	if len(frag.Objects) == 0 {
		freeze(0, 0xFFFF)
		s.db.freezeAnalogsRange(0, 0xFFFF, at, clear)
		return
	}
	for _, h := range frag.Objects {
		if h.Group == 30 {
			if !forEachPointRun(h, func(start, stop uint16) { s.db.freezeAnalogsRange(start, stop, at, clear) }) {
				s.iin = s.iin.Set(app.IINParameterError)
			}
			continue
		}
		if h.Group != 20 {
			s.iin = s.iin.Set(app.IINObjectUnknown)
			continue
		}
		if !forEachPointRun(h, freeze) {
			s.iin = s.iin.Set(app.IINParameterError)
		}
	}
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
	s.lastRespErrors = 0
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

	// The request error indications the original response carried were
	// cleared once it was sent, and the repeat is owed the same answer:
	// without them, a master whose refused request's response was lost would
	// read the replay as that request succeeding.
	s.iin = s.iin.Set(s.lastRespErrors)

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
	s.lastRespErrors = s.iin & app.RequestErrorMask

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
		// The first fragment answers the request under its sequence number and
		// each later one increments it, so a confirm names exactly the
		// fragment it acknowledges.
		Seq: (s.pendingSeq + uint8(i)) % app.SeqModulus,
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
	// broadcast, so it is cleared once reported. The request error
	// indications are the same: they answer one request, and a response that
	// carried them has told the master. Left set, one refused request — a
	// clock write the application does not accept, a read of an object it
	// does not have — would make every later response report the refusal,
	// and a master reads a response carrying NO_FUNC_CODE_SUPPORT or
	// OBJECT_UNKNOWN as its own request refused.
	s.iin = s.iin.Clear(app.IINBroadcast | app.RequestErrorMask)
	return nil
}

// currentIIN assembles the indications to report, folding in the event state
// that changes between responses.
func (s *Session) currentIIN() app.IIN {
	iin := s.iin | app.IIN(s.indications.Load())

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
	case 31, 33:
		return dnp3.TypeFrozenAnalog, true
	case 87, 88:
		return dnp3.TypeDataset, true
	case 112, 113:
		return dnp3.TypeVirtualTerminal, true
	case 30, 32:
		return dnp3.TypeAnalog, true
	case 40, 42:
		return dnp3.TypeAnalogOutputStatus, true
	}
	return dnp3.TypeUnknown, false
}

// isEventGroup reports whether a group is one of the event groups.
func isEventGroup(group uint8) bool {
	_, ok := eventTypeForGroup(group)
	return ok
}

// eventVariationKnown reports whether a variation of an event group exists.
// An octet string event's variation is its length, so it has no table row and
// only the "any" variation is a request that means anything.
func eventVariationKnown(pt dnp3.PointType, variation uint8) bool {
	if pt == dnp3.TypeDataset {
		return variation == 1
	}
	if pt == dnp3.TypeOctetString || pt == dnp3.TypeVirtualTerminal {
		return false
	}
	_, ok := objects.Lookup(objects.GV(eventGroup(pt), variation))
	return ok
}

// eventReadLimit says how many events an event read asks for: the count when
// the range is one, every event when it is "all objects". Any other range —
// a start-stop, or an index prefix — means nothing for events, which have no
// stable index to range over, and is not a limit at all.
func eventReadLimit(h app.ObjectHeader) (int, bool) {
	if h.Qualifier.IndexPrefix() != app.PrefixNone {
		return 0, false
	}
	switch h.Range.Spec {
	case app.RangeAllObjects:
		return math.MaxInt32, true
	case app.RangeCount8, app.RangeCount16, app.RangeCount32:
		return int(h.Range.Count), true
	}
	return 0, false
}
