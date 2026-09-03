// Package multidrop shares one channel between several DNP3 sessions.
//
// A session owns its channel: [master.Session.Run] connects, polls, and
// reconnects on that one stream. Over TCP that is exactly right — every
// outstation gets its own socket — and on a multi-drop serial line it is
// impossible. One RS-485 pair carries every station on the line, and a serial
// port cannot be opened twice: the second session to try gets "device or
// resource busy", or worse, on the platforms that allow it, two sessions
// interleaving frames into the same UART.
//
// A [Bus] sits between them. It opens the shared channel once and hands each
// session a [channel.Channel] of its own, so nothing above changes: a session
// still connects, still reconnects, still owns its stack. The bus routes
// inbound link frames to the session they are addressed to, serialises what
// goes out so two stations' frames cannot interleave, and — because the line is
// half duplex — keeps one master's exchange from starting while another's is
// still in flight.
//
// The same arrangement covers a terminal server: several RTUs behind one TCP
// connection to a serial gateway is the same line with a longer wire.
//
//	port := channel.SerialChannel(cfg, channel.DefaultRetry)
//	bus := multidrop.New(port, multidrop.Config{})
//	defer bus.Close()
//
//	for _, addr := range []uint16{10, 11, 12} {
//		ch, err := bus.Add(multidrop.Station{LocalAddr: 1, RemoteAddr: addr, Master: true})
//		if err != nil {
//			return err
//		}
//		m := master.New(master.Config{LocalAddr: 1, RemoteAddr: addr, UseLinkConfirms: true}, h)
//		go m.Run(ctx, ch)
//	}
//
// What the bus does not do is schedule the sessions against each other. Each
// master still polls on its own clock; the bus only stops their exchanges from
// overlapping. Three masters polling a slow line every second will spend their
// time waiting for each other — pace the polls, and give the line the time it
// needs.
//
// That example works when one piece of code owns the whole line and knows to
// build the Bus once. When two independent parts of a program each reach for
// the same device — a master here, a simulator there, neither aware of the
// other — each doing the right thing on its own reproduces the very problem
// multidrop exists to solve: two Buses, two opens, the second refused by the
// OS. A [Registry] is where such callers meet: each asks it for a bus by the
// channel it would open, and a caller asking for one that is already open
// gets the existing bus back instead of a second one.
package multidrop

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/dscsystems/go-dnp3/channel"
	"github.com/dscsystems/go-dnp3/internal/link"
	"github.com/dscsystems/go-dnp3/internal/transport"
)

// Defaults for [Config].
const (
	// DefaultTurnaround is how long a master keeps the line after transmitting
	// while it waits for the outstation to answer.
	//
	// It bounds the damage a silent outstation does: the line is idle for this
	// long before another master may transmit. Too short and a slow device's
	// reply collides with the next master's request; too long and one dead
	// station paces the whole line. Two seconds is a compromise for 9600 baud,
	// where a maximum-length response alone takes a third of a second.
	DefaultTurnaround = 2 * time.Second

	// DefaultQueue is how many frames may be waiting for one station before
	// further frames are dropped.
	DefaultQueue = 16
)

// Config parameterises a bus.
type Config struct {
	// Turnaround is how long a master holds the line after transmitting a
	// request, waiting for the reply. The hold ends early as soon as the reply
	// arrives complete, so this only governs stations that do not answer.
	//
	// Zero uses [DefaultTurnaround]. A negative value disables arbitration
	// altogether, which is right only when the far end is not a shared medium —
	// a gateway that queues and turns the line around itself.
	Turnaround time.Duration

	// Queue is how many frames may be waiting for one station. Zero uses
	// [DefaultQueue].
	//
	// A queue only fills when a session has stopped reading, which means it is
	// wedged or shutting down; frames beyond it are dropped and counted rather
	// than stalling the bus for every other station.
	Queue int

	// Log receives bus events. Nil discards them.
	Log *slog.Logger
}

func (c *Config) applyDefaults() {
	switch {
	case c.Turnaround == 0:
		c.Turnaround = DefaultTurnaround
	case c.Turnaround < 0:
		c.Turnaround = 0 // arbitration disabled
	}
	if c.Queue <= 0 {
		c.Queue = DefaultQueue
	}
	if c.Log == nil {
		c.Log = slog.New(slog.DiscardHandler)
	}
}

