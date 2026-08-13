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
	"github.com/dscsystems/go-dnp3/master"
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

	var wg sync.WaitGroup
	defer wg.Wait()

	// The explorer draws the terminal, so nothing else may write to it. The
	// session's own logging goes nowhere; what the operator needs to see
	// arrives as messages and appears on the Log screen.
	quiet := slog.New(slog.DiscardHandler)

	ch, target, err := buildChannel(ctx, &wg, *demo, *host, *serial, *baud, quiet)
	if err != nil {
		return err
	}
	defer ch.Close()

	conn := &connection{
		msgs:   make(chan tea.Msg, 2048),
		ctx:    ctx,
		target: target,
	}
	h := &handler{conn: conn}

	sess := master.New(master.Config{
		LocalAddr:             uint16(*local),
		RemoteAddr:            uint16(*remote),
		ResponseTimeout:       *timeout,
		IntegrityOnStartup:    true,
		DisableUnsolOnStartup: true,
		UnsolClassMask:        dnp3.Class123,
		KeepAlive:             30 * time.Second,
		Log:                   quiet,
	}, h)
	conn.sess = sess

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := sess.Run(ctx, ch); err != nil && ctx.Err() == nil {
			conn.push(statusMsg{text: "failed", err: err.Error()})
		}
	}()

	if *poll > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Give the startup sequence a moment before adding to the queue.
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			if err := sess.AddPeriodicScan(ctx, *poll, dnp3.Class123); err != nil && ctx.Err() == nil {
				conn.push(logMsg{level: "warn", text: "periodic poll: " + err.Error()})
			}
		}()
	}

	// A connection watcher, so the header reflects reality even when the
	// outstation has nothing to say.
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				state := "disconnected"
				if sess.Connected() {
					state = "connected"
				}
				conn.push(statusMsg{
					text:      state,
					connected: sess.Connected(),
					stats:     sess.Stats(),
					iin:       sess.LastIIN().String(),
				})
			}
		}
	}()

	// Rendered inline rather than on an alternate screen: this is a
	// diagnostic tool, and leaving the session visible in the scrollback after
	// it exits is more useful than a clean terminal.
	p := tea.NewProgram(NewModel(conn), tea.WithContext(ctx))
	if _, err := p.Run(); err != nil && !errors.Is(err, tea.ErrProgramKilled) {
		return err
	}
	stop()
	return nil
}

// buildChannel selects the transport, starting a simulated outstation for the
// demo.
func buildChannel(ctx context.Context, wg *sync.WaitGroup, demo bool,
	host, serialDev string, baud int, log *slog.Logger) (channel.Channel, string, error) {

	switch {
	case demo:
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
		return mch, "demo (in-process outstation)", nil

	case serialDev != "":
		return channel.SerialChannel(channel.SerialConfig{
			Device: serialDev, Baud: baud,
		}, channel.DefaultRetry), fmt.Sprintf("serial %s", serialDev), nil

	default:
		return channel.TCPClient(host, channel.DefaultRetry), host, nil
	}
}

// demoOutstation is a small simulated device for the demo mode.
//
// It is deliberately modest — four binaries, four analogs, two counters, two
// controls — because its job is to make the interface explorable, not to be a
// second simulator. dnp3-outstation is the one with plant behind it.
type demoOutstation struct {
	session *outstation.Session
	breaker [2]bool
}

func newDemoOutstation(log *slog.Logger) *demoOutstation {
	d := &demoOutstation{breaker: [2]bool{true, false}}

	d.session = outstation.New(outstation.Config{
		LocalAddr:  10,
		RemoteAddr: 1,
		Database: outstation.DatabaseConfig{
			Binary: 4, Analog: 4, Counter: 2,
			BinaryOutputStatus: 2, AnalogOutputStatus: 2,
			DefaultClass: dnp3.Class1,
		},
		Log: log,
	}, nil, d)

	for i := range uint16(4) {
		d.session.Database().Configure(dnp3.TypeAnalog, i, outstation.PointConfig{
			Class:           dnp3.Class2,
			Deadband:        0.5,
			StaticVariation: 5, // single precision; the default would truncate
			EventVariation:  7,
		})
	}
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
				db.UpdateAnalog(0, dnp3.Analog{
					Value: 11000 + 200*math.Sin(n/10), Flags: dnp3.Online, Time: dnp3.Now(now)})
				db.UpdateAnalog(1, dnp3.Analog{
					Value: 150 + 120*math.Sin(n/7), Flags: dnp3.Online, Time: dnp3.Now(now)})
				db.UpdateAnalog(2, dnp3.Analog{
					Value: 45 + 20*math.Sin(n/23), Flags: dnp3.Online, Time: dnp3.Now(now)})
				db.UpdateAnalog(3, dnp3.Analog{
					Value: 8, Flags: dnp3.Online, Time: dnp3.Now(now)})

				for i := range uint16(2) {
					db.UpdateBinary(i, dnp3.Binary{
						Value: d.breaker[i], Flags: dnp3.Online, Time: dnp3.Now(now)})
					db.UpdateBinaryOutputStatus(i, dnp3.BinaryOutputStatus{
						Value: d.breaker[i], Flags: dnp3.Online, Time: dnp3.Now(now)})
				}
				// A point that toggles on its own, so the Events screen has
				// something to show without the operator doing anything.
				db.UpdateBinary(2, dnp3.Binary{
					Value: int(n)%20 < 10, Flags: dnp3.Online, Time: dnp3.Now(now)})

				db.UpdateCounter(0, dnp3.Counter{
					Value: uint32(n * 3), Flags: dnp3.Online, Time: dnp3.Now(now)})
				db.UpdateCounter(1, dnp3.Counter{
					Value: uint32(n * 2), Flags: dnp3.Online, Time: dnp3.Now(now)})
			})
		}
	}
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

func (d *demoOutstation) SelectAnalog(uint16, outstation.AnalogOutput) dnp3.CommandStatus {
	return dnp3.CommandSuccess
}

func (d *demoOutstation) OperateAnalog(uint16, outstation.AnalogOutput,
	outstation.OperateType) dnp3.CommandStatus {
	return dnp3.CommandSuccess
}

func usage() {
	fmt.Fprint(os.Stderr, `dnp3-explorer — a terminal browser for a DNP3 outstation

Usage:
  dnp3-explorer -host HOST:PORT [flags]
  dnp3-explorer -demo

Connection:
  -host ADDR       outstation address
  -serial PORT     use a serial port instead of TCP
  -baud RATE       serial line rate                  (default 9600)
  -local N         master link address               (default 1)
  -remote N        outstation link address           (default 10)
  -poll DUR        event class poll interval         (default 5s, 0 disables)
  -timeout DUR     response timeout                  (default 5s)
  -demo            run a simulated outstation in-process

Keys:
  1-4 / tab        switch screens
  up/down, j/k     move the cursor
  i                integrity poll (classes 0, 1, 2 and 3)
  p                poll event classes 1, 2 and 3
  t                set the outstation clock
  c / o            close / open the selected binary output
  /                filter the point list
  f                follow the newest row
  x                clear the events or log list
  q                quit

Examples:
  dnp3-explorer -demo
  dnp3-explorer -host 10.0.0.5:20000 -remote 10 -poll 2s
`)
}

// discardLogger is a logger that writes nothing, used where the terminal
// belongs to the UI.
func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }
