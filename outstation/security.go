package outstation

import (
	"crypto/hmac"
	"crypto/rand"
	"encoding/binary"
	"io"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/internal/security"
	"github.com/dscsystems/go-dnp3/internal/stack"
	"github.com/dscsystems/go-dnp3/objects"
)

// SecureAuthenticationConfig enables the symmetric SAv5 challenge/reply path.
// Update keys are provisioned locally. Remote user/update-key management and
// aggressive mode are not supported. This is not a certified SAv5 profile.
type SecureAuthenticationConfig struct {
	Users               map[uint16][]byte // user number to 16- or 32-byte AES update key
	ReplyTimeout        time.Duration
	KeyTimeout          time.Duration
	MaxMessages         uint32
	StatisticsClass     dnp3.Class
	StatisticsThreshold uint32
	// Authorize grants a user the requested function. Nil grants configured
	// users all functions; applications can enforce roles here.
	Authorize func(user uint16, function uint8) bool
}

type securityUser struct {
	update, control, monitor []byte
	status                   []byte
	keyChange                []byte
	statusSource             uint16
	statusUntil              time.Time
	source                   uint16
	until                    time.Time
	messages                 uint32
}
type authenticationPending struct {
	received  stack.Received
	challenge []byte
	sequence  uint32
	until     time.Time
}
type securityState struct {
	users                 map[uint16]*securityUser
	sequence, keySequence uint32
	pending               *authenticationPending
	statistics            [18]uint32
}

func (s *Session) resetSecurity() {
	// Keep sequence numbers monotonically advancing across reconnects.
	seq, kseq := s.security.sequence, s.security.keySequence
	statistics := s.security.statistics
	for _, u := range s.security.users {
		clear(u.control)
		clear(u.monitor)
		clear(u.update)
	}
	s.security = securityState{users: map[uint16]*securityUser{}, sequence: seq, keySequence: kseq, statistics: statistics}
	if cfg := s.cfg.SecureAuthentication; cfg != nil {
		for id, key := range cfg.Users {
			if id != 0 && (len(key) == 16 || len(key) == 32) {
				s.security.users[id] = &securityUser{update: append([]byte(nil), key...)}
			}
		}
	}
}

func (s *Session) securityKeyValid(u *securityUser, source uint16) bool {
	return u != nil && len(u.control) >= 16 && u.source == source && s.appl.Now().Before(u.until) && u.messages < s.securityMaxMessages()
}
func (s *Session) securityMaxMessages() uint32 {
	if s.cfg.SecureAuthentication.MaxMessages > 0 {
		return s.cfg.SecureAuthentication.MaxMessages
	}
	return 1000
}
func (s *Session) securityReplyTimeout() time.Duration {
	if s.cfg.SecureAuthentication.ReplyTimeout > 0 {
		return s.cfg.SecureAuthentication.ReplyTimeout
	}
	return 5 * time.Second
}
func (s *Session) securityKeyTimeout() time.Duration {
	if s.cfg.SecureAuthentication.KeyTimeout > 0 {
		return s.cfg.SecureAuthentication.KeyTimeout
	}
	return 15 * time.Minute
}

// criticalFunction includes every mandatory SAv5 critical request.
func criticalFunction(f app.FuncCode) bool {
	switch f {
	case app.FuncWrite, app.FuncSelect, app.FuncOperate, app.FuncDirectOperate, app.FuncDirectOperateNR,
		app.FuncColdRestart, app.FuncWarmRestart, app.FuncInitializeAppl, app.FuncStartAppl, app.FuncStopAppl,
		app.FuncSaveConfig, app.FuncEnableUnsolicited, app.FuncDisableUnsolicited, app.FuncRecordCurrentTime,
		app.FuncOpenFile, app.FuncCloseFile, app.FuncDeleteFile, app.FuncGetFileInfo, app.FuncAuthenticateFile,
		app.FuncAbortFile, app.FuncActivateConfig:
		return true
	}
	return false
}

func (s *Session) sendAuthentication(w io.Writer, r stack.Received, seq uint8, h app.ObjectHeader) ([]byte, error) {
	if r.Broadcast {
		return nil, nil
	}
	data := app.AppendHeader(nil, app.Header{Control: app.Control{Fir: true, Fin: true, Seq: seq}, Func: app.FuncAuthResponse, IIN: s.currentIIN()})
	data = app.AppendObjectHeader(data, h)
	if len(data) > s.cfg.MaxTxFragment {
		return nil, app.ErrFragmentTooLarge
	}
	s.countSecurity(5)
	err := s.stack.SendTo(w, r.Source, data)
	return data, err
}

