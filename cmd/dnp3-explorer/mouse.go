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

import tea "charm.land/bubbletea/v2"

// The pointer never gets its own copy of any action.
//
// Every click resolves to a region from [Model.layout] and then either moves
// the cursor or presses the key the keyboard would have pressed. That is what
// keeps the two input methods from drifting: there is one implementation of
// "close this breaker", and the mouse is a second way of reaching it rather
// than a second version of it.

type mouseKind int

const (
	mouseClick mouseKind = iota
	mouseRelease
	mouseWheel
	mouseMotion
)

// mouseEvent is Bubble Tea's four mouse messages flattened into one shape, so
// the handler below can be driven from a test without constructing terminal
// escape sequences.
type mouseEvent struct {
	x, y   int
	button tea.MouseButton
	kind   mouseKind
}

// wheelStep is how far one notch scrolls. Three rows is the convention
// everywhere else, and a table that scrolls a whole page per notch is a table
// nobody can aim.
const wheelStep = 3

func (m *Model) HandleMouse(e mouseEvent) (tea.Model, tea.Cmd) {
	if !m.mouse {
		return m, nil
	}
	l := m.layout()
	if !l.ok {
		return m, nil
	}

	switch e.kind {
	case mouseWheel:
		return m.handleWheel(e)
	case mouseMotion:
		return m.handleMotion(e, l)
	case mouseRelease:
		m.dragging = false
		return m, nil
	}

	// A prompt owns the pointer the same way it owns the keyboard. Routing a
	// click through HandleKey while one is open would type the button's key
	// into the prompt — clicking "Integrity" mid-filter would add an "i" to the
	// filter — and on the Points screen it would reach the control dialog. So
	// the click closes the prompt and stops there: the click that dismisses is
	// never also the click that acts.
	if m.prompt.active {
		m.closePrompt()
		return m, nil
	}

	// The editor takes the pointer the way it takes the keyboard: a click
	// picks a field, the footer buttons still work, and a click outside is
	// left alone. Dismissing on a stray click would throw away a half-typed
	// address, which is the opposite of what a form is for.
	if m.form.active {
		switch z, ok := l.zoneAt(e.x, e.y); {
		case !ok:
			return m, nil
		case z.kind == zoneField && z.n < len(m.form.fields):
			m.form.cursor = z.n
		case z.kind == zoneButton && z.n < len(l.buttons):
			return m.HandleKey(l.buttons[z.n].key)
		}
		return m, nil
	}

	// A dialog is modal for the pointer too: clicking its choices works, and
	// clicking anywhere else dismisses it rather than reaching the table
	// underneath.
	if m.modal.kind != modalNone {
		if z, ok := l.zoneAt(e.x, e.y); ok && z.kind == zoneChoice && z.n < len(l.choices) {
			return m.handleModalKey(l.choices[z.n].key)
		}
		if !l.modal.contains(e.x, e.y) {
			m.modal = modalState{}
		}
		return m, nil
	}

	z, ok := l.zoneAt(e.x, e.y)
	if !ok {
		return m, nil
	}

	switch z.kind {
	case zoneTab:
		m.setScreen(Screen(z.n))

	case zoneButton:
		if z.n < len(l.buttons) {
			return m.HandleKey(l.buttons[z.n].key)
		}

	case zoneColumn:
		if z.n < len(l.cols) {
			return m.sortByColumn(l.cols[z.n].key)
		}

	case zoneScroll:
		m.dragging = true
		m.scrollToTrack(e.y, l)

	case zoneRows:
		row, inList := l.rowAt(e.y)
		if !inList {
			return m, nil
		}
		return m.clickRow(row, e.button)

	case zoneDetail:
		// Clicking the inspector when nothing is selected is a request to see
		// it; clicking it again is how it goes away.
		if e.button == tea.MouseRight {
			m.detail = false
		}
	}
	return m, nil
}

func (m *Model) handleWheel(e mouseEvent) (tea.Model, tea.Cmd) {
	// The wheel over the tab bar walks the tabs, which is what a browser does
	// and what people try first.
	if e.y <= rowTabs {
		switch e.button {
		case tea.MouseWheelUp:
			m.setScreen((m.screen + numScreens - 1) % numScreens)
		case tea.MouseWheelDown:
			m.setScreen((m.screen + 1) % numScreens)
		}
		return m, nil
	}
	if !m.screen.scrolls() {
		return m, nil
	}
	switch e.button {
	case tea.MouseWheelUp:
		m.scroll(-wheelStep)
	case tea.MouseWheelDown:
		m.scroll(wheelStep)
	}
	return m, nil
}

func (m *Model) handleMotion(e mouseEvent, l layout) (tea.Model, tea.Cmd) {
	if m.dragging && e.button == tea.MouseLeft {
		m.scrollToTrack(e.y, l)
		return m, nil
	}
	// Hover is only tracked for things that light up. Anything else would
	// repaint the screen for every pixel of travel and buy nothing.
	if z, ok := l.zoneAt(e.x, e.y); ok && (z.kind == zoneTab || z.kind == zoneButton ||
		z.kind == zoneChoice || z.kind == zoneColumn) {
		m.hover = z
	} else {
		m.hover = zone{}
	}
	return m, nil
}

// scrollToTrack maps a position on the scrollbar to a position in the list.
func (m *Model) scrollToTrack(y int, l layout) {
	if l.scroll.empty() || l.total <= l.rows.h {
		return
	}
	span := max(l.scroll.h-1, 1)
	rel := min(max(y-l.scroll.y, 0), span)
	off := rel * (l.total - l.rows.h) / span

	if m.follow && m.screen.follows() {
		m.follow = false
	}
	m.offset[m.screen] = off
	m.cursor[m.screen] = min(max(m.cursor[m.screen], off), off+max(l.rows.h-1, 0))
}

// clickRow selects a row, and acts on it when it was already selected.
//
// Selecting first and acting second is deliberate: on a screen that can trip a
// breaker, a single click must never be the whole gesture. Clicking a row that
// is already under the cursor is the second half of it, and it opens the
// control dialog rather than operating anything.
func (m *Model) clickRow(row int, button tea.MouseButton) (tea.Model, tea.Cmd) {
	already := m.cursor[m.screen] == row

	if m.follow && m.screen.follows() {
		m.follow = false
	}
	m.cursor[m.screen] = row

	switch {
	case button == tea.MouseRight:
		m.detail = true
	case already && m.screen == ScreenPoints:
		return m.contextAction("enter")
	}
	return m, nil
}

// sortByColumn sorts by a clicked column, reversing when it is already the
// sort column.
func (m *Model) sortByColumn(key sortKey) (tea.Model, tea.Cmd) {
	if key == sortNone {
		return m, nil
	}
	if m.sortBy == key {
		m.sortDesc = !m.sortDesc
	} else {
		m.sortBy, m.sortDesc = key, false
	}
	m.cursor[m.screen], m.offset[m.screen] = 0, 0
	return m, nil
}
