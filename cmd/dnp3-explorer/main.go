// Copyright (C) 2026 Ricardo Olsen / DSC Systems.
//
// This program is free software: you can redistribute it and/or modify it
// under the terms of the GNU General Public License as published by the Free
// Software Foundation, either version 3 of the License, or (at your option)
// any later version. It is distributed WITHOUT ANY WARRANTY; without even the
// implied warranty of MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.
// See the GNU General Public License for more details, in the LICENSE file at
// the root of this repository or at <https://www.gnu.org/licenses/>.

// Command dnp3-explorer is a terminal browser for a DNP3 outstation.
//
// It connects, polls, shows what came back, and lets an operator issue
// controls — the things you actually want when pointing at an unfamiliar
// device and asking "what is this thing reporting, and does it respond?".
//
//	dnp3-explorer --host 10.0.0.5:20000
//	dnp3-explorer --demo                  # a simulated outstation in-process
//
// Demo mode runs a full outstation inside the same program, connected over an
// in-memory pipe. It is the real link, transport, application and object
// layers with no socket and no hardware, which makes the tool usable — and
// demonstrable — without a device to point at.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/channel"
	"github.com/dscsystems/go-dnp3/outstation"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "dnp3-explorer:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		host    = flag.String("host", "", "outstation `address` (host:port)")
		local   = flag.Int("local", 1, "master link `address`")
		remote  = flag.Int("remote", 10, "outstation link `address`")
		demo    = flag.Bool("demo", false, "run a simulated outstation in-process")
		poll    = flag.Duration("poll", 5*time.Second, "how often to poll event classes; 0 disables")
		serial  = flag.String("serial", "", "use a serial `port` instead of TCP")
		baud    = flag.Int("baud", 9600, "serial line `rate`")
		timeout = flag.Duration("timeout", 5*time.Second, "response `timeout`")

		mouse     = flag.Bool("mouse", true, "enable the mouse")
		inline    = flag.Bool("inline", false, "draw inline instead of taking the whole terminal")
		direct    = flag.Bool("direct", false, "direct operate controls instead of select-before-operate")
		noConfirm = flag.Bool("no-confirm", false, "issue controls without asking first")
		pulse     = flag.Uint("pulse", 1000, "pulse `duration` in ms for trip and close controls")
		stale     = flag.Duration("stale", 30*time.Second, "mark a point stale after this long without an update; 0 disables")
	)
	flag.Usage = usage
	flag.Parse()

	if *host == "" && *serial == "" && !*demo {
		usage()
		return errors.New("give -host, -serial, or -demo")
	}

	ctx, stop := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The explorer draws the terminal, so nothing else may write to it. The
	// session's own logging goes nowhere; what the operator needs to see
	// arrives as messages and appears on the Log screen.
	quiet := slog.New(slog.DiscardHandler)

	start := link{
		Demo:    *demo,
		Serial:  *serial,
		Baud:    *baud,
		Host:    *host,
		Local:   uint16(*local),
		Remote:  uint16(*remote),
		Timeout: *timeout,
		Poll:    *poll,
	}
	if err := start.validate(); err != nil {
		return err
	}

	conn := &connection{
		msgs: make(chan tea.Msg, 2048),
		ctx:  ctx,
	}
	conn.sup = &supervisor{conn: conn, root: ctx, log: quiet}

	// The session is brought up through the same path a reconnect uses, so the
	// one an operator reaches for after changing an address is the path that
	// has been exercised since startup rather than a second implementation.
	conn.sup.start(start)
	defer conn.sup.stop()

	// The alternate screen is the default: the tool lays out a fixed frame and
	// fills the terminal with it, which is what makes a table, a scrollbar and
	// a footer possible at all. -inline gives back the old behaviour for the
	// times when leaving the session in the scrollback is worth more than the
	// layout — logging a commissioning run, or a terminal that cannot switch
	// screens.
	model := NewModel(conn)
	model.mouse = *mouse
	model.altmode = !*inline
	model.sbo = !*direct
	model.confirm = !*noConfirm
	model.pulseMs = uint32(*pulse)
	model.staleAge = *stale

	p := tea.NewProgram(model, tea.WithContext(ctx))
	if _, err := p.Run(); err != nil && !errors.Is(err, tea.ErrProgramKilled) {
		return err
	}
	stop()
	return nil
}

// buildChannel selects the transport, starting a simulated outstation for the
// demo.
//
// The demo device is built per connection rather than once for the process, so
// that reconnecting to it works the same way as reconnecting to anything else.
// It comes up fresh, which is the honest outcome: the pipe the old one was
// reached over has been closed.
func buildChannel(ctx context.Context, wg *sync.WaitGroup, p link,
	log *slog.Logger) channel.Channel {

	switch {
	case p.Demo:
		mch, och := channel.Pipe()
		sim := newDemoOutstation(log)

		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = sim.session.Run(ctx, och)
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			sim.run(ctx)
		}()
		return mch

	case p.Serial != "":
		return channel.SerialChannel(channel.SerialConfig{
			Device: p.Serial, Baud: p.Baud,
		}, channel.DefaultRetry)

	default:
		return channel.TCPClient(p.Host, channel.DefaultRetry)
	}
}

