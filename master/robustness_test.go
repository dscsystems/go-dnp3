package master

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

type oneConnChannel struct {
	conn   net.Conn
	once   sync.Once
	closed chan struct{}
}

func (c *oneConnChannel) Connect(ctx context.Context) (io.ReadWriteCloser, error) {
	var conn io.ReadWriteCloser
	c.once.Do(func() { conn = &notifyConn{Conn: c.conn, closed: c.closed} })
	if conn != nil {
		return conn, nil
	}
	<-ctx.Done()
	return nil, ctx.Err()
}
func (c *oneConnChannel) Close() error   { return nil }
func (c *oneConnChannel) String() string { return "one-conn" }

type notifyConn struct {
	net.Conn
	once   sync.Once
	closed chan struct{}
}

func (c *notifyConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

// An outstation that never reads makes the master's first request miss its
// write deadline, and the master drops the connection instead of hanging.
func TestMasterWriteTimeoutDropsAPeerThatDoesNotRead(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	ch := &oneConnChannel{conn: a, closed: make(chan struct{})}
	m := New(Config{
		LocalAddr: 1, RemoteAddr: 10,
		IntegrityOnStartup: true,
		WriteTimeout:       100 * time.Millisecond,
	}, nil)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _ = m.Run(ctx, ch) }()
	defer func() { cancel(); <-done }()

	select {
	case <-ch.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("the master did not drop a connection it could not write to")
	}
}
