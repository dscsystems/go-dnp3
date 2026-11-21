package secevent

import (
	"fmt"
	"time"

	"github.com/dscsystems/go-dnp3"
)

// Limits are the rates beyond which traffic of one class from one
// connection is a flood. A zero Rate uses the default below; a negative N
// turns that check off. The defaults are set so that ordinary SCADA polling
// never reaches them.
type Limits struct {
	Commands       Rate // control objects: SELECT, OPERATE, DIRECT_OPERATE(_NR)
	Integrity      Rate // class 0 reads
	EventPolls     Rate // class 1, 2 and 3 reads
	Restart        Rate // COLD_RESTART and WARM_RESTART
	TimeWrite      Rate // clock writes
	AppControl     Rate // application and configuration functions
	LinkStatus     Rate // REQUEST_LINK_STATUS
	LinkReset      Rate // RESET_LINK_STATES
	AuthChallenges Rate // critical requests challenged under SAv5
	FileOps        Rate // file opens and deletes
	Frames         Rate // link frames of any kind

	FCBRepeats     Rate // seq.fcb_repeat becomes Alert above this
	Repeats        Rate // seq.repeat_request becomes Warning above this
	CommandRejects Rate // cmd.rejected above this raises cmd.brute_force
}

// DefaultLimits are the limits used where Limits leaves a rate zero.
var DefaultLimits = Limits{
	Commands:       Rate{N: 20, Per: 2 * time.Second},
	Integrity:      Rate{N: 6, Per: time.Minute},
	EventPolls:     Rate{N: 20, Per: time.Second},
	Restart:        Rate{N: 2, Per: 10 * time.Minute},
	TimeWrite:      Rate{N: 6, Per: time.Minute},
	AppControl:     Rate{N: 2, Per: time.Minute},
	LinkStatus:     Rate{N: 5, Per: time.Second},
	LinkReset:      Rate{N: 5, Per: 10 * time.Second},
	AuthChallenges: Rate{N: 20, Per: time.Second},
	FileOps:        Rate{N: 10, Per: time.Second},
	Frames:         Rate{N: 2000, Per: time.Second},
	FCBRepeats:     Rate{N: 5, Per: time.Minute},
	Repeats:        Rate{N: 5, Per: time.Minute},
	CommandRejects: Rate{N: 10, Per: time.Minute},
}

// With returns l with every zero rate replaced by its default.
func (l Limits) With(def Limits) Limits {
	l.Commands = l.Commands.Or(def.Commands)
	l.Integrity = l.Integrity.Or(def.Integrity)
	l.EventPolls = l.EventPolls.Or(def.EventPolls)
	l.Restart = l.Restart.Or(def.Restart)
	l.TimeWrite = l.TimeWrite.Or(def.TimeWrite)
	l.AppControl = l.AppControl.Or(def.AppControl)
	l.LinkStatus = l.LinkStatus.Or(def.LinkStatus)
	l.LinkReset = l.LinkReset.Or(def.LinkReset)
	l.AuthChallenges = l.AuthChallenges.Or(def.AuthChallenges)
	l.FileOps = l.FileOps.Or(def.FileOps)
	l.Frames = l.Frames.Or(def.Frames)
	l.FCBRepeats = l.FCBRepeats.Or(def.FCBRepeats)
	l.Repeats = l.Repeats.Or(def.Repeats)
	l.CommandRejects = l.CommandRejects.Or(def.CommandRejects)
	return l
}

// EffectiveLimits returns the policy's limits with the defaults filled in.
func (p *Policy) EffectiveLimits() Limits {
	if p == nil {
		return DefaultLimits
	}
	return p.Limits.With(DefaultLimits)
}

// Flood returns what to do with traffic beyond a limit: Observed (report
// only, the default), Throttled or Disconnected.
func (p *Policy) Flood() Action {
	if p == nil {
		return Observed
	}
	return p.FloodAction
}

// Throttle returns the status a throttled control is answered with.
func (p *Policy) Throttle() dnp3.CommandStatus {
	if p == nil || p.ThrottleStatus == 0 {
		return dnp3.CommandTooManyOps
	}
	return p.ThrottleStatus
}

// DefaultConfirmWithheld is how many confirmations in a row a master may
// let time out before dos.confirm_withheld.
const DefaultConfirmWithheld = 3

// ConfirmWithheld returns the effective count, zero when off.
func (p *Policy) ConfirmWithheld() int {
	switch {
	case p == nil || p.ConfirmWithheldAfter < 0:
		return 0
	case p.ConfirmWithheldAfter == 0:
		return DefaultConfirmWithheld
	}
	return p.ConfirmWithheldAfter
}

func (p *Policy) validFlood() error {
	switch p.FloodAction {
	case Observed, Throttled, Disconnected:
		return nil
	}
	return fmt.Errorf("%w: secevent: FloodAction must be Observed, Throttled or Disconnected", dnp3.ErrBadConfig)
}
