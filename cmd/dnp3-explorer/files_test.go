package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/channel"
	"github.com/dscsystems/go-dnp3/master"
)

// The Files screen writes to the device, so the tests that matter are the ones
// about what it does before it sends anything: that a delete asks first, that
// an upload names what it is about to overwrite, and that neither happens by
// pressing one key.

// seedFiles puts a listing into the model as if one had arrived.
func seedFiles(m *Model, dir string, entries ...dnp3.FileInfo) {
	m.applyFiles(filesMsg{dir: dir, entries: entries})
}

func file(name string, size uint32) dnp3.FileInfo {
	return dnp3.FileInfo{
		Name: name, Type: dnp3.FileTypeSimple, Size: size,
		Created: time.Now(), Permissions: dnp3.PermOwnerRead | dnp3.PermOwnerWrite,
	}
}

func dir(name string) dnp3.FileInfo {
	return dnp3.FileInfo{Name: name, Type: dnp3.FileTypeDirectory}
}

func TestFilesListingAndNavigation(t *testing.T) {
	m := testModel()
	m.screen = ScreenFiles
	seedFiles(m, "/", dir("logs"), file("config.yaml", 400))

	if got := m.rowCount(); got != 2 {
		t.Fatalf("rowCount = %d, want 2", got)
	}

	// Entering a directory asks for its listing; the model does not descend
	// until the answer arrives, because the device may say no.
	m, cmd := pressCmd(m, "enter")
	if cmd == nil {
		t.Fatal("enter on a directory issued no command")
	}
	if m.files.dir != "/" {
		t.Errorf("the model descended to %q before the listing arrived", m.files.dir)
	}

	seedFiles(m, "/logs", file("events.log", 900))
	if m.files.dir != "/logs" {
		t.Errorf("dir = %q, want /logs", m.files.dir)
	}
	if m.cursor[ScreenFiles] != 0 {
		t.Errorf("the cursor stayed at %d after changing directory", m.cursor[ScreenFiles])
	}

	// Going up is a listing of the parent.
	m, cmd = pressCmd(m, "-")
	if cmd == nil {
		t.Fatal("- issued no command")
	}
	seedFiles(m, "/", dir("logs"))
	if m.files.dir != "/" {
		t.Errorf("dir = %q, want /", m.files.dir)
	}

	// And at the top it says so rather than asking for the parent of nothing.
	m, cmd = pressCmd(m, "-")
	if cmd != nil {
		t.Error("- at the top issued a command")
	}
	if !strings.Contains(m.toast.text, "top") {
		t.Errorf("toast = %q, want something about being at the top", m.toast.text)
	}
}

func TestFilesPreview(t *testing.T) {
	m := testModel()
	m.screen = ScreenFiles
	seedFiles(m, "/", file("config.yaml", 40))

	m, cmd := pressCmd(m, "enter")
	if cmd == nil {
		t.Fatal("enter on a file issued no command")
	}

	m.applyPreview(filePreviewMsg{
		name: "/config.yaml", size: 40,
		lines: []string{"address: 10", "master: 1"},
	})
	if !m.files.showingPreview() {
		t.Fatal("the preview did not open")
	}
	if got := m.rowCount(); got != 2 {
		t.Errorf("rowCount = %d, want the 2 preview lines", got)
	}

	// The rendered screen has to show the contents, not the listing.
	if out := m.View().Content; !strings.Contains(out, "address: 10") {
		t.Error("the preview is not on screen")
	}

	m = press(m, "esc")
	if m.files.showingPreview() {
		t.Error("esc did not close the preview")
	}
	if got := m.rowCount(); got != 1 {
		t.Errorf("rowCount = %d, want the listing back", got)
	}
}

// A file too large to hold in memory is refused with an explanation rather
// than pulled down anyway.
func TestFilesPreviewRefusesHugeFiles(t *testing.T) {
	m := testModel()
	m.screen = ScreenFiles
	seedFiles(m, "/", file("firmware.bin", previewLimit+1))

	m, cmd := pressCmd(m, "enter")
	if cmd != nil {
		t.Error("enter on an oversized file started a transfer")
	}
	if !strings.Contains(m.toast.text, "save") {
		t.Errorf("toast = %q, want the suggestion to save it instead", m.toast.text)
	}
}

