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
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/master"
	"github.com/dscsystems/go-dnp3/objects"
)

// The concurrency rule this whole file is built around: the DNP3 session runs
// in its own goroutine and never touches the model. It pushes messages in; the
// model only ever reads them. Update never blocks — every action is dispatched
// as a tea.Cmd that returns a result message — because a UI that blocks on a
// protocol timeout stops repainting for five seconds, and an operator reads
// that as the tool having crashed.

// Screen identifies a tab.
type Screen int

// Screens, in tab order.
const (
	ScreenOverview Screen = iota
	ScreenPoints
	ScreenEvents
	ScreenLog
	ScreenFiles
	ScreenHelp
	numScreens
)

var screenNames = [numScreens]string{"Overview", "Points", "Events", "Log", "Files", "Help"}

// isTable reports whether the screen draws a scrollable list.
func (s Screen) isTable() bool {
	return s == ScreenPoints || s == ScreenEvents || s == ScreenLog || s == ScreenFiles
}

// follows reports whether the screen has a newest row worth pinning to.
func (s Screen) follows() bool { return s == ScreenEvents || s == ScreenLog }

// scrolls reports whether the screen has more content than fits, which the
// help screen does on a short terminal even though it is not a list.
func (s Screen) scrolls() bool { return s.isTable() || s == ScreenHelp }

func (s Screen) String() string {
	if s >= 0 && s < numScreens {
		return screenNames[s]
	}
	return "Screen(?)"
}

// pointKey identifies a point in the model's tables.
type pointKey struct {
	Type  dnp3.PointType
	Index uint16
}

// histCap bounds the per-point trend. Two minutes of a one-second scan is
// enough to see a trend and small enough to keep ten thousand points cheap.
const histCap = 120

// pointState is one measurement as the UI knows it.
type pointState struct {
	Key      pointKey
	Value    string
	Num      float64
	HasNum   bool
	Flags    dnp3.Flags
	Time     dnp3.Timestamp
	Updated  time.Time
	First    time.Time
	IsEvent  bool
	Previous string
	Updates  int
	Events   int
	Hist     []float64
	GV       objects.GroupVar
	Class    dnp3.Class
}

// stale reports whether a point has not been refreshed recently enough to be
// trusted as live. Quality flags say what the device thinks; this says whether
// the device is still talking.
func (p *pointState) stale(now time.Time, limit time.Duration) bool {
	return limit > 0 && now.Sub(p.Updated) > limit
}

// eventRow is one entry in the sequence-of-events list.
type eventRow struct {
	At    time.Time
	Key   pointKey
	Value string
	Flags dnp3.Flags
	Stamp dnp3.Timestamp
	Class dnp3.Class
	GV    objects.GroupVar
}

// logRow is one line of the activity log.
type logRow struct {
	At    time.Time
	Level string
	Text  string
}

// Model is the whole application state.
type Model struct {
	width, height int
	screen        Screen

	conn      *connection
	status    string
	lastErr   string
	startedAt time.Time
	linkSince time.Time
	now       time.Time

	// points is keyed for update and pointsOrder keeps a stable natural order,
	// so a table does not reshuffle under the cursor every time a value
	// arrives. Display order is derived from it by visiblePoints.
	points      map[pointKey]*pointState
	pointsOrder []pointKey

	events []eventRow
	logs   []logRow
	files  filesState

	// One cursor and scroll offset per screen, so moving between tabs does not
	// lose the operator's place in a list they were reading.
	cursor [numScreens]int
	offset [numScreens]int

	follow bool
	detail bool
	filter string

	sortBy   sortKey
	sortDesc bool

	prompt promptState
	modal  modalState
	form   formState
	toast  toastState

	// sbo selects between select-before-operate and direct operate, and
	// confirm decides whether a control asks first. Both are on screen —
	// sbo as a toolbar button, confirm as a standing warning in the toolbar
	// when it is off — because an operator must never have to remember which
	// mode a control tool is in. Confirmation is deliberately not bindable to
	// a key: it is a decision made when starting the tool, not one to be
	// turned off by a keystroke next to the one that closes a breaker.
	sbo      bool
	confirm  bool
	pulseMs  uint32
	mouse    bool
	altmode  bool
	staleAge time.Duration

	// hover is the region under the pointer, and dragging is set while the
	// scrollbar is being pulled. Both exist only to make the pointer feel like
	// a pointer rather than a slow keyboard.
	hover    zone
	dragging bool

	stats     master.Stats
	iin       string
	connected bool

	// rate counts updates in 500ms buckets over the last ten seconds, which is
	// what turns "the link is up" into "the link is doing something".
	// rateHist keeps the resulting number over the last minute, because a
	// device that has gone quiet looks exactly like a healthy idle one until
	// you can see that it used to be busy.
	rate     [20]int
	rateIdx  int
	rateHist []float64

	quitting bool
}