func (s *Session) authenticationError(w io.Writer, r stack.Received, seq uint8, user uint16, code uint8) error {
	data := binary.LittleEndian.AppendUint32(nil, s.security.sequence)
	data = binary.LittleEndian.AppendUint16(data, user)
	data = binary.LittleEndian.AppendUint16(data, 0) // this association
	data = append(data, code)
	data = objects.AppendTime48(data, dnp3.Now(s.appl.Now()))
	h, _ := app.FreeFormat(120, 7, data)
	s.countSecurity(10)
	_, err := s.sendAuthentication(w, r, seq, h)
	return err
}

func (s *Session) challengeRequest(w io.Writer, r stack.Received, frag app.Fragment) error {
	if r.Broadcast {
		s.countSecurity(9)
		return nil
	}
	ready := false
	for _, u := range s.security.users {
		ready = ready || s.securityKeyValid(u, r.Source)
	}
	if !ready {
		return s.authenticationError(w, r, frag.Header.Control.Seq, 0, 1)
	}
	s.security.sequence++
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	h, _ := app.FreeFormat(120, 1, objects.AppendAuthChallenge(nil, objects.AuthChallenge{Sequence: s.security.sequence, Algorithm: 4, Reason: 1, Data: nonce}))
	data, err := s.sendAuthentication(w, r, frag.Header.Control.Seq, h)
	if err != nil {
		return err
	}
	r.Fragment = append([]byte(nil), r.Fragment...)
	s.security.pending = &authenticationPending{received: r, challenge: data, sequence: s.security.sequence, until: s.appl.Now().Add(s.securityReplyTimeout())}
	s.countSecurity(8)
	return nil
}

func (s *Session) onAuthentication(w io.Writer, r stack.Received, frag app.Fragment) error {
	seq := frag.Header.Control.Seq
	if s.cfg.SecureAuthentication == nil {
		s.iin = s.iin.Set(app.IINNoFuncCodeSupport)
		if frag.Header.Func.NoReply() {
			return nil
		}
		return s.respond(w, r, frag.Header, nil)
	}

	if frag.Header.Func == app.FuncAuthRequestNoAck {
		// This code carries errors and must never execute an operation.
		s.security.pending = nil
		s.countSecurity(11)
		return nil
	}
	if r.Broadcast || len(frag.Objects) != 1 {
		return s.authenticationError(w, r, seq, 0, 1)
	}
	h := frag.Objects[0]
	if h.Group != 120 || h.Count() != 1 {
		return s.authenticationError(w, r, seq, 0, 1)
	}
	if h.Variation == 4 {
		if h.Qualifier != app.MakeQualifier(app.PrefixNone, app.RangeCount8) || len(h.Data) != 2 {
			return s.authenticationError(w, r, seq, 0, 1)
		}
		user := binary.LittleEndian.Uint16(h.Data)
		u := s.security.users[user]
		if u == nil {
			return s.authenticationError(w, r, seq, user, 11)
		}
		return s.sendKeyStatus(w, r, seq, user, u)
	}
	if h.Qualifier != app.FreeFormatQualifier {
		return s.authenticationError(w, r, seq, 0, 1)
	}
	data, err := app.FirstFreeFormatObject(h)
	if err != nil {
		return s.authenticationError(w, r, seq, 0, 1)
	}
	switch h.Variation {
	case 6:
		v, err := objects.ParseAuthKeyChange(data)
		if err != nil {
			return s.authenticationError(w, r, seq, 0, 1)
		}
		u := s.security.users[v.User]
		if u == nil {
			return s.authenticationError(w, r, seq, v.User, 11)
		}
		status := u.status
		u.status = nil // status challenges are single-use, including failures
		if len(status) < 11 || v.Sequence != binary.LittleEndian.Uint32(status) || u.statusSource != r.Source || !s.appl.Now().Before(u.statusUntil) {
			return s.authenticationError(w, r, seq, v.User, 1)
		}
		plain, err := security.Unwrap(u.update, v.Wrapped)
		if err != nil {
			s.countSecurity(14)
			return s.authenticationError(w, r, seq, v.User, 1)
		}
		defer clear(plain)
		if len(plain) < 2 {
			return s.authenticationError(w, r, seq, v.User, 1)
		}
		n := int(binary.LittleEndian.Uint16(plain))
		end := 2 + 2*n
		if n < 16 || n > 64 || end+len(status) > len(plain) || len(plain)-end-len(status) > 7 || !hmac.Equal(plain[end:end+len(status)], status) {
			s.countSecurity(14)
			return s.authenticationError(w, r, seq, v.User, 1)
		}
		clear(u.control)
		clear(u.monitor)
		u.control = append([]byte(nil), plain[2:2+n]...)
		u.monitor = append([]byte(nil), plain[2+n:end]...)
		u.keyChange = append([]byte(nil), frag.Raw...)
		u.source = r.Source
		u.until = s.appl.Now().Add(s.securityKeyTimeout())
		u.messages = 0
		s.security.pending = nil
		s.countSecurity(13)
		return s.sendKeyStatus(w, r, seq, v.User, u)
	case 2:
		v, err := objects.ParseAuthReply(data)
		p := s.security.pending
		if err != nil || p == nil {
			return s.authenticationError(w, r, seq, 0, 1)
		}
		s.security.pending = nil
		u := s.security.users[v.User]
		if v.Sequence != p.sequence || r.Source != p.received.Source || !s.appl.Now().Before(p.until) || !s.securityKeyValid(u, r.Source) || !hmac.Equal(v.MAC, security.MAC(u.control, p.challenge, p.received.Fragment)) {
			s.countSecurity(2)
			return s.authenticationError(w, r, seq, v.User, 1)
		}
		original, err := app.ParseRequest(nil, p.received.Fragment)
		if err != nil {
			return err
		}
		if authorize := s.cfg.SecureAuthentication.Authorize; authorize != nil && !authorize(v.User, uint8(original.Header.Func)) {
			s.countSecurity(1)
			return s.authenticationError(w, r, seq, v.User, 7)
		}
		u.messages++
		s.countSecurity(12)
		return s.handleAuthenticated(w, p.received, true)
	default:
		return s.authenticationError(w, r, seq, 0, 4)
	}
}