// Station identifies one session's place on the bus.
//
// The addresses must match the session's own configuration: they are what the
// bus routes on, and a station listening on an address its session does not
// use would collect frames the session then discards.
type Station struct {
	// LocalAddr is the session's own link address, and RemoteAddr the station
	// it talks to. They are [master.Config.LocalAddr] and
	// [master.Config.RemoteAddr], or the outstation equivalents.
	LocalAddr  uint16
	RemoteAddr uint16

	// Master says the session is a master.
	//
	// It decides two things. Masters sharing a line normally share one link
	// address, so a master is routed by source address — whose reply this is —
	// while an outstation is routed by destination and answers whichever master
	// addressed it. And only a master takes the line: it is the one that starts
	// an exchange, so it is the one that has to wait its turn.
	Master bool
}

func (s Station) String() string {
	role := "outstation"
	if s.Master {
		role = "master"
	}
	return fmt.Sprintf("%s %d↔%d", role, s.LocalAddr, s.RemoteAddr)
}

// conflicts reports whether a frame could match both stations, which makes the
// pair unroutable.
func (s Station) conflicts(o Station) bool {
	if s.LocalAddr != o.LocalAddr {
		return false
	}
	// An outstation accepts every source, so it is indistinguishable from
	// anything else sharing its address. Two masters are told apart by whose
	// outstation answered.
	if !s.Master || !o.Master {
		return true
	}
	return s.RemoteAddr == o.RemoteAddr
}

// Stats counts what a bus has carried.
type Stats struct {
	// Connections counts how many times the underlying channel connected.
	Connections uint64

	// FramesRouted and FramesUnrouted count frames delivered to at least one
	// station and frames addressed to nobody on this bus — no station has that
	// address, or the one that does has no session connected, which to the line
	// looks the same as a device that is not powered up. Neither is an error on
	// a line shared with equipment this program does not own, but a steady
	// climb with no traffic reaching a session means an address is wrong.
	FramesRouted   uint64
	FramesUnrouted uint64
	// FramesDropped counts frames discarded because a station's queue was
	// full — a session that has stopped reading.
	FramesDropped uint64

	// The link parser's own counters, accumulated across connections. A
	// per-session Stats cannot report these: the bus decodes the stream, so its
	// sessions never see the octets that failed to make a frame.
	FramesDecoded   uint64
	BytesDiscarded  uint64
	HeaderCRCErrors uint64
	BodyCRCErrors   uint64
	BadLength       uint64
	Resyncs         uint64
}

// Bus shares one channel between several sessions.
type Bus struct {
	ch  channel.Channel
	cfg Config
	log *slog.Logger
	arb arbiter

	// The pump owns the underlying connection. It starts when the first
	// station connects, so a bus that is built and never used opens nothing.
	once   sync.Once
	ctx    context.Context
	cancel context.CancelFunc

	// wmu serialises transmission. A stack writes one complete frame per call,
	// so holding this for the duration of one Write is what keeps two stations'
	// frames from interleaving on the wire.
	wmu sync.Mutex

	mu       sync.Mutex
	stations []*station
	conn     io.ReadWriteCloser
	// up is closed when a connection becomes available and replaced when one
	// drops, which is how a station's Connect waits for the line without
	// polling. dead is closed once, when the bus stops for good.
	up       chan struct{}
	dead     chan struct{}
	deadOnce sync.Once
	// stopped is set when the pump has finished for good, and err says why if
	// it was a failure. A station that connects after that gets the answer
	// rather than waiting for a line that is never coming back.
	stopped bool
	err     error
	closed  bool
	stats   Stats
}

// New returns a bus over ch.
//
// The bus takes ownership of ch: [Bus.Close] closes it, and nothing else should
// connect it.
func New(ch channel.Channel, cfg Config) *Bus {
	cfg.applyDefaults()
	ctx, cancel := context.WithCancel(context.Background())
	b := &Bus{
		ch:     ch,
		cfg:    cfg,
		log:    cfg.Log.With("bus", ch.String()),
		ctx:    ctx,
		cancel: cancel,
		up:     make(chan struct{}),
		dead:   make(chan struct{}),
	}
	b.arb.init(cfg.Turnaround)
	return b
}

