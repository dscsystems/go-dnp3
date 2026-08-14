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
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/dscsystems/go-dnp3"
)

// The screen is drawn as a fixed frame: a header, a tab bar, a body that takes
// whatever is left, and a footer that is always in the same place. Nothing
// reflows as data arrives, because an operator reaching for a control should
// not have to find it again every time a value updates.

func (m *Model) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = m.altmode
	if m.mouse {
		// All-motion reporting is what makes tabs and buttons light up under
		// the pointer. Cell-motion would be enough for dragging the scrollbar,
		// but a control surface that does not acknowledge the pointer feels
		// broken, and the extra traffic costs a diffed repaint.
		v.MouseMode = tea.MouseModeAllMotion
	}
	v.WindowTitle = "dnp3-explorer — " + m.conn.target()
	return v
}

func (m *Model) render() string {
	if m.quitting {
		return ""
	}
	if m.width == 0 {
		return "starting…"
	}

	l := m.layout()
	if !l.ok {
		// A terminal too small to lay out honestly gets told so, rather than a
		// mangled table that looks like corrupted data.
		return fmt.Sprintf("terminal is %dx%d; this needs at least %dx%d",
			m.width, m.height, minWidth, minHeight)
	}

	lines := make([]string, 0, m.height)
	lines = append(lines, m.viewHeader(l))
	lines = append(lines, m.viewTabs(l))
	lines = append(lines, stMuted.Render(repeat("─", m.width)))
	lines = append(lines, m.viewBody(l)...)
	lines = append(lines, stMuted.Render(repeat("─", m.width)))
	lines = append(lines, m.viewToolbar(l))
	lines = append(lines, m.viewHint(l))

	return strings.Join(lines, "\n")
}

// ---------- frame ----------

func (m *Model) viewHeader(l layout) string {
	state := stBad.Render("○ disconnected")
	if m.connected {
		state = stGood.Render("● connected")
	}

	left := stTitle.Render("dnp3-explorer") + "  " +
		stMuted.Render(m.conn.target()) + "  " + state

	// The right-hand side gives up its least important part first, so a narrow
	// terminal loses the uptime rather than the clock.
	clock := m.clock().Format("15:04:05")
	candidates := []string{clock, m.rateText() + "  " + clock}
	if !m.linkSince.IsZero() {
		candidates = append(candidates,
			"up "+fmtDuration(m.clock().Sub(m.linkSince))+"  "+m.rateText()+"  "+clock)
	}

	for i := len(candidates) - 1; i >= 0; i-- {
		gap := m.width - lipgloss.Width(left) - lipgloss.Width(candidates[i]) - 1
		if gap >= 1 {
			return left + repeat(" ", gap+1) + stMuted.Render(candidates[i])
		}
	}
	return fit(left, m.width)
}

func tabLabel(i int, name string) string {
	return fmt.Sprintf(" %d %s ", i+1, name)
}

func (m *Model) viewTabs(l layout) string {
	var b strings.Builder
	for i, name := range screenNames {
		label := tabLabel(i, name)
		switch {
		case Screen(i) == m.screen:
			b.WriteString(stTabOn.Render(label))
		case m.hover.kind == zoneTab && m.hover.n == i:
			b.WriteString(stKey.Render(label))
		default:
			b.WriteString(stTabOff.Render(label))
		}
	}

	// The right of the tab bar is where the view's own state lives: what is
	// being filtered out, and how much of the list is on screen. Without it a
	// filtered table is indistinguishable from a device that stopped talking.
	var status []string
	if m.filter != "" {
		status = append(status, "filter "+strconv.Quote(m.filter))
	}
	if m.screen.isTable() {
		status = append(status, plural(l.total, rowNoun(m.screen)))
	}
	if m.screen.follows() && m.follow {
		status = append(status, "following")
	}
	if len(status) == 0 {
		return fit(b.String(), m.width)
	}

	right := stMuted.Render(strings.Join(status, " · ") + " ")
	gap := m.width - lipgloss.Width(b.String()) - lipgloss.Width(right)
	if gap < 1 {
		return fit(b.String(), m.width)
	}
	return b.String() + repeat(" ", gap) + right
}

func (m *Model) viewBody(l layout) []string {
	if m.form.active {
		return clip(m.viewForm(l), l.body.h, l.body.w)
	}
	if m.modal.kind != modalNone {
		return clip(m.viewModal(l), l.body.h, l.body.w)
	}

	var body []string
	switch m.screen {
	case ScreenOverview:
		body = m.viewOverview(l.body)
	case ScreenPoints:
		body = m.viewPoints(l)
	case ScreenEvents:
		body = m.viewEvents(l)
	case ScreenLog:
		body = m.viewLog(l)
	case ScreenHelp:
		body = m.viewHelp(l)
	}

	if l.detail.empty() {
		return clip(body, l.body.h, l.body.w)
	}
	// The inspector is a second column of the body, joined row by row so that
	// neither side can push the other out of the frame.
	return joinColumns([][]string{
		clip(body, l.body.h, l.table.w),
		box("Inspector", l.detail.w, l.detail.h, m.viewDetail(l.detail.w-4)),
	}, l.body.h)
}

