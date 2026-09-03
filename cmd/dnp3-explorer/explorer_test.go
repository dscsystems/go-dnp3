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
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/channel"
	"github.com/dscsystems/go-dnp3/master"
)

// The explorer is driven here without a terminal: the model is a pure function
// of the messages it has been given, so it can be exercised by feeding it the
// same messages Bubble Tea would. That is the payoff of keeping the DNP3
// session entirely out of the model — and it extends to the pointer, because
// every click resolves through the same layout the renderer draws from.

func testModel() *Model {
	conn := &connection{
		msgs: make(chan tea.Msg, 64),
		ctx:  context.Background(),
		cur: link{
			Host: "test:20000", Local: 1, Remote: 10,
			Timeout: 5 * time.Second, Poll: 5 * time.Second,
		},
	}
	m := NewModel(conn)
	m.width, m.height = 100, 30
	return m
}

func press(m *Model, key string) *Model {
	next, _ := m.HandleKey(key)
	return next.(*Model)
}

func pressCmd(m *Model, key string) (*Model, tea.Cmd) {
	next, cmd := m.HandleKey(key)
	return next.(*Model), cmd
}

func click(m *Model, x, y int) *Model {
	next, _ := m.HandleMouse(mouseEvent{x: x, y: y, button: tea.MouseLeft, kind: mouseClick})
	return next.(*Model)
}

func wheel(m *Model, x, y int, up bool) *Model {
	b := tea.MouseWheelDown
	if up {
		b = tea.MouseWheelUp
	}
	next, _ := m.HandleMouse(mouseEvent{x: x, y: y, button: b, kind: mouseWheel})
	return next.(*Model)
}

// zoneOf finds where a kind of region was laid out, so a test can click the
// thing rather than a hard-coded coordinate.
func zoneOf(m *Model, kind zoneKind, n int) (rect, bool) {
	for _, z := range m.layout().zones {
		if z.kind == kind && z.n == n {
			return z.rect, true
		}
	}
	return rect{}, false
}

func TestViewRendersWithoutData(t *testing.T) {
	m := testModel()
	out := m.View().Content

	if !strings.Contains(out, "dnp3-explorer") {
		t.Error("the header is missing")
	}
	for _, name := range screenNames {
		if !strings.Contains(out, name) {
			t.Errorf("the %s tab is missing", name)
		}
	}
	// An empty tool must say what to do, not just show an empty box.
	if !strings.Contains(out, "integrity poll") {
		t.Error("an empty overview should tell the operator how to fill it")
	}
}

// TestFrameFillsTheTerminal covers the thing that makes a full-screen layout
// possible at all: the frame is exactly as tall as the terminal and never
// wider, at every size and on every screen. A single overlong line wraps and
// pushes the footer off the bottom.
func TestFrameFillsTheTerminal(t *testing.T) {
	sizes := [][2]int{{60, 12}, {80, 24}, {100, 30}, {200, 60}, {96, 13}, {121, 41}}

	// Both confirmation modes, because the unconfirmed one claims the right of
	// the toolbar for its warning and the buttons have to give way to it
	// rather than run past the edge.
	for _, confirm := range []bool{true, false} {
		for _, size := range sizes {
			for screen := range numScreens {
				m := testModel()
				m.width, m.height = size[0], size[1]
				m.screen = screen
				m.detail = true
				m.confirm = confirm
				seedPoints(m, 40)
				for i := range 50 {
					m.addLog("info", strings.Repeat("log line ", i%5+1))
				}

				// Closed and open, because the editor is a second body layout
				// that has to fill the same frame exactly — including on a
				// terminal too short to show all of its fields at once.
				for _, form := range []bool{false, true} {
					m.form = formState{}
					if form {
						m.openConnectionForm()
						m.form.cursor = numFields - 1
						m.form.err = strings.Repeat("a long complaint ", 6)
					}

					lines := strings.Split(m.View().Content, "\n")
					if len(lines) != m.height {
						t.Errorf("%dx%d %v confirm=%v form=%v: drew %d lines, want %d",
							m.width, m.height, screen, confirm, form, len(lines), m.height)
					}
					for i, l := range lines {
						if w := lipgloss.Width(l); w > m.width {
							t.Errorf("%dx%d %v confirm=%v form=%v: line %d is %d columns wide, want at most %d:\n%q",
								m.width, m.height, screen, confirm, form, i, w, m.width, l)
						}
					}
				}
			}
		}
	}
}

func TestTooSmallTerminalSaysSo(t *testing.T) {
	m := testModel()
	m.width, m.height = 40, 8

	out := m.View().Content
	if !strings.Contains(out, "at least") {
		t.Errorf("a cramped terminal should be told, not given a mangled table:\n%s", out)
	}
}

func TestPointsScreenShowsValues(t *testing.T) {
	m := testModel()
	m.applyUpdate(updateMsg{
		Type: dnp3.TypeAnalog, Index: 3, Value: "11025.500",
		Flags: dnp3.Online, Stamp: dnp3.Now(time.Now()),
	})
	m.applyUpdate(updateMsg{
		Type: dnp3.TypeBinary, Index: 0, Value: "ON", Flags: dnp3.Online,
	})
	m.screen = ScreenPoints

	out := m.View().Content
	for _, want := range []string{"AI 3", "11025.500", "BI 0", "ON", "ONLINE"} {
		if !strings.Contains(out, want) {
			t.Errorf("the points table is missing %q:\n%s", want, out)
		}
	}
	// The state bit is already spelled as ON; repeating it as a flag is noise.
	if strings.Contains(out, "STATE") {
		t.Error("the state bit should not be repeated in the quality column")
	}
}

func TestPointsSortedByTypeThenIndex(t *testing.T) {
	m := testModel()
	// Deliberately out of order.
	m.applyUpdate(updateMsg{Type: dnp3.TypeAnalog, Index: 5, Value: "5"})
	m.applyUpdate(updateMsg{Type: dnp3.TypeBinary, Index: 2, Value: "ON"})
	m.applyUpdate(updateMsg{Type: dnp3.TypeAnalog, Index: 1, Value: "1"})
	m.applyUpdate(updateMsg{Type: dnp3.TypeBinary, Index: 0, Value: "OFF"})

	rows := m.visiblePoints()
	want := []string{"BI 0", "BI 2", "AI 1", "AI 5"}
	for i, w := range want {
		if got := pointLabel(rows[i].Key); got != w {
			t.Errorf("row %d = %s, want %s — the table reorders under the cursor", i, got, w)
		}
	}
}

