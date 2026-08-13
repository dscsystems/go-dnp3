package outstation

import (
	"bytes"
	"io"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/internal/stack"
	"github.com/dscsystems/go-dnp3/objects"
)

// OperateType says how a command reached the outstation.
//
// The distinction matters to an implementation that logs or authorises
// controls: a direct operate arrived with no prior select, so nothing was
// reserved, and a no-reply operate will get no acknowledgement whatever the
// outcome.
type OperateType uint8

// Operate types.
const (
	// OperateDirect is a DIRECT_OPERATE: execute immediately, answer with the
	// outcome.
	OperateDirect OperateType = iota
	// OperateDirectNoAck is a DIRECT_OPERATE_NR: execute immediately and send
	// no response at all.
	OperateDirectNoAck
	// OperateSelected is an OPERATE following a successful SELECT.
	OperateSelected
)

func (o OperateType) String() string {
	switch o {
	case OperateDirect:
		return "direct-operate"
	case OperateDirectNoAck:
		return "direct-operate-no-reply"
	case OperateSelected:
		return "operate-after-select"
	default:
		return "OperateType(?)"
	}
}

// AnalogOutput is an analog output command, carried as a float64 regardless of
// the variation it arrived in.
//
// The variation is kept because it is information the handler may need — a
// setpoint sent as a 16-bit integer cannot express what one sent as a double
// can — but a handler that does not care can read Value and ignore it.
type AnalogOutput struct {
	Value float64
	// Variation is the group 41 variation the command arrived in: 1 for
	// 32-bit integer, 2 for 16-bit, 3 for single precision, 4 for double.
	Variation uint8
}

// CommandHandler executes controls.
//
// Select is called for a SELECT and must not operate anything: it reports
// whether the outstation *would* accept the command. Operate is called for an
// OPERATE, a DIRECT_OPERATE or a DIRECT_OPERATE_NR, and is the call that
// actually moves the plant.
//
// Both are called from the session goroutine, so a handler that takes a long
// time stalls the protocol. Anything slow belongs behind a queue — but note
// that returning success before the operation completes is a claim the
// outstation cannot take back.
type CommandHandler interface {
	SelectCROB(index uint16, c dnp3.ControlRelayOutputBlock) dnp3.CommandStatus
	OperateCROB(index uint16, c dnp3.ControlRelayOutputBlock, op OperateType) dnp3.CommandStatus

	SelectAnalog(index uint16, v AnalogOutput) dnp3.CommandStatus
	OperateAnalog(index uint16, v AnalogOutput, op OperateType) dnp3.CommandStatus
}

// RejectingCommandHandler refuses every command with NOT_SUPPORTED.
//
// It is the default, and it is deliberately a refusal rather than a success:
// an outstation whose controls are not wired up must say so, not silently
// report that a breaker operated.
type RejectingCommandHandler struct{}

func (RejectingCommandHandler) SelectCROB(uint16, dnp3.ControlRelayOutputBlock) dnp3.CommandStatus {
	return dnp3.CommandNotSupported
}

func (RejectingCommandHandler) OperateCROB(uint16, dnp3.ControlRelayOutputBlock, OperateType) dnp3.CommandStatus {
	return dnp3.CommandNotSupported
}

func (RejectingCommandHandler) SelectAnalog(uint16, AnalogOutput) dnp3.CommandStatus {
	return dnp3.CommandNotSupported
}

func (RejectingCommandHandler) OperateAnalog(uint16, AnalogOutput, OperateType) dnp3.CommandStatus {
	return dnp3.CommandNotSupported
}

// selection is the state a SELECT leaves behind for the OPERATE that follows.
type selection struct {
	active bool
	// objects is the raw object portion of the select request. The operate
	// must reproduce it exactly.
	objects []byte
	seq     uint8
	expires time.Time
}

// clear discards the selection.
func (s *selection) clear() {
	s.active = false
	s.objects = s.objects[:0]
}

