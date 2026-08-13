package master

import (
	"context"
	"fmt"
	"strings"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/objects"
)

// Command is one control to issue at one point index.
//
// Build one with [CROB] or one of the AnalogOutput constructors rather than by
// hand: the encoding differs per variation, and the status octet must be zero
// on the way out so the outstation's echo is what fills it in.
type Command struct {
	Index uint16

	gv   objects.GroupVar
	data []byte
	desc string
}

func (c Command) String() string { return fmt.Sprintf("[%d] %s", c.Index, c.desc) }

// CROB builds a control relay output block command — the one that operates
// breakers, reclosers and other discrete outputs.
func CROB(index uint16, c dnp3.ControlRelayOutputBlock) Command {
	c.Status = dnp3.CommandSuccess // zero on the wire; the outstation fills it in
	return Command{
		Index: index,
		gv:    objects.GV(12, 1),
		data:  objects.AppendCROB(nil, c),
		desc:  c.String(),
	}
}

// Trip builds a CROB that trips a breaker with a pulse of the given duration.
func Trip(index uint16, pulseMillis uint32) Command {
	return CROB(index, dnp3.ControlRelayOutputBlock{
		Code:   dnp3.ControlPulseOn | dnp3.ControlTrip,
		Count:  1,
		OnTime: pulseMillis,
	})
}

// Close builds a CROB that closes a breaker with a pulse of the given
// duration.
func Close(index uint16, pulseMillis uint32) Command {
	return CROB(index, dnp3.ControlRelayOutputBlock{
		Code:   dnp3.ControlPulseOn | dnp3.ControlClose,
		Count:  1,
		OnTime: pulseMillis,
	})
}

// LatchOn and LatchOff build the CROBs that hold an output in a state.
func LatchOn(index uint16) Command {
	return CROB(index, dnp3.ControlRelayOutputBlock{Code: dnp3.ControlLatchOn, Count: 1})
}

func LatchOff(index uint16) Command {
	return CROB(index, dnp3.ControlRelayOutputBlock{Code: dnp3.ControlLatchOff, Count: 1})
}

// AnalogOutputInt16 builds a group 41 variation 2 setpoint.
func AnalogOutputInt16(index uint16, v int16) Command {
	return Command{
		Index: index,
		gv:    objects.GV(41, 2),
		data:  objects.AppendAnalogOutputInt16(nil, dnp3.AnalogOutputInt16{Value: v}),
		desc:  fmt.Sprintf("%d (int16)", v),
	}
}

// AnalogOutputInt32 builds a group 41 variation 1 setpoint.
func AnalogOutputInt32(index uint16, v int32) Command {
	return Command{
		Index: index,
		gv:    objects.GV(41, 1),
		data:  objects.AppendAnalogOutputInt32(nil, dnp3.AnalogOutputInt32{Value: v}),
		desc:  fmt.Sprintf("%d (int32)", v),
	}
}

// AnalogOutputFloat32 builds a group 41 variation 3 setpoint.
func AnalogOutputFloat32(index uint16, v float32) Command {
	return Command{
		Index: index,
		gv:    objects.GV(41, 3),
		data:  objects.AppendAnalogOutputFloat32(nil, dnp3.AnalogOutputFloat32{Value: v}),
		desc:  fmt.Sprintf("%v (float32)", v),
	}
}

// AnalogOutputFloat64 builds a group 41 variation 4 setpoint.
func AnalogOutputFloat64(index uint16, v float64) Command {
	return Command{
		Index: index,
		gv:    objects.GV(41, 4),
		data:  objects.AppendAnalogOutputFloat64(nil, dnp3.AnalogOutputFloat64{Value: v}),
		desc:  fmt.Sprintf("%v (float64)", v),
	}
}

// CommandResult reports what the outstation made of each command.
type CommandResult struct {
	// Statuses holds one status per command, in the order they were sent.
	Statuses []dnp3.CommandStatus
	// Commands echoes what was sent, so a caller logging a failure has the
	// point index to hand.
	Commands []Command
}

// OK reports whether every command succeeded.
//
// A multi-command request can partially succeed, and treating that as success
// would tell an operator a breaker operated when it did not.
func (r CommandResult) OK() bool {
	if len(r.Statuses) == 0 {
		return false
	}
	for _, s := range r.Statuses {
		if !s.OK() {
			return false
		}
	}
	return true
}

// Err returns an error describing the failures, or nil if every command
// succeeded.
func (r CommandResult) Err() error {
	if r.OK() {
		return nil
	}
	if len(r.Statuses) == 0 {
		return fmt.Errorf("master: the outstation returned no command statuses")
	}

	var b strings.Builder
	for i, s := range r.Statuses {
		if s.OK() {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("; ")
		}
		idx := uint16(0)
		if i < len(r.Commands) {
			idx = r.Commands[i].Index
		}
		fmt.Fprintf(&b, "index %d: %s", idx, s)
	}
	return fmt.Errorf("master: command failed: %s", b.String())
}

func (r CommandResult) String() string {
	parts := make([]string, len(r.Statuses))
	for i, s := range r.Statuses {
		idx := uint16(0)
		if i < len(r.Commands) {
			idx = r.Commands[i].Index
		}
		parts[i] = fmt.Sprintf("[%d]=%s", idx, s)
	}
	return strings.Join(parts, " ")
}