// viewToolbar draws the clickable actions, and the standing warning that
// controls are not being confirmed.
//
// That warning has no key and cannot be dismissed. Running with -no-confirm
// means the next c or o goes to the plant with nothing in between, and the one
// moment an operator needs to be told that is the moment they have stopped
// expecting a dialog to appear.
func (m *Model) viewToolbar(l layout) string {
	var b strings.Builder
	b.WriteByte(' ')
	for i, btn := range l.buttons {
		if i > 0 {
			b.WriteByte(' ')
		}
		hovered := m.hover.kind == zoneButton && m.hover.n == i
		b.WriteString(renderButton(btn, hovered))
	}
	if m.confirm {
		return fit(b.String(), m.width)
	}

	warn := stWarn.Render(noConfirmWarning)
	gap := m.width - lipgloss.Width(b.String()) - lipgloss.Width(warn)
	if gap < 1 {
		return fit(b.String(), m.width)
	}
	return b.String() + repeat(" ", gap) + warn
}

func buttonLabel(b button) string { return "[" + b.key + " " + b.label + "]" }

func renderButton(b button, hovered bool) string {
	key, label := stKey.Render(b.key), b.label
	switch {
	case hovered:
		return stSel.Render(buttonLabel(b))
	case b.on:
		// An engaged mode is drawn as engaged, so the toolbar reports state
		// rather than only offering actions.
		return stMuted.Render("[") + key + " " + stGood.Render(label) + stMuted.Render("]")
	}
	return stMuted.Render("[") + key + " " + label + stMuted.Render("]")
}

// footerButtons is the action set for the current screen.
func (m *Model) footerButtons() []button {
	if m.form.active {
		return []button{
			{label: "Connect", key: "enter"},
			{label: "Cancel", key: "esc"},
		}
	}
	if m.modal.kind != modalNone {
		return []button{{label: "Cancel", key: "esc"}}
	}
	switch m.screen {
	case ScreenPoints:
		return []button{
			{label: "Integrity", key: "i"},
			{label: "Poll", key: "p"},
			{label: "Filter", key: "/"},
			{label: "Inspect", key: "d", on: m.detail},
			{label: "Close", key: "c"},
			{label: "Open", key: "o"},
			{label: sboLabel(m.sbo), key: "S"},
			{label: "Export", key: "e"},
			{label: "Help", key: "?"},
		}
	case ScreenEvents:
		return []button{
			{label: "Poll", key: "p"},
			{label: "Follow", key: "f", on: m.follow},
			{label: "Filter", key: "/"},
			{label: "Clear", key: "x"},
			{label: "Export", key: "e"},
			{label: "Help", key: "?"},
		}
	case ScreenLog:
		return []button{
			{label: "Follow", key: "f", on: m.follow},
			{label: "Filter", key: "/"},
			{label: "Clear", key: "x"},
			{label: "Export", key: "e"},
			{label: "Help", key: "?"},
		}
	case ScreenHelp:
		return []button{
			{label: "Points", key: "2"},
			{label: "Quit", key: "q"},
		}
	default:
		return []button{
			{label: "Integrity", key: "i"},
			{label: "Poll", key: "p"},
			{label: "Set clock", key: "t"},
			{label: "Unsol on", key: "u"},
			{label: "Restart", key: "R"},
			{label: "Connection", key: "C"},
			{label: "Help", key: "?"},
			{label: "Quit", key: "q"},
		}
	}
}

// confirmText says whether anything stands between the keystroke and the
// plant. It gets a row of its own rather than a clause appended to the control
// mode, because in a narrow column that clause is the half that gets truncated.
func (m *Model) confirmText() string {
	if m.confirm {
		return "asks first"
	}
	return stWarn.Render("none — sends immediately")
}

func sboLabel(sbo bool) string {
	if sbo {
		return "SBO"
	}
	return "Direct"
}

