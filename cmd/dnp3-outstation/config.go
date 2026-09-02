package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/outstation"
	"gopkg.in/yaml.v3"
)

// Config is the whole simulated device.
type Config struct {
	// Address is this outstation's link address, Master the address it
	// expects to be polled from.
	Address uint16 `yaml:"address"`
	Master  uint16 `yaml:"master"`

	// Points sizes the database. Zero counts are derived from the simulated
	// plant, so a simple configuration need not repeat itself.
	Points PointCounts `yaml:"points"`

	Breakers  []BreakerSim  `yaml:"breakers"`
	Analogs   []AnalogSim   `yaml:"analogs"`
	Counters  []CounterSim  `yaml:"counters"`
	Setpoints []SetpointSim `yaml:"setpoints"`

	// Unsolicited configures pushing events without being polled.
	Unsolicited UnsolicitedYAML `yaml:"unsolicited"`

	// Files configures group 70 file transfer.
	Files FilesYAML `yaml:"files"`

	// Device is what the outstation says about itself over group 0.
	Device DeviceYAML `yaml:"device"`

	// MaxEvents caps the event buffer.
	MaxEvents int `yaml:"max_events"`

	injection Injection

	// attributes is Device resolved into what the library serves, filled in
	// by Validate so a bad entry is reported once rather than at every read.
	attributes []dnp3.Attribute
}

// PointCounts sizes each point type.
type PointCounts struct {
	Binary             int `yaml:"binary"`
	Analog             int `yaml:"analog"`
	Counter            int `yaml:"counter"`
	FrozenCounter      int `yaml:"frozen_counter"`
	BinaryOutputStatus int `yaml:"binary_output"`
	AnalogOutputStatus int `yaml:"analog_output"`
}

// FilesYAML configures what a master may read and write over file transfer.
//
// The default is a small filesystem synthesised in memory, so the feature can
// be exercised without preparing anything. Naming a directory serves that
// directory instead — and only that directory: the handler is rooted, so a
// master cannot climb out of it.
type FilesYAML struct {
	// Directory serves real files instead of the simulated ones. Empty uses
	// the in-memory device.
	Directory string `yaml:"directory"`
	// ReadOnly refuses writes and deletes.
	ReadOnly bool `yaml:"read_only"`
	// Disabled turns file transfer off entirely, so the outstation answers
	// the file function codes the way a device without them does. It is how a
	// master's handling of an unsupported feature gets tested.
	Disabled bool `yaml:"disabled"`
	// BlockSize caps the transfer block. Zero uses the library's default.
	BlockSize uint16 `yaml:"block_size"`
	// Timeout closes a transfer that has gone quiet. Zero uses the default.
	Timeout time.Duration `yaml:"timeout"`
}

// UnsolicitedYAML mirrors the library's unsolicited configuration.
type UnsolicitedYAML struct {
	Enabled   bool          `yaml:"enabled"`
	HoldTime  time.Duration `yaml:"hold_time"`
	MaxEvents int           `yaml:"max_events"`
}

