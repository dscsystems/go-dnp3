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
	"errors"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/dscsystems/go-dnp3"
)

// Device attributes answer the question this whole tool is named for. Pointed
// at an unfamiliar panel, the first thing anyone wants is not a point value
// but "what is this thing?" — and group 0 is the device's own answer, read out
// of it rather than off a drawing that may describe the one it replaced.
//
// So the read happens once, automatically, when the link comes up. A device
// that does not implement attributes says so and is not asked again until
// somebody presses the key: an operator who has just connected should not have
// to know that a feature exists in order to find out that this device lacks
// it.

// deviceState is what the outstation says about itself.
type deviceState struct {
	attrs []dnp3.Attribute

	// asked records that a read has been attempted on this connection, so the
	// automatic one happens once rather than on every status message.
	asked bool
	busy  bool

	// unsupported marks a device that has told us it does not implement
	// attributes, which is a fact worth showing rather than an error to bury
	// in the log.
	unsupported bool
	err         string
}

// attributesMsg carries the answer back to the UI.
type attributesMsg struct {
	attrs       []dnp3.Attribute
	unsupported bool
	err         string
}

// readAttributes asks the device what it is.
func (m *Model) readAttributes() tea.Cmd {
	m.device.busy = true
	m.device.asked = true

	conn := m.conn
	return func() tea.Msg {
		sess := conn.session()
		if sess == nil {
			return attributesMsg{err: "not connected"}
		}
		ctx, cancel := context.WithTimeout(conn.ctx, requestTimeout)
		defer cancel()

		attrs, err := sess.ReadAttributes(ctx)
		switch {
		case errors.Is(err, dnp3.ErrNotSupported):
			return attributesMsg{unsupported: true}
		case err != nil:
			return attributesMsg{err: err.Error()}
		}
		return attributesMsg{attrs: attrs}
	}
}

func (m *Model) applyAttributes(msg attributesMsg) {
	m.device.busy = false
	m.device.err = msg.err
	m.device.unsupported = msg.unsupported

	switch {
	case msg.unsupported:
		m.addLog("info", "the outstation reports no device attributes")
		return
	case msg.err != "":
		m.addLog("warn", "device attributes: "+msg.err)
		return
	}

	// Sorted so the panel does not reorder itself between reads, and
	// descending because the standard numbers the identity of a device — its
	// vendor, model, version, serial — above its capability counts. On a panel
	// that has to be truncated, the nameplate is what survives.
	attrs := slices.Clone(msg.attrs)
	slices.SortFunc(attrs, func(a, b dnp3.Attribute) int {
		if a.Set != b.Set {
			return int(a.Set) - int(b.Set)
		}
		if pa, pb := identityOrder(a.Variation), identityOrder(b.Variation); pa != pb {
			return pa - pb
		}
		return int(b.Variation) - int(a.Variation)
	})
	m.device.attrs = attrs

	m.addLog("ok", "read "+plural(len(attrs), "device attribute"))
	if name, ok := m.deviceName(); ok {
		m.toast.show("ok", name, m.now)
	}
}

// identityOrder puts the nameplate first, in the order somebody reads it off
// a label on the front of the box. Everything else keeps its numeric order
// behind them.
func identityOrder(variation uint8) int {
	for i, v := range []uint8{
		attrVendorName, attrProductName, attrSoftwareVersion, attrSerialNumber,
	} {
		if v == variation {
			return i
		}
	}
	return len(nameplate)
}

// nameplate is the identity, in reading order.
var nameplate = []uint8{
	attrVendorName, attrProductName, attrSoftwareVersion, attrSerialNumber,
}

// deviceName is the most identifying thing the device said about itself, for
// the header and the toast: a model beats a vendor, and either beats nothing.
func (m *Model) deviceName() (string, bool) {
	var vendor, model, serial string
	for _, a := range m.device.attrs {
		if a.Set != dnp3.AttrSetStandard {
			continue
		}
		switch a.Variation {
		case attrProductName:
			model = a.Value()
		case attrVendorName:
			vendor = a.Value()
		case attrSerialNumber:
			serial = a.Value()
		}
	}

	switch {
	case model != "" && vendor != "":
		name := vendor + " " + model
		if serial != "" {
			name += " (" + serial + ")"
		}
		return name, true
	case model != "":
		return model, true
	case vendor != "":
		return vendor, true
	default:
		return "", false
	}
}

// Variations the panel treats specially. The rest are shown as the device
// numbered them.
const (
	attrSoftwareVersion uint8 = 242
	attrSerialNumber    uint8 = 248
	attrProductName     uint8 = 250
	attrVendorName      uint8 = 252
)

