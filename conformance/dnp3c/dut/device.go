// Copyright (C) 2026 Ricardo Olsen / DSC Systems.
//
// This program is free software: you can redistribute it and/or modify it
// under the terms of the GNU General Public License as published by the Free
// Software Foundation, either version 3 of the License, or (at your option)
// any later version. It is distributed WITHOUT ANY WARRANTY; without even the
// implied warranty of MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.
// See the GNU General Public License for more details, in the LICENSE file at
// the root of this repository or at <https://www.gnu.org/licenses/>.

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/channel"
	"github.com/dscsystems/go-dnp3/outstation"
)

// The device's shape. device.ini describes the same device to dnp3c, and the
// two have to agree: a point count or class changed here and not there makes
// the harness test a device that does not exist.
const (
	outstationAddr = 1024
	masterAddr     = 1

	// pointsPerType is how many points of each type the device has.
	pointsPerType = 20

	// maxEvents is the event buffer's capacity, and what device.ini gives as
	// the maximum for each type: go-dnp3 has one buffer shared by every
	// type, so a test that fills it with one type fills it at this count.
	maxEvents = 1000

	selectTimeout  = 2 * time.Second
	confirmTimeout = 2 * time.Second
	unsolTimeout   = 2 * time.Second
	linkTimeout    = time.Second
	linkRetries    = 3
	fragmentSize   = 2048

	// maxControls is how many controls one request may carry, device.ini's
	// *.maxPointsInSingleRequest.
	maxControls = 4

	// syncInterval is how long after the master sets the clock the device
	// asks for the time again, device.ini's
	// timesync.maxTimeBetweenSyncRequestsMs, and clockHoldover how much
	// longer it trusts the clock: timesync.timeToUnsynchronizedMs is the two
	// together.
	syncInterval  = 10 * time.Second
	clockHoldover = 2 * time.Second

	// unlimitedRetries stands in for "retry forever", which UnsolicitedConfig
	// spells as a count.
	unlimitedRetries = math.MaxInt32
)

// eventClass is where each type's events go, as device.ini's *.eventsClass
// options say. Binary inputs from binaryClassOthers up are the exception: 3.1r1's
// 8.26 reassigns the classes of binary inputs that start in class 2 and 3, so
// a few do, and the generators leave them alone.
var eventClass = map[kind]dnp3.Class{
	kindBinary:        dnp3.Class1,
	kindDoubleBit:     dnp3.Class1,
	kindCounter:       dnp3.Class2,
	kindFrozenCounter: dnp3.Class2,
	kindAnalog:        dnp3.Class3,
}

// eventVariation is each type's event variation, where it is not the
// outstation's default. Subset level 3 has counter and analog events without
// time (g22v1, g23v1, g32v1); the defaults, with time, are level 4.
var eventVariation = map[kind]uint8{
	kindCounter:       1,
	kindFrozenCounter: 1,
	kindAnalog:        1,
}

// binaryClassOthers is the first binary input outside class 1: 16 and 17 are
// in class 2, 18 and 19 in class 3.
const binaryClassOthers = 16

// pointClass is the class a point starts in.
func pointClass(k kind, i int) dnp3.Class {
	if k == kindBinary && i >= binaryClassOthers {
		if i < binaryClassOthers+2 {
			return dnp3.Class2
		}
		return dnp3.Class3
	}
	return eventClass[k]
}

// generated is how many points of a kind the generators change.
func generated(k kind) int {
	if k == kindBinary {
		return binaryClassOthers
	}
	return pointsPerType
}

// settings are what the procedures reconfigure. Like a real device's
// configuration they take effect when the device restarts, which is how
// every procedure that changes one uses it: change, then cycle power.
type settings struct {
	linkConfirms bool
	selfAddress  bool
	fragmentSize int
	unsolicited  bool
	unsolTimeout time.Duration
	unsolRetries int
	master       uint16
	uninstalled  map[kind]bool
	// noColdRestart turns cold restart support off. Unlike the rest it
	// applies at once, as 3.1r1's 8.8.2 expects.
	noColdRestart bool
}

