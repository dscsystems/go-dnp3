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
	"strings"

	"charm.land/lipgloss/v2"
)

// The palette stays inside the terminal's own sixteen colours, so the tool
// inherits whatever theme the operator already trusts instead of imposing one
// — a control room screen that fights the rest of the desktop is a screen
// people stop reading. Colour carries meaning and nothing else: quality is
// what an operator scans for, so quality gets the colour and the furniture
// stays grey.
var (
	cAccent = lipgloss.Color("6") // cyan: structure and selection
	cGood   = lipgloss.Color("2")
	cWarn   = lipgloss.Color("3")
	cBad    = lipgloss.Color("1")
	cEvent  = lipgloss.Color("5")
	cMuted  = lipgloss.Color("8")
)

var (
	stTitle   = lipgloss.NewStyle().Bold(true).Foreground(cAccent)
	stMuted   = lipgloss.NewStyle().Foreground(cMuted)
	stBold    = lipgloss.NewStyle().Bold(true)
	stColHead = lipgloss.NewStyle().Bold(true).Foreground(cAccent)
	stSel     = lipgloss.NewStyle().Reverse(true)
	stTabOn   = lipgloss.NewStyle().Bold(true).Reverse(true)
	stTabOff  = lipgloss.NewStyle().Faint(true)
	stGood    = lipgloss.NewStyle().Foreground(cGood)
	stWarn    = lipgloss.NewStyle().Foreground(cWarn)
	stBad     = lipgloss.NewStyle().Foreground(cBad)
	stEvent   = lipgloss.NewStyle().Foreground(cEvent)
	stKey     = lipgloss.NewStyle().Bold(true).Foreground(cAccent)
	stStale   = lipgloss.NewStyle().Faint(true)
)

// levelStyle maps a log level to how loudly it should be drawn.
func levelStyle(level string) lipgloss.Style {
	switch level {
	case "error":
		return stBad
	case "warn":
		return stWarn
	case "ok":
		return stGood
	default:
		return lipgloss.NewStyle()
	}
}

// ---------- text fitting ----------
//
// Every cell in this interface is drawn at an exact column width. Styles are
// applied to already-padded text, never inside it, because measuring a string
// that has escape sequences in the middle of it is how tables come out ragged.

// truncate fits s into w display columns, marking any loss with an ellipsis.
func truncate(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= w {
		return s
	}
	if w == 1 {
		return "…"
	}
	r := []rune(s)
	for len(r) > 0 && lipgloss.Width(string(r))+1 > w {
		r = r[:len(r)-1]
	}
	return string(r) + "…"
}

// cell renders s as exactly w columns, left or right aligned.
func cell(s string, w int, right bool) string {
	s = truncate(s, w)
	pad := w - lipgloss.Width(s)
	if pad <= 0 {
		return s
	}
	if right {
		return strings.Repeat(" ", pad) + s
	}
	return s + strings.Repeat(" ", pad)
}

// pad extends s to w columns without truncating it, for lines that are already
// known to fit.
func pad(s string, w int) string {
	if n := w - lipgloss.Width(s); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}

// fit forces s to exactly w columns.
func fit(s string, w int) string {
	if lipgloss.Width(s) > w {
		return truncate(s, w)
	}
	return pad(s, w)
}

func repeat(r string, n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat(r, n)
}

// ---------- drawing ----------

// box draws a titled frame of exactly w columns and h rows around the given
// content lines.
//
// It is drawn by hand rather than with a border style because the title
// belongs in the top rule: a panel that names itself in the frame costs no
// content row, and on a twelve-row terminal every row is contested.
func box(title string, w, h int, lines []string) []string {
	if w < 4 || h < 2 {
		return clip(lines, h, w)
	}
	out := make([]string, 0, h)

	head := "╭─ " + title + " "
	if lipgloss.Width(head) > w-1 {
		head = truncate(head, w-1)
	}
	out = append(out, stMuted.Render("╭─ ")+stColHead.Render(title)+" "+
		stMuted.Render(repeat("─", w-lipgloss.Width(head)-1)+"╮"))

	inner := w - 4 // one space of padding either side of the frame
	for i := range h - 2 {
		var content string
		if i < len(lines) {
			content = lines[i]
		}
		out = append(out, stMuted.Render("│ ")+fit(content, inner)+stMuted.Render(" │"))
	}
	out = append(out, stMuted.Render("╰"+repeat("─", w-2)+"╯"))
	return out
}

// clip forces a block of lines to exactly h rows of w columns.
func clip(lines []string, h, w int) []string {
	out := make([]string, h)
	for i := range h {
		if i < len(lines) {
			out[i] = fit(lines[i], w)
		} else {
			out[i] = repeat(" ", w)
		}
	}
	return out
}

// joinColumns places blocks side by side with a single column of gutter.
func joinColumns(blocks [][]string, h int) []string {
	out := make([]string, h)
	for row := range h {
		var b strings.Builder
		for i, blk := range blocks {
			if i > 0 {
				b.WriteByte(' ')
			}
			if row < len(blk) {
				b.WriteString(blk[row])
			}
		}
		out[row] = b.String()
	}
	return out
}

// sparkRunes is an eight-level bar, low to high.
var sparkRunes = []rune("▁▂▃▄▅▆▇█")

// sparkline renders recent history as a single row of blocks.
//
// It is scaled to the window it shows rather than to the point's engineering
// range, because the question it answers is "is this moving, and which way",
// not "what is the value" — the value column already says that.
func sparkline(vals []float64, w int) string {
	if w <= 0 || len(vals) == 0 {
		return ""
	}
	if len(vals) > w {
		vals = vals[len(vals)-w:]
	}
	lo, hi := vals[0], vals[0]
	for _, v := range vals {
		lo = min(lo, v)
		hi = max(hi, v)
	}
	span := hi - lo
	var b strings.Builder
	for _, v := range vals {
		if span <= 1e-12 {
			// A flat trace must still read as a trace, not as an empty cell.
			b.WriteRune(sparkRunes[3])
			continue
		}
		idx := int((v - lo) / span * float64(len(sparkRunes)-1))
		b.WriteRune(sparkRunes[min(max(idx, 0), len(sparkRunes)-1)])
	}
	return b.String()
}

// scrollbarRune returns the character for one row of a scrollbar track.
func scrollbarRune(row, height, offset, total int) string {
	if total <= height || height <= 0 {
		return " "
	}
	// The thumb is at least one row tall, and its position reflects how far
	// down the list the window sits.
	thumb := max(height*height/total, 1)
	top := offset * (height - thumb) / max(total-height, 1)
	if row >= top && row < top+thumb {
		return "█"
	}
	return "│"
}