// viewHint is the bottom line: a prompt while one is open, then whatever the
// last action had to say, then the keys for this screen.
func (m *Model) viewHint(l layout) string {
	if m.prompt.active {
		return fit(stTabOn.Render(" "+m.prompt.label+" › "+m.prompt.input+"▏"), m.width)
	}
	if m.form.active {
		return fit(stMuted.Render(
			" ↑↓ or tab move between fields · enter connects · esc cancels"), m.width)
	}
	if m.modal.kind != modalNone {
		return fit(stMuted.Render(" press a key from the list, click a line, or esc to cancel"),
			m.width)
	}
	if m.toast.active() {
		return fit(" "+levelStyle(m.toast.level).Render(m.toast.text), m.width)
	}

	var hint string
	switch m.screen {
	case ScreenPoints:
		hint = "↑↓ move · enter act · d inspect · / filter · < > r sort · b deadband · s range scan · ? help"
	case ScreenEvents:
		hint = "↑↓ move · f follow · x clear · / filter · p poll · ? help"
	case ScreenLog:
		hint = "↑↓ move · f follow · x clear · / filter · ? help"
	case ScreenHelp:
		hint = "↑↓ scroll · tab or 1-5 change screen · q quit"
	default:
		hint = "i integrity · p poll · t clock · u/U unsolicited · R restart · click anything · ? help"
	}
	return fit(stMuted.Render(" "+hint), m.width)
}

// ---------- overview ----------

type panelSpec struct {
	title string
	lines []string
}

func (m *Model) viewOverview(b rect) []string {
	session := panelSpec{"Session", m.overviewSession()}
	traffic := panelSpec{"Traffic", m.overviewTraffic()}
	database := panelSpec{"Database", m.overviewDatabase()}
	activity := panelSpec{"Recent activity", m.overviewActivity(b.h/2 - 2)}

	// Two columns when the terminal can hold them without squeezing the
	// numbers; one when it cannot.
	if b.w >= 92 {
		colW := (b.w - 1) / 2
		left := stackPanels([]panelSpec{session, traffic}, colW, b.h)
		right := stackPanels([]panelSpec{database, activity}, b.w-1-colW, b.h)
		return joinColumns([][]string{left, right}, b.h)
	}
	return stackPanels([]panelSpec{session, database, traffic, activity}, b.w, b.h)
}

// stackPanels gives each panel its natural height and lets the last one absorb
// whatever is left over, so the column always fills the body exactly.
func stackPanels(panels []panelSpec, w, h int) []string {
	if len(panels) == 0 {
		return nil
	}
	heights := make([]int, len(panels))
	total := 0
	for i, p := range panels {
		heights[i] = len(p.lines) + 2
		total += heights[i]
	}

	// Shrink from the last panel backwards when there is not enough room, so
	// the first panel — the one that says whether the link is up — survives.
	for i := len(panels) - 1; i >= 0 && total > h; i-- {
		shrink := min(total-h, max(heights[i]-3, 0))
		heights[i] -= shrink
		total -= shrink
	}
	if total < h {
		heights[len(panels)-1] += h - total
	}

	out := make([]string, 0, h)
	for i, p := range panels {
		if heights[i] < 3 {
			continue
		}
		out = append(out, box(p.title, w, heights[i], p.lines)...)
	}
	return clip(out, h, w)
}

// field renders one "name   value" row of a panel.
func field(name, value string) string {
	return stMuted.Render(cell(name, 16, false)) + value
}

// typeLabel names a point type the way an engineer says it, rather than the
// way the Go type is spelled: a panel column is not wide enough for
// "BinaryOutputStatus" and truncating it loses the word that matters.
func typeLabel(t dnp3.PointType) string {
	switch t {
	case dnp3.TypeBinary:
		return "binary inputs"
	case dnp3.TypeDoubleBitBinary:
		return "double-bit"
	case dnp3.TypeCounter:
		return "counters"
	case dnp3.TypeFrozenCounter:
		return "frozen ctrs"
	case dnp3.TypeAnalog:
		return "analog inputs"
	case dnp3.TypeBinaryOutputStatus:
		return "binary outputs"
	case dnp3.TypeAnalogOutputStatus:
		return "analog outputs"
	case dnp3.TypeOctetString:
		return "octet strings"
	}
	return t.String()
}

func (m *Model) overviewSession() []string {
	state := stBad.Render(m.status)
	if m.connected {
		state = stGood.Render(m.status)
	}
	up := "—"
	if !m.linkSince.IsZero() {
		up = fmtDuration(m.clock().Sub(m.linkSince))
	}

	lines := []string{
		field("outstation", m.conn.target()),
		field("transport", m.conn.transport()),
		field("state", state),
		field("link up for", up),
		field("indications", iinStyled(m.iin)),
		field("controls", controlMode(m.sbo)),
		field("confirmation", m.confirmText()),
	}
	if m.lastErr != "" {
		lines = append(lines, field("last error", stBad.Render(m.lastErr)))
	}
	return lines
}

func (m *Model) overviewTraffic() []string {
	st := m.stats
	lines := []string{
		field("tasks", fmt.Sprintf("%d run · %s · %s",
			st.TasksRun,
			stGood.Render(fmt.Sprintf("%d ok", st.TasksSucceeded)),
			failedText(st.TasksFailed))),
		field("timeouts", countText(st.ResponseTimeout)),
		field("fragments", fmt.Sprintf("%d received · %d unsolicited",
			st.FragmentsRx, st.Unsolicited)),
		field("restarts seen", countText(st.RestartsSeen)),
		field("connections", fmt.Sprint(st.Connections)),
		field("update rate", m.rateText()),
	}
	if dropped := m.conn.dropped.Load(); dropped > 0 {
		lines = append(lines, field("dropped",
			stWarn.Render(fmt.Sprintf("%d (UI fell behind)", dropped))))
	}
	if len(m.rateHist) > 1 {
		lines = append(lines, "", stMuted.Render("measurements per second, last minute"),
			stMuted.Render(sparkline(m.rateHist, 60)))
	}
	return lines
}