func defaultSettings() settings {
	return settings{
		fragmentSize: fragmentSize,
		// Off until a procedure turns it on: the procedures that test
		// unsolicited responses enable them and cycle power first, and the
		// rest take a startup unsolicited response for a fault.
		unsolicited:  false,
		unsolTimeout: unsolTimeout,
		unsolRetries: unlimitedRetries,
		master:       masterAddr,
		uninstalled:  map[kind]bool{},
	}
}

func (s settings) clone() settings {
	c := s
	c.uninstalled = make(map[kind]bool, len(s.uninstalled))
	for k, v := range s.uninstalled {
		c.uninstalled[k] = v
	}
	return c
}

// count is how many points of a type the device has with these settings.
func (s settings) count(k kind) int {
	if s.uninstalled[k] {
		return 0
	}
	return pointsPerType
}

// device is the outstation and everything the plugin needs to know about it.
type device struct {
	addr string
	log  *slog.Logger
	// level is the subset level the device is configured for, which limits
	// the variations it may report in.
	level int

	mu       sync.Mutex
	settings settings
	// running is the settings the session was started with, which say what
	// points it has; settings may have changed since, for the next start.
	running settings
	sess    *outstation.Session
	clock   *clock
	ch      channel.Channel
	cancel  context.CancelFunc
	done    chan struct{}

	// values carries the inputs across a restart. A power cycle does not
	// change what the plant is doing, so the device comes back reporting the
	// values it had.
	values *snapshot

	// local is the remote supervisory state: the global switch, and the
	// outputs switched to local one by one.
	globalLocal bool
	localPoints map[uint16]bool

	// operations records the outputs the master operated since the last
	// check of them.
	operations []operation

	// attributes are the values the device reports in group 0, kept to
	// check a read against, and updated by the master's writes.
	attributes map[uint8]dnp3.Attribute

	track tracker
}

// operation is an output the master operated.
type operation struct {
	kind  kind
	index uint16
}

func newDevice(addr string, level int, log *slog.Logger) *device {
	return &device{
		addr:        addr,
		log:         log,
		level:       level,
		settings:    defaultSettings(),
		localPoints: map[uint16]bool{},
	}
}

// reset puts the device back to how it ships, for the start of a procedure.
func (d *device) reset() error {
	d.stop()
	d.mu.Lock()
	d.settings = defaultSettings()
	d.values = nil
	d.globalLocal = false
	d.localPoints = map[uint16]bool{}
	d.operations = nil
	d.track = tracker{}
	d.mu.Unlock()
	return d.start()
}

// restart cycles the outstation's power with the current settings.
func (d *device) restart() error {
	d.stop()
	return d.start()
}

