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
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/dscsystems/go-dnp3/master"
)

// A form is several labelled fields edited together, which the single-line
// prompt cannot express: the connection parameters only make sense as a set,
// because an address and the link addresses have to change together to reach a
// different device and applying them one at a time would reconnect three times
// on the way to somewhere the operator never meant to go.

// formIndex names the connection fields by what they are rather than by
// position, so reading a value back cannot silently pick up its neighbour.
const (
	fieldAddress = iota
	fieldLocal
	fieldRemote
	fieldTimeout
	fieldPoll
	numFields
)

type formField struct {
	label string
	value string
	hint  string
}

type formState struct {
	active bool
	title  string
	fields []formField
	cursor int
	// offset scrolls the field list on a terminal too short to show it whole.
	offset int
	// err is the last rejection, kept on screen so the operator can see what
	// was wrong with what they typed while they retype it.
	err string
}

func (f *formState) focused() *formField {
	if f.cursor < 0 || f.cursor >= len(f.fields) {
		return nil
	}
	return &f.fields[f.cursor]
}

func (f *formState) move(delta int) {
	if len(f.fields) == 0 {
		return
	}
	// Wrapping, because a five-field form is a loop in the operator's head and
	// stopping dead at the last field reads as the key having failed.
	f.cursor = (f.cursor + delta + len(f.fields)) % len(f.fields)
}

// openConnectionForm fills the editor from the live connection.
func (m *Model) openConnectionForm() {
	l := m.conn.current()
	m.form = formState{
		active: true,
		title:  "Connection",
		fields: []formField{
			{label: "Outstation", value: l.address(),
				hint: "host:port, /dev/ttyUSB0@9600, or demo"},
			{label: "Local address", value: itoa(int(l.Local)),
				hint: "this master's link address"},
			{label: "Remote address", value: itoa(int(l.Remote)),
				hint: "the outstation's link address"},
			{label: "Response timeout", value: l.Timeout.String(),
				hint: "how long to wait for an answer"},
			{label: "Poll interval", value: l.Poll.String(),
				hint: "0 disables the periodic class poll"},
		},
	}
}

func (m *Model) handleFormKey(key string) (tea.Model, tea.Cmd) {
	f := &m.form

	switch key {
	case "esc":
		m.form = formState{}
		return m, nil

	case "enter":
		return m.submitForm()

	// Only the arrows and tab move between fields. j and k are letters here,
	// and a form that ate them could not be given a serial device to talk to.
	case "up", "shift+tab":
		f.move(-1)
	case "down", "tab":
		f.move(1)

	case "backspace":
		if fld := f.focused(); fld != nil && fld.value != "" {
			r := []rune(fld.value)
			fld.value = string(r[:len(r)-1])
		}
	case "ctrl+u":
		if fld := f.focused(); fld != nil {
			fld.value = ""
		}
	case "space":
		if fld := f.focused(); fld != nil {
			fld.value += " "
		}
	default:
		if fld := f.focused(); fld != nil && len([]rune(key)) == 1 {
			fld.value += key
		}
	}
	return m, nil
}

// submitForm validates every field before changing anything.
//
// Nothing is applied until all of it parses, because a reconnect that took the
// new address and kept the old link address would be a third device that the
// operator never asked to talk to.
func (m *Model) submitForm() (tea.Model, tea.Cmd) {
	p, err := parseForm(m.conn.current(), m.form.fields)
	if err != nil {
		m.form.err = err.Error()
		return m, nil
	}

	was := m.conn.current()
	m.form = formState{}

	if was.sameDevice(p) {
		// Only the timing changed, so what is on screen still describes the
		// device in front of the operator and is worth keeping.
		m.addLog("info", "reconnecting to "+p.target())
	} else {
		// A different device, so the old measurements are not this device's.
		// Keeping them would leave an operator reading values that were never
		// polled from the thing they are now connected to.
		m.forgetDevice()
		m.addLog("info", "connecting to "+p.target()+" as master "+
			itoa(int(p.Local))+" → outstation "+itoa(int(p.Remote)))
	}

	m.status, m.connected, m.lastErr = "connecting", false, ""
	m.linkSince = time.Time{}
	m.toast.show("info", "connecting to "+p.target(), m.now)
	return m, m.conn.reconnect(p)
}

// forgetDevice drops everything that described the previous outstation.
func (m *Model) forgetDevice() {
	m.points = map[pointKey]*pointState{}
	m.pointsOrder = nil
	m.events = nil
	m.iin = ""
	m.stats = master.Stats{}
	for i := range m.cursor {
		m.cursor[i], m.offset[i] = 0, 0
	}
}

// parseForm reads the editor back into connection parameters.
func parseForm(base link, fields []formField) (link, error) {
	if len(fields) < numFields {
		return link{}, fmt.Errorf("connection: the form is incomplete")
	}
	out := base

	if err := parseAddress(fields[fieldAddress].value, &out); err != nil {
		return link{}, err
	}
	local, err := parseLinkAddr("local address", fields[fieldLocal].value)
	if err != nil {
		return link{}, err
	}
	remote, err := parseLinkAddr("remote address", fields[fieldRemote].value)
	if err != nil {
		return link{}, err
	}
	timeout, err := parseInterval("timeout", fields[fieldTimeout].value)
	if err != nil {
		return link{}, err
	}
	poll, err := parseInterval("poll", fields[fieldPoll].value)
	if err != nil {
		return link{}, err
	}

	out.Local, out.Remote, out.Timeout, out.Poll = local, remote, timeout, poll
	if err := out.validate(); err != nil {
		return link{}, err
	}
	return out, nil
}

func itoa(n int) string { return strconv.Itoa(n) }