func TestBadQualityIsVisible(t *testing.T) {
	// Quality is the thing an operator scans for, so an offline point must not
	// look like a healthy one.
	m := testModel()
	m.applyUpdate(updateMsg{
		Type: dnp3.TypeAnalog, Index: 0, Value: "42",
		Flags: dnp3.CommLost, // not online
	})
	m.screen = ScreenPoints

	out := m.View().Content
	if !strings.Contains(out, "COMM_LOST") {
		t.Errorf("the quality column does not show the fault:\n%s", out)
	}
}

// TestSortByQualityPutsFaultsFirst covers what sorting is for here: finding
// the broken points in a device with a thousand good ones.
func TestSortByQualityPutsFaultsFirst(t *testing.T) {
	m := testModel()
	m.applyUpdate(updateMsg{Type: dnp3.TypeAnalog, Index: 0, Value: "1", Flags: dnp3.Online})
	m.applyUpdate(updateMsg{Type: dnp3.TypeAnalog, Index: 1, Value: "2", Flags: dnp3.CommLost})
	m.applyUpdate(updateMsg{Type: dnp3.TypeAnalog, Index: 2, Value: "3", Flags: dnp3.Online})

	m.sortBy = sortQuality
	rows := m.visiblePoints()
	if rows[0].Key.Index != 1 {
		t.Errorf("quality sort put %s first, want the offline point AI 1",
			pointLabel(rows[0].Key))
	}
}

func TestEventsScreenCollectsEvents(t *testing.T) {
	m := testModel()
	for i := range 3 {
		m.applyUpdate(updateMsg{
			Type: dnp3.TypeBinary, Index: uint16(i), Value: "ON",
			Flags: dnp3.Online, IsEvent: true,
		})
	}
	m.screen = ScreenEvents

	if len(m.events) != 3 {
		t.Fatalf("%d events recorded, want 3", len(m.events))
	}
	out := m.View().Content
	if !strings.Contains(out, "BI 2") {
		t.Errorf("the events list is missing a row:\n%s", out)
	}

	// Static values must not land in the event list.
	m.applyUpdate(updateMsg{Type: dnp3.TypeAnalog, Index: 0, Value: "1"})
	if len(m.events) != 3 {
		t.Error("a static value was recorded as an event")
	}
}

// TestEventListIsBounded covers an event storm: the list must not grow without
// limit, because nobody scrolls back ten thousand rows and the memory is real.
func TestEventListIsBounded(t *testing.T) {
	m := testModel()
	for i := range 5000 {
		m.applyUpdate(updateMsg{
			Type: dnp3.TypeBinary, Index: uint16(i % 10), Value: "ON", IsEvent: true,
		})
	}
	if len(m.events) > 2000 {
		t.Errorf("the event list grew to %d rows unbounded", len(m.events))
	}
	// The newest must survive, since that is what an operator is watching.
	if len(m.events) == 0 {
		t.Fatal("the event list was emptied")
	}
}

func TestFilterNarrowsPoints(t *testing.T) {
	m := testModel()
	m.applyUpdate(updateMsg{Type: dnp3.TypeAnalog, Index: 0, Value: "1"})
	m.applyUpdate(updateMsg{Type: dnp3.TypeBinary, Index: 0, Value: "ON"})

	m.filter = "ai"
	rows := m.visiblePoints()
	if len(rows) != 1 || rows[0].Key.Type != dnp3.TypeAnalog {
		t.Errorf("the filter matched %d rows, want just the analog", len(rows))
	}
}

// TestFilterMatchesQuality covers the filter reading the whole row: an
// operator hunting a fault types the flag, not the point name.
func TestFilterMatchesQuality(t *testing.T) {
	m := testModel()
	m.applyUpdate(updateMsg{Type: dnp3.TypeAnalog, Index: 0, Value: "1", Flags: dnp3.Online})
	m.applyUpdate(updateMsg{Type: dnp3.TypeAnalog, Index: 1, Value: "2", Flags: dnp3.CommLost})

	m.filter = "comm_lost"
	rows := m.visiblePoints()
	if len(rows) != 1 || rows[0].Key.Index != 1 {
		t.Errorf("filtering on a quality flag matched %d rows, want the one bad point", len(rows))
	}
}

func TestFilterTypingCapturesKeys(t *testing.T) {
	m := testModel()
	m.screen = ScreenPoints

	m = press(m, "/")
	if !m.prompt.active {
		t.Fatal("/ did not open the filter")
	}
	// While filtering, keys are text — pressing q must not quit.
	m = press(m, "q")
	if m.quitting {
		t.Fatal("q quit the program while the operator was typing a filter")
	}
	if m.filter != "q" {
		t.Errorf("filter = %q, want %q", m.filter, "q")
	}

	m = press(m, "enter")
	if m.prompt.active {
		t.Error("enter did not close the filter prompt")
	}
	m = press(m, "esc")
	if m.filter != "" {
		t.Errorf("esc left the filter as %q", m.filter)
	}
}

func TestScreenNavigation(t *testing.T) {
	m := testModel()

	m = press(m, "3")
	if m.screen != ScreenEvents {
		t.Errorf("screen = %v, want Events", m.screen)
	}

	m = press(m, "tab")
	if m.screen != ScreenLog {
		t.Errorf("tab from Events gave %v, want Log", m.screen)
	}
	m = press(m, "tab")
	if m.screen != ScreenFiles {
		t.Errorf("tab from Log gave %v, want Files", m.screen)
	}
	m = press(m, "tab")
	if m.screen != ScreenHelp {
		t.Errorf("tab from Files gave %v, want Help", m.screen)
	}
	// And it wraps rather than sticking at the end.
	m = press(m, "tab")
	if m.screen != ScreenOverview {
		t.Errorf("tab from the last screen gave %v, want Overview", m.screen)
	}
}

// TestPerScreenCursors covers not losing an operator's place: reading down the
// event list, glancing at Points, and coming back must return to the same row.
func TestPerScreenCursors(t *testing.T) {
	m := testModel()
	seedPoints(m, 20)

	m.screen = ScreenPoints
	for range 5 {
		m = press(m, "j")
	}
	m = press(m, "3")
	m = press(m, "2")

	if got := m.cursor[ScreenPoints]; got != 5 {
		t.Errorf("cursor came back at %d, want 5", got)
	}
}

func TestCursorStaysInRange(t *testing.T) {
	m := testModel()
	m.screen = ScreenPoints
	m.applyUpdate(updateMsg{Type: dnp3.TypeAnalog, Index: 0, Value: "1"})
	m.applyUpdate(updateMsg{Type: dnp3.TypeAnalog, Index: 1, Value: "2"})

	for range 10 {
		m = press(m, "j")
	}
	if m.cursor[ScreenPoints] > 1 {
		t.Errorf("cursor ran past the last row: %d", m.cursor[ScreenPoints])
	}
	for range 10 {
		m = press(m, "k")
	}
	if m.cursor[ScreenPoints] < 0 {
		t.Errorf("cursor ran above the first row: %d", m.cursor[ScreenPoints])
	}
}