// DefaultConfig is a small substation: two feeders with breakers, the analogs
// a feeder actually reports, and an energy counter.
//
// It exists so the tool is useful with no configuration at all — a master
// under development needs something to poll before anyone writes a YAML file.
func DefaultConfig() *Config {
	return &Config{
		Address: 10,
		Master:  1,
		Breakers: []BreakerSim{
			{StatusIndex: 0, ControlIndex: 0, Name: "Feeder 1 breaker", Closed: true,
				TravelTime: 200 * time.Millisecond, Class: 1},
			{StatusIndex: 1, ControlIndex: 1, Name: "Feeder 2 breaker", Closed: true,
				TravelTime: 200 * time.Millisecond, Class: 1},
			{StatusIndex: 2, ControlIndex: 2, Name: "Bus tie", Closed: false,
				TravelTime: 500 * time.Millisecond, Class: 1},
			{StatusIndex: 3, ControlIndex: 3, Name: "Earth switch (racked out)",
				Interlocked: true, Class: 1},
		},
		Analogs: []AnalogSim{
			{Index: 0, Name: "Bus voltage", Units: "kV", Signal: SignalSine,
				Min: 10.8, Max: 11.2, Period: 45 * time.Second, Noise: 0.01, Deadband: 0.05, Class: 2},
			{Index: 1, Name: "Feeder 1 current", Units: "A", Signal: SignalRandomWalk,
				Min: 0, Max: 400, Noise: 0.005, Deadband: 5, Class: 2},
			{Index: 2, Name: "Feeder 2 current", Units: "A", Signal: SignalRandomWalk,
				Min: 0, Max: 400, Noise: 0.005, Deadband: 5, Class: 2},
			{Index: 3, Name: "Transformer temperature", Units: "degC", Signal: SignalRamp,
				Min: 35, Max: 85, Period: 120 * time.Second, Deadband: 1, Class: 3},
			{Index: 4, Name: "Tap position", Units: "", Signal: SignalStep,
				Min: 7, Max: 9, Period: 90 * time.Second, Class: 2},
		},
		Counters: []CounterSim{
			{Index: 0, Name: "Feeder 1 energy", PerSecond: 12, Class: 3},
			{Index: 1, Name: "Feeder 2 energy", PerSecond: 8, Class: 3},
		},
		Setpoints: []SetpointSim{
			// Bounded, so a master's out-of-range handling has something to run
			// into. The ranges match the measurements at the same indexes, so
			// every setpoint the device accepts is also one the simulated plant
			// can reach — writing one moves the analog input with it.
			{Index: 0, Name: "Voltage setpoint", Units: "kV",
				Min: 10.8, Max: 11.2, Initial: 11.0, Class: 2},
			{Index: 4, Name: "Tap position setpoint", Units: "",
				Min: 7, Max: 9, Initial: 8, Class: 2},
		},
		Unsolicited: UnsolicitedYAML{HoldTime: 500 * time.Millisecond},
		Device:      DefaultDevice(),
		MaxEvents:   1000,
	}
}

// LoadConfig reads a configuration file.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := DefaultConfig()
	// Start from a blank slate so a file that lists two analogs does not
	// silently inherit the five defaults — and the same for setpoints, or a
	// device configured with one output would answer for three.
	cfg.Breakers, cfg.Analogs, cfg.Counters, cfg.Setpoints = nil, nil, nil, nil

	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true) // a typo must fail, not be ignored
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// derivePoints sizes the database from the simulated plant when the
// configuration does not say.
func (c *Config) derivePoints() {
	need := func(have int, indexes ...uint16) int {
		maxIdx := -1
		for _, i := range indexes {
			maxIdx = max(maxIdx, int(i))
		}
		return max(have, maxIdx+1)
	}

	for _, b := range c.Breakers {
		c.Points.Binary = need(c.Points.Binary, b.StatusIndex)
		c.Points.BinaryOutputStatus = need(c.Points.BinaryOutputStatus, b.ControlIndex)
	}
	for _, a := range c.Analogs {
		c.Points.Analog = need(c.Points.Analog, a.Index)
	}
	for _, ct := range c.Counters {
		c.Points.Counter = need(c.Points.Counter, ct.Index)
	}
	if c.Points.FrozenCounter == 0 {
		c.Points.FrozenCounter = c.Points.Counter
	}
	if c.Points.AnalogOutputStatus == 0 {
		// An outstation with analogs and no configured setpoints gets one
		// output per input, which is the shape of most devices and what this
		// tool did before setpoints could be described.
		c.Points.AnalogOutputStatus = c.Points.Analog
	}
	for _, sp := range c.Setpoints {
		c.Points.AnalogOutputStatus = need(c.Points.AnalogOutputStatus, sp.Index)
	}
}

