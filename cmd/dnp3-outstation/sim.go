package main

import (
	"math"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/outstation"
)

// The simulation exists so the outstation has something to report that behaves
// like plant rather than like a random number generator. A master under
// development wants a breaker that stays open after it is tripped, an analog
// that ramps rather than jumping, and a counter that only ever counts up —
// because those are the behaviours its own logic will be wrong about.

// Signal is how an analog point moves.
type Signal string

// Signal shapes.
const (
	SignalSine       Signal = "sine"
	SignalRamp       Signal = "ramp"
	SignalRandomWalk Signal = "walk"
	SignalStep       Signal = "step"
	SignalFixed      Signal = "fixed"
)

// AnalogSim describes one simulated analog point.
type AnalogSim struct {
	Index  uint16  `yaml:"index"`
	Name   string  `yaml:"name"`
	Units  string  `yaml:"units"`
	Signal Signal  `yaml:"signal"`
	Min    float64 `yaml:"min"`
	Max    float64 `yaml:"max"`
	// Period is how long a full cycle takes for the periodic shapes.
	Period time.Duration `yaml:"period"`
	// Noise is the fraction of the range added as random jitter.
	Noise float64 `yaml:"noise"`
	// Deadband is reported to the master and used to suppress small changes.
	Deadband float64 `yaml:"deadband"`
	// Class is the event class the point's events go to.
	Class int `yaml:"class"`

	value float64
	phase float64
}

// BreakerSim describes a two-state device with a status input and a control
// output.
type BreakerSim struct {
	// StatusIndex is the binary input reporting the breaker's position, and
	// ControlIndex is the binary output that operates it.
	StatusIndex  uint16 `yaml:"status_index"`
	ControlIndex uint16 `yaml:"control_index"`
	Name         string `yaml:"name"`
	// Closed is the starting position.
	Closed bool `yaml:"closed"`
	// Interlocked refuses commands, standing in for a device that is racked
	// out or under local control.
	Interlocked bool `yaml:"interlocked"`
	// TravelTime is how long the breaker takes to move. A master that assumes
	// a control takes effect instantly is wrong about real plant.
	TravelTime time.Duration `yaml:"travel_time"`
	// Class is the event class for its status changes.
	Class int `yaml:"class"`

	movingUntil time.Time
	target      bool
}

// CounterSim describes an accumulator.
type CounterSim struct {
	Index uint16 `yaml:"index"`
	Name  string `yaml:"name"`
	// PerSecond is how fast it counts.
	PerSecond float64 `yaml:"per_second"`
	Class     int     `yaml:"class"`

	value float64
}

// Injection is a fault the simulator can be told to produce, so a master can
// be tested against the failures that are hard to arrange with real plant.
type Injection struct {
	// EventStorm generates this many binary events per second.
	EventStorm int
	// DeviceTrouble asserts the corresponding internal indication.
	DeviceTrouble bool
	// RestartAfter makes the outstation report a restart after this long.
	RestartAfter time.Duration
	// OfflineEvery takes points offline periodically, so a master's handling
	// of bad quality is exercised rather than assumed.
	OfflineEvery time.Duration
}

// Simulator drives an outstation's database.
type Simulator struct {
	mu sync.Mutex

	analogs  []AnalogSim
	breakers []BreakerSim
	counters []CounterSim

	inject Injection
	start  time.Time

	stormIndex  uint16
	lastOffline time.Time
	offlineNow  bool
	restarted   bool
}

// NewSimulator builds a simulator from a configuration.
func NewSimulator(cfg *Config) *Simulator {
	s := &Simulator{
		analogs:  cfg.Analogs,
		breakers: cfg.Breakers,
		counters: cfg.Counters,
		inject:   cfg.injection,
		start:    time.Now(),
	}
	for i := range s.analogs {
		a := &s.analogs[i]
		a.value = (a.Min + a.Max) / 2
		// Stagger the phases so a rack of points does not move in lockstep,
		// which makes an event stream look artificial and hides ordering bugs.
		a.phase = rand.Float64() * 2 * math.Pi
	}
	for i := range s.breakers {
		s.breakers[i].target = s.breakers[i].Closed
	}
	return s
}

// Apply writes the current simulated state into the outstation's database.
func (s *Simulator) Apply(sess *outstation.Session, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	elapsed := now.Sub(s.start).Seconds()

	// The offline injection flips quality for a while, then restores it.
	if s.inject.OfflineEvery > 0 && now.Sub(s.lastOffline) > s.inject.OfflineEvery {
		s.offlineNow = !s.offlineNow
		s.lastOffline = now
	}
	flags := dnp3.Online
	if s.offlineNow {
		flags = dnp3.CommLost
	}

	sess.Update(func(db *outstation.Database) {
		for i := range s.analogs {
			a := &s.analogs[i]
			a.value = a.next(elapsed)
			db.UpdateAnalog(a.Index, dnp3.Analog{
				Value: a.value,
				Flags: flags,
				Time:  dnp3.Now(now),
			})
		}

		for i := range s.breakers {
			b := &s.breakers[i]
			// A breaker mid-travel keeps its old position until it arrives.
			if !b.movingUntil.IsZero() && now.After(b.movingUntil) {
				b.Closed = b.target
				b.movingUntil = time.Time{}
			}
			db.UpdateBinary(b.StatusIndex, dnp3.Binary{
				Value: b.Closed,
				Flags: flags,
				Time:  dnp3.Now(now),
			})
			db.UpdateBinaryOutputStatus(b.ControlIndex, dnp3.BinaryOutputStatus{
				Value: b.Closed,
				Flags: flags,
				Time:  dnp3.Now(now),
			})
		}

		for i := range s.counters {
			c := &s.counters[i]
			c.value += c.PerSecond * tickSeconds
			db.UpdateCounter(c.Index, dnp3.Counter{
				Value: uint32(c.value),
				Flags: flags,
				Time:  dnp3.Now(now),
			})
		}

		// An event storm exercises the master's ability to keep up, and the
		// outstation's buffer-overflow reporting when it cannot.
		if n := s.inject.EventStorm; n > 0 && len(s.breakers) > 0 {
			per := int(float64(n) * tickSeconds)
			for range per {
				b := &s.breakers[int(s.stormIndex)%len(s.breakers)]
				s.stormIndex++
				db.UpdateBinary(b.StatusIndex, dnp3.Binary{
					Value: s.stormIndex%2 == 0,
					Flags: dnp3.Online,
					Time:  dnp3.Now(now),
				})
			}
		}
	})
}

