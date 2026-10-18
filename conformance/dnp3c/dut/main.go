// Copyright (C) 2026 Ricardo Olsen / DSC Systems.
//
// This program is free software: you can redistribute it and/or modify it
// under the terms of the GNU General Public License as published by the Free
// Software Foundation, either version 3 of the License, or (at your option)
// any later version. It is distributed WITHOUT ANY WARRANTY; without even the
// implied warranty of MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.
// See the GNU General Public License for more details, in the LICENSE file at
// the root of this repository or at <https://www.gnu.org/licenses/>.

// Command dut is the device under test for the dnp3c conformance harness.
//
// dnp3c (github.com/stepfunc/dnp3c) runs the DNP Users Group's IED
// certification procedures against an outstation. The procedures ask for
// things only the device's owner can do — cycle its power, generate events,
// check that an output really operated — and dnp3c hands those to an
// integration plugin. Ours, in ../plugin, is a thin shim that forwards each
// call here over HTTP, so everything that knows about go-dnp3 is in Go:
//
//	dut -listen 127.0.0.1:20000 -control 127.0.0.1:20080
//
// The DNP3 side is a go-dnp3 outstation on -listen. The control side takes a
// JSON request per plugin call on -control and answers with what was done or
// why a check failed. Nothing on the control side is a DNP3 concern; it exists
// only so a Java harness can drive a Go outstation.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "dut:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		listen  = flag.String("listen", "127.0.0.1:20000", "serve DNP3 on `address`")
		control = flag.String("control", "127.0.0.1:20080", "serve the plugin's requests on `address`")
		subset  = flag.Int("level", 3, "the DNP3 subset `level` to report in, 2 or 3")
		verbose = flag.Bool("v", false, "log protocol activity")
	)
	flag.Parse()

	level := slog.LevelWarn
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	if *subset != 2 && *subset != 3 {
		return fmt.Errorf("-level %d: the DUT reports at subset level 2 or 3", *subset)
	}
	dev := newDevice(*listen, *subset, log)
	if err := dev.start(); err != nil {
		return err
	}
	defer dev.stop()

	ln, err := net.Listen("tcp", *control)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: newRPC(dev), ReadHeaderTimeout: 5 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		// Best effort: the process is finishing either way.
		_ = srv.Close()
	}()

	fmt.Printf("dut: DNP3 on %s, control on %s\n", *listen, ln.Addr())
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
