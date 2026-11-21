package outstation

import (
	"fmt"
	"time"

	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/internal/sechub"
	"github.com/dscsystems/go-dnp3/internal/stack"
	"github.com/dscsystems/go-dnp3/secevent"
)

// secDoS is the outstation's state for the resource-exhaustion events.
type secDoS struct {
	confirmMisses int
	overflow      bool
}

// floodClass returns the flood class a request belongs to, if any.
func floodClass(frag app.Fragment, l secevent.Limits) (secevent.Kind, secevent.Rate, bool) {
	switch f := frag.Header.Func; f {
	case app.FuncRead:
		for _, h := range frag.Objects {
			if h.Group == 60 && h.Variation == 1 {
				return secevent.KindFloodIntegrity, l.Integrity, true
			}
		}
		for _, h := range frag.Objects {
			if h.Group == 60 {
				return secevent.KindFloodEventPolls, l.EventPolls, true
			}
		}
	case app.FuncColdRestart, app.FuncWarmRestart:
		return secevent.KindFloodRestart, l.Restart, true
	case app.FuncWrite:
		for _, h := range frag.Objects {
			if h.Group == 50 && (h.Variation == 1 || h.Variation == 3) {
				return secevent.KindFloodTimeWrite, l.TimeWrite, true
			}
		}
	case app.FuncInitializeData, app.FuncInitializeAppl, app.FuncStartAppl, app.FuncStopAppl,
		app.FuncSaveConfig, app.FuncActivateConfig:
		return secevent.KindFloodAppControl, l.AppControl, true
	case app.FuncOpenFile, app.FuncDeleteFile:
		return secevent.KindFloodFileOps, l.FileOps, true
	}
	return "", secevent.Rate{}, false
}

// flooded counts a request against its flood class and reports whether the
// policy says to refuse it.
func (s *Session) flooded(r stack.Received, frag app.Fragment) bool {
	if !s.sec.Enabled() {
		return false
	}
	k, rate, ok := floodClass(frag, s.cfg.Security.EffectiveLimits())
	if !ok {
		return false
	}
	return s.sec.Flood(k, rate, 1, sechub.Refusable, appEvent("", r, &frag.Header)) != secevent.Observed
}

// commandFlooded counts one control against the command rate, and reports
// whether the policy says to refuse it.
func (s *Session) commandFlooded(group, variation uint8, index uint16) bool {
	if !s.sec.Enabled() {
		return false
	}
	e := s.commandEvent("", group, variation, uint32(index), 0)
	e.Fields &^= secevent.HasCommandStatus
	return s.sec.Flood(secevent.KindFloodCommands, s.cfg.Security.EffectiveLimits().Commands, 1, sechub.Refusable, e) != secevent.Observed
}

// checkBacklog reports a receive queue that has stayed full: the peer sends
// faster than the outstation processes.
func (s *Session) checkBacklog(queued, capacity int, now time.Time) {
	s.sec.Backlog(queued, capacity, now)
}

// confirmMissed counts a confirmation the master let time out.
func (s *Session) confirmMissed(unsolicited bool) {
	if !s.sec.Enabled() {
		return
	}
	d := &s.secDoS
	d.confirmMisses++
	if n := s.cfg.Security.ConfirmWithheld(); n > 0 && d.confirmMisses == n {
		what := "response"
		if unsolicited {
			what = "unsolicited response"
		}
		s.emit(secevent.Event{Kind: secevent.KindDoSConfirmWithheld,
			Detail: fmt.Sprintf("%d confirmations in a row timed out, the last for a %s: events are requeued again and again", n, what)})
	}
}

// confirmed resets the run of missed confirmations.
func (s *Session) confirmed() { s.secDoS.confirmMisses = 0 }

// unexpectedConfirm reports a confirmation for a response not awaited.
func (s *Session) unexpectedConfirm(h app.Header, unsolicited bool) {
	if !s.sec.Enabled() {
		return
	}
	what := "response"
	if unsolicited {
		what = "unsolicited response"
	}
	e := appEvent(secevent.KindUnsolUnexpectedConfirm, s.secReq.r, &h)
	e.Action, e.Detail = secevent.Ignored, fmt.Sprintf("CONFIRM (SEQ %d) for a %s not awaiting one", h.Control.Seq, what)
	s.emit(e)
}

// checkOverflow reports the event buffer overflowing.
func (s *Session) checkOverflow() {
	if !s.sec.Enabled() {
		return
	}
	cur := s.db.events.Overflowed()
	if cur && !s.secDoS.overflow {
		s.emit(secevent.Event{Kind: secevent.KindDoSEventOverflow,
			Detail: "event buffer overflowed: events have been lost"})
	}
	s.secDoS.overflow = cur
}