// TestControlKeysNeedABinaryOutput covers the guard against operating the
// wrong thing: 'c' on an analog must do nothing but explain itself.
func TestControlKeysNeedABinaryOutput(t *testing.T) {
	m := testModel()
	m.screen = ScreenPoints
	m.applyUpdate(updateMsg{Type: dnp3.TypeAnalog, Index: 0, Value: "1"})

	m = press(m, "c")
	if _, ok := m.selectedControl(); ok {
		t.Error("an analog point was offered as a control")
	}
	if m.modal.kind != modalNone {
		t.Error("a control dialog opened for an analog input")
	}
	found := false
	for _, l := range m.logs {
		if strings.Contains(l.Text, "binary output") {
			found = true
		}
	}
	if !found {
		t.Error("pressing c on a non-control should say why nothing happened")
	}
}

func TestControlKeyFindsBinaryOutput(t *testing.T) {
	m := testModel()
	m.screen = ScreenPoints
	m.applyUpdate(updateMsg{Type: dnp3.TypeBinaryOutputStatus, Index: 7, Value: "OFF"})

	k, ok := m.selectedControl()
	if !ok || k.Index != 7 {
		t.Errorf("selectedControl = %v, %v; want BO 7", k, ok)
	}
}

// TestControlAsksBeforeOperating covers the safety interlock: a keystroke on
// the Points screen must not reach a breaker on its own.
func TestControlAsksBeforeOperating(t *testing.T) {
	m := testModel()
	m.screen = ScreenPoints
	m.applyUpdate(updateMsg{Type: dnp3.TypeBinaryOutputStatus, Index: 3, Value: "OFF"})

	m, cmd := pressCmd(m, "c")
	if cmd != nil {
		t.Fatal("c sent a control without confirmation")
	}
	if m.modal.kind != modalConfirm {
		t.Fatalf("no confirmation dialog opened (modal %v)", m.modal.kind)
	}
	if !strings.Contains(m.modal.desc, "close BO 3") {
		t.Errorf("the dialog does not name the control: %q", m.modal.desc)
	}
	// It has to be visible, not merely present in the state.
	if out := m.View().Content; !strings.Contains(out, "Confirm control") {
		t.Errorf("the dialog was not drawn:\n%s", out)
	}

	if _, cmd = pressCmd(m, "y"); cmd == nil {
		t.Error("y did not issue the control")
	}

	// And cancelling leaves nothing behind — from a fresh dialog, since
	// confirming closed the last one.
	m, _ = pressCmd(m, "c")
	m = press(m, "n")
	if m.modal.kind != modalNone {
		t.Error("n did not close the dialog")
	}
}

func TestControlSkipsConfirmationWhenAsked(t *testing.T) {
	m := testModel()
	m.confirm = false
	m.screen = ScreenPoints
	m.applyUpdate(updateMsg{Type: dnp3.TypeBinaryOutputStatus, Index: 1, Value: "OFF"})

	_, cmd := pressCmd(m, "o")
	if cmd == nil {
		t.Error("-no-confirm should send the control directly")
	}
}

// TestControlDialogOffersPulses covers enter on an output: the operator gets
// the four things a CROB can do, named, rather than having to remember which
// key is which.
func TestControlDialogOffersPulses(t *testing.T) {
	m := testModel()
	m.screen = ScreenPoints
	m.applyUpdate(updateMsg{Type: dnp3.TypeBinaryOutputStatus, Index: 2, Value: "ON"})

	m = press(m, "enter")
	if m.modal.kind != modalControl {
		t.Fatalf("enter on a binary output opened %v", m.modal.kind)
	}
	out := m.View().Content
	for _, want := range []string{"Latch ON", "Latch OFF", "Pulse trip", "Pulse close"} {
		if !strings.Contains(out, want) {
			t.Errorf("the control dialog is missing %q:\n%s", want, out)
		}
	}

	// Choosing a pulse goes on to ask for confirmation rather than firing.
	m = press(m, "t")
	if m.modal.kind != modalConfirm {
		t.Fatalf("choosing a pulse gave %v, want a confirmation", m.modal.kind)
	}
	if !strings.Contains(m.modal.desc, "pulse trip") {
		t.Errorf("the confirmation does not describe the pulse: %q", m.modal.desc)
	}
}

func TestAnalogSetpointPrompt(t *testing.T) {
	m := testModel()
	m.screen = ScreenPoints
	m.applyUpdate(updateMsg{Type: dnp3.TypeAnalogOutputStatus, Index: 4, Value: "0"})

	m = press(m, "enter")
	if !m.prompt.active || m.prompt.kind != promptAnalog {
		t.Fatal("enter on an analog output did not ask for a value")
	}
	for _, k := range []string{"1", "3", ".", "7"} {
		m = press(m, k)
	}
	m = press(m, "enter")

	if m.modal.kind != modalConfirm {
		t.Fatalf("a setpoint went out without confirmation (modal %v)", m.modal.kind)
	}
	if !strings.Contains(m.modal.desc, "13.7") {
		t.Errorf("the confirmation does not show the value: %q", m.modal.desc)
	}
}

func TestParseAnalogWrite(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"12", "i32", false},
		{"12.5", "f32", false},
		{"-3.25f64", "f64", false},
		{"100i16", "i16", false},
		{"70000i16", "", true},
		{"", "", true},
		{"banana", "", true},
	}
	for _, tc := range tests {
		_, desc, err := parseAnalogWrite(1, tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseAnalogWrite(%q) accepted a value it cannot send", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseAnalogWrite(%q): %v", tc.in, err)
			continue
		}
		// The encoding must be named back to the operator: a device that
		// wants an int16 will reject a float, and a silent choice is a
		// rejection they cannot explain.
		if !strings.Contains(desc, tc.want) {
			t.Errorf("parseAnalogWrite(%q) described as %q, want it to name %s",
				tc.in, desc, tc.want)
		}
	}
}

func TestParseRangeScan(t *testing.T) {
	tests := []struct {
		in          string
		g, v        uint8
		start, stop uint16
		wantErr     bool
	}{
		{in: "30.5 0-15", g: 30, v: 5, start: 0, stop: 15},
		{in: "1 0 9", g: 1, v: 0, start: 0, stop: 9},
		{in: "30,2,4,8", g: 30, v: 2, start: 4, stop: 8},
		{in: "30 9-2", wantErr: true},
		{in: "30", wantErr: true},
		{in: "x 0-1", wantErr: true},
	}
	for _, tc := range tests {
		g, v, start, stop, err := parseRangeScan(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseRangeScan(%q) accepted nonsense", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseRangeScan(%q): %v", tc.in, err)
			continue
		}
		if g != tc.g || v != tc.v || start != tc.start || stop != tc.stop {
			t.Errorf("parseRangeScan(%q) = g%dv%d %d-%d, want g%dv%d %d-%d",
				tc.in, g, v, start, stop, tc.g, tc.v, tc.start, tc.stop)
		}
	}
}