func (s *Session) sendKeyStatus(w io.Writer, r stack.Received, seq uint8, user uint16, u *securityUser) error {
	s.security.keySequence++
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	status := objects.AuthKeyStatus{Sequence: s.security.keySequence, User: user, WrapAlgorithm: 1, Status: 2, Challenge: nonce}
	if len(u.update) == 32 {
		status.WrapAlgorithm = 2
	}
	if s.securityKeyValid(u, r.Source) {
		status.Status = 1
	}
	if len(u.monitor) > 0 {
		status.Algorithm = 4
		status.MAC = security.MAC(u.monitor, u.keyChange)
	}
	u.status = objects.AppendAuthKeyStatus(nil, status)
	u.status = u.status[:len(u.status)-len(status.MAC)]
	u.statusSource = r.Source
	u.statusUntil = s.appl.Now().Add(s.securityReplyTimeout())
	h, _ := app.FreeFormat(120, 5, objects.AppendAuthKeyStatus(nil, status))
	_, err := s.sendAuthentication(w, r, seq, h)
	return err
}

func (s *Session) readSecurityStatistics(b *responseBuilder, h app.ObjectHeader) {
	if s.cfg.SecureAuthentication == nil || h.Variation != 1 {
		s.iin = s.iin.Set(app.IINObjectUnknown)
		return
	}
	if !forEachPointRun(h, func(start, stop uint16) {
		for i := int(start); i <= int(stop) && i < 18; i++ {
			data := []byte{byte(dnp3.Online), 0, 0}
			data = binary.LittleEndian.AppendUint32(data, s.security.statistics[i])
			b.add(rangeObjectHeader(objects.GV(121, 1), uint16(i), uint16(i), data))
		}
	}) {
		s.iin = s.iin.Set(app.IINParameterError)
	}
}

// hasAuthenticationObject prevents g120 objects from bypassing validation when
// carried under a normal READ/WRITE function code.
func hasAuthenticationObject(f app.Fragment) bool {
	for _, h := range f.Objects {
		if h.Group == 120 {
			return true
		}
	}
	return false
}

func (s *Session) countSecurity(index int) {
	s.security.statistics[index]++
	cfg := s.cfg.SecureAuthentication
	if cfg == nil {
		return
	}
	threshold := cfg.StatisticsThreshold
	if threshold == 0 {
		threshold = 10
	}
	if s.security.statistics[index]%threshold != 0 {
		return
	}
	class := cfg.StatisticsClass & dnp3.Class123
	if class == dnp3.ClassNone {
		class = dnp3.Class1
	}
	s.db.events.Add(Event{Type: dnp3.TypeSecurityStatistic, Index: uint16(index), Class: class, Variation: 2, SecurityStatistic: s.security.statistics[index], Time: dnp3.Now(s.appl.Now())})
}
func (s *Session) expireSecurity() {
	if s.cfg.SecureAuthentication == nil {
		return
	}
	if p := s.security.pending; p != nil && !s.appl.Now().Before(p.until) {
		s.security.pending = nil
		s.countSecurity(3)
		s.countSecurity(9)
	}
	for _, u := range s.security.users {
		if !s.appl.Now().Before(u.until) || u.messages >= s.securityMaxMessages() {
			clear(u.control)
			u.control = nil
		}
	}
}
