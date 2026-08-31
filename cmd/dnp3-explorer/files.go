// Copyright (C) 2026 Ricardo Olsen / DSC Systems.
//
// This program is free software: you can redistribute it and/or modify it
// under the terms of the GNU General Public License as published by the Free
// Software Foundation, either version 3 of the License, or (at your option)
// any later version. It is distributed WITHOUT ANY WARRANTY; without even the
// implied warranty of MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.
// See the GNU General Public License for more details, in the LICENSE file at
// the root of this repository or at <https://www.gnu.org/licenses/>.

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/dscsystems/go-dnp3"
)

// The Files screen is a file browser for the device: what is on it, what is in
// those files, and — carefully — what to put back.
//
// It is the one screen where the tool writes to the device without operating
// plant, and the failure modes are different from a control's. A control is
// instantaneous and reversible by another control; a file transfer takes many
// round trips, holds the session for its whole duration, and overwrites
// something the device may be relying on. So uploads and deletes ask first
// even when controls have been told not to, and the screen says what it is
// about to overwrite rather than only what it is about to send.

const (
	// previewLimit is the largest file this will pull into the pane. A preview
	// is for looking at a configuration or a log, and a master holds the whole
	// file in memory to show it; anything bigger belongs on disk, where the
	// transfer streams.
	previewLimit = 256 << 10

	// uploadLimit bounds what will be read off the local disk and sent. The
	// protocol's size field is 32 bits, but a terminal tool sending a hundred
	// megabytes over a poll loop is a mistake, not a feature.
	uploadLimit = 16 << 20

	// previewLines caps what is kept for display. A million-line log renders
	// as its first few hundred lines rather than as a pause.
	previewLines = 2000

	// transferTimeout bounds a whole transfer rather than one request. A file
	// is many round trips, and on a serial link the slow part is the line.
	transferTimeout = 5 * time.Minute
)

// filesState is the Files screen.
type filesState struct {
	// dir is the directory being listed, as the device names it.
	dir     string
	entries []dnp3.FileInfo

	// listed records that a listing has been attempted, so an empty directory
	// reads as empty rather than as "not loaded yet".
	listed  bool
	busy    bool
	lastErr string

	// preview holds a file being looked at. It replaces the listing until it
	// is closed, because a terminal has no room to show both honestly.
	preview     []string
	previewName string
	previewSize int
	previewMore bool
}

// newFilesState starts at the device's root, which is what a device with a
// flat namespace calls the only directory it has.
func newFilesState() filesState { return filesState{dir: "/"} }

// showingPreview reports whether the pane is showing a file rather than a
// listing.
func (f *filesState) showingPreview() bool { return f.previewName != "" }

// selected returns the entry under the cursor.
func (m *Model) selectedFile() (dnp3.FileInfo, bool) {
	rows := m.visibleFiles()
	i := m.cursor[ScreenFiles]
	if i < 0 || i >= len(rows) {
		return dnp3.FileInfo{}, false
	}
	return rows[i], true
}

// visibleFiles applies the filter, so a directory of a thousand logs can be
// narrowed the same way the points table is.
func (m *Model) visibleFiles() []dnp3.FileInfo {
	if m.filter == "" {
		return m.files.entries
	}
	out := make([]dnp3.FileInfo, 0, len(m.files.entries))
	for _, e := range m.files.entries {
		if matchesFilter(m.filter, e.Name) {
			out = append(out, e)
		}
	}
	return out
}

// remotePath joins a name onto the directory being listed.
//
// The separator is whatever the directory already uses. A device that answers
// to "C:\LOGS" is a device whose files are named with backslashes, and joining
// an entry onto it with a forward slash produces a path it has never heard of.
func (f *filesState) remotePath(name string) string {
	return joinRemote(f.dir, name)
}