// NewModel builds the initial model.
func NewModel(conn *connection) *Model {
	return &Model{
		screen:    ScreenOverview,
		conn:      conn,
		files:     newFilesState(),
		points:    map[pointKey]*pointState{},
		follow:    true,
		status:    "connecting",
		sortBy:    sortPoint,
		confirm:   true,
		sbo:       true,
		pulseMs:   1000,
		mouse:     true,
		altmode:   true,
		staleAge:  30 * time.Second,
		startedAt: time.Now(),
		now:       time.Now(),
	}
}

func (m *Model) Init() tea.Cmd {
	return tea.Batch(m.conn.wait(), tick())
}

// tickMsg drives the age column, the clock and the rate meter without needing
// a repaint on every protocol event.
type tickMsg time.Time

func tick() tea.Cmd {
	return tea.Tick(500*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tickMsg:
		m.now = time.Time(msg)
		m.rateHist = append(m.rateHist, m.eventRate())
		if len(m.rateHist) > histCap {
			m.rateHist = m.rateHist[len(m.rateHist)-histCap:]
		}
		m.rateIdx = (m.rateIdx + 1) % len(m.rate)
		m.rate[m.rateIdx] = 0
		m.toast.expire(m.now)
		return m, tick()

	case tea.KeyPressMsg:
		return m.HandleKey(msg.String())

	case tea.MouseWheelMsg:
		return m.HandleMouse(mouseEvent{x: msg.X, y: msg.Y, button: msg.Button, kind: mouseWheel})
	case tea.MouseClickMsg:
		return m.HandleMouse(mouseEvent{x: msg.X, y: msg.Y, button: msg.Button, kind: mouseClick})
	case tea.MouseMotionMsg:
		return m.HandleMouse(mouseEvent{x: msg.X, y: msg.Y, button: msg.Button, kind: mouseMotion})
	case tea.MouseReleaseMsg:
		return m.HandleMouse(mouseEvent{x: msg.X, y: msg.Y, button: msg.Button, kind: mouseRelease})

	case updateMsg:
		m.applyUpdate(msg)
		return m, m.conn.wait()

	case statusMsg:
		m.applyStatus(msg)
		return m, m.conn.wait()

	case logMsg:
		m.addLog(msg.level, msg.text)
		return m, m.conn.wait()

	case commandResultMsg:
		m.addLog(msg.level(), msg.text)
		m.toast.show(msg.level(), msg.text, m.now)
		return m, nil

	case filesMsg:
		m.applyFiles(msg)
		return m, nil

	case filePreviewMsg:
		m.applyPreview(msg)
		return m, nil
	}
	return m, nil
}

func (m *Model) applyStatus(msg statusMsg) {
	was := m.connected
	m.status = msg.text
	m.connected = msg.connected
	m.stats = msg.stats
	m.iin = msg.iin
	if msg.connected && !was {
		m.linkSince = time.Now()
		m.addLog("ok", "link up")
	}
	if !msg.connected && was {
		m.linkSince = time.Time{}
		m.addLog("warn", "link down")
	}
	if msg.err != "" && msg.err != m.lastErr {
		m.lastErr = msg.err
		m.addLog("error", msg.err)
	}
}

