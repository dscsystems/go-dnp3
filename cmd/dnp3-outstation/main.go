// Copyright (C) 2026 Ricardo Olsen / DSC Systems.
//
// This program is free software: you can redistribute it and/or modify it
// under the terms of the GNU General Public License as published by the Free
// Software Foundation, either version 3 of the License, or (at your option)
// any later version. It is distributed WITHOUT ANY WARRANTY; without even the
// implied warranty of MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.
// See the GNU General Public License for more details, in the LICENSE file at
// the root of this repository or at <https://www.gnu.org/licenses/>.

// Command dnp3-outstation is a simulated DNP3 outstation: a substation RTU
// with plant behind it.
//
// It exists to be something to develop a master against. The points move the
// way plant moves — a breaker stays open once tripped and takes time to
// travel, an analog ramps rather than jumping, a counter only counts up — and
// commands close the loop: tripping breaker 0 opens breaker 0, which raises a
// binary input event, which the master then receives.
//
//	dnp3-outstation                          # a default substation on :20000
//	dnp3-outstation --config substation.yaml
//	dnp3-outstation --inject event-storm=500 --inject offline-every=30s
//
// The fault injections are the point of the tool as much as the simulation is:
// an event storm, a device that restarts under you, points that go comm-lost.
// Those are the conditions a master's error handling is usually wrong about
// and the hardest to arrange with real equipment.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/channel"
	"github.com/dscsystems/go-dnp3/outstation"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "dnp3-outstation:", err)
		os.Exit(1)
	}
}

// injectFlag collects repeated --inject options.
type injectFlag []string

func (f *injectFlag) String() string { return strings.Join(*f, ",") }
func (f *injectFlag) Set(v string) error {
	*f = append(*f, v)
	return nil
}

func run() error {
	var (
		configPath = flag.String("config", "", "read the simulated plant from a YAML `file`")
		listen     = flag.String("listen", ":20000", "listen on `address`")
		serialDev  = flag.String("serial", "", "use a serial `port` instead of TCP")
		baud       = flag.Int("baud", 9600, "serial line `rate`")
		udp        = flag.Bool("udp", false, "use UDP instead of TCP")
		address    = flag.Int("address", 0, "override the outstation link `address`")
		master     = flag.Int("master", 0, "override the master link `address`")
		unsol      = flag.Bool("unsolicited", false, "push events without being polled")
		filesDir   = flag.String("files", "", "serve file transfer from `directory` instead of the simulated files")
		filesRO    = flag.Bool("files-read-only", false, "refuse file writes and deletes")
		noFiles    = flag.Bool("no-files", false, "disable file transfer entirely")
		noAttrs    = flag.Bool("no-attributes", false, "report no device attributes")
		verbose    = flag.Bool("v", false, "log protocol activity")
		quiet      = flag.Bool("q", false, "log nothing but errors")
		dump       = flag.Bool("points", false, "print the point list and exit")

		tlsCert = flag.String("tls-cert", "", "TLS certificate `file` (enables TLS)")
		tlsKey  = flag.String("tls-key", "", "TLS private key `file`")
		tlsCA   = flag.String("tls-ca", "", "CA `file` used to verify the master")

		inject injectFlag
	)
	flag.Var(&inject, "inject", "inject a fault; repeatable (see -help)")
	flag.Usage = usage
	flag.Parse()

	cfg := DefaultConfig()
	if *configPath != "" {
		var err error
		if cfg, err = LoadConfig(*configPath); err != nil {
			return err
		}
	}
	if *address > 0 {
		cfg.Address = uint16(*address)
	}
	if *master > 0 {
		cfg.Master = uint16(*master)
	}
	if *unsol {
		cfg.Unsolicited.Enabled = true
	}
	if *filesDir != "" {
		cfg.Files.Directory = *filesDir
	}
	if *filesRO {
		cfg.Files.ReadOnly = true
	}
	if *noFiles {
		cfg.Files.Disabled = true
	}
	if *noAttrs {
		cfg.Device.Disabled = true
	}

	var err error
	if cfg.injection, err = parseInjections(inject); err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("configuration: %w", err)
	}

	sim := NewSimulator(cfg)
	if *dump {
		fmt.Print(sim.Describe())
		return nil
	}

	log := newLogger(*verbose, *quiet)
	ocfg := cfg.outstationConfig()
	ocfg.Log = log

	files, servingFiles, closeFiles, err := buildFileConfig(cfg, sim)
	if err != nil {
		return err
	}
	if closeFiles != nil {
		defer func() { _ = closeFiles() }()
	}
	ocfg.Files = files

	plant := &plantHandler{sim: sim, log: log}
	sess := outstation.New(ocfg, &clock{}, plant)
	cfg.applyPointConfig(sess.Database())
	plant.sess = sess

	ch, description, err := buildChannel(*listen, *serialDev, *baud, *udp,
		*tlsCert, *tlsKey, *tlsCA)
	if err != nil {
		return err
	}
	// Best effort: the process is finishing, and there is nothing left to tell
	// about a socket that objected to being shut.
	defer func() { _ = ch.Close() }()

	ctx, stop := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Print(sim.Describe())
	fmt.Printf("\nOutstation %d listening for master %d on %s\n",
		cfg.Address, cfg.Master, description)
	fmt.Print(describeAttributes(cfg.attributes))
	fmt.Printf("File transfer: %s\n", servingFiles)
	if m, ok := files.Handler.(*memFiles); ok {
		fmt.Print(m.Describe())
	}
	if cfg.injection.any() {
		fmt.Printf("Injecting: %s\n", cfg.injection)
	}
	fmt.Println("Press Ctrl-C to stop.")

	go simulate(ctx, sess, sim, cfg.injection, log)

	if err := sess.Run(ctx, ch); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}

	st := sess.Stats()
	fmt.Printf("\n%d requests, %d responses, %d commands, %d unsolicited\n",
		st.RequestsReceived, st.ResponsesSent, st.CommandsExecuted, st.UnsolicitedSent)
	if st.FilesOpened > 0 {
		fmt.Printf("%d file transfers, %d blocks sent, %d received, %d refused\n",
			st.FilesOpened, st.FileBlocksSent, st.FileBlocksReceived, st.FileErrors)
	}
	return nil
}

