package main

import (
	"io"
	"strings"
	"testing"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/outstation"
)

// The simulated filesystem is what a master under development points at, so
// the thing to check is that it behaves like a device: the files it advertises
// are the ones it serves, a directory lists what is in it and nothing else,
// and a write lands only when the transfer finishes.

func testFiles(t *testing.T) *memFiles {
	t.Helper()
	cfg := DefaultConfig()
	cfg.derivePoints()
	return newMemFiles(cfg, NewSimulator(cfg), false)
}

func TestMemFilesListing(t *testing.T) {
	m := testFiles(t)

	root, st := m.List("/")
	if st != dnp3.FileSuccess {
		t.Fatalf("listing /: %s", st)
	}

	byName := map[string]dnp3.FileInfo{}
	for _, e := range root {
		byName[e.Name] = e
	}
	for _, want := range []string{"device.txt", "points.txt", "config.yaml", "logs"} {
		if _, ok := byName[want]; !ok {
			t.Errorf("/ has no %s: %v", want, root)
		}
	}
	if !byName["logs"].IsDir() {
		t.Error("logs is not a directory")
	}
	// The listing is the directory's own entries, not everything on the
	// device: a nested file appearing at the root would have a master reading
	// paths that do not exist.
	if _, ok := byName["events.log"]; ok {
		t.Error("a file from /logs appeared in /")
	}

	sub, st := m.List("/logs")
	if st != dnp3.FileSuccess {
		t.Fatalf("listing /logs: %s", st)
	}
	if len(sub) != 1 || sub[0].Name != "events.log" {
		t.Errorf("/logs holds %v", sub)
	}

	if _, st := m.List("/nope"); st != dnp3.FileNotFound {
		t.Errorf("listing a missing directory gave %s", st)
	}
	// A file is not a directory, and saying so is what stops a master
	// treating its contents as a listing.
	if _, st := m.List("/device.txt"); st != dnp3.FileInvalidMode {
		t.Errorf("listing a file gave %s", st)
	}
}

// Everything the listing advertises has to be readable, and its size has to be
// the size that comes back — a master reading a file trusts the size to know
// when it is done.
func TestMemFilesSizesMatchContents(t *testing.T) {
	m := testFiles(t)

	for _, dir := range []string{"/", "/logs"} {
		entries, _ := m.List(dir)
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := strings.TrimSuffix(dir, "/") + "/" + e.Name

			rc, info, st := m.OpenRead(name)
			if st != dnp3.FileSuccess {
				t.Errorf("%s: %s", name, st)
				continue
			}
			data, err := io.ReadAll(rc)
			_ = rc.Close()
			if err != nil {
				t.Errorf("%s: %v", name, err)
				continue
			}
			if uint32(len(data)) != e.Size {
				t.Errorf("%s: listed %d octets, read %d", name, e.Size, len(data))
			}
			if info.Size != e.Size {
				t.Errorf("%s: open reported %d, listing said %d", name, info.Size, e.Size)
			}
			if len(data) == 0 {
				t.Errorf("%s is empty", name)
			}
		}
	}
}

func TestMemFilesWriteAndRead(t *testing.T) {
	m := testFiles(t)

	w, st := m.OpenWrite("/uploaded.txt", dnp3.FileModeWrite, 5)
	if st != dnp3.FileSuccess {
		t.Fatalf("open for writing: %s", st)
	}
	if _, err := w.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}

	// Nothing is installed until the transfer is closed: a half-written file
	// is worse than no file.
	if _, st := m.Info("/uploaded.txt"); st != dnp3.FileNotFound {
		t.Errorf("the file appeared before the transfer finished: %s", st)
	}

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	rc, info, st := m.OpenRead("/uploaded.txt")
	if st != dnp3.FileSuccess {
		t.Fatalf("open for reading: %s", st)
	}
	defer rc.Close()

	data, _ := io.ReadAll(rc)
	if string(data) != "hello" {
		t.Errorf("read %q, want hello", data)
	}
	if info.Size != 5 {
		t.Errorf("size %d, want 5", info.Size)
	}
}

func TestMemFilesAppend(t *testing.T) {
	m := testFiles(t)

	for i, part := range []string{"one", "-two"} {
		mode := dnp3.FileModeWrite
		if i > 0 {
			mode = dnp3.FileModeAppend
		}
		w, st := m.OpenWrite("/notes.txt", mode, uint32(len(part)))
		if st != dnp3.FileSuccess {
			t.Fatalf("open: %s", st)
		}
		if _, err := w.Write([]byte(part)); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	}

	rc, _, _ := m.OpenRead("/notes.txt")
	defer rc.Close()
	if data, _ := io.ReadAll(rc); string(data) != "one-two" {
		t.Errorf("read %q, want one-two", data)
	}
}

