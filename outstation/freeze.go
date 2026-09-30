package outstation

import (
	"slices"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/objects"
)

// maxFreezeSchedules bounds how many FREEZE_AT_TIME requests are held at once.
const maxFreezeSchedules = 16

// freezeSchedule is one FREEZE_AT_TIME request waiting for its moment.
type freezeSchedule struct {
	// next is when the counters are frozen next.
	next time.Time
	// interval repeats the freeze; zero freezes once.
	interval time.Duration
	// ranges are the counters to freeze, as inclusive index ranges.
	ranges [][2]uint16
}

// onFreezeAtTime schedules a freeze.
//
// The request leads with a group 50 variation 2 object — the time of the first
// freeze and the interval between repeats in milliseconds — followed by counter
// headers naming what to freeze. A request with no counter headers freezes
// every counter, as an immediate freeze does.
//
// A time already past is refused with PARAMETER_ERROR when nothing repeats,
// since there is no moment left to freeze at; with an interval it is moved on
// to the next multiple that has not passed.
func (s *Session) onFreezeAtTime(frag app.Fragment) {
	var (
		haveTime bool
		first    time.Time
		interval time.Duration
		ranges   [][2]uint16
		named    bool
	)

	for _, h := range frag.Objects {
		switch {
		case h.Group == 50 && h.Variation == 2 && !haveTime:
			skip := h.Qualifier.IndexPrefix().Octets()
			if h.Count() != 1 || len(h.Data) < skip+objects.Time48Size+4 {
				s.iin = s.iin.Set(app.IINParameterError)
				return
			}
			d := h.Data[skip:]
			first = objects.ParseTime48(d).Time
			ms := uint32(d[6]) | uint32(d[7])<<8 | uint32(d[8])<<16 | uint32(d[9])<<24
			interval = time.Duration(ms) * time.Millisecond
			haveTime = true

		case h.Group == 20:
			named = true
			if !forEachPointRun(h, func(start, stop uint16) {
				ranges = append(ranges, [2]uint16{start, stop})
			}) {
				s.iin = s.iin.Set(app.IINParameterError)
				return
			}

		default:
			s.iin = s.iin.Set(app.IINObjectUnknown)
			return
		}
	}

	if !haveTime {
		s.iin = s.iin.Set(app.IINParameterError)
		return
	}
	if !named {
		ranges = [][2]uint16{{0, 0xFFFF}}
	}

	now := s.appl.Now()
	next := first
	if next.Before(now) {
		if interval <= 0 {
			s.iin = s.iin.Set(app.IINParameterError)
			return
		}
		periods := int64(now.Sub(first)/interval) + 1
		next = first.Add(time.Duration(periods) * interval)
	}

	// The same freeze asked for twice is one freeze: the request is
	// understood, and the operation is already waiting to run.
	for _, f := range s.freezes {
		if f.next.Equal(next) && f.interval == interval && slices.Equal(f.ranges, ranges) {
			s.iin = s.iin.Set(app.IINAlreadyExecuting)
			return
		}
	}

	if len(s.freezes) >= maxFreezeSchedules {
		s.iin = s.iin.Set(app.IINParameterError)
		return
	}
	s.freezes = append(s.freezes, freezeSchedule{next: next, interval: interval, ranges: ranges})
	s.log.Debug("freeze scheduled", "at", next, "interval", interval, "ranges", len(ranges))
}

// runFreezes performs every scheduled freeze that has come due.
func (s *Session) runFreezes(now time.Time) {
	if len(s.freezes) == 0 {
		return
	}

	kept := s.freezes[:0]
	for _, f := range s.freezes {
		if now.Before(f.next) {
			kept = append(kept, f)
			continue
		}

		// The frozen values carry the moment they were scheduled for, which is
		// what a master that asked for a freeze at 12:00:00 means by the time
		// of the frozen value, however late the tick that ran it.
		at := dnp3.Unsynchronized(f.next)
		if s.synchronized {
			at = dnp3.Now(f.next)
		}
		for _, r := range f.ranges {
			s.db.freezeCountersRange(r[0], r[1], at)
		}
		s.bump(func(st *Stats) { st.ScheduledFreezes++ })

		if f.interval <= 0 {
			continue // one-shot: done
		}
		periods := int64(now.Sub(f.next)/f.interval) + 1
		f.next = f.next.Add(time.Duration(periods) * f.interval)
		kept = append(kept, f)
	}
	s.freezes = kept
}
