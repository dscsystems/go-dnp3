//go:build interop

package otfabric

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/outstation"
)

// fixture is the part of a dnp3-interop fixture the tests use. The image
// prints it (print-fixture), so expectations cannot drift from the image.
type fixture struct {
	Name       string `json:"name"`
	Outstation struct {
		Address       uint16 `json:"address"`
		MasterAddress uint16 `json:"masterAddress"`
		Unsolicited   bool   `json:"unsolicited"`
		NeedTime      bool   `json:"needTime"`
		ColdDelayMs   *int   `json:"coldRestartDelayMs"`
		WarmDelayMs   *int   `json:"warmRestartDelayMs"`
	} `json:"outstation"`

	BinaryInputs    []point `json:"binaryInputs"`
	DoubleBitInputs []point `json:"doubleBitInputs"`
	BinaryOutputs   []point `json:"binaryOutputs"`
	Counters        []point `json:"counters"`
	FrozenCounters  []point `json:"frozenCounters"`
	AnalogInputs    []point `json:"analogInputs"`
	AnalogOutputs   []point `json:"analogOutputs"`
	OctetStrings    []point `json:"octetStrings"`
}

type point struct {
	Index          uint16   `json:"index"`
	Value          any      `json:"value"`
	Flags          []string `json:"flags"`
	Class          int      `json:"class"`
	StaticVar      uint8    `json:"staticVariation"`
	EventVar       uint8    `json:"eventVariation"`
	Target         *uint16  `json:"target"`
	SelectRequired bool     `json:"selectRequired"`
	Min            *float64 `json:"min"`
	Max            *float64 `json:"max"`
}

func parseFixture(text []byte) (*fixture, error) {
	var f fixture
	if err := json.Unmarshal(text, &f); err != nil {
		return nil, err
	}
	return &f, nil
}

// flagBits are the quality names a fixture uses. The type-specific names share
// bit positions across the point types that have them.
var flagBits = map[string]dnp3.Flags{
	"ONLINE": dnp3.Online, "RESTART": dnp3.Restart, "COMM_LOST": dnp3.CommLost,
	"REMOTE_FORCED": dnp3.RemoteForced, "LOCAL_FORCED": dnp3.LocalForced,
	"CHATTER_FILTER": dnp3.ChatterFilter, "OVER_RANGE": dnp3.OverRange,
	"DISCONTINUITY": dnp3.Discontinuity, "REFERENCE_ERR": dnp3.ReferenceErr,
}

// flags is a point's quality: ONLINE when the fixture says nothing.
func (p point) flags() dnp3.Flags {
	if p.Flags == nil {
		return dnp3.Online
	}
	var f dnp3.Flags
	for _, name := range p.Flags {
		f |= flagBits[name]
	}
	return f
}

func (p point) bool() bool { b, _ := p.Value.(bool); return b }

func (p point) number() float64 { n, _ := p.Value.(float64); return n }

func (p point) octets() []byte {
	s, _ := p.Value.(string)
	b, _ := hex.DecodeString(s)
	return b
}

func classOf(n int) dnp3.Class {
	switch n {
	case 1:
		return dnp3.Class1
	case 2:
		return dnp3.Class2
	case 3:
		return dnp3.Class3
	}
	return dnp3.ClassNone
}

func find(points []point, index uint16) *point {
	for i := range points {
		if points[i].Index == index {
			return &points[i]
		}
	}
	return nil
}

// firstOutput returns the first output with the property, or nil.
func firstOutput(points []point, selectRequired bool) *point {
	for i := range points {
		if points[i].SelectRequired == selectRequired {
			return &points[i]
		}
	}
	return nil
}

// ---- the outstation under test ----------------------------------------------

// commandRecord is what reached our outstation's control handler.
type commandRecord struct {
	function string // select, operate, direct-operate, direct-operate-no-ack
	kind     string // crob, analog
	index    uint16
	code     uint8
	value    float64
	status   dnp3.CommandStatus
}

// device is our outstation, built from a fixture, and a record of what the
// reference masters asked of it.
type device struct {
	fx      *fixture
	session *outstation.Session

	mu       sync.Mutex
	commands []commandRecord
	times    []time.Time
	restarts []dnp3.RestartMode
	offset   time.Duration
}

