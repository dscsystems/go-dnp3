package master

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/channel"
	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/internal/stack"
	"github.com/dscsystems/go-dnp3/objects"
)

// Config parameterises a master session.
type Config struct {
	// LocalAddr is this master's link address; RemoteAddr is the
	// outstation's.
	LocalAddr  uint16
	RemoteAddr uint16

	// ResponseTimeout is how long to wait for an outstation to answer.
	ResponseTimeout time.Duration
	// TaskRetryPeriod is how long to wait before retrying a failed task.
	TaskRetryPeriod time.Duration

	// IntegrityOnStartup runs a class 0+1+2+3 poll when the session starts and
	// whenever the outstation reports a restart.
	IntegrityOnStartup bool
	// DisableUnsolOnStartup sends a disable-unsolicited request before the
	// integrity poll, which is the standard's startup sequence.
	DisableUnsolOnStartup bool
	// UnsolClassMask is the set of classes to enable for unsolicited
	// reporting after the integrity poll. Zero enables none.
	UnsolClassMask dnp3.Class

	// MaxTxFragment and MaxRxFragment cap request and response fragments.
	MaxTxFragment int
	MaxRxFragment int

	// UseLinkConfirms enables link-layer confirmation, normally off over TCP.
	UseLinkConfirms bool
	// LinkRetries is how many times a confirmed frame is retransmitted.
	LinkRetries int
	// LinkTimeout is how long to wait for a link-layer acknowledgement before
	// retransmitting. It matters only when UseLinkConfirms is set.
	LinkTimeout time.Duration

	// KeepAlive probes an idle link this often with a link status request.
	// Zero disables it.
	//
	// An idle TCP connection is indistinguishable from a peer that has gone
	// away: both are silent. Without a probe, a master notices only when its
	// next poll times out, which on a slow schedule can be minutes.
	KeepAlive time.Duration

	// Log receives protocol and session events. Nil discards them.
	Log *slog.Logger
}

func (c *Config) applyDefaults() {
	if c.ResponseTimeout <= 0 {
		c.ResponseTimeout = 5 * time.Second
	}
	if c.TaskRetryPeriod <= 0 {
		c.TaskRetryPeriod = 5 * time.Second
	}
	if c.MaxTxFragment <= 0 {
		c.MaxTxFragment = app.DefaultMaxFragment
	}
	if c.MaxRxFragment <= 0 {
		c.MaxRxFragment = app.DefaultMaxFragment
	}
	if c.LinkTimeout <= 0 {
		c.LinkTimeout = time.Second
	}
	if c.Log == nil {
		c.Log = slog.New(slog.DiscardHandler)
	}
}

// Stats counts what a session has done.
type Stats struct {
	TasksRun        uint64
	TasksSucceeded  uint64
	TasksFailed     uint64
	ResponseTimeout uint64
	FragmentsRx     uint64
	Unsolicited     uint64
	Connections     uint64
	RestartsSeen    uint64
}

// Session is a master's connection to one outstation.
//
// All protocol state lives in the goroutine started by [Session.Run]. The
// request methods are safe to call from anywhere: they hand a task to that
// goroutine and wait for it to finish.
type Session struct {
	cfg     Config
	handler Handler
	log     *slog.Logger
	stack   *stack.Stack

	// seq is the application sequence number for solicited requests.
	seq uint8
	// unsolSeq tracks the outstation's unsolicited sequence space, which is
	// separate from the solicited one.
	unsolSeq    uint8
	hasUnsolSeq bool

	// synchronized mirrors the outstation's clock state, taken from NEED_TIME.
	synchronized bool

	sched    scheduler
	inflight *task

	submit chan *task
	mu     sync.Mutex
	stats  Stats

	connected bool
	lastIIN   app.IIN

	// lastRx is when octets last arrived, which paces the keep-alive, and
	// linkDeadline is when an unacknowledged link frame should be retried.
	lastRx       time.Time
	linkDeadline time.Time

	// startupActive is set while the startup sequence is in flight. It has to
	// exist because the sequence is triggered by an indication that is still
	// set until the sequence's own first step clears it: without the guard,
	// every response arriving mid-sequence starts another one.
	startupActive bool
}

