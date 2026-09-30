package conformance

import (
	"testing"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/objects"
	"github.com/dscsystems/go-dnp3/outstation"
)

func TestFileAuthenticationRequiredAndExpires(t *testing.T) {
	h := newHarness(t, outstation.Config{Files: outstation.FileConfig{
		Handler: newMemFiles(map[string]string{"data": "content"}), Timeout: 100 * time.Millisecond,
		Authenticate: func(user, password string) bool { return user == "operator" && password == "secret" },
	}}, nil)
	r := h.request(app.FuncOpenFile, openRequest(t, "data", dnp3.FileModeRead, 16))
	if fileStatus(t, r).Status != dnp3.FilePermissionDenied {
		t.Fatal(r)
	}
	for _, password := range []string{"wrong", "secret"} {
		r = h.request(app.FuncAuthenticateFile, fileHeader(t, 2, objects.AppendFileAuth(nil, objects.FileAuth{User: "operator", Password: password})))
		if len(r.Objects) != 1 {
			t.Fatal(r)
		}
		data, err := app.FirstFreeFormatObject(r.Objects[0])
		if err != nil {
			t.Fatal(err)
		}
		auth, err := objects.ParseFileAuth(data)
		if err != nil {
			t.Fatal(err)
		}
		if auth.User != "" || auth.Password != "" {
			t.Fatal("credentials leaked", auth)
		}
		if password == "wrong" {
			if auth.Key != 0 {
				t.Fatal("invalid credentials accepted")
			}
			continue
		}
		if auth.Key == 0 {
			t.Fatal("valid credentials refused")
		}
		open := fileHeader(t, 3, objects.AppendFileCommand(nil, objects.FileCommand{Name: "data", Mode: dnp3.FileModeRead, MaxBlockSize: 16, Key: auth.Key, RequestID: 1}))
		r = h.request(app.FuncOpenFile, open)
		status := fileStatus(t, r)
		if status.Status != dnp3.FileSuccess {
			t.Fatal(status)
		}
		r = h.request(app.FuncCloseFile, fileHeader(t, 4, objects.AppendFileCommandStatus(nil, objects.FileCommandStatus{Handle: status.Handle})))
		if fileStatus(t, r).Status != dnp3.FileSuccess {
			t.Fatal(r)
		}
		time.Sleep(120 * time.Millisecond)
		r = h.request(app.FuncOpenFile, open)
		if fileStatus(t, r).Status != dnp3.FilePermissionDenied {
			t.Fatal("expired key accepted", r)
		}
	}
}