// start brings the outstation up and returns once it is listening.
func (d *device) start() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	st := d.settings
	clk := &clock{noCold: st.noColdRestart}
	cfg := outstation.Config{
		LocalAddr:  outstationAddr,
		RemoteAddr: st.master,
		Database: outstation.DatabaseConfig{
			Binary:             st.count(kindBinary),
			DoubleBitBinary:    st.count(kindDoubleBit),
			Counter:            st.count(kindCounter),
			FrozenCounter:      st.count(kindFrozenCounter),
			Analog:             st.count(kindAnalog),
			BinaryOutputStatus: st.count(kindBinaryOutput),
			AnalogOutputStatus: st.count(kindAnalogOutput),
		},
		Events:         outstation.EventBufferConfig{MaxEvents: maxEvents},
		MaxTxFragment:  st.fragmentSize,
		ConfirmTimeout: confirmTimeout,
		SelectTimeout:  selectTimeout,
		Clock: outstation.ClockConfig{
			SyncInterval: syncInterval,
			Holdover:     clockHoldover,
		},
		MaxControlsPerRequest: maxControls,
		Unsolicited: outstation.UnsolicitedConfig{
			Enabled:        st.unsolicited,
			ConfirmTimeout: st.unsolTimeout,
			MaxRetries:     st.unsolRetries,
		},
		Attributes:         configuredAttributes(),
		WritableAttributes: []outstation.AttributeID{{Set: 0, Variation: 245}, {Set: 0, Variation: 247}},
		AttributeWrite:     d.attributeWritten,
		SelfAddress:        st.selfAddress,
		UseLinkConfirms:    st.linkConfirms,
		LinkRetries:        linkRetries,
		LinkTimeout:        linkTimeout,
		Log:                d.log,
	}
	d.attributes = expectedAttributes(cfg)

	sess := outstation.New(cfg, clk, d)
	d.initialise(sess.Database(), st)
	if d.globalLocal || len(d.localPoints) > 0 {
		sess.SetIndication(outstation.IndicationLocalControl, true)
	}

	ch := channel.TCPServer(d.addr)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := sess.Run(ctx, ch); err != nil && !errors.Is(err, context.Canceled) {
			d.log.Error("outstation stopped", "err", err)
		}
	}()

	// The harness connects as soon as this returns, without retrying, so
	// "listening" has to mean the socket is bound, not that Run was called.
	deadline := time.Now().Add(5 * time.Second)
	for channel.TCPServerAddr(ch) == nil {
		if time.Now().After(deadline) {
			cancel()
			<-done
			return fmt.Errorf("outstation did not start listening on %s", d.addr)
		}
		time.Sleep(5 * time.Millisecond)
	}

	d.sess, d.clock, d.ch, d.cancel, d.done = sess, clk, ch, cancel, done
	d.running = st
	return nil
}

// stop powers the outstation off, keeping its inputs for the next start.
func (d *device) stop() {
	d.mu.Lock()
	sess, ch, cancel, done := d.sess, d.ch, d.cancel, d.done
	d.sess, d.ch, d.cancel, d.done = nil, nil, nil, nil
	d.mu.Unlock()
	if sess == nil {
		return
	}

	values := d.snapshot(sess)
	cancel()
	// The listener and any connection go with the channel; there is nothing
	// useful to do about one that objects to being closed.
	_ = ch.Close()
	<-done

	d.mu.Lock()
	d.values = values
	d.mu.Unlock()
}

// initialise loads the points' values and configuration into a new database.
//
// The values go in before the points are assigned to classes. Loaded the
// other way round, every point would raise an event for changing from the
// database's zero value to the value it already had before the restart.
func (d *device) initialise(db *outstation.Database, st settings) {
	online := dnp3.Online
	at := dnp3.Unsynchronized(time.Now())
	v := d.values

	for i := range st.count(kindBinary) {
		val := dnp3.Binary{Flags: online, Time: at}
		if v != nil && i < len(v.binary) {
			val = v.binary[i]
		}
		db.UpdateBinary(uint16(i), val)
	}
	for i := range st.count(kindDoubleBit) {
		val := dnp3.DoubleBitBinary{Value: dnp3.DoubleBitDeterminedOff, Flags: online, Time: at}
		if v != nil && i < len(v.doubleBit) {
			val = v.doubleBit[i]
		}
		db.UpdateDoubleBit(uint16(i), val)
	}
	for i := range st.count(kindCounter) {
		val := dnp3.Counter{Flags: online, Time: at}
		if v != nil && i < len(v.counter) {
			val = v.counter[i]
		}
		db.UpdateCounter(uint16(i), val)
	}
	for i := range st.count(kindFrozenCounter) {
		val := dnp3.FrozenCounter{Flags: online, Time: at}
		if v != nil && i < len(v.frozenCounter) {
			val = v.frozenCounter[i]
		}
		db.UpdateFrozenCounter(uint16(i), val)
	}
	for i := range st.count(kindAnalog) {
		val := dnp3.Analog{Flags: online, Time: at}
		if v != nil && i < len(v.analog) {
			val = v.analog[i]
		}
		db.UpdateAnalog(uint16(i), val)
	}
	for i := range st.count(kindBinaryOutput) {
		val := dnp3.BinaryOutputStatus{Flags: online, Time: at}
		if v != nil && i < len(v.binaryOutput) {
			val = v.binaryOutput[i]
		}
		if d.isLocal(uint16(i)) {
			val.Flags = val.Flags.Clear(dnp3.Online)
		} else {
			val.Flags = val.Flags.Set(dnp3.Online)
		}
		db.UpdateBinaryOutputStatus(uint16(i), val)
	}
	for i := range st.count(kindAnalogOutput) {
		val := dnp3.AnalogOutputStatus{Flags: online, Time: at}
		if v != nil && i < len(v.analogOutput) {
			val = v.analogOutput[i]
		}
		db.UpdateAnalogOutputStatus(uint16(i), val)
	}

	// Subset level 2 has 16-bit analog output status (g40v2) only.
	if d.level == 2 {
		for i := range st.count(kindAnalogOutput) {
			db.Configure(dnp3.TypeAnalogOutputStatus, uint16(i), outstation.PointConfig{StaticVariation: 2})
		}
	}
	for k := range eventClass {
		for i := range st.count(k) {
			db.Configure(k.pointType(), uint16(i), outstation.PointConfig{
				Class:          pointClass(k, i),
				EventVariation: eventVariation[k],
			})
		}
	}
}

