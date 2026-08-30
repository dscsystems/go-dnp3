package outstation

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dscsystems/go-dnp3"
)

// TestResolve covers the name handling that stands between a master and the
// filesystem.
//
// Traversal is collapsed rather than rejected: an absolute path cleaned against
// the served directory can never climb out of it, which is what a device does
// and what a master expects. os.Root is the backstop for anything — a symlink —
// that tries to escape by another route.
func TestResolve(t *testing.T) {
	cases := []struct {
		name string
		want string
	}{
		{"/config.xml", "config.xml"},
		{"config.xml", "config.xml"},
		{"/logs/events.log", "logs/events.log"},
		{"/", "."},
		{"", "."},
		{".", "."},
		{"//double//slash//", "double/slash"},
		{"/logs/../config.xml", "config.xml"},
		{`\windows\style`, "windows/style"},

		// Everything that climbs lands back inside the root.
		{"../secret", "secret"},
		{"/../secret", "secret"},
		{"/a/../../secret", "secret"},
		{`..\secret`, "secret"},
		{"../../../../etc/passwd", "etc/passwd"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := resolve(c.name)
			if got != c.want {
				t.Errorf("resolve(%q) = %q, want %q", c.name, got, c.want)
			}
			// Whatever comes out, it must be a relative path that does not
			// begin by climbing: that is the property os.Root then enforces.
			if strings.HasPrefix(got, "/") || strings.HasPrefix(got, "../") || got == ".." {
				t.Errorf("resolve(%q) = %q, which leaves the root", c.name, got)
			}
		})
	}
}

// TestReadBlock covers the look-ahead that decides which block is the last.
//
// It is the one piece of the transfer that cannot be checked from the octets
// alone: a full block at the end of a file is indistinguishable from a full
// block in the middle until something says otherwise, and a master that never
// sees the flag waits for a block that is not coming.
func TestReadBlock(t *testing.T) {
	const blockSize = 8

	cases := []struct {
		name    string
		content string
		want    []string
	}{
		{"empty", "", []string{""}},
		{"shorter than a block", "abc", []string{"abc"}},
		{"exactly one block", "01234567", []string{"01234567"}},
		{"a block and a bit", "01234567ab", []string{"01234567", "ab"}},
		{"exactly two blocks", "0123456789abcdef", []string{"01234567", "89abcdef"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := io.NopCloser(strings.NewReader(c.content))
			tr := &transfer{
				r:         r,
				br:        bufio.NewReaderSize(r, blockSize+1),
				blockSize: blockSize,
			}

			var got []string
			for i := 0; ; i++ {
				data, last, err := readBlock(tr)
				if err != nil {
					t.Fatalf("block %d: %v", i, err)
				}
				got = append(got, string(data))
				if last {
					break
				}
				if i > 10 {
					t.Fatal("the reader never reported a last block")
				}
			}

			if len(got) != len(c.want) {
				t.Fatalf("read %d blocks %q, want %d %q", len(got), got, len(c.want), c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("block %d = %q, want %q", i, got[i], c.want[i])
				}
			}
			if strings.Join(got, "") != c.content {
				t.Errorf("the blocks do not reassemble to the file")
			}
		})
	}
}

