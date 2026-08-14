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
	"encoding/csv"
	"fmt"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

// Export exists because the answer to "what is this device reporting" usually
// has to leave the terminal: it goes into a commissioning report, an email to
// the vendor, or a diff against yesterday. Writing what is on screen — after
// the filter and the sort, not before — means the operator exports the view
// they were looking at rather than something they have to reconstruct.

func (m *Model) export() tea.Cmd {
	var name string
	var rows [][]string
	stamp := time.Now().Format("20060102-150405")

	switch m.screen {
	case ScreenPoints, ScreenOverview:
		name = "dnp3-points-" + stamp + ".csv"
		rows = append(rows, []string{"type", "index", "value", "quality",
			"timestamp", "timestamp_quality", "received", "updates", "events", "source"})
		for _, p := range m.visiblePoints() {
			rows = append(rows, []string{
				p.Key.Type.String(), fmt.Sprint(p.Key.Index), p.Value,
				qualityText(p.Flags, p.Key.Type),
				stampCSV(p), p.Time.Quality.String(),
				p.Updated.Format(time.RFC3339Nano),
				fmt.Sprint(p.Updates), fmt.Sprint(p.Events), p.GV.String(),
			})
		}

	case ScreenEvents:
		name = "dnp3-events-" + stamp + ".csv"
		rows = append(rows, []string{"received", "type", "index", "value",
			"quality", "timestamp", "class", "source"})
		for _, e := range m.visibleEvents() {
			ts := ""
			if e.Stamp.IsValid() {
				ts = e.Stamp.Time.Format(time.RFC3339Nano)
			}
			rows = append(rows, []string{
				e.At.Format(time.RFC3339Nano), e.Key.Type.String(), fmt.Sprint(e.Key.Index),
				e.Value, qualityText(e.Flags, e.Key.Type), ts,
				classText(e.Class), e.GV.String(),
			})
		}

	case ScreenLog:
		name = "dnp3-log-" + stamp + ".csv"
		rows = append(rows, []string{"time", "level", "message"})
		for _, l := range m.visibleLogs() {
			rows = append(rows, []string{l.At.Format(time.RFC3339Nano), l.Level, l.Text})
		}

	default:
		return func() tea.Msg {
			return commandResultMsg{text: "nothing to export from this screen"}
		}
	}

	if len(rows) == 1 {
		return func() tea.Msg {
			return commandResultMsg{text: "nothing to export — the list is empty"}
		}
	}

	return func() tea.Msg {
		var b strings.Builder
		w := csv.NewWriter(&b)
		if err := w.WriteAll(rows); err != nil {
			return commandResultMsg{text: "export failed: " + err.Error()}
		}
		if err := os.WriteFile(name, []byte(b.String()), 0o644); err != nil {
			return commandResultMsg{text: "export failed: " + err.Error()}
		}
		return commandResultMsg{
			text: fmt.Sprintf("wrote %d rows to %s", len(rows)-1, name),
			ok:   true,
		}
	}
}

func stampCSV(p *pointState) string {
	if !p.Time.IsValid() {
		return ""
	}
	return p.Time.Time.Format(time.RFC3339Nano)
}