// HandleKey applies one keystroke, named the way Bubble Tea names it.
//
// Taking the key as a string rather than a tea.KeyPressMsg is what lets the
// whole interface be driven from a test without a terminal — synthesising the
// message for a special key means reproducing Bubble Tea's own encoding, and a
// test that does that is testing the encoding rather than the tool.
func (m *Model) HandleKey(key string) (tea.Model, tea.Cmd) {
	// A prompt or a dialog owns the keyboard while it is open. Controls are
	// issued from this interface, so a keystroke must never fall through to a
	// breaker while the operator believes they are typing.
	if m.prompt.active {
		return m.handlePromptKey(key)
	}
	if m.form.active {
		return m.handleFormKey(key)
	}
	if m.modal.kind != modalNone {
		return m.handleModalKey(key)
	}

	// The Files screen claims a few keys before the global bindings see them,
	// because a file browser needs meanings a table does not have: descending
	// into a directory, going back up, closing a file it is showing.
	if m.screen == ScreenFiles {
		if model, cmd, handled := m.handleFilesKey(key); handled {
			return model, cmd
		}
	}

	switch key {
	case "q", "ctrl+c":
		m.quitting = true
		return m, tea.Quit

	case "esc":
		switch {
		case m.filter != "":
			m.filter = ""
			m.toast.show("info", "filter cleared", m.now)
		case m.detail:
			m.detail = false
		}

	// ---- navigation ----
	case "tab", "right":
		return m, m.setScreen((m.screen + 1) % numScreens)
	case "shift+tab", "left":
		return m, m.setScreen((m.screen + numScreens - 1) % numScreens)
	case "1", "2", "3", "4", "5", "6":
		return m, m.setScreen(Screen(key[0] - '1'))
	case "?":
		return m, m.setScreen(ScreenHelp)

	case "up", "k":
		m.moveCursor(-1)
	case "down", "j":
		m.moveCursor(1)
	case "pgup", "ctrl+b":
		m.moveCursor(-m.pageSize())
	case "pgdown", "ctrl+f":
		m.moveCursor(m.pageSize())
	case "home", "g":
		m.jumpTo(0)
	case "end", "G":
		m.jumpTo(m.rowCount() - 1)

	// ---- view ----
	case "/":
		m.prompt = promptState{active: true, kind: promptFilter,
			label: "filter", input: m.filter}
		if m.screen == ScreenOverview || m.screen == ScreenHelp {
			m.setScreen(ScreenPoints)
		}
	case "f":
		m.follow = !m.follow
		m.toast.show("info", "follow "+onOff(m.follow), m.now)
	case "d", "enter", " ":
		return m.contextAction(key)
	case "r":
		m.sortDesc = !m.sortDesc
	case "<":
		m.cycleSort(-1)
	case ">":
		m.cycleSort(1)
	case "x":
		m.clearList()

	// ---- protocol ----
	case "i":
		m.addLog("info", "integrity poll requested")
		return m, m.conn.integrityPoll()
	case "p":
		m.addLog("info", "class 1/2/3 poll requested")
		return m, m.conn.classPoll()
	case "t":
		m.addLog("info", "time sync requested")
		return m, m.conn.syncTime(false)
	case "T":
		m.addLog("info", "time sync with delay measurement requested")
		return m, m.conn.syncTime(true)
	case "u":
		m.addLog("info", "enabling unsolicited classes 1/2/3")
		return m, m.conn.unsolicited(true)
	case "U":
		m.addLog("info", "disabling unsolicited classes 1/2/3")
		return m, m.conn.unsolicited(false)
	case "s":
		m.prompt = promptState{active: true, kind: promptRange,
			label: "scan range  group[.var] start-stop", input: ""}
	case "R":
		m.openRestartDialog()
	case "C":
		m.openConnectionForm()
	case "S":
		m.sbo = !m.sbo
		m.toast.show("info", "controls: "+controlMode(m.sbo), m.now)
	case "e":
		return m, m.export()

	// ---- controls ----
	case "c", "o":
		return m.quickControl(key == "c")
	case "b":
		return m.startDeadbandPrompt()
	}
	return m, nil
}

// setScreen switches tabs and returns whatever the new screen needs fetching
// on arrival.
//
// The Files screen is the only one with anything to fetch. The others fill
// themselves from what the session pushes at them, while a directory listing
// happens only because somebody asked — and a file browser that opens on an
// empty pane with an instruction to press a key is a file browser that looks
// broken.
func (m *Model) setScreen(s Screen) tea.Cmd {
	if s < 0 || s >= numScreens {
		return nil
	}
	m.screen = s

	if s == ScreenFiles && !m.files.listed && !m.files.busy && m.conn.session() != nil {
		return m.listDirectory(m.files.dir)
	}
	return nil
}

func (m *Model) pageSize() int {
	return max(m.height-chromeTop-chromeBottom-2, 1)
}

func (m *Model) moveCursor(delta int) {
	// Moving the cursor by hand is a deliberate statement that the operator
	// wants to read something, so it takes the view off the live tail.
	if delta != 0 && m.follow && m.screen.follows() {
		m.follow = false
	}
	m.cursor[m.screen] = min(max(m.cursor[m.screen]+delta, 0), max(m.rowCount()-1, 0))
}

func (m *Model) jumpTo(row int) {
	if m.follow && m.screen.follows() && row < m.rowCount()-1 {
		m.follow = false
	}
	m.cursor[m.screen] = min(max(row, 0), max(m.rowCount()-1, 0))
}

// scroll moves the window without moving the cursor, which is what a wheel
// does everywhere else.
func (m *Model) scroll(delta int) {
	if delta != 0 && m.follow && m.screen.follows() {
		m.follow = false
	}
	m.offset[m.screen] = max(m.offset[m.screen]+delta, 0)
	// The cursor is dragged along only when the scroll would leave it behind;
	// clampScroll does that with the real geometry at draw time.
	visible := max(m.height-chromeTop-chromeBottom-1, 1)
	off := m.offset[m.screen]
	if c := m.cursor[m.screen]; c < off {
		m.cursor[m.screen] = off
	} else if c >= off+visible {
		m.cursor[m.screen] = off + visible - 1
	}
}