// demoOutstation is a small simulated device for the demo mode.
//
// It is deliberately modest — six binaries, six analogs, two counters, two
// controls and the strings a device reports about itself — because its job is
// to make the interface explorable, not to be a second simulator.
// dnp3-outstation is the one with plant behind it.
//
// What it does contain is one of everything the interface has to draw: a point
// that goes offline, a point that is locally forced, a value that ramps so the
// trend has a shape, controls that actually move something, and setpoints that
// come back as analog output status. Otherwise half the tool can only be
// tested against real hardware.
type demoOutstation struct {
	session *outstation.Session

	// mu guards the plant state. Updates normally run on the session
	// goroutine, but Session.Update falls back to applying directly on the
	// caller's when its queue is full, and that is enough to make a race real.
	mu       sync.Mutex
	breaker  [2]bool
	setpoint [2]float64
}

func newDemoOutstation(log *slog.Logger) *demoOutstation {
	d := &demoOutstation{
		breaker:  [2]bool{true, false},
		setpoint: [2]float64{13.75, 50},
	}

	d.session = outstation.New(outstation.Config{
		LocalAddr:  10,
		RemoteAddr: 1,
		Database: outstation.DatabaseConfig{
			Binary: 6, Analog: 6, Counter: 2,
			BinaryOutputStatus: 2, AnalogOutputStatus: 2, OctetString: 2,
			DefaultClass: dnp3.Class1,
		},
		Log: log,
	}, nil, d)

	db := d.session.Database()
	for i := range uint16(6) {
		db.Configure(dnp3.TypeAnalog, i, outstation.PointConfig{
			Class:           dnp3.Class2,
			Deadband:        0.5,
			StaticVariation: 5, // single precision; the default would truncate
			EventVariation:  7,
		})
	}
	// The strings never change, so they are written once rather than on every
	// tick: a device name that produced an event every half second would be a
	// device nobody would ship.
	db.UpdateOctetString(0, dnp3.OctetString("GO-DNP3 DEMO RTU"))
	db.UpdateOctetString(1, dnp3.OctetString("firmware 1.0.0-demo"))
	return d
}

func (d *demoOutstation) run(ctx context.Context) {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()

	var n float64
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			n += 0.5
			d.session.Update(func(db *outstation.Database) {
				d.mu.Lock()
				breaker, setpoint := d.breaker, d.setpoint
				d.mu.Unlock()

				stamp := dnp3.Now(now)

				db.UpdateAnalog(0, dnp3.Analog{
					Value: 11000 + 200*math.Sin(n/10), Flags: dnp3.Online, Time: stamp})
				db.UpdateAnalog(1, dnp3.Analog{
					Value: 150 + 120*math.Sin(n/7), Flags: dnp3.Online, Time: stamp})
				db.UpdateAnalog(2, dnp3.Analog{
					Value: 45 + 20*math.Sin(n/23), Flags: dnp3.Online, Time: stamp})
				db.UpdateAnalog(3, dnp3.Analog{
					Value: 8, Flags: dnp3.Online, Time: stamp})
				// A sensor that drops out for ten seconds in every forty, so
				// the quality column and the stale fade have something to show.
				dropped := int(n)%40 < 10
				db.UpdateAnalog(4, dnp3.Analog{
					Value: 0.42, Flags: onlineUnless(dropped, dnp3.CommLost), Time: stamp})
				// A ramp, because a trend needs a shape to be worth drawing.
				db.UpdateAnalog(5, dnp3.Analog{
					Value: math.Mod(n, 60), Flags: dnp3.Online, Time: stamp})

				for i := range uint16(2) {
					db.UpdateBinary(i, dnp3.Binary{
						Value: breaker[i], Flags: dnp3.Online, Time: stamp})
					db.UpdateBinaryOutputStatus(i, dnp3.BinaryOutputStatus{
						Value: breaker[i], Flags: dnp3.Online, Time: stamp})
					db.UpdateAnalogOutputStatus(i, dnp3.AnalogOutputStatus{
						Value: setpoint[i], Flags: dnp3.Online, Time: stamp})
				}
				// A point that toggles on its own, so the Events screen has
				// something to show without the operator doing anything.
				db.UpdateBinary(2, dnp3.Binary{
					Value: int(n)%20 < 10, Flags: dnp3.Online, Time: stamp})
				db.UpdateBinary(3, dnp3.Binary{
					Value: int(n)%14 < 3, Flags: dnp3.Online | dnp3.ChatterFilter, Time: stamp})
				db.UpdateBinary(4, dnp3.Binary{
					Value: false, Flags: dnp3.Online | dnp3.LocalForced, Time: stamp})
				db.UpdateBinary(5, dnp3.Binary{
					Value: true, Flags: dnp3.CommLost, Time: stamp})

				db.UpdateCounter(0, dnp3.Counter{
					Value: uint32(n * 3), Flags: dnp3.Online, Time: stamp})
				db.UpdateCounter(1, dnp3.Counter{
					Value: uint32(n * 2), Flags: dnp3.Online, Time: stamp})
			})
		}
	}
}

