package main

import (
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/master"
)

// Recording is deliberately CSV rather than a database.
//
// A recorder that needs a schema migration before it can start is a recorder
// that does not get used during an outage, and CSV is the format every
// engineer already has a tool for. The plan called for SQLite too; that would
// mean a large dependency in what is meant to be an example, so it is left
// out — anyone who wants a database can read these files into one.

// Recorder writes measurements and events to CSV.
type Recorder struct {
	mu sync.Mutex

	values *csvFile
	events *csvFile
}

type csvFile struct {
	f *os.File
	w *csv.Writer
}

func newCSVFile(path string, header []string) (*csvFile, error) {
	// Append rather than truncate: a restarted recorder must not erase the
	// history it was started to keep.
	existing, statErr := os.Stat(path)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}

	w := csv.NewWriter(f)
	if statErr != nil || existing.Size() == 0 {
		if err := w.Write(header); err != nil {
			_ = f.Close()
			return nil, err
		}
		w.Flush()
	}
	return &csvFile{f: f, w: w}, nil
}

func (c *csvFile) write(row []string) error {
	if err := c.w.Write(row); err != nil {
		return err
	}
	// Flushed per row: a recorder that loses the last minute of an outage to a
	// buffer is worse than one that writes a little more slowly.
	c.w.Flush()
	return c.w.Error()
}

func (c *csvFile) Close() error {
	c.w.Flush()
	return c.f.Close()
}

// NewRecorder opens the recording files under dir.
func NewRecorder(dir string) (*Recorder, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}

	values, err := newCSVFile(filepath.Join(dir, "values.csv"),
		[]string{"received", "site", "type", "index", "value", "quality", "timestamp", "source"})
	if err != nil {
		return nil, err
	}

	events, err := newCSVFile(filepath.Join(dir, "events.csv"),
		[]string{"received", "site", "type", "index", "value", "quality", "timestamp"})
	if err != nil {
		_ = values.Close()
		return nil, err
	}

	return &Recorder{values: values, events: events}, nil
}

func (r *Recorder) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	_ = r.events.Close()
	return r.values.Close()
}

// Record writes one measurement.
//
// Events go to both files: values.csv is the current picture, events.csv is
// the sequence of record. Keeping them separate matters because a
// sequence-of-events file with static poll data mixed in is no longer a
// sequence of events.
func (r *Recorder) Record(site string, u master.Update) error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	value, flags, stamp := describeUpdate(u)
	now := time.Now().Format(time.RFC3339Nano)

	source := "static"
	if u.Info.IsEvent() {
		source = "event"
	}

	row := []string{
		now, site, u.Type.String(), strconv.Itoa(int(u.Index)),
		value, qualityText(flags, u.Type), stampText(stamp), source,
	}
	if err := r.values.write(row); err != nil {
		return err
	}

	if u.Info.IsEvent() {
		return r.events.write(row[:7])
	}
	return nil
}

// qualityText renders the quality flags, dropping the state bit for binary
// points because the value column already says ON or OFF. Repeating it makes
// every binary row look like it carries a flag it does not.
func qualityText(f dnp3.Flags, t dnp3.PointType) string {
	if t == dnp3.TypeBinary || t == dnp3.TypeBinaryOutputStatus {
		f &^= dnp3.StateBit
	}
	return f.StringFor(t)
}

func stampText(t dnp3.Timestamp) string {
	if !t.IsValid() {
		return ""
	}
	return t.Time.Format(time.RFC3339Nano)
}

// describeUpdate pulls the value, quality and timestamp out of an update.
func describeUpdate(u master.Update) (string, dnp3.Flags, dnp3.Timestamp) {
	switch u.Type {
	case dnp3.TypeBinary:
		return boolText(u.Binary.Value), u.Binary.Flags, u.Binary.Time
	case dnp3.TypeDoubleBitBinary:
		return u.DoubleBit.Value.String(), u.DoubleBit.Flags, u.DoubleBit.Time
	case dnp3.TypeCounter:
		return strconv.FormatUint(uint64(u.Counter.Value), 10), u.Counter.Flags, u.Counter.Time
	case dnp3.TypeFrozenCounter:
		return strconv.FormatUint(uint64(u.FrozenCounter.Value), 10), u.FrozenCounter.Flags, u.FrozenCounter.Time
	case dnp3.TypeAnalog:
		return formatFloat(u.Analog.Value), u.Analog.Flags, u.Analog.Time
	case dnp3.TypeBinaryOutputStatus:
		return boolText(u.BinaryOutput.Value), u.BinaryOutput.Flags, u.BinaryOutput.Time
	case dnp3.TypeAnalogOutputStatus:
		return formatFloat(u.AnalogOutput.Value), u.AnalogOutput.Flags, u.AnalogOutput.Time
	}
	return "", 0, dnp3.Timestamp{}
}