func (m *Model) overviewDatabase() []string {
	if len(m.points) == 0 {
		// Split over two lines so it survives the narrow column: an empty tool
		// that cannot fit the sentence telling you how to fill it is an empty
		// tool that looks broken.
		return []string{
			stMuted.Render("nothing polled yet"),
			stMuted.Render("press i for an integrity poll"),
		}
	}

	counts := map[dnp3.PointType]int{}
	var bad, forced, stale int
	now := m.clock()
	for k, p := range m.points {
		counts[k.Type]++
		switch {
		case !p.Flags.Has(dnp3.Online) || p.Flags.HasAny(dnp3.CommLost):
			bad++
		case p.Flags.HasAny(dnp3.RemoteForced | dnp3.LocalForced):
			forced++
		}
		if p.stale(now, m.staleAge) {
			stale++
		}
	}

	var lines []string
	for _, t := range []dnp3.PointType{
		dnp3.TypeBinary, dnp3.TypeDoubleBitBinary, dnp3.TypeCounter,
		dnp3.TypeFrozenCounter, dnp3.TypeAnalog,
		dnp3.TypeBinaryOutputStatus, dnp3.TypeAnalogOutputStatus,
		dnp3.TypeOctetString,
	} {
		if n := counts[t]; n > 0 {
			lines = append(lines, field(typeLabel(t), fmt.Sprint(n)))
		}
	}

	health := stGood.Render(fmt.Sprintf("%d good", len(m.points)-bad-forced))
	if bad > 0 {
		health += " · " + stBad.Render(fmt.Sprintf("%d bad", bad))
	}
	if forced > 0 {
		health += " · " + stWarn.Render(fmt.Sprintf("%d forced", forced))
	}
	if stale > 0 {
		health += " · " + stMuted.Render(fmt.Sprintf("%d stale", stale))
	}
	lines = append(lines,
		field("quality", health),
		field("events held", fmt.Sprint(len(m.events))))
	return lines
}

// overviewActivity shows the newest events, so the first screen answers "is
// anything happening" without changing tabs.
func (m *Model) overviewActivity(n int) []string {
	n = max(n, 1)
	if len(m.events) == 0 {
		return []string{stMuted.Render("no events yet — press p to poll classes 1, 2 and 3")}
	}
	start := max(len(m.events)-n, 0)
	lines := make([]string, 0, n)
	for i := len(m.events) - 1; i >= start; i-- {
		e := m.events[i]
		lines = append(lines, fmt.Sprintf("%s  %s  %s",
			stMuted.Render(e.At.Format("15:04:05.000")),
			cell(pointLabel(e.Key), 7, false),
			e.Value))
	}
	return lines
}

func failedText(n uint64) string {
	s := fmt.Sprintf("%d failed", n)
	if n > 0 {
		return stBad.Render(s)
	}
	return s
}

func countText(n uint64) string {
	if n > 0 {
		return stWarn.Render(fmt.Sprint(n))
	}
	return fmt.Sprint(n)
}

func (m *Model) rateText() string {
	r := m.eventRate()
	if r == 0 {
		return "0/s"
	}
	return fmt.Sprintf("%.1f/s", r)
}

// ---------- tables ----------

// tableRow is one drawn row: cells keyed by which column they belong to, with
// optional per-cell colour and an optional style for the whole line.
type tableRow struct {
	cells map[colID]string
	// cellStyle colours individual cells; a missing entry leaves them plain.
	cellStyle map[colID]lipgloss.Style
	// line, when set, styles the whole row and overrides the cell colours,
	// which is what a selected or failed row needs: a row that is half
	// reversed and half red is unreadable.
	line    lipgloss.Style
	lineSet bool
}

// renderTable draws the column header and the visible slice of rows.
func (m *Model) renderTable(l layout, empty string, row func(i int) tableRow) []string {
	out := make([]string, 0, l.table.h)
	out = append(out, m.renderColumnHeader(l))

	if l.total == 0 {
		out = append(out, "")
		out = append(out, "  "+stMuted.Render(empty))
		return out
	}

	cursor := m.cursor[m.screen]
	for i := range l.rows.h {
		idx := l.offset + i
		var line string
		if idx < l.total {
			line = renderRow(l.cols, row(idx), idx == cursor)
		}
		if !l.scroll.empty() {
			bar := scrollbarRune(i, l.rows.h, l.offset, l.total)
			line = fit(" "+line, l.table.w-1) + stMuted.Render(bar)
		} else {
			line = " " + line
		}
		out = append(out, line)
	}
	return out
}