func (m *Model) cycleSort(dir int) {
	if m.screen != ScreenPoints {
		return
	}
	order := []sortKey{sortPoint, sortValue, sortQuality, sortAge, sortTime}
	at := 0
	for i, k := range order {
		if k == m.sortBy {
			at = i
		}
	}
	m.sortBy = order[(at+dir+len(order))%len(order)]
	m.toast.show("info", "sorted by "+sortName(m.sortBy), m.now)
}

func (m *Model) clearList() {
	switch m.screen {
	case ScreenEvents:
		m.events = nil
		m.cursor[ScreenEvents], m.offset[ScreenEvents] = 0, 0
	case ScreenLog:
		m.logs = nil
		m.cursor[ScreenLog], m.offset[ScreenLog] = 0, 0
	case ScreenPoints:
		// Forgetting the points is how an operator re-reads a device after
		// changing its configuration, rather than restarting the tool.
		m.points = map[pointKey]*pointState{}
		m.pointsOrder = nil
		m.cursor[ScreenPoints], m.offset[ScreenPoints] = 0, 0
		m.toast.show("info", "point table cleared — press i to re-read", m.now)
	}
}

// contextAction is what enter, space and d do, which depends on what is under
// the cursor: a control point offers its controls, anything else opens the
// inspector.
func (m *Model) contextAction(key string) (tea.Model, tea.Cmd) {
	if key == "d" || m.screen != ScreenPoints {
		m.detail = !m.detail
		return m, nil
	}
	p, ok := m.selectedPoint()
	if !ok {
		m.detail = !m.detail
		return m, nil
	}
	switch p.Key.Type {
	case dnp3.TypeBinaryOutputStatus:
		m.openControlDialog(p)
	case dnp3.TypeAnalogOutputStatus:
		m.startAnalogPrompt(p)
	default:
		m.detail = !m.detail
	}
	return m, nil
}

// quickControl is the two-keystroke path for a breaker: c closes, o opens.
//
// Spelled out rather than offered as a generic "toggle", because a control
// that depends on the operator's idea of the current state is how the wrong
// breaker gets opened.
func (m *Model) quickControl(closing bool) (tea.Model, tea.Cmd) {
	p, ok := m.selectedControl()
	if !ok {
		m.addLog("warn", "select a binary output point first (Points screen)")
		m.toast.show("warn", "no binary output selected", m.now)
		return m, nil
	}
	op := latchOp(p.Index, closing)
	return m.issueControl(op)
}

// issueControl either asks first or sends, depending on the confirm setting.
func (m *Model) issueControl(op controlOp) (tea.Model, tea.Cmd) {
	if !m.confirm {
		m.addLog("info", op.describe(m.sbo))
		return m, m.conn.operate(op, m.sbo)
	}
	m.modal = modalState{
		kind:  modalConfirm,
		title: "Confirm control",
		lines: []string{
			op.describe(m.sbo),
			"",
			"This operates plant. Check the point before confirming.",
		},
		choices: []modalChoice{
			{key: "y", label: "Send it"},
			{key: "n", label: "Cancel"},
		},
		pending: m.conn.operate(op, m.sbo),
		desc:    op.describe(m.sbo),
	}
	return m, nil
}

func (m *Model) openControlDialog(p *pointState) {
	m.modal = modalState{
		kind:  modalControl,
		title: fmt.Sprintf("Control %s", pointLabel(p.Key)),
		lines: []string{
			fmt.Sprintf("currently %s, %s", p.Value, qualityText(p.Flags, p.Key.Type)),
			fmt.Sprintf("mode %s, pulse %dms", controlMode(m.sbo), m.pulseMs),
		},
		choices: []modalChoice{
			{key: "c", label: "Latch ON  (close)"},
			{key: "o", label: "Latch OFF (open)"},
			{key: "l", label: fmt.Sprintf("Pulse close %dms", m.pulseMs)},
			{key: "t", label: fmt.Sprintf("Pulse trip  %dms", m.pulseMs)},
			{key: "esc", label: "Cancel"},
		},
		target: p.Key,
	}
}

func (m *Model) openRestartDialog() {
	m.modal = modalState{
		kind:  modalRestart,
		title: "Restart outstation",
		lines: []string{
			"The device stops answering until it comes back.",
			"A cold restart reinitialises everything; a warm",
			"restart only the communications process.",
		},
		choices: []modalChoice{
			{key: "c", label: "Cold restart"},
			{key: "w", label: "Warm restart"},
			{key: "esc", label: "Cancel"},
		},
	}
}

func (m *Model) startAnalogPrompt(p *pointState) {
	m.prompt = promptState{
		active: true, kind: promptAnalog, target: p.Key,
		label: fmt.Sprintf("write %s  value[i16|i32|f32|f64]", pointLabel(p.Key)),
	}
}

