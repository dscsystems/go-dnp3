// Copyright (C) 2026 Ricardo Olsen / DSC Systems.
//
// This program is free software: you can redistribute it and/or modify it
// under the terms of the GNU General Public License as published by the Free
// Software Foundation, either version 3 of the License, or (at your option)
// any later version. It is distributed WITHOUT ANY WARRANTY; without even the
// implied warranty of MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.
// See the GNU General Public License for more details, in the LICENSE file at
// the root of this repository or at <https://www.gnu.org/licenses/>.

// Command dnp3-master is a SCADA client: it polls outstations, records what
// they report, and issues controls.
//
//	dnp3-master -host 127.0.0.1:20000              # poll one device
//	dnp3-master -config sites.yaml -record ./data  # poll many, record to CSV
//	dnp3-master -host HOST operate latch-on 3      # one control, then exit
//
// Running it polls continuously and, with -listen, exposes the live values
// over HTTP. The operate subcommand is a separate one-shot connection, because
// issuing a control should not require standing up the whole recorder.
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
	"sync"
	"syscall"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/master"
)

func main() {
	if err := main2(); err != nil {
		fmt.Fprintln(os.Stderr, "dnp3-master:", err)
		os.Exit(1)
	}
}

func main2() error {
	var (
		configPath = flag.String("config", "", "read sites from a YAML `file`")
		host       = flag.String("host", "", "poll a single outstation at `host:port`")
		serialDev  = flag.String("serial", "", "poll a single outstation on a serial `port`")
		baud       = flag.Int("baud", 9600, "serial line `rate`")
		local      = flag.Int("local", 1, "master link `address`")
		remote     = flag.Int("remote", 10, "outstation link `address`")
		poll       = flag.Duration("poll", 5*time.Second, "event class poll `interval`; 0 disables")
		integrity  = flag.Duration("integrity", 5*time.Minute, "integrity poll `interval`; 0 disables")
		timeout    = flag.Duration("timeout", 5*time.Second, "response `timeout`")
		record     = flag.String("record", "", "write CSV recordings into `directory`")
		listen     = flag.String("listen", "", "serve the live values over HTTP on `address`")
		unsol      = flag.Bool("unsolicited", false, "enable unsolicited reporting")
		verbose    = flag.Bool("v", false, "log protocol activity")
		quiet      = flag.Bool("q", false, "log nothing but errors")
	)
	flag.Usage = usage
	flag.Parse()

	log := newLogger(*verbose, *quiet)

	// The operate subcommand takes over entirely: it connects, issues one
	// control, reports the outcome and exits.
	if args := flag.Args(); len(args) > 0 {
		if args[0] != "operate" {
			return fmt.Errorf("unknown command %q; see -help", args[0])
		}
		return operate(args[1:], *host, *serialDev, *baud, *local, *remote, *timeout, log)
	}

	cfg, err := buildConfig(*configPath, *host, *serialDev, *baud,
		*local, *remote, *poll, *integrity, *timeout, *unsol)
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("configuration: %w", err)
	}

	var rec *Recorder
	if *record != "" {
		if rec, err = NewRecorder(*record); err != nil {
			return fmt.Errorf("opening the recording: %w", err)
		}
		// Rows are flushed as they are written, so this reports only a failure
		// to close the files. It is still worth saying: the whole point of a
		// recording is that it can be trusted afterwards, and a recorder that
		// could not close its own record should not exit quietly.
		defer func() {
			if err := rec.Close(); err != nil {
				log.Error("closing the recording", "err", err)
			}
		}()
	}
	snap := NewSnapshot()

	ctx, stop := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Printf("Polling %d site(s) as master %d:\n", len(cfg.Sites), cfg.Local)
	for _, s := range cfg.Sites {
		r := cfg.resolved(s)
		fmt.Printf("  %-16s outstation %-5d %s  poll %s  integrity %s\n",
			r.Name, r.Address, r.describe(), r.Poll, r.Integrity)
	}
	if *record != "" {
		fmt.Printf("Recording to %s/values.csv and %s/events.csv\n", *record, *record)
	}
	fmt.Println("Press Ctrl-C to stop.")

	if err := run(ctx, cfg, rec, snap, log, *listen); err != nil {
		return err
	}

	fmt.Println("\nFinal state:")
	fmt.Print(snap.Summary())
	return nil
}

