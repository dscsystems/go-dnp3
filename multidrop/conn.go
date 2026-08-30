package multidrop

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"

	"github.com/dscsystems/go-dnp3/channel"
	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/internal/link"
	"github.com/dscsystems/go-dnp3/internal/transport"
)

// errDisconnected is what a station's connection reports once the line under it
// has gone. It ends the session's connection the same way a socket error would,
// and the session reconnects — onto whatever connection the bus has by then.
var errDisconnected = errors.New("multidrop: the bus connection dropped")

// station is one session's view of the bus. It satisfies [channel.Channel], so
// a session cannot tell it from a socket.
type station struct {
	bus  *Bus
	addr Station

	// cur is the station's live connection and removed marks a station taken
	// off the bus. Both are guarded by the bus mutex, which is also what makes
	// routing and reconnection agree on which connection is current.
	cur     *conn
	removed bool
}

// matches reports whether a frame belongs to this station. The caller holds the
// bus mutex.
func (s *station) matches(h link.Header) bool {
	if s.addr.Master {
		// Masters on one line normally share a link address — the line is one
		// master station with several sessions on it — so the destination says
		// almost nothing and the source is what separates one outstation's
		// reply from another's. Broadcasts are deliberately not matched:
		// masters send them, nobody sends them to a master.
		return h.Dest == s.addr.LocalAddr && h.Src == s.addr.RemoteAddr
	}
	// An outstation answers whichever master addressed it, so the destination
	// decides on its own. Its RemoteAddr says where unsolicited responses go;
	// it is not a filter on what it will accept, and making it one would leave
	// a second master on the line unable to poll.
	return h.Dest == s.addr.LocalAddr || link.IsBroadcast(h.Dest)
}