// simulate advances the plant and writes it into the database.
func simulate(ctx context.Context, sess *outstation.Session, sim *Simulator,
	inject Injection, log *slog.Logger) {

	// Prime the database before the first tick. A master that polls in the
	// first half second would otherwise read zeros for everything — including
	// the analog outputs, whose configured starting values are the one thing
	// about them nobody can recover by waiting.
	sim.Apply(sess, time.Now())

	tick := time.NewTicker(time.Duration(tickSeconds * float64(time.Second)))
	defer tick.Stop()

	var restartAt time.Time
	if inject.RestartAfter > 0 {
		restartAt = time.Now().Add(inject.RestartAfter)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			sim.Apply(sess, now)

			if !restartAt.IsZero() && now.After(restartAt) {
				log.Warn("injecting a restart")
				// A restart is the one event that invalidates a master's whole
				// picture, so it is worth being able to produce on demand.
				sess.Restart()
				restartAt = now.Add(inject.RestartAfter)
			}
		}
	}
}

// buildChannel selects the transport from the flags.
func buildChannel(listen, serialDev string, baud int, udp bool,
	cert, key, ca string) (channel.Channel, string, error) {

	switch {
	case serialDev != "":
		return channel.SerialChannel(channel.SerialConfig{
			Device: serialDev, Baud: baud,
		}, channel.DefaultRetry), fmt.Sprintf("serial %s at %d baud", serialDev, baud), nil

	case udp:
		return channel.UDPChannel(channel.UDPConfig{LocalAddr: listen}),
			"UDP " + listen, nil

	case cert != "" || key != "" || ca != "":
		if cert == "" || key == "" || ca == "" {
			return nil, "", errors.New("TLS needs -tls-cert, -tls-key and -tls-ca together")
		}
		ch, err := channel.TLSServer(listen, channel.TLSConfig{
			CertFile: cert, KeyFile: key, CAFile: ca,
		})
		if err != nil {
			return nil, "", err
		}
		return ch, "TLS " + listen, nil

	default:
		return channel.TCPServer(listen), "TCP " + listen, nil
	}
}

// plantHandler routes commands into the simulation.
type plantHandler struct {
	sim  *Simulator
	sess *outstation.Session
	log  *slog.Logger
}

func (p *plantHandler) SelectCROB(index uint16, c dnp3.ControlRelayOutputBlock) dnp3.CommandStatus {
	// A select must not operate anything; it reports whether the operate
	// would be accepted. Asking the simulator without applying is exactly
	// that.
	return p.sim.wouldAccept(index)
}

func (p *plantHandler) OperateCROB(index uint16, c dnp3.ControlRelayOutputBlock,
	op outstation.OperateType) dnp3.CommandStatus {

	var closing bool
	switch {
	case c.Code.IsTrip():
		closing = false
	case c.Code.IsClose():
		closing = true
	case c.Code.OpType() == dnp3.ControlLatchOn:
		closing = true
	case c.Code.OpType() == dnp3.ControlLatchOff:
		closing = false
	default:
		return dnp3.CommandNotSupported
	}

	status := p.sim.Operate(index, closing, time.Now())
	p.log.Info("control", "index", index, "code", c.Code.String(),
		"operate", op.String(), "status", status.String())
	return status
}

