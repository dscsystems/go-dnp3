package main

import (
	"fmt"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/dscsystems/go-dnp3/secevent"
)

// The Security screen lists the session's security events: what the master
// saw that looked wrong — a restart, a refused request, an unsolicited
// response it had switched off, a malformed or out-of-sequence response.
//
// The session's dispatcher calls the sink on its own goroutine, so events
// cross into the UI as messages like everything else, and are dropped rather
// than block it.

// secEventMsg is one security event from the session.
type secEventMsg secevent.Event

// securityPolicy is the policy every session the explorer builds reports
// through. It only reports: a browsing tool has no business refusing what
// the device sends.
func securityPolicy(c *connection) *secevent.Policy {
	return &secevent.Policy{
		Sink: secevent.SinkFunc(func(e secevent.Event) { c.push(secEventMsg(e)) }),
		// The explorer reconnects to the same device over and over; the
		// process-wide MAC history would report its own reconnects.
		DisableMACLookup: true,
	}
}

// secRow is one line of the Security screen.
type secRow struct {
	At       time.Time
	Kind     secevent.Kind
	Severity secevent.Severity
	Action   secevent.Action
	Count    int
	Text     string
}

// maxSecRows bounds the Security screen like the log.
const maxSecRows = 2000

func (m *Model) addSecEvent(e secevent.Event) {
	text := e.Detail
	if text == "" {
		text = string(e.Kind)
	}
	if e.Count > 1 {
		text += fmt.Sprintf(" (×%d)", e.Count)
	}
	if e.Action != secevent.Observed {
		text += " [" + e.Action.String() + "]"
	}
	at := e.Time
	if at.IsZero() {
		at = time.Now()
	}
	m.security = append(m.security, secRow{At: at, Kind: e.Kind, Severity: e.Severity,
		Action: e.Action, Count: e.Count, Text: text})
	if e.Severity >= secevent.Warning {
		m.secAlerts++
	}
	if len(m.security) > maxSecRows {
		drop := len(m.security) - maxSecRows
		m.security = m.security[drop:]
		m.cursor[ScreenSecurity] = max(m.cursor[ScreenSecurity]-drop, 0)
		m.offset[ScreenSecurity] = max(m.offset[ScreenSecurity]-drop, 0)
	}
}

// visibleSecurity returns the security rows after filtering.
func (m *Model) visibleSecurity() []secRow {
	if m.filter == "" {
		return m.security
	}
	out := make([]secRow, 0, len(m.security))
	for _, r := range m.security {
		if matchesFilter(m.filter, string(r.Kind), r.Severity.String(), r.Text) {
			out = append(out, r)
		}
	}
	return out
}

func (m *Model) viewSecurity(l layout) []string {
	rows := m.visibleSecurity()
	empty := "no security events — nothing unusual seen from this device"
	if m.filter != "" {
		empty = fmt.Sprintf("no security events match %q", m.filter)
	}
	return m.renderTable(l, empty, func(i int) tableRow {
		e := rows[i]
		r := tableRow{
			cells: map[colID]string{
				colReceived: e.At.Format("15:04:05.000"),
				colLevel:    strings.ToUpper(e.Severity.String()),
				colKind:     string(e.Kind),
				colMessage:  e.Text,
			},
			cellStyle: map[colID]lipgloss.Style{colReceived: stMuted},
		}
		switch {
		case e.Severity >= secevent.Alert:
			r.line, r.lineSet = stBad, true
		case e.Severity == secevent.Warning:
			r.line, r.lineSet = stWarn, true
		case e.Severity == secevent.Info:
			r.cellStyle[colLevel] = stMuted
		}
		return r
	})
}

// securityText summarises the Security screen for the overview.
func (m *Model) securityText() string {
	switch {
	case len(m.security) == 0:
		return stMuted.Render("no events")
	case m.secAlerts == 0:
		return fmt.Sprintf("%d events", len(m.security))
	}
	return fmt.Sprintf("%d events · %s", len(m.security),
		stWarn.Render(fmt.Sprintf("%d warning+", m.secAlerts)))
}
