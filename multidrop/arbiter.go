package multidrop

import (
	"sync"
	"time"

	"github.com/dscsystems/go-dnp3/channel"
)

// arbiter gives the line to one master at a time.
//
// A multi-drop line is half duplex: if two masters transmit requests at once,
// the two outstations answer at once and both replies are lost. Nothing in a
// session knows that — each one believes it owns its channel — so the turn
// taking has to happen here, where the transmissions meet.
//
// A master takes the line when it transmits and gives it back when the reply
// arrives complete, or when the turnaround elapses with nothing having come
// back. That second case is what keeps one unresponsive outstation from
// stopping the line for good: the hold is a reservation with an expiry, not a
// lock.
//
// Outstations do not take the line at all. They transmit only when addressed,
// so they are already taking turns; making them wait would deadlock the case
// where a master holds the line and the reply it is waiting for is queued
// behind that hold.
type arbiter struct {
	// period is the turnaround. Zero disables arbitration entirely.
	period time.Duration

	mu     sync.Mutex
	cond   *sync.Cond
	holder *conn
	until  time.Time
	// timer wakes the waiters when the current hold expires, which is what
	// sync.Cond cannot do on its own.
	timer  *time.Timer
	closed bool
}

func (a *arbiter) init(period time.Duration) {
	a.period = period
	a.cond = sync.NewCond(&a.mu)
}

// acquire blocks until the caller may transmit.
func (a *arbiter) acquire(c *conn) error {
	if a.period <= 0 {
		return nil
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	for {
		switch {
		case a.closed:
			return channel.ErrClosed

		// An expired hold is taken over rather than waited on: the station that
		// had it asked a question nobody answered.
		case a.holder == nil, a.holder == c, !time.Now().Before(a.until):
			a.holdLocked(c)
			return nil
		}
		a.cond.Wait()
	}
}

// observe reports a frame routed to c, extending or ending its hold.
func (a *arbiter) observe(c *conn, complete bool) {
	if a.period <= 0 {
		return
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if a.holder != c {
		return
	}
	if complete {
		// The fragment is whole; the exchange is over and the line is free.
		a.releaseLocked()
		return
	}
	// Traffic is flowing but the reply is not finished — more segments, or a
	// link-layer acknowledgement with the response still to come. Hold on for
	// another turnaround rather than letting it expire mid-answer.
	a.extendLocked()
}

// release gives up c's hold, if it has one.
func (a *arbiter) release(c *conn) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.holder == c {
		a.releaseLocked()
	}
}

// close frees the line and fails every waiter.
func (a *arbiter) close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closed = true
	a.releaseLocked()
}

func (a *arbiter) holdLocked(c *conn) {
	a.holder = c
	a.extendLocked()
}

func (a *arbiter) extendLocked() {
	a.until = time.Now().Add(a.period)
	if a.timer == nil {
		a.timer = time.AfterFunc(a.period, a.wake)
		return
	}
	a.timer.Reset(a.period)
}

func (a *arbiter) releaseLocked() {
	a.holder = nil
	if a.timer != nil {
		a.timer.Stop()
	}
	a.cond.Broadcast()
}

// wake lets the waiters re-check an expired hold.
func (a *arbiter) wake() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cond.Broadcast()
}
