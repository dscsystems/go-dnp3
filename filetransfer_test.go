package dnp3_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/channel"
	"github.com/dscsystems/go-dnp3/master"
	"github.com/dscsystems/go-dnp3/outstation"
)

// File transfer is the one exchange in DNP3 that is a conversation rather than
// a request, and the one whose failure is silent: a file reassembled in the
// wrong order, or cut off a block early, is still a file. So these tests run a
// real master against a real outstation over channel.Pipe and compare the
// octets that came out with the octets that went in.
//
// The block size is deliberately tiny. A transfer that fits in one block
// exercises none of the sequencing, and the last-block flag — the only thing
// that says a file has ended — is only interesting when there is more than one.

const testBlockSize = 16

// filePair builds a master and an outstation serving dir, and starts both.
func filePair(t *testing.T, files outstation.FileConfig) *master.Session {
	t.Helper()

	mch, och := channel.Pipe()

	out := outstation.New(outstation.Config{
		LocalAddr:      10,
		RemoteAddr:     1,
		Database:       outstation.DatabaseConfig{Binary: 1},
		ConfirmTimeout: time.Second,
		Files:          files,
	}, nil, nil)

	m := master.New(master.Config{
		LocalAddr:       1,
		RemoteAddr:      10,
		ResponseTimeout: 2 * time.Second,
		FileBlockSize:   testBlockSize,
	}, nil)

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
	return m
}

// serving builds a handler over a temporary directory holding the given files.
func serving(t *testing.T, files map[string]string) (*master.Session, string) {
	t.Helper()

	dir := t.TempDir()
	for name, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("preparing %s: %v", name, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}

	h, err := outstation.OpenDir(dir)
	if err != nil {
		t.Fatalf("opening the served directory: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })

	return filePair(t, outstation.FileConfig{Handler: h, MaxBlockSize: testBlockSize}), dir
}

// TestFileReadRoundTrip covers the block sizes that go wrong: a file that ends
// mid-block, one that ends exactly on a boundary — where the last block is
// full and only the flag says so — and one with nothing in it at all.
func TestFileReadRoundTrip(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"empty", ""},
		{"shorter than a block", "hello"},
		{"exactly one block", strings.Repeat("a", testBlockSize)},
		{"exactly two blocks", strings.Repeat("b", testBlockSize*2)},
		{"several blocks and a remainder", strings.Repeat("dnp3", 30)},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, _ := serving(t, map[string]string{"data.bin": c.content})

			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()

			got, err := m.ReadFileBytes(ctx, "/data.bin")
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if string(got) != c.content {
				t.Errorf("read %d octets, want %d\n got %q\nwant %q",
					len(got), len(c.content), got, c.content)
			}
		})
	}
}

// TestFileReadIntoWriter covers the streaming form, and the octet count a
// caller uses to tell a short file from a failed one.
func TestFileReadIntoWriter(t *testing.T) {
	content := strings.Repeat("x", 100)
	m, _ := serving(t, map[string]string{"log.txt": content})

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	var buf bytes.Buffer
	n, err := m.ReadFile(ctx, "/log.txt", &buf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if n != int64(len(content)) {
		t.Errorf("reported %d octets, want %d", n, len(content))
	}
	if buf.String() != content {
		t.Errorf("content mismatch")
	}
}

func TestFileWriteRoundTrip(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"empty", ""},
		{"shorter than a block", "short"},
		{"exactly one block", strings.Repeat("z", testBlockSize)},
		{"several blocks and a remainder", strings.Repeat("payload", 40)},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			content := c.content
			m, dir := serving(t, nil)

			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()

			if err := m.WriteFileBytes(ctx, "/uploaded.bin", []byte(content)); err != nil {
				t.Fatalf("write: %v", err)
			}

			got, err := os.ReadFile(filepath.Join(dir, "uploaded.bin"))
			if err != nil {
				t.Fatalf("the file was not created: %v", err)
			}
			if string(got) != content {
				t.Errorf("wrote %d octets, want %d\n got %q\nwant %q",
					len(got), len(content), got, content)
			}
		})
	}
}

// TestFileWriteThenRead is the round trip that matters most: what the master
// sent is what it gets back.
func TestFileWriteThenRead(t *testing.T) {
	m, _ := serving(t, nil)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	want := make([]byte, 500)
	for i := range want {
		want[i] = byte(i)
	}

	if err := m.WriteFileBytes(ctx, "/config.bin", want); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := m.ReadFileBytes(ctx, "/config.bin")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("the file came back different: %d octets, want %d", len(got), len(want))
	}
}