func (m *Model) startDeadbandPrompt() (tea.Model, tea.Cmd) {
	p, ok := m.selectedPoint()
	if !ok || p.Key.Type != dnp3.TypeAnalog {
		m.toast.show("warn", "select an analog input first", m.now)
		return m, nil
	}
	m.prompt = promptState{
		active: true, kind: promptDeadband, target: p.Key,
		label: fmt.Sprintf("deadband for %s", pointLabel(p.Key)),
	}
	return m, nil
}

// selectedPoint returns the row under the cursor on the Points screen.
func (m *Model) selectedPoint() (*pointState, bool) {
	if m.screen != ScreenPoints {
		return nil, false
	}
	rows := m.visiblePoints()
	if c := m.cursor[ScreenPoints]; c >= 0 && c < len(rows) {
		return rows[c], true
	}
	return nil, false
}

// selectedControl returns the binary output under the cursor.
func (m *Model) selectedControl() (pointKey, bool) {
	p, ok := m.selectedPoint()
	if !ok || p.Key.Type != dnp3.TypeBinaryOutputStatus {
		return pointKey{}, false
	}
	return p.Key, true
}

// applyUpdate folds one measurement into the model.
func (m *Model) applyUpdate(u updateMsg) {
	k := pointKey{Type: u.Type, Index: u.Index}
	now := time.Now()

	p, ok := m.points[k]
	if !ok {
		p = &pointState{Key: k, First: now}
		m.points[k] = p
		m.pointsOrder = append(m.pointsOrder, k)
		sort.Slice(m.pointsOrder, func(i, j int) bool {
			a, b := m.pointsOrder[i], m.pointsOrder[j]
			if a.Type != b.Type {
				return a.Type < b.Type
			}
			return a.Index < b.Index
		})
	}

	if p.Value != u.Value {
		p.Previous = p.Value
	}
	p.Value, p.Flags, p.Time = u.Value, u.Flags, u.Stamp
	p.Num, p.HasNum = u.Num, u.HasNum
	p.Updated = now
	p.IsEvent = u.IsEvent
	p.Updates++
	p.GV, p.Class = u.GV, u.Class

	if u.HasNum {
		p.Hist = append(p.Hist, u.Num)
		if len(p.Hist) > histCap {
			p.Hist = p.Hist[len(p.Hist)-histCap:]
		}
	}

	m.rate[m.rateIdx]++

	if u.IsEvent {
		p.Events++
		m.events = append(m.events, eventRow{
			At: now, Key: k, Value: u.Value, Flags: u.Flags, Stamp: u.Stamp,
			Class: u.Class, GV: u.GV,
		})
		// Bound the list: an event storm would otherwise grow it without
		// limit, and nobody scrolls back ten thousand rows.
		if len(m.events) > 2000 {
			drop := len(m.events) - 2000
			m.events = m.events[drop:]
			// Keep the cursor on the row it was on rather than letting the
			// list slide under a reader.
			m.cursor[ScreenEvents] = max(m.cursor[ScreenEvents]-drop, 0)
			m.offset[ScreenEvents] = max(m.offset[ScreenEvents]-drop, 0)
		}
	}
}

func (m *Model) addLog(level, text string) {
	m.logs = append(m.logs, logRow{At: time.Now(), Level: level, Text: text})
	if len(m.logs) > 2000 {
		drop := len(m.logs) - 2000
		m.logs = m.logs[drop:]
		m.cursor[ScreenLog] = max(m.cursor[ScreenLog]-drop, 0)
		m.offset[ScreenLog] = max(m.offset[ScreenLog]-drop, 0)
	}
}

// eventRate returns measurements per second over the last ten seconds.
func (m *Model) eventRate() float64 {
	total := 0
	for _, n := range m.rate {
		total += n
	}
	return float64(total) / (float64(len(m.rate)) * 0.5)
}

// matchesFilter tests a row against the filter.
//
// The filter reads the whole row rather than just the point name: an operator
// hunting a fault types "comm_lost", and one hunting a value types "11".
func matchesFilter(filter string, fields ...string) bool {
	if filter == "" {
		return true
	}
	f := strings.ToLower(filter)
	for _, s := range fields {
		if strings.Contains(strings.ToLower(s), f) {
			return true
		}
	}
	return false
}

