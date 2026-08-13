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

// Octet strings are how a device reports the things that are text rather than
// measurements: point names, firmware versions, serial numbers.
//
// Their encoding is the odd one in DNP3 — the variation number *is* the
// string's length — which means two points of different lengths cannot share
// an object header, and a point whose string changes length changes the
// variation it is reported in. Both of those are exercised here.

// stringCollector records the octet strings a master receives.
type stringCollector struct {
	master.NopHandler

	mu      sync.Mutex
	strings map[uint16]string
	events  int
}

func newStringCollector() *stringCollector {
	return &stringCollector{strings: map[uint16]string{}}
}

func (c *stringCollector) HandleOctetString(info master.HeaderInfo, vs []dnp3.Indexed[dnp3.OctetString]) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, v := range vs {
		c.strings[v.Index] = string(v.Value)
		if info.IsEvent() {
			c.events++
		}
	}
}

func (c *stringCollector) get(i uint16) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.strings[i]
}

func (c *stringCollector) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.strings)
}

func (c *stringCollector) eventCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.events
}

// stringPair builds a master and outstation with octet string points.
func stringPair(t *testing.T, count int) (*master.Session, *outstation.Session, *stringCollector) {
	t.Helper()

	mch, och := channel.Pipe()
	coll := newStringCollector()

	out := outstation.New(outstation.Config{
		LocalAddr: 10, RemoteAddr: 1,
		Database: outstation.DatabaseConfig{
			Binary: 2, OctetString: count, DefaultClass: dnp3.Class1,
		},
		ConfirmTimeout: time.Second,
	}, nil, nil)

	m := master.New(master.Config{
		LocalAddr: 1, RemoteAddr: 10, ResponseTimeout: 3 * time.Second,
	}, coll)

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
	return m, out, coll
}

func TestOctetStringsCrossTheWire(t *testing.T) {
	m, out, coll := stringPair(t, 3)

	out.Update(func(db *outstation.Database) {
		db.UpdateOctetString(0, []byte("Feeder 1"))
		db.UpdateOctetString(1, []byte("Feeder 2"))
		db.UpdateOctetString(2, []byte("Bus tie"))
	})

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := m.IntegrityPoll(ctx); err != nil {
		t.Fatalf("integrity poll: %v", err)
	}

	if got := coll.count(); got != 3 {
		t.Fatalf("received %d strings, want 3", got)
	}
	for i, want := range map[uint16]string{0: "Feeder 1", 1: "Feeder 2", 2: "Bus tie"} {
		if got := coll.get(i); got != want {
			t.Errorf("string %d = %q, want %q", i, got, want)
		}
	}
}

// TestOctetStringsOfDifferentLengths is the case the encoding makes awkward.
//
// The variation is the length, so strings of different lengths cannot share an
// object header. An implementation that reported them all at one length would
// truncate the long ones and pad the short ones — and the padding would look
// like part of the name.
func TestOctetStringsOfDifferentLengths(t *testing.T) {
	m, out, coll := stringPair(t, 4)

	want := map[uint16]string{
		0: "A",
		1: "Feeder 1 protection relay",
		2: "BB",
		3: "Transformer 2 tap changer controller",
	}
	out.Update(func(db *outstation.Database) {
		for i, v := range want {
			db.UpdateOctetString(i, []byte(v))
		}
	})

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := m.IntegrityPoll(ctx); err != nil {
		t.Fatalf("integrity poll: %v", err)
	}

	for i, w := range want {
		if got := coll.get(i); got != w {
			t.Errorf("string %d = %q, want %q — lengths are being conflated", i, got, w)
		}
	}
}

// TestOctetStringLengthChange covers a point whose string changes length,
// which changes the variation it is reported in. That is legal and a master
// must cope.
func TestOctetStringLengthChange(t *testing.T) {
	m, out, coll := stringPair(t, 2)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	out.Update(func(db *outstation.Database) {
		db.UpdateOctetString(0, []byte("short"))
	})
	waitFor(t, 2*time.Second, func() bool {
		v, _, _ := out.Database().OctetString(0)
		return string(v) == "short"
	})
	if err := m.IntegrityPoll(ctx); err != nil {
		t.Fatal(err)
	}
	if got := coll.get(0); got != "short" {
		t.Fatalf("string = %q, want short", got)
	}

	out.Update(func(db *outstation.Database) {
		db.UpdateOctetString(0, []byte("a considerably longer label"))
	})
	// Update is queued to the session goroutine, so the poll below has to wait
	// for it to land or it reads the previous value and proves nothing.
	waitFor(t, 2*time.Second, func() bool {
		v, _, _ := out.Database().OctetString(0)
		return string(v) == "a considerably longer label"
	})

	if err := m.IntegrityPoll(ctx); err != nil {
		t.Fatal(err)
	}
	if got := coll.get(0); got != "a considerably longer label" {
		t.Errorf("string = %q; the variation should have grown with the length", got)
	}
}

// TestOctetStringEvents covers group 111: a changed string is an event like
// any other measurement.
func TestOctetStringEvents(t *testing.T) {
	m, out, coll := stringPair(t, 2)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	out.Update(func(db *outstation.Database) {
		db.UpdateOctetString(0, []byte("initial"))
	})
	if err := m.IntegrityPoll(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, func() bool { return out.Events().Total() == 0 })

	before := coll.eventCount()
	out.Update(func(db *outstation.Database) {
		db.UpdateOctetString(0, []byte("changed"))
	})
	waitFor(t, 2*time.Second, func() bool { return out.Events().Total() >= 1 })

	if err := m.ScanClasses(ctx, dnp3.Class1); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, func() bool { return coll.eventCount() > before })

	if got := coll.get(0); got != "changed" {
		t.Errorf("the event carried %q, want changed", got)
	}
}

// TestOctetStringUnchangedProducesNoEvent confirms the change detection
// compares contents rather than firing on every update.
func TestOctetStringUnchangedProducesNoEvent(t *testing.T) {
	_, out, _ := stringPair(t, 2)

	out.Update(func(db *outstation.Database) {
		db.UpdateOctetString(0, []byte("steady"))
	})
	waitFor(t, 2*time.Second, func() bool { return out.Events().Total() == 1 })

	out.Update(func(db *outstation.Database) {
		db.UpdateOctetString(0, []byte("steady"))
		db.UpdateOctetString(0, []byte("steady"))
	})
	time.Sleep(150 * time.Millisecond)

	if got := out.Events().Total(); got != 1 {
		t.Errorf("%d events after writing the same string three times, want 1", got)
	}
}

// TestOctetStringTooLongIsTruncated covers the hard limit: the length must fit
// a variation number, so 255 octets is the most a string can be.
func TestOctetStringTooLongIsTruncated(t *testing.T) {
	m, out, coll := stringPair(t, 1)

	long := make([]byte, 400)
	for i := range long {
		long[i] = 'x'
	}
	out.Update(func(db *outstation.Database) {
		db.UpdateOctetString(0, long)
	})

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := m.IntegrityPoll(ctx); err != nil {
		t.Fatalf("integrity poll: %v", err)
	}

	got := coll.get(0)
	if len(got) != dnp3.MaxOctetStringLen {
		t.Errorf("received %d octets, want the %d maximum", len(got), dnp3.MaxOctetStringLen)
	}
}