func (m *Model) renderColumnHeader(l layout) string {
	var b strings.Builder
	b.WriteByte(' ')
	for i, c := range l.cols {
		if i > 0 {
			b.WriteByte(' ')
		}
		title := c.title
		if c.key != sortNone && c.key == m.sortBy && m.screen == ScreenPoints {
			if m.sortDesc {
				title += " ▼"
			} else {
				title += " ▲"
			}
		}
		text := cell(title, c.width, c.right)
		switch {
		case m.hover.kind == zoneColumn && m.hover.n == i && c.key != sortNone:
			b.WriteString(stSel.Render(text))
		default:
			b.WriteString(stColHead.Render(text))
		}
	}
	return fit(b.String(), l.table.w)
}

func renderRow(cols []column, r tableRow, selected bool) string {
	var b strings.Builder
	for i, c := range cols {
		if i > 0 {
			b.WriteByte(' ')
		}
		text := cell(r.cells[c.id], c.width, c.right)
		if !selected && !r.lineSet {
			if st, ok := r.cellStyle[c.id]; ok {
				text = st.Render(text)
			}
		}
		b.WriteString(text)
	}
	switch {
	case selected:
		return stSel.Render(b.String())
	case r.lineSet:
		return r.line.Render(b.String())
	}
	return b.String()
}

// columnsFor is the column set for a screen, before widths are resolved.
func columnsFor(s Screen) []column {
	switch s {
	case ScreenPoints:
		return []column{
			{id: colPoint, title: "POINT", key: sortPoint, width: 7},
			{id: colValue, title: "VALUE", key: sortValue, width: 14, right: true},
			{id: colTrend, title: "TREND", width: 12, prio: 3},
			{id: colQuality, title: "QUALITY", key: sortQuality, min: 12, flex: true},
			{id: colAge, title: "AGE", key: sortAge, width: 7, right: true, prio: 2},
			{id: colStamp, title: "TIMESTAMP", key: sortTime, width: 12, prio: 1},
		}
	case ScreenEvents:
		return []column{
			{id: colReceived, title: "RECEIVED", width: 12},
			{id: colPoint, title: "POINT", width: 7},
			{id: colValue, title: "VALUE", width: 14, right: true},
			{id: colClass, title: "CL", width: 2, prio: 3},
			{id: colQuality, title: "QUALITY", min: 10, flex: true},
			{id: colSource, title: "SOURCE", width: 7, prio: 4},
			{id: colStamp, title: "TIMESTAMP", width: 12, prio: 1},
		}
	default:
		return []column{
			{id: colReceived, title: "TIME", width: 12},
			{id: colLevel, title: "LEVEL", width: 5},
			{id: colMessage, title: "MESSAGE", min: 20, flex: true},
		}
	}
}

func (m *Model) viewPoints(l layout) []string {
	rows := m.visiblePoints()
	empty := "nothing polled yet — press i for an integrity poll"
	if m.filter != "" {
		empty = fmt.Sprintf("no points match %q — press esc to clear the filter", m.filter)
	}
	now := m.clock()

	return m.renderTable(l, empty, func(i int) tableRow {
		p := rows[i]
		stamp := "—"
		if p.Time.IsValid() {
			stamp = p.Time.Time.Format("15:04:05.000")
		}
		trend := ""
		if len(p.Hist) > 1 {
			trend = sparkline(p.Hist, 12)
		}

		r := tableRow{
			cells: map[colID]string{
				colPoint:   pointLabel(p.Key),
				colValue:   p.Value,
				colTrend:   trend,
				colQuality: qualityText(p.Flags, p.Key.Type),
				colAge:     fmtAge(now.Sub(p.Updated)),
				colStamp:   stamp,
			},
			cellStyle: map[colID]lipgloss.Style{colTrend: stMuted, colStamp: stMuted},
		}

		switch {
		case !p.Flags.Has(dnp3.Online):
			r.line, r.lineSet = stBad, true
		case p.Flags.HasAny(dnp3.Restart | dnp3.CommLost):
			r.line, r.lineSet = stWarn, true
		case p.stale(now, m.staleAge):
			// Nothing is wrong with the value; it is just old, and the row
			// says so by fading rather than by shouting.
			r.line, r.lineSet = stStale, true
		case p.IsEvent && now.Sub(p.Updated) < time.Second:
			r.line, r.lineSet = stEvent, true
		}
		return r
	})
}