// TestFilesDeleteAsksFirst is the important one: D must open a dialog, and the
// delete must only be issued by answering it.
func TestFilesDeleteAsksFirst(t *testing.T) {
	m := testModel()
	m.screen = ScreenFiles
	m.confirm = false // even with control confirmation turned off
	seedFiles(m, "/", file("config.yaml", 400))

	m, cmd := pressCmd(m, "D")
	if cmd != nil {
		t.Fatal("D deleted the file without asking")
	}
	if m.modal.kind != modalConfirm {
		t.Fatalf("modal = %v, want a confirmation", m.modal.kind)
	}
	if !strings.Contains(strings.Join(m.modal.lines, "\n"), "/config.yaml") {
		t.Errorf("the dialog does not name the file: %v", m.modal.lines)
	}

	// Cancelling sends nothing.
	m, cmd = pressCmd(m, "n")
	if cmd != nil {
		t.Error("cancelling the dialog issued a command")
	}
	if m.modal.kind != modalNone {
		t.Error("the dialog stayed open")
	}

	// Confirming does.
	m = press(m, "D")
	m, cmd = pressCmd(m, "y")
	if cmd == nil {
		t.Error("confirming the dialog issued nothing")
	}
}

func TestFilesDeleteNeedsASelection(t *testing.T) {
	m := testModel()
	m.screen = ScreenFiles
	seedFiles(m, "/") // an empty directory

	m, cmd := pressCmd(m, "D")
	if cmd != nil || m.modal.kind != modalNone {
		t.Error("D with nothing selected started a delete")
	}
}

// A directory cannot be deleted from here, and saying so beats a status code
// from the device.
func TestFilesDeleteRefusesDirectories(t *testing.T) {
	m := testModel()
	m.screen = ScreenFiles
	seedFiles(m, "/", dir("logs"))

	m, cmd := pressCmd(m, "D")
	if cmd != nil || m.modal.kind != modalNone {
		t.Error("D on a directory started a delete")
	}
	if !strings.Contains(m.toast.text, "director") {
		t.Errorf("toast = %q", m.toast.text)
	}
}

func TestFilesSavePrompt(t *testing.T) {
	m := testModel()
	m.screen = ScreenFiles
	seedFiles(m, "/logs", file("events.log", 900))

	m = press(m, "w")
	if !m.prompt.active || m.prompt.kind != promptFileSave {
		t.Fatalf("w did not open the save prompt: %+v", m.prompt)
	}
	// The default is the file's own name, and the device path is remembered
	// separately so moving the cursor cannot change what gets saved.
	if m.prompt.input != "events.log" {
		t.Errorf("default = %q, want events.log", m.prompt.input)
	}
	if m.prompt.file != "/logs/events.log" {
		t.Errorf("target = %q, want /logs/events.log", m.prompt.file)
	}

	m, cmd := pressCmd(m, "enter")
	if cmd == nil {
		t.Error("submitting the prompt issued no command")
	}
	if m.prompt.active {
		t.Error("the prompt stayed open")
	}
}