// Validate reports configuration problems that would otherwise show up as
// silently missing points.
func (c *Config) Validate() error {
	if c.Address == c.Master {
		return fmt.Errorf("the outstation and master addresses are both %d; "+
			"a station cannot poll itself", c.Address)
	}

	attrs, err := c.Device.attributes()
	if err != nil {
		return err
	}
	c.attributes = attrs

	// Everything downstream — the simulator, the device attributes, the
	// banner — reads the derived point counts, and Validate is the last thing
	// that runs before any of it.
	c.derivePoints()

	seen := map[string]string{}
	dup := func(kind string, index uint16, name string) error {
		key := kind + ":" + strconv.Itoa(int(index))
		if prev, ok := seen[key]; ok {
			return fmt.Errorf("%s index %d is used by both %q and %q", kind, index, prev, name)
		}
		seen[key] = name
		return nil
	}

	for _, b := range c.Breakers {
		if err := dup("binary input", b.StatusIndex, b.Name); err != nil {
			return err
		}
		if err := dup("binary output", b.ControlIndex, b.Name); err != nil {
			return err
		}
	}
	for _, a := range c.Analogs {
		if err := dup("analog", a.Index, a.Name); err != nil {
			return err
		}
		if a.Max < a.Min {
			return fmt.Errorf("analog %d (%s) has max %v below min %v", a.Index, a.Name, a.Max, a.Min)
		}
	}
	for _, ct := range c.Counters {
		if err := dup("counter", ct.Index, ct.Name); err != nil {
			return err
		}
	}
	for _, sp := range c.Setpoints {
		if err := dup("analog output", sp.Index, sp.Name); err != nil {
			return err
		}
		if sp.Max < sp.Min {
			return fmt.Errorf("setpoint %d (%s) has max %v below min %v",
				sp.Index, sp.Name, sp.Max, sp.Min)
		}
		if sp.bounded() && !sp.accepts(sp.Initial) {
			return fmt.Errorf("setpoint %d (%s) starts at %v, outside its %v..%v range",
				sp.Index, sp.Name, sp.Initial, sp.Min, sp.Max)
		}
	}
	return nil
}

// outstationConfig turns the file into the library's configuration.
func (c *Config) outstationConfig() outstation.Config {
	c.derivePoints()

	return outstation.Config{
		LocalAddr:  c.Address,
		RemoteAddr: c.Master,
		Database: outstation.DatabaseConfig{
			Binary:             c.Points.Binary,
			Analog:             c.Points.Analog,
			Counter:            c.Points.Counter,
			FrozenCounter:      c.Points.FrozenCounter,
			BinaryOutputStatus: c.Points.BinaryOutputStatus,
			AnalogOutputStatus: c.Points.AnalogOutputStatus,
			DefaultClass:       dnp3.Class1,
		},
		Events:     outstation.EventBufferConfig{MaxEvents: c.MaxEvents},
		Attributes: c.attributes,
		Unsolicited: outstation.UnsolicitedConfig{
			Enabled:   c.Unsolicited.Enabled,
			HoldTime:  c.Unsolicited.HoldTime,
			MaxEvents: c.Unsolicited.MaxEvents,
		},
	}
}

// applyPointConfig assigns per-point classes and deadbands after the session
// is built.
func (c *Config) applyPointConfig(db *outstation.Database) {
	class := func(n int) dnp3.Class {
		switch n {
		case 1:
			return dnp3.Class1
		case 2:
			return dnp3.Class2
		case 3:
			return dnp3.Class3
		case 0:
			return dnp3.ClassNone
		default:
			return dnp3.Class1
		}
	}

	for _, sp := range c.Setpoints {
		db.Configure(dnp3.TypeAnalogOutputStatus, sp.Index, outstation.PointConfig{
			Class: class(sp.Class),
			// A setpoint carries fractions as readily as a measurement does,
			// and the default 32-bit integer variation would truncate the one
			// the master just wrote — reporting back something it did not send.
			StaticVariation: 3, // g40v3
			EventVariation:  7, // g42v7, float with time
		})
	}

	for _, b := range c.Breakers {
		db.Configure(dnp3.TypeBinary, b.StatusIndex,
			outstation.PointConfig{Class: class(b.Class)})
		db.Configure(dnp3.TypeBinaryOutputStatus, b.ControlIndex,
			outstation.PointConfig{Class: class(b.Class)})
	}
	for _, a := range c.Analogs {
		db.Configure(dnp3.TypeAnalog, a.Index, outstation.PointConfig{
			Class:    class(a.Class),
			Deadband: a.Deadband,
			// Analogs carry fractions, so the default 32-bit integer variation
			// would truncate every reading. Single precision is the narrowest
			// encoding that does not.
			StaticVariation: 5, // g30v5
			EventVariation:  7, // g32v7, float with time
		})
	}
	for _, ct := range c.Counters {
		db.Configure(dnp3.TypeCounter, ct.Index,
			outstation.PointConfig{Class: class(ct.Class)})
	}
}

// pad renders a point index in a fixed width so a listing lines up.
func pad(i uint16) string {
	s := strconv.Itoa(int(i))
	for len(s) < 3 {
		s = " " + s
	}
	return s
}

// trim renders a float without trailing zeros.
func trim(v float64) string {
	s := strconv.FormatFloat(v, 'f', -1, 64)
	return s
}
