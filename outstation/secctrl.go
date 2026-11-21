package outstation

import (
	"fmt"
	"time"

	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/secevent"
)

// auditControl reports the state-changing function the request carried
// out, or tried to: the audit trail. errs is the request error indications
// this request earned, which say whether it was refused. Commands, time
// writes and deadband writes are reported where they happen, with the
// detail only those places know.
func (s *Session) auditControl(errs app.IIN) {
	q := &s.secReq
	if !q.dispatched {
		return
	}
	hdr := &q.hdr
	refused := errs != 0
	ev := func(k secevent.Kind, detail string) {
		e := appEvent(k, q.r, hdr)
		e.Detail = detail
		if refused {
			e.Action = secevent.Rejected
			e.Detail += "; refused"
		}
		s.emit(e)
	}

	if hdr.Func == app.FuncColdRestart || hdr.Func == app.FuncWarmRestart {
		// Refused or not, a restart request is always worth knowing about.
		if errs&app.IINAlreadyExecuting != 0 {
			return // ctrl.already_executing says it
		}
		k := secevent.KindCtrlWarmRestart
		if hdr.Func == app.FuncColdRestart {
			k = secevent.KindCtrlColdRestart
		}
		e := appEvent(k, q.r, hdr)
		e.Detail = hdr.Func.String() + " accepted"
		if refused {
			e.Severity, e.Action, e.Detail = secevent.Warning, secevent.Rejected, hdr.Func.String()+" refused"
		}
		s.emit(e)
		return
	}
	if !s.sec.Audit() {
		return
	}

	switch hdr.Func {
	case app.FuncStopAppl:
		ev(secevent.KindCtrlStopApplication, "STOP_APPL")
	case app.FuncStartAppl:
		ev(secevent.KindCtrlStartApplication, "START_APPL")
	case app.FuncInitializeAppl:
		ev(secevent.KindCtrlInitializeApplication, "INITIALIZE_APPL")
	case app.FuncInitializeData:
		ev(secevent.KindCtrlInitializeData, "INITIALIZE_DATA")
	case app.FuncSaveConfig:
		ev(secevent.KindCtrlSaveConfig, "SAVE_CONFIG")
	case app.FuncActivateConfig:
		ev(secevent.KindCtrlActivateConfig, "ACTIVATE_CONFIG")
	case app.FuncRecordCurrentTime:
		ev(secevent.KindCtrlRecordCurrentTime, "RECORD_CURRENT_TIME")
	case app.FuncDelayMeasure:
		ev(secevent.KindCtrlDelayMeasure, "DELAY_MEASURE")

	case app.FuncEnableUnsolicited, app.FuncDisableUnsolicited:
		on, off := s.unsolClasses&^q.unsolBefore, q.unsolBefore&^s.unsolClasses
		if off != 0 {
			ev(secevent.KindCtrlUnsolDisable, fmt.Sprintf("unsolicited reporting disabled for %v", off))
		}
		if on != 0 {
			ev(secevent.KindCtrlUnsolEnable, fmt.Sprintf("unsolicited reporting enabled for %v", on))
		}

	case app.FuncAssignClass:
		e := appEvent(secevent.KindCtrlAssignClass, q.r, hdr)
		e.Detail = "event classes reassigned"
		for _, o := range q.objects {
			if o == [2]uint8{60, 1} {
				// Class 0 means no events at all: alarm suppression.
				e.Severity, e.Detail = secevent.Alert, "points moved to class 0: they will report no events"
			}
		}
		if refused {
			e.Action = secevent.Rejected
		}
		s.emit(e)

	case app.FuncImmedFreeze, app.FuncImmedFreezeNR:
		ev(secevent.KindCtrlFreeze, hdr.Func.String())
	case app.FuncFreezeClear, app.FuncFreezeClearNR:
		ev(secevent.KindCtrlFreezeClear, hdr.Func.String()+": counters frozen and cleared")
	case app.FuncFreezeAtTime, app.FuncFreezeAtTimeNR:
		ev(secevent.KindCtrlFreezeAtTime, hdr.Func.String())

	case app.FuncWrite:
		seen := map[secevent.Kind]bool{}
		for _, o := range q.objects {
			var k secevent.Kind
			var what string
			switch g, v := o[0], o[1]; {
			case g == 80 && v == 1:
				k, what = secevent.KindCtrlRestartIINCleared, "DEVICE_RESTART indication cleared"
			case g == 0:
				k, what = secevent.KindCtrlAttributeWrite, "device attribute written"
			case g == 112:
				k, what = secevent.KindCtrlTerminalWrite, "virtual terminal output written"
			case g >= 85 && g <= 87:
				k, what = secevent.KindCtrlDatasetWrite, "data set written"
			case g == 50 && v == 4:
				k, what = secevent.KindCtrlTimeIntervalWrite, "time and interval written"
			default:
				continue
			}
			if !seen[k] {
				seen[k] = true
				ev(k, what)
			}
		}
	}
}

// auditTimeWrite reports a clock the master just set, from old to now.
func (s *Session) auditTimeWrite(old, now time.Time) {
	if !s.sec.Enabled() {
		return
	}
	delta := now.Sub(old)
	if s.sec.Audit() {
		e := appEvent(secevent.KindCtrlTimeWrite, s.secReq.r, &s.secReq.hdr)
		e.Value = delta.String()
		e.Detail = fmt.Sprintf("clock set by the master, moved by %v", delta.Round(time.Millisecond))
		s.emit(e)
	}
	if step := s.cfg.Security.ClockStep(); step > 0 && (delta > step || delta < -step) {
		e := appEvent(secevent.KindCtrlClockStep, s.secReq.r, &s.secReq.hdr)
		e.Value = delta.String()
		e.Detail = fmt.Sprintf("clock stepped by %v, more than %v", delta.Round(time.Millisecond), step)
		s.emit(e)
	}
}

// auditDeadband reports a deadband the master changed.
func (s *Session) auditDeadband(index uint16, old, now float64) {
	if !s.sec.Audit() {
		return
	}
	e := appEvent(secevent.KindCtrlDeadbandWrite, s.secReq.r, &s.secReq.hdr)
	e.Fields |= secevent.HasObject | secevent.HasIndex
	e.Group, e.Index = 34, uint32(index)
	e.Value = fmt.Sprintf("%g->%g", old, now)
	e.Detail = fmt.Sprintf("analog %d deadband %g -> %g", index, old, now)
	if now > old {
		e.Detail += ": fewer events will be reported"
	}
	s.emit(e)
}