func (m *Model) viewEvents(l layout) []string {
	rows := m.visibleEvents()
	empty := "no events yet — press p to poll classes 1, 2 and 3"
	if m.filter != "" {
		empty = fmt.Sprintf("no events match %q", m.filter)
	}

	return m.renderTable(l, empty, func(i int) tableRow {
		e := rows[i]
		stamp := "—"
		if e.Stamp.IsValid() {
			stamp = e.Stamp.Time.Format("15:04:05.000")
		}
		r := tableRow{
			cells: map[colID]string{
				colReceived: e.At.Format("15:04:05.000"),
				colPoint:    pointLabel(e.Key),
				colValue:    e.Value,
				colClass:    classText(e.Class),
				colQuality:  qualityText(e.Flags, e.Key.Type),
				colSource:   e.GV.String(),
				colStamp:    stamp,
			},
			cellStyle: map[colID]lipgloss.Style{
				colReceived: stMuted, colClass: stMuted,
				colSource: stMuted, colStamp: stMuted,
			},
		}
		if !e.Flags.Has(dnp3.Online) {
			r.line, r.lineSet = stBad, true
		}
		return r
	})
}

func (m *Model) viewLog(l layout) []string {
	rows := m.visibleLogs()
	empty := "nothing logged yet"
	if m.filter != "" {
		empty = fmt.Sprintf("no log lines match %q", m.filter)
	}

	return m.renderTable(l, empty, func(i int) tableRow {
		e := rows[i]
		r := tableRow{
			cells: map[colID]string{
				colReceived: e.At.Format("15:04:05.000"),
				colLevel:    e.Level,
				colMessage:  e.Text,
			},
			cellStyle: map[colID]lipgloss.Style{colReceived: stMuted},
		}
		switch e.Level {
		case "error":
			r.line, r.lineSet = stBad, true
		case "warn":
			r.line, r.lineSet = stWarn, true
		case "ok":
			r.cellStyle[colLevel] = stGood
		}
		return r
	})
}

func classText(c dnp3.Class) string {
	if c == 0 {
		return "—"
	}
	return c.String()
}

// ---------- inspector ----------

// viewDetail is the point inspector: everything known about one point,
// including the things a table has no room for.
func (m *Model) viewDetail(w int) []string {
	p, ok := m.selectedPoint()
	if !ok {
		return []string{
			stMuted.Render("no point selected"),
			"",
			stMuted.Render("Move the cursor with ↑↓ or click"),
			stMuted.Render("a row. Right-click opens this"),
			stMuted.Render("panel from anywhere."),
		}
	}
	now := m.clock()

	lines := []string{
		stTitle.Render(pointLabel(p.Key)) + stMuted.Render("  "+p.Key.Type.String()),
		"",
		stBold.Render(truncate(p.Value, w)),
	}
	if p.Previous != "" && p.Previous != p.Value {
		lines = append(lines, stMuted.Render("was "+truncate(p.Previous, w-4)))
	}

	if len(p.Hist) > 1 {
		lo, hi := p.Hist[0], p.Hist[0]
		for _, v := range p.Hist {
			lo, hi = min(lo, v), max(hi, v)
		}
		lines = append(lines,
			"",
			stMuted.Render(sparkline(p.Hist, w)),
			stMuted.Render(fmt.Sprintf("%s … %s over %d samples",
				formatFloat(lo), formatFloat(hi), len(p.Hist))))
	}

	stamp := "—"
	if p.Time.IsValid() {
		stamp = p.Time.Time.Format("15:04:05.000") + " " + short(p.Time.Quality.String())
	}
	lines = append(lines,
		"",
		detailField("age", fmtAge(now.Sub(p.Updated))),
		detailField("stamp", stamp),
		detailField("source", p.GV.String()),
		detailField("class", classText(p.Class)),
		detailField("updates", fmt.Sprintf("%d (%d events)", p.Updates, p.Events)),
		detailField("first seen", p.First.Format("15:04:05")),
		"",
		stMuted.Render("QUALITY"),
	)
	lines = append(lines, qualityLines(p.Flags, p.Key.Type)...)

	if actions := detailActions(p.Key.Type); len(actions) > 0 {
		lines = append(lines, "", stMuted.Render("ACTIONS"))
		lines = append(lines, actions...)
	}
	return lines
}

func detailField(name, value string) string {
	return stMuted.Render(cell(name, 11, false)) + value
}

// qualityLines lists the flags one to a line, coloured by what they mean, so
// the inspector answers "why is this row red".
func qualityLines(f dnp3.Flags, t dnp3.PointType) []string {
	if t == dnp3.TypeBinary || t == dnp3.TypeBinaryOutputStatus {
		f &^= dnp3.StateBit
	}
	if f == 0 {
		return []string{stBad.Render("  no flags at all")}
	}
	var out []string
	for _, name := range strings.Split(f.StringFor(t), "|") {
		switch name {
		case "ONLINE":
			out = append(out, stGood.Render("  ✓ "+name))
		case "RESTART", "COMM_LOST":
			out = append(out, stBad.Render("  ✗ "+name))
		case "—":
		default:
			out = append(out, stWarn.Render("  ! "+name))
		}
	}
	if !f.Has(dnp3.Online) {
		out = append([]string{stBad.Render("  ✗ NOT ONLINE")}, out...)
	}
	return out
}

