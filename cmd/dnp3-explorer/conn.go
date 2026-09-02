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
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/master"
	"github.com/dscsystems/go-dnp3/objects"
)

// connection is the boundary between the DNP3 session and the UI.
//
// Everything crosses it as a message on a channel. The session goroutine never
// touches the model, and the model never calls into the session synchronously:
// a request is a tea.Cmd that runs on Bubble Tea's own goroutine and returns a
// result message. A poll that takes five seconds therefore costs nothing but
// five seconds of that one command — the UI keeps repainting throughout.
type connection struct {
	msgs chan tea.Msg
	ctx  context.Context
	sup  *supervisor

	// mu guards everything the supervisor replaces on a reconnect. The UI
	// goroutine reads these on every repaint while a restart may be swapping
	// them from a command goroutine.
	mu   sync.Mutex
	sess *master.Session
	cur  link

	// dropped counts messages the UI was too slow to take, which is worth
	// admitting rather than hiding: a table quietly missing rows is worse than
	// a table that says it is missing rows. It is written from the session
	// goroutine and read by the renderer, so it is atomic.
	dropped atomic.Int64
}

// adopt installs a newly built session as the live one.
func (c *connection) adopt(sess *master.Session, p link) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sess, c.cur = sess, p
}

// session returns the live session, or nil while a reconnect is in flight.
func (c *connection) session() *master.Session {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sess
}

// setUnsolicited records the operator's choice against the connection, so the
// session a reconnect builds asks for the same thing this one did.
//
// It does not reconnect. Turning unsolicited reporting on or off is a request
// to the outstation, not a change of link, and tearing the session down to
// deliver it would lose everything on screen to say something the device can
// be told directly.
func (c *connection) setUnsolicited(on bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cur.Unsolicited = on
}

// current returns the parameters the live session was built with.
func (c *connection) current() link {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cur
}

// target is what the header shows, so an operator with several terminals open
// knows which device this one is pointed at.
func (c *connection) target() string { return c.current().target() }

// transport names the link for the overview panel.
func (c *connection) transport() string { return c.current().transport() }

// reconnect rebuilds the session with new parameters.
//
// It runs as a command, off the update loop, because tearing a session down
// means waiting for its goroutines: doing that in Update would freeze the
// interface for exactly as long as the old link took to notice it was closed.
func (c *connection) reconnect(p link) tea.Cmd {
	return func() tea.Msg {
		c.sup.start(p)
		return commandResultMsg{text: "connecting to " + p.target(), ok: true}
	}
}

// updateMsg is one measurement arriving from the outstation.
type updateMsg struct {
	Type    dnp3.PointType
	Index   uint16
	Value   string
	Num     float64
	HasNum  bool
	Flags   dnp3.Flags
	Stamp   dnp3.Timestamp
	IsEvent bool
	Class   dnp3.Class
	GV      objects.GroupVar
}

// statusMsg reports session state.
type statusMsg struct {
	text      string
	connected bool
	stats     master.Stats
	iin       string
	err       string
}

// logMsg is a line for the activity log.
type logMsg struct {
	level string
	text  string
}

// commandResultMsg reports the outcome of something the operator asked for.
type commandResultMsg struct {
	text string
	ok   bool
}

func (c commandResultMsg) level() string {
	if c.ok {
		return "ok"
	}
	return "error"
}

// wait returns a command that blocks until the next message arrives.
//
// Bubble Tea runs commands on their own goroutines, so blocking here is
// exactly right — it is Update that must never block.
func (c *connection) wait() tea.Cmd {
	return func() tea.Msg {
		select {
		case msg := <-c.msgs:
			return msg
		case <-c.ctx.Done():
			return statusMsg{text: "closed"}
		}
	}
}

// push delivers a message to the UI, dropping it if the UI has fallen behind.
//
// Dropping is deliberate. The alternative is blocking the session goroutine on
// a slow terminal, which would stall the protocol — an operator's scrollback
// is not worth a missed poll.
func (c *connection) push(msg tea.Msg) {
	select {
	case c.msgs <- msg:
	default:
		c.dropped.Add(1)
	}
}

// handler feeds the UI from the session.
//
// It holds its own session rather than reading the connection's, because a
// handler belongs to the session it was built with: reading the live one would
// report the successor's statistics against the predecessor's fragments during
// a reconnect.
type handler struct {
	master.NopHandler
	conn *connection
	sess *master.Session
}

func (h *handler) BeginFragment(info master.ResponseInfo) {
	h.conn.push(statusMsg{
		text:      "connected",
		connected: true,
		stats:     h.sess.Stats(),
		iin:       info.IIN.String(),
	})
}

func (h *handler) HandleBinary(info master.HeaderInfo, vs []dnp3.Indexed[dnp3.Binary]) {
	for _, v := range vs {
		h.conn.push(updateMsg{
			Type: dnp3.TypeBinary, Index: v.Index,
			Value: boolText(v.Value.Value), Flags: v.Value.Flags, Stamp: v.Value.Time,
			Num: boolNum(v.Value.Value), HasNum: true,
			IsEvent: info.IsEvent(), Class: info.Class, GV: info.GV,
		})
	}
}

