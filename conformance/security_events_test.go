package conformance

import (
	"encoding/binary"
	"sync"
	"testing"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/internal/security"
	"github.com/dscsystems/go-dnp3/objects"
	"github.com/dscsystems/go-dnp3/outstation"
	"github.com/dscsystems/go-dnp3/secevent"
)

// The attacks in security_attacks_test.go, again, checking that each is
// reported as well as refused.

type authEvents struct {
	mu     sync.Mutex
	events []secevent.Event
}

func (a *authEvents) HandleEvent(e secevent.Event) {
	a.mu.Lock()
	a.events = append(a.events, e)
	a.mu.Unlock()
}

func (a *authEvents) find(t *testing.T, k secevent.Kind, match func(secevent.Event) bool) secevent.Event {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		a.mu.Lock()
		for _, e := range a.events {
			if e.Kind == k && (match == nil || match(e)) {
				a.mu.Unlock()
				return e
			}
		}
		a.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no matching %s event", k)
	return secevent.Event{}
}

func secureEventHarness(t *testing.T, cfg *outstation.SecureAuthenticationConfig, p *secevent.Policy) (*harness, *recordingHandler, *authEvents) {
	t.Helper()
	ev := &authEvents{}
	if p == nil {
		p = &secevent.Policy{}
	}
	p.Sink = secevent.SyncSink(ev)
	rec := &recordingHandler{}
	cfg.Users = map[uint16][]byte{1: updateKey}
	db := smallDB()
	h := newHarnessWith(t, outstation.Config{Database: db, SecureAuthentication: cfg, Security: p}, nil, rec)
	return h, rec, ev
}

func TestAuthenticationEvents(t *testing.T) {
	h, rec, ev := secureEventHarness(t, &outstation.SecureAuthenticationConfig{}, nil)
	control := establishSessionKeys(t, h)
	ev.find(t, secevent.KindAuthKeyChange, func(e secevent.Event) bool { return e.User == 1 })

	req, chal, c := challengeFor(t, h, app.FuncDirectOperate, crobHeader(1, dnp3.ControlLatchOn))
	ev.find(t, secevent.KindAuthChallenge, nil)
	sendReply(h, c.Sequence, security.MAC(control, chal.Raw, req))
	waitFor(t, func() bool { return rec.operates == 1 })
	ev.find(t, secevent.KindAuthSuccess, func(e secevent.Event) bool { return e.User == 1 && e.AppFunc == uint8(app.FuncDirectOperate) })

	req, chal, c = challengeFor(t, h, app.FuncDirectOperate, crobHeader(1, dnp3.ControlLatchOn))
	bad := security.MAC(control, chal.Raw, req)
	bad[0] ^= 1
	sendReply(h, c.Sequence, bad)
	ev.find(t, secevent.KindAuthFailure, func(e secevent.Event) bool { return e.Value == "bad_mac" })

	_, chal, c = challengeFor(t, h, app.FuncDirectOperate, crobHeader(1, dnp3.ControlLatchOn))
	sendReply(h, c.Sequence+1, security.MAC(control, chal.Raw, req))
	ev.find(t, secevent.KindAuthFailure, func(e secevent.Event) bool { return e.Value == "wrong_challenge_sequence" })

	h.rawExchange(app.FuncAuthRequest, app.ObjectHeader{Group: 120, Variation: 4,
		Qualifier: app.MakeQualifier(app.PrefixNone, app.RangeCount8),
		Range:     app.Range{Spec: app.RangeCount8, Count: 1}, Data: binary.LittleEndian.AppendUint16(nil, 77)})
	ev.find(t, secevent.KindAuthUnknownUser, func(e secevent.Event) bool { return e.User == 77 })

	hdr, _ := app.FreeFormat(120, 2, objects.AppendAuthReply(nil, objects.AuthReply{Sequence: 1, User: 1, MAC: make([]byte, 16)}))
	h.rawExchange(app.FuncRead, hdr)
	ev.find(t, secevent.KindAuthObjectSmuggled, nil)

	h.sendRaw(app.BuildRequest(nil, app.Control{Fir: true, Fin: true, Seq: 5}, app.FuncAuthRequestNoAck))
	ev.find(t, secevent.KindAuthErrorReceived, nil)
}

func TestAuthorizationDeniedAndBruteForce(t *testing.T) {
	deny := func(uint16, uint8) bool { return false }
	h, rec, ev := secureEventHarness(t, &outstation.SecureAuthenticationConfig{Authorize: deny},
		&secevent.Policy{AuthFailureRate: secevent.Rate{N: 1, Per: time.Hour}})
	control := establishSessionKeys(t, h)

	req, chal, c := challengeFor(t, h, app.FuncDirectOperate, crobHeader(1, dnp3.ControlLatchOn))
	sendReply(h, c.Sequence, security.MAC(control, chal.Raw, req))
	ev.find(t, secevent.KindAuthAuthorizationDenied, func(e secevent.Event) bool { return e.User == 1 })
	settle()
	if rec.operates != 0 {
		t.Fatal("operated although not authorized")
	}

	for range 2 {
		_, chal, c = challengeFor(t, h, app.FuncDirectOperate, crobHeader(1, dnp3.ControlLatchOn))
		sendReply(h, c.Sequence, make([]byte, 16))
	}
	ev.find(t, secevent.KindAuthBruteForce, nil)
}