// tickSeconds is the simulation step, in seconds.
const tickSeconds = 0.25

// next advances one analog point.
func (a *AnalogSim) next(elapsed float64) float64 {
	span := a.Max - a.Min
	mid := (a.Min + a.Max) / 2

	var v float64
	switch a.Signal {
	case SignalSine:
		period := a.Period.Seconds()
		if period <= 0 {
			period = 60
		}
		v = mid + (span/2)*math.Sin(2*math.Pi*elapsed/period+a.phase)

	case SignalRamp:
		period := a.Period.Seconds()
		if period <= 0 {
			period = 60
		}
		frac := math.Mod(elapsed, period) / period
		v = a.Min + span*frac

	case SignalRandomWalk:
		step := span * 0.02 * (2*rand.Float64() - 1)
		v = math.Min(math.Max(a.value+step, a.Min), a.Max)

	case SignalStep:
		period := a.Period.Seconds()
		if period <= 0 {
			period = 30
		}
		if math.Mod(elapsed, period) < period/2 {
			v = a.Min
		} else {
			v = a.Max
		}

	default: // fixed
		v = a.value
		if v == 0 {
			v = mid
		}
	}

	if a.Noise > 0 {
		v += span * a.Noise * (2*rand.Float64() - 1)
	}
	return v
}

// Operate applies a control to the simulated plant.
//
// This is what makes the simulator worth having: a trip actually opens the
// breaker, which raises a status event, which the master then receives — the
// whole loop a real integration exercises.
func (s *Simulator) Operate(controlIndex uint16, close bool, now time.Time) dnp3.CommandStatus {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.breakers {
		b := &s.breakers[i]
		if b.ControlIndex != controlIndex {
			continue
		}
		if b.Interlocked {
			// A real interlock refuses; reporting success would tell an
			// operator the breaker moved when it did not.
			return dnp3.CommandNotSupported
		}
		if !b.movingUntil.IsZero() {
			return dnp3.CommandAlreadyActive
		}

		b.target = close
		if b.TravelTime > 0 {
			b.movingUntil = now.Add(b.TravelTime)
		} else {
			b.Closed = close
		}
		return dnp3.CommandSuccess
	}
	return dnp3.CommandNotSupported
}

// wouldAccept reports whether a control would be accepted, without applying
// it.
//
// This is what a SELECT needs: the outstation's chance to say "not that point,
// not right now" before anything moves. A select that operated would defeat
// the entire two-pass sequence.
func (s *Simulator) wouldAccept(controlIndex uint16) dnp3.CommandStatus {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.breakers {
		b := &s.breakers[i]
		if b.ControlIndex != controlIndex {
			continue
		}
		switch {
		case b.Interlocked:
			return dnp3.CommandNotSupported
		case !b.movingUntil.IsZero():
			return dnp3.CommandAlreadyActive
		default:
			return dnp3.CommandSuccess
		}
	}
	return dnp3.CommandNotSupported
}

// SetAnalog applies an analog output command to a simulated setpoint.
func (s *Simulator) SetAnalog(index uint16, value float64) dnp3.CommandStatus {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.analogs {
		a := &s.analogs[i]
		if a.Index != index {
			continue
		}
		if value < a.Min || value > a.Max {
			return dnp3.CommandOutOfRange
		}
		a.Signal = SignalFixed
		a.value = value
		return dnp3.CommandSuccess
	}
	return dnp3.CommandNotSupported
}

// Describe renders the simulated plant, so a user starting the tool can see
// what the point indexes mean without reading the config file.
func (s *Simulator) Describe() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	var b strings.Builder
	b.WriteString("Simulated plant\n")

	if len(s.breakers) > 0 {
		b.WriteString("\n  Breakers (binary input / binary output)\n")
		for _, br := range s.breakers {
			state := "open"
			if br.Closed {
				state = "closed"
			}
			extra := ""
			if br.Interlocked {
				extra = "  [interlocked: commands refused]"
			}
			b.WriteString("    BI " + pad(br.StatusIndex) + " / BO " + pad(br.ControlIndex) +
				"  " + br.Name + " (" + state + ")" + extra + "\n")
		}
	}

	if len(s.analogs) > 0 {
		b.WriteString("\n  Analogs\n")
		for _, a := range s.analogs {
			b.WriteString("    AI " + pad(a.Index) + "  " + a.Name +
				"  " + string(a.Signal) + " " + trim(a.Min) + ".." + trim(a.Max) + " " + a.Units + "\n")
		}
	}

	if len(s.counters) > 0 {
		b.WriteString("\n  Counters\n")
		for _, c := range s.counters {
			b.WriteString("    CT " + pad(c.Index) + "  " + c.Name +
				"  " + trim(c.PerSecond) + "/s\n")
		}
	}
	return b.String()
}
