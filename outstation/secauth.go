package outstation

import (
	"fmt"

	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/internal/stack"
	"github.com/dscsystems/go-dnp3/secevent"
)

// authEvent reports a secure authentication event about request r. A
// failure also counts towards auth.brute_force.
func (s *Session) authEvent(k secevent.Kind, r stack.Received, user uint16, action secevent.Action, value, detail string) {
	if !s.sec.Enabled() {
		return
	}
	hdr, _, err := app.ParseHeader(r.Fragment)
	var hp *app.Header
	if err == nil {
		hp = &hdr
	}
	e := appEvent(k, r, hp)
	if user != 0 {
		e.Fields |= secevent.HasUser
		e.User = user
	}
	e.Action, e.Value, e.Detail = action, value, detail
	s.emit(e)

	if k == secevent.KindAuthFailure || k == secevent.KindAuthKeyChangeFailed {
		if s.sec.Exceeded("auth", s.cfg.Security.AuthFailureRate.Or(secevent.DefaultAuthFailureRate)) {
			b := appEvent(secevent.KindAuthBruteForce, r, hp)
			b.Detail = "secure authentication failures from this connection exceed the policy's rate"
			s.emit(b)
		}
	}
}

// authFailed reports a challenge reply that did not verify, naming the
// first check it failed.
func (s *Session) authFailed(r stack.Received, user uint16, seqOK, srcOK, inTime, keysOK bool) {
	if !s.sec.Enabled() {
		return
	}
	reason := "bad_mac"
	switch {
	case !seqOK:
		reason = "wrong_challenge_sequence"
	case !srcOK:
		reason = "reply_from_other_source"
	case !inTime:
		reason = "late_reply"
	case !keysOK:
		reason = "no_session_keys"
	}
	s.authEvent(secevent.KindAuthFailure, r, user, secevent.Rejected, reason,
		fmt.Sprintf("challenge reply for user %d failed: %s", user, reason))
}