// New returns a master session. Pass a nil Handler for [NopHandler].
func New(cfg Config, h Handler) *Session {
	cfg.applyDefaults()
	if h == nil {
		h = NopHandler{}
	}
	return &Session{
		cfg:     cfg,
		handler: h,
		log:     cfg.Log.With("role", "master", "outstation", cfg.RemoteAddr),
		submit:  make(chan *task, 32),
		// Assume the outstation's clock is good until it says otherwise, so a
		// capture from a healthy device is not littered with unsynchronized
		// stamps.
		synchronized: true,
	}
}

// Stats returns a snapshot of the session counters.
func (s *Session) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}

// LastIIN returns the internal indications from the most recent response.
func (s *Session) LastIIN() app.IIN {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastIIN
}

func (s *Session) bump(fn func(*Stats)) {
	s.mu.Lock()
	fn(&s.stats)
	s.mu.Unlock()
}

// Run connects and polls until the context is cancelled.
func (s *Session) Run(ctx context.Context, ch channel.Channel) error {
	s.stack = stack.New(stack.Config{
		LocalAddr:     s.cfg.LocalAddr,
		RemoteAddr:    s.cfg.RemoteAddr,
		IsMaster:      true,
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
			return fmt.Errorf("master: connect: %w", err)
		}

		s.bump(func(st *Stats) { st.Connections++ })
		s.log.Info("connected", "channel", ch.String())

		s.stack.Reset()
		s.setConnected(true)
		s.lastRx = time.Now()
		s.startupSequence()

		s.serve(ctx, conn)

		s.setConnected(false)
		_ = conn.Close()
		s.failInflight(dnp3.ErrNoConnection)
		s.log.Info("disconnected")
	}
}

func (s *Session) setConnected(v bool) {
	s.mu.Lock()
	s.connected = v
	s.mu.Unlock()
}

// Connected reports whether a connection is currently established.
func (s *Session) Connected() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.connected
}

// startupSequence queues the tasks the standard requires after connecting or
// after the outstation reports a restart.
//
// The order is not negotiable: clear the restart indication, stop unsolicited
// reporting, take a complete picture, then re-enable unsolicited. Running the
// integrity poll before disabling unsolicited would race an event stream
// against the poll and produce a picture that is neither.
func (s *Session) startupSequence() {
	s.sched.clear()

	// The steps are chained rather than queued so nothing can be interleaved
	// between them. Queuing them relies on scheduler ordering to keep the
	// sequence intact, and a user-submitted poll arriving mid-sequence would
	// then land between the disable and the integrity read — exactly the race
	// the ordering exists to prevent.
	var steps []*task
	steps = append(steps, newClearRestartTask())
	if s.cfg.DisableUnsolOnStartup {
		steps = append(steps, newUnsolicitedTask(false, dnp3.Class123))
	}
	if s.cfg.IntegrityOnStartup {
		steps = append(steps, newScanTask(dnp3.ClassAll, s.handler))
	}
	if s.cfg.UnsolClassMask != 0 {
		steps = append(steps, newUnsolicitedTask(true, s.cfg.UnsolClassMask))
	}

	for i, t := range steps {
		t.startup = true
		if i < len(steps)-1 {
			next := steps[i+1]
			t.next = func() *task { return next }
		}
	}

	s.startupActive = true
	s.enqueue(steps[0])
}

// enqueue schedules a task to run as soon as the session is free.
func (s *Session) enqueue(t *task) {
	t.due = time.Now()
	s.sched.push(t)
}

