package outstation

import (
	"crypto/rand"
	"encoding/binary"
	"io"
	"time"

	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/internal/stack"
	"github.com/dscsystems/go-dnp3/objects"
)

// FileAuthenticator validates credentials. The session generates and tracks
// the returned wire key; a handler must never log the password.
type FileAuthenticator func(user, password string) bool

type fileAuthorization struct {
	key    uint32
	source uint16
	until  time.Time
}

func (s *Session) onAuthenticateFile(w io.Writer, r stack.Received, frag app.Fragment) error {
	if !s.fileEnabled() || s.cfg.Files.Authenticate == nil {
		return s.unsupportedFile(w, r, frag.Header)
	}
	h, ok := fileObject(frag)
	if !ok || len(frag.Objects) != 1 || h.Variation != 2 || h.Count() != 1 {
		s.iin = s.iin.Set(app.IINParameterError)
		return s.respond(w, r, frag.Header, nil)
	}
	data, err := app.FirstFreeFormatObject(h)
	if err != nil {
		s.iin = s.iin.Set(app.IINParameterError)
		return s.respond(w, r, frag.Header, nil)
	}
	a, err := objects.ParseFileAuth(data)
	if err != nil || a.Key != 0 {
		s.iin = s.iin.Set(app.IINParameterError)
		return s.respond(w, r, frag.Header, nil)
	}
	s.fileAuth = fileAuthorization{}
	if s.cfg.Files.Authenticate(a.User, a.Password) {
		var key [4]byte
		for s.fileAuth.key == 0 {
			if _, err := rand.Read(key[:]); err != nil {
				return err
			}
			s.fileAuth.key = binary.LittleEndian.Uint32(key[:])
		}
		s.fileAuth.source = r.Source
		s.fileAuth.until = s.appl.Now().Add(s.cfg.Files.Timeout)
	}
	// A failed authentication returns key zero. Credentials never appear in
	// the response or the session log.
	return s.respondFile(w, r, frag.Header, 2, objects.AppendFileAuth(nil, objects.FileAuth{Key: s.fileAuth.key}))
}

func (s *Session) fileAuthorized(source uint16, key uint32) bool {
	if s.cfg.Files.Authenticate == nil {
		return key == 0
	}
	return key != 0 && key == s.fileAuth.key && source == s.fileAuth.source && s.appl.Now().Before(s.fileAuth.until)
}