// visiblePoints returns the point rows after filtering and sorting.
func (m *Model) visiblePoints() []*pointState {
	out := make([]*pointState, 0, len(m.pointsOrder))
	for _, k := range m.pointsOrder {
		p := m.points[k]
		if !matchesFilter(m.filter, pointLabel(k), p.Value,
			qualityText(p.Flags, k.Type), k.Type.String()) {
			continue
		}
		out = append(out, p)
	}

	if m.sortBy != sortPoint && m.sortBy != sortNone {
		sort.SliceStable(out, func(i, j int) bool { return pointLess(out[i], out[j], m.sortBy) })
	}
	if m.sortDesc {
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	return out
}

func pointLess(a, b *pointState, key sortKey) bool {
	switch key {
	case sortValue:
		// Numbers compare as numbers; ON and OFF fall back to text, which puts
		// them in a stable order rather than an arbitrary one.
		if a.HasNum && b.HasNum {
			return a.Num < b.Num
		}
		return a.Value < b.Value
	case sortQuality:
		// Worst first: a sort on quality is a search for the broken points.
		return qualityRank(a.Flags) < qualityRank(b.Flags)
	case sortAge:
		return a.Updated.After(b.Updated)
	case sortTime:
		return a.Time.Time.After(b.Time.Time)
	}
	return false
}

// qualityRank orders points by how much they should worry an operator.
func qualityRank(f dnp3.Flags) int {
	switch {
	case !f.Has(dnp3.Online):
		return 0
	case f.HasAny(dnp3.CommLost):
		return 1
	case f.HasAny(dnp3.Restart):
		return 2
	case f.HasAny(dnp3.RemoteForced | dnp3.LocalForced):
		return 3
	default:
		return 4
	}
}

// visibleEvents returns the event rows after filtering.
func (m *Model) visibleEvents() []eventRow {
	if m.filter == "" {
		return m.events
	}
	out := make([]eventRow, 0, len(m.events))
	for _, e := range m.events {
		if matchesFilter(m.filter, pointLabel(e.Key), e.Value, qualityText(e.Flags, e.Key.Type)) {
			out = append(out, e)
		}
	}
	return out
}

// visibleLogs returns the log rows after filtering.
func (m *Model) visibleLogs() []logRow {
	if m.filter == "" {
		return m.logs
	}
	out := make([]logRow, 0, len(m.logs))
	for _, l := range m.logs {
		if matchesFilter(m.filter, l.Text, l.Level) {
			out = append(out, l)
		}
	}
	return out
}

func (m *Model) rowCount() int {
	switch m.screen {
	case ScreenPoints:
		return len(m.visiblePoints())
	case ScreenEvents:
		return len(m.visibleEvents())
	case ScreenLog:
		return len(m.visibleLogs())
	case ScreenFiles:
		if m.files.showingPreview() {
			return len(m.files.preview)
		}
		return len(m.visibleFiles())
	case ScreenHelp:
		return len(m.helpLines(m.bodyRect()))
	}
	return 0
}

// bodyRect is the content area, derived from the terminal size alone so that
// anything needing it — including the row count — can have it without a full
// layout pass.
func (m *Model) bodyRect() rect {
	return rect{x: 0, y: chromeTop, w: m.width, h: m.height - chromeTop - chromeBottom}
}

// ---------- prompts ----------

type promptKind int

const (
	promptFilter promptKind = iota
	promptAnalog
	promptDeadband
	promptRange
	promptFileSave
	promptFileUpload
	promptFileDir
)

type promptState struct {
	active bool
	kind   promptKind
	label  string
	input  string
	target pointKey
	// file is the device path a file prompt is about, carried alongside the
	// typed text because the cursor may move while the prompt is open.
	file string
}

// closePrompt dismisses a prompt without submitting it.
//
// A filter keeps whatever was typed, because it has been applied to the list
// with every keystroke and taking it away now would undo what the operator
// just watched happen. Anything that would send something — a setpoint, a
// deadband — is dropped instead, because a prompt abandoned halfway is not an
// instruction.
func (m *Model) closePrompt() {
	m.prompt = promptState{}
}

func (m *Model) handlePromptKey(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "esc":
		if m.prompt.kind == promptFilter {
			m.filter = ""
		}
		m.prompt = promptState{}
		return m, nil

	case "enter":
		p := m.prompt
		m.prompt = promptState{}
		return m.submitPrompt(p)

	case "backspace":
		if m.prompt.input != "" {
			r := []rune(m.prompt.input)
			m.prompt.input = string(r[:len(r)-1])
		}
	case "ctrl+u":
		m.prompt.input = ""
	case "space":
		m.prompt.input += " "
	default:
		if len([]rune(key)) == 1 {
			m.prompt.input += key
		}
	}
	if m.prompt.kind == promptFilter {
		// The filter applies as it is typed: an operator narrowing a list
		// wants to see it narrow.
		m.filter = m.prompt.input
		m.cursor[m.screen], m.offset[m.screen] = 0, 0
	}
	return m, nil
}