func joinRemote(dir, name string) string {
	joined := path.Join(slashed(dir), name)
	if backslashed(dir) {
		return strings.ReplaceAll(joined, "/", `\`)
	}
	return joined
}

// parent is the directory above the one being listed, or "" at the top.
//
// What counts as the top depends on the device: "/" for one that looks like a
// filesystem, "C:/" for one that answers with a drive letter. Both are reached
// by walking up until the answer stops changing.
func (f *filesState) parent() string {
	dir := slashed(f.dir)
	if dir == "" || dir == "/" || dir == "." {
		return ""
	}

	up := path.Dir(dir)
	if up == dir {
		return ""
	}
	if backslashed(f.dir) {
		return strings.ReplaceAll(up, "/", `\`)
	}
	return up
}

// backslashed reports whether a device path is spelled the Windows way.
func backslashed(p string) bool {
	return strings.Contains(p, `\`) && !strings.Contains(p, "/")
}

// slashed rewrites a path for the arithmetic in path, which only knows about
// forward slashes.
func slashed(p string) string { return strings.ReplaceAll(p, `\`, "/") }

// ---------- key handling ----------

// handleFilesKey is the Files screen's own bindings, tried before the global
// ones so that a key meaning one thing on a table can mean another here.
//
// It returns false for anything it does not claim, which then falls through to
// the ordinary handling — the cursor keys, the filter, the tabs.
func (m *Model) handleFilesKey(key string) (tea.Model, tea.Cmd, bool) {
	switch key {
	case "esc":
		if m.files.showingPreview() {
			m.closePreview()
			return m, nil, true
		}
		return m, nil, false

	case "l", "ctrl+r":
		return m, m.listDirectory(m.files.dir), true

	case ":", "p":
		// Typing a path is not a nicety. A device names its root however it
		// likes — "/" for some, "C:/" or "." for others — and a browser that
		// can only ever ask for the one it was born with is a browser that
		// shows nothing on half the devices it meets.
		m.prompt = promptState{
			active: true, kind: promptFileDir,
			label: "list directory", input: m.files.dir,
		}
		return m, nil, true

	case "backspace", "-":
		parent := m.files.parent()
		if parent == "" {
			m.toast.show("info", "already at the top", m.now)
			return m, nil, true
		}
		return m, m.listDirectory(parent), true

	case "enter", " ", "d":
		return m.openSelectedFile(key)

	case "w":
		return m.promptDownload()

	case "W":
		return m.promptUpload()

	case "D":
		return m.confirmDelete()

	case "x":
		// On this screen there is no list to clear, so x closes the preview —
		// which is the only thing on it that accumulates.
		if m.files.showingPreview() {
			m.closePreview()
			return m, nil, true
		}
		return m, nil, true
	}
	return m, nil, false
}

// openSelectedFile descends into a directory or previews a file.
func (m *Model) openSelectedFile(key string) (tea.Model, tea.Cmd, bool) {
	if m.files.showingPreview() {
		// The pane is already showing a file; enter closes it, the way esc
		// does, rather than doing nothing.
		m.closePreview()
		return m, nil, true
	}
	if key == "d" {
		// d is the inspector everywhere else, and there is nothing here it
		// could inspect that the row does not already show.
		return m, nil, true
	}

	info, ok := m.selectedFile()
	if !ok {
		if !m.files.listed {
			return m, m.listDirectory(m.files.dir), true
		}
		return m, nil, true
	}

	if info.IsDir() {
		return m, m.listDirectory(m.files.remotePath(info.Name)), true
	}
	if info.Size > previewLimit {
		m.toast.show("warn", fmt.Sprintf(
			"%s is %s — too large to preview; press w to save it", info.Name, humanSize(info.Size)),
			m.now)
		return m, nil, true
	}
	return m, m.previewFile(m.files.remotePath(info.Name)), true
}

func (m *Model) closePreview() {
	m.files.preview = nil
	m.files.previewName = ""
	m.files.previewMore = false
	m.cursor[ScreenFiles] = min(m.cursor[ScreenFiles], max(m.rowCount()-1, 0))
	m.offset[ScreenFiles] = 0
}

// promptDownload asks where to put the file.
func (m *Model) promptDownload() (tea.Model, tea.Cmd, bool) {
	info, ok := m.selectedFile()
	if !ok || info.IsDir() {
		m.toast.show("warn", "select a file first", m.now)
		return m, nil, true
	}
	m.prompt = promptState{
		active: true, kind: promptFileSave,
		label: "save " + info.Name + " to", input: info.Name,
		file: m.files.remotePath(info.Name),
	}
	return m, nil, true
}

// promptUpload asks which local file to send.
func (m *Model) promptUpload() (tea.Model, tea.Cmd, bool) {
	m.prompt = promptState{
		active: true, kind: promptFileUpload,
		label: "send a local file to " + m.files.dir, input: "",
	}
	return m, nil, true
}

// confirmDelete asks before removing a file from the device.
//
// It asks even when controls are running unconfirmed: -no-confirm is a
// statement about operating plant quickly, not about deleting a device's
// configuration by mistyping one key next to another.
func (m *Model) confirmDelete() (tea.Model, tea.Cmd, bool) {
	info, ok := m.selectedFile()
	if !ok {
		m.toast.show("warn", "select a file first", m.now)
		return m, nil, true
	}
	if info.IsDir() {
		m.toast.show("warn", "directories cannot be deleted from here", m.now)
		return m, nil, true
	}

	remote := m.files.remotePath(info.Name)
	m.modal = modalState{
		kind:  modalConfirm,
		title: "Delete a file on the outstation",
		lines: []string{
			"",
			"  " + remote,
			"  " + humanSize(info.Size) + ", " + info.Permissions.String(),
			"",
			"  This cannot be undone from here.",
			"",
		},
		choices: []modalChoice{
			{key: "y", label: "delete it"},
			{key: "n", label: "cancel"},
		},
		pending: m.deleteFile(remote),
		desc:    "deleting " + remote,
	}
	return m, nil, true
}

// confirmUpload asks before overwriting whatever is on the device.
func (m *Model) confirmUpload(local, remote string, size int) (tea.Model, tea.Cmd) {
	existing := "  It does not exist on the device yet."
	for _, e := range m.files.entries {
		if e.Name == path.Base(remote) {
			existing = "  Replacing " + humanSize(e.Size) + " already there."
			break
		}
	}

	m.modal = modalState{
		kind:  modalConfirm,
		title: "Write a file to the outstation",
		lines: []string{
			"",
			"  from  " + local,
			"  to    " + remote,
			"  " + humanSize(uint32(size)),
			"",
			existing,
			"",
		},
		choices: []modalChoice{
			{key: "y", label: "send it"},
			{key: "n", label: "cancel"},
		},
		pending: m.uploadFile(local, remote),
		desc:    "writing " + local + " to " + remote,
	}
	return m, nil
}

// ---------- commands ----------

// filesMsg carries a directory listing back to the UI.
type filesMsg struct {
	dir     string
	entries []dnp3.FileInfo
	err     string
}

// filePreviewMsg carries a file's contents back for display.
type filePreviewMsg struct {
	name  string
	lines []string
	size  int
	more  bool
	err   string
}

// listDirectory reads a directory off the device.
func (m *Model) listDirectory(dir string) tea.Cmd {
	m.files.busy = true
	m.addLog("info", "listing "+dir)

	conn := m.conn
	return func() tea.Msg {
		sess := conn.session()
		if sess == nil {
			return filesMsg{dir: dir, err: "not connected"}
		}
		ctx, cancel := context.WithTimeout(conn.ctx, requestTimeout)
		defer cancel()

		entries, err := sess.ReadDirectory(ctx, dir)
		if err != nil {
			return filesMsg{dir: dir, err: err.Error()}
		}
		return filesMsg{dir: dir, entries: entries}
	}
}

// previewFile reads a file into the pane.
func (m *Model) previewFile(remote string) tea.Cmd {
	m.files.busy = true
	m.addLog("info", "reading "+remote)

	conn := m.conn
	return func() tea.Msg {
		sess := conn.session()
		if sess == nil {
			return filePreviewMsg{name: remote, err: "not connected"}
		}
		ctx, cancel := context.WithTimeout(conn.ctx, transferTimeout)
		defer cancel()

		data, err := sess.ReadFileBytes(ctx, remote)
		if err != nil {
			return filePreviewMsg{name: remote, err: err.Error()}
		}
		lines, more := renderFile(data)
		return filePreviewMsg{name: remote, lines: lines, size: len(data), more: more}
	}
}

// saveFile streams a file from the device to a local path.
//
// It streams rather than reading into memory first: the reason to save instead
// of preview is that the file is large.
func (m *Model) saveFile(remote, local string) tea.Cmd {
	conn := m.conn
	return func() tea.Msg {
		sess := conn.session()
		if sess == nil {
			return commandResultMsg{text: "save " + remote + ": not connected"}
		}

		f, err := os.Create(local)
		if err != nil {
			return commandResultMsg{text: "save " + remote + ": " + err.Error()}
		}
		defer f.Close()

		ctx, cancel := context.WithTimeout(conn.ctx, transferTimeout)
		defer cancel()

		n, err := sess.ReadFile(ctx, remote, f)
		if err != nil {
			// The partial file is left where it is and named, because deleting
			// what did arrive would throw away the evidence of how far the
			// transfer got.
			return commandResultMsg{text: fmt.Sprintf(
				"save %s failed after %s (kept %s): %v", remote, humanSize(uint32(n)), local, err)}
		}
		return commandResultMsg{
			text: fmt.Sprintf("saved %s to %s (%s)", remote, local, humanSize(uint32(n))),
			ok:   true,
		}
	}
}

// uploadFile sends a local file to the device.
func (m *Model) uploadFile(local, remote string) tea.Cmd {
	conn := m.conn
	dir := m.files.dir
	return func() tea.Msg {
		data, err := os.ReadFile(local)
		if err != nil {
			return commandResultMsg{text: "send " + local + ": " + err.Error()}
		}
		if len(data) > uploadLimit {
			return commandResultMsg{text: fmt.Sprintf(
				"send %s: %s exceeds the %s this tool will send",
				local, humanSize(uint32(len(data))), humanSize(uploadLimit))}
		}

		sess := conn.session()
		if sess == nil {
			return commandResultMsg{text: "send " + local + ": not connected"}
		}
		ctx, cancel := context.WithTimeout(conn.ctx, transferTimeout)
		defer cancel()

		if err := sess.WriteFileBytes(ctx, remote, data); err != nil {
			return commandResultMsg{text: "send " + local + " failed: " + err.Error()}
		}
		return commandResultMsg{
			text: fmt.Sprintf("sent %s to %s (%s); press l to re-list %s",
				local, remote, humanSize(uint32(len(data))), dir),
			ok: true,
		}
	}
}

// deleteFile removes a file from the device.
func (m *Model) deleteFile(remote string) tea.Cmd {
	conn := m.conn
	return func() tea.Msg {
		sess := conn.session()
		if sess == nil {
			return commandResultMsg{text: "delete " + remote + ": not connected"}
		}
		ctx, cancel := context.WithTimeout(conn.ctx, requestTimeout)
		defer cancel()

		if err := sess.DeleteFile(ctx, remote); err != nil {
			return commandResultMsg{text: "delete " + remote + " failed: " + err.Error()}
		}
		return commandResultMsg{text: "deleted " + remote + " — press l to re-list", ok: true}
	}
}

// ---------- message handling ----------

func (m *Model) applyFiles(msg filesMsg) {
	m.files.busy = false
	m.files.lastErr = msg.err

	if msg.err != "" {
		m.addLog("error", "listing "+msg.dir+": "+msg.err)
		m.toast.show("error", "listing "+msg.dir+": "+msg.err, m.now)
		// The directory is still recorded, so the header says what was asked
		// for rather than leaving the operator wondering where they are.
		m.files.dir, m.files.listed, m.files.entries = msg.dir, true, nil
		return
	}

	m.files.dir = msg.dir
	m.files.entries = msg.entries
	m.files.listed = true
	m.closePreview()
	m.cursor[ScreenFiles], m.offset[ScreenFiles] = 0, 0

	m.addLog("ok", fmt.Sprintf("%s: %d entries", msg.dir, len(msg.entries)))
}

func (m *Model) applyPreview(msg filePreviewMsg) {
	m.files.busy = false

	if msg.err != "" {
		m.addLog("error", "reading "+msg.name+": "+msg.err)
		m.toast.show("error", msg.err, m.now)
		return
	}

	m.files.preview = msg.lines
	m.files.previewName = msg.name
	m.files.previewSize = msg.size
	m.files.previewMore = msg.more
	m.cursor[ScreenFiles], m.offset[ScreenFiles] = 0, 0

	m.addLog("ok", fmt.Sprintf("read %s (%s)", msg.name, humanSize(uint32(msg.size))))
}

// ---------- rendering the contents ----------

// renderFile turns a file's octets into lines to look at.
//
// Text is shown as text. Anything else is shown as a hex dump rather than as
// mangled text: a configuration file and a firmware image are both things a
// master pulls off a device, and only one of them is worth trying to read.
func renderFile(data []byte) (lines []string, more bool) {
	if isBinary(data) {
		return hexDump(data), false
	}

	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	out := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(out) > previewLines {
		return out[:previewLines], true
	}
	return out, false
}

// isBinary reports whether the octets look like something other than text.
func isBinary(data []byte) bool {
	limit := min(len(data), 8000)
	odd := 0
	for _, b := range data[:limit] {
		switch {
		case b == 0:
			return true
		case b == '\n' || b == '\r' || b == '\t':
		case b < 0x20 || b == 0x7F:
			odd++
		}
	}
	return limit > 0 && odd*100/limit > 5
}

// hexDump renders octets the way a protocol analyser does: offset, hex, and
// the printable characters beside it.
func hexDump(data []byte) []string {
	const perLine = 16
	out := make([]string, 0, len(data)/perLine+1)

	for off := 0; off < len(data); off += perLine {
		end := min(off+perLine, len(data))
		chunk := data[off:end]

		var hex, text strings.Builder
		for i, b := range chunk {
			if i == perLine/2 {
				hex.WriteByte(' ')
			}
			fmt.Fprintf(&hex, "%02X ", b)
			if b >= 0x20 && b < 0x7F {
				text.WriteByte(b)
			} else {
				text.WriteByte('.')
			}
		}
		out = append(out, fmt.Sprintf("%08X  %-50s |%s|", off, hex.String(), text.String()))

		if len(out) >= previewLines {
			break
		}
	}
	return out
}

// humanSize renders an octet count the way a file listing does.
func humanSize(n uint32) string {
	switch {
	case n < 1024:
		return strconv.FormatUint(uint64(n), 10) + " B"
	case n < 1024*1024:
		return strconv.FormatFloat(float64(n)/1024, 'f', 1, 64) + " KiB"
	default:
		return strconv.FormatFloat(float64(n)/(1024*1024), 'f', 1, 64) + " MiB"
	}
}

// localName turns what was typed into a path to write, defaulting a bare
// directory to the file's own name.
func localName(input, remote string) (string, error) {
	in := strings.TrimSpace(input)
	if in == "" {
		return "", fmt.Errorf("give a local path")
	}
	if st, err := os.Stat(in); err == nil && st.IsDir() {
		return filepath.Join(in, path.Base(remote)), nil
	}
	return in, nil
}

// printableName rejects a name the device would not be able to hold, which is
// worth catching here rather than as a status code after a transfer.
func printableName(name string) error {
	if name == "" {
		return fmt.Errorf("give a file name")
	}
	for _, r := range name {
		if !unicode.IsPrint(r) {
			return fmt.Errorf("file names must be printable")
		}
	}
	return nil
}

// ---------- the view ----------

// viewFiles draws the listing, or the file being looked at.
func (m *Model) viewFiles(l layout) []string {
	if m.files.showingPreview() {
		return m.viewFilePreview(l)
	}

	rows := m.visibleFiles()

	empty := "press l to list " + m.files.dir
	switch {
	case m.files.busy:
		empty = "reading " + m.files.dir + "…"
	case m.files.lastErr != "":
		// The usual reason a first listing fails is that the device does not
		// call its root what this one asked for, so the way out goes in the
		// message rather than in the manual.
		empty = m.files.dir + ": " + m.files.lastErr +
			`  —  press : to try another path, such as "C:/" or "."`
	case m.files.listed && m.filter != "":
		empty = "nothing matches " + m.filter + " — press esc to clear the filter"
	case m.files.listed:
		empty = m.files.dir + " is empty"
	}

	body := m.renderTable(l, empty, func(i int) tableRow {
		e := rows[i]

		name := e.Name
		size := humanSize(e.Size)
		if e.IsDir() {
			// A trailing slash rather than a TYPE column: it is the convention
			// every file listing uses, and it costs no width on a narrow
			// terminal.
			name += "/"
			size = "—"
		}

		stamp := "—"
		if !e.Created.IsZero() {
			stamp = e.Created.Format("2006-01-02 15:04")
		}

		row := tableRow{cells: map[colID]string{
			colFileName:  name,
			colFileSize:  size,
			colFilePerms: e.Permissions.String(),
			colFileTime:  stamp,
		}}
		if e.IsDir() {
			row.cellStyle = map[colID]lipgloss.Style{colFileName: stKey}
		}
		return row
	})

	return append([]string{m.filesHeader(l.table.w)}, body...)
}

// filesHeader says which directory is on screen, because a listing with no
// path above it is the one way a file browser gets an operator lost.
//
// The entry count is not repeated here: the tab bar already carries it, along
// with the filter, for every list in the tool.
func (m *Model) filesHeader(w int) string {
	head := " " + m.files.dir
	if m.files.busy {
		head += "   reading…"
	}
	return stColHead.Render(fit(head, w))
}

// viewFilePreview draws a file's contents, scrolled by the ordinary table
// machinery so it behaves like every other list here.
func (m *Model) viewFilePreview(l layout) []string {
	lines := m.files.preview
	return m.renderTable(l, "the file is empty", func(i int) tableRow {
		return tableRow{cells: map[colID]string{colPreview: lines[i]}}
	})
}

// previewTitle is what the column heading says about the file on screen.
func (m *Model) previewTitle() string {
	title := path.Base(m.files.previewName) + "  " + humanSize(uint32(m.files.previewSize))
	if m.files.previewMore {
		title += "  truncated for display"
	}
	return title + "  ·  esc closes"
}

// ---------- the demo device's files ----------

// demoFiles is the filesystem the demo outstation serves.
//
// Demo mode exists so the interface can be explored without hardware, and a
// Files screen with nothing on it explores nothing. These are the files a
// device of this kind actually carries: what it is, how it is configured, and
// what it has been doing.
type demoFiles struct {
	mu    sync.Mutex
	files map[string][]byte
	dirs  map[string]bool
	when  time.Time
}

func newDemoFiles() *demoFiles {
	return &demoFiles{
		when: time.Now().Add(-36 * time.Hour),
		dirs: map[string]bool{"/logs": true},
		files: map[string][]byte{
			"/device.txt": []byte(
				"vendor:   DSC Systems\n" +
					"model:    GO-DNP3 DEMO RTU\n" +
					"firmware: 1.0.0-demo\n" +
					"address:  10\n"),
			"/config.yaml": []byte(
				"# The demo outstation's configuration.\n" +
					"address: 10\nmaster: 1\n\n" +
					"binary_inputs: 6\nanalog_inputs: 6\ncounters: 2\n"),
			"/logs/events.log": []byte(demoLog()),
			// Something that is not text, so the hex view has a reason to
			// exist and can be seen working.
			"/firmware.bin": demoBinary(),
		},
	}
}

func demoLog() string {
	var b strings.Builder
	start := time.Now().Add(-90 * time.Minute)
	for i, line := range []string{
		"cold start",
		"database initialised, 16 points",
		"master 1 connected",
		"integrity poll served",
		"clock set by master",
		"breaker 0 opened by control",
		"analog 2 crossed its deadband",
		"unsolicited enabled for classes 1 2 3",
	} {
		fmt.Fprintf(&b, "%s  %s\n", start.Add(time.Duration(i)*time.Minute).Format(time.DateTime), line)
	}
	return b.String()
}

func demoBinary() []byte {
	out := make([]byte, 512)
	for i := range out {
		out[i] = byte(i * 7)
	}
	copy(out, "GO-DNP3-FW\x00\x01")
	return out
}

func (d *demoFiles) info(name string, data []byte, isDir bool) dnp3.FileInfo {
	info := dnp3.FileInfo{
		Name:        path.Base(name),
		Type:        dnp3.FileTypeSimple,
		Size:        uint32(len(data)),
		Created:     d.when,
		Permissions: dnp3.PermOwnerRead | dnp3.PermOwnerWrite | dnp3.PermGroupRead,
	}
	if isDir {
		info.Type = dnp3.FileTypeDirectory
		info.Size = 0
		info.Permissions = dnp3.PermOwnerRead | dnp3.PermOwnerExecute
	}
	return info
}

// clean normalises a name against the device's root, so a name that climbs
// lands inside it rather than anywhere else.
func (d *demoFiles) clean(name string) string {
	return path.Clean("/" + strings.ReplaceAll(name, `\`, "/"))
}

func (d *demoFiles) Info(name string) (dnp3.FileInfo, dnp3.FileStatus) {
	d.mu.Lock()
	defer d.mu.Unlock()

	full := d.clean(name)
	if full == "/" || d.dirs[full] {
		return d.info(full, nil, true), dnp3.FileSuccess
	}
	data, ok := d.files[full]
	if !ok {
		return dnp3.FileInfo{}, dnp3.FileNotFound
	}
	return d.info(full, data, false), dnp3.FileSuccess
}

func (d *demoFiles) List(name string) ([]dnp3.FileInfo, dnp3.FileStatus) {
	d.mu.Lock()
	defer d.mu.Unlock()

	full := d.clean(name)
	if full != "/" && !d.dirs[full] {
		return nil, dnp3.FileNotFound
	}

	var out []dnp3.FileInfo
	for name := range d.dirs {
		if path.Dir(name) == full {
			out = append(out, d.info(name, nil, true))
		}
	}
	for name, data := range d.files {
		if path.Dir(name) == full {
			out = append(out, d.info(name, data, false))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, dnp3.FileSuccess
}

func (d *demoFiles) OpenRead(name string) (io.ReadCloser, dnp3.FileInfo, dnp3.FileStatus) {
	d.mu.Lock()
	defer d.mu.Unlock()

	full := d.clean(name)
	data, ok := d.files[full]
	if !ok {
		return nil, dnp3.FileInfo{}, dnp3.FileNotFound
	}
	return io.NopCloser(bytes.NewReader(bytes.Clone(data))), d.info(full, data, false), dnp3.FileSuccess
}

func (d *demoFiles) OpenWrite(name string, _ dnp3.FileMode, _ uint32) (io.WriteCloser, dnp3.FileStatus) {
	d.mu.Lock()
	full := d.clean(name)
	known := path.Dir(full) == "/" || d.dirs[path.Dir(full)]
	d.mu.Unlock()

	if !known {
		return nil, dnp3.FileNotFound
	}
	return &demoWriter{files: d, name: full}, dnp3.FileSuccess
}

func (d *demoFiles) Delete(name string) dnp3.FileStatus {
	d.mu.Lock()
	defer d.mu.Unlock()

	full := d.clean(name)
	if d.dirs[full] {
		return dnp3.FileInvalidMode
	}
	if _, ok := d.files[full]; !ok {
		return dnp3.FileNotFound
	}
	delete(d.files, full)
	return dnp3.FileSuccess
}

// demoWriter installs a written file when it is closed, so an abandoned
// transfer leaves what was there before.
type demoWriter struct {
	files *demoFiles
	name  string
	buf   bytes.Buffer
}

func (w *demoWriter) Write(p []byte) (int, error) {
	if w.buf.Len()+len(p) > uploadLimit {
		return 0, fmt.Errorf("the demo device holds at most %s per file", humanSize(uploadLimit))
	}
	return w.buf.Write(p)
}

func (w *demoWriter) Close() error {
	w.files.mu.Lock()
	defer w.files.mu.Unlock()
	w.files.files[w.name] = bytes.Clone(w.buf.Bytes())
	return nil
}
