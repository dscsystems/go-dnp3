package channel

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
)

// busyRejecter turns away connections that arrive while a server channel is
// serving one, for [BusyReject].
//
// While the served connection is open, a goroutine accepts on the listener and
// closes whatever it gets. It stops when the served connection is closed, or
// when the channel next accepts or closes — whichever comes first — so it
// never competes with the channel's own accept.
type busyRejecter struct {
	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}

	// rejected counts connections turned away.
	rejected atomic.Uint64
}

// start begins rejecting on l while served is open, and returns served wrapped
// so that closing it stops the rejecting.
func (b *busyRejecter) start(l *net.TCPListener, served net.Conn, sec *secHolder) net.Conn {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	b.mu.Lock()
	b.cancel, b.done = cancel, done
	b.mu.Unlock()

	go func() {
		defer close(done)
		for {
			conn, err := acceptContext(ctx, l, sec.acceptFailed)
			if err != nil {
				return
			}
			sec.rejectedBusy(conn)
			b.rejected.Add(1)
			_ = conn.Close()
		}
	}()
	return &servedConn{Conn: served, release: func() { b.stopIf(done) }}
}

// stopIf stops the rejecter only if it is still the one identified by done,
// so a stale connection closed late cannot stop a newer rejecter.
func (b *busyRejecter) stopIf(done chan struct{}) {
	b.mu.Lock()
	current := b.done == done
	b.mu.Unlock()
	if current {
		b.stop()
	}
}

// stop ends the rejecting and waits for it to finish. It is safe to call when
// nothing is being rejected.
func (b *busyRejecter) stop() {
	b.mu.Lock()
	cancel, done := b.cancel, b.done
	b.cancel, b.done = nil, nil
	b.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
}

// servedConn is a served connection whose Close also stops the rejecter, so
// the slot is open to the next connection the moment it ends.
type servedConn struct {
	net.Conn
	release func()
	once    sync.Once
}

func (c *servedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}

// Unwrap returns the underlying connection.
func (c *servedConn) Unwrap() net.Conn { return c.Conn }