// Add puts a station on the bus and returns the channel its session runs on.
//
// It fails if the station cannot be told apart from one already added: two
// masters polling the same outstation, or two outstations at one address, would
// both match the same frames and there is no answer to which of them should
// have it.
func (b *Bus) Add(s Station) (channel.Channel, error) {
	if link.IsBroadcast(s.LocalAddr) || link.IsReserved(s.LocalAddr) {
		return nil, fmt.Errorf("multidrop: %d is not a usable local address", s.LocalAddr)
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return nil, channel.ErrClosed
	}
	for _, existing := range b.stations {
		if existing.addr.conflicts(s) {
			return nil, fmt.Errorf("multidrop: %s cannot be told apart from %s", s, existing.addr)
		}
	}

	st := &station{bus: b, addr: s}
	b.stations = append(b.stations, st)
	return st, nil
}

// Stats returns a snapshot of the bus counters.
func (b *Bus) Stats() Stats {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.stats
}

// Close shuts the bus down, closing the underlying channel.
//
// Every session on it sees its connection drop and its next connect report
// [channel.ErrClosed], which both sessions treat as a clean shutdown.
func (b *Bus) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	conn := b.conn
	b.conn = nil
	live := b.detachAllLocked()
	b.mu.Unlock()

	b.die(nil)
	b.cancel()
	b.arb.close()
	for _, c := range live {
		c.teardown()
	}
	// Closing the connection as well as the channel is what unblocks the pump's
	// read: a TCP channel does not own the connections it dialled, so closing
	// it alone would leave the pump parked in Read until the peer noticed.
	if conn != nil {
		_ = conn.Close()
	}
	return b.ch.Close()
}

func (b *Bus) String() string { return "multidrop " + b.ch.String() }

func (b *Bus) bump(fn func(*Stats)) {
	b.mu.Lock()
	fn(&b.stats)
	b.mu.Unlock()
}

// start launches the pump on the first connect.
func (b *Bus) start() {
	b.once.Do(func() { go b.pump() })
}

// die records why the bus stopped and wakes everyone waiting on it.
func (b *Bus) die(err error) {
	b.mu.Lock()
	b.stopped = true
	if b.err == nil {
		b.err = err
	}
	b.mu.Unlock()
	b.deadOnce.Do(func() { close(b.dead) })
}

// pump owns the underlying connection: it connects, reads, routes, and
// reconnects, for the life of the bus.
func (b *Bus) pump() {
	buf := make([]byte, link.MaxFrameSize)

	for {
		if b.ctx.Err() != nil {
			b.die(nil)
			return
		}

		conn, err := b.ch.Connect(b.ctx)
		if err != nil {
			// Cancellation and a closed channel are a shutdown, not a failure;
			// anything else stops the bus and is reported to every station, so
			// a misconfigured port surfaces as a session error rather than as
			// sessions that never connect.
			if b.ctx.Err() != nil || errors.Is(err, channel.ErrClosed) {
				b.die(nil)
			} else {
				b.die(fmt.Errorf("multidrop: connect: %w", err))
			}
			return
		}

		if !b.attach(conn) {
			// The bus was closed while this connection was being made. Nobody
			// is going to use it, and leaving it open would leave the pump
			// reading a line the bus has given up.
			_ = conn.Close()
			b.die(nil)
			return
		}
		b.log.Info("bus connected")

		// A fresh parser per connection: buffered octets from a line that has
		// just dropped are half a frame that will never be completed.
		parser := link.NewParser()
		b.read(conn, parser, buf)

		b.detach(conn)
		b.log.Info("bus disconnected")
	}
}

// attach publishes a new connection and wakes the stations waiting for one. It
// reports false if the bus closed while the connection was being made.
func (b *Bus) attach(conn io.ReadWriteCloser) bool {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return false
	}
	b.conn = conn
	b.stats.Connections++
	up := b.up
	b.mu.Unlock()
	close(up)
	return true
}

// detach retires a connection, dropping every station's connection with it.
func (b *Bus) detach(conn io.ReadWriteCloser) {
	b.mu.Lock()
	b.conn = nil
	b.up = make(chan struct{})
	live := b.detachAllLocked()
	b.mu.Unlock()

	for _, c := range live {
		c.teardown()
	}
	_ = conn.Close()
}