// TestFilesUploadAsksFirst covers the other direction: choosing a local file
// opens a dialog that names what it would replace.
func TestFilesUploadAsksFirst(t *testing.T) {
	local := filepath.Join(t.TempDir(), "new-config.yaml")
	if err := os.WriteFile(local, []byte("address: 11\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := testModel()
	m.screen = ScreenFiles
	m.confirm = false
	seedFiles(m, "/", file("new-config.yaml", 400))

	m = press(m, "W")
	if !m.prompt.active || m.prompt.kind != promptFileUpload {
		t.Fatalf("W did not open the upload prompt: %+v", m.prompt)
	}

	m.prompt.input = local
	m, cmd := pressCmd(m, "enter")
	if cmd != nil {
		t.Fatal("the upload was sent without asking")
	}
	if m.modal.kind != modalConfirm {
		t.Fatalf("modal = %v, want a confirmation", m.modal.kind)
	}

	text := strings.Join(m.modal.lines, "\n")
	if !strings.Contains(text, local) || !strings.Contains(text, "/new-config.yaml") {
		t.Errorf("the dialog does not name both ends: %v", m.modal.lines)
	}
	if !strings.Contains(text, "Replacing") {
		t.Errorf("the dialog does not say it would replace an existing file: %v", m.modal.lines)
	}

	if _, cmd = pressCmd(m, "y"); cmd == nil {
		t.Error("confirming the upload issued nothing")
	}
}

func TestFilesUploadRejectsMissingLocalFile(t *testing.T) {
	m := testModel()
	m.screen = ScreenFiles
	seedFiles(m, "/")

	m = press(m, "W")
	m.prompt.input = filepath.Join(t.TempDir(), "not-there")
	m, cmd := pressCmd(m, "enter")

	if cmd != nil || m.modal.kind != modalNone {
		t.Error("a missing local file started a transfer")
	}
	if m.toast.text == "" {
		t.Error("nothing was reported")
	}
}

// The filter applies to a listing the way it does to every other table.
func TestFilesFilter(t *testing.T) {
	m := testModel()
	m.screen = ScreenFiles
	seedFiles(m, "/", file("config.yaml", 1), file("events.log", 2), file("device.txt", 3))

	m.filter = "log"
	if got := len(m.visibleFiles()); got != 1 {
		t.Fatalf("%d entries match, want 1", got)
	}
	if m.visibleFiles()[0].Name != "events.log" {
		t.Errorf("matched %q", m.visibleFiles()[0].Name)
	}
}

func TestRenderFileText(t *testing.T) {
	lines, more := renderFile([]byte("one\ntwo\r\nthree\n"))
	if more {
		t.Error("a short file was reported as truncated")
	}
	want := []string{"one", "two", "three"}
	if len(lines) != len(want) {
		t.Fatalf("got %d lines %q, want %d", len(lines), lines, len(want))
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, lines[i], want[i])
		}
	}
}

// Binary content is hex-dumped rather than rendered as mangled text: a
// firmware image and a configuration file both come off a device, and only one
// of them is worth trying to read.
func TestRenderFileBinary(t *testing.T) {
	data := []byte{0x00, 0x01, 0x02, 'G', 'O', 0xFF}
	lines, _ := renderFile(data)
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1", len(lines))
	}
	if !strings.Contains(lines[0], "00 01 02") {
		t.Errorf("no hex in %q", lines[0])
	}
	if !strings.Contains(lines[0], "|...GO.|") {
		t.Errorf("no character column in %q", lines[0])
	}
}