func TestReadDirectory(t *testing.T) {
	m, _ := serving(t, map[string]string{
		"alpha.txt":       "a",
		"beta.bin":        strings.Repeat("b", 40),
		"logs/events.log": "e",
	})

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	entries, err := m.ReadDirectory(ctx, "/")
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	byName := make(map[string]dnp3.FileInfo, len(entries))
	for _, e := range entries {
		byName[e.Name] = e
	}
	if len(byName) != 3 {
		t.Fatalf("listed %d entries, want 3: %v", len(byName), entries)
	}

	if got := byName["beta.bin"]; got.Size != 40 || got.IsDir() {
		t.Errorf("beta.bin = %+v, want a 40 octet file", got)
	}
	if got, ok := byName["logs"]; !ok || !got.IsDir() {
		t.Errorf("logs = %+v, want a directory", got)
	}
	// A listing whose entries lost their creation times would still parse; the
	// times are the part that silently degrades.
	if byName["alpha.txt"].Created.IsZero() {
		t.Error("alpha.txt has no creation time")
	}
}

func TestFileInfo(t *testing.T) {
	m, _ := serving(t, map[string]string{"settings.xml": strings.Repeat("s", 33)})

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	info, err := m.FileInfo(ctx, "/settings.xml")
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	if info.Name != "settings.xml" {
		t.Errorf("name %q, want settings.xml", info.Name)
	}
	if info.Size != 33 {
		t.Errorf("size %d, want 33", info.Size)
	}
	if info.IsDir() {
		t.Error("reported as a directory")
	}
}

func TestDeleteFile(t *testing.T) {
	m, dir := serving(t, map[string]string{"stale.log": "old"})

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	if err := m.DeleteFile(ctx, "/stale.log"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "stale.log")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the file is still there: %v", err)
	}
}

// TestFileErrors covers what a master is told when the answer is no. Each of
// these has to come back as something a caller can act on: retrying a missing
// file is pointless, and retrying a locked one is not.
func TestFileErrors(t *testing.T) {
	m, _ := serving(t, map[string]string{"readable.txt": "hello"})

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	t.Run("missing file", func(t *testing.T) {
		_, err := m.ReadFileBytes(ctx, "/nope.txt")
		if !errors.Is(err, dnp3.ErrFileTransfer) {
			t.Fatalf("error %v, want one wrapping dnp3.ErrFileTransfer", err)
		}
		if !strings.Contains(err.Error(), dnp3.FileNotFound.String()) {
			t.Errorf("error %q does not say the file was not found", err)
		}
	})

	t.Run("missing file info", func(t *testing.T) {
		if _, err := m.FileInfo(ctx, "/nope.txt"); !errors.Is(err, dnp3.ErrFileTransfer) {
			t.Errorf("error %v, want one wrapping dnp3.ErrFileTransfer", err)
		}
	})

	t.Run("deleting a missing file", func(t *testing.T) {
		if err := m.DeleteFile(ctx, "/nope.txt"); !errors.Is(err, dnp3.ErrFileTransfer) {
			t.Errorf("error %v, want one wrapping dnp3.ErrFileTransfer", err)
		}
	})

	t.Run("empty name", func(t *testing.T) {
		if _, err := m.ReadFileBytes(ctx, ""); !errors.Is(err, dnp3.ErrBadConfig) {
			t.Errorf("error %v, want one wrapping dnp3.ErrBadConfig", err)
		}
	})

	// A failed transfer must leave nothing open, or the next one is refused.
	t.Run("the next transfer still works", func(t *testing.T) {
		got, err := m.ReadFileBytes(ctx, "/readable.txt")
		if err != nil {
			t.Fatalf("read after a failure: %v", err)
		}
		if string(got) != "hello" {
			t.Errorf("read %q, want hello", got)
		}
	})
}