func TestMemFilesReadOnly(t *testing.T) {
	cfg := DefaultConfig()
	cfg.derivePoints()
	m := newMemFiles(cfg, NewSimulator(cfg), true)

	if _, _, st := m.OpenRead("/device.txt"); st != dnp3.FileSuccess {
		t.Errorf("reading gave %s", st)
	}
	if _, st := m.OpenWrite("/x", dnp3.FileModeWrite, 1); st != dnp3.FilePermissionDenied {
		t.Errorf("writing gave %s, want permission denied", st)
	}
	if st := m.Delete("/device.txt"); st != dnp3.FilePermissionDenied {
		t.Errorf("deleting gave %s, want permission denied", st)
	}
}

func TestMemFilesDelete(t *testing.T) {
	m := testFiles(t)

	if st := m.Delete("/config.yaml"); st != dnp3.FileSuccess {
		t.Fatalf("delete: %s", st)
	}
	if _, st := m.Info("/config.yaml"); st != dnp3.FileNotFound {
		t.Errorf("the file survived: %s", st)
	}
	if st := m.Delete("/config.yaml"); st != dnp3.FileNotFound {
		t.Errorf("a second delete gave %s", st)
	}
	// A directory with something in it is not removable, which is what stops
	// a master orphaning the file inside it.
	if st := m.Delete("/logs"); st != dnp3.FileInvalidMode {
		t.Errorf("deleting a non-empty directory gave %s", st)
	}
}

// Traversal is collapsed against the device's root, so a name that climbs
// lands on a file that is not there rather than on the host's filesystem.
func TestMemFilesNamesCannotEscape(t *testing.T) {
	m := testFiles(t)

	for _, name := range []string{"../../etc/passwd", "/../device.txt", `..\device.txt`} {
		info, st := m.Info(name)
		if st == dnp3.FileSuccess && info.Name != "device.txt" {
			t.Errorf("%q resolved to %q", name, info.Name)
		}
	}
	// The two that collapse onto a real file must land on the device's own.
	if _, st := m.Info("/../device.txt"); st != dnp3.FileSuccess {
		t.Errorf("/../device.txt gave %s, want the device's own file", st)
	}
}

// TestBuildFileConfig covers the three ways the tool can be started.
func TestBuildFileConfig(t *testing.T) {
	cfg := DefaultConfig()
	cfg.derivePoints()
	sim := NewSimulator(cfg)

	t.Run("simulated by default", func(t *testing.T) {
		fc, what, closer, err := buildFileConfig(cfg, sim)
		if err != nil {
			t.Fatal(err)
		}
		if closer != nil {
			t.Error("the in-memory device needs no closing")
		}
		if _, ok := fc.Handler.(*memFiles); !ok {
			t.Errorf("handler is %T, want the simulated files", fc.Handler)
		}
		if !strings.Contains(what, "memory") {
			t.Errorf("description %q does not say where the files are", what)
		}
	})

	t.Run("disabled", func(t *testing.T) {
		off := *cfg
		off.Files.Disabled = true
		fc, what, _, err := buildFileConfig(&off, sim)
		if err != nil {
			t.Fatal(err)
		}
		if fc.Handler != nil {
			t.Error("file transfer is meant to be off")
		}
		if what != "disabled" {
			t.Errorf("description %q", what)
		}
	})

	t.Run("a real directory", func(t *testing.T) {
		real := *cfg
		real.Files.Directory = t.TempDir()
		real.Files.ReadOnly = true

		fc, what, closer, err := buildFileConfig(&real, sim)
		if err != nil {
			t.Fatal(err)
		}
		if closer == nil {
			t.Fatal("a real directory has to be closed")
		}
		defer func() { _ = closer() }()

		h, ok := fc.Handler.(*outstation.DirFileHandler)
		if !ok {
			t.Fatalf("handler is %T, want a directory handler", fc.Handler)
		}
		if !h.ReadOnly {
			t.Error("read-only was not passed through")
		}
		if !strings.Contains(what, real.Files.Directory) {
			t.Errorf("description %q does not name the directory", what)
		}
	})

	t.Run("a directory that is not there", func(t *testing.T) {
		bad := *cfg
		bad.Files.Directory = t.TempDir() + "/nowhere"
		if _, _, _, err := buildFileConfig(&bad, sim); err == nil {
			t.Error("a missing directory was accepted")
		}
	})
}
