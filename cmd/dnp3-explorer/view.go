package main

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/dscsystems/go-dnp3"
)

// The palette is chosen for a control room rather than a code editor: quality
// is the thing an operator scans for, so it gets the colour, and everything
// else stays quiet. Colours are the terminal's own, so the tool inherits
// whatever theme the user already trusts rather than imposing one.
var (
	styleTitle  = lipgloss.NewStyle().Bold(true)
	styleDim    = lipgloss.NewStyle().Faint(true)
	styleHeader = lipgloss.NewStyle().Bold(true).Underline(true)
	styleTabOn  = lipgloss.NewStyle().Bold(true).Reverse(true)
	styleTabOff = lipgloss.NewStyle().Faint(true)
	styleCursor = lipgloss.NewStyle().Reverse(true)

	styleGood  = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	styleWarn  = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	styleBad   = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	styleEvent = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
)

func (m *Model) View() tea.View {
	if m.quitting {
		return tea.NewView("")
	}
	if m.width == 0 {
		return tea.NewView("starting…")
	}
	// A terminal too small to lay out honestly gets told so, rather than a
	// mangled table that looks like corrupted data.
	if m.width < 60 || m.height < 12 {
		return tea.NewView(fmt.Sprintf("terminal is %dx%d; this needs at least 60x12",
			m.width, m.height))
	}

	var b strings.Builder
	b.WriteString(m.viewHeader())
	b.WriteByte('\n')
	b.WriteString(m.viewTabs())
	b.WriteString("\n\n")

	body := m.height - 6 // header, tabs, blank, status
	switch m.screen {
	case ScreenOverview:
		b.WriteString(m.viewOverview())
	case ScreenPoints:
		b.WriteString(m.viewPoints(body))
	case ScreenEvents:
		b.WriteString(m.viewEvents(body))
	case ScreenLog:
		b.WriteString(m.viewLog(body))
	}

	b.WriteByte('\n')
	b.WriteString(m.viewStatus())
	return tea.NewView(b.String())
}

func (m *Model) viewHeader() string {
	state := styleBad.Render("disconnected")
	if m.connected {
		state = styleGood.Render("connected")
	}
	return styleTitle.Render("dnp3-explorer") + "  " +
		styleDim.Render(m.conn.target) + "  " + state
}

func (m *Model) viewTabs() string {
	var parts []string
	for i, name := range screenNames {
		label := fmt.Sprintf(" %d %s ", i+1, name)
		if Screen(i) == m.screen {
			parts = append(parts, styleTabOn.Render(label))
		} else {
			parts = append(parts, styleTabOff.Render(label))
		}
	}
	return strings.Join(parts, "")
}

func (m *Model) viewOverview() string {
	var b strings.Builder

	b.WriteString(styleHeader.Render("Session") + "\n")
	fmt.Fprintf(&b, "  outstation      %s\n", m.conn.target)
	fmt.Fprintf(&b, "  state           %s\n", m.status)
	fmt.Fprintf(&b, "  indications     %s\n", iinStyled(m.iin))
	b.WriteByte('\n')

	st := m.stats
	b.WriteString(styleHeader.Render("Counters") + "\n")
	fmt.Fprintf(&b, "  tasks           %d run, %d ok, %d failed\n",
		st.TasksRun, st.TasksSucceeded, st.TasksFailed)
	fmt.Fprintf(&b, "  timeouts        %d\n", st.ResponseTimeout)
	fmt.Fprintf(&b, "  fragments       %d received, %d unsolicited\n",
		st.FragmentsRx, st.Unsolicited)
	fmt.Fprintf(&b, "  restarts seen   %d\n", st.RestartsSeen)
	fmt.Fprintf(&b, "  connections     %d\n", st.Connections)
	b.WriteByte('\n')

	b.WriteString(styleHeader.Render("Database") + "\n")
	counts := map[dnp3.PointType]int{}
	for k := range m.points {
		counts[k.Type]++
	}
	if len(counts) == 0 {
		b.WriteString(styleDim.Render("  nothing polled yet — press i for an integrity poll") + "\n")
	}
	for _, t := range []dnp3.PointType{
		dnp3.TypeBinary, dnp3.TypeDoubleBitBinary, dnp3.TypeCounter,
		dnp3.TypeFrozenCounter, dnp3.TypeAnalog,
		dnp3.TypeBinaryOutputStatus, dnp3.TypeAnalogOutputStatus,
	} {
		if n := counts[t]; n > 0 {
			fmt.Fprintf(&b, "  %-15s %d\n", t.String(), n)
		}
	}
	fmt.Fprintf(&b, "  events held     %d\n", len(m.events))

	if m.lastErr != "" {
		b.WriteByte('\n')
		b.WriteString(styleBad.Render("  last error: "+m.lastErr) + "\n")
	}
	return b.String()
}

