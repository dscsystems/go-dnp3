package dnp3_test

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/dscsystems/go-dnp3/channel"
	"github.com/dscsystems/go-dnp3/master"
)

// A keep-alive probe nobody answers used to be retried forever on a
// connection the master still called up: nothing was waiting on the link, so
// nothing noticed. The master has to drop the connection so Run reconnects.
func TestUnansweredKeepAliveDropsTheConnection(t *testing.T) {
	mch, och := channel.Pipe()
	m := master.New(master.Config{
		LocalAddr: 1, RemoteAddr: 10,
		KeepAlive:       40 * time.Millisecond,
		LinkTimeout:     40 * time.Millisecond,
		LinkRetries:     1,
		ResponseTimeout: time.Second,
	}, master.NopHandler{})

	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = m.Run(ctx, mch) }()
	t.Cleanup(func() {
		cancel()
		_ = mch.Close()
		_ = och.Close()
		wg.Wait()
	})

	conn, err := och.Connect(ctx)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	// A peer that reads everything and answers nothing.
	closed := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, conn)
		close(closed)
	}()

	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("the master kept a connection whose peer never answered a keep-alive")
	}
}
