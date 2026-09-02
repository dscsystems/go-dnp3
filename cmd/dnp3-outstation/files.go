package main

import (
	"bytes"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/outstation"
)

// A simulated RTU has files on it: the configuration it was commissioned with,
// an event log, a firmware version. A master being developed against this tool
// needs somewhere to point its file transfer, and asking whoever runs it to
// prepare a directory first would mean the feature goes untested — the same
// reason the plant simulation ships with a default substation.
//
// So the device serves an in-memory filesystem unless told to serve a real
// directory. Nothing here touches the host: reading is synthesised, and what a
// master writes stays in this process and disappears with it.

// memFileLimit caps what one written file may hold, so a master with a bug —
// or a test of what happens when a transfer runs away — cannot exhaust a
// simulator that is meant to be left running.
const memFileLimit = 4 << 20

// memFiles is a filesystem in memory.
//
// It is deliberately flat apart from one directory, because the point is to
// exercise a master's file transfer rather than to be a filesystem: a listing
// with a directory in it, files of different sizes, and one that is not there.
type memFiles struct {
	mu    sync.Mutex
	files map[string]*memFile

	// readOnly refuses writes and deletes, which is how a device that
	// publishes its configuration without accepting a new one behaves.
	readOnly bool
}

type memFile struct {
	name    string
	data    []byte
	dir     bool
	created time.Time
	perms   dnp3.FilePermissions
}

// newMemFiles builds the device's filesystem from the simulated plant, so the
// configuration a master reads back describes the device it is talking to.
func newMemFiles(cfg *Config, sim *Simulator, readOnly bool) *memFiles {
	now := time.Now()
	m := &memFiles{files: map[string]*memFile{}, readOnly: readOnly}

	add := func(name string, perms dnp3.FilePermissions, content string) {
		m.files[name] = &memFile{
			name: name, data: []byte(content), created: now, perms: perms,
		}
	}

	const rw = dnp3.PermOwnerRead | dnp3.PermOwnerWrite | dnp3.PermGroupRead
	const ro = dnp3.PermOwnerRead | dnp3.PermGroupRead | dnp3.PermWorldRead

	m.files["/logs"] = &memFile{
		name: "/logs", dir: true, created: now,
		perms: dnp3.PermOwnerRead | dnp3.PermOwnerExecute,
	}

	add("/device.txt", ro, deviceFile(cfg))
	add("/points.txt", ro, sim.Describe())
	add("/config.yaml", rw, configFile(cfg))
	add("/logs/events.log", ro, eventLogFile(now))

	return m
}

// deviceFile is the identification a real RTU exposes: what it is, what it
// runs, and the addresses it answers on.
func deviceFile(cfg *Config) string {
	var b strings.Builder
	// The same identity the device answers a group 0 read with. A device whose
	// file and whose attributes disagree about its own serial number is a
	// device nobody can commission.
	fmt.Fprintf(&b, "vendor:        %s\n", cfg.Device.Vendor)
	fmt.Fprintf(&b, "model:         %s\n", cfg.Device.Model)
	fmt.Fprintf(&b, "version:       %s\n", cfg.Device.Version)
	fmt.Fprintf(&b, "serial:        %s\n", cfg.Device.Serial)
	fmt.Fprintf(&b, "address:       %d\n", cfg.Address)
	fmt.Fprintf(&b, "master:        %d\n", cfg.Master)
	fmt.Fprintf(&b, "binary_inputs: %d\n", cfg.Points.Binary)
	fmt.Fprintf(&b, "analog_inputs: %d\n", cfg.Points.Analog)
	fmt.Fprintf(&b, "counters:      %d\n", cfg.Points.Counter)
	return b.String()
}

// configFile is a YAML rendering of the running configuration, which is the
// file an engineer would expect to pull off a device and edit.
func configFile(cfg *Config) string {
	var b strings.Builder
	b.WriteString("# The running configuration of this simulated outstation.\n")
	b.WriteString("# Writing it back changes nothing: the simulator keeps what\n")
	b.WriteString("# a master sends so the transfer can be verified, and goes on\n")
	b.WriteString("# running the plant it started with.\n")
	fmt.Fprintf(&b, "address: %d\nmaster: %d\n", cfg.Address, cfg.Master)

	if len(cfg.Breakers) > 0 {
		b.WriteString("breakers:\n")
		for _, br := range cfg.Breakers {
			fmt.Fprintf(&b, "  - {status_index: %d, control_index: %d, name: %q}\n",
				br.StatusIndex, br.ControlIndex, br.Name)
		}
	}
	if len(cfg.Analogs) > 0 {
		b.WriteString("analogs:\n")
		for _, a := range cfg.Analogs {
			fmt.Fprintf(&b, "  - {index: %d, name: %q, units: %q}\n", a.Index, a.Name, a.Units)
		}
	}
	return b.String()
}

