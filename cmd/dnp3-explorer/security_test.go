package main

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/dscsystems/go-dnp3/secevent"
)

func TestSecurityScreen(t *testing.T) {
	m := testModel()
	dev := m.conn.target()
	m.applySessionMsg(secEventMsg{dev, secevent.Event{Time: time.Now(), Kind: secevent.KindRemoteRestart,
		Severity: secevent.Warning, Detail: "the outstation restarted"}})
	m.applySessionMsg(secEventMsg{dev, secevent.Event{Time: time.Now(), Kind: secevent.KindSeqResponseRepeat,
		Severity: secevent.Info, Count: 3, Detail: "response repeated"}})
	// A late event from the session for a device no longer selected.
	m.applySessionMsg(secEventMsg{"elsewhere:20000", secevent.Event{Kind: secevent.KindRemoteRestart,
		Severity: secevent.Alert, Detail: "from the old device"}})
	if len(m.security) != 2 {
		t.Fatalf("%d rows, want 2: the straggler was filed", len(m.security))
	}

	m = press(m, "6")
	if m.screen != ScreenSecurity {
		t.Fatalf("screen = %v, want Security", m.screen)
	}
	out := m.View().Content
	for _, want := range []string{"remote.restart", "the outstation restarted", "(×3)", "WARNING"} {
		if !strings.Contains(out, want) {
			t.Errorf("Security screen lacks %q:\n%s", want, out)
		}
	}

	m.filter = "restart"
	if n := m.rowCount(); n != 1 {
		t.Errorf("filtered rows = %d, want 1", n)
	}
	m.filter = ""

	m = press(m, "1")
	if out := m.View().Content; !strings.Contains(out, "1 warning+") {
		t.Errorf("overview does not count the warning:\n%s", out)
	}

	m = press(m, "6")
	m = press(m, "x")
	if len(m.security) != 0 || m.secAlerts != 0 {
		t.Errorf("clear left %d rows, %d alerts", len(m.security), m.secAlerts)
	}
}

// The policy the explorer builds reports and never refuses.
func TestSecurityPolicyOnlyReports(t *testing.T) {
	c := &connection{msgs: make(chan tea.Msg, 1)}
	p := securityPolicy(c, "dev:20000")
	if err := p.Valid(); err != nil {
		t.Fatal(err)
	}
	if p.RejectUnlisted || p.Functions != nil || p.FloodAction != secevent.Observed {
		t.Errorf("policy refuses something: %+v", p)
	}
	p.Sink.HandleEvent(secevent.Event{Kind: secevent.KindRemoteRestart})
	if msg, ok := (<-c.msgs).(secEventMsg); !ok || msg.event.Kind != secevent.KindRemoteRestart || msg.device != "dev:20000" {
		t.Errorf("sink delivered %v", msg)
	}
}

// Switching to another device forgets the previous one's security events.
func TestSecurityForgottenWithDevice(t *testing.T) {
	m := testModel()
	m.applySessionMsg(secEventMsg{m.conn.target(), secevent.Event{Kind: secevent.KindRemoteRestart,
		Severity: secevent.Warning}})
	m.forgetDevice()
	if len(m.security) != 0 || m.secAlerts != 0 {
		t.Errorf("after forgetDevice: %d rows, %d alerts", len(m.security), m.secAlerts)
	}
}