// buildConfig turns the flags into a configuration, or loads one from a file.
func buildConfig(path, host, serialDev string, baud, local, remote int,
	poll, integrity, timeout time.Duration, unsol bool) (*Config, error) {

	if path != "" {
		return LoadConfig(path)
	}
	if host == "" && serialDev == "" {
		usage()
		return nil, errors.New("give -config, -host, or -serial")
	}

	cfg := DefaultConfig()
	cfg.Local = uint16(local)
	cfg.Defaults.Poll = poll
	cfg.Defaults.Integrity = integrity
	cfg.Defaults.Timeout = timeout
	cfg.Defaults.Unsolicited = unsol

	name := host
	if name == "" {
		name = serialDev
	}
	cfg.Sites = []Site{{
		Name:    name,
		Host:    host,
		Serial:  serialDev,
		Baud:    baud,
		Address: uint16(remote),
	}}
	return cfg, nil
}

// operate issues a single control and reports the outcome.
func operate(args []string, host, serialDev string, baud, local, remote int,
	timeout time.Duration, log *slog.Logger) error {

	if len(args) < 2 {
		return errors.New("usage: dnp3-master -host HOST operate <control> <index> [value]")
	}
	if host == "" && serialDev == "" {
		return errors.New("operate needs -host or -serial")
	}

	index64, err := strconv.ParseUint(args[1], 10, 16)
	if err != nil {
		return fmt.Errorf("index %q: %w", args[1], err)
	}
	index := uint16(index64)

	var cmd master.Command
	switch args[0] {
	case "latch-on":
		cmd = master.LatchOn(index)
	case "latch-off":
		cmd = master.LatchOff(index)
	case "trip":
		cmd = master.Trip(index, 1000)
	case "close":
		cmd = master.Close(index, 1000)
	case "analog":
		if len(args) < 3 {
			return errors.New("analog needs a value: operate analog <index> <value>")
		}
		v, err := strconv.ParseFloat(args[2], 64)
		if err != nil {
			return fmt.Errorf("value %q: %w", args[2], err)
		}
		cmd = master.AnalogOutputFloat32(index, float32(v))
	default:
		return fmt.Errorf("unknown control %q; try latch-on, latch-off, trip, close or analog", args[0])
	}

	site := Site{
		Name: "operate", Host: host, Serial: serialDev, Baud: baud,
		Local: uint16(local), Address: uint16(remote), Timeout: timeout,
	}
	ch, err := buildChannel(site)
	if err != nil {
		return err
	}
	// Best effort: the process is finishing, and there is nothing left to tell
	// about a socket that objected to being shut.
	defer func() { _ = ch.Close() }()

	sess := master.New(master.Config{
		LocalAddr:       uint16(local),
		RemoteAddr:      uint16(remote),
		ResponseTimeout: timeout,
		// No integrity poll: this connects to issue one control and leave, and
		// polling a device first would be noise on the wire and in its log.
		Log: log,
	}, nil)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = sess.Run(ctx, ch) }()
	defer wg.Wait()

	// Wait for the link before issuing anything.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) && !sess.Connected() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	if !sess.Connected() {
		return fmt.Errorf("could not connect to %s", site.describe())
	}

	opCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// Select-before-operate: this is a person issuing a control, and the
	// select is the outstation's chance to refuse before anything moves.
	res, err := sess.SelectAndOperate(opCtx, cmd)
	stop()

	if err != nil {
		fmt.Printf("%s: %v\n", cmd, err)
		return err
	}
	fmt.Printf("%s: %s\n", cmd, res)
	return nil
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

var _ = dnp3.Class123 // referenced by the scheduling code

func usage() {
	fmt.Fprint(os.Stderr, `dnp3-master — poll DNP3 outstations, record what they report, issue controls

Usage:
  dnp3-master [flags]
  dnp3-master [flags] operate <control> <index> [value]

Sites:
  -config FILE     read sites from YAML
  -host ADDR       poll a single outstation over TCP
  -serial PORT     poll a single outstation over serial
  -baud RATE       serial line rate                  (default 9600)
  -local N         master link address               (default 1)
  -remote N        outstation link address           (default 10)

Polling:
  -poll DUR        event class poll interval         (default 5s, 0 disables)
  -integrity DUR   integrity poll interval           (default 5m, 0 disables)
  -timeout DUR     response timeout                  (default 5s)
  -unsolicited     enable unsolicited reporting

Output:
  -record DIR      write values.csv and events.csv into DIR
  -listen ADDR     serve live values over HTTP
  -v / -q          more or less logging

Controls (operate connects, issues one control, and exits):
  latch-on N       latch a binary output on
  latch-off N      latch it off
  trip N           pulse the trip coil
  close N          pulse the close coil
  analog N VALUE   write an analog setpoint

HTTP endpoints:
  /status          per-site connection state and counters
  /points          the latest value of every point (?site=NAME to filter)
  /healthz

Examples:
  dnp3-master -host 127.0.0.1:20000 -record ./data -listen :8080
  dnp3-master -config sites.yaml -v
  dnp3-master -host 127.0.0.1:20000 operate trip 0
`)
}
