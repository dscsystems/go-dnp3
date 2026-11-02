package dnp3_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/channel"
	"github.com/dscsystems/go-dnp3/master"
	"github.com/dscsystems/go-dnp3/outstation"
)

// restartDelayApp announces a fixed restart delay.
type restartDelayApp struct {
	outstation.NopApplication
	delay time.Duration
}

func (a restartDelayApp) ColdRestart() time.Duration { return a.delay }
func (a restartDelayApp) WarmRestart() time.Duration { return a.delay }

// intervalCollector records the time-and-interval values a master receives.
type intervalCollector struct {
	master.NopHandler
	mu     sync.Mutex
	values []dnp3.Indexed[dnp3.TimeAndInterval]
}

func (c *intervalCollector) HandleTimeAndInterval(_ master.HeaderInfo, v []dnp3.Indexed[dnp3.TimeAndInterval]) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.values = append(c.values, v...)
}

func runPair(t *testing.T, out *outstation.Session, m *master.Session) {
	t.Helper()
	mch, och := channel.Pipe()
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = out.Run(ctx, och) }()
	go func() { defer wg.Done(); _ = m.Run(ctx, mch) }()
	t.Cleanup(func() {
		cancel()
		_ = mch.Close()
		_ = och.Close()
		wg.Wait()
	})
	waitFor(t, 3*time.Second, func() bool { return m.Connected() })
}

// TestRestartWithDelayReturnsAnnouncedDelay checks the master returns the
// group 52 delay a go-dnp3 outstation answers a restart with.
func TestRestartWithDelayReturnsAnnouncedDelay(t *testing.T) {
	out := outstation.New(outstation.Config{LocalAddr: 10, RemoteAddr: 1}, restartDelayApp{delay: 1500 * time.Millisecond}, nil)
	m := master.New(master.Config{LocalAddr: 1, RemoteAddr: 10, ResponseTimeout: 2 * time.Second}, master.NopHandler{})
	runPair(t, out, m)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	d, err := m.RestartWithDelay(ctx, dnp3.ColdRestart)
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	if d != 1500*time.Millisecond {
		t.Errorf("delay = %v, want 1.5s", d)
	}
}

// TestMasterReadsTimeAndInterval checks a go-dnp3 master can read the g50v4
// values a go-dnp3 outstation serves.
func TestMasterReadsTimeAndInterval(t *testing.T) {
	out := outstation.New(outstation.Config{
		LocalAddr: 10, RemoteAddr: 1,
		Database: outstation.DatabaseConfig{TimeAndInterval: 1},
	}, nil, nil)
	coll := &intervalCollector{}
	m := master.New(master.Config{LocalAddr: 1, RemoteAddr: 10, ResponseTimeout: 2 * time.Second}, coll)
	runPair(t, out, m)

	at := time.UnixMilli(1_700_000_000_000).UTC()
	out.Update(func(db *outstation.Database) {
		db.UpdateTimeAndInterval(0, dnp3.TimeAndInterval{Time: dnp3.Now(at), Interval: 60, Units: 1})
	})

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := m.ScanRange(ctx, 50, 4, 0, 0); err != nil {
		t.Fatalf("read g50v4: %v", err)
	}
	coll.mu.Lock()
	defer coll.mu.Unlock()
	if len(coll.values) != 1 {
		t.Fatalf("got %d g50v4 values, want 1", len(coll.values))
	}
	if v := coll.values[0].Value; !v.Time.Time.Equal(at) || v.Interval != 60 || v.Units != 1 {
		t.Errorf("got %+v", v)
	}
}
