package outstation

import (
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/internal/stack"
	"github.com/dscsystems/go-dnp3/objects"
	"github.com/dscsystems/go-dnp3/secevent"
)

// selectFault says why an OPERATE did not match its SELECT.
type selectFault uint8

const (
	faultNone selectFault = iota
	faultNoSelect
	faultTimeout
	faultSeqGap
	faultMismatch
)

// refusal says who refused a command before the handler saw it.
type refusal uint8

const (
	refusedNot refusal = iota
	refusedSelect
	refusedUnknownPoint
	refusedTooMany
	refusedPolicy
)

// maxCommandEvents is how many objects of one request get a cmd.received
// each; a request with more gets one event for all of them.
const maxCommandEvents = 16

// guardCommand applies the policy to one control before the handler sees
// it, and reports fields no sane master sends. It returns SUCCESS to let the
// control through.
func (s *Session) guardCommand(fc app.FuncCode, group, variation uint8, index uint16, raw []byte) dnp3.CommandStatus {
	p := s.cfg.Security
	if p == nil {
		return dnp3.CommandSuccess
	}
	var code dnp3.ControlCode
	if group == 12 {
		c := objects.ParseCROB(raw)
		code = c.Code
		if reason := crobInvalid(c); reason != "" {
			s.commandFieldInvalid(group, variation, index, code, reason)
		}
	} else if v := parseAnalogOutput(variation, raw).Value; math.IsNaN(v) || math.IsInf(v, 0) {
		s.commandFieldInvalid(group, variation, index, 0, "analog output value "+strconv.FormatFloat(v, 'g', -1, 64))
	}

	if s.commandFlooded(group, variation, index) {
		return p.Throttle()
	}

	status, detail := dnp3.CommandSuccess, ""
	switch {
	case p.Functions.Blocks(uint8(fc)):
		status = p.Functions.CommandDenyStatus()
		detail = fmt.Sprintf("%s refused by the function policy", fc)
	case p.CommandGuard != nil:
		peer, _ := s.sec.Peer()
		status = p.CommandGuard(peer, uint8(fc), group, index, code)
		detail = "control refused by the command guard"
	}
	if status.OK() {
		return dnp3.CommandSuccess
	}
	e := s.commandEvent(secevent.KindAccessFunctionBlocked, group, variation, uint32(index), status)
	if group == 12 {
		e.Fields |= secevent.HasControlCode
		e.ControlCode = code
	}
	e.Action, e.Detail = secevent.Rejected, detail
	s.emit(e)
	return status
}

// crobInvalid names what is wrong with a CROB's fields, if anything.
func crobInvalid(c dnp3.ControlRelayOutputBlock) string {
	switch op := c.Code.OpType(); {
	case op > dnp3.ControlLatchOff:
		return fmt.Sprintf("reserved operation type %d", op)
	case op == dnp3.ControlNUL:
		return ""
	case c.Count == 0:
		return "count 0"
	case (op == dnp3.ControlPulseOn || op == dnp3.ControlPulseOff) && c.OnTime == 0:
		return "pulse with on-time 0"
	}
	return ""
}

func (s *Session) commandFieldInvalid(group, variation uint8, index uint16, code dnp3.ControlCode, reason string) {
	e := s.commandEvent(secevent.KindProtoFieldInvalid, group, variation, uint32(index), 0)
	e.Fields &^= secevent.HasCommandStatus
	if group == 12 {
		e.Fields |= secevent.HasControlCode
		e.ControlCode = code
	}
	e.Detail = "control with " + reason
	s.emit(e)
}

// commandEvent returns an event about one command object of the request
// being handled.
func (s *Session) commandEvent(k secevent.Kind, group, variation uint8, index uint32, status dnp3.CommandStatus) secevent.Event {
	e := appEvent(k, s.secReq.r, &s.secReq.hdr)
	e.Fields |= secevent.HasObject | secevent.HasIndex | secevent.HasCommandStatus
	e.Group, e.Variation, e.Index, e.CommandStatus = group, variation, index, status
	return e
}