func (m *Model) viewPoints(height int) string {
	rows := m.visiblePoints()
	if len(rows) == 0 {
		if m.filter != "" {
			return styleDim.Render(fmt.Sprintf("  no points match %q", m.filter))
		}
		return styleDim.Render("  nothing polled yet — press i for an integrity poll")
	}

	var b strings.Builder
	b.WriteString(styleHeader.Render(fmt.Sprintf(
		"  %-7s %14s  %-26s %-10s %s", "POINT", "VALUE", "QUALITY", "AGE", "TIMESTAMP")) + "\n")

	start, end := window(m.cursor, len(rows), height-1)
	for i := start; i < end; i++ {
		p := rows[i]
		age := time.Since(p.Updated).Truncate(100 * time.Millisecond).String()
		stamp := "—"
		if p.Time.IsValid() {
			stamp = p.Time.Time.Format("15:04:05.000")
		}

		line := fmt.Sprintf("  %-7s %14s  %-26s %-10s %s",
			pointLabel(p.Key), p.Value, qualityText(p.Flags, p.Key.Type), age, stamp)

		switch {
		case i == m.cursor:
			line = styleCursor.Render(line)
		case !p.Flags.Has(dnp3.Online):
			line = styleBad.Render(line)
		case p.Flags.HasAny(dnp3.Restart | dnp3.CommLost):
			line = styleWarn.Render(line)
		case p.IsEvent:
			line = styleEvent.Render(line)
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

func (m *Model) viewEvents(height int) string {
	if len(m.events) == 0 {
		return styleDim.Render("  no events yet — press p to poll classes 1, 2 and 3")
	}

	var b strings.Builder
	b.WriteString(styleHeader.Render(fmt.Sprintf(
		"  %-12s %-7s %14s  %-24s %s", "RECEIVED", "POINT", "VALUE", "QUALITY", "TIMESTAMP")) + "\n")

	// Following pins the view to the newest row, which is what an operator
	// watching a live system wants; pausing is how they read one that scrolled
	// past.
	cursor := m.cursor
	if m.follow {
		cursor = len(m.events) - 1
	}
	start, end := window(cursor, len(m.events), height-1)

	for i := start; i < end; i++ {
		e := m.events[i]
		stamp := "—"
		if e.Stamp.IsValid() {
			stamp = e.Stamp.Time.Format("15:04:05.000")
		}
		line := fmt.Sprintf("  %-12s %-7s %14s  %-24s %s",
			e.At.Format("15:04:05.000"), pointLabel(e.Key), e.Value,
			qualityText(e.Flags, e.Key.Type), stamp)
		if i == cursor && !m.follow {
			line = styleCursor.Render(line)
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

func (m *Model) viewLog(height int) string {
	if len(m.logs) == 0 {
		return styleDim.Render("  nothing logged yet")
	}

	var b strings.Builder
	cursor := m.cursor
	if m.follow {
		cursor = len(m.logs) - 1
	}
	start, end := window(cursor, len(m.logs), height)

	for i := start; i < end; i++ {
		l := m.logs[i]
		line := fmt.Sprintf("  %-12s %-5s %s", l.At.Format("15:04:05.000"), l.Level, l.Text)
		switch l.Level {
		case "error":
			line = styleBad.Render(line)
		case "warn":
			line = styleWarn.Render(line)
		}
		b.WriteString(line + "\n")
	}
	return b.String()
}

func (m *Model) viewStatus() string {
	if m.filtering {
		return styleTabOn.Render(" filter: " + m.filter + "▏")
	}

	keys := "1-4 screens · i integrity · p poll · t time · c/o close/open · / filter · f follow · q quit"
	if m.follow {
		keys = "follow ON  " + keys
	}
	return styleDim.Render(truncate(keys, m.width))
}

// window returns the slice of rows to draw so the cursor stays visible.
func window(cursor, total, height int) (start, end int) {
	if height < 1 {
		height = 1
	}
	if total <= height {
		return 0, total
	}
	start = cursor - height/2
	start = max(start, 0)
	start = min(start, total-height)
	return start, start + height
}

// qualityText renders the quality flags, with the state bit dropped for binary
// points because the value column already says ON or OFF.
func qualityText(f dnp3.Flags, t dnp3.PointType) string {
	if t == dnp3.TypeBinary || t == dnp3.TypeBinaryOutputStatus {
		f &^= dnp3.StateBit
	}
	s := f.StringFor(t)
	return truncate(s, 26)
}

func iinStyled(s string) string {
	switch {
	case s == "" || s == "—":
		return styleDim.Render("—")
	case strings.Contains(s, "OVERFLOW") || strings.Contains(s, "TROUBLE"):
		return styleBad.Render(s)
	case strings.Contains(s, "RESTART") || strings.Contains(s, "NEED_TIME"):
		return styleWarn.Render(s)
	default:
		return s
	}
}

func truncate(s string, w int) string {
	if w <= 1 || len(s) <= w {
		return s
	}
	return s[:w-1] + "…"
}