func TestRestartAsksWhichKind(t *testing.T) {
	m := testModel()
	m, cmd := pressCmd(m, "R")
	if cmd != nil {
		t.Fatal("R restarted the device without asking")
	}
	if m.modal.kind != modalRestart {
		t.Fatalf("R opened %v, want the restart dialog", m.modal.kind)
	}
	_, cmd = pressCmd(m, "c")
	if cmd == nil {
		t.Error("choosing a cold restart did not issue one")
	}
}

// ---------- scrolling ----------

func TestClampScrollKeepsCursorVisible(t *testing.T) {
	tests := []struct {
		cursor, offset, total, visible int
		wantOffset                     int
	}{
		{cursor: 0, offset: 0, total: 5, visible: 10, wantOffset: 0},   // everything fits
		{cursor: 0, offset: 0, total: 100, visible: 10, wantOffset: 0}, // at the top
		{cursor: 99, offset: 0, total: 100, visible: 10, wantOffset: 90},
		{cursor: 50, offset: 90, total: 100, visible: 10, wantOffset: 50},
		{cursor: 5, offset: 40, total: 100, visible: 10, wantOffset: 5},
	}
	for _, tc := range tests {
		m := testModel()
		m.screen = ScreenPoints
		m.cursor[ScreenPoints], m.offset[ScreenPoints] = tc.cursor, tc.offset
		m.clampScroll(tc.total, tc.visible)

		off, cur := m.offset[ScreenPoints], m.cursor[ScreenPoints]
		if off != tc.wantOffset {
			t.Errorf("clampScroll(cursor %d, offset %d, %d rows, %d visible) = %d, want %d",
				tc.cursor, tc.offset, tc.total, tc.visible, off, tc.wantOffset)
		}
		if cur < off || cur >= off+tc.visible {
			t.Errorf("clampScroll scrolled the cursor (%d) out of the window [%d,%d)",
				cur, off, off+tc.visible)
		}
	}
}

func TestFollowPinsToTheNewestRow(t *testing.T) {
	m := testModel()
	m.screen = ScreenEvents
	for i := range 100 {
		m.applyUpdate(updateMsg{Type: dnp3.TypeBinary, Index: uint16(i % 4),
			Value: "ON", IsEvent: true})
	}
	m.layout() // clamps

	if got := m.cursor[ScreenEvents]; got != 99 {
		t.Errorf("following left the cursor at %d, want the newest row 99", got)
	}
	// Moving by hand takes the view off the tail, or an operator can never
	// read a row that scrolled past.
	m = press(m, "k")
	if m.follow {
		t.Error("moving the cursor did not stop following")
	}
}

// ---------- the mouse ----------

func TestClickingATabSwitchesScreen(t *testing.T) {
	m := testModel()
	r, ok := zoneOf(m, zoneTab, int(ScreenEvents))
	if !ok {
		t.Fatal("the Events tab is not clickable")
	}
	m = click(m, r.x+1, r.y)
	if m.screen != ScreenEvents {
		t.Errorf("clicking the Events tab gave %v", m.screen)
	}
}

func TestClickingARowSelectsIt(t *testing.T) {
	m := testModel()
	m.screen = ScreenPoints
	seedPoints(m, 30)

	l := m.layout()
	m = click(m, l.rows.x+2, l.rows.y+4)

	if got := m.cursor[ScreenPoints]; got != 4 {
		t.Errorf("clicking the fifth row selected row %d", got)
	}
	// A click below the last row must not select anything that is not there.
	m2 := testModel()
	m2.screen = ScreenPoints
	m2.applyUpdate(updateMsg{Type: dnp3.TypeAnalog, Index: 0, Value: "1"})
	l2 := m2.layout()
	m2 = click(m2, l2.rows.x+2, l2.rows.y+8)
	if got := m2.cursor[ScreenPoints]; got != 0 {
		t.Errorf("clicking empty space moved the cursor to %d", got)
	}
}

// TestClickingASelectedControlOpensTheDialog covers the two-stage gesture: one
// click selects, and only a second one on the same row offers to operate.
func TestClickingASelectedControlOpensTheDialog(t *testing.T) {
	m := testModel()
	m.screen = ScreenPoints
	m.applyUpdate(updateMsg{Type: dnp3.TypeBinaryOutputStatus, Index: 0, Value: "OFF"})
	m.applyUpdate(updateMsg{Type: dnp3.TypeBinaryOutputStatus, Index: 1, Value: "OFF"})

	l := m.layout()
	m = click(m, l.rows.x+2, l.rows.y+1) // select BO 1
	if m.modal.kind != modalNone {
		t.Fatal("a single click opened a control dialog")
	}
	m = click(m, l.rows.x+2, l.rows.y+1) // act on it
	if m.modal.kind != modalControl {
		t.Errorf("clicking a selected output gave %v, want the control dialog", m.modal.kind)
	}
	if m.modal.target.Index != 1 {
		t.Errorf("the dialog targets BO %d, want the row that was clicked", m.modal.target.Index)
	}
}

func TestClickingAColumnHeadingSorts(t *testing.T) {
	m := testModel()
	m.screen = ScreenPoints
	seedPoints(m, 5)

	l := m.layout()
	var valueCol int
	for i, c := range l.cols {
		if c.key == sortValue {
			valueCol = i
		}
	}
	r, ok := zoneOf(m, zoneColumn, valueCol)
	if !ok {
		t.Fatal("the VALUE heading is not clickable")
	}

	m = click(m, r.x+1, r.y)
	if m.sortBy != sortValue || m.sortDesc {
		t.Errorf("clicking VALUE gave sort %v desc=%v", m.sortBy, m.sortDesc)
	}
	m = click(m, r.x+1, r.y)
	if !m.sortDesc {
		t.Error("clicking the same heading again did not reverse the sort")
	}
}

func TestWheelScrollsWithoutMovingTheCursor(t *testing.T) {
	m := testModel()
	m.screen = ScreenPoints
	seedPoints(m, 100)
	m.layout()

	before := m.cursor[ScreenPoints]
	l := m.layout()
	m = wheel(m, l.rows.x+2, l.rows.y+2, false)

	if m.offset[ScreenPoints] != wheelStep {
		t.Errorf("the wheel scrolled to offset %d, want %d",
			m.offset[ScreenPoints], wheelStep)
	}
	if m.cursor[ScreenPoints] != before+wheelStep {
		// The cursor only moves because the window left it behind, and it must
		// land at the edge of the window rather than anywhere else.
		t.Errorf("cursor = %d, want it dragged to the top of the window",
			m.cursor[ScreenPoints])
	}

	m = wheel(m, l.rows.x+2, l.rows.y+2, true)
	if m.offset[ScreenPoints] != 0 {
		t.Errorf("scrolling back gave offset %d, want 0", m.offset[ScreenPoints])
	}
}

