package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/channel"
	"github.com/dscsystems/go-dnp3/master"
	"github.com/dscsystems/go-dnp3/objects"
	"github.com/dscsystems/go-dnp3/outstation"
)

// ---------- Configuration ----------

func TestValidateCatchesMisconfiguration(t *testing.T) {
	tests := []struct {
		name string
		cfg  *Config
		want string
	}{
		{
			name: "no sites",
			cfg:  &Config{Local: 1},
			want: "no sites",
		},
		{
			name: "unnamed site",
			cfg:  &Config{Local: 1, Sites: []Site{{Host: "h:1", Address: 10}}},
			want: "no name",
		},
		{
			name: "duplicate names",
			cfg: &Config{Local: 1, Sites: []Site{
				{Name: "a", Host: "h:1", Address: 10},
				{Name: "a", Host: "h:2", Address: 11},
			}},
			want: "both named",
		},
		{
			name: "no transport",
			cfg:  &Config{Local: 1, Sites: []Site{{Name: "a", Address: 10}}},
			want: "neither a host nor a serial",
		},
		{
			name: "both transports",
			cfg:  &Config{Local: 1, Sites: []Site{{Name: "a", Host: "h:1", Serial: "/dev/x", Address: 10}}},
			want: "pick one",
		},
		{
			name: "no outstation address",
			cfg:  &Config{Local: 1, Sites: []Site{{Name: "a", Host: "h:1"}}},
			want: "no outstation address",
		},
		{
			// A station cannot poll itself, and the symptom is a session that
			// answers its own requests rather than an obvious failure.
			name: "same address both ends",
			cfg:  &Config{Local: 10, Sites: []Site{{Name: "a", Host: "h:1", Address: 10}}},
			want: "both",
		},
		{
			name: "TLS without a CA",
			cfg: &Config{Local: 1, Sites: []Site{{
				Name: "a", Host: "h:1", Address: 10,
				TLS: &TLSFiles{Cert: "c", Key: "k"},
			}}},
			want: "CA",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestValidateAcceptsAGoodConfig(t *testing.T) {
	cfg := &Config{Local: 1, Sites: []Site{
		{Name: "north", Host: "10.0.0.1:20000", Address: 10},
		{Name: "south", Serial: "/dev/ttyUSB0", Address: 11},
	}}
	if err := cfg.Validate(); err != nil {
		t.Errorf("a valid configuration was rejected: %v", err)
	}
}

func TestDefaultsAreInherited(t *testing.T) {
	cfg := &Config{
		Local: 3,
		Defaults: SiteDefaults{
			Poll: 9 * time.Second, Integrity: time.Minute,
			Timeout: 2 * time.Second, KeepAlive: 20 * time.Second,
		},
		Sites: []Site{{Name: "a", Host: "h:1", Address: 10}},
	}

	got := cfg.resolved(cfg.Sites[0])
	if got.Local != 3 {
		t.Errorf("local = %d, want 3", got.Local)
	}
	if got.Poll != 9*time.Second || got.Integrity != time.Minute {
		t.Errorf("poll intervals not inherited: %+v", got)
	}
	if got.Timeout != 2*time.Second || got.KeepAlive != 20*time.Second {
		t.Errorf("timeouts not inherited: %+v", got)
	}
}

func TestSiteOverridesDefaults(t *testing.T) {
	cfg := &Config{
		Local:    1,
		Defaults: SiteDefaults{Poll: 9 * time.Second},
		Sites:    []Site{{Name: "a", Host: "h:1", Address: 10, Poll: time.Second, Local: 7}},
	}
	got := cfg.resolved(cfg.Sites[0])
	if got.Poll != time.Second {
		t.Errorf("poll = %v, want the site's own 1s", got.Poll)
	}
	if got.Local != 7 {
		t.Errorf("local = %d, want the site's own 7", got.Local)
	}
}

// TestSerialDefaultsToLinkConfirms pins a decision that matters: a serial line
// has no delivery guarantee of its own, so link-layer confirmation is what
// makes it reliable. Leaving it off by default there would give a silently
// lossy link.
func TestSerialDefaultsToLinkConfirms(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Sites = []Site{{Name: "a", Serial: "/dev/ttyUSB0", Address: 10}}

	got := cfg.resolved(cfg.Sites[0])
	if got.LinkConfirms == nil || !*got.LinkConfirms {
		t.Error("a serial site should enable link confirmation by default")
	}

	cfg.Sites = []Site{{Name: "b", Host: "h:1", Address: 10}}
	got = cfg.resolved(cfg.Sites[0])
	if got.LinkConfirms != nil && *got.LinkConfirms {
		t.Error("a TCP site should not enable link confirmation by default")
	}
}

func TestLoadConfigRejectsUnknownFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sites.yaml")
	// "pol" rather than "poll" — a typo that would otherwise silently leave
	// the default interval in place.
	body := "local: 1\nsites:\n  - name: a\n    host: h:1\n    address: 10\n    pol: 5s\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadConfig(path); err == nil {
		t.Error("a misspelled field should fail rather than be ignored")
	}
}

func TestLoadConfigReadsSites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sites.yaml")
	body := `local: 2
defaults:
  poll: 3s
sites:
  - name: north
    host: 10.0.0.1:20000
    address: 10
  - name: south
    host: 10.0.0.2:20000
    address: 11
    poll: 10s
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Sites) != 2 || cfg.Local != 2 {
		t.Fatalf("config = %+v", cfg)
	}
	if got := cfg.resolved(cfg.Sites[0]).Poll; got != 3*time.Second {
		t.Errorf("north poll = %v, want the default 3s", got)
	}
	if got := cfg.resolved(cfg.Sites[1]).Poll; got != 10*time.Second {
		t.Errorf("south poll = %v, want its own 10s", got)
	}
}

// ---------- Recording ----------

func TestRecorderWritesValuesAndEvents(t *testing.T) {
	dir := t.TempDir()
	rec, err := NewRecorder(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()

	static := master.Update{
		Type: dnp3.TypeAnalog, Index: 4,
		Analog: dnp3.Analog{Value: 42.5, Flags: dnp3.Online},
	}
	event := master.Update{
		Type: dnp3.TypeBinary, Index: 1,
		Binary: dnp3.Binary{Value: true, Flags: dnp3.Online},
		Info:   master.HeaderInfo{Kind: objects.KindEvent},
	}

	if err := rec.Record("site-a", static); err != nil {
		t.Fatal(err)
	}
	if err := rec.Record("site-a", event); err != nil {
		t.Fatal(err)
	}

	values := readCSV(t, filepath.Join(dir, "values.csv"))
	if len(values) != 3 { // header plus two rows
		t.Fatalf("values.csv has %d rows, want 3", len(values))
	}
	if values[1][4] != "42.5" {
		t.Errorf("value = %q, want 42.5", values[1][4])
	}
	if values[1][7] != "static" || values[2][7] != "event" {
		t.Errorf("source column = %q, %q", values[1][7], values[2][7])
	}

	// The sequence-of-events file holds only events: mixing static poll data
	// into it would stop it being a sequence of events.
	events := readCSV(t, filepath.Join(dir, "events.csv"))
	if len(events) != 2 {
		t.Fatalf("events.csv has %d rows, want 2 (header plus one event)", len(events))
	}
	if events[1][2] != "Binary" || events[1][3] != "1" {
		t.Errorf("event row = %v", events[1])
	}
}

// TestRecorderAppends covers a restart: a recorder that truncated would erase
// the history it exists to keep.
func TestRecorderAppends(t *testing.T) {
	dir := t.TempDir()

	for range 2 {
		rec, err := NewRecorder(dir)
		if err != nil {
			t.Fatal(err)
		}
		if err := rec.Record("a", master.Update{
			Type: dnp3.TypeCounter, Index: 0,
			Counter: dnp3.Counter{Value: 5, Flags: dnp3.Online},
		}); err != nil {
			t.Fatal(err)
		}
		rec.Close()
	}

	rows := readCSV(t, filepath.Join(dir, "values.csv"))
	if len(rows) != 3 {
		t.Fatalf("%d rows after two runs, want 3 (one header, two records)", len(rows))
	}
	if rows[0][0] != "received" {
		t.Error("the header was written twice or lost")
	}
}

// TestRecordedQualityDropsStateBit pins the same decision the explorer makes:
// the value column already says ON, so repeating it as a quality flag makes
// every binary row look like it carries something it does not.
func TestRecordedQualityDropsStateBit(t *testing.T) {
	dir := t.TempDir()
	rec, err := NewRecorder(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()

	if err := rec.Record("a", master.Update{
		Type:   dnp3.TypeBinary,
		Binary: dnp3.Binary{Value: true, Flags: dnp3.Online | dnp3.StateBit},
	}); err != nil {
		t.Fatal(err)
	}

	rows := readCSV(t, filepath.Join(dir, "values.csv"))
	if strings.Contains(rows[1][5], "STATE") {
		t.Errorf("quality = %q; the state bit should not be repeated", rows[1][5])
	}
	if !strings.Contains(rows[1][5], "ONLINE") {
		t.Errorf("quality = %q; the real flags should survive", rows[1][5])
	}
}

func readCSV(t *testing.T, path string) [][]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// ---------- Snapshot ----------

func TestSnapshotHoldsLatestValue(t *testing.T) {
	s := NewSnapshot()

	s.Apply("a", master.Update{
		Type: dnp3.TypeAnalog, Index: 0,
		Analog: dnp3.Analog{Value: 1, Flags: dnp3.Online},
	})
	s.Apply("a", master.Update{
		Type: dnp3.TypeAnalog, Index: 0,
		Analog: dnp3.Analog{Value: 2, Flags: dnp3.Online},
	})

	pts := s.Points("")
	if len(pts) != 1 {
		t.Fatalf("%d points, want 1 — the snapshot is accumulating rather than replacing", len(pts))
	}
	if pts[0].Value != "2" {
		t.Errorf("value = %q, want the latest", pts[0].Value)
	}
	if !pts[0].Good {
		t.Error("an online point should read as good")
	}
}

func TestSnapshotMarksBadQuality(t *testing.T) {
	s := NewSnapshot()
	s.Apply("a", master.Update{
		Type: dnp3.TypeAnalog, Index: 0,
		Analog: dnp3.Analog{Value: 1, Flags: dnp3.CommLost},
	})
	if s.Points("")[0].Good {
		t.Error("a comm-lost point should not read as good")
	}
}

func TestSnapshotFiltersBySite(t *testing.T) {
	s := NewSnapshot()
	s.Apply("a", master.Update{Type: dnp3.TypeAnalog, Index: 0})
	s.Apply("b", master.Update{Type: dnp3.TypeAnalog, Index: 0})

	if got := len(s.Points("a")); got != 1 {
		t.Errorf("site filter returned %d points, want 1", got)
	}
	if got := len(s.Points("")); got != 2 {
		t.Errorf("unfiltered returned %d points, want 2", got)
	}
}

// TestSnapshotOrderIsStable matters because the API output is diffed: an
// unstable order would show every point as changed on every request.
func TestSnapshotOrderIsStable(t *testing.T) {
	s := NewSnapshot()
	for i := range 20 {
		s.Apply("a", master.Update{Type: dnp3.TypeAnalog, Index: uint16(19 - i)})
		s.Apply("a", master.Update{Type: dnp3.TypeBinary, Index: uint16(i)})
	}

	first := s.Points("")
	for range 5 {
		next := s.Points("")
		for i := range first {
			if first[i].Type != next[i].Type || first[i].Index != next[i].Index {
				t.Fatal("the snapshot order changes between reads")
			}
		}
	}
	// And it is sorted by type then index, not by arrival.
	for i := 1; i < len(first); i++ {
		if first[i-1].Type == first[i].Type && first[i-1].Index > first[i].Index {
			t.Fatalf("points are not ordered by index: %v then %v", first[i-1], first[i])
		}
	}
}

// ---------- End to end ----------

// TestPollsSimulatedOutstation runs the real master machinery against an
// outstation over a pipe, through the recorder and the snapshot — the whole
// path an operator's data takes.
func TestPollsSimulatedOutstation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	mch, och := channel.Pipe()
	out := outstation.New(outstation.Config{
		LocalAddr: 10, RemoteAddr: 1,
		Database: outstation.DatabaseConfig{
			Binary: 4, Analog: 3, Counter: 2, DefaultClass: dnp3.Class1,
		},
	}, nil, nil)
	out.Database().Configure(dnp3.TypeAnalog, 0, outstation.PointConfig{
		Class: dnp3.Class1, StaticVariation: 5, EventVariation: 7,
	})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = out.Run(ctx, och) }()

	h := master.NewChannelHandler(1024)
	sess := master.New(master.Config{
		LocalAddr: 1, RemoteAddr: 10, ResponseTimeout: 5 * time.Second,
	}, h)

	wg.Add(1)
	go func() { defer wg.Done(); _ = sess.Run(ctx, mch) }()
	t.Cleanup(func() {
		cancel()
		_ = mch.Close()
		_ = och.Close()
		wg.Wait()
	})

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !sess.Connected() {
		time.Sleep(5 * time.Millisecond)
	}
	if !sess.Connected() {
		t.Fatal("never connected")
	}

	out.Update(func(db *outstation.Database) {
		db.UpdateAnalog(0, dnp3.Analog{Value: 123.75, Flags: dnp3.Online})
		db.UpdateBinary(2, dnp3.Binary{Value: true, Flags: dnp3.Online})
		db.UpdateCounter(1, dnp3.Counter{Value: 4321, Flags: dnp3.Online})
	})

	pollCtx, pollCancel := context.WithTimeout(ctx, 10*time.Second)
	defer pollCancel()
	if err := sess.IntegrityPoll(pollCtx); err != nil {
		t.Fatalf("integrity poll: %v", err)
	}

	dir := t.TempDir()
	rec, err := NewRecorder(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	snap := NewSnapshot()

	// Drain what the session produced, as the consume loop would.
	drained := 0
	for {
		select {
		case u := <-h.Updates():
			snap.Apply("sim", u)
			if err := rec.Record("sim", u); err != nil {
				t.Fatal(err)
			}
			drained++
			continue
		default:
		}
		break
	}
	if drained == 0 {
		t.Fatal("no measurements arrived")
	}

	pts := snap.Points("sim")
	byKey := map[string]PointValue{}
	for _, p := range pts {
		byKey[p.Type+":"+string(rune('0'+p.Index))] = p
	}

	if got := byKey["Analog:0"].Value; got != "123.75" {
		t.Errorf("analog 0 = %q, want 123.75 — a float variation should keep the fraction", got)
	}
	if got := byKey["Binary:2"].Value; got != "ON" {
		t.Errorf("binary 2 = %q, want ON", got)
	}
	if got := byKey["Counter:1"].Value; got != "4321" {
		t.Errorf("counter 1 = %q, want 4321", got)
	}

	// And the recording holds what the snapshot does.
	rows := readCSV(t, filepath.Join(dir, "values.csv"))
	if len(rows) < drained {
		t.Errorf("recorded %d rows for %d updates", len(rows)-1, drained)
	}
}

// TestAPIServesSnapshot checks the HTTP surface an operator or a dashboard
// would actually call.
func TestAPIServesSnapshot(t *testing.T) {
	snap := NewSnapshot()
	snap.Apply("north", master.Update{
		Type: dnp3.TypeAnalog, Index: 1,
		Analog: dnp3.Analog{Value: 11.5, Flags: dnp3.Online},
	})
	snap.SetStatus(SiteStatus{Name: "north", Connected: true, TasksRun: 3})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// Port zero would be better, but the server binds inside serveAPI; a high
	// fixed port keeps the test simple and is released on shutdown.
	const addr = "127.0.0.1:18099"
	go serveAPI(ctx, addr, snap, discardLogger())

	var resp *http.Response
	var err error
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resp, err = http.Get("http://" + addr + "/points")
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("the API never came up: %v", err)
	}
	defer resp.Body.Close()

	var points []PointValue
	if err := json.NewDecoder(resp.Body).Decode(&points); err != nil {
		t.Fatal(err)
	}
	if len(points) != 1 || points[0].Site != "north" || points[0].Value != "11.5" {
		t.Errorf("points = %+v", points)
	}

	sresp, err := http.Get("http://" + addr + "/status")
	if err != nil {
		t.Fatal(err)
	}
	defer sresp.Body.Close()

	var status []SiteStatus
	if err := json.NewDecoder(sresp.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	if len(status) != 1 || !status[0].Connected || status[0].TasksRun != 3 {
		t.Errorf("status = %+v", status)
	}
}