func (h *handler) HandleDoubleBit(info master.HeaderInfo, vs []dnp3.Indexed[dnp3.DoubleBitBinary]) {
	for _, v := range vs {
		h.conn.push(updateMsg{
			Type: dnp3.TypeDoubleBitBinary, Index: v.Index,
			Value: v.Value.Value.String(), Flags: v.Value.Flags, Stamp: v.Value.Time,
			IsEvent: info.IsEvent(), Class: info.Class, GV: info.GV,
		})
	}
}

func (h *handler) HandleCounter(info master.HeaderInfo, vs []dnp3.Indexed[dnp3.Counter]) {
	for _, v := range vs {
		h.conn.push(updateMsg{
			Type: dnp3.TypeCounter, Index: v.Index,
			Value: fmt.Sprint(v.Value.Value), Flags: v.Value.Flags, Stamp: v.Value.Time,
			Num: float64(v.Value.Value), HasNum: true,
			IsEvent: info.IsEvent(), Class: info.Class, GV: info.GV,
		})
	}
}

func (h *handler) HandleFrozenCounter(info master.HeaderInfo, vs []dnp3.Indexed[dnp3.FrozenCounter]) {
	for _, v := range vs {
		h.conn.push(updateMsg{
			Type: dnp3.TypeFrozenCounter, Index: v.Index,
			Value: fmt.Sprint(v.Value.Value), Flags: v.Value.Flags, Stamp: v.Value.Time,
			Num: float64(v.Value.Value), HasNum: true,
			IsEvent: info.IsEvent(), Class: info.Class, GV: info.GV,
		})
	}
}

func (h *handler) HandleAnalog(info master.HeaderInfo, vs []dnp3.Indexed[dnp3.Analog]) {
	for _, v := range vs {
		h.conn.push(updateMsg{
			Type: dnp3.TypeAnalog, Index: v.Index,
			Value: formatFloat(v.Value.Value), Flags: v.Value.Flags, Stamp: v.Value.Time,
			Num: v.Value.Value, HasNum: true,
			IsEvent: info.IsEvent(), Class: info.Class, GV: info.GV,
		})
	}
}

func (h *handler) HandleBinaryOutputStatus(info master.HeaderInfo, vs []dnp3.Indexed[dnp3.BinaryOutputStatus]) {
	for _, v := range vs {
		h.conn.push(updateMsg{
			Type: dnp3.TypeBinaryOutputStatus, Index: v.Index,
			Value: boolText(v.Value.Value), Flags: v.Value.Flags, Stamp: v.Value.Time,
			Num: boolNum(v.Value.Value), HasNum: true,
			IsEvent: info.IsEvent(), Class: info.Class, GV: info.GV,
		})
	}
}

func (h *handler) HandleAnalogOutputStatus(info master.HeaderInfo, vs []dnp3.Indexed[dnp3.AnalogOutputStatus]) {
	for _, v := range vs {
		h.conn.push(updateMsg{
			Type: dnp3.TypeAnalogOutputStatus, Index: v.Index,
			Value: formatFloat(v.Value.Value), Flags: v.Value.Flags, Stamp: v.Value.Time,
			Num: v.Value.Value, HasNum: true,
			IsEvent: info.IsEvent(), Class: info.Class, GV: info.GV,
		})
	}
}

// HandleOctetString shows the strings a device reports about itself — point
// names, firmware versions, serial numbers. They are the fastest way to find
// out what an unfamiliar device actually is, so they belong in the table
// rather than in a debug log.
func (h *handler) HandleOctetString(info master.HeaderInfo, vs []dnp3.Indexed[dnp3.OctetString]) {
	for _, v := range vs {
		h.conn.push(updateMsg{
			Type: dnp3.TypeOctetString, Index: v.Index,
			Value: printable(v.Value), Flags: dnp3.Online,
			IsEvent: info.IsEvent(), Class: info.Class, GV: info.GV,
		})
	}
}

// ---------- Actions ----------
//
// Each action is a tea.Cmd: it runs off the update loop and reports back as a
// message, so a slow outstation never freezes the interface.

// requestTimeout bounds any one operator request. It is generous compared with
// the session's own response timeout because a scan of a large device is
// several fragments, each of which gets that timeout.
const requestTimeout = 30 * time.Second

// do wraps a session call as a command, naming it for the log either way.
//
// The session is resolved when the command runs rather than when it is built,
// because a reconnect replaces it: a command holding the old one would be sent
// to a device the operator has already navigated away from.
func (c *connection) do(what string, timeout time.Duration,
	fn func(context.Context, *master.Session) error) tea.Cmd {

	return func() tea.Msg {
		sess := c.session()
		if sess == nil {
			return commandResultMsg{text: what + ": not connected"}
		}
		ctx, cancel := context.WithTimeout(c.ctx, timeout)
		defer cancel()
		if err := fn(ctx, sess); err != nil {
			return commandResultMsg{text: what + " failed: " + err.Error()}
		}
		return commandResultMsg{text: what + " complete", ok: true}
	}
}