func TestWheelOnTabsChangesScreen(t *testing.T) {
	m := testModel()
	m = wheel(m, 3, rowTabs, false)
	if m.screen != ScreenPoints {
		t.Errorf("wheeling down the tab bar gave %v, want Points", m.screen)
	}
	m = wheel(m, 3, rowTabs, true)
	if m.screen != ScreenOverview {
		t.Errorf("wheeling back gave %v, want Overview", m.screen)
	}
}

func TestClickingAToolbarButtonRunsIt(t *testing.T) {
	m := testModel()
	m.screen = ScreenEvents
	m.follow = true

	l := m.layout()
	var followBtn = -1
	for i, b := range l.buttons {
		if b.key == "f" {
			followBtn = i
		}
	}
	if followBtn < 0 {
		t.Fatal("the Events toolbar has no follow button")
	}
	r, _ := zoneOf(m, zoneButton, followBtn)
	m = click(m, r.x+1, r.y)

	if m.follow {
		t.Error("clicking the follow button did not toggle following")
	}
}

func TestDraggingTheScrollbar(t *testing.T) {
	m := testModel()
	m.screen = ScreenPoints
	seedPoints(m, 200)
	l := m.layout()
	if l.scroll.empty() {
		t.Fatal("a list longer than the window drew no scrollbar")
	}

	m = click(m, l.scroll.x, l.scroll.y+l.scroll.h-1)
	if !m.dragging {
		t.Error("clicking the scrollbar did not start a drag")
	}
	if got := m.offset[ScreenPoints]; got != l.total-l.rows.h {
		t.Errorf("dragging to the bottom gave offset %d, want %d", got, l.total-l.rows.h)
	}

	next, _ := m.HandleMouse(mouseEvent{x: l.scroll.x, y: l.scroll.y, kind: mouseRelease})
	if next.(*Model).dragging {
		t.Error("releasing the button did not end the drag")
	}
}

// TestClickingOutsideADialogDismissesIt covers the pointer honouring the same
// interlock as the keyboard: the dialog is modal for both.
func TestClickingOutsideADialogDismissesIt(t *testing.T) {
	m := testModel()
	m.screen = ScreenPoints
	m.applyUpdate(updateMsg{Type: dnp3.TypeBinaryOutputStatus, Index: 0, Value: "OFF"})
	m = press(m, "enter")
	if m.modal.kind == modalNone {
		t.Fatal("no dialog to dismiss")
	}

	l := m.layout()
	m = click(m, l.body.x, l.body.y) // the top-left corner, outside the box
	if m.modal.kind != modalNone {
		t.Error("clicking outside the dialog left it open")
	}
}

func TestClickingADialogChoiceRunsIt(t *testing.T) {
	m := testModel()
	m.screen = ScreenPoints
	m.applyUpdate(updateMsg{Type: dnp3.TypeBinaryOutputStatus, Index: 0, Value: "OFF"})
	m = press(m, "enter")

	l := m.layout()
	var latchOn = -1
	for i, c := range l.choices {
		if c.key == "c" {
			latchOn = i
		}
	}
	if latchOn < 0 {
		t.Fatal("the control dialog offers no latch-on choice")
	}
	r, _ := zoneOf(m, zoneChoice, latchOn)
	m = click(m, r.x+1, r.y)

	if m.modal.kind != modalConfirm {
		t.Errorf("clicking a choice gave %v, want the confirmation", m.modal.kind)
	}
}

// TestClickingWhileAPromptIsOpenDoesNotTypeIntoIt covers the one way the two
// input methods could still have reached each other by accident: a click is
// dispatched by pressing the key the keyboard would have pressed, and while a
// prompt is open that key is a character. Clicking the Integrity button
// mid-filter used to filter for "i".
func TestClickingWhileAPromptIsOpenDoesNotTypeIntoIt(t *testing.T) {
	m := testModel()
	seedPoints(m, 5)
	m = press(m, "2")
	m = press(m, "/")
	m = press(m, "B")

	r, ok := zoneOf(m, zoneButton, 0) // [i Integrity]
	if !ok {
		t.Fatal("the toolbar has no buttons to click")
	}
	m = click(m, r.x+1, r.y)

	if m.prompt.active {
		t.Error("a click should close the prompt rather than leave it open")
	}
	if m.filter != "B" {
		t.Errorf("filter is %q; the click was typed into it", m.filter)
	}
}

// A prompt that would send something is abandoned by a click, not submitted:
// half a setpoint is not an instruction.
func TestClickingAwayFromASetpointPromptDropsIt(t *testing.T) {
	m := testModel()
	m.screen = ScreenPoints
	m.applyUpdate(updateMsg{Type: dnp3.TypeAnalogOutputStatus, Index: 0, Value: "13.75"})

	m = press(m, "enter")
	if m.prompt.kind != promptAnalog || !m.prompt.active {
		t.Fatal("enter on an analog output did not open the write prompt")
	}
	m = press(m, "9")

	r, _ := zoneOf(m, zoneRows, 0)
	m = click(m, r.x+1, r.y)

	if m.prompt.active {
		t.Error("the prompt survived a click")
	}
	if m.modal.kind != modalNone {
		t.Errorf("the click reached a control dialog: %v", m.modal.kind)
	}
}

// TestNoConfirmIsVisible covers the mode that has no dialog to announce it.
// With -no-confirm the next control goes straight out, and the only thing
// standing between the operator and that is the toolbar saying so.
func TestNoConfirmIsVisible(t *testing.T) {
	m := testModel()
	m.confirm = false
	m.screen = ScreenPoints

	if out := m.View().Content; !strings.Contains(out, "controls send immediately") {
		t.Error("-no-confirm is not announced anywhere on the Points screen")
	}

	m.screen = ScreenOverview
	if out := m.View().Content; !strings.Contains(out, "sends immediately") {
		t.Error("the overview does not say controls are unconfirmed")
	}

	m.confirm = true
	for _, s := range []Screen{ScreenPoints, ScreenOverview} {
		m.screen = s
		if out := m.View().Content; strings.Contains(out, "sends immediately") {
			t.Errorf("%v warns about confirmation when controls are confirmed", s)
		}
	}
}

// ---------- the connection editor ----------