// overviewDevice is the Device panel: what the outstation says it is.
//
// The identity goes one attribute to a row, because that is what somebody
// reads. The capability numbers are summarised into two rows instead: a panel
// listing "number of binary inputs" and "max binary input index" separately
// spends eight rows saying what one row says better, and pushes the nameplate
// off the screen to do it.
func (m *Model) overviewDevice(rows int) []string {
	switch {
	case m.device.busy:
		return []string{stMuted.Render("  reading…")}

	case m.device.unsupported:
		return []string{
			"  " + stMuted.Render("this device reports no attributes"),
			"  " + stMuted.Render("(group 0 not implemented)"),
		}

	case m.device.err != "":
		return []string{
			"  " + stWarn.Render(truncate(m.device.err, max(m.width/2-6, 20))),
			"  " + stMuted.Render("press a to try again"),
		}

	case len(m.device.attrs) == 0:
		return []string{"  " + stMuted.Render("press a to read the device attributes")}
	}

	var (
		out    []string
		counts []string
		frags  []string
	)

	for _, a := range m.device.attrs {
		switch {
		case a.Set == dnp3.AttrSetStandard && pointCountLabels[a.Variation] != "":
			counts = append(counts, a.Value()+" "+pointCountLabels[a.Variation])
		case a.Set == dnp3.AttrSetStandard && a.Variation == attrMaxTxFragment:
			frags = append(frags, a.Value()+" tx")
		case a.Set == dnp3.AttrSetStandard && a.Variation == attrMaxRxFragment:
			frags = append(frags, a.Value()+" rx")
		default:
			out = append(out, field(attributeLabel(a), a.Value()))
		}
	}

	if len(counts) > 0 {
		out = append(out, field("points", strings.Join(counts, " · ")))
	}
	if len(frags) > 0 {
		out = append(out, field("fragments", strings.Join(frags, " · ")))
	}

	if rows > 0 && len(out) > rows {
		hidden := len(out) - rows + 1
		out = out[:rows-1]
		out = append(out, "  "+stMuted.Render("…and "+plural(hidden, "more")))
	}
	return out
}

// pointCountLabels are the attributes that become the one "points" row, in the
// shorthand a panel has room for.
var pointCountLabels = map[uint8]string{
	226: "BI",
	223: "DBBI",
	216: "CT",
	220: "AI",
	211: "BO",
	208: "AO",
}

// The fragment sizes, which become their own row.
const (
	attrMaxTxFragment uint8 = 227
	attrMaxRxFragment uint8 = 228
)

// attributeLabel is what the panel calls an attribute.
//
// The library's names are written for a log line, where there is room to say
// "number of double-bit binary inputs". A panel column is sixteen characters,
// so the long ones get a short form here rather than being truncated into
// something that reads as a different attribute.
//
// A device's own attributes, and any set other than the standard one, come
// back numbered: this tool has no business guessing what a vendor calls
// something it invented.
func attributeLabel(a dnp3.Attribute) string {
	if a.Set != dnp3.AttrSetStandard {
		return a.Name()
	}
	if short, ok := shortAttributeNames[a.Variation]; ok {
		return short
	}
	if name, ok := dnp3.AttributeName(a.Variation); ok {
		return name
	}
	return a.Name()
}

// shortAttributeNames fit the panel. Only the ones whose full name does not.
var shortAttributeNames = map[uint8]string{
	203: "max BO per req",
	206: "AO events",
	207: "max AO index",
	208: "analog outputs",
	209: "BO events",
	210: "max BO index",
	211: "binary outputs",
	212: "frozen ctr events",
	213: "frozen counters",
	214: "counter events",
	215: "max counter index",
	216: "counters",
	217: "frozen analogs",
	218: "AI events",
	219: "max AI index",
	220: "analog inputs",
	221: "DBBI events",
	222: "max DBBI index",
	223: "double-bit inputs",
	224: "BI events",
	225: "max BI index",
	226: "binary inputs",
	227: "max tx fragment",
	228: "max rx fragment",
	242: "version",
	243: "hardware",
	244: "owner",
	246: "ID code",
	247: "device name",
	248: "serial",
	249: "subset level",
	250: "product",
	252: "vendor",
}

// deviceSummary is the one line the session panel shows, so the identity is on
// screen even when the Device panel has been squeezed off it.
func (m *Model) deviceSummary() string {
	switch {
	case m.device.unsupported:
		return "not reported"
	case m.device.busy:
		return "reading…"
	}
	if name, ok := m.deviceName(); ok {
		return name
	}
	return "—"
}

// resetDevice forgets what the last device said, which is what a reconnect
// means: the address may now be answered by something else entirely.
func (m *Model) resetDevice() { m.device = deviceState{} }

// attributeRefreshDelay spaces the automatic read out from the startup
// sequence, so the device's first exchange with this master is the one the
// standard expects rather than a question about its nameplate.
const attributeRefreshDelay = 250 * time.Millisecond