// update applies fn to the database and waits until it has been applied.
//
// Session.Update returns before applying a batch while a connection is being
// served. The plugin's next step may be a check of what the batch did, so
// returning early would race it.
func (d *device) update(fn func(db *outstation.Database, now dnp3.Timestamp)) error {
	d.mu.Lock()
	sess, clk := d.sess, d.clock
	d.mu.Unlock()
	if sess == nil {
		return errors.New("the outstation is not running")
	}
	done := make(chan struct{})
	sess.Update(func(db *outstation.Database) {
		defer close(done)
		fn(db, clk.stamp())
	})
	select {
	case <-done:
		return nil
	case <-time.After(10 * time.Second):
		return errors.New("the outstation did not apply an update within 10 s")
	}
}

// isLocal reports whether a binary output is under local control. d.mu held.
func (d *device) isLocal(index uint16) bool {
	return d.globalLocal || d.localPoints[index]
}

// setLocal applies the remote supervisory state to the outputs' flags and the
// LOCAL_CONTROL indication.
//
// In local mode an output clears ONLINE and keeps its state and every other
// flag (IEEE 1815-2012 Table 11-5): it is still there, it is just not the
// master's to command.
func (d *device) setLocal(global bool, points map[uint16]bool) error {
	d.mu.Lock()
	d.globalLocal, d.localPoints = global, points
	sess := d.sess
	n := d.running.count(kindBinaryOutput)
	anyLocal := global || len(points) > 0
	d.mu.Unlock()
	if sess == nil {
		return errors.New("the outstation is not running")
	}
	sess.SetIndication(outstation.IndicationLocalControl, anyLocal)
	return d.update(func(db *outstation.Database, _ dnp3.Timestamp) {
		for i := range n {
			v, _, _ := db.BinaryOutputStatus(uint16(i))
			if global || points[uint16(i)] {
				v.Flags = v.Flags.Clear(dnp3.Online)
			} else {
				v.Flags = v.Flags.Set(dnp3.Online)
			}
			db.UpdateBinaryOutputStatus(uint16(i), v)
		}
	})
}

// ---------- Commands ----------

// The codes every binary output accepts. dnp3c's single-function and
// complementary roles (device.ini's binaryOutput.*Point options) test only
// that an output carries out the codes its role names, and the procedures
// that send several commands at once send the primary code to every
// installed output, so each output takes them all.
const (
	codePulseOn    = dnp3.ControlPulseOn
	codeLatchOn    = dnp3.ControlLatchOn
	codeLatchOff   = dnp3.ControlLatchOff
	codePulseClose = dnp3.ControlClose | dnp3.ControlPulseOn
	codePulseTrip  = dnp3.ControlTrip | dnp3.ControlPulseOn
)

var crobCodes = []dnp3.ControlCode{codePulseOn, codeLatchOn, codeLatchOff, codePulseClose, codePulseTrip}