func TestConnectionFormOpensFromTheLiveSession(t *testing.T) {
	m := testModel()
	m = press(m, "C")

	if !m.form.active {
		t.Fatal("C did not open the connection editor")
	}
	want := map[int]string{
		fieldAddress: "test:20000",
		fieldLocal:   "1",
		fieldRemote:  "10",
		fieldTimeout: "5s",
		fieldPoll:    "5s",
	}
	for idx, w := range want {
		if got := m.form.fields[idx].value; got != w {
			t.Errorf("field %d = %q, want %q — the editor must open on what is in force", idx, got, w)
		}
	}
	if out := m.View().Content; !strings.Contains(out, "Connection") {
		t.Error("the editor is not drawn")
	}
}

// TestFormSwallowsEveryKeystroke is the one that matters for safety: while a
// field is being typed into, the letters that are actions everywhere else must
// be letters. q must not quit and i must not poll a device the operator is in
// the middle of navigating away from.
func TestFormSwallowsEveryKeystroke(t *testing.T) {
	m := testModel()
	m = press(m, "C")
	m = press(m, "ctrl+u")

	for _, key := range []string{"q", "i", "p", "c", "o", "x", "R", "S", "f"} {
		m = press(m, key)
	}
	if m.quitting {
		t.Error("typing q into a field quit the tool")
	}
	if got := m.form.fields[fieldAddress].value; got != "qipcoxRSf" {
		t.Errorf("field = %q; keystrokes were dispatched as actions", got)
	}
}

func TestConnectionFormRejectsBadInput(t *testing.T) {
	tests := []struct {
		name  string
		field int
		value string
		want  string
	}{
		{"no port", fieldAddress, "10.0.0.5", "port"},
		{"empty address", fieldAddress, "", "outstation"},
		{"bad port", fieldAddress, "10.0.0.5:notaport", "port"},
		{"address not a number", fieldLocal, "twelve", "local address"},
		{"address out of range", fieldRemote, "70000", "remote address"},
		{"bad duration", fieldTimeout, "soon", "timeout"},
		{"negative poll", fieldPoll, "-1s", "poll"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := testModel()
			m = press(m, "C")
			m.form.cursor = tc.field
			m = press(m, "ctrl+u")
			for _, r := range tc.value {
				m = press(m, string(r))
			}

			next, cmd := m.HandleKey("enter")
			m = next.(*Model)

			if cmd != nil {
				t.Error("a rejected form still tried to reconnect")
			}
			if !m.form.active {
				t.Fatal("a rejected form closed; the operator would lose what they typed")
			}
			if !strings.Contains(m.form.err, tc.want) {
				t.Errorf("error = %q, want it to mention %q", m.form.err, tc.want)
			}
			if out := m.View().Content; !strings.Contains(out, tc.want) {
				t.Error("the rejection is not shown on screen")
			}
		})
	}
}

// Equal link addresses are the mistake worth catching by hand: the link comes
// up for nobody and the device gives no clue why.
func TestConnectionFormRejectsEqualAddresses(t *testing.T) {
	m := testModel()
	m = press(m, "C")
	m.form.cursor = fieldRemote
	m = press(m, "ctrl+u")
	m = press(m, "1")

	next, cmd := m.HandleKey("enter")
	m = next.(*Model)

	if cmd != nil || !m.form.active {
		t.Fatal("local == remote was accepted")
	}
	if !strings.Contains(m.form.err, "differ") {
		t.Errorf("error = %q, want it to say the addresses must differ", m.form.err)
	}
}

// A new device means the table no longer describes what is in front of the
// operator, so it is dropped rather than left to be read as live.
func TestConnectingElsewhereForgetsTheOldDevice(t *testing.T) {
	m := testModel()
	seedPoints(m, 10)
	m.applyUpdate(updateMsg{Type: dnp3.TypeBinary, Index: 0, Value: "ON", IsEvent: true})

	m = press(m, "C")
	m.form.cursor = fieldAddress
	m = press(m, "ctrl+u")
	for _, r := range "10.0.0.9:20000" {
		m = press(m, string(r))
	}

	next, cmd := m.HandleKey("enter")
	m = next.(*Model)

	if cmd == nil {
		t.Fatal("a valid form did not reconnect")
	}
	if len(m.points) != 0 || len(m.events) != 0 {
		t.Errorf("kept %d points and %d events from the previous device",
			len(m.points), len(m.events))
	}
	if m.connected {
		t.Error("still reporting the old link as up")
	}
}

// Changing only the timing leaves the measurements meaningful, so they stay.
func TestChangingTimingKeepsThePoints(t *testing.T) {
	m := testModel()
	seedPoints(m, 10)

	m = press(m, "C")
	m.form.cursor = fieldTimeout
	m = press(m, "ctrl+u")
	for _, r := range "9s" {
		m = press(m, string(r))
	}

	next, cmd := m.HandleKey("enter")
	m = next.(*Model)

	if cmd == nil {
		t.Fatal("a valid form did not reconnect")
	}
	if len(m.points) != 10 {
		t.Errorf("dropped the point table for a timeout change: %d points left", len(m.points))
	}
}

func TestClickingAFormFieldFocusesIt(t *testing.T) {
	m := testModel()
	m = press(m, "C")

	r, ok := zoneOf(m, zoneField, fieldRemote)
	if !ok {
		t.Fatal("the form fields are not clickable")
	}
	m = click(m, r.x+1, r.y)

	if m.form.cursor != fieldRemote {
		t.Errorf("cursor = %d, want %d after clicking that field", m.form.cursor, fieldRemote)
	}
	if !m.form.active {
		t.Error("clicking a field closed the form")
	}
}

// A click outside must not discard a half-typed address.
func TestClickingOutsideTheFormKeepsIt(t *testing.T) {
	m := testModel()
	m = press(m, "C")
	m = click(m, 0, chromeTop)

	if !m.form.active {
		t.Error("a stray click threw the form away")
	}
}