func (m *Model) submitPrompt(p promptState) (tea.Model, tea.Cmd) {
	switch p.kind {
	case promptFilter:
		m.filter = p.input

	case promptAnalog:
		cmd, desc, err := parseAnalogWrite(p.target.Index, p.input)
		if err != nil {
			m.toast.show("error", err.Error(), m.now)
			return m, nil
		}
		op := controlOp{label: desc, cmd: cmd}
		return m.issueControl(op)

	case promptDeadband:
		v, err := strconv.ParseFloat(strings.TrimSpace(p.input), 32)
		if err != nil {
			m.toast.show("error", "deadband: "+err.Error(), m.now)
			return m, nil
		}
		m.addLog("info", fmt.Sprintf("writing deadband %g to AI %d", v, p.target.Index))
		return m, m.conn.writeDeadband(p.target.Index, float32(v))

	case promptRange:
		g, v, start, stop, err := parseRangeScan(p.input)
		if err != nil {
			m.toast.show("error", err.Error(), m.now)
			return m, nil
		}
		m.addLog("info", fmt.Sprintf("range scan g%dv%d %d-%d", g, v, start, stop))
		return m, m.conn.rangeScan(g, v, start, stop)

	case promptFileSave:
		local, err := localName(p.input, p.file)
		if err != nil {
			m.toast.show("error", err.Error(), m.now)
			return m, nil
		}
		m.addLog("info", "saving "+p.file+" to "+local)
		return m, m.saveFile(p.file, local)

	case promptFileDir:
		dir := strings.TrimSpace(p.input)
		if dir == "" {
			return m, nil
		}
		return m, m.listDirectory(dir)

	case promptFileUpload:
		local := strings.TrimSpace(p.input)
		st, err := os.Stat(local)
		if err != nil {
			m.toast.show("error", err.Error(), m.now)
			return m, nil
		}
		if st.IsDir() {
			m.toast.show("error", local+" is a directory", m.now)
			return m, nil
		}
		name := filepath.Base(local)
		if err := printableName(name); err != nil {
			m.toast.show("error", err.Error(), m.now)
			return m, nil
		}
		return m.confirmUpload(local, m.files.remotePath(name), int(st.Size()))
	}
	return m, nil
}

// parseRangeScan reads "30.5 0-15", "30 0-15" or "30.5 0 15".
func parseRangeScan(s string) (group, variation uint8, start, stop uint16, err error) {
	f := strings.FieldsFunc(strings.TrimSpace(s), func(r rune) bool {
		return r == ' ' || r == ',' || r == '-'
	})
	if len(f) < 3 {
		return 0, 0, 0, 0, fmt.Errorf("range: want \"group[.var] start-stop\"")
	}
	gv := strings.SplitN(f[0], ".", 2)
	g64, err := strconv.ParseUint(gv[0], 10, 8)
	if err != nil {
		return 0, 0, 0, 0, fmt.Errorf("range: bad group %q", gv[0])
	}
	if len(gv) == 2 {
		v64, verr := strconv.ParseUint(gv[1], 10, 8)
		if verr != nil {
			return 0, 0, 0, 0, fmt.Errorf("range: bad variation %q", gv[1])
		}
		variation = uint8(v64)
		f = f[1:]
	} else if len(f) >= 4 {
		// Four numbers and no dot means the variation was written out
		// separately, as "30 2 0 15" or "30,2,0,15".
		v64, verr := strconv.ParseUint(f[1], 10, 8)
		if verr != nil {
			return 0, 0, 0, 0, fmt.Errorf("range: bad variation %q", f[1])
		}
		variation = uint8(v64)
		f = f[2:]
	} else {
		f = f[1:]
	}

	a, err := strconv.ParseUint(f[0], 10, 16)
	if err != nil {
		return 0, 0, 0, 0, fmt.Errorf("range: bad start %q", f[0])
	}
	b, err := strconv.ParseUint(f[1], 10, 16)
	if err != nil {
		return 0, 0, 0, 0, fmt.Errorf("range: bad stop %q", f[1])
	}
	if a > b {
		return 0, 0, 0, 0, fmt.Errorf("range: start %d is above stop %d", a, b)
	}
	return uint8(g64), variation, uint16(a), uint16(b), nil
}