// HasOutput says which outputs the device has, for outstation.OutputPoints:
// the ones installed. With every one uninstalled, the database has no output
// status points to go by.
func (d *device) HasOutput(pt dnp3.PointType, index uint16) bool {
	k := kindBinaryOutput
	if pt == dnp3.TypeAnalogOutputStatus {
		k = kindAnalogOutput
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return int(index) < d.running.count(k)
}

func (d *device) crobStatus(index uint16, c dnp3.ControlRelayOutputBlock) dnp3.CommandStatus {
	d.mu.Lock()
	defer d.mu.Unlock()
	if int(index) >= d.running.count(kindBinaryOutput) {
		return dnp3.CommandNotSupported
	}
	supported := false
	for _, code := range crobCodes {
		supported = supported || code == c.Code
	}
	if !supported {
		return dnp3.CommandNotSupported
	}
	if d.isLocal(index) {
		return dnp3.CommandLocal
	}
	return dnp3.CommandSuccess
}

// SelectCROB reports whether an operate would be accepted.
func (d *device) SelectCROB(index uint16, c dnp3.ControlRelayOutputBlock) dnp3.CommandStatus {
	return d.crobStatus(index, c)
}

// OperateCROB operates a binary output, moving its status the way the
// operation would move a latching relay: on for latch on and close, off for
// latch off and trip, and unchanged for a plain pulse, which ends where it
// started.
func (d *device) OperateCROB(index uint16, c dnp3.ControlRelayOutputBlock, _ outstation.OperateType) dnp3.CommandStatus {
	status := d.crobStatus(index, c)
	if status != dnp3.CommandSuccess {
		return status
	}

	d.mu.Lock()
	d.operations = append(d.operations, operation{kindBinaryOutput, index})
	sess, clk := d.sess, d.clock
	d.mu.Unlock()

	// This runs on the session goroutine, between requests, so the database
	// is the handler's to change directly; Session.Update from here would
	// queue behind the request being answered.
	db := sess.Database()
	v, _, _ := db.BinaryOutputStatus(index)
	switch c.Code {
	case codeLatchOn, codePulseClose:
		v.Value = true
	case codeLatchOff, codePulseTrip:
		v.Value = false
	}
	v.Time = clk.stamp()
	db.UpdateBinaryOutputStatus(index, v)
	return dnp3.CommandSuccess
}

func (d *device) analogStatus(index uint16) dnp3.CommandStatus {
	d.mu.Lock()
	defer d.mu.Unlock()
	if int(index) >= d.running.count(kindAnalogOutput) {
		return dnp3.CommandNotSupported
	}
	return dnp3.CommandSuccess
}

// SelectAnalog reports whether an operate would be accepted.
func (d *device) SelectAnalog(index uint16, _ outstation.AnalogOutput) dnp3.CommandStatus {
	return d.analogStatus(index)
}

// OperateAnalog sets an analog output, whose status then reports the value.
func (d *device) OperateAnalog(index uint16, v outstation.AnalogOutput, _ outstation.OperateType) dnp3.CommandStatus {
	status := d.analogStatus(index)
	if status != dnp3.CommandSuccess {
		return status
	}

	d.mu.Lock()
	d.operations = append(d.operations, operation{kindAnalogOutput, index})
	sess, clk := d.sess, d.clock
	d.mu.Unlock()

	db := sess.Database()
	out, _, _ := db.AnalogOutputStatus(index)
	out.Value = v.Value
	out.Time = clk.stamp()
	db.UpdateAnalogOutputStatus(index, out)
	return dnp3.CommandSuccess
}

// takeOperation reports whether an output operated since its last check,
// and forgets that it did. The other operations stay for their own checks:
// a request that operates several outputs is checked one output at a time.
func (d *device) takeOperation(k kind, index uint16) (bool, []operation) {
	d.mu.Lock()
	defer d.mu.Unlock()
	seen := d.operations
	for i, op := range d.operations {
		if op.kind == k && op.index == index {
			d.operations = append(d.operations[:i:i], d.operations[i+1:]...)
			return true, seen
		}
	}
	return false, seen
}

// takeOperations returns the operations not yet checked, and forgets them.
func (d *device) takeOperations() []operation {
	d.mu.Lock()
	defer d.mu.Unlock()
	ops := d.operations
	d.operations = nil
	return ops
}

// ---------- Attributes ----------

// configuredAttributes are the device attributes the device states about
// itself. The point counts and fragment sizes are the outstation's to derive.
func configuredAttributes() []dnp3.Attribute {
	text := func(v uint8, s string) dnp3.Attribute {
		return dnp3.Attribute{Set: 0, Variation: v, Type: dnp3.AttrVisibleString, Text: s}
	}
	return []dnp3.Attribute{
		text(242, "go-dnp3"),
		text(243, "software"),
		text(245, "conformance bench"),
		text(247, "go-dnp3 dut"),
		text(248, "0001"),
		text(250, "go-dnp3 outstation"),
		text(252, "DSC Systems"),
	}
}

// expectedAttributes is everything a read of the standard set should report.
func expectedAttributes(cfg outstation.Config) map[uint8]dnp3.Attribute {
	uint := func(v uint8, n int) dnp3.Attribute {
		return dnp3.Attribute{Set: 0, Variation: v, Type: dnp3.AttrUnsignedInt, Number: int64(n)}
	}
	db := cfg.Database
	rx := cfg.MaxRxFragment
	if rx <= 0 {
		rx = fragmentSize
	}
	out := map[uint8]dnp3.Attribute{
		outstation.AttrMaxTxFragment: uint(outstation.AttrMaxTxFragment, cfg.MaxTxFragment),
		outstation.AttrMaxRxFragment: uint(outstation.AttrMaxRxFragment, rx),
	}
	for v, n := range map[uint8]int{
		outstation.AttrBinaryInputCount:    db.Binary,
		outstation.AttrDoubleBitInputCount: db.DoubleBitBinary,
		outstation.AttrCounterCount:        db.Counter,
		outstation.AttrAnalogInputCount:    db.Analog,
		outstation.AttrBinaryOutputCount:   db.BinaryOutputStatus,
		outstation.AttrAnalogOutputCount:   db.AnalogOutputStatus,
	} {
		if n > 0 {
			out[v] = uint(v, n)
		}
	}
	for _, a := range cfg.Attributes {
		out[a.Variation] = a
	}
	return out
}

// attributeWritten keeps the expected values in step with the master's
// writes, which the outstation stores.
func (d *device) attributeWritten(a dnp3.Attribute) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if a.Set == 0 {
		d.attributes[a.Variation] = a
	}
	return true
}

