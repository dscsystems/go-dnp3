package master

import (
	"context"
	"fmt"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/objects"
)

// FileCredentials enables AUTHENTICATE_FILE before file opens and deletes.
// DNP3 file authentication sends the password on the wire; protect the channel
// when credentials require confidentiality.
type FileCredentials struct{ User, Password string }

// AuthenticateFile exchanges credentials for a file authentication key.
func (s *Session) AuthenticateFile(ctx context.Context, user, password string) (uint32, error) {
	t := &transfer{}
	if err := s.run(ctx, newFileAuthTask(t, FileCredentials{user, password}, nil)); err != nil {
		return 0, err
	}
	return t.key, t.err
}

func newFileAuthTask(t *transfer, credentials FileCredentials, next *task) *task {
	return &task{name: "file-authenticate", funcCode: app.FuncAuthenticateFile, priority: priorityCommand,
		build: func(b *app.Builder) error {
			if len(credentials.User)+len(credentials.Password) > app.MaxFreeFormatObject-objects.FileAuthSize {
				return dnp3.ErrBadConfig
			}
			h, err := app.FreeFormat(70, 2, objects.AppendFileAuth(nil, objects.FileAuth{User: credentials.User, Password: credentials.Password}))
			if err != nil {
				return err
			}
			return b.AddObject(h)
		},
		onFragment: func(f app.Fragment) {
			data, ok := fileObject(f, 2)
			if !ok {
				t.fail(fmt.Errorf("master: file authentication: %w", dnp3.ErrMalformed))
				return
			}
			a, err := objects.ParseFileAuth(data)
			if err != nil {
				t.fail(err)
				return
			}
			t.key = a.Key
		},
		onDone: func(iin app.IIN) {
			t.checkSupported(iin, "authenticate")
			if t.key == 0 && t.err == nil {
				t.fail(dnp3.FilePermissionDenied.Err())
			}
		},
		next: func() *task {
			if t.err != nil {
				return nil
			}
			return next
		},
	}
}
