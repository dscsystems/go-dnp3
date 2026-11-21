package secflags

import (
	"flag"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dscsystems/go-dnp3/secevent"
)

func parse(t *testing.T, outstation bool, args ...string) (*secevent.Policy, func() error, error) {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	f := Register(fs, outstation)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse: %v", err)
	}
	return f.Policy()
}

func TestNoFlagsNoPolicy(t *testing.T) {
	p, closer, err := parse(t, true)
	if err != nil || p != nil {
		t.Fatalf("policy = %v, err = %v", p, err)
	}
	if err := closer(); err != nil {
		t.Fatal(err)
	}
}

func TestOutstationPolicy(t *testing.T) {
	p, closer, err := parse(t, true,
		"-allow-peer", "10.0.0.0/24,192.168.1.7", "-reject-unlisted",
		"-allowed-master", "1", "-allowed-master", "2", "-reject-unexpected-master",
		"-read-only", "-flood-action", "throttle")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closer() }()
	if err := p.Valid(); err != nil {
		t.Fatalf("invalid policy: %v", err)
	}
	want := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/24"), netip.MustParsePrefix("192.168.1.7/32")}
	if len(p.AllowedPeers) != 2 || p.AllowedPeers[0] != want[0] || p.AllowedPeers[1] != want[1] {
		t.Fatalf("AllowedPeers = %v", p.AllowedPeers)
	}
	if !p.RejectUnlisted || !p.RejectUnexpectedMaster || len(p.AllowedMasters) != 2 {
		t.Fatalf("policy = %+v", p)
	}
	if p.Functions == nil || !p.Functions.ReadOnly || !p.Functions.AllowTimeWrite {
		t.Fatalf("Functions = %+v", p.Functions)
	}
	if p.FloodAction != secevent.Throttled || p.Dispatcher != nil {
		t.Fatalf("FloodAction = %v, Dispatcher = %v", p.FloodAction, p.Dispatcher)
	}
}

func TestEventsToFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	p, closer, err := parse(t, false, "-events", path, "-events-min", "warning")
	if err != nil {
		t.Fatal(err)
	}
	p.Dispatcher.Submit(secevent.Event{Kind: secevent.KindConnClosed, Severity: secevent.Info})
	p.Dispatcher.Submit(secevent.Event{Kind: secevent.KindConnUnexpected, Severity: secevent.Warning})
	if err := closer(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if s := string(b); strings.Count(s, "\n") != 1 || !strings.Contains(s, `"conn.unexpected"`) {
		t.Fatalf("file = %q", s)
	}
}

func TestBadFlags(t *testing.T) {
	for _, args := range [][]string{
		{"-reject-unlisted"},
		{"-reject-unexpected-master"},
		{"-flood-action", "explode"},
		{"-events", "json", "-events-min", "loud"},
	} {
		if _, _, err := parse(t, true, args...); err == nil {
			t.Errorf("%v: no error", args)
		}
	}
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	Register(fs, true)
	fs.SetOutput(new(strings.Builder))
	if err := fs.Parse([]string{"-allow-peer", "nowhere"}); err == nil {
		t.Error("bad address accepted")
	}
	if err := fs.Parse([]string{"-allowed-master", "70000"}); err == nil {
		t.Error("bad link address accepted")
	}
}
