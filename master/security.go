package master

import (
	"crypto/hmac"
	"crypto/rand"
	"encoding/binary"
	"io"
	"time"

	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/internal/security"
	"github.com/dscsystems/go-dnp3/internal/stack"
	"github.com/dscsystems/go-dnp3/objects"
)

// SecureAuthenticationConfig enables the symmetric SAv5 master exchange.
// UpdateKey must match the outstation's locally provisioned user key.
// Remote update-key management and aggressive mode are not supported.
type SecureAuthenticationConfig struct {
	User        uint16
	UpdateKey   []byte
	KeyTimeout  time.Duration
	MaxMessages uint32
}

type masterSecurity struct {
	control, monitor []byte
	until            time.Time
	messages         uint32
	lastRequest      []byte
}

func (s *Session) securityReady() bool {
	return len(s.security.control) >= 16 && time.Now().Before(s.security.until) && s.security.messages < s.securityMessageLimit()
}
func (s *Session) securityMessageLimit() uint32 {
	if s.cfg.SecureAuthentication.MaxMessages > 0 {
		return s.cfg.SecureAuthentication.MaxMessages
	}
	return 500
}

func (s *Session) newSessionKeysTask(next *task) *task {
	cfg := s.cfg.SecureAuthentication
	var status objects.AuthKeyStatus
	var control, monitor, changeASDU []byte
	change := &task{name: "auth-key-change", funcCode: app.FuncAuthRequest, priority: priorityStartup, startup: next.startup}
	change.build = func(b *app.Builder) error {
		control = make([]byte, 32)
		monitor = make([]byte, 32)
		if _, err := rand.Read(control); err != nil {
			return err
		}
		if _, err := rand.Read(monitor); err != nil {
			return err
		}
		plain := binary.LittleEndian.AppendUint16(nil, 32)
		plain = append(plain, control...)
		plain = append(plain, monitor...)
		status.MAC = nil
		plain = objects.AppendAuthKeyStatus(plain, status)
		for len(plain)%8 != 0 {
			plain = append(plain, 0)
		}
		wrapped, err := security.Wrap(cfg.UpdateKey, plain)
		clear(plain)
		if err != nil {
			return err
		}
		h, err := app.FreeFormat(120, 6, objects.AppendAuthKeyChange(nil, objects.AuthKeyChange{Sequence: status.Sequence, User: cfg.User, Wrapped: wrapped}))
		if err != nil {
			return err
		}
		if err := b.AddObject(h); err != nil {
			return err
		}
		changeASDU = append([]byte(nil), b.Bytes()...)
		return nil
	}
	change.onFragment = func(f app.Fragment) {
		h, ok := singleAuthObject(f, 5)
		if !ok {
			change.failure = security.ErrAuthentication
			return
		}
		data, err := app.FirstFreeFormatObject(h)
		if err != nil {
			change.failure = err
			return
		}
		v, err := objects.ParseAuthKeyStatus(data)
		if err != nil || v.User != cfg.User || v.Status != 1 || v.Algorithm != 4 || !hmac.Equal(v.MAC, security.MAC(monitor, changeASDU)) {
			change.failure = security.ErrAuthentication
			return
		}
		clear(s.security.control)
		clear(s.security.monitor)
		s.security.control = control
		s.security.monitor = monitor
		s.security.messages = 0
		ttl := cfg.KeyTimeout
		if ttl <= 0 {
			ttl = 10 * time.Minute
		}
		s.security.until = time.Now().Add(ttl)
	}
	change.next = func() *task { return next }
	request := &task{name: "auth-key-status", funcCode: app.FuncAuthRequest, priority: priorityStartup, startup: next.startup}
	request.build = func(b *app.Builder) error {
		if cfg.User == 0 || (len(cfg.UpdateKey) != 16 && len(cfg.UpdateKey) != 32) {
			return security.ErrAuthentication
		}
		return b.AddObject(app.ObjectHeader{Group: 120, Variation: 4, Qualifier: app.MakeQualifier(app.PrefixNone, app.RangeCount8), Range: app.Range{Spec: app.RangeCount8, Count: 1}, Data: binary.LittleEndian.AppendUint16(nil, cfg.User)})
	}
	request.onFragment = func(f app.Fragment) {
		h, ok := singleAuthObject(f, 5)
		if !ok {
			request.failure = security.ErrAuthentication
			return
		}
		data, err := app.FirstFreeFormatObject(h)
		if err != nil {
			request.failure = err
			return
		}
		status, err = objects.ParseAuthKeyStatus(data)
		if err != nil || status.User != cfg.User || (status.WrapAlgorithm != 1 && status.WrapAlgorithm != 2) || (status.WrapAlgorithm == 1 && len(cfg.UpdateKey) != 16) || (status.WrapAlgorithm == 2 && len(cfg.UpdateKey) != 32) {
			request.failure = security.ErrAuthentication
		}
	}
	request.next = func() *task { return change }
	request.done, next.done = next.done, nil
	return request
}

func singleAuthObject(f app.Fragment, variation uint8) (app.ObjectHeader, bool) {
	if len(f.Objects) != 1 {
		return app.ObjectHeader{}, false
	}
	h := f.Objects[0]
	return h, h.Group == 120 && h.Variation == variation && h.Count() == 1 && h.Qualifier == app.FreeFormatQualifier
}

func (s *Session) onAuthenticationResponse(w io.Writer, r stack.Received, f app.Fragment) {
	t := s.inflight
	if s.cfg.SecureAuthentication == nil || t == nil || f.Header.Control.Uns || !f.Header.Control.Single() || f.Header.Control.Con || f.Header.Control.Seq != t.seq {
		return
	}
	if t.funcCode == app.FuncAuthRequest {
		s.onSolicited(w, f)
		return
	}
	h, ok := singleAuthObject(f, 1)
	if !ok {
		s.inflight = nil
		s.completeTask(t, security.ErrAuthentication)
		return
	}
	data, err := app.FirstFreeFormatObject(h)
	if err != nil {
		s.inflight = nil
		s.completeTask(t, err)
		return
	}
	c, err := objects.ParseAuthChallenge(data)
	if err != nil || !s.securityReady() || c.Algorithm != 4 || c.Reason != 1 || (c.User != 0 && c.User != s.cfg.SecureAuthentication.User) || len(c.Data) != 32 {
		s.inflight = nil
		s.completeTask(t, security.ErrAuthentication)
		return
	}
	mac := security.MAC(s.security.control, f.Raw, s.security.lastRequest)
	reply, _ := app.FreeFormat(120, 2, objects.AppendAuthReply(nil, objects.AuthReply{Sequence: c.Sequence, User: s.cfg.SecureAuthentication.User, MAC: mac}))
	fragment := app.BuildRequest(nil, app.Control{Fir: true, Fin: true, Seq: t.seq}, app.FuncAuthRequest, reply)
	if err := s.stack.Send(w, fragment); err != nil {
		s.inflight = nil
		s.completeTask(t, err)
		return
	}
	s.security.messages++
	if t.noResponse {
		// The command still needs its authentication challenge/reply exchange,
		// even though its application function has no normal response.
		s.inflight = nil
		s.completeTask(t, nil)
		return
	}
	t.deadline = time.Now().Add(s.cfg.ResponseTimeout)
}