// newDevice builds an outstation that holds the fixture's points. The
// library's database holds the indexes 0..n-1 of each type, which the
// baseline fixture does.
func newDevice(fx *fixture) (*device, error) {
	d := &device{fx: fx}
	cfg := outstation.Config{
		LocalAddr:  fx.Outstation.Address,
		RemoteAddr: fx.Outstation.MasterAddress,
		Database: outstation.DatabaseConfig{
			Binary:             len(fx.BinaryInputs),
			DoubleBitBinary:    len(fx.DoubleBitInputs),
			BinaryOutputStatus: len(fx.BinaryOutputs),
			Counter:            len(fx.Counters),
			FrozenCounter:      len(fx.FrozenCounters),
			Analog:             len(fx.AnalogInputs),
			AnalogOutputStatus: len(fx.AnalogOutputs),
			OctetString:        len(fx.OctetStrings),
		},
		Events:        outstation.EventBufferConfig{MaxEvents: 1000},
		SelectTimeout: 5 * time.Second,
		Unsolicited:   outstation.UnsolicitedConfig{Enabled: fx.Outstation.Unsolicited},
	}
	d.session = outstation.New(cfg, d, d)

	db := d.session.Database()
	now := dnp3.Now(time.Now())
	type kind struct {
		pt     dnp3.PointType
		points []point
		set    func(p point)
	}
	for _, k := range []kind{
		{dnp3.TypeBinary, fx.BinaryInputs, func(p point) {
			db.UpdateBinary(p.Index, dnp3.Binary{Value: p.bool(), Flags: p.flags(), Time: now})
		}},
		{dnp3.TypeDoubleBitBinary, fx.DoubleBitInputs, func(p point) {
			db.UpdateDoubleBit(p.Index, dnp3.DoubleBitBinary{Value: dnp3.DoubleBit(p.number()), Flags: p.flags(), Time: now})
		}},
		{dnp3.TypeBinaryOutputStatus, fx.BinaryOutputs, func(p point) {
			db.UpdateBinaryOutputStatus(p.Index, dnp3.BinaryOutputStatus{Value: p.bool(), Flags: p.flags(), Time: now})
		}},
		{dnp3.TypeCounter, fx.Counters, func(p point) {
			db.UpdateCounter(p.Index, dnp3.Counter{Value: uint32(p.number()), Flags: p.flags(), Time: now})
		}},
		{dnp3.TypeFrozenCounter, fx.FrozenCounters, func(p point) {
			db.UpdateFrozenCounter(p.Index, dnp3.FrozenCounter{Value: uint32(p.number()), Flags: p.flags(), Time: now})
		}},
		{dnp3.TypeAnalog, fx.AnalogInputs, func(p point) {
			db.UpdateAnalog(p.Index, dnp3.Analog{Value: p.number(), Flags: p.flags(), Time: now})
		}},
		{dnp3.TypeAnalogOutputStatus, fx.AnalogOutputs, func(p point) {
			db.UpdateAnalogOutputStatus(p.Index, dnp3.AnalogOutputStatus{Value: p.number(), Flags: p.flags(), Time: now})
		}},
		{dnp3.TypeOctetString, fx.OctetStrings, func(p point) {
			db.UpdateOctetString(p.Index, dnp3.OctetString(p.octets()))
		}},
	} {
		for i, p := range k.points {
			if int(p.Index) != i {
				return nil, fmt.Errorf("%v: the database holds the indexes 0..n-1, the fixture has %d at position %d", k.pt, p.Index, i)
			}
			// Variations first, then the value with no event class so that
			// loading it raises no event, then the class.
			cfg := outstation.PointConfig{StaticVariation: p.StaticVar, EventVariation: p.EventVar}
			if k.pt == dnp3.TypeOctetString {
				cfg.StaticVariation, cfg.EventVariation = 0, 0
			}
			if !db.Configure(k.pt, p.Index, cfg) {
				return nil, fmt.Errorf("%v[%d]: variations %d/%d refused", k.pt, p.Index, p.StaticVar, p.EventVar)
			}
			k.set(p)
			cfg.Class = classOf(p.Class)
			db.Configure(k.pt, p.Index, cfg)
		}
	}
	return d, nil
}

// Application. The clock is a correction on the host's, not a change to it.

func (d *device) Now() time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()
	return time.Now().Add(d.offset)
}

func (d *device) WriteAbsoluteTime(t time.Time) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.offset = time.Until(t)
	d.times = append(d.times, t)
	return true
}

func (d *device) ColdRestart() time.Duration {
	return d.restart(dnp3.ColdRestart, d.fx.Outstation.ColdDelayMs)
}
func (d *device) WarmRestart() time.Duration {
	return d.restart(dnp3.WarmRestart, d.fx.Outstation.WarmDelayMs)
}
func (d *device) SupportsWriteTime() bool { return true }

func (d *device) restart(mode dnp3.RestartMode, ms *int) time.Duration {
	d.mu.Lock()
	d.restarts = append(d.restarts, mode)
	d.mu.Unlock()
	if ms == nil {
		return 0
	}
	return time.Duration(*ms) * time.Millisecond
}

