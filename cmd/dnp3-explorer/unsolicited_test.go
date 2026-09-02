package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/channel"
	"github.com/dscsystems/go-dnp3/master"
)

// Unsolicited reporting is a request the master makes on every connection, so
// the thing that matters is not that the keystroke sends something — it is
// that the answer sticks. An operator who turned it off and then lost the link
// for a moment must not find it back on.

func TestUnsolicitedDefaultsToOn(t *testing.T) {
	m := testModel()
	m.conn.setUnsolicited(true)

	if !m.unsolicitedOn() {
		t.Fatal("the setting did not take")
	}
	if got := m.unsolicitedText(); !strings.Contains(got, "asked for") {
		t.Errorf("the panel says %q", got)
	}
	// The classes the master asks for, which is what the session is built with.
	if got := unsolMask(true); got != dnp3.Class123 {
		t.Errorf("mask = %v, want classes 1, 2 and 3", got)
	}
	if got := unsolMask(false); got != 0 {
		t.Errorf("mask = %v, want none", got)
	}
}

// The keys send the request and record it, in that order of importance.
func TestUnsolicitedKeysRecordTheChoice(t *testing.T) {
	m := testModel()
	m.conn.setUnsolicited(true)

	m, cmd := pressCmd(m, "U")
	if cmd == nil {
		t.Fatal("U sent nothing to the outstation")
	}
	if m.unsolicitedOn() {
		t.Error("U did not record the choice")
	}

	m, cmd = pressCmd(m, "u")
	if cmd == nil {
		t.Fatal("u sent nothing to the outstation")
	}
	if !m.unsolicitedOn() {
		t.Error("u did not record the choice")
	}
}

// The whole point: the choice survives the session being rebuilt.
func TestUnsolicitedSurvivesReconnect(t *testing.T) {
	m := testModel()
	m.conn.setUnsolicited(true)

	m = press(m, "U")

	// A reconnect builds a new session from the connection parameters, which
	// is where the choice now lives.
	if m.conn.current().Unsolicited {
		t.Fatal("the connection would ask for unsolicited reporting again")
	}
	if got := unsolMask(m.conn.current().Unsolicited); got != 0 {
		t.Errorf("the rebuilt session would enable %v", got)
	}
}

// Editing the connection must not quietly reset it: the form has no field for
// unsolicited reporting, so it has to carry through untouched.
func TestUnsolicitedSurvivesTheConnectionForm(t *testing.T) {
	m := testModel()
	m.conn.setUnsolicited(false)

	m.openConnectionForm()
	m.form.fields[fieldPoll].value = "2s"

	got, err := parseForm(m.conn.current(), m.form.fields)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.Unsolicited {
		t.Error("editing the poll interval turned unsolicited reporting back on")
	}
	if got.Poll != 2*time.Second {
		t.Errorf("poll = %v, want the edit to have taken", got.Poll)
	}
}

// The toolbar offers the opposite of what is in force; a button that repeats
// the current state does nothing.
func TestUnsolicitedButtonToggles(t *testing.T) {
	on := unsolicitedButton(true)
	if on.key != "U" || !on.on {
		t.Errorf("with it on the button is %+v, want the key that turns it off", on)
	}
	off := unsolicitedButton(false)
	if off.key != "u" || off.on {
		t.Errorf("with it off the button is %+v, want the key that turns it on", off)
	}
}

// And it actually works: the demo device pushes events without being polled,
// which is the only way the setting can be seen to do anything.
func TestUnsolicitedArrivesFromTheDemoDevice(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	mch, och := channel.Pipe()
	demo := newDemoOutstation(discardLogger())

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = demo.session.Run(ctx, och) }()
	go func() { defer wg.Done(); demo.run(ctx) }()

	conn := &connection{msgs: make(chan tea.Msg, 512), ctx: ctx}
	h := &handler{conn: conn}
	sess := master.New(master.Config{
		LocalAddr: 1, RemoteAddr: 10, ResponseTimeout: 5 * time.Second,
		IntegrityOnStartup:    true,
		DisableUnsolOnStartup: true,
		UnsolClassMask:        unsolMask(true),
	}, h)
	h.sess = sess
	conn.adopt(sess, link{Demo: true, Local: 1, Remote: 10, Unsolicited: true})

	wg.Add(1)
	go func() { defer wg.Done(); _ = sess.Run(ctx, mch) }()
	t.Cleanup(func() {
		cancel()
		_ = mch.Close()
		_ = och.Close()
		wg.Wait()
	})

	// Nothing is polled here on purpose: anything that arrives, arrives
	// because the outstation sent it.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if sess.Stats().Unsolicited > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no unsolicited response arrived: %+v", sess.Stats())
}