// matches reports whether an operate request corresponds to the live
// selection.
//
// The comparison is over the raw object octets, which is the strictest
// reading of the standard's "same objects" requirement and the right one. A
// looser check — matching only indexes, say — would let an OPERATE trip a
// breaker after a SELECT that named a close, and the whole point of
// select-before-operate is that the operator confirms exactly what was
// proposed.
func (s *selection) matches(objectsRaw []byte, seq uint8, now time.Time) dnp3.CommandStatus {
	switch {
	case !s.active:
		return dnp3.CommandNoSelect
	case now.After(s.expires):
		return dnp3.CommandTimeout
	case seq != nextSeq(s.seq):
		// The operate must be the request immediately after the select.
		// Anything else means a request went missing in between, and the
		// operator may not be acting on what they think they are.
		return dnp3.CommandNoSelect
	case !bytes.Equal(s.objects, objectsRaw):
		return dnp3.CommandNoSelect
	default:
		return dnp3.CommandSuccess
	}
}

func nextSeq(s uint8) uint8 { return (s + 1) % app.SeqModulus }

// commandOutcome is the status assigned to one command object.
type commandOutcome struct {
	index  uint16
	status dnp3.CommandStatus
}

// onCommand handles SELECT, OPERATE, DIRECT_OPERATE and DIRECT_OPERATE_NR.
//
// The response echoes the request's objects with each status filled in, which
// is how a master learns which point in a multi-command request failed.
func (s *Session) onCommand(w io.Writer, r stack.Received, frag app.Fragment) error {
	now := s.appl.Now()
	fc := frag.Header.Func
	objectsRaw := frag.Raw[frag.Header.Size():]

	// A SELECT reserves; everything else operates.
	if fc == app.FuncSelect {
		return s.onSelect(w, r, frag, objectsRaw, now)
	}

	opType := OperateDirect
	switch fc {
	case app.FuncOperate:
		opType = OperateSelected
	case app.FuncDirectOperateNR:
		opType = OperateDirectNoAck
	}

	// An OPERATE is only honoured against a live, matching selection.
	var selectStatus = dnp3.CommandSuccess
	if opType == OperateSelected {
		selectStatus = s.sel.matches(objectsRaw, frag.Header.Control.Seq, now)
		// The selection is consumed either way: a failed operate must not
		// leave a reservation an operator could stumble into later.
		s.sel.clear()
	}

	body, outcomes := s.executeCommands(frag, opType, selectStatus, now)
	s.logCommands(fc, opType, outcomes)

	if fc.NoReply() || r.Broadcast {
		return nil
	}
	return s.respond(w, r, frag.Header, body)
}

// onSelect reserves the commands without operating anything.
func (s *Session) onSelect(w io.Writer, r stack.Received, frag app.Fragment, objectsRaw []byte, now time.Time) error {
	body, outcomes := s.executeCommands(frag, 0, dnp3.CommandSuccess, now)

	allOK := true
	for _, o := range outcomes {
		if !o.status.OK() {
			allOK = false
			break
		}
	}

	if allOK {
		s.sel.active = true
		s.sel.objects = append(s.sel.objects[:0], objectsRaw...)
		s.sel.seq = frag.Header.Control.Seq
		s.sel.expires = now.Add(s.cfg.SelectTimeout)
	} else {
		s.sel.clear()
	}

	s.logCommands(app.FuncSelect, 0, outcomes)
	if r.Broadcast {
		return nil
	}
	return s.respond(w, r, frag.Header, body)
}