// buildCommands appends the command objects to a request.
//
// Commands sharing a group and variation are grouped into one header with
// per-object index prefixes, since the indexes being operated are rarely
// contiguous.
func buildCommands(b *app.Builder, cmds []Command) error {
	for i := 0; i < len(cmds); {
		gv := cmds[i].gv

		j := i
		for j < len(cmds) && cmds[j].gv == gv {
			j++
		}
		run := cmds[i:j]

		var data []byte
		for _, c := range run {
			data = append(data, byte(c.Index))
			data = append(data, c.data...)
		}

		if err := b.AddObject(app.ObjectHeader{
			Group:     gv.Group,
			Variation: gv.Variation,
			Qualifier: app.MakeQualifier(app.PrefixIndex1, app.RangeCount8),
			Range:     app.Range{Spec: app.RangeCount8, Count: uint32(len(run))},
			Data:      data,
		}); err != nil {
			return err
		}
		i = j
	}
	return nil
}

// parseCommandStatuses reads the statuses out of an outstation's echo.
func parseCommandStatuses(frag app.Fragment) []dnp3.CommandStatus {
	var out []dnp3.CommandStatus

	for _, h := range frag.Objects {
		d, ok := objects.Lookup(objects.GV(h.Group, h.Variation))
		if !ok || d.Kind != objects.KindCommand {
			continue
		}
		size, ok := d.SizeOctets()
		if !ok || size == 0 {
			continue
		}

		prefixLen := 0
		if p := h.Qualifier.IndexPrefix(); p.IsIndex() {
			prefixLen = p.Octets()
		}

		off := 0
		for range int(h.Count()) {
			if off+prefixLen+size > len(h.Data) {
				break
			}
			// The status is the last octet of every command object.
			out = append(out, dnp3.CommandStatus(h.Data[off+prefixLen+size-1]))
			off += prefixLen + size
		}
	}
	return out
}

// newCommandTask builds the task for one command request.
func newCommandTask(fc app.FuncCode, cmds []Command, result *CommandResult) *task {
	return &task{
		name:     "command-" + fc.String(),
		funcCode: fc,
		priority: priorityCommand,
		build: func(b *app.Builder) error {
			return buildCommands(b, cmds)
		},
		onFragment: func(frag app.Fragment) {
			result.Statuses = append(result.Statuses, parseCommandStatuses(frag)...)
		},
	}
}

// ---------- Public API ----------

// DirectOperate executes commands immediately, without a select.
//
// It is the right choice for an automated action. An operator-initiated
// control on plant that matters should use [Session.SelectAndOperate], which
// gives the outstation a chance to refuse before anything moves.
func (s *Session) DirectOperate(ctx context.Context, cmds ...Command) (CommandResult, error) {
	if len(cmds) == 0 {
		return CommandResult{}, fmt.Errorf("master: %w: no commands", dnp3.ErrBadConfig)
	}

	result := CommandResult{Commands: cmds}
	err := s.run(ctx, newCommandTask(app.FuncDirectOperate, cmds, &result))
	if err != nil {
		return result, err
	}
	return result, result.Err()
}

// DirectOperateNoReply executes commands and asks for no response.
//
// Nothing comes back, so nothing can be checked: this returns as soon as the
// request is on the wire. Use it only where the outcome genuinely does not
// need confirming.
func (s *Session) DirectOperateNoReply(ctx context.Context, cmds ...Command) error {
	if len(cmds) == 0 {
		return fmt.Errorf("master: %w: no commands", dnp3.ErrBadConfig)
	}
	var result CommandResult
	t := newCommandTask(app.FuncDirectOperateNR, cmds, &result)
	t.noResponse = true
	return s.run(ctx, t)
}

// SelectAndOperate runs the two-pass control sequence: select, check the
// outstation accepted it, then operate.
//
// The select is not a formality. It is the outstation's opportunity to say
// "not that point, not right now" before anything in the substation moves, and
// a failed select must never be followed by an operate.
func (s *Session) SelectAndOperate(ctx context.Context, cmds ...Command) (CommandResult, error) {
	if len(cmds) == 0 {
		return CommandResult{}, fmt.Errorf("master: %w: no commands", dnp3.ErrBadConfig)
	}

	selectResult := CommandResult{Commands: cmds}
	operateResult := CommandResult{Commands: cmds}

	sel := newCommandTask(app.FuncSelect, cmds, &selectResult)
	// Chaining rather than submitting two tasks: the standard requires the
	// OPERATE to carry the sequence number one above the SELECT, so anything
	// scheduled between them — a periodic poll, say — would make the
	// outstation reject the operate with NO_SELECT.
	sel.next = func() *task {
		if !selectResult.OK() {
			return nil
		}
		return newCommandTask(app.FuncOperate, cmds, &operateResult)
	}

	if err := s.run(ctx, sel); err != nil {
		return selectResult, err
	}
	if err := selectResult.Err(); err != nil {
		return selectResult, fmt.Errorf("select rejected: %w", err)
	}
	return operateResult, operateResult.Err()
}