func TestDirFileHandler(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}

	h, err := OpenDir(dir)
	if err != nil {
		t.Fatalf("OpenDir: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })

	t.Run("info", func(t *testing.T) {
		info, st := h.Info("/a.txt")
		if st != dnp3.FileSuccess {
			t.Fatalf("status %s", st)
		}
		if info.Name != "a.txt" || info.Size != 5 || info.IsDir() {
			t.Errorf("info = %+v", info)
		}
		if info.Created.IsZero() {
			t.Error("no modification time reported")
		}
	})

	t.Run("info on a directory", func(t *testing.T) {
		info, st := h.Info("/sub")
		if st != dnp3.FileSuccess || !info.IsDir() {
			t.Errorf("info = %+v, status %s", info, st)
		}
	})

	t.Run("info on something missing", func(t *testing.T) {
		if _, st := h.Info("/nope"); st != dnp3.FileNotFound {
			t.Errorf("status %s, want %s", st, dnp3.FileNotFound)
		}
	})

	t.Run("list", func(t *testing.T) {
		entries, st := h.List("/")
		if st != dnp3.FileSuccess {
			t.Fatalf("status %s", st)
		}
		if len(entries) != 2 {
			t.Fatalf("listed %d entries, want 2: %v", len(entries), entries)
		}
	})

	t.Run("read", func(t *testing.T) {
		rc, info, st := h.OpenRead("/a.txt")
		if st != dnp3.FileSuccess {
			t.Fatalf("status %s", st)
		}
		defer rc.Close()

		var buf bytes.Buffer
		if _, err := io.Copy(&buf, rc); err != nil {
			t.Fatal(err)
		}
		if buf.String() != "hello" {
			t.Errorf("read %q, want hello", buf.String())
		}
		if info.Size != 5 {
			t.Errorf("size %d, want 5", info.Size)
		}
	})

	// A directory is served as a listing, not as a stream of whatever the
	// filesystem keeps in it.
	t.Run("reading a directory as a stream is refused", func(t *testing.T) {
		if _, _, st := h.OpenRead("/sub"); st != dnp3.FileInvalidMode {
			t.Errorf("status %s, want %s", st, dnp3.FileInvalidMode)
		}
	})

	t.Run("write and append", func(t *testing.T) {
		wc, st := h.OpenWrite("/new.txt", dnp3.FileModeWrite, 3)
		if st != dnp3.FileSuccess {
			t.Fatalf("status %s", st)
		}
		if _, err := wc.Write([]byte("one")); err != nil {
			t.Fatal(err)
		}
		if err := wc.Close(); err != nil {
			t.Fatal(err)
		}

		wc, st = h.OpenWrite("/new.txt", dnp3.FileModeAppend, 4)
		if st != dnp3.FileSuccess {
			t.Fatalf("append status %s", st)
		}
		if _, err := wc.Write([]byte("-two")); err != nil {
			t.Fatal(err)
		}
		if err := wc.Close(); err != nil {
			t.Fatal(err)
		}

		got, err := os.ReadFile(filepath.Join(dir, "new.txt"))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "one-two" {
			t.Errorf("file holds %q, want one-two", got)
		}
	})

	t.Run("delete", func(t *testing.T) {
		if st := h.Delete("/new.txt"); st != dnp3.FileSuccess {
			t.Fatalf("status %s", st)
		}
		if st := h.Delete("/new.txt"); st != dnp3.FileNotFound {
			t.Errorf("second delete status %s, want %s", st, dnp3.FileNotFound)
		}
	})

	t.Run("traversal", func(t *testing.T) {
		for _, name := range []string{"../outside", "/../outside", `..\outside`} {
			if _, st := h.Info(name); st == dnp3.FileSuccess {
				t.Errorf("%q was resolved", name)
			}
			if _, _, st := h.OpenRead(name); st == dnp3.FileSuccess {
				t.Errorf("%q was opened", name)
			}
			if st := h.Delete(name); st == dnp3.FileSuccess {
				t.Errorf("%q was deleted", name)
			}
		}
	})
}

func TestDirFileHandlerReadOnly(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	h, err := OpenDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	h.ReadOnly = true
	t.Cleanup(func() { _ = h.Close() })

	if _, _, st := h.OpenRead("/a.txt"); st != dnp3.FileSuccess {
		t.Errorf("read status %s, want success", st)
	}
	if _, st := h.OpenWrite("/b.txt", dnp3.FileModeWrite, 1); st != dnp3.FilePermissionDenied {
		t.Errorf("write status %s, want %s", st, dnp3.FilePermissionDenied)
	}
	if st := h.Delete("/a.txt"); st != dnp3.FilePermissionDenied {
		t.Errorf("delete status %s, want %s", st, dnp3.FilePermissionDenied)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.txt")); err != nil {
		t.Errorf("the file was deleted anyway: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "b.txt")); err == nil {
		t.Error("the file was created anyway")
	}
}

func TestRejectingFileHandler(t *testing.T) {
	var h RejectingFileHandler

	if _, st := h.Info("/x"); st != dnp3.FilePermissionDenied {
		t.Errorf("info status %s", st)
	}
	if _, st := h.List("/"); st != dnp3.FilePermissionDenied {
		t.Errorf("list status %s", st)
	}
	if _, _, st := h.OpenRead("/x"); st != dnp3.FilePermissionDenied {
		t.Errorf("read status %s", st)
	}
	if _, st := h.OpenWrite("/x", dnp3.FileModeWrite, 0); st != dnp3.FilePermissionDenied {
		t.Errorf("write status %s", st)
	}
	if st := h.Delete("/x"); st != dnp3.FilePermissionDenied {
		t.Errorf("delete status %s", st)
	}
}

// TestNegotiateBlockSize covers the third constraint a device forgets: a block
// that does not fit in a response fragment cannot be sent at all.
func TestNegotiateBlockSize(t *testing.T) {
	s := &Session{cfg: Config{
		MaxTxFragment: 2048,
		Files:         FileConfig{MaxBlockSize: 1024},
	}}

	if got := s.negotiateBlockSize(512); got != 512 {
		t.Errorf("the master asked for 512 and got %d", got)
	}
	if got := s.negotiateBlockSize(4096); got != 1024 {
		t.Errorf("the master asked for more than we offer and got %d, want 1024", got)
	}
	if got := s.negotiateBlockSize(0); got != 1024 {
		t.Errorf("no request from the master gave %d, want our own 1024", got)
	}

	// A fragment too small for the configured block size caps it.
	s.cfg.MaxTxFragment = 300
	if got := s.negotiateBlockSize(1024); got != 268 {
		t.Errorf("block size %d, want it clamped to the fragment", got)
	}
}