// Connect waits for the bus to have a line and returns this station's view of
// it.
func (s *station) Connect(ctx context.Context) (io.ReadWriteCloser, error) {
	b := s.bus
	b.start()

	for {
		b.mu.Lock()
		switch {
		case b.err != nil:
			err := b.err
			b.mu.Unlock()
			return nil, err

		case b.closed, b.stopped, s.removed:
			b.mu.Unlock()
			return nil, channel.ErrClosed

		case b.conn != nil:
			c := &conn{
				st:   s,
				bus:  b,
				w:    b.conn,
				rx:   make(chan []byte, b.cfg.Queue),
				done: make(chan struct{}),
			}
			prev := s.cur
			s.cur = c
			b.mu.Unlock()
			// A session that reconnects while the line is still up abandons its
			// previous connection, which must not be left holding the line.
			if prev != nil {
				prev.teardown()
			}
			return c, nil
		}

		up := b.up
		b.mu.Unlock()

		select {
		case <-up:
		case <-b.dead:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// Close takes the station off the bus. It does not close the underlying
// channel — the bus owns that, and other stations are still using it.
func (s *station) Close() error {
	b := s.bus

	b.mu.Lock()
	s.removed = true
	cur := s.cur
	s.cur = nil
	if i := slices.Index(b.stations, s); i >= 0 {
		b.stations = slices.Delete(b.stations, i, i+1)
	}
	b.mu.Unlock()

	if cur != nil {
		cur.teardown()
	}
	return nil
}

func (s *station) String() string {
	return fmt.Sprintf("multidrop %s [%s]", s.bus.ch.String(), s.addr)
}

// conn is one station's connection: frames routed to it on the way in, the
// shared line on the way out.
type conn struct {
	st  *station
	bus *Bus
	w   io.Writer

	// rx carries whole frames from the bus pump; rem is what is left of the
	// frame a partial Read did not take.
	rx   chan []byte
	rem  []byte
	done chan struct{}
	once sync.Once
}

// Read returns the next octets routed to this station, blocking until there are
// some.
//
// A session reads with a buffer of [link.MaxFrameSize], so a frame normally
// comes back in one call; a smaller buffer is served across several rather than
// truncating one.
func (c *conn) Read(p []byte) (int, error) {
	for len(c.rem) == 0 {
		select {
		case f := <-c.rx:
			c.rem = f
		case <-c.done:
			return 0, io.EOF
		}
	}
	n := copy(p, c.rem)
	c.rem = c.rem[n:]
	return n, nil
}

// Write puts one frame on the line, waiting for its turn if another master is
// mid-exchange.
//
// A stack writes one complete frame per call, which is what makes the write
// lock enough to keep two stations' frames from interleaving.
func (c *conn) Write(p []byte) (int, error) {
	if c.gone() {
		return 0, errDisconnected
	}

	// Only a master waits its turn: it is the one that starts an exchange.
	takesLine := c.st.addr.Master
	if takesLine {
		if err := c.bus.arb.acquire(c); err != nil {
			return 0, err
		}
	}

	c.bus.wmu.Lock()
	var (
		n   int
		err error
	)
	if c.gone() {
		// The line dropped while we waited for our turn. Writing now would put
		// a frame on a connection this session no longer owns.
		err = errDisconnected
	} else {
		n, err = c.w.Write(p)
	}
	c.bus.wmu.Unlock()

	if takesLine && (err != nil || !expectsReply(p)) {
		// Nothing is coming back — an application confirm, a no-reply control,
		// a broadcast, or a frame that never made it onto the line — so the
		// line is free at once rather than idling for the whole turnaround.
		c.bus.arb.release(c)
	}
	return n, err
}

// Close ends this session's connection. The bus keeps the line.
func (c *conn) Close() error {
	b := c.bus
	b.mu.Lock()
	if c.st.cur == c {
		c.st.cur = nil
	}
	b.mu.Unlock()

	c.teardown()
	return nil
}

// teardown makes the connection dead: reads end, writes fail, and any hold on
// the line is given up.
func (c *conn) teardown() {
	c.once.Do(func() {
		close(c.done)
		c.bus.arb.release(c)
	})
}

func (c *conn) gone() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

// push queues a frame for the session, reporting whether it fit.
//
// A full queue means the session has stopped reading, so the frame is dropped
// and counted. Blocking instead would stall every other station on the line
// behind one wedged session, which is the failure this whole package exists to
// avoid.
func (c *conn) push(frame []byte) bool {
	select {
	case c.rx <- frame:
		return true
	default:
		return false
	}
}

// expectsReply reports whether a transmitted frame leaves the line owed an
// answer, and so whether the sender should keep it.
//
// It peeks rather than decodes. The transport and application headers sit at a
// fixed offset after the ten-octet link header, ahead of the first block CRC,
// so they are readable without re-verifying CRCs the stack has just computed.
func expectsReply(frame []byte) bool {
	if len(frame) < link.HeaderSize {
		return false
	}
	if link.IsBroadcast(binary.LittleEndian.Uint16(frame[4:6])) {
		// Nothing answers a broadcast. Every outstation replying at once is
		// precisely the collision arbitration exists to prevent, and the
		// standard has them stay silent for the same reason.
		return false
	}
	if !link.ParseControl(frame[3]).Prm {
		// A secondary frame is itself a reply — an acknowledgement, a link
		// status — and nothing comes back for it. Reading it as a request
		// would leave a master holding the line for a whole turnaround every
		// time it acknowledged an outstation, which is most frames on a
		// confirmed link.
		return false
	}

	// A primary frame with no user data is a link-layer request — a reset, a
	// status request, a test — and every one of them is answered.
	if len(frame) < link.HeaderSize+3 {
		return true
	}
	if !transport.ParseHeader(frame[link.HeaderSize]).Fir {
		// A continuation segment: the decision was made on the first one, and
		// this station is holding the line either way.
		return true
	}

	fn := app.FuncCode(frame[link.HeaderSize+2])
	return fn != app.FuncConfirm && !fn.NoReply()
}