func detailActions(t dnp3.PointType) []string {
	switch t {
	case dnp3.TypeBinaryOutputStatus:
		return []string{
			"  " + stKey.Render("c") + " close   " + stKey.Render("o") + " open",
			"  " + stKey.Render("enter") + " control dialog",
		}
	case dnp3.TypeAnalogOutputStatus:
		return []string{"  " + stKey.Render("enter") + " write a setpoint"}
	case dnp3.TypeAnalog:
		return []string{"  " + stKey.Render("b") + " write a deadband"}
	}
	return nil
}

// ---------- dialogs ----------

func (m *Model) viewModal(l layout) []string {
	d := m.modal
	content := make([]string, 0, l.modal.h-2)
	content = append(content, d.lines...)
	// Pad so the choices sit flush against the bottom of the box, where the
	// hit test expects to find them.
	for len(content) < l.modal.h-2-len(d.choices) {
		content = append(content, "")
	}
	for i, c := range d.choices {
		marker := "  "
		if m.hover.kind == zoneChoice && m.hover.n == i {
			marker = stKey.Render("▸ ")
		}
		content = append(content, marker+stKey.Render(cell(c.key, 5, false))+c.label)
	}

	title := d.title
	drawn := box(title, l.modal.w, l.modal.h, content)

	// The dialog is centred on an otherwise empty body: splicing a box into
	// live table rows means measuring around escape sequences, and getting
	// that wrong on the screen that operates plant is not a trade worth making.
	out := make([]string, l.body.h)
	for i := range out {
		out[i] = repeat(" ", l.body.w)
	}
	for i, line := range drawn {
		y := l.modal.y - l.body.y + i
		if y >= 0 && y < len(out) {
			out[y] = repeat(" ", l.modal.x-l.body.x) + line
		}
	}
	return out
}

// ---------- connection editor ----------

// viewForm draws the connection editor.
//
// The focused field is drawn with a cursor in it and its explanation beneath,
// so the operator is told what a field means at the moment they are typing
// into it rather than having to remember from a manual.
func (m *Model) viewForm(l layout) []string {
	f := m.form
	inner := l.form.w - 4

	content := make([]string, 0, l.form.h)
	for i := range l.formRows {
		idx := l.formFirst + i
		if idx >= len(f.fields) {
			break
		}
		fld := f.fields[idx]
		focused := idx == f.cursor

		name := cell(fld.label, inner, false)
		if focused {
			content = append(content, stKey.Render(name))
		} else {
			content = append(content, stMuted.Render(name))
		}

		value := fld.value
		if focused {
			value += "▏"
		}
		entry := "  " + cell(value, max(inner-2, 1), false)
		if focused {
			entry = stSel.Render(entry)
		}
		content = append(content, entry)
	}

	// The footer says what is wrong if anything is, and otherwise explains the
	// field being edited.
	footer := ""
	if fld := f.fields[min(f.cursor, len(f.fields)-1)]; fld.hint != "" {
		footer = stMuted.Render(truncate(fld.hint, inner))
	}
	if f.err != "" {
		footer = stBad.Render(truncate(f.err, inner))
	}
	content = append(content, "", footer)

	drawn := box(f.title, l.form.w, l.form.h, content)

	out := make([]string, l.body.h)
	for i := range out {
		out[i] = repeat(" ", l.body.w)
	}
	for i, line := range drawn {
		if y := l.form.y - l.body.y + i; y >= 0 && y < len(out) {
			out[y] = repeat(" ", l.form.x-l.body.x) + line
		}
	}
	return out
}

// ---------- help ----------

// viewHelp draws the reference, scrolled to wherever the operator has got to.
func (m *Model) viewHelp(l layout) []string {
	lines := m.helpLines(l.body)
	if l.offset >= len(lines) {
		return lines
	}
	return lines[l.offset:]
}

