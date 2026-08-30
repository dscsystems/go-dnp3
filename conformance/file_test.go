package conformance

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/objects"
	"github.com/dscsystems/go-dnp3/outstation"
)

// File transfer is a state machine an outstation holds across several
// requests, so the interesting cases are the ones a well-behaved master never
// produces: a second open while one is in flight, a block out of order, a
// handle from a transfer that has already ended. The integration tests cannot
// reach any of them, because the master will not do those things.

// fileHarness drives an outstation serving files from memory.
func fileHarness(t *testing.T, h outstation.FileHandler, blockSize uint16) *harness {
	t.Helper()
	return newHarness(t, outstation.Config{
		Database: smallDB(),
		Files: outstation.FileConfig{
			Handler:      h,
			MaxBlockSize: blockSize,
			Timeout:      200 * time.Millisecond,
		},
	}, nil)
}

// memFiles serves a fixed set of files out of a map.
type memFiles struct {
	files map[string]string
	// written collects what a master wrote, so a test can check the octets
	// that landed rather than only the statuses that came back.
	written map[string]*strings.Builder
}

func newMemFiles(files map[string]string) *memFiles {
	return &memFiles{files: files, written: map[string]*strings.Builder{}}
}

func (m *memFiles) Info(name string) (dnp3.FileInfo, dnp3.FileStatus) {
	content, ok := m.files[name]
	if !ok {
		return dnp3.FileInfo{}, dnp3.FileNotFound
	}
	return dnp3.FileInfo{
		Name: name, Type: dnp3.FileTypeSimple, Size: uint32(len(content)),
	}, dnp3.FileSuccess
}

func (m *memFiles) List(string) ([]dnp3.FileInfo, dnp3.FileStatus) {
	return nil, dnp3.FileInvalidMode
}

func (m *memFiles) OpenRead(name string) (io.ReadCloser, dnp3.FileInfo, dnp3.FileStatus) {
	content, ok := m.files[name]
	if !ok {
		return nil, dnp3.FileInfo{}, dnp3.FileNotFound
	}
	info, _ := m.Info(name)
	return io.NopCloser(strings.NewReader(content)), info, dnp3.FileSuccess
}

func (m *memFiles) OpenWrite(name string, _ dnp3.FileMode, _ uint32) (io.WriteCloser, dnp3.FileStatus) {
	var b strings.Builder
	m.written[name] = &b
	return nopWriteCloser{&b}, dnp3.FileSuccess
}