// onlineUnless is the quality of a point that is healthy unless something is
// wrong with it.
func onlineUnless(faulted bool, fault dnp3.Flags) dnp3.Flags {
	if faulted {
		return fault
	}
	return dnp3.Online
}

func (d *demoOutstation) SelectCROB(index uint16, _ dnp3.ControlRelayOutputBlock) dnp3.CommandStatus {
	if int(index) >= len(d.breaker) {
		return dnp3.CommandNotSupported
	}
	return dnp3.CommandSuccess
}

func (d *demoOutstation) OperateCROB(index uint16, c dnp3.ControlRelayOutputBlock,
	_ outstation.OperateType) dnp3.CommandStatus {

	if int(index) >= len(d.breaker) {
		return dnp3.CommandNotSupported
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case c.Code.IsClose() || c.Code.OpType() == dnp3.ControlLatchOn:
		d.breaker[index] = true
	case c.Code.IsTrip() || c.Code.OpType() == dnp3.ControlLatchOff:
		d.breaker[index] = false
	default:
		return dnp3.CommandNotSupported
	}
	return dnp3.CommandSuccess
}

func (d *demoOutstation) SelectAnalog(index uint16, _ outstation.AnalogOutput) dnp3.CommandStatus {
	if int(index) >= len(d.setpoint) {
		return dnp3.CommandNotSupported
	}
	return dnp3.CommandSuccess
}

// OperateAnalog stores the setpoint, which the next tick reports back as
// analog output status — the round trip an operator is really checking when
// they write one.
func (d *demoOutstation) OperateAnalog(index uint16, v outstation.AnalogOutput,
	_ outstation.OperateType) dnp3.CommandStatus {

	if int(index) >= len(d.setpoint) {
		return dnp3.CommandNotSupported
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.setpoint[index] = v.Value
	return dnp3.CommandSuccess
}

func usage() {
	fmt.Fprint(os.Stderr, `dnp3-explorer — a terminal browser for a DNP3 outstation

Usage:
  dnp3-explorer -host HOST:PORT [flags]
  dnp3-explorer -demo

Connection (all of it editable while running, with C):
  -host ADDR       outstation address
  -serial PORT     use a serial port instead of TCP
  -baud RATE       serial line rate                  (default 9600)
  -local N         master link address               (default 1)
  -remote N        outstation link address           (default 10)
  -poll DUR        event class poll interval         (default 5s, 0 disables)
  -timeout DUR     response timeout                  (default 5s)
  -demo            run a simulated outstation in-process

Interface:
  -mouse           enable the mouse                  (default true)
  -inline          draw inline instead of taking the whole terminal
  -stale DUR       fade points not updated for this long   (default 30s)

Controls:
  -direct          direct operate instead of select-before-operate
  -no-confirm      issue controls without asking first
  -pulse MS        pulse duration for trip and close  (default 1000)

Keys:
  1-5 / tab        switch screens
  up/down, j/k     move the cursor; pgup/pgdn by a page
  enter            act on the selected row (control, setpoint, inspector)
  i / p            integrity poll / poll event classes 1, 2 and 3
  s                range scan a group
  t / T            set the outstation clock (T measures the link delay)
  u / U            enable / disable unsolicited reporting
  R                restart the outstation
  c / o            close / open the selected binary output
  b                write a deadband to the selected analog input
  S                switch between select-before-operate and direct operate
  C                edit the connection and reconnect in place
  / and esc        filter the list, and clear the filter
  < > and r        change and reverse the sort column
  d                the point inspector
  f                follow the newest row
  x                clear the current list
  e                export the current list as CSV
  ? or 5           the full key and mouse reference
  q                quit

Mouse:
  click a tab, a row, a column heading or a footer button; click a
  selected row again to act on it, right-click one for the inspector,
  scroll with the wheel, and drag the scrollbar.

Examples:
  dnp3-explorer -demo
  dnp3-explorer -host 10.0.0.5:20000 -remote 10 -poll 2s
`)
}

// discardLogger is a logger that writes nothing, used where the terminal
// belongs to the UI.
func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }
