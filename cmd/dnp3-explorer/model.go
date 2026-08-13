package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/master"
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
	numScreens
)

var screenNames = [numScreens]string{"Overview", "Points", "Events", "Log"}

// pointKey identifies a point in the model's tables.
type pointKey struct {
	Type  dnp3.PointType
	Index uint16
}

// pointState is one measurement as the UI knows it.
type pointState struct {
	Key      pointKey
	Value    string
	Flags    dnp3.Flags
	Time     dnp3.Timestamp
	Updated  time.Time
	IsEvent  bool
	Previous string
}

// eventRow is one entry in the sequence-of-events list.
type eventRow struct {
	At    time.Time
	Key   pointKey
	Value string
	Flags dnp3.Flags
	Stamp dnp3.Timestamp
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

	conn    *connection
	status  string
	lastErr string

	// points is keyed for update and pointsOrder keeps a stable display order,
	// so a table does not reshuffle under the cursor every time a value
	// arrives.
	points      map[pointKey]*pointState
	pointsOrder []pointKey

	events []eventRow
	logs   []logRow

	cursor    int
	follow    bool
	filter    string
	filtering bool

	stats     master.Stats
	iin       string
	connected bool

	quitting bool
}

// NewModel builds the initial model.
func NewModel(conn *connection) *Model {
	return &Model{
		screen: ScreenOverview,
		conn:   conn,
		points: map[pointKey]*pointState{},
		follow: true,
		status: "connecting",
	}
}

func (m *Model) Init() tea.Cmd {
	return tea.Batch(m.conn.wait(), tick())
}

// tickMsg drives the age column and the sparkline without needing a repaint on
// every protocol event.
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
		return m, tick()

	case tea.KeyPressMsg:
		return m.HandleKey(msg.String())

	case updateMsg:
		m.applyUpdate(msg)
		return m, m.conn.wait()

	case statusMsg:
		m.status = msg.text
		m.connected = msg.connected
		m.stats = msg.stats
		m.iin = msg.iin
		if msg.err != "" {
			m.lastErr = msg.err
			m.addLog("error", msg.err)
		}
		return m, m.conn.wait()

	case logMsg:
		m.addLog(msg.level, msg.text)
		return m, m.conn.wait()

	case commandResultMsg:
		m.addLog(msg.level(), msg.text)
		return m, nil
	}
	return m, nil
}

// HandleKey applies one keystroke, named the way Bubble Tea names it.
//
// Taking the key as a string rather than a tea.KeyPressMsg is what lets the
// whole interface be driven from a test without a terminal — synthesising the
// message for a special key means reproducing Bubble Tea's own encoding, and a
// test that does that is testing the encoding rather than the tool.
func (m *Model) HandleKey(key string) (tea.Model, tea.Cmd) {
	// A filter box swallows most keys, so it is handled first.
	if m.filtering {
		switch key {
		case "enter", "esc":
			m.filtering = false
		case "backspace":
			if m.filter != "" {
				m.filter = m.filter[:len(m.filter)-1]
			}
		default:
			if len(key) == 1 {
				m.filter += key
			}
		}
		return m, nil
	}

	switch key {
	case "q", "ctrl+c":
		m.quitting = true
		return m, tea.Quit

	case "tab", "right", "l":
		m.screen = (m.screen + 1) % numScreens
		m.cursor = 0
	case "shift+tab", "left", "h":
		m.screen = (m.screen + numScreens - 1) % numScreens
		m.cursor = 0

	case "1", "2", "3", "4":
		m.screen = Screen(key[0] - '1')
		m.cursor = 0

	case "up", "k":
		m.cursor = max(m.cursor-1, 0)
	case "down", "j":
		m.cursor = min(m.cursor+1, max(m.rowCount()-1, 0))
	case "home", "g":
		m.cursor = 0
	case "end", "G":
		m.cursor = max(m.rowCount()-1, 0)

	case "/":
		m.filtering = true
		m.filter = ""

	case "f":
		m.follow = !m.follow

	case "i":
		m.addLog("info", "integrity poll requested")
		return m, m.conn.integrityPoll()

	case "p":
		m.addLog("info", "class 1/2/3 poll requested")
		return m, m.conn.classPoll()

	case "t":
		m.addLog("info", "time sync requested")
		return m, m.conn.syncTime()

	case "c", "o":
		// Operate the selected control point. Close on 'c', open on 'o' —
		// spelled out rather than a generic "toggle", because a control that
		// depends on the operator's idea of the current state is how the wrong
		// breaker gets opened.
		if p, ok := m.selectedControl(); ok {
			closing := key == "c"
			m.addLog("info", fmt.Sprintf("%s binary output %d",
				map[bool]string{true: "closing", false: "opening"}[closing], p.Index))
			return m, m.conn.operate(p.Index, closing)
		}
		m.addLog("warn", "select a binary output point first (Points screen)")

	case "x":
		if m.screen == ScreenEvents {
			m.events = nil
		}
		if m.screen == ScreenLog {
			m.logs = nil
		}
	}
	return m, nil
}

// selectedControl returns the binary output under the cursor.
func (m *Model) selectedControl() (pointKey, bool) {
	if m.screen != ScreenPoints {
		return pointKey{}, false
	}
	rows := m.visiblePoints()
	if m.cursor >= len(rows) {
		return pointKey{}, false
	}
	k := rows[m.cursor].Key
	if k.Type != dnp3.TypeBinaryOutputStatus {
		return pointKey{}, false
	}
	return k, true
}

// applyUpdate folds one measurement into the model.
func (m *Model) applyUpdate(u updateMsg) {
	k := pointKey{Type: u.Type, Index: u.Index}

	p, ok := m.points[k]
	if !ok {
		p = &pointState{Key: k}
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
	p.Updated = time.Now()
	p.IsEvent = u.IsEvent

	if u.IsEvent {
		m.events = append(m.events, eventRow{
			At: time.Now(), Key: k, Value: u.Value, Flags: u.Flags, Stamp: u.Stamp,
		})
		// Bound the list: an event storm would otherwise grow it without
		// limit, and nobody scrolls back ten thousand rows.
		if len(m.events) > 2000 {
			m.events = m.events[len(m.events)-2000:]
		}
	}
}

func (m *Model) addLog(level, text string) {
	m.logs = append(m.logs, logRow{At: time.Now(), Level: level, Text: text})
	if len(m.logs) > 1000 {
		m.logs = m.logs[len(m.logs)-1000:]
	}
}

// visiblePoints returns the point rows after filtering.
func (m *Model) visiblePoints() []*pointState {
	out := make([]*pointState, 0, len(m.pointsOrder))
	for _, k := range m.pointsOrder {
		p := m.points[k]
		if m.filter != "" && !strings.Contains(strings.ToLower(pointLabel(k)), strings.ToLower(m.filter)) {
			continue
		}
		out = append(out, p)
	}
	return out
}

func (m *Model) rowCount() int {
	switch m.screen {
	case ScreenPoints:
		return len(m.visiblePoints())
	case ScreenEvents:
		return len(m.events)
	case ScreenLog:
		return len(m.logs)
	}
	return 0
}

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
	}
	return "??"
}