// auditCommands reports what became of a request's commands.
func (s *Session) auditCommands(r stack.Received, frag app.Fragment, op OperateType, fault selectFault, outcomes []commandOutcome) {
	if !s.sec.Enabled() {
		return
	}
	hdr := &frag.Header
	switch fault {
	case faultNoSelect:
		s.emitApp(secevent.KindCmdOperateWithoutSelect, r, hdr, secevent.Rejected, "OPERATE with no selection", nil)
	case faultTimeout:
		s.emitApp(secevent.KindCmdSelectTimeout, r, hdr, secevent.Rejected, "OPERATE after the selection expired", nil)
	case faultSeqGap:
		s.emitApp(secevent.KindCmdOperateSeqGap, r, hdr, secevent.Rejected,
			"OPERATE is not the request right after its SELECT: another request came between", nil)
	case faultMismatch:
		s.emitApp(secevent.KindCmdOperateMismatch, r, hdr, secevent.Rejected,
			"OPERATE objects differ from the SELECT's", nil)
	}
	if hdr.Func == app.FuncDirectOperateNR {
		s.emitApp(secevent.KindCmdNoAckOperate, r, hdr, secevent.Observed,
			fmt.Sprintf("DIRECT_OPERATE_NR of %d control(s): executed with no response", len(outcomes)), nil)
	}

	for _, o := range outcomes {
		switch {
		case o.refusal == refusedUnknownPoint:
			e := s.commandEvent(secevent.KindAccessUnknownPoint, o.group, o.variation, o.index, o.status)
			e.Action, e.Detail = secevent.Rejected, fmt.Sprintf("control for output %d, which this device does not have", o.index)
			s.emit(e)
			s.sec.Unknown(fmt.Sprintf("p%d/%d", o.group, o.index))
		case o.status == dnp3.CommandLocal || (o.status == dnp3.CommandNotAuthorized && o.refusal == refusedNot):
			e := s.commandEvent(secevent.KindCmdLocalMode, o.group, o.variation, o.index, o.status)
			e.Action, e.Detail = secevent.Rejected, "control refused by the device: "+o.status.String()
			s.emit(e)
		case !o.status.OK() && o.refusal != refusedSelect && o.refusal != refusedPolicy:
			// Select faults are reported once per request above, and policy
			// refusals by guardCommand.
			e := s.commandEvent(secevent.KindCmdRejected, o.group, o.variation, o.index, o.status)
			e.Action, e.Value = secevent.Rejected, o.status.String()
			e.Detail = "control refused: " + o.status.String()
			s.emit(e)
			if s.sec.Exceeded("cmd.rejected", s.cfg.Security.EffectiveLimits().CommandRejects) {
				b := s.commandEvent(secevent.KindCmdBruteForce, o.group, o.variation, o.index, o.status)
				b.Detail = "refused controls from this connection exceed the policy's rate: guessing points or codes?"
				s.emit(b)
			}
		}
	}

	if !s.sec.Audit() || len(outcomes) == 0 {
		return
	}
	verb := "operated"
	if hdr.Func == app.FuncSelect {
		verb = "selected"
	}
	if len(outcomes) > maxCommandEvents {
		ok := 0
		for _, o := range outcomes {
			if o.status.OK() {
				ok++
			}
		}
		s.emitApp(secevent.KindCmdReceived, r, hdr, secevent.Observed,
			fmt.Sprintf("%s: %d controls, %d accepted (%s)", hdr.Func, len(outcomes), ok, op), nil)
		return
	}
	for _, o := range outcomes {
		e := s.commandEvent(secevent.KindCmdReceived, o.group, o.variation, o.index, o.status)
		e.Value = describeCommand(o)
		e.Detail = fmt.Sprintf("%s %s output %d: %s", hdr.Func, verb, o.index, o.status)
		if hdr.Func != app.FuncSelect {
			e.Detail += " (" + op.String() + ")"
		}
		if o.group == 12 {
			e.Fields |= secevent.HasControlCode
			e.ControlCode = objects.ParseCROB(o.raw).Code
		}
		if !o.status.OK() {
			e.Action = secevent.Rejected
		}
		s.emit(e)
	}
}

// describeCommand renders a command object for the audit trail.
func describeCommand(o commandOutcome) string {
	if o.group == 12 {
		c := objects.ParseCROB(o.raw)
		return fmt.Sprintf("%s count=%d on=%dms off=%dms", c.Code, c.Count, c.OnTime, c.OffTime)
	}
	return strconv.FormatFloat(parseAnalogOutput(o.variation, o.raw).Value, 'g', -1, 64)
}

// selectAbandoned reports a selection that was never operated.
func (s *Session) selectAbandoned(r stack.Received, now time.Time) {
	if !s.sec.Enabled() {
		return
	}
	detail := "selection replaced by a new SELECT without an OPERATE"
	if now.After(s.sel.expires) {
		detail = "selection expired without an OPERATE"
	}
	s.emitApp(secevent.KindCmdSelectAbandoned, r, nil, secevent.Observed, detail, nil)
}

// stateChanging reports whether a replay of fc would change the device:
// the functions cmd.duplicate watches. Class assignment and unsolicited
// enables are left out — a master's startup sequence repeats them octet for
// octet after every reconnect.
func stateChanging(fc app.FuncCode, frag app.Fragment) bool {
	switch fc {
	case app.FuncOperate, app.FuncDirectOperate, app.FuncDirectOperateNR,
		app.FuncColdRestart, app.FuncWarmRestart,
		app.FuncInitializeData, app.FuncInitializeAppl, app.FuncStartAppl, app.FuncStopAppl,
		app.FuncSaveConfig, app.FuncActivateConfig, app.FuncDeleteFile:
		return true
	case app.FuncWrite:
		for _, h := range frag.Objects {
			if h.Group != 50 && h.Group != 80 {
				return true
			}
		}
	}
	return false
}

// checkDuplicate reports a state-changing request whose octets another
// connection already sent within the window: a replay.
func (s *Session) checkDuplicate(r stack.Received, frag app.Fragment) {
	if !s.sec.Enabled() || s.cfg.SecureAuthentication != nil || !stateChanging(frag.Header.Func, frag) {
		return
	}
	prev, dup := s.sec.Duplicate(uint8(frag.Header.Func), frag.Raw[frag.Header.Size():])
	if !dup {
		return
	}
	s.emitApp(secevent.KindCmdDuplicate, r, &frag.Header, secevent.Observed,
		fmt.Sprintf("%s identical to one connection %d sent: a replay?", frag.Header.Func, prev), nil)
}