// parseAnalogWrite reads a value and an optional explicit encoding.
//
// The encoding matters on the wire: an outstation that expects a 16-bit
// setpoint will reject a float, and an operator who has to guess will guess
// wrong. Bare integers go out as 32-bit integers and bare decimals as 32-bit
// floats, which is what most devices accept, and a suffix overrides that.
func parseAnalogWrite(index uint16, in string) (master.Command, string, error) {
	s := strings.TrimSpace(strings.ToLower(in))
	if s == "" {
		return master.Command{}, "", fmt.Errorf("write: no value given")
	}

	enc := ""
	for _, suffix := range []string{"i16", "i32", "f32", "f64"} {
		if strings.HasSuffix(s, suffix) {
			enc, s = suffix, strings.TrimSpace(strings.TrimSuffix(s, suffix))
			break
		}
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return master.Command{}, "", fmt.Errorf("write: %q is not a number", in)
	}
	if enc == "" {
		if v == float64(int64(v)) {
			enc = "i32"
		} else {
			enc = "f32"
		}
	}

	desc := fmt.Sprintf("write AO %d = %s (%s)", index, strings.TrimSpace(s), enc)
	switch enc {
	case "i16":
		if v < -32768 || v > 32767 {
			return master.Command{}, "", fmt.Errorf("write: %g does not fit in an int16", v)
		}
		return master.AnalogOutputInt16(index, int16(v)), desc, nil
	case "i32":
		if v < -2147483648 || v > 2147483647 {
			return master.Command{}, "", fmt.Errorf("write: %g does not fit in an int32", v)
		}
		return master.AnalogOutputInt32(index, int32(v)), desc, nil
	case "f64":
		return master.AnalogOutputFloat64(index, v), desc, nil
	default:
		return master.AnalogOutputFloat32(index, float32(v)), desc, nil
	}
}

// ---------- dialogs ----------

type modalKind int

const (
	modalNone modalKind = iota
	modalConfirm
	modalControl
	modalRestart
)

type modalChoice struct {
	key   string
	label string
}

type modalState struct {
	kind    modalKind
	title   string
	lines   []string
	choices []modalChoice
	target  pointKey
	pending tea.Cmd
	desc    string
}

func (m *Model) handleModalKey(key string) (tea.Model, tea.Cmd) {
	if key == "esc" || key == "q" || key == "n" {
		m.modal = modalState{}
		return m, nil
	}

	switch m.modal.kind {
	case modalConfirm:
		if key == "y" || key == "enter" {
			cmd, desc := m.modal.pending, m.modal.desc
			m.modal = modalState{}
			m.addLog("info", desc)
			return m, cmd
		}

	case modalControl:
		idx := m.modal.target.Index
		var op controlOp
		switch key {
		case "c":
			op = latchOp(idx, true)
		case "o":
			op = latchOp(idx, false)
		case "l":
			op = controlOp{label: fmt.Sprintf("pulse close BO %d for %dms", idx, m.pulseMs),
				cmd: master.Close(idx, m.pulseMs)}
		case "t":
			op = controlOp{label: fmt.Sprintf("pulse trip BO %d for %dms", idx, m.pulseMs),
				cmd: master.Trip(idx, m.pulseMs)}
		default:
			return m, nil
		}
		m.modal = modalState{}
		return m.issueControl(op)

	case modalRestart:
		var mode dnp3.RestartMode
		switch key {
		case "c":
			mode = dnp3.ColdRestart
		case "w":
			mode = dnp3.WarmRestart
		default:
			return m, nil
		}
		m.modal = modalState{}
		m.addLog("warn", mode.String()+" restart requested")
		return m, m.conn.restart(mode)
	}
	return m, nil
}

// ---------- toasts ----------

// toastState is the transient banner that reports what an action did.
//
// It duplicates the log line on purpose: the log is where an operator looks
// afterwards, and the banner is what tells them now, without making them
// change screens to find out whether a control went through.
type toastState struct {
	text  string
	level string
	until time.Time
}

func (t *toastState) show(level, text string, now time.Time) {
	if now.IsZero() {
		now = time.Now()
	}
	t.text, t.level, t.until = text, level, now.Add(6*time.Second)
}

func (t *toastState) expire(now time.Time) {
	if !t.until.IsZero() && now.After(t.until) {
		*t = toastState{}
	}
}

func (t toastState) active() bool { return t.text != "" }

// ---------- labels ----------

// pointLabel renders a point's identity: "AI 3", "BO 0".
func pointLabel(k pointKey) string {
	return fmt.Sprintf("%s %d", typeAbbrev(k.Type), k.Index)
}

func typeAbbrev(t dnp3.PointType) string {
	switch t {
	case dnp3.TypeBinary:
		return "BI"
	case dnp3.TypeDoubleBitBinary:
		return "DB"
	case dnp3.TypeCounter:
		return "CT"
	case dnp3.TypeFrozenCounter:
		return "FC"
	case dnp3.TypeAnalog:
		return "AI"
	case dnp3.TypeBinaryOutputStatus:
		return "BO"
	case dnp3.TypeAnalogOutputStatus:
		return "AO"
	case dnp3.TypeOctetString:
		return "OS"
	}
	return "??"
}

func sortName(k sortKey) string {
	switch k {
	case sortValue:
		return "value"
	case sortQuality:
		return "quality"
	case sortAge:
		return "age"
	case sortTime:
		return "timestamp"
	default:
		return "point"
	}
}

func controlMode(sbo bool) string {
	if sbo {
		return "select-before-operate"
	}
	return "direct operate"
}

func onOff(v bool) string {
	if v {
		return "on"
	}
	return "off"
}
