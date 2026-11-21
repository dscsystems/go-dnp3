package conformance

import (
	"strings"
	"testing"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/objects"
	"github.com/dscsystems/go-dnp3/outstation"
	"github.com/dscsystems/go-dnp3/secevent"
)

func fileEventHarness(t *testing.T, files outstation.FileConfig, p *secevent.Policy) (*harness, *authEvents) {
	t.Helper()
	ev := &authEvents{}
	if p == nil {
		p = &secevent.Policy{}
	}
	p.Sink = secevent.SyncSink(ev)
	return newHarness(t, outstation.Config{Files: files, Security: p}, nil), ev
}

func TestFileTransferEvents(t *testing.T) {
	h, ev := fileEventHarness(t, outstation.FileConfig{
		Handler: newMemFiles(map[string]string{"data": "content"}), Timeout: time.Minute,
	}, &secevent.Policy{MaxFileSize: 4})

	st := fileStatus(t, h.request(app.FuncOpenFile, openRequest(t, "data", dnp3.FileModeRead, 16)))
	ev.find(t, secevent.KindFileReadOpened, func(e secevent.Event) bool { return e.FileName == "data" })
	h.request(app.FuncCloseFile, fileHeader(t, 4, objects.AppendFileCommandStatus(nil, objects.FileCommandStatus{Handle: st.Handle})))

	h.request(app.FuncCloseFile, fileHeader(t, 4, objects.AppendFileCommandStatus(nil, objects.FileCommandStatus{Handle: 999})))
	ev.find(t, secevent.KindFileInvalidHandle, nil)

	h.request(app.FuncOpenFile, openRequest(t, "../../etc/passwd", dnp3.FileModeRead, 16))
	ev.find(t, secevent.KindFileSuspiciousName, func(e secevent.Event) bool { return e.FileName == "../../etc/passwd" })

	h.request(app.FuncOpenFile, openRequest(t, "missing", dnp3.FileModeRead, 16))
	ev.find(t, secevent.KindFileRejected, func(e secevent.Event) bool { return e.FileStatus == dnp3.FileNotFound })

	w := fileHeader(t, 3, objects.AppendFileCommand(nil, objects.FileCommand{
		Name: "fw.bin", Mode: dnp3.FileModeWrite, Size: 1000, MaxBlockSize: 16, RequestID: 2}))
	st = fileStatus(t, h.request(app.FuncOpenFile, w))
	ev.find(t, secevent.KindFileWriteOpened, func(e secevent.Event) bool { return e.FileName == "fw.bin" })
	ev.find(t, secevent.KindFileOversized, nil)
	h.request(app.FuncAbortFile, fileHeader(t, 4, objects.AppendFileCommandStatus(nil, objects.FileCommandStatus{Handle: st.Handle})))
	ev.find(t, secevent.KindFileAborted, nil)

	del := fileHeader(t, 3, objects.AppendFileCommand(nil, objects.FileCommand{Name: "data", RequestID: 3}))
	h.request(app.FuncDeleteFile, del)
	ev.find(t, secevent.KindFileDelete, func(e secevent.Event) bool { return e.FileName == "data" })
}

func TestFileWriteDeniedAndAuthFailure(t *testing.T) {
	h, ev := fileEventHarness(t, outstation.FileConfig{
		Handler: newMemFiles(map[string]string{}), Timeout: time.Minute,
		Authenticate: func(user, password string) bool { return user == "operator" && password == "secret" },
	}, &secevent.Policy{Functions: &secevent.FunctionPolicy{DenyFileWrite: true}})

	h.request(app.FuncAuthenticateFile, fileHeader(t, 2, objects.AppendFileAuth(nil, objects.FileAuth{User: "operator", Password: "guess"})))
	e := ev.find(t, secevent.KindAuthFileFailed, nil)
	if e.Detail == "" || strings.Contains(e.Detail, "guess") {
		t.Errorf("auth failure detail %q", e.Detail)
	}

	r := h.request(app.FuncAuthenticateFile, fileHeader(t, 2, objects.AppendFileAuth(nil, objects.FileAuth{User: "operator", Password: "secret"})))
	data, _ := app.FirstFreeFormatObject(r.Objects[0])
	auth, _ := objects.ParseFileAuth(data)
	w := fileHeader(t, 3, objects.AppendFileCommand(nil, objects.FileCommand{
		Name: "x", Mode: dnp3.FileModeWrite, Size: 1, MaxBlockSize: 16, Key: auth.Key, RequestID: 2}))
	if st := fileStatus(t, h.request(app.FuncOpenFile, w)); st.Status != dnp3.FilePermissionDenied {
		t.Errorf("denied write answered %v", st.Status)
	}
	ev.find(t, secevent.KindAccessFunctionBlocked, func(e secevent.Event) bool { return e.FileName == "x" })
}