// executeCommands walks the request's command objects, running each through
// the handler and building the echo response.
//
// selectStatus lets a failed select-before-operate check short-circuit every
// command in the request without touching the handler, which is what keeps a
// stale OPERATE from moving anything.
func (s *Session) executeCommands(
	frag app.Fragment, opType OperateType, selectStatus dnp3.CommandStatus, _ time.Time,
) ([]byte, []commandOutcome) {
	var body []byte
	var outcomes []commandOutcome

	selecting := frag.Header.Func == app.FuncSelect

	for _, h := range frag.Objects {
		d, ok := objects.Lookup(objects.GV(h.Group, h.Variation))
		if !ok || d.Kind != objects.KindCommand {
			s.iin = s.iin.Set(app.IINObjectUnknown)
			continue
		}
		size, ok := d.SizeOctets()
		if !ok || size == 0 {
			s.iin = s.iin.Set(app.IINObjectUnknown)
			continue
		}

		prefixLen := h.Qualifier.IndexPrefix().Octets()
		if !h.Qualifier.IndexPrefix().IsIndex() {
			// A command with no index prefix cannot say which point it means.
			s.iin = s.iin.Set(app.IINParameterError)
			continue
		}

		echo := make([]byte, 0, len(h.Data))
		off := 0
		for range int(h.Count()) {
			if off+prefixLen+size > len(h.Data) {
				break
			}
			index := uint16(readPrefix(h.Data[off:], prefixLen))
			raw := h.Data[off+prefixLen : off+prefixLen+size]
			off += prefixLen + size

			status := selectStatus
			if status.OK() {
				status = s.runCommand(h.Group, h.Variation, index, raw, selecting, opType)
			}
			outcomes = append(outcomes, commandOutcome{index: index, status: status})

			echo = append(echo, h.Data[off-prefixLen-size:off]...)
			// The status octet is the last of every command object, so
			// overwriting it in place turns the echoed request into the
			// response the master expects.
			echo[len(echo)-1] = byte(status)
		}

		body = app.AppendObjectHeader(body, app.ObjectHeader{
			Group:     h.Group,
			Variation: h.Variation,
			Qualifier: h.Qualifier,
			Range:     h.Range,
			Data:      echo,
		})
	}

	return body, outcomes
}

// runCommand decodes one command object and hands it to the handler.
func (s *Session) runCommand(
	group, variation uint8, index uint16, raw []byte, selecting bool, opType OperateType,
) dnp3.CommandStatus {
	switch group {
	case 12:
		c := objects.ParseCROB(raw)
		if selecting {
			return s.cmds.SelectCROB(index, c)
		}
		return s.cmds.OperateCROB(index, c, opType)

	case 41:
		v := parseAnalogOutput(variation, raw)
		if selecting {
			return s.cmds.SelectAnalog(index, v)
		}
		return s.cmds.OperateAnalog(index, v, opType)
	}
	return dnp3.CommandNotSupported
}

// parseAnalogOutput decodes any of the four group 41 variations into a
// common form.
func parseAnalogOutput(variation uint8, raw []byte) AnalogOutput {
	v := AnalogOutput{Variation: variation}
	switch variation {
	case 1:
		v.Value = float64(objects.ParseAnalogOutputInt32(raw).Value)
	case 2:
		v.Value = float64(objects.ParseAnalogOutputInt16(raw).Value)
	case 3:
		v.Value = float64(objects.ParseAnalogOutputFloat32(raw).Value)
	case 4:
		v.Value = objects.ParseAnalogOutputFloat64(raw).Value
	}
	return v
}

func (s *Session) logCommands(fc app.FuncCode, opType OperateType, outcomes []commandOutcome) {
	for _, o := range outcomes {
		level := "ok"
		if !o.status.OK() {
			level = "rejected"
		}
		s.log.Info("command",
			"func", fc.String(), "operate", opType.String(),
			"index", o.index, "status", o.status.String(), "result", level)
	}
}

// checkSelectTimeout expires a stale selection.
//
// A reservation that outlives its window must not be operable: an operator who
// selected a breaker, walked away, and came back minutes later should have to
// select again rather than operate on a decision they no longer remember
// making.
func (s *Session) checkSelectTimeout(now time.Time) {
	if s.sel.active && now.After(s.sel.expires) {
		s.log.Debug("selection expired", "seq", s.sel.seq)
		s.sel.clear()
	}
}

// readPrefix reads a little-endian per-object index prefix.
func readPrefix(buf []byte, width int) uint32 {
	switch width {
	case 1:
		return uint32(buf[0])
	case 2:
		return uint32(buf[0]) | uint32(buf[1])<<8
	case 4:
		return uint32(buf[0]) | uint32(buf[1])<<8 | uint32(buf[2])<<16 | uint32(buf[3])<<24
	}
	return 0
}
