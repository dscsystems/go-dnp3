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
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/master"
)

// link is the connection setup: which device, over what, and as whom.
//
// These are the parameters an operator has to get right in front of an
// unfamiliar device and usually cannot, because a link address read off a
// drawing is a guess until something answers. Quitting and restarting the tool
// to try 11 instead of 10 is how ten minutes of commissioning becomes an
// afternoon, so they are editable while it runs and applied by reconnecting.
type link struct {
	// Exactly one of Demo, Serial and Host describes the transport.
	Demo   bool
	Serial string
	Baud   int
	Host   string

	Local   uint16
	Remote  uint16
	Timeout time.Duration
	Poll    time.Duration

	// Unsolicited asks the outstation to report events without being polled.
	//
	// It lives on the connection rather than in the model because the request
	// is part of the master's startup sequence: an operator who turns it off
	// means off, and a session rebuilt by a reconnect would otherwise enable it
	// again the moment the link came back.
	Unsolicited bool
}

// unsolMask is the classes to enable after the startup poll, or none.
func unsolMask(on bool) dnp3.Class {
	if on {
		return dnp3.Class123
	}
	return 0
}

// target names the device for the header and the overview.
func (l link) target() string {
	switch {
	case l.Demo:
		return "demo (in-process outstation)"
	case l.Serial != "":
		return "serial " + l.Serial
	default:
		return l.Host
	}
}

// transport names the link the device is reached over.
func (l link) transport() string {
	switch {
	case l.Demo:
		return "in-memory pipe"
	case l.Serial != "":
		return fmt.Sprintf("serial %d baud", l.Baud)
	default:
		return "tcp"
	}
}

// address renders the transport the way the editor accepts it back, so what
// the operator sees in the field is what they could have typed.
func (l link) address() string {
	switch {
	case l.Demo:
		return "demo"
	case l.Serial != "":
		return fmt.Sprintf("%s@%d", l.Serial, l.Baud)
	default:
		return l.Host
	}
}

// sameDevice reports whether two setups point at the same physical device.
//
// Changing a timeout leaves the measurements on screen meaningful; changing
// the address or the link address does not, because they then describe a
// device this tool is no longer talking to.
func (l link) sameDevice(o link) bool {
	return l.Demo == o.Demo && l.Serial == o.Serial && l.Baud == o.Baud &&
		l.Host == o.Host && l.Local == o.Local && l.Remote == o.Remote
}

// parseAddress reads the one field that covers every transport: "demo", a
// serial device with an optional rate, or a host and port.
//
// One field rather than three because the transports are alternatives, not a
// combination: a form with an empty serial box next to a filled host box
// invites the reader to wonder which one is in force.
func parseAddress(s string, into *link) error {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return fmt.Errorf("outstation: give a host:port, a serial device, or demo")

	case strings.EqualFold(s, "demo"):
		into.Demo, into.Serial, into.Host = true, "", ""
		return nil

	case strings.HasPrefix(s, "/"), strings.HasPrefix(s, "."),
		strings.HasPrefix(strings.ToUpper(s), "COM"):

		dev, baud := s, into.Baud
		if at := strings.LastIndex(s, "@"); at >= 0 {
			dev = strings.TrimSpace(s[:at])
			n, err := strconv.Atoi(strings.TrimSpace(s[at+1:]))
			if err != nil || n <= 0 {
				return fmt.Errorf("outstation: %q is not a line rate", s[at+1:])
			}
			baud = n
		}
		if baud <= 0 {
			baud = 9600
		}
		into.Demo, into.Serial, into.Host, into.Baud = false, dev, "", baud
		return nil

	default:
		// A bare host is almost always a slip rather than an intention, and
		// defaulting the port silently is how a tool ends up reporting that it
		// cannot reach a device the operator never asked for.
		host, port, err := net.SplitHostPort(s)
		if err != nil {
			return fmt.Errorf("outstation: %q needs a port, as host:port", s)
		}
		if host == "" {
			return fmt.Errorf("outstation: %q has no host", s)
		}
		if n, err := strconv.Atoi(port); err != nil || n <= 0 || n > 65535 {
			return fmt.Errorf("outstation: %q is not a port", port)
		}
		into.Demo, into.Serial, into.Host = false, "", s
		return nil
	}
}

// validate checks the parts that only make sense together.
func (l link) validate() error {
	if l.Local == l.Remote {
		// The link layer addresses the two ends apart. Equal addresses mean
		// every frame this master sends is addressed to itself, and the
		// symptom is a link that never comes up for no visible reason.
		return fmt.Errorf("addresses: local and remote are both %d; they must differ", l.Local)
	}
	if l.Timeout <= 0 {
		return fmt.Errorf("timeout: must be longer than zero")
	}
	if l.Poll < 0 {
		return fmt.Errorf("poll: cannot be negative; use 0 to disable it")
	}
	return nil
}

