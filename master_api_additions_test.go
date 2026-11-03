package dnp3_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/master"
	"github.com/dscsystems/go-dnp3/outstation"
)

// ScanAll reads every point of a group without the master knowing how many
// there are.
func TestScanAll(t *testing.T) {
	m, out, coll := pair(t, outstation.DatabaseConfig{Analog: 7}, master.Config{})
	out.Update(func(db *outstation.Database) {
		for i := range 7 {
			db.UpdateAnalog(uint16(i), dnp3.Analog{Value: float64(i * 10), Flags: dnp3.Online})
		}
	})

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := m.ScanAll(ctx, 30, 1); err != nil {
		t.Fatalf("scan all: %v", err)
	}
	_, analog, _ := coll.snapshot()
	if len(analog) != 7 {
		t.Fatalf("received %d points, want 7", len(analog))
	}
	if got := analog[6].Value; got != 60 {
		t.Errorf("analog 6 = %v, want 60", got)
	}
}

// The no-reply freezes are carried out though nothing is answered.
func TestFreezeCountersNoReply(t *testing.T) {
	m, out, _ := pair(t, outstation.DatabaseConfig{Counter: 1, FrozenCounter: 1}, master.Config{})
	out.Update(func(db *outstation.Database) {
		db.UpdateCounter(0, dnp3.Counter{Value: 7, Flags: dnp3.Online})
	})
	waitFor(t, 2*time.Second, func() bool {
		c, _, _ := out.Database().Counter(0)
		return c.Value == 7
	})

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := m.FreezeCountersNoReply(ctx, true); err != nil {
		t.Fatalf("freeze-clear no reply: %v", err)
	}
	waitFor(t, 2*time.Second, func() bool {
		f, _, _ := out.Database().FrozenCounter(0)
		c, _, _ := out.Database().Counter(0)
		return f.Value == 7 && c.Value == 0
	})
}

// AppendFile adds to the end of a file rather than replacing it.
func TestFileAppend(t *testing.T) {
	m, _ := serving(t, map[string]string{"log.txt": "abc"})

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := m.AppendFile(ctx, "/log.txt", bytes.NewReader([]byte("def")), 3); err != nil {
		t.Fatalf("append: %v", err)
	}
	got, err := m.ReadFileBytes(ctx, "/log.txt")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != "abcdef" {
		t.Errorf("file = %q, want %q", got, "abcdef")
	}
}