// ---------- Clock ----------

// clock is the outstation's time: the host's, corrected by the master's
// writes, and unsynchronized until the first one.
type clock struct {
	mu       sync.Mutex
	offset   time.Duration
	syncedAt time.Time
	last     time.Time
	noCold   bool
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.offset)
}

func (c *clock) WriteAbsoluteTime(t time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offset = time.Until(t)
	c.syncedAt = time.Now()
	return true
}

func (c *clock) ColdRestart() time.Duration { return time.Second }
func (c *clock) WarmRestart() time.Duration { return time.Second }
func (c *clock) SupportsWriteTime() bool    { return true }

func (c *clock) SupportsColdRestart() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.noCold
}

func (c *clock) SupportsWarmRestart() bool { return true }

func (c *clock) setColdRestart(supported bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.noCold = !supported
}

// stamp is the time to give a change made now. Successive stamps are at least
// a millisecond apart, the resolution DNP3 carries, so events generated in a
// burst still have an order the master can see.
func (c *clock) stamp() dnp3.Timestamp {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now().Add(c.offset).Truncate(time.Millisecond)
	if !now.After(c.last) {
		now = c.last.Add(time.Millisecond)
	}
	c.last = now
	// Synchronized for as long as the outstation says so, which is until the
	// holdover after the sync interval.
	if !c.syncedAt.IsZero() && time.Since(c.syncedAt) < syncInterval+clockHoldover {
		return dnp3.Now(now)
	}
	return dnp3.Unsynchronized(now)
}