// parseLinkAddr reads a link address, which is a 16-bit number.
func parseLinkAddr(what, s string) (uint16, error) {
	n, err := strconv.ParseUint(strings.TrimSpace(s), 10, 16)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not an address between 0 and 65535", what, s)
	}
	return uint16(n), nil
}

// parseInterval reads a duration, accepting a bare number as seconds because
// that is what an operator in a hurry types.
func parseInterval(what, s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("%s: give a duration, such as 5s", what)
	}
	if n, err := strconv.Atoi(s); err == nil {
		return time.Duration(n) * time.Second, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not a duration, such as 5s or 500ms", what, s)
	}
	return d, nil
}

// ---------- supervisor ----------

// supervisor owns the session lifecycle so it can be torn down and rebuilt
// with new parameters while the interface stays up.
//
// One session runs at a time. Restarting cancels the old context and waits for
// its goroutines to finish before starting any new ones, so a reconnect can
// never leave two sessions pushing measurements into the same model — which
// would show an operator a blend of two devices with nothing to say so.
type supervisor struct {
	conn *connection
	root context.Context
	log  *slog.Logger

	// op serialises restarts. A second reconnect arriving while the first is
	// still tearing down waits for it rather than interleaving with it.
	op sync.Mutex

	mu     sync.Mutex
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// start stops whatever is running and brings up a session for p.
func (s *supervisor) start(p link) {
	s.op.Lock()
	defer s.op.Unlock()

	s.stopLocked()

	ctx, cancel := context.WithCancel(s.root)
	s.mu.Lock()
	s.cancel = cancel
	s.mu.Unlock()

	ch := buildChannel(ctx, &s.wg, p, s.log)

	h := &handler{conn: s.conn}
	sess := master.New(master.Config{
		LocalAddr:          p.Local,
		RemoteAddr:         p.Remote,
		ResponseTimeout:    p.Timeout,
		IntegrityOnStartup: true,
		// The standard's startup sequence stops unsolicited reporting before
		// the integrity poll either way: an event stream racing the poll
		// produces a picture that is neither. What the setting decides is
		// whether it is turned back on afterwards.
		DisableUnsolOnStartup: true,
		UnsolClassMask:        unsolMask(p.Unsolicited),
		KeepAlive:             30 * time.Second,
		Log:                   s.log,
	}, h)
	// Written before the session goroutine starts, which is what makes it safe
	// for that goroutine to read without a lock.
	h.sess = sess
	s.conn.adopt(sess, p)

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() { _ = ch.Close() }()
		if err := sess.Run(ctx, ch); err != nil && ctx.Err() == nil {
			s.conn.push(statusMsg{text: "failed", err: err.Error()})
		}
	}()

	if p.Poll > 0 {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			// Give the startup sequence a moment before adding to the queue.
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			if err := sess.AddPeriodicScan(ctx, p.Poll, dnp3.Class123); err != nil && ctx.Err() == nil {
				s.conn.push(logMsg{level: "warn", text: "periodic poll: " + err.Error()})
			}
		}()
	}

	// A connection watcher, so the header reflects reality even when the
	// outstation has nothing to say.
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
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
				s.conn.push(statusMsg{
					text:      state,
					connected: sess.Connected(),
					stats:     sess.Stats(),
					iin:       sess.LastIIN().String(),
				})
			}
		}
	}()
}

// stop tears the current session down and waits for it.
func (s *supervisor) stop() {
	s.op.Lock()
	defer s.op.Unlock()
	s.stopLocked()
}

func (s *supervisor) stopLocked() {
	s.mu.Lock()
	cancel := s.cancel
	s.cancel = nil
	s.mu.Unlock()

	if cancel == nil {
		return
	}
	cancel()
	// Waiting is the point: the next session must not start until this one has
	// stopped touching the model.
	s.wg.Wait()
}

// unsolicitedAction says what a u or U keystroke is about to do.
func unsolicitedAction(on bool) string {
	if on {
		return "enabling unsolicited reporting for classes 1, 2 and 3"
	}
	return "disabling unsolicited reporting"
}

// unsolicitedOn reports whether the session is asking the outstation to report
// without being polled.
func (m *Model) unsolicitedOn() bool { return m.conn.current().Unsolicited }

// unsolicitedText is what the session panel says about it.
//
// "asked for" rather than "on": the master can ask, and only the outstation
// decides whether anything arrives. A device that does not implement
// unsolicited reporting is entitled to say nothing at all.
func (m *Model) unsolicitedText() string {
	if m.unsolicitedOn() {
		return "asked for (classes 1, 2, 3)"
	}
	return "not asked for"
}

// unsolicitedButton toggles the setting: it offers the opposite of what is in
// force, because a button that repeats the current state does nothing.
func unsolicitedButton(on bool) button {
	if on {
		return button{label: "Unsol off", key: "U", on: true}
	}
	return button{label: "Unsol on", key: "u"}
}