// serve runs one connection.
func (s *Session) serve(ctx context.Context, conn io.ReadWriteCloser) {
	// The read goroutine only moves octets. Everything that touches protocol
	// state — the link state machines, the reassembler, the scheduler — runs
	// on this goroutine, so the stack needs no locking and a send can never
	// interleave with the processing of an inbound frame.
	rx := make(chan []byte, 8)
	readErr := make(chan error, 1)
	go readInto(ctx, conn, rx, readErr)

	timer := time.NewTimer(time.Hour)
	defer timer.Stop()

	for {
		s.runDueTask(conn)
		resetTimer(timer, s.nextWakeup())

		select {
		case <-ctx.Done():
			return

		case err := <-readErr:
			if err != nil && !errors.Is(err, io.EOF) {
				s.log.Warn("read loop ended", "err", err)
			}
			return

		case t := <-s.submit:
			s.enqueue(t)

		case data := <-rx:
			s.lastRx = time.Now()
			if err := s.stack.Receive(conn, data, func(r stack.Received) {
				s.onFragment(conn, r)
			}); err != nil {
				s.log.Warn("receive failed", "err", err)
				return
			}

		case <-timer.C:
			if s.checkLinkTimeout(conn) {
				continue
			}
			s.checkTimeout()
			s.checkKeepAlive(conn)
		}
	}
}

// readInto moves octets from the connection to the session goroutine.
func readInto(ctx context.Context, r io.Reader, out chan<- []byte, errc chan<- error) {
	buf := make([]byte, stack.ReadChunk)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			// Copy: the buffer is reused on the next read.
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

// checkLinkTimeout retransmits an unacknowledged link frame. It reports
// whether the link layer handled the tick, so the caller does not also age out
// the application request that is still legitimately in flight.
func (s *Session) checkLinkTimeout(w io.Writer) bool {
	if !s.stack.Pending() || time.Now().Before(s.linkDeadline) {
		return false
	}
	failed, err := s.stack.OnTimeout(w)
	if err != nil {
		s.log.Warn("link retransmission failed", "err", err)
		return false
	}
	s.linkDeadline = time.Now().Add(s.cfg.LinkTimeout)
	if failed {
		s.log.Warn("link layer gave up on a frame")
		s.failInflight(dnp3.ErrTimeout)
		return false
	}
	return true
}

// checkKeepAlive probes an idle link so a peer that has gone away is noticed
// before the next poll is due.
func (s *Session) checkKeepAlive(w io.Writer) {
	if s.cfg.KeepAlive <= 0 || s.inflight != nil || s.stack.Busy() {
		return
	}
	if time.Since(s.lastRx) < s.cfg.KeepAlive {
		return
	}
	s.lastRx = time.Now()
	if err := s.stack.SendLinkStatusRequest(w); err != nil {
		s.log.Warn("keep-alive failed", "err", err)
		return
	}
	s.linkDeadline = time.Now().Add(s.cfg.LinkTimeout)
	s.log.Debug("keep-alive sent")
}

// nextWakeup returns how long to sleep before something needs doing.
func (s *Session) nextWakeup() time.Duration {
	now := time.Now()

	if s.inflight != nil {
		return max(s.inflight.deadline.Sub(now), time.Millisecond)
	}
	if s.stack != nil && s.stack.Pending() {
		return max(s.linkDeadline.Sub(now), time.Millisecond)
	}
	if t, ok := s.sched.peek(); ok {
		return max(t.due.Sub(now), time.Millisecond)
	}
	if s.cfg.KeepAlive > 0 {
		return max(s.lastRx.Add(s.cfg.KeepAlive).Sub(now), time.Millisecond)
	}
	return time.Hour
}

func resetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}

// runDueTask sends the next due task if nothing is in flight.
func (s *Session) runDueTask(w io.Writer) {
	if s.inflight != nil {
		return
	}
	t, ok := s.sched.peek()
	if !ok || t.due.After(time.Now()) {
		return
	}
	s.sched.pop()
	s.sendTask(w, t)
}