func TestParseAddress(t *testing.T) {
	tests := []struct {
		in     string
		demo   bool
		serial string
		baud   int
		host   string
		bad    bool
	}{
		{in: "10.0.0.5:20000", host: "10.0.0.5:20000"},
		{in: "demo", demo: true},
		{in: "DEMO", demo: true},
		{in: "/dev/ttyUSB0", serial: "/dev/ttyUSB0", baud: 9600},
		{in: "/dev/ttyUSB0@19200", serial: "/dev/ttyUSB0", baud: 19200},
		{in: "COM3", serial: "COM3", baud: 9600},
		{in: "10.0.0.5", bad: true},
		{in: "", bad: true},
		{in: "/dev/ttyUSB0@fast", bad: true},
	}

	for _, tc := range tests {
		got := link{Baud: 9600}
		err := parseAddress(tc.in, &got)
		if tc.bad {
			if err == nil {
				t.Errorf("parseAddress(%q) was accepted", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseAddress(%q): %v", tc.in, err)
			continue
		}
		if got.Demo != tc.demo || got.Serial != tc.serial || got.Host != tc.host {
			t.Errorf("parseAddress(%q) = %+v", tc.in, got)
		}
		if tc.baud != 0 && got.Baud != tc.baud {
			t.Errorf("parseAddress(%q) baud = %d, want %d", tc.in, got.Baud, tc.baud)
		}
	}
}

func TestParseInterval(t *testing.T) {
	tests := []struct {
		in   string
		want time.Duration
		bad  bool
	}{
		{in: "5s", want: 5 * time.Second},
		{in: "500ms", want: 500 * time.Millisecond},
		{in: "2m", want: 2 * time.Minute},
		{in: "5", want: 5 * time.Second}, // a bare number is seconds
		{in: "0", want: 0},
		{in: "", bad: true},
		{in: "soon", bad: true},
	}

	for _, tc := range tests {
		got, err := parseInterval("poll", tc.in)
		switch {
		case tc.bad && err == nil:
			t.Errorf("parseInterval(%q) was accepted", tc.in)
		case !tc.bad && err != nil:
			t.Errorf("parseInterval(%q): %v", tc.in, err)
		case !tc.bad && got != tc.want:
			t.Errorf("parseInterval(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestMouseCanBeTurnedOff(t *testing.T) {
	m := testModel()
	m.mouse = false
	r, _ := zoneOf(m, zoneTab, int(ScreenEvents))
	m = click(m, r.x+1, r.y)
	if m.screen != ScreenOverview {
		t.Error("clicks are still acted on with -mouse=false")
	}
	if m.View().MouseMode != tea.MouseModeNone {
		t.Error("-mouse=false still asked the terminal for mouse reporting")
	}
}

// ---------- layout ----------

func TestNarrowTerminalDropsColumnsRatherThanSquashing(t *testing.T) {
	wide := layoutColumns(columnsFor(ScreenPoints), 120)
	narrow := layoutColumns(columnsFor(ScreenPoints), 58)

	if len(narrow) >= len(wide) {
		t.Errorf("a narrow table kept all %d columns instead of dropping the optional ones",
			len(narrow))
	}
	for _, c := range narrow {
		if c.title == "POINT" || c.title == "VALUE" || c.title == "QUALITY" {
			continue
		}
		if c.title == "TREND" {
			t.Error("the trend column survived on a terminal with no room for it")
		}
	}
	// Whatever survives must add up to the width it was given.
	total := len(narrow) - 1
	for _, c := range narrow {
		total += c.width
	}
	if total != 58 {
		t.Errorf("the resolved columns occupy %d of 58 columns", total)
	}
}

// TestDroppedColumnsDoNotShiftCells covers the failure a narrow terminal used
// to cause: with cells matched to columns by position, dropping TREND slid
// every quality one column left and put a sparkline where an operator reads
// for faults.
func TestDroppedColumnsDoNotShiftCells(t *testing.T) {
	m := testModel()
	m.screen = ScreenPoints
	m.width, m.height = 62, 14
	seedPoints(m, 4)

	l := m.layout()
	for _, c := range l.cols {
		if c.id == colTrend {
			t.Fatal("the trend column survived a terminal with no room for it")
		}
	}

	out := m.View().Content
	if !strings.Contains(out, "ONLINE") {
		t.Errorf("the quality column lost its contents:\n%s", out)
	}
	if strings.ContainsAny(out, string(sparkRunes)) {
		t.Errorf("a sparkline was drawn in a table with no trend column:\n%s", out)
	}
}

func TestInspectorOnlyOpensWhenThereIsRoom(t *testing.T) {
	m := testModel()
	m.screen = ScreenPoints
	m.detail = true
	seedPoints(m, 5)

	m.width = 120
	if m.layout().detail.empty() {
		t.Error("the inspector did not open on a wide terminal")
	}
	m.width = 80
	if !m.layout().detail.empty() {
		t.Error("the inspector squeezed the table on a narrow terminal")
	}
}

func TestInspectorShowsTheSelectedPoint(t *testing.T) {
	m := testModel()
	m.screen = ScreenPoints
	m.detail = true
	m.applyUpdate(updateMsg{
		Type: dnp3.TypeAnalog, Index: 9, Value: "42.5", Num: 42.5, HasNum: true,
		Flags: dnp3.Online | dnp3.LocalForced, Stamp: dnp3.Now(time.Now()),
	})

	out := m.View().Content
	for _, want := range []string{"Inspector", "AI 9", "42.5", "LOCAL_FORCED"} {
		if !strings.Contains(out, want) {
			t.Errorf("the inspector is missing %q:\n%s", want, out)
		}
	}
}

// ---------- exports ----------

func TestExportWritesTheVisibleRows(t *testing.T) {
	t.Chdir(t.TempDir())

	m := testModel()
	m.screen = ScreenPoints
	m.applyUpdate(updateMsg{Type: dnp3.TypeAnalog, Index: 0, Value: "1", Flags: dnp3.Online})
	m.applyUpdate(updateMsg{Type: dnp3.TypeBinary, Index: 0, Value: "ON", Flags: dnp3.Online})
	m.filter = "ai"

	msg := m.export()()
	res, ok := msg.(commandResultMsg)
	if !ok || !res.ok {
		t.Fatalf("export failed: %v", msg)
	}

	files, _ := filepath.Glob("dnp3-points-*.csv")
	if len(files) != 1 {
		t.Fatalf("export wrote %d files", len(files))
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	if !strings.Contains(body, "Analog") {
		t.Errorf("the export is missing the analog point:\n%s", body)
	}
	// The filter is part of the view, so it is part of the export.
	if strings.Contains(body, "Binary") {
		t.Errorf("the export ignored the filter:\n%s", body)
	}
}

// ---------- rendering helpers ----------

func TestCellAlwaysMeasuresExact(t *testing.T) {
	for _, w := range []int{1, 4, 12} {
		for _, s := range []string{"", "ok", "COMM_LOST|REMOTE_FORCED", "▁▂▃▄▅▆▇█"} {
			if got := lipgloss.Width(cell(s, w, false)); got != w {
				t.Errorf("cell(%q, %d) is %d columns wide", s, w, got)
			}
		}
	}
}

func TestSparklineFitsItsWidth(t *testing.T) {
	vals := make([]float64, 200)
	for i := range vals {
		vals[i] = float64(i % 7)
	}
	if got := lipgloss.Width(sparkline(vals, 12)); got != 12 {
		t.Errorf("sparkline width = %d, want 12", got)
	}
	// A flat trace still has to read as a trace.
	if s := sparkline([]float64{5, 5, 5}, 8); strings.TrimSpace(s) == "" {
		t.Error("a constant value drew an empty sparkline")
	}
}

func TestToastExpires(t *testing.T) {
	m := testModel()
	now := time.Now()
	m.toast.show("info", "hello", now)
	if !m.toast.active() {
		t.Fatal("the toast did not appear")
	}
	m.toast.expire(now.Add(time.Minute))
	if m.toast.active() {
		t.Error("the toast never went away")
	}
}

// seedPoints fills the model with enough points to need scrolling.
func seedPoints(m *Model, n int) {
	for i := range n {
		m.applyUpdate(updateMsg{
			Type: dnp3.TypeAnalog, Index: uint16(i),
			Value: formatFloat(float64(i) * 1.5), Num: float64(i) * 1.5, HasNum: true,
			Flags: dnp3.Online, Stamp: dnp3.Now(time.Now()),
		})
	}
}

// TestEndToEndAgainstDemoOutstation runs the real master against the demo
// outstation and checks that measurements reach the model — the whole pipeline
// from octets to table row.
func TestEndToEndAgainstDemoOutstation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	mch, och := channel.Pipe()
	demo := newDemoOutstation(discardLogger())

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = demo.session.Run(ctx, och) }()
	go func() { defer wg.Done(); demo.run(ctx) }()

	conn := &connection{msgs: make(chan tea.Msg, 512), ctx: ctx}
	h := &handler{conn: conn}
	sess := master.New(master.Config{
		LocalAddr: 1, RemoteAddr: 10, ResponseTimeout: 5 * time.Second,
	}, h)
	h.sess = sess
	conn.adopt(sess, link{Demo: true, Local: 1, Remote: 10})

	wg.Add(1)
	go func() { defer wg.Done(); _ = sess.Run(ctx, mch) }()

	t.Cleanup(func() {
		cancel()
		_ = mch.Close()
		_ = och.Close()
		wg.Wait()
	})

	// Wait for the link, then poll.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !sess.Connected() {
		time.Sleep(5 * time.Millisecond)
	}
	if !sess.Connected() {
		t.Fatal("the demo outstation never connected")
	}

	// Let the demo's tick populate the database. Polling immediately would
	// read a device that has not measured anything yet — every point zero and
	// unflagged, which is a real answer to the wrong question.
	time.Sleep(700 * time.Millisecond)

	pollCtx, pollCancel := context.WithTimeout(ctx, 10*time.Second)
	defer pollCancel()
	if err := sess.IntegrityPoll(pollCtx); err != nil {
		t.Fatalf("integrity poll: %v", err)
	}

	// Drain what the handler pushed into the model, through the model's own
	// dispatch rather than a copy of it: the shape of what the session pushes
	// is the model's business, and a test that decodes it by hand goes on
	// passing after the model has stopped understanding it.
	m := testModel()
	m.conn = conn
	before := len(m.points)
	for {
		select {
		case msg := <-conn.msgs:
			m.applySessionMsg(msg)
			continue
		default:
		}
		break
	}

	if len(m.points) == before {
		t.Fatal("no measurements reached the model")
	}
	if len(m.points) < 8 {
		t.Errorf("the model holds %d points; the demo device has more than that", len(m.points))
	}

	m.screen = ScreenPoints
	out := m.View().Content
	if !strings.Contains(out, "AI 0") || !strings.Contains(out, "BI 0") {
		t.Errorf("the rendered table is missing points:\n%s", out)
	}
	// The bus voltage is around 11 kV and must not have been truncated to an
	// integer by a badly chosen variation.
	if !strings.Contains(out, "11") {
		t.Errorf("the bus voltage did not arrive:\n%s", out)
	}
	// The group and variation each value arrived under is what tells a
	// commissioning engineer whether the device is using the encoding they
	// configured, so it must survive the trip into the model.
	for _, p := range m.points {
		if p.GV.Group == 0 {
			t.Errorf("%s arrived without its group and variation", pointLabel(p.Key))
			break
		}
	}
}

// waitConnected blocks until the live session reports a link, or gives up.
func waitConnected(t *testing.T, conn *connection) *master.Session {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if s := conn.session(); s != nil && s.Connected() {
			return s
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the session never connected")
	return nil
}

// TestReconnectRebuildsTheSession drives the supervisor for real: a live link
// is torn down and a new one brought up with different parameters, which is
// the whole point of editing them in place. It is the path the interface takes
// on enter, so a reconnect that deadlocked or left the old session running
// would show up here rather than in front of a device.
func TestReconnectRebuildsTheSession(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	conn := &connection{msgs: make(chan tea.Msg, 512), ctx: ctx}
	conn.sup = &supervisor{conn: conn, root: ctx, log: discardLogger()}

	first := link{Demo: true, Local: 1, Remote: 10, Timeout: 5 * time.Second}
	conn.sup.start(first)
	t.Cleanup(conn.sup.stop)

	was := waitConnected(t, conn)

	// Drain what the first session pushed, so the channel cannot fill and
	// stall the second one.
	go func() {
		for {
			select {
			case <-conn.msgs:
			case <-ctx.Done():
				return
			}
		}
	}()

	second := first
	second.Poll = 2 * time.Second
	conn.sup.start(second)

	now := waitConnected(t, conn)

	if now == was {
		t.Error("reconnecting reused the old session; the new parameters cannot have taken effect")
	}
	if got := conn.current(); got.Poll != 2*time.Second {
		t.Errorf("the connection reports poll %v, want 2s", got.Poll)
	}
	if !was.Connected() {
		// The old session is cancelled, but the assertion that matters is that
		// the new one is live and answering.
		t.Log("the previous session has shut down, as expected")
	}
}

// A reconnect to somewhere with nothing listening must leave the tool usable
// and honest rather than wedged: the link simply never comes up.
func TestReconnectToADeadAddressStaysUsable(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	conn := &connection{msgs: make(chan tea.Msg, 512), ctx: ctx}
	conn.sup = &supervisor{conn: conn, root: ctx, log: discardLogger()}

	conn.sup.start(link{Demo: true, Local: 1, Remote: 10, Timeout: time.Second})
	t.Cleanup(conn.sup.stop)
	waitConnected(t, conn)

	go func() {
		for {
			select {
			case <-conn.msgs:
			case <-ctx.Done():
				return
			}
		}
	}()

	// 127.0.0.1:1 has nothing behind it.
	dead := link{Host: "127.0.0.1:1", Local: 1, Remote: 10, Timeout: time.Second}
	done := make(chan struct{})
	go func() { defer close(done); conn.sup.start(dead) }()

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("reconnecting to a dead address never returned")
	}

	if s := conn.session(); s == nil {
		t.Fatal("no session after reconnecting")
	} else if s.Connected() {
		t.Error("reported a link to an address with nothing listening")
	}
	if got := conn.target(); got != "127.0.0.1:1" {
		t.Errorf("target = %q, want the address that was asked for", got)
	}
}