// detachAllLocked clears every station's current connection and returns them
// for the caller to tear down outside the lock.
func (b *Bus) detachAllLocked() []*conn {
	live := make([]*conn, 0, len(b.stations))
	for _, s := range b.stations {
		if s.cur != nil {
			live = append(live, s.cur)
			s.cur = nil
		}
	}
	return live
}

// read pumps one connection until it fails.
func (b *Bus) read(conn io.Reader, p *link.Parser, buf []byte) {
	var prev link.Stats

	for {
		n, rerr := conn.Read(buf)
		if n > 0 {
			b.feed(p, buf[:n])
			b.syncParserStats(p, &prev)
		}
		if rerr != nil {
			if !errors.Is(rerr, io.EOF) && b.ctx.Err() == nil {
				b.log.Warn("bus read failed", "err", rerr)
			}
			return
		}
	}
}

// feed pushes received octets through the parser, routing every frame they
// complete.
//
// The read buffer holds one maximum frame and every complete frame is drained
// before the next read, so the parser — which holds two — always has room. The
// loop and the refusal check stay anyway: octets dropped here would surface as
// frames that vanished with nothing in the log to say why.
func (b *Bus) feed(p *link.Parser, data []byte) {
	for len(data) > 0 {
		n, werr := p.Write(data)
		data = data[n:]

		for {
			f, err := p.Next()
			if err != nil {
				if !errors.Is(err, link.ErrNeedMore) {
					b.log.Warn("bus decode failed", "err", err)
				}
				break
			}
			b.route(f)
		}

		if werr != nil && n == 0 {
			// The parser took nothing and draining freed nothing, so the rest
			// of this read has nowhere to go. Giving up beats spinning.
			b.log.Warn("bus parser is full; octets dropped",
				"dropped", len(data), "err", werr)
			return
		}
	}
}

// syncParserStats folds the parser's counters into the bus totals.
//
// They are added as deltas because the parser is replaced on every reconnect,
// and a counter that goes backwards when a cable is re-seated is worse than no
// counter at all.
func (b *Bus) syncParserStats(p *link.Parser, prev *link.Stats) {
	cur := p.Stats()
	b.bump(func(st *Stats) {
		st.FramesDecoded += cur.FramesDecoded - prev.FramesDecoded
		st.BytesDiscarded += cur.BytesDiscarded - prev.BytesDiscarded
		st.HeaderCRCErrors += cur.HeaderCRCErrors - prev.HeaderCRCErrors
		st.BodyCRCErrors += cur.BodyCRCErrors - prev.BodyCRCErrors
		st.BadLength += cur.BadLength - prev.BadLength
		st.Resyncs += cur.Resyncs - prev.Resyncs
	})
	*prev = cur
}

// route delivers one frame to the stations it belongs to.
func (b *Bus) route(f link.Frame) {
	b.mu.Lock()
	targets := make([]*conn, 0, len(b.stations))
	for _, s := range b.stations {
		if s.matches(f.Header) && s.cur != nil {
			targets = append(targets, s.cur)
		}
	}
	if len(targets) == 0 {
		b.stats.FramesUnrouted++
		b.mu.Unlock()
		b.log.Debug("frame for nobody on the bus",
			"src", f.Header.Src, "dest", f.Header.Dest)
		return
	}
	b.stats.FramesRouted++
	b.mu.Unlock()

	// The frame is re-encoded rather than forwarded verbatim: the parser hands
	// back a decoded frame, not the octets it consumed. Every CRC was just
	// verified and the payload is unchanged, so what goes to the session is the
	// same frame it would have read off the wire.
	out, err := link.Encode(nil, f.Header, f.Payload)
	if err != nil {
		b.log.Warn("bus re-encode failed", "err", err)
		return
	}

	// A fragment is complete when the transport header says so. That is what
	// ends a master's turn: everything before it is more of the same reply, and
	// releasing the line early would let another master transmit into the
	// middle of it.
	complete := len(f.Payload) > 0 && transport.ParseHeader(f.Payload[0]).Fin

	for _, c := range targets {
		if !c.push(out) {
			b.bump(func(st *Stats) { st.FramesDropped++ })
			b.log.Warn("station queue full; frame dropped", "station", c.st.addr)
		}
		b.arb.observe(c, complete)
	}
}