func boolText(v bool) string {
	if v {
		return "ON"
	}
	return "OFF"
}

func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// ---------- Live snapshot ----------

// pointKey identifies a point within a site.
type pointKey struct {
	Type  dnp3.PointType
	Index uint16
}

// PointValue is one point as the snapshot holds it.
type PointValue struct {
	Site     string `json:"site"`
	Type     string `json:"type"`
	Index    uint16 `json:"index"`
	Value    string `json:"value"`
	Quality  string `json:"quality"`
	Good     bool   `json:"good"`
	Stamp    string `json:"timestamp,omitempty"`
	Received string `json:"received"`
	Source   string `json:"source"`
}

// Snapshot holds the latest value of every point, for the HTTP API.
//
// It is separate from the recorder because they answer different questions:
// the recording is what happened, the snapshot is what is true now.
type Snapshot struct {
	mu     sync.RWMutex
	values map[string]map[pointKey]PointValue
	status map[string]SiteStatus
}

// SiteStatus is what the API reports about one session.
type SiteStatus struct {
	Name        string `json:"name"`
	Target      string `json:"target"`
	Connected   bool   `json:"connected"`
	Indications string `json:"indications"`

	TasksRun       uint64 `json:"tasks_run"`
	TasksSucceeded uint64 `json:"tasks_succeeded"`
	TasksFailed    uint64 `json:"tasks_failed"`
	Timeouts       uint64 `json:"response_timeouts"`
	Unsolicited    uint64 `json:"unsolicited"`
	Restarts       uint64 `json:"restarts_seen"`
	Connections    uint64 `json:"connections"`
}

func NewSnapshot() *Snapshot {
	return &Snapshot{
		values: map[string]map[pointKey]PointValue{},
		status: map[string]SiteStatus{},
	}
}

func (s *Snapshot) Apply(site string, u master.Update) {
	value, flags, stamp := describeUpdate(u)

	s.mu.Lock()
	defer s.mu.Unlock()

	m, ok := s.values[site]
	if !ok {
		m = map[pointKey]PointValue{}
		s.values[site] = m
	}

	source := "static"
	if u.Info.IsEvent() {
		source = "event"
	}
	m[pointKey{u.Type, u.Index}] = PointValue{
		Site:     site,
		Type:     u.Type.String(),
		Index:    u.Index,
		Value:    value,
		Quality:  qualityText(flags, u.Type),
		Good:     flags.IsGood(),
		Stamp:    stampText(stamp),
		Received: time.Now().Format(time.RFC3339Nano),
		Source:   source,
	}
}

func (s *Snapshot) SetStatus(st SiteStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status[st.Name] = st
}

// Points returns every known value, sorted so the output is stable between
// requests — a diff of two API responses should show what changed, not the
// map iteration order.
func (s *Snapshot) Points(site string) []PointValue {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var out []PointValue
	for name, points := range s.values {
		if site != "" && name != site {
			continue
		}
		for _, v := range points {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Site != out[j].Site {
			return out[i].Site < out[j].Site
		}
		if out[i].Type != out[j].Type {
			return out[i].Type < out[j].Type
		}
		return out[i].Index < out[j].Index
	})
	return out
}

func (s *Snapshot) Status() []SiteStatus {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]SiteStatus, 0, len(s.status))
	for _, st := range s.status {
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Summary renders a one-line-per-site overview for the console.
func (s *Snapshot) Summary() string {
	var b []byte
	for _, st := range s.Status() {
		state := "down"
		if st.Connected {
			state = "up"
		}
		b = append(b, fmt.Sprintf("  %-16s %-4s %-24s tasks %d/%d  timeouts %d\n",
			st.Name, state, st.Indications, st.TasksSucceeded, st.TasksRun, st.Timeouts)...)
	}
	return string(b)
}