// helpLines composes the whole reference, in two columns when there is room.
//
// It is composed rather than drawn so the screen can be scrolled: on a
// terminal shorter than the reference, the alternative is a reference whose
// last third does not exist.
func (m *Model) helpLines(b rect) []string {
	keys := [][2]string{
		{"1 – 5, tab", "change screen"},
		{"↑ ↓ j k", "move the cursor"},
		{"pgup pgdn", "move a page"},
		{"home end g G", "first and last row"},
		{"/", "filter the list"},
		{"esc", "clear the filter, close a dialog"},
		{"f", "follow the newest row"},
		{"d, enter", "inspector, or act on the row"},
		{"< >", "change the sort column"},
		{"r", "reverse the sort"},
		{"x", "clear this list"},
		{"e", "export the list as CSV"},
		{"q, ctrl+c", "quit"},
	}
	proto := [][2]string{
		{"i", "integrity poll (classes 0–3)"},
		{"p", "poll event classes 1, 2, 3"},
		{"s", "range scan a group"},
		{"t", "set the outstation clock"},
		{"T", "set it measuring the link delay"},
		{"u / U", "enable / disable unsolicited"},
		{"R", "restart the outstation"},
		{"c / o", "close / open the selected output"},
		{"enter", "control dialog, or write a setpoint"},
		{"b", "write an analog deadband"},
		{"S", "select-before-operate or direct"},
		{"C", "change the connection"},
	}
	mouse := [][2]string{
		{"click a tab", "change screen"},
		{"click a row", "select it"},
		{"click it again", "act on it"},
		{"right-click a row", "open the inspector"},
		{"click a heading", "sort by that column"},
		{"click it again", "reverse the sort"},
		{"wheel", "scroll the list"},
		{"wheel on the tabs", "change screen"},
		{"drag the scrollbar", "scroll the list"},
		{"click a button", "run that action"},
	}

	about := []string{
		"", stColHead.Render("CONNECTION"), "",
		" " + stMuted.Render("C opens the connection editor:"),
		" " + stMuted.Render("the address, the two link addresses,"),
		" " + stMuted.Render("the timeout and the poll interval."),
		" " + stMuted.Render("Applying it reconnects in place, so a"),
		" " + stMuted.Render("guessed link address can be corrected"),
		" " + stMuted.Render("without restarting the tool."),
		"", stColHead.Render("ABOUT"), "",
		" " + stMuted.Render("dnp3-explorer, part of go-dnp3:"),
		" " + stMuted.Render("an IEEE 1815-2012 master and"),
		" " + stMuted.Render("outstation stack in Go. GPLv3."),
		"",
		" " + stMuted.Render("Controls ask before they operate"),
		" " + stMuted.Render("unless started with -no-confirm."),
	}

	render := func(title string, rows [][2]string, w int) []string {
		out := make([]string, 0, len(rows)+2)
		out = append(out, stColHead.Render(title), "")
		for _, r := range rows {
			out = append(out, " "+stKey.Render(cell(r[0], 18, false))+
				truncate(r[1], max(w-19, 4)))
		}
		return out
	}

	if b.w >= 96 {
		colW := (b.w - 1) / 2
		left := append(render("KEYS", keys, colW), about...)
		right := append(render("PROTOCOL", proto, b.w-colW-1), "")
		right = append(right, render("MOUSE", mouse, b.w-colW-1)...)

		h := max(len(left), len(right))
		return joinColumns([][]string{clip(left, h, colW), clip(right, h, b.w-colW-1)}, h)
	}

	out := render("KEYS", keys, b.w)
	out = append(out, "")
	out = append(out, render("PROTOCOL", proto, b.w)...)
	out = append(out, "")
	out = append(out, render("MOUSE", mouse, b.w)...)
	out = append(out, about...)
	return out
}

// ---------- small renderers ----------

// qualityText renders the quality flags, with the state bit dropped for binary
// points because the value column already says ON or OFF.
func qualityText(f dnp3.Flags, t dnp3.PointType) string {
	if t == dnp3.TypeBinary || t == dnp3.TypeBinaryOutputStatus {
		f &^= dnp3.StateBit
	}
	return f.StringFor(t)
}

func iinStyled(s string) string {
	switch {
	case s == "" || s == "—":
		return stMuted.Render("—")
	case strings.Contains(s, "OVERFLOW") || strings.Contains(s, "TROUBLE"):
		return stBad.Render(s)
	case strings.Contains(s, "RESTART") || strings.Contains(s, "NEED_TIME"):
		return stWarn.Render(s)
	default:
		return s
	}
}

// clock returns the model's idea of now, which the tick keeps current. Reading
// it from one place keeps every age on a repainted screen consistent with
// every other.
func (m *Model) clock() time.Time {
	if m.now.IsZero() {
		return time.Now()
	}
	return m.now
}

// fmtAge renders how long ago something happened, in as few characters as it
// can be said honestly.
func fmtAge(d time.Duration) string {
	switch {
	case d < 0:
		return "0s"
	case d < 10*time.Second:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

func fmtDuration(d time.Duration) string {
	d = d.Truncate(time.Second)
	h := int(d.Hours())
	mn := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, mn, s)
	}
	return fmt.Sprintf("%d:%02d", mn, s)
}

// rowNoun names what the current screen is a list of, because "1 rows" is the
// sort of thing that makes an operator distrust the rest of the numbers.
func rowNoun(s Screen) string {
	switch s {
	case ScreenPoints:
		return "point"
	case ScreenEvents:
		return "event"
	default:
		return "line"
	}
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func short(s string) string {
	if len(s) > 4 {
		return s[:4]
	}
	return s
}
