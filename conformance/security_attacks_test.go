package conformance

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/internal/security"
	"github.com/dscsystems/go-dnp3/objects"
	"github.com/dscsystems/go-dnp3/outstation"
)

// These tests attack the authentication rather than use it: each one tries to
// get a command executed without a valid, fresh, matching authentication, and
// fails if it succeeds. They speak the wire protocol directly, so a shared
// misreading between the library's master and outstation cannot hide a hole.

var updateKey = bytes.Repeat([]byte{0x5A}, 16)

// rawExchange sends one request exactly as built and returns it with the reply.
func (h *harness) rawExchange(fc app.FuncCode, objs ...app.ObjectHeader) ([]byte, app.Fragment) {
	h.t.Helper()
	h.seq = (h.seq + 1) % app.SeqModulus
	frag := app.BuildRequest(nil, app.Control{Fir: true, Fin: true, Seq: h.seq}, fc, objs...)
	before := h.count()
	h.sendRaw(frag)
	return frag, h.await(before)
}

func authObject(t *testing.T, f app.Fragment, variation uint8) []byte {
	t.Helper()
	if len(f.Objects) != 1 || f.Objects[0].Group != 120 || f.Objects[0].Variation != variation {
		t.Fatalf("want one g120v%d, got %+v (function %v)", variation, f.Objects, f.Header.Func)
	}
	d, err := app.FirstFreeFormatObject(f.Objects[0])
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func secureHarness(t *testing.T, cfg *outstation.SecureAuthenticationConfig) (*harness, *recordingHandler) {
	t.Helper()
	rec := &recordingHandler{}
	cfg.Users = map[uint16][]byte{1: updateKey}
	db := smallDB()
	db.Counter, db.FrozenCounter = 3, 3
	h := newHarnessWith(t, outstation.Config{Database: db, SecureAuthentication: cfg}, nil, rec)
	return h, rec
}

// establishSessionKeys performs the key exchanges, checking the outstation's proof
// that it received the keys, and returns
// the session control key.
func establishSessionKeys(t *testing.T, h *harness) []byte {
	t.Helper()
	_, st := h.rawExchange(app.FuncAuthRequest, app.ObjectHeader{Group: 120, Variation: 4,
		Qualifier: app.MakeQualifier(app.PrefixNone, app.RangeCount8),
		Range:     app.Range{Spec: app.RangeCount8, Count: 1}, Data: binary.LittleEndian.AppendUint16(nil, 1)})
	status, err := objects.ParseAuthKeyStatus(authObject(t, st, 5))
	if err != nil {
		t.Fatal(err)
	}

	control, monitor := bytes.Repeat([]byte{0x11}, 32), bytes.Repeat([]byte{0x22}, 32)
	status.MAC = nil
	plain := binary.LittleEndian.AppendUint16(nil, 32)
	plain = append(append(plain, control...), monitor...)
	plain = objects.AppendAuthKeyStatus(plain, status)
	for len(plain)%8 != 0 {
		plain = append(plain, 0)
	}
	wrapped, _ := security.Wrap(updateKey, plain)
	hdr, _ := app.FreeFormat(120, 6, objects.AppendAuthKeyChange(nil,
		objects.AuthKeyChange{Sequence: status.Sequence, User: 1, Wrapped: wrapped}))
	change, resp := h.rawExchange(app.FuncAuthRequest, hdr)

	ks, err := objects.ParseAuthKeyStatus(authObject(t, resp, 5))
	if err != nil || ks.Status != 1 || !bytes.Equal(ks.MAC, security.MAC(monitor, change)) {
		t.Fatalf("key change not accepted: %+v %v", ks, err)
	}
	return control
}

// challengeFor sends a request and returns it with the challenge it drew.
func challengeFor(t *testing.T, h *harness, fc app.FuncCode, objs ...app.ObjectHeader) ([]byte, app.Fragment, objects.AuthChallenge) {
	t.Helper()
	req, resp := h.rawExchange(fc, objs...)
	if resp.Header.Func != app.FuncAuthResponse {
		t.Fatalf("%v was answered with %v, want a challenge", fc, resp.Header.Func)
	}
	c, err := objects.ParseAuthChallenge(authObject(t, resp, 1))
	if err != nil {
		t.Fatal(err)
	}
	return req, resp, c
}

func sendReply(h *harness, seq uint32, mac []byte) app.Fragment {
	hdr, _ := app.FreeFormat(120, 2, objects.AppendAuthReply(nil, objects.AuthReply{Sequence: seq, User: 1, MAC: mac}))
	_, r := h.rawExchange(app.FuncAuthRequest, hdr)
	return r
}

func settle() { time.Sleep(100 * time.Millisecond) }

func TestAuthenticatedCriticalRequestExecutesExactlyOnce(t *testing.T) {
	h, rec := secureHarness(t, &outstation.SecureAuthenticationConfig{})
	control := establishSessionKeys(t, h)

	req, chal, c := challengeFor(t, h, app.FuncDirectOperate, crobHeader(1, dnp3.ControlLatchOn))
	if rec.operates != 0 {
		t.Fatal("operated before the authentication completed")
	}
	sendReply(h, c.Sequence, security.MAC(control, chal.Raw, req))
	waitFor(t, func() bool { return rec.operates == 1 })
}

func TestUnauthenticatedCriticalRequestDoesNotExecute(t *testing.T) {
	h, rec := secureHarness(t, &outstation.SecureAuthenticationConfig{})

	_, resp := h.rawExchange(app.FuncDirectOperate, crobHeader(1, dnp3.ControlLatchOn))
	if resp.Header.Func == app.FuncResponse && len(resp.Objects) > 0 {
		t.Errorf("a critical request with no authentication was answered with data: %+v", resp.Objects)
	}
	settle()
	if rec.operates != 0 {
		t.Fatalf("operated %d times with no authentication", rec.operates)
	}
}

func TestWrongMACDoesNotExecute(t *testing.T) {
	h, rec := secureHarness(t, &outstation.SecureAuthenticationConfig{})
	control := establishSessionKeys(t, h)
	req, chal, c := challengeFor(t, h, app.FuncDirectOperate, crobHeader(1, dnp3.ControlLatchOn))

	bad := security.MAC(control, chal.Raw, req)
	bad[0] ^= 1
	sendReply(h, c.Sequence, bad)
	settle()
	if rec.operates != 0 {
		t.Fatal("operated on a wrong MAC")
	}
}

// A MAC is over the challenge and the request together, so authenticating one
// command cannot authorise another.
func TestMACOverADifferentRequestDoesNotExecute(t *testing.T) {
	h, rec := secureHarness(t, &outstation.SecureAuthenticationConfig{})
	control := establishSessionKeys(t, h)
	_, chal, c := challengeFor(t, h, app.FuncDirectOperate, crobHeader(1, dnp3.ControlLatchOn))

	other := app.BuildRequest(nil, app.Control{Fir: true, Fin: true, Seq: 9}, app.FuncDirectOperate,
		crobHeader(2, dnp3.ControlLatchOn))
	sendReply(h, c.Sequence, security.MAC(control, chal.Raw, other))
	settle()
	if rec.operates != 0 {
		t.Fatal("a MAC over a different request authorised this one")
	}
}

func TestReplayedReplyAndRequestDoNotExecuteAgain(t *testing.T) {
	h, rec := secureHarness(t, &outstation.SecureAuthenticationConfig{})
	control := establishSessionKeys(t, h)
	req, chal, c := challengeFor(t, h, app.FuncDirectOperate, crobHeader(1, dnp3.ControlLatchOn))
	mac := security.MAC(control, chal.Raw, req)
	sendReply(h, c.Sequence, mac)
	waitFor(t, func() bool { return rec.operates == 1 })

	sendReply(h, c.Sequence, mac) // the same reply again
	settle()
	if rec.operates != 1 {
		t.Fatalf("a replayed reply executed the command again (%d)", rec.operates)
	}

	h.sendRaw(req) // and the same request, which needs a fresh challenge
	settle()
	if rec.operates != 1 {
		t.Fatalf("a replayed request executed without a new challenge (%d)", rec.operates)
	}
}

func TestLateReplyDoesNotExecute(t *testing.T) {
	h, rec := secureHarness(t, &outstation.SecureAuthenticationConfig{ReplyTimeout: 100 * time.Millisecond})
	control := establishSessionKeys(t, h)
	req, chal, c := challengeFor(t, h, app.FuncDirectOperate, crobHeader(1, dnp3.ControlLatchOn))

	time.Sleep(250 * time.Millisecond)
	sendReply(h, c.Sequence, security.MAC(control, chal.Raw, req))
	settle()
	if rec.operates != 0 {
		t.Fatal("a reply after the timeout executed the command")
	}
}

func TestWrongChallengeSequenceDoesNotExecute(t *testing.T) {
	h, rec := secureHarness(t, &outstation.SecureAuthenticationConfig{})
	control := establishSessionKeys(t, h)
	req, chal, c := challengeFor(t, h, app.FuncDirectOperate, crobHeader(1, dnp3.ControlLatchOn))

	sendReply(h, c.Sequence+1, security.MAC(control, chal.Raw, req))
	settle()
	if rec.operates != 0 {
		t.Fatal("a wrong challenge sequence executed the command")
	}
}

func TestBroadcastCriticalRequestDoesNotExecute(t *testing.T) {
	h, rec := secureHarness(t, &outstation.SecureAuthenticationConfig{})
	establishSessionKeys(t, h)

	before := h.count()
	h.sendTo(0xFFFF, app.FuncDirectOperate, crobHeader(1, dnp3.ControlLatchOn))
	time.Sleep(150 * time.Millisecond)
	if rec.operates != 0 || h.count() != before {
		t.Fatalf("broadcast critical request: %d operates, %d unexpected fragment(s)", rec.operates, h.count()-before)
	}
}

// AUTH_REQUEST_NO_ACK carries errors, and must never execute what it carries.
func TestAuthRequestNoAckNeverExecutes(t *testing.T) {
	h, rec := secureHarness(t, &outstation.SecureAuthenticationConfig{})
	establishSessionKeys(t, h)

	h.sendRaw(app.BuildRequest(nil, app.Control{Fir: true, Fin: true, Seq: 5}, app.FuncAuthRequestNoAck,
		crobHeader(1, dnp3.ControlLatchOn)))
	settle()
	if rec.operates != 0 {
		t.Fatal("AUTH_REQUEST_NO_ACK executed a command")
	}
}

func TestKeyMessageBudgetIsEnforced(t *testing.T) {
	h, rec := secureHarness(t, &outstation.SecureAuthenticationConfig{MaxMessages: 1})
	control := establishSessionKeys(t, h)
	req, chal, c := challengeFor(t, h, app.FuncDirectOperate, crobHeader(1, dnp3.ControlLatchOn))
	sendReply(h, c.Sequence, security.MAC(control, chal.Raw, req))
	waitFor(t, func() bool { return rec.operates == 1 })

	_, resp := h.rawExchange(app.FuncDirectOperate, crobHeader(1, dnp3.ControlLatchOn))
	if resp.Header.Func == app.FuncAuthResponse && len(resp.Objects) == 1 && resp.Objects[0].Variation == 1 {
		t.Error("a challenge was issued after the key's message budget was spent")
	}
	settle()
	if rec.operates != 1 {
		t.Fatalf("operated %d times past the budget", rec.operates)
	}
}

// Authentication objects are not a way round the gate: a READ that carries one
// is challenged like any critical request.
func TestAuthenticationObjectUnderAReadIsNotAnswered(t *testing.T) {
	h, _ := secureHarness(t, &outstation.SecureAuthenticationConfig{})
	hdr, _ := app.FreeFormat(120, 2, objects.AppendAuthReply(nil,
		objects.AuthReply{Sequence: 1, User: 1, MAC: make([]byte, 16)}))

	_, resp := h.rawExchange(app.FuncRead, hdr)
	if resp.Header.Func == app.FuncResponse && len(resp.Objects) > 0 {
		t.Errorf("a READ carrying g120 was answered with data: %+v", resp.Objects)
	}
}

// Every request that changes state is critical. These used to run for anyone:
// a peer with no credentials could clear counters, freeze them at a moment of
// its choosing, or move points between event classes.
func TestEveryStateChangingRequestNeedsAuthentication(t *testing.T) {
	for _, fc := range []app.FuncCode{
		app.FuncFreezeClear, app.FuncImmedFreeze, app.FuncFreezeAtTime, app.FuncAssignClass,
		app.FuncFreezeClearNR, app.FuncImmedFreezeNR, app.FuncFreezeAtTimeNR,
	} {
		t.Run(fc.String(), func(t *testing.T) {
			h, _ := secureHarness(t, &outstation.SecureAuthenticationConfig{})
			h.out.Update(func(d *outstation.Database) {
				d.UpdateCounter(0, dnp3.Counter{Value: 100, Flags: dnp3.Online})
			})
			waitFor(t, func() bool { c, _, _ := h.out.Database().Counter(0); return c.Value == 100 })

			objs := []app.ObjectHeader{app.ReadAllObjects(20, 0)}
			switch fc {
			case app.FuncFreezeAtTime, app.FuncFreezeAtTimeNR:
				objs = []app.ObjectHeader{freezeAt(time.Now().Add(50*time.Millisecond), 0), app.ReadAllObjects(20, 0)}
			case app.FuncAssignClass:
				objs = []app.ObjectHeader{app.ReadAllObjects(60, 2), app.ReadAllObjects(20, 0)}
			}

			h.seq = (h.seq + 1) % app.SeqModulus
			before := h.count()
			h.sendRaw(app.BuildRequest(nil, app.Control{Fir: true, Fin: true, Seq: h.seq}, fc, objs...))
			resp := h.await(before)
			if resp.Header.Func != app.FuncAuthResponse {
				t.Fatalf("answered with %v, want a challenge", resp.Header.Func)
			}
			time.Sleep(150 * time.Millisecond)
			if f, _, _ := h.out.Database().FrozenCounter(0); f.Value != 0 {
				t.Errorf("the frozen counter holds %d: the request ran without authentication", f.Value)
			}
			if c, _, _ := h.out.Database().Counter(0); c.Value != 100 {
				t.Errorf("the running counter is %d: it was cleared without authentication", c.Value)
			}
		})
	}
}

// Reading, confirming and measuring delay change nothing, so they stay open.
func TestReadOnlyRequestsNeedNoAuthentication(t *testing.T) {
	h, _ := secureHarness(t, &outstation.SecureAuthenticationConfig{})
	for _, fc := range []app.FuncCode{app.FuncRead, app.FuncDelayMeasure} {
		objs := []app.ObjectHeader{}
		if fc == app.FuncRead {
			objs = append(objs, app.ReadAllObjects(60, 1))
		}
		_, resp := h.rawExchange(fc, objs...)
		if resp.Header.Func != app.FuncResponse {
			t.Errorf("%v was answered with %v, want an ordinary response", fc, resp.Header.Func)
		}
	}
}

// With the session keys in place, an authenticated freeze does run.
func TestAuthenticatedFreezeClearRuns(t *testing.T) {
	h, _ := secureHarness(t, &outstation.SecureAuthenticationConfig{})
	control := establishSessionKeys(t, h)
	h.out.Update(func(d *outstation.Database) {
		d.UpdateCounter(0, dnp3.Counter{Value: 100, Flags: dnp3.Online})
	})
	waitFor(t, func() bool { c, _, _ := h.out.Database().Counter(0); return c.Value == 100 })

	req, chal, c := challengeFor(t, h, app.FuncFreezeClear, app.ReadAllObjects(20, 0))
	sendReply(h, c.Sequence, security.MAC(control, chal.Raw, req))
	waitFor(t, func() bool { f, _, _ := h.out.Database().FrozenCounter(0); return f.Value == 100 })
	if c, _, _ := h.out.Database().Counter(0); c.Value != 0 {
		t.Errorf("counter = %d after an authenticated FREEZE_CLEAR, want 0", c.Value)
	}
}

// Security statistics are readable and count what happens: a rejected
// authentication shows up in them.
func TestSecurityStatisticsCountRejectedAuthentications(t *testing.T) {
	h, _ := secureHarness(t, &outstation.SecureAuthenticationConfig{})
	control := establishSessionKeys(t, h)

	read := func() map[uint16]uint32 {
		r := h.request(app.FuncRead, app.ReadAllObjects(121, 1))
		out := map[uint16]uint32{}
		for _, o := range r.Objects {
			if o.Group != 121 || len(o.Data) != 7 {
				t.Fatalf("unexpected object g%dv%d with %d octets", o.Group, o.Variation, len(o.Data))
			}
			out[uint16(o.Range.Start)] = binary.LittleEndian.Uint32(o.Data[3:])
		}
		return out
	}

	before := read()
	if len(before) != 18 {
		t.Fatalf("%d statistics, want 18", len(before))
	}

	req, chal, c := challengeFor(t, h, app.FuncDirectOperate, crobHeader(1, dnp3.ControlLatchOn))
	bad := security.MAC(control, chal.Raw, req)
	bad[0] ^= 1
	sendReply(h, c.Sequence, bad)

	after := read()
	changed := 0
	for i, v := range after {
		if v > before[i] {
			changed++
		}
	}
	if changed == 0 {
		t.Error("no statistic moved after a rejected authentication")
	}
	if after[2] != before[2]+1 {
		t.Errorf("statistic 2 (rejected replies) went %d -> %d, want one more", before[2], after[2])
	}
}