// sendTask builds and transmits one task, making it the in-flight request.
//
// Chained tasks come through here too, which is what keeps a select and its
// operate on consecutive sequence numbers.
func (s *Session) sendTask(w io.Writer, t *task) {
	s.seq = (s.seq + 1) % app.SeqModulus

	b := app.NewBuilder(s.cfg.MaxTxFragment)
	if err := b.SetHeader(app.Header{
		Control: app.Control{Fir: true, Fin: true, Seq: s.seq},
		Func:    t.funcCode,
	}); err != nil {
		s.completeTask(t, err)
		return
	}
	if err := t.build(b); err != nil {
		s.completeTask(t, err)
		return
	}

	if err := s.stack.Send(w, b.Bytes()); err != nil {
		s.log.Warn("send failed", "task", t.name, "err", err)
		s.completeTask(t, err)
		return
	}

	t.seq = s.seq
	s.bump(func(st *Stats) { st.TasksRun++ })
	s.log.Debug("task sent", "task", t.name, "seq", s.seq)

	if t.noResponse {
		// Nothing will come back, so there is nothing to wait for.
		s.inflight = nil
		s.completeTask(t, nil)
		return
	}

	t.deadline = time.Now().Add(s.cfg.ResponseTimeout)
	s.inflight = t
}

// checkTimeout fails the in-flight task when the outstation did not answer.
func (s *Session) checkTimeout() {
	if s.inflight == nil || time.Now().Before(s.inflight.deadline) {
		return
	}
	t := s.inflight
	s.inflight = nil
	s.bump(func(st *Stats) { st.ResponseTimeout++ })
	s.log.Warn("response timeout", "task", t.name, "seq", t.seq)
	s.completeTask(t, dnp3.ErrTimeout)
}

// completeTask finishes a task, rescheduling it if it is periodic.
func (s *Session) completeTask(t *task, err error) {
	// The startup sequence ends when its last step finishes, or when any step
	// fails — leaving the flag set would suppress the re-baseline a genuine
	// later restart needs.
	if t.startup && (err != nil || t.next == nil) {
		s.startupActive = false
	}

	if err != nil {
		s.bump(func(st *Stats) { st.TasksFailed++ })
	} else {
		s.bump(func(st *Stats) { st.TasksSucceeded++ })
	}

	if t.period > 0 {
		// A periodic task keeps its slot in the schedule whether or not this
		// run succeeded; a failed poll should not stop the next one.
		next := *t
		next.due = time.Now().Add(t.period)
		next.done = nil
		s.sched.push(&next)
	}
	t.finish(err)
}

// failInflight abandons the in-flight task when the connection drops.
func (s *Session) failInflight(err error) {
	if s.inflight == nil {
		return
	}
	t := s.inflight
	s.inflight = nil
	s.completeTask(t, err)
}

// onFragment routes an incoming fragment.
func (s *Session) onFragment(w io.Writer, r stack.Received) {
	s.bump(func(st *Stats) { st.FragmentsRx++ })

	frag, err := app.ParseFragment(nil, r.Fragment)
	if err != nil {
		s.log.Warn("malformed response", "err", err)
		return
	}
	if !frag.Header.IsResponse() {
		s.log.Debug("ignoring a non-response fragment", "func", frag.Header.Func)
		return
	}

	s.observeIIN(frag.Header.IIN)

	if frag.Header.Func == app.FuncUnsolicitedResponse {
		s.onUnsolicited(w, frag)
		return
	}
	s.onSolicited(w, frag)
}

// observeIIN reacts to the indications on every response.
func (s *Session) observeIIN(iin app.IIN) {
	s.mu.Lock()
	s.lastIIN = iin
	s.mu.Unlock()

	// NEED_TIME means the outstation's clock is not set, so the timestamps it
	// reports cannot be treated as synchronized.
	s.synchronized = !iin.Has(app.IINNeedTime)

	if iin.Has(app.IINDeviceRestart) {
		if s.startupActive {
			// The sequence already running is the response to this. Its first
			// step is the write that clears the indication, so every fragment
			// until then still carries it — reacting again would restart the
			// sequence on its own output, indefinitely.
			return
		}
		s.bump(func(st *Stats) { st.RestartsSeen++ })
		s.log.Info("outstation reported a restart; re-running the startup sequence")
		// The outstation's event buffer is gone, so the master's picture is
		// stale in a way no incremental poll can fix. Only a full re-baseline
		// restores it.
		s.startupSequence()
	}
}