// TestFileTraversalRefused is the security case: a master must not be able to
// read a file outside the served directory, whatever the name looks like.
//
// The mechanism is not a blocklist. A name is cleaned as though the served
// directory were the whole filesystem, so "../secret.txt" resolves to
// "secret.txt" *inside* it — which is not there — and os.Root refuses anything
// that tries to leave by another route. Neither depends on this package
// spotting every spelling of "..".
func TestFileTraversalRefused(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret.txt")
	if err := os.WriteFile(secret, []byte("classified"), 0o600); err != nil {
		t.Fatal(err)
	}

	served := filepath.Join(dir, "public")
	if err := os.MkdirAll(served, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(served, "ok.txt"), []byte("fine"), 0o644); err != nil {
		t.Fatal(err)
	}

	h, err := outstation.OpenDir(served)
	if err != nil {
		t.Fatalf("opening the served directory: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })

	m := filePair(t, outstation.FileConfig{Handler: h, MaxBlockSize: testBlockSize})

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	for _, name := range []string{
		"../secret.txt",
		"/../secret.txt",
		"/public/../../secret.txt",
		`..\secret.txt`,
	} {
		t.Run(name, func(t *testing.T) {
			got, err := m.ReadFileBytes(ctx, name)
			if err == nil {
				t.Fatalf("the outstation served %q: %q", name, got)
			}
			if bytes.Contains(got, []byte("classified")) {
				t.Fatalf("%q escaped the served directory", name)
			}
		})
	}

	// The refusals must not have broken the handler for legitimate names.
	if got, err := m.ReadFileBytes(ctx, "/ok.txt"); err != nil || string(got) != "fine" {
		t.Errorf("a legitimate read after the refusals returned %q, %v", got, err)
	}
}

// TestReadOnlyHandler covers a device that publishes its files but does not
// accept new ones.
func TestReadOnlyHandler(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "fixed.txt"), []byte("read me"), 0o644); err != nil {
		t.Fatal(err)
	}

	h, err := outstation.OpenDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	h.ReadOnly = true
	t.Cleanup(func() { _ = h.Close() })

	m := filePair(t, outstation.FileConfig{Handler: h, MaxBlockSize: testBlockSize})

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	if got, err := m.ReadFileBytes(ctx, "/fixed.txt"); err != nil || string(got) != "read me" {
		t.Errorf("read returned %q, %v", got, err)
	}
	if err := m.WriteFileBytes(ctx, "/new.txt", []byte("nope")); !errors.Is(err, dnp3.ErrFileTransfer) {
		t.Errorf("write error %v, want one wrapping dnp3.ErrFileTransfer", err)
	}
	if err := m.DeleteFile(ctx, "/fixed.txt"); !errors.Is(err, dnp3.ErrFileTransfer) {
		t.Errorf("delete error %v, want one wrapping dnp3.ErrFileTransfer", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "fixed.txt")); err != nil {
		t.Errorf("the file was deleted anyway: %v", err)
	}
}

// TestFileTransferNotConfigured covers the default: an outstation with no
// handler answers the way a device without file transfer does, and the master
// reports that as something distinguishable from a refusal.
func TestFileTransferNotConfigured(t *testing.T) {
	m := filePair(t, outstation.FileConfig{})

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	if _, err := m.ReadFileBytes(ctx, "/anything"); !errors.Is(err, dnp3.ErrNotSupported) {
		t.Errorf("read error %v, want one wrapping dnp3.ErrNotSupported", err)
	}
	if err := m.WriteFileBytes(ctx, "/anything", []byte("x")); !errors.Is(err, dnp3.ErrNotSupported) {
		t.Errorf("write error %v, want one wrapping dnp3.ErrNotSupported", err)
	}
	if err := m.DeleteFile(ctx, "/anything"); !errors.Is(err, dnp3.ErrNotSupported) {
		t.Errorf("delete error %v, want one wrapping dnp3.ErrNotSupported", err)
	}

	// The file-info path is the one that got this wrong against a real device:
	// the response body is empty, reading it yields "carried no descriptor",
	// and that true-but-useless message hid the outstation's own statement
	// that it does not implement the function.
	if _, err := m.FileInfo(ctx, "/anything"); !errors.Is(err, dnp3.ErrNotSupported) {
		t.Errorf("file info error %v, want one wrapping dnp3.ErrNotSupported", err)
	}
	if _, err := m.ReadDirectory(ctx, "/"); !errors.Is(err, dnp3.ErrNotSupported) {
		t.Errorf("directory error %v, want one wrapping dnp3.ErrNotSupported", err)
	}
}

// TestFileTransferLeavesPollingWorking proves a transfer gives the session
// back: the outstation's own state machine has to survive a conversation that
// spans many requests.
func TestFileTransferLeavesPollingWorking(t *testing.T) {
	m, _ := serving(t, map[string]string{"data.bin": strings.Repeat("q", 200)})

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	if _, err := m.ReadFileBytes(ctx, "/data.bin"); err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := m.IntegrityPoll(ctx); err != nil {
		t.Fatalf("polling after a transfer: %v", err)
	}
}
