package main

import (
	"context"
	"fmt"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/master"
)

// connection is the boundary between the DNP3 session and the UI.
//
// Everything crosses it as a message on a channel. The session goroutine never
// touches the model, and the model never calls into the session synchronously:
// a request is a tea.Cmd that runs on Bubble Tea's own goroutine and returns a
// result message. A poll that takes five seconds therefore costs nothing but
// five seconds of that one command — the UI keeps repainting throughout.
type connection struct {
	sess *master.Session
	msgs chan tea.Msg
	ctx  context.Context
	// target is what the header shows, so an operator with several terminals
	// open knows which device this one is pointed at.
	target string
}

// updateMsg is one measurement arriving from the outstation.
type updateMsg struct {
	Type    dnp3.PointType
	Index   uint16
	Value   string
	Flags   dnp3.Flags
	Stamp   dnp3.Timestamp
	IsEvent bool
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
		return "info"
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
	}
}

// handler feeds the UI from the session.
type handler struct {
	master.NopHandler
	conn *connection
}

func (h *handler) BeginFragment(info master.ResponseInfo) {
	h.conn.push(statusMsg{
		text:      "connected",
		connected: true,
		stats:     h.conn.sess.Stats(),
		iin:       info.IIN.String(),
	})
}

func (h *handler) HandleBinary(info master.HeaderInfo, vs []dnp3.Indexed[dnp3.Binary]) {
	for _, v := range vs {
		h.conn.push(updateMsg{
			Type: dnp3.TypeBinary, Index: v.Index,
			Value: boolText(v.Value.Value), Flags: v.Value.Flags, Stamp: v.Value.Time,
			IsEvent: info.IsEvent(),
		})
	}
}

func (h *handler) HandleDoubleBit(info master.HeaderInfo, vs []dnp3.Indexed[dnp3.DoubleBitBinary]) {
	for _, v := range vs {
		h.conn.push(updateMsg{
			Type: dnp3.TypeDoubleBitBinary, Index: v.Index,
			Value: v.Value.Value.String(), Flags: v.Value.Flags, Stamp: v.Value.Time,
			IsEvent: info.IsEvent(),
		})
	}
}

func (h *handler) HandleCounter(info master.HeaderInfo, vs []dnp3.Indexed[dnp3.Counter]) {
	for _, v := range vs {
		h.conn.push(updateMsg{
			Type: dnp3.TypeCounter, Index: v.Index,
			Value: fmt.Sprint(v.Value.Value), Flags: v.Value.Flags, Stamp: v.Value.Time,
			IsEvent: info.IsEvent(),
		})
	}
}

func (h *handler) HandleFrozenCounter(info master.HeaderInfo, vs []dnp3.Indexed[dnp3.FrozenCounter]) {
	for _, v := range vs {
		h.conn.push(updateMsg{
			Type: dnp3.TypeFrozenCounter, Index: v.Index,
			Value: fmt.Sprint(v.Value.Value), Flags: v.Value.Flags, Stamp: v.Value.Time,
			IsEvent: info.IsEvent(),
		})
	}
}

func (h *handler) HandleAnalog(info master.HeaderInfo, vs []dnp3.Indexed[dnp3.Analog]) {
	for _, v := range vs {
		h.conn.push(updateMsg{
			Type: dnp3.TypeAnalog, Index: v.Index,
			Value: formatFloat(v.Value.Value), Flags: v.Value.Flags, Stamp: v.Value.Time,
			IsEvent: info.IsEvent(),
		})
	}
}

func (h *handler) HandleBinaryOutputStatus(info master.HeaderInfo, vs []dnp3.Indexed[dnp3.BinaryOutputStatus]) {
	for _, v := range vs {
		h.conn.push(updateMsg{
			Type: dnp3.TypeBinaryOutputStatus, Index: v.Index,
			Value: boolText(v.Value.Value), Flags: v.Value.Flags, Stamp: v.Value.Time,
			IsEvent: info.IsEvent(),
		})
	}
}

func (h *handler) HandleAnalogOutputStatus(info master.HeaderInfo, vs []dnp3.Indexed[dnp3.AnalogOutputStatus]) {
	for _, v := range vs {
		h.conn.push(updateMsg{
			Type: dnp3.TypeAnalogOutputStatus, Index: v.Index,
			Value: formatFloat(v.Value.Value), Flags: v.Value.Flags, Stamp: v.Value.Time,
			IsEvent: info.IsEvent(),
		})
	}
}

// ---------- Actions ----------

// Each action is a tea.Cmd: it runs off the update loop and reports back as a
// message, so a slow outstation never freezes the interface.

func (c *connection) integrityPoll() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(c.ctx, 30*time.Second)
		defer cancel()
		if err := c.sess.IntegrityPoll(ctx); err != nil {
			return commandResultMsg{text: "integrity poll failed: " + err.Error()}
		}
		return commandResultMsg{text: "integrity poll complete", ok: true}
	}
}

func (c *connection) classPoll() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(c.ctx, 30*time.Second)
		defer cancel()
		if err := c.sess.ScanClasses(ctx, dnp3.Class123); err != nil {
			return commandResultMsg{text: "class poll failed: " + err.Error()}
		}
		return commandResultMsg{text: "class 1/2/3 poll complete", ok: true}
	}
}

func (c *connection) syncTime() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(c.ctx, 15*time.Second)
		defer cancel()
		if err := c.sess.SyncTime(ctx); err != nil {
			return commandResultMsg{text: "time sync failed: " + err.Error()}
		}
		return commandResultMsg{text: "clock set", ok: true}
	}
}

// operate runs a select-before-operate on a binary output.
//
// The two-pass sequence rather than a direct operate: this is an interactive
// tool driven by a person, and the select is the outstation's opportunity to
// refuse before anything in the substation moves.
func (c *connection) operate(index uint16, closing bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(c.ctx, 20*time.Second)
		defer cancel()

		cmd := master.LatchOff(index)
		verb := "open"
		if closing {
			cmd = master.LatchOn(index)
			verb = "close"
		}

		res, err := c.sess.SelectAndOperate(ctx, cmd)
		if err != nil {
			return commandResultMsg{
				text: fmt.Sprintf("%s BO %d failed: %v", verb, index, err),
			}
		}
		return commandResultMsg{
			text: fmt.Sprintf("%s BO %d: %s", verb, index, res),
			ok:   true,
		}
	}
}

func boolText(v bool) string {
	if v {
		return "ON"
	}
	return "OFF"
}

func formatFloat(v float64) string {
	if v == float64(int64(v)) && v < 1e15 && v > -1e15 {
		return fmt.Sprintf("%d", int64(v))
	}
	return fmt.Sprintf("%.3f", v)
}