// onSolicited handles a response to a request we sent.
func (s *Session) onSolicited(w io.Writer, frag app.Fragment) {
	t := s.inflight
	if t == nil {
		s.log.Debug("response with nothing in flight", "seq", frag.Header.Control.Seq)
		return
	}
	if frag.Header.Control.Seq != t.seq {
		// A response for a request we have already given up on. Acting on it
		// would attribute stale data to the current poll.
		s.log.Debug("response sequence mismatch",
			"got", frag.Header.Control.Seq, "want", t.seq)
		return
	}

	s.deliver(frag, false)
	if t.onFragment != nil {
		t.onFragment(frag)
	}

	if frag.Header.Control.Con {
		s.sendConfirm(w, frag.Header.Control.Seq, false)
	}

	if !frag.Header.Control.Fin {
		// More fragments are coming. Extend the deadline rather than
		// completing, so a large integrity poll is not cut off midway.
		t.deadline = time.Now().Add(s.cfg.ResponseTimeout)
		return
	}

	s.inflight = nil
	if t.onDone != nil {
		t.onDone(frag.Header.IIN)
	}

	// A chained task runs immediately rather than going back to the
	// scheduler, so nothing can be interleaved between the two.
	if t.next != nil {
		if nt := t.next(); nt != nil {
			nt.done, t.done = t.done, nil
			s.completeTask(t, nil)
			s.sendTask(w, nt)
			return
		}
	}
	s.completeTask(t, nil)
}

// onUnsolicited handles a fragment the outstation sent on its own.
func (s *Session) onUnsolicited(w io.Writer, frag app.Fragment) {
	s.bump(func(st *Stats) { st.Unsolicited++ })

	seq := frag.Header.Control.Seq
	duplicate := s.hasUnsolSeq && seq == s.unsolSeq
	s.unsolSeq, s.hasUnsolSeq = seq, true

	if duplicate {
		// Our confirm was lost, not the data. Confirm again but do not deliver
		// the measurements a second time.
		s.log.Debug("duplicate unsolicited response", "seq", seq)
	} else {
		s.deliver(frag, true)
	}

	if frag.Header.Control.Con {
		s.sendConfirm(w, seq, true)
	}
}

// sendConfirm answers a fragment that asked to be confirmed.
func (s *Session) sendConfirm(w io.Writer, seq uint8, unsolicited bool) {
	frag := app.AppendHeader(nil, app.Header{
		Control: app.Control{Fir: true, Fin: true, Uns: unsolicited, Seq: seq},
		Func:    app.FuncConfirm,
	})
	if err := s.stack.Send(w, frag); err != nil {
		s.log.Warn("confirm failed", "err", err, "seq", seq)
	}
}

// deliver decodes a fragment's measurements and hands them to the handler.
func (s *Session) deliver(frag app.Fragment, unsolicited bool) {
	info := ResponseInfo{
		IIN:         frag.Header.IIN,
		Unsolicited: unsolicited,
		Sequence:    frag.Header.Control.Seq,
		Received:    time.Now(),
	}

	s.handler.BeginFragment(info)
	defer s.handler.EndFragment(info)

	ctx := objects.Context{Synchronized: s.synchronized}
	for _, h := range frag.Objects {
		// A group 51 object sets the base for the relative-time events that
		// follow it in this fragment.
		if h.Group == 51 && len(h.Data) >= objects.Time48Size {
			ctx = ctx.WithCTO(objects.ParseTime48(h.Data).Time)
			continue
		}
		s.dispatch(h, ctx)
	}
}