func (c *connection) integrityPoll() tea.Cmd {
	return c.do("integrity poll", requestTimeout,
		func(ctx context.Context, s *master.Session) error { return s.IntegrityPoll(ctx) })
}

func (c *connection) classPoll() tea.Cmd {
	return c.do("class 1/2/3 poll", requestTimeout,
		func(ctx context.Context, s *master.Session) error {
			return s.ScanClasses(ctx, dnp3.Class123)
		})
}

func (c *connection) rangeScan(group, variation uint8, start, stop uint16) tea.Cmd {
	what := fmt.Sprintf("range scan g%dv%d %d-%d", group, variation, start, stop)
	return c.do(what, requestTimeout, func(ctx context.Context, s *master.Session) error {
		return s.ScanRange(ctx, group, variation, start, stop)
	})
}

// syncTime sets the outstation's clock, optionally measuring the link delay
// first — which is what a slow serial link needs and Ethernet does not.
func (c *connection) syncTime(withDelay bool) tea.Cmd {
	if withDelay {
		return c.do("time sync (with delay measurement)", requestTimeout,
			func(ctx context.Context, s *master.Session) error {
				return s.SyncTimeWithDelay(ctx)
			})
	}
	return c.do("time sync", requestTimeout,
		func(ctx context.Context, s *master.Session) error { return s.SyncTime(ctx) })
}

func (c *connection) unsolicited(enable bool) tea.Cmd {
	if enable {
		return c.do("enable unsolicited 1/2/3", requestTimeout,
			func(ctx context.Context, s *master.Session) error {
				return s.EnableUnsolicited(ctx, dnp3.Class123)
			})
	}
	return c.do("disable unsolicited 1/2/3", requestTimeout,
		func(ctx context.Context, s *master.Session) error {
			return s.DisableUnsolicited(ctx, dnp3.Class123)
		})
}

func (c *connection) restart(mode dnp3.RestartMode) tea.Cmd {
	return c.do(mode.String()+" restart", requestTimeout,
		func(ctx context.Context, s *master.Session) error { return s.Restart(ctx, mode) })
}

func (c *connection) writeDeadband(index uint16, v float32) tea.Cmd {
	what := fmt.Sprintf("deadband AI %d = %g", index, v)
	return c.do(what, requestTimeout, func(ctx context.Context, s *master.Session) error {
		return s.WriteDeadband(ctx, map[uint16]float32{index: v})
	})
}

// controlOp is one control the operator can issue, carried with the words that
// will be shown to them before it goes out.
type controlOp struct {
	label string
	cmd   master.Command
}

func (op controlOp) describe(sbo bool) string {
	return fmt.Sprintf("%s — %s", op.label, controlMode(sbo))
}

// latchOp builds the plain close or open of a binary output.
func latchOp(index uint16, closing bool) controlOp {
	if closing {
		return controlOp{label: fmt.Sprintf("close BO %d (latch on)", index),
			cmd: master.LatchOn(index)}
	}
	return controlOp{label: fmt.Sprintf("open BO %d (latch off)", index),
		cmd: master.LatchOff(index)}
}

// operate issues a control, either select-before-operate or direct.
//
// Select-before-operate is the default because this is an interactive tool
// driven by a person: the select is the outstation's opportunity to refuse
// before anything in the substation moves. Direct operate is offered because
// some devices do not implement select, and a tool that cannot talk to them is
// no use in front of one.
func (c *connection) operate(op controlOp, sbo bool) tea.Cmd {
	return func() tea.Msg {
		sess := c.session()
		if sess == nil {
			return commandResultMsg{text: op.label + ": not connected"}
		}
		ctx, cancel := context.WithTimeout(c.ctx, requestTimeout)
		defer cancel()

		var res master.CommandResult
		var err error
		if sbo {
			res, err = sess.SelectAndOperate(ctx, op.cmd)
		} else {
			res, err = sess.DirectOperate(ctx, op.cmd)
		}
		switch {
		case err != nil:
			return commandResultMsg{text: fmt.Sprintf("%s failed: %v", op.label, err)}
		case !res.OK():
			// A refusal is not an error in the transport sense, and reporting
			// it as success because the exchange completed is how an operator
			// comes to believe a breaker moved when it did not.
			return commandResultMsg{text: fmt.Sprintf("%s refused: %v", op.label, res.Err())}
		default:
			return commandResultMsg{text: op.label + ": accepted", ok: true}
		}
	}
}

func boolText(v bool) string {
	if v {
		return "ON"
	}
	return "OFF"
}

func boolNum(v bool) float64 {
	if v {
		return 1
	}
	return 0
}

func formatFloat(v float64) string {
	if v == float64(int64(v)) && v < 1e15 && v > -1e15 {
		return fmt.Sprintf("%d", int64(v))
	}
	return fmt.Sprintf("%.3f", v)
}

// printable renders an octet string for a table cell, showing the bytes of
// anything that is not text rather than letting control characters loose in
// the terminal.
func printable(b []byte) string {
	for _, c := range b {
		if c < 0x20 || c > 0x7e {
			return fmt.Sprintf("%x", b)
		}
	}
	return string(b)
}