func TestHumanSize(t *testing.T) {
	cases := []struct {
		in   uint32
		want string
	}{
		{0, "0 B"}, {512, "512 B"}, {2048, "2.0 KiB"}, {3 << 20, "3.0 MiB"},
	}
	for _, c := range cases {
		if got := humanSize(c.in); got != c.want {
			t.Errorf("humanSize(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestLocalName(t *testing.T) {
	dir := t.TempDir()

	// A directory takes the file's own name.
	got, err := localName(dir, "/logs/events.log")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "events.log"); got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	// A path is taken as given.
	if got, err = localName("out.bin", "/x"); err != nil || got != "out.bin" {
		t.Errorf("got %q, %v", got, err)
	}
	if _, err = localName("  ", "/x"); err == nil {
		t.Error("an empty path was accepted")
	}
}

// TestFilesEndToEnd browses the demo device over a real session: list the
// root, read a file, and compare what arrived with what the device holds.
func TestFilesEndToEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	mch, och := channel.Pipe()
	demo := newDemoOutstation(discardLogger())

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = demo.session.Run(ctx, och) }()

	conn := &connection{msgs: make(chan tea.Msg, 64), ctx: ctx}
	sess := master.New(master.Config{
		LocalAddr: 1, RemoteAddr: 10, ResponseTimeout: 5 * time.Second,
	}, nil)
	conn.adopt(sess, link{Demo: true, Local: 1, Remote: 10})

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
		t.Fatal("the demo outstation never connected")
	}

	m := testModel()
	m.conn = conn
	m.screen = ScreenFiles

	// List the root by running the command the key would.
	msg := m.listDirectory("/")()
	listing, ok := msg.(filesMsg)
	if !ok {
		t.Fatalf("listing returned %T", msg)
	}
	if listing.err != "" {
		t.Fatalf("listing: %s", listing.err)
	}
	m.applyFiles(listing)

	byName := map[string]dnp3.FileInfo{}
	for _, e := range m.files.entries {
		byName[e.Name] = e
	}
	for _, want := range []string{"device.txt", "config.yaml", "logs", "firmware.bin"} {
		if _, ok := byName[want]; !ok {
			t.Errorf("the listing has no %s: %v", want, m.files.entries)
		}
	}
	if !byName["logs"].IsDir() {
		t.Error("logs is not reported as a directory")
	}

	// Read a text file and check what it says.
	pmsg, ok := m.previewFile("/device.txt")().(filePreviewMsg)
	if !ok {
		t.Fatal("the preview command returned the wrong message")
	}
	if pmsg.err != "" {
		t.Fatalf("preview: %s", pmsg.err)
	}
	m.applyPreview(pmsg)

	if !strings.Contains(strings.Join(m.files.preview, "\n"), "GO-DNP3 DEMO RTU") {
		t.Errorf("the file does not look like the device's: %q", m.files.preview)
	}

	// And a binary one comes back as a hex dump rather than as noise.
	pmsg, _ = m.previewFile("/firmware.bin")().(filePreviewMsg)
	if pmsg.err != "" {
		t.Fatalf("preview: %s", pmsg.err)
	}
	if pmsg.size != 512 {
		t.Errorf("read %d octets, want 512", pmsg.size)
	}
	if !strings.HasPrefix(pmsg.lines[0], "00000000  47 4F") {
		t.Errorf("the hex dump starts %q", pmsg.lines[0])
	}

	// Descending into the directory works over the wire too.
	listing, _ = m.listDirectory("/logs")().(filesMsg)
	if listing.err != "" {
		t.Fatalf("listing /logs: %s", listing.err)
	}
	if len(listing.entries) != 1 || listing.entries[0].Name != "events.log" {
		t.Errorf("/logs holds %v", listing.entries)
	}

	// Writing a file and reading it back is the round trip that proves the
	// screen's send path.
	local := filepath.Join(t.TempDir(), "upload.txt")
	content := strings.Repeat("uploaded by the explorer\n", 30)
	if err := os.WriteFile(local, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	res, ok := m.uploadFile(local, "/upload.txt")().(commandResultMsg)
	if !ok || !res.ok {
		t.Fatalf("upload: %+v", res)
	}

	pmsg, _ = m.previewFile("/upload.txt")().(filePreviewMsg)
	if pmsg.err != "" {
		t.Fatalf("reading back: %s", pmsg.err)
	}
	if got := strings.Join(pmsg.lines, "\n") + "\n"; got != content {
		t.Errorf("read back %d octets, want %d", len(got), len(content))
	}

	// Saving to disk streams rather than buffering, so it gets its own path.
	target := filepath.Join(t.TempDir(), "saved.txt")
	res, _ = m.saveFile("/upload.txt", target)().(commandResultMsg)
	if !res.ok {
		t.Fatalf("save: %+v", res)
	}
	saved, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(saved) != content {
		t.Errorf("the saved file differs: %d octets, want %d", len(saved), len(content))
	}

	// And deleting it takes it off the device.
	res, _ = m.deleteFile("/upload.txt")().(commandResultMsg)
	if !res.ok {
		t.Fatalf("delete: %+v", res)
	}
	listing, _ = m.listDirectory("/")().(filesMsg)
	for _, e := range listing.entries {
		if e.Name == "upload.txt" {
			t.Error("the file is still on the device")
		}
	}
}