func (p *plantHandler) SelectAnalog(index uint16, v outstation.AnalogOutput) dnp3.CommandStatus {
	// A select reports whether the operate would be accepted without moving
	// anything, which for a setpoint means checking the value against the
	// output's range.
	return p.sim.wouldAcceptAnalog(index, v.Value)
}

func (p *plantHandler) OperateAnalog(index uint16, v outstation.AnalogOutput,
	op outstation.OperateType) dnp3.CommandStatus {

	status := p.sim.SetAnalog(index, v.Value)
	p.log.Info("setpoint", "index", index, "value", v.Value, "status", status.String())
	return status
}

// clock is the outstation's idea of time.
type clock struct{ offset time.Duration }

func (c *clock) Now() time.Time { return time.Now().Add(c.offset) }

func (c *clock) WriteAbsoluteTime(t time.Time) bool {
	// Record the master's correction rather than changing the host clock,
	// which a simulator has no business doing.
	c.offset = time.Until(t)
	return true
}

func (c *clock) ColdRestart() time.Duration { return 3 * time.Second }
func (c *clock) WarmRestart() time.Duration { return time.Second }
func (c *clock) SupportsWriteTime() bool    { return true }

// parseInjections turns the repeated --inject flags into a configuration.
func parseInjections(flags injectFlag) (Injection, error) {
	var inj Injection

	for _, f := range flags {
		name, value, _ := strings.Cut(f, "=")
		switch strings.TrimSpace(name) {
		case "event-storm":
			n, err := strconv.Atoi(value)
			if err != nil || n <= 0 {
				return inj, fmt.Errorf("--inject event-storm needs a positive count, got %q", value)
			}
			inj.EventStorm = n

		case "device-trouble":
			inj.DeviceTrouble = true

		case "restart-after":
			d, err := time.ParseDuration(value)
			if err != nil {
				return inj, fmt.Errorf("--inject restart-after: %w", err)
			}
			inj.RestartAfter = d

		case "offline-every":
			d, err := time.ParseDuration(value)
			if err != nil {
				return inj, fmt.Errorf("--inject offline-every: %w", err)
			}
			inj.OfflineEvery = d

		default:
			return inj, fmt.Errorf("unknown injection %q; see -help", name)
		}
	}
	return inj, nil
}

func (i Injection) any() bool {
	return i.EventStorm > 0 || i.DeviceTrouble || i.RestartAfter > 0 || i.OfflineEvery > 0
}

func (i Injection) String() string {
	var parts []string
	if i.EventStorm > 0 {
		parts = append(parts, fmt.Sprintf("event storm %d/s", i.EventStorm))
	}
	if i.DeviceTrouble {
		parts = append(parts, "device trouble")
	}
	if i.RestartAfter > 0 {
		parts = append(parts, "restart every "+i.RestartAfter.String())
	}
	if i.OfflineEvery > 0 {
		parts = append(parts, "points offline every "+i.OfflineEvery.String())
	}
	return strings.Join(parts, ", ")
}

func newLogger(verbose, quiet bool) *slog.Logger {
	level := slog.LevelInfo
	switch {
	case quiet:
		level = slog.LevelError
	case verbose:
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

func usage() {
	fmt.Fprint(os.Stderr, `dnp3-outstation — a simulated DNP3 outstation with plant behind it

Usage:
  dnp3-outstation [flags]

Transport (TCP by default):
  -listen ADDR        listen address                      (default :20000)
  -udp                use UDP instead of TCP
  -serial PORT        use a serial port
  -baud RATE          serial line rate                    (default 9600)
  -tls-cert FILE      certificate; with -tls-key and -tls-ca, enables TLS
  -tls-key FILE       private key
  -tls-ca FILE        authority used to verify the master

Device:
  -config FILE        read the simulated plant from YAML
  -address N          override the outstation link address
  -master N           override the master link address
  -unsolicited        push events without being polled
  -points             print the point list and exit

Device attributes (group 0):
  -no-attributes      report none, so a master meets a device without them

File transfer (group 70; simulated files in memory by default):
  -files DIR          serve a real directory instead, and nothing above it
  -files-read-only    refuse writes and deletes
  -no-files           disable file transfer, so the device reports it has none

Fault injection (repeatable):
  -inject event-storm=N       generate N binary events per second
  -inject restart-after=DUR   report a restart every DUR
  -inject offline-every=DUR   flip points to comm-lost every DUR
  -inject device-trouble      assert the DEVICE_TROUBLE indication

Logging:
  -v                  log protocol activity
  -q                  log nothing but errors

Examples:
  dnp3-outstation
  dnp3-outstation -config substation.yaml -unsolicited -v
  dnp3-outstation -inject event-storm=500 -inject offline-every=30s
  dnp3-outstation -files ./device-files -files-read-only
`)
}