// eventLogFile is a plausible commissioning log, long enough that reading it
// takes several blocks — a transfer that fits in one block exercises none of
// the sequencing.
func eventLogFile(now time.Time) string {
	var b strings.Builder
	lines := []string{
		"cold start",
		"database initialised",
		"listening for master",
		"master connected",
		"integrity poll served",
		"clock synchronised by master",
		"breaker 0 opened by control",
		"breaker 0 closed by control",
		"analog 3 crossed its deadband",
		"event buffer high water mark 128",
	}
	for i, line := range lines {
		stamp := now.Add(-time.Duration(len(lines)-i) * time.Minute)
		fmt.Fprintf(&b, "%s  %s\n", stamp.Format("2006-01-02 15:04:05"), line)
	}
	return b.String()
}

// ---------- the FileHandler contract ----------

func (m *memFiles) lookup(name string) (*memFile, bool) {
	f, ok := m.files[clean(name)]
	return f, ok
}

// clean normalises a name the way a device with a flat namespace would: leading
// slash, no trailing one, no traversal.
func clean(name string) string {
	c := path.Clean("/" + strings.ReplaceAll(name, `\`, "/"))
	return c
}

func (m *memFiles) info(f *memFile) dnp3.FileInfo {
	info := dnp3.FileInfo{
		Name:        path.Base(f.name),
		Type:        dnp3.FileTypeSimple,
		Size:        uint32(len(f.data)),
		Created:     f.created,
		Permissions: f.perms,
	}
	if f.dir {
		info.Type = dnp3.FileTypeDirectory
		info.Size = 0
	}
	return info
}

func (m *memFiles) Info(name string) (dnp3.FileInfo, dnp3.FileStatus) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if clean(name) == "/" {
		return dnp3.FileInfo{
			Name: "/", Type: dnp3.FileTypeDirectory,
			Permissions: dnp3.PermOwnerRead | dnp3.PermOwnerExecute,
		}, dnp3.FileSuccess
	}
	f, ok := m.lookup(name)
	if !ok {
		return dnp3.FileInfo{}, dnp3.FileNotFound
	}
	return m.info(f), dnp3.FileSuccess
}

func (m *memFiles) List(name string) ([]dnp3.FileInfo, dnp3.FileStatus) {
	m.mu.Lock()
	defer m.mu.Unlock()

	dir := clean(name)
	if dir != "/" {
		f, ok := m.lookup(dir)
		if !ok {
			return nil, dnp3.FileNotFound
		}
		if !f.dir {
			return nil, dnp3.FileInvalidMode
		}
	}

	var out []dnp3.FileInfo
	for full, f := range m.files {
		if path.Dir(full) != dir {
			continue
		}
		out = append(out, m.info(f))
	}
	// Sorted so two reads of the same directory agree, which matters to
	// anything comparing captures.
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, dnp3.FileSuccess
}

func (m *memFiles) OpenRead(name string) (io.ReadCloser, dnp3.FileInfo, dnp3.FileStatus) {
	m.mu.Lock()
	defer m.mu.Unlock()

	f, ok := m.lookup(name)
	if !ok {
		return nil, dnp3.FileInfo{}, dnp3.FileNotFound
	}
	if f.dir {
		return nil, dnp3.FileInfo{}, dnp3.FileInvalidMode
	}
	if f.perms&dnp3.PermOwnerRead == 0 {
		return nil, dnp3.FileInfo{}, dnp3.FilePermissionDenied
	}

	// A copy: the transfer outlives this lock, and a file the master is part
	// way through reading must not change under it.
	return io.NopCloser(bytes.NewReader(bytes.Clone(f.data))), m.info(f), dnp3.FileSuccess
}

func (m *memFiles) OpenWrite(name string, mode dnp3.FileMode, _ uint32) (io.WriteCloser, dnp3.FileStatus) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.readOnly {
		return nil, dnp3.FilePermissionDenied
	}

	full := clean(name)
	if f, ok := m.files[full]; ok {
		if f.dir {
			return nil, dnp3.FileInvalidMode
		}
		if f.perms&dnp3.PermOwnerWrite == 0 {
			return nil, dnp3.FilePermissionDenied
		}
	}
	if dir := path.Dir(full); dir != "/" {
		if d, ok := m.files[dir]; !ok || !d.dir {
			return nil, dnp3.FileNotFound
		}
	}

	w := &memWriter{files: m, name: full, mode: mode}
	if mode == dnp3.FileModeAppend {
		if f, ok := m.files[full]; ok {
			w.buf.Write(f.data)
		}
	}
	return w, dnp3.FileSuccess
}

func (m *memFiles) Delete(name string) dnp3.FileStatus {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.readOnly {
		return dnp3.FilePermissionDenied
	}
	full := clean(name)
	f, ok := m.files[full]
	if !ok {
		return dnp3.FileNotFound
	}
	if f.dir {
		for other := range m.files {
			if path.Dir(other) == full {
				return dnp3.FileInvalidMode // a directory with things in it
			}
		}
	}
	delete(m.files, full)
	return dnp3.FileSuccess
}

// memWriter collects a transfer and installs it when the file is closed.
//
// Installing on close rather than block by block is what makes a failed
// transfer leave the previous contents intact — a half-written configuration
// is worse than an old one, and a real device that can afford the buffer does
// the same.
type memWriter struct {
	files *memFiles
	name  string
	mode  dnp3.FileMode
	buf   bytes.Buffer
	over  bool
}

func (w *memWriter) Write(p []byte) (int, error) {
	if w.buf.Len()+len(p) > memFileLimit {
		w.over = true
		return 0, fmt.Errorf("%s: a simulated file is capped at %d octets", w.name, memFileLimit)
	}
	return w.buf.Write(p)
}

func (w *memWriter) Close() error {
	if w.over {
		return nil // nothing is installed; the master was told at the time
	}

	w.files.mu.Lock()
	defer w.files.mu.Unlock()

	f, ok := w.files.files[w.name]
	if !ok {
		f = &memFile{
			name:  w.name,
			perms: dnp3.PermOwnerRead | dnp3.PermOwnerWrite | dnp3.PermGroupRead,
		}
		w.files.files[w.name] = f
	}
	f.data = bytes.Clone(w.buf.Bytes())
	f.created = time.Now()
	return nil
}

// Describe renders the filesystem for the startup banner, so whoever is
// pointing a master at this device can see what there is to ask for.
func (m *memFiles) Describe() string {
	m.mu.Lock()
	defer m.mu.Unlock()

	names := make([]string, 0, len(m.files))
	for name := range m.files {
		names = append(names, name)
	}
	sort.Strings(names)

	var b strings.Builder
	for _, name := range names {
		f := m.files[name]
		info := m.info(f)
		info.Name = name // the full path, since this is not one directory
		b.WriteString("    " + info.String() + "\n")
	}
	return b.String()
}

// buildFileConfig turns the flags and the configuration file into the
// library's file transfer settings.
//
// A real directory is opt-in and the in-memory device is the default, because
// serving the host filesystem is the one thing this tool should never do by
// accident.
func buildFileConfig(cfg *Config, sim *Simulator) (outstation.FileConfig, string, func() error, error) {
	fc := cfg.Files

	if fc.Disabled {
		return outstation.FileConfig{}, "disabled", nil, nil
	}

	settings := outstation.FileConfig{
		MaxBlockSize: fc.BlockSize,
		Timeout:      fc.Timeout,
	}

	if fc.Directory == "" {
		files := newMemFiles(cfg, sim, fc.ReadOnly)
		settings.Handler = files
		what := "simulated, in memory"
		if fc.ReadOnly {
			what += ", read-only"
		}
		return settings, what, nil, nil
	}

	dir, err := outstation.OpenDir(fc.Directory)
	if err != nil {
		return outstation.FileConfig{}, "", nil, fmt.Errorf("serving files from %s: %w", fc.Directory, err)
	}
	dir.ReadOnly = fc.ReadOnly
	settings.Handler = dir

	what := fc.Directory
	if fc.ReadOnly {
		what += " (read-only)"
	}
	return settings, what, dir.Close, nil
}