// Control handler. The fixture decides what is accepted: only the outputs it
// lists, a select required where it says so, and the ranges of the analog
// outputs. An output's target input follows the output.

func functionName(op outstation.OperateType) string {
	switch op {
	case outstation.OperateDirect:
		return "direct-operate"
	case outstation.OperateDirectNoAck:
		return "direct-operate-no-ack"
	}
	return "operate"
}

func (d *device) record(c commandRecord) {
	d.mu.Lock()
	d.commands = append(d.commands, c)
	d.mu.Unlock()
}

func (d *device) crob(index uint16, c dnp3.ControlRelayOutputBlock, direct bool) (dnp3.CommandStatus, bool) {
	p := find(d.fx.BinaryOutputs, index)
	if p == nil {
		return dnp3.CommandNotSupported, false
	}
	switch uint8(c.Code) {
	case 0x03, 0x41: // LATCH_ON; PULSE_ON with CLOSE
		if direct && p.SelectRequired {
			return dnp3.CommandNotSupported, false
		}
		return dnp3.CommandSuccess, true
	case 0x04, 0x81: // LATCH_OFF; PULSE_ON with TRIP
		if direct && p.SelectRequired {
			return dnp3.CommandNotSupported, false
		}
		return dnp3.CommandSuccess, false
	}
	return dnp3.CommandNotSupported, false
}

func (d *device) SelectCROB(index uint16, c dnp3.ControlRelayOutputBlock) dnp3.CommandStatus {
	status, _ := d.crob(index, c, false)
	d.record(commandRecord{"select", "crob", index, uint8(c.Code), 0, status})
	return status
}

func (d *device) OperateCROB(index uint16, c dnp3.ControlRelayOutputBlock, op outstation.OperateType) dnp3.CommandStatus {
	status, value := d.crob(index, c, op != outstation.OperateSelected)
	if status == dnp3.CommandSuccess {
		p := find(d.fx.BinaryOutputs, index)
		now := dnp3.Now(time.Now())
		db := d.session.Database()
		db.UpdateBinaryOutputStatus(index, dnp3.BinaryOutputStatus{Value: value, Flags: p.flags(), Time: now})
		if p.Target != nil {
			if t := find(d.fx.BinaryInputs, *p.Target); t != nil {
				db.UpdateBinary(t.Index, dnp3.Binary{Value: value, Flags: t.flags(), Time: now})
			}
		}
	}
	d.record(commandRecord{functionName(op), "crob", index, uint8(c.Code), 0, status})
	return status
}

func (d *device) analog(index uint16, value float64, direct bool) dnp3.CommandStatus {
	p := find(d.fx.AnalogOutputs, index)
	if p == nil {
		return dnp3.CommandNotSupported
	}
	if (p.Min != nil && value < *p.Min) || (p.Max != nil && value > *p.Max) {
		return dnp3.CommandOutOfRange
	}
	if direct && p.SelectRequired {
		return dnp3.CommandNotSupported
	}
	return dnp3.CommandSuccess
}

func (d *device) SelectAnalog(index uint16, v outstation.AnalogOutput) dnp3.CommandStatus {
	status := d.analog(index, v.Value, false)
	d.record(commandRecord{"select", "analog", index, 0, v.Value, status})
	return status
}

func (d *device) OperateAnalog(index uint16, v outstation.AnalogOutput, op outstation.OperateType) dnp3.CommandStatus {
	status := d.analog(index, v.Value, op != outstation.OperateSelected)
	if status == dnp3.CommandSuccess {
		p := find(d.fx.AnalogOutputs, index)
		now := dnp3.Now(time.Now())
		db := d.session.Database()
		db.UpdateAnalogOutputStatus(index, dnp3.AnalogOutputStatus{Value: v.Value, Flags: p.flags(), Time: now})
		if p.Target != nil {
			if t := find(d.fx.AnalogInputs, *p.Target); t != nil {
				db.UpdateAnalog(t.Index, dnp3.Analog{Value: v.Value, Flags: t.flags(), Time: now})
			}
		}
	}
	d.record(commandRecord{functionName(op), "analog", index, 0, v.Value, status})
	return status
}

// commandsWith returns the recorded commands with the function.
func (d *device) commandsWith(function string) []commandRecord {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []commandRecord
	for _, c := range d.commands {
		if c.function == function {
			out = append(out, c)
		}
	}
	return out
}

func (d *device) timeWrites() []time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.times)
}

func (d *device) restartRequests() []dnp3.RestartMode {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.restarts)
}