// TestFilesGoToPath covers the way out of a device whose root is not "/".
// Without it a listing that fails on the first request is a dead end: there is
// no other way to name a directory.
func TestFilesGoToPath(t *testing.T) {
	m := testModel()
	m.screen = ScreenFiles

	// A device that refuses the root it was asked for.
	m.applyFiles(filesMsg{dir: "/", err: "file not found"})
	if out := m.View().Content; !strings.Contains(out, "press : to try another path") {
		t.Error("the failed listing does not say how to try another path")
	}

	m = press(m, ":")
	if !m.prompt.active || m.prompt.kind != promptFileDir {
		t.Fatalf(": did not open the path prompt: %+v", m.prompt)
	}
	if m.prompt.input != "/" {
		t.Errorf("the prompt starts at %q, want the current directory", m.prompt.input)
	}

	m.prompt.input = "C:/"
	m, cmd := pressCmd(m, "enter")
	if cmd == nil {
		t.Fatal("submitting a path issued no listing")
	}

	m.applyFiles(filesMsg{dir: "C:/", entries: []dnp3.FileInfo{file("Setup.log", 212), dir("Windows")}})
	if m.files.dir != "C:/" {
		t.Errorf("dir = %q, want C:/", m.files.dir)
	}
	if got := m.rowCount(); got != 2 {
		t.Errorf("rowCount = %d, want 2", got)
	}
}

// TestFilesWindowsPaths covers a device that names things with a drive letter
// and backslashes: joining an entry onto the directory has to produce a path
// that device has heard of.
func TestFilesWindowsPaths(t *testing.T) {
	cases := []struct {
		dir, name, want string
	}{
		{"/", "config.yaml", "/config.yaml"},
		{"/logs", "events.log", "/logs/events.log"},
		{"C:/", "Setup.log", "C:/Setup.log"},
		{"C:/Windows", "win.ini", "C:/Windows/win.ini"},
		{`C:\`, "Setup.log", `C:\Setup.log`},
		{`C:\Windows`, "win.ini", `C:\Windows\win.ini`},
		{".", "readme.txt", "readme.txt"},
	}
	for _, c := range cases {
		f := &filesState{dir: c.dir}
		if got := f.remotePath(c.name); got != c.want {
			t.Errorf("join(%q, %q) = %q, want %q", c.dir, c.name, got, c.want)
		}
	}
}

func TestFilesParent(t *testing.T) {
	cases := []struct{ dir, want string }{
		{"/", ""},
		{"", ""},
		{".", ""},
		{"/logs", "/"},
		{"/a/b/c", "/a/b"},
		{"C:/Windows", "C:"},
		{`C:\Windows\Logs`, `C:\Windows`},
	}
	for _, c := range cases {
		f := &filesState{dir: c.dir}
		if got := f.parent(); got != c.want {
			t.Errorf("parent(%q) = %q, want %q", c.dir, got, c.want)
		}
	}
}

// Arriving on the Files screen fetches the listing, rather than showing an
// empty pane with an instruction to press a key.
func TestFilesListsOnArrival(t *testing.T) {
	m := testModel()

	// With no session there is nothing to ask, so nothing is sent.
	if cmd := m.setScreen(ScreenFiles); cmd != nil {
		t.Error("a listing was issued with no session")
	}

	m = testModel()
	m.conn.adopt(&master.Session{}, link{})
	m.screen = ScreenOverview

	m, cmd := pressCmd(m, "5")
	if m.screen != ScreenFiles {
		t.Fatalf("screen = %v", m.screen)
	}
	if cmd == nil {
		t.Fatal("arriving on the Files screen fetched nothing")
	}

	// And it does not fetch again on every visit.
	m.applyFiles(filesMsg{dir: "/", entries: []dnp3.FileInfo{file("a", 1)}})
	m = press(m, "1")
	m, cmd = pressCmd(m, "5")
	if cmd != nil {
		t.Error("coming back to the screen re-listed it")
	}
}