func (m *memFiles) Delete(name string) dnp3.FileStatus {
	if _, ok := m.files[name]; !ok {
		return dnp3.FileNotFound
	}
	delete(m.files, name)
	return dnp3.FileSuccess
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// ---------- request builders ----------

func fileHeader(t *testing.T, variation uint8, obj []byte) app.ObjectHeader {
	t.Helper()
	h, err := app.FreeFormat(70, variation, obj)
	if err != nil {
		t.Fatalf("building a group 70 header: %v", err)
	}
	return h
}

func openRequest(t *testing.T, name string, mode dnp3.FileMode, blockSize uint16) app.ObjectHeader {
	t.Helper()
	return fileHeader(t, 3, objects.AppendFileCommand(nil, objects.FileCommand{
		Name: name, Mode: mode, MaxBlockSize: blockSize, RequestID: 1,
	}))
}

// fileStatus pulls the g70v4 out of a response.
func fileStatus(t *testing.T, frag app.Fragment) objects.FileCommandStatus {
	t.Helper()
	for _, h := range frag.Objects {
		if h.Group != 70 || h.Variation != 4 {
			continue
		}
		obj, err := app.FirstFreeFormatObject(h)
		if err != nil {
			t.Fatalf("free-format object: %v", err)
		}
		st, err := objects.ParseFileCommandStatus(obj)
		if err != nil {
			t.Fatalf("parsing g70v4: %v", err)
		}
		return st
	}
	t.Fatalf("the response carried no g70v4: %v", frag.Objects)
	return objects.FileCommandStatus{}
}

// transportStatus pulls the g70v6 out of a response.
func transportStatus(t *testing.T, frag app.Fragment) objects.FileTransportStatus {
	t.Helper()
	for _, h := range frag.Objects {
		if h.Group != 70 || h.Variation != 6 {
			continue
		}
		obj, err := app.FirstFreeFormatObject(h)
		if err != nil {
			t.Fatalf("free-format object: %v", err)
		}
		st, err := objects.ParseFileTransportStatus(obj)
		if err != nil {
			t.Fatalf("parsing g70v6: %v", err)
		}
		return st
	}
	t.Fatalf("the response carried no g70v6: %v", frag.Objects)
	return objects.FileTransportStatus{}
}

// openFile runs a successful open and returns the handle.
func openFile(t *testing.T, h *harness, name string, mode dnp3.FileMode, blockSize uint16) uint32 {
	t.Helper()
	st := fileStatus(t, h.request(app.FuncOpenFile, openRequest(t, name, mode, blockSize)))
	if st.Status != dnp3.FileSuccess {
		t.Fatalf("opening %q: %s", name, st.Status)
	}
	if st.Handle == 0 {
		t.Fatal("the outstation issued a zero handle")
	}
	return st.Handle
}

// ---------- procedures ----------

// A second open while a transfer is in flight must be refused. Answering with
// the handle of the transfer already running would have two masters writing
// the same file.
func TestFileSecondOpenRefused(t *testing.T) {
	h := fileHarness(t, newMemFiles(map[string]string{"/a.txt": "aaa", "/b.txt": "bbb"}), 16)

	openFile(t, h, "/a.txt", dnp3.FileModeRead, 16)

	st := fileStatus(t, h.request(app.FuncOpenFile, openRequest(t, "/b.txt", dnp3.FileModeRead, 16)))
	if st.Status != dnp3.FileTooManyOpen {
		t.Errorf("status %s, want %s", st.Status, dnp3.FileTooManyOpen)
	}
}

// A block asked for out of order cannot be served: the handler's reader is a
// stream, and answering with the next block instead would silently reassemble
// the file in the wrong order.
func TestFileReadOutOfSequenceRefused(t *testing.T) {
	h := fileHarness(t, newMemFiles(map[string]string{"/a.txt": strings.Repeat("x", 40)}), 16)
	handle := openFile(t, h, "/a.txt", dnp3.FileModeRead, 16)

	// Block 0 is the one due; ask for block 3.
	frag := h.request(app.FuncRead, fileHeader(t, 5,
		objects.AppendFileTransport(nil, objects.FileTransport{Handle: handle, Block: 3})))

	if st := transportStatus(t, frag); st.Status != dnp3.FileBlockSequence {
		t.Errorf("status %s, want %s", st.Status, dnp3.FileBlockSequence)
	}
}

func TestFileWriteOutOfSequenceRefused(t *testing.T) {
	files := newMemFiles(nil)
	h := fileHarness(t, files, 16)
	handle := openFile(t, h, "/upload.bin", dnp3.FileModeWrite, 16)

	frag := h.request(app.FuncWrite, fileHeader(t, 5,
		objects.AppendFileTransport(nil, objects.FileTransport{
			Handle: handle, Block: 7, Data: []byte("data"),
		})))

	if st := transportStatus(t, frag); st.Status != dnp3.FileBlockSequence {
		t.Errorf("status %s, want %s", st.Status, dnp3.FileBlockSequence)
	}
	if b, ok := files.written["/upload.bin"]; ok && b.Len() != 0 {
		t.Errorf("the rejected block was written anyway: %q", b.String())
	}
}

// A handle from a transfer that has ended must be refused rather than
// silently attached to whatever is open now.
func TestFileStaleHandleRefused(t *testing.T) {
	h := fileHarness(t, newMemFiles(map[string]string{"/a.txt": "aaa"}), 16)
	handle := openFile(t, h, "/a.txt", dnp3.FileModeRead, 16)

	st := fileStatus(t, h.request(app.FuncCloseFile, fileHeader(t, 4,
		objects.AppendFileCommandStatus(nil, objects.FileCommandStatus{Handle: handle}))))
	if st.Status != dnp3.FileSuccess {
		t.Fatalf("close: %s", st.Status)
	}

	frag := h.request(app.FuncRead, fileHeader(t, 5,
		objects.AppendFileTransport(nil, objects.FileTransport{Handle: handle})))
	if got := transportStatus(t, frag).Status; got != dnp3.FileNotOpen {
		t.Errorf("status %s, want %s", got, dnp3.FileNotOpen)
	}
}

func TestFileCloseWrongHandleRefused(t *testing.T) {
	h := fileHarness(t, newMemFiles(map[string]string{"/a.txt": "aaa"}), 16)
	handle := openFile(t, h, "/a.txt", dnp3.FileModeRead, 16)

	st := fileStatus(t, h.request(app.FuncCloseFile, fileHeader(t, 4,
		objects.AppendFileCommandStatus(nil, objects.FileCommandStatus{Handle: handle + 1}))))
	if st.Status != dnp3.FileInvalidHandle {
		t.Errorf("status %s, want %s", st.Status, dnp3.FileInvalidHandle)
	}
}

// Writing to a file opened for reading must be refused, and the reverse.
func TestFileModeEnforced(t *testing.T) {
	h := fileHarness(t, newMemFiles(map[string]string{"/a.txt": "aaa"}), 16)
	handle := openFile(t, h, "/a.txt", dnp3.FileModeRead, 16)

	frag := h.request(app.FuncWrite, fileHeader(t, 5,
		objects.AppendFileTransport(nil, objects.FileTransport{
			Handle: handle, Data: []byte("nope"),
		})))
	if got := transportStatus(t, frag).Status; got != dnp3.FileInvalidMode {
		t.Errorf("status %s, want %s", got, dnp3.FileInvalidMode)
	}
}

// An outstation that let a handle live forever would refuse every later
// transfer after one master went away mid-file.
func TestFileTransferTimesOut(t *testing.T) {
	h := fileHarness(t, newMemFiles(map[string]string{"/a.txt": strings.Repeat("x", 40)}), 16)
	handle := openFile(t, h, "/a.txt", dnp3.FileModeRead, 16)

	// The harness configures a 200ms inactivity timeout.
	time.Sleep(400 * time.Millisecond)

	frag := h.request(app.FuncRead, fileHeader(t, 5,
		objects.AppendFileTransport(nil, objects.FileTransport{Handle: handle})))
	if got := transportStatus(t, frag).Status; got != dnp3.FileNotOpen {
		t.Errorf("status %s, want %s", got, dnp3.FileNotOpen)
	}

	// And the slot is free again.
	openFile(t, h, "/a.txt", dnp3.FileModeRead, 16)
}

// Abort releases the handle without finishing the transfer.
func TestFileAbortReleasesTheHandle(t *testing.T) {
	h := fileHarness(t, newMemFiles(map[string]string{"/a.txt": strings.Repeat("x", 40)}), 16)
	handle := openFile(t, h, "/a.txt", dnp3.FileModeRead, 16)

	st := fileStatus(t, h.request(app.FuncAbortFile, fileHeader(t, 4,
		objects.AppendFileCommandStatus(nil, objects.FileCommandStatus{Handle: handle}))))
	if st.Status != dnp3.FileSuccess {
		t.Errorf("abort status %s, want success", st.Status)
	}
	openFile(t, h, "/a.txt", dnp3.FileModeRead, 16)
}

// The block size the outstation settles on is the smaller of the two, and the
// master has to be told which it was.
func TestFileBlockSizeNegotiated(t *testing.T) {
	h := fileHarness(t, newMemFiles(map[string]string{"/a.txt": "aaa"}), 64)

	st := fileStatus(t, h.request(app.FuncOpenFile, openRequest(t, "/a.txt", dnp3.FileModeRead, 16)))
	if st.MaxBlockSize != 16 {
		t.Errorf("block size %d, want the master's 16", st.MaxBlockSize)
	}

	// Closing so the next open is not refused.
	h.request(app.FuncCloseFile, fileHeader(t, 4,
		objects.AppendFileCommandStatus(nil, objects.FileCommandStatus{Handle: st.Handle})))

	st = fileStatus(t, h.request(app.FuncOpenFile, openRequest(t, "/a.txt", dnp3.FileModeRead, 4096)))
	if st.MaxBlockSize != 64 {
		t.Errorf("block size %d, want the outstation's 64", st.MaxBlockSize)
	}
}

// An outstation with no handler must answer the file function codes the way a
// device that does not implement them does, rather than staying silent.
func TestFileFunctionCodesUnsupportedWithoutHandler(t *testing.T) {
	h := newHarness(t, outstation.Config{Database: smallDB()}, nil)

	for _, fc := range []app.FuncCode{
		app.FuncOpenFile, app.FuncCloseFile, app.FuncDeleteFile,
		app.FuncGetFileInfo, app.FuncAbortFile,
	} {
		frag := h.request(fc, openRequest(t, "/a.txt", dnp3.FileModeRead, 16))
		if !frag.Header.IIN.Has(app.IINNoFuncCodeSupport) {
			t.Errorf("%s: IIN %v, want NO_FUNC_CODE_SUPPORT", fc, frag.Header.IIN)
		}
	}
}

// A malformed group 70 object must be reported, not acted on.
func TestFileMalformedObjectRejected(t *testing.T) {
	h := fileHarness(t, newMemFiles(nil), 16)

	frag := h.request(app.FuncOpenFile, fileHeader(t, 3, []byte{1, 2, 3}))
	if !frag.Header.IIN.Has(app.IINParameterError) {
		t.Errorf("IIN %v, want PARAM_ERROR", frag.Header.IIN)
	}
}

// The whole read exchange, block by block, checked against the file.
func TestFileReadSequence(t *testing.T) {
	const content = "0123456789abcdefghij" // 20 octets, block size 8
	h := fileHarness(t, newMemFiles(map[string]string{"/a.txt": content}), 8)
	handle := openFile(t, h, "/a.txt", dnp3.FileModeRead, 8)

	var got []byte
	for block := uint32(0); ; block++ {
		frag := h.request(app.FuncRead, fileHeader(t, 5,
			objects.AppendFileTransport(nil, objects.FileTransport{Handle: handle, Block: block})))

		obj, err := app.FirstFreeFormatObject(findObject(t, frag, 5))
		if err != nil {
			t.Fatalf("block %d: %v", block, err)
		}
		b, err := objects.ParseFileTransport(obj)
		if err != nil {
			t.Fatalf("block %d: %v", block, err)
		}
		if b.Block != block {
			t.Fatalf("block %d arrived numbered %d", block, b.Block)
		}
		got = append(got, b.Data...)

		if b.Last {
			break
		}
		if block > 10 {
			t.Fatal("the outstation never marked a block final")
		}
	}

	if string(got) != content {
		t.Errorf("read %q, want %q", got, content)
	}
}

func findObject(t *testing.T, frag app.Fragment, variation uint8) app.ObjectHeader {
	t.Helper()
	for _, h := range frag.Objects {
		if h.Group == 70 && h.Variation == variation {
			return h
		}
	}
	t.Fatalf("the response carried no g70v%d: %v", variation, frag.Objects)
	return app.ObjectHeader{}
}

// A delete of a file that is open must be refused: the transfer would be left
// writing to something with no name.
func TestFileDeleteWhileOpenRefused(t *testing.T) {
	h := fileHarness(t, newMemFiles(map[string]string{"/a.txt": "aaa"}), 16)
	openFile(t, h, "/a.txt", dnp3.FileModeRead, 16)

	st := fileStatus(t, h.request(app.FuncDeleteFile,
		fileHeader(t, 3, objects.AppendFileCommand(nil, objects.FileCommand{
			Name: "/a.txt", Mode: dnp3.FileModeNull,
		}))))
	if st.Status != dnp3.FileLocked {
		t.Errorf("status %s, want %s", st.Status, dnp3.FileLocked)
	}
}

func TestFileStatusErrorsAreClassifiable(t *testing.T) {
	// Not a protocol procedure, but the contract the master API depends on:
	// every non-success status has to be an error a caller can classify.
	for _, st := range []dnp3.FileStatus{
		dnp3.FileNotFound, dnp3.FilePermissionDenied, dnp3.FileLocked,
		dnp3.FileTooManyOpen, dnp3.FileBlockSequence, dnp3.FileUndefined,
	} {
		err := st.Err()
		if !errors.Is(err, dnp3.ErrFileTransfer) {
			t.Errorf("%s: %v does not wrap dnp3.ErrFileTransfer", st, err)
		}
	}
}
