package master

import (
	"fmt"
	"io"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/objects"
)

// A file transfer is a conversation, not a request: open, then a block at a
// time, then close. Every step depends on what the last one said — the handle
// comes back from the open, the block size may be smaller than what was asked
// for, and only the outstation knows which block is the last.
//
// The steps are chained rather than queued, the way select-and-operate is. A
// poll landing between two blocks would not corrupt the file, but it would put
// the outstation's inactivity timer at risk on a slow link, and the outstation
// is holding a handle for the duration.
//
// That is also the cost: a transfer owns the session until it finishes, so
// polls queued behind a large file wait for it. On a serial link a firmware
// image is minutes of the line, whoever asks for it.

// transfer is the state one file exchange carries between its steps.
type transfer struct {
	name      string
	requestID uint16

	// handle, size and blockSize are what the open told us. A zero handle
	// means nothing was opened, so nothing needs closing.
	handle    uint32
	size      uint32
	blockSize uint16

	// block is the next block number to ask for or to send.
	block uint32
	// last is set once the block marked final has been handled.
	last bool

	dst io.Writer
	src io.Reader

	// transferred counts the octets moved, which is what a caller sees when
	// the file turns out to be shorter than the outstation said.
	transferred uint64

	// err is the protocol-level outcome. The task machinery reports transport
	// failures — a timeout, a dropped link — through its own error; a file the
	// outstation refused is a successful exchange with a status code in it,
	// and this is where that lands.
	err error

	// closed records that the close step has run, so a caller does not send a
	// second one after a failure.
	closed bool
}

// fail records the first protocol-level failure and stops the chain.
func (t *transfer) fail(err error) {
	if t.err == nil {
		t.err = err
	}
}

// fileObject returns the group 70 object a response carries.
func fileObject(frag app.Fragment, variation uint8) ([]byte, bool) {
	for _, h := range frag.Objects {
		if h.Group != 70 || h.Variation != variation {
			continue
		}
		obj, err := app.FirstFreeFormatObject(h)
		if err != nil {
			return nil, false
		}
		return obj, true
	}
	return nil, false
}

// statusObject pulls the g70v6 an outstation reports a failed block with.
func statusObject(frag app.Fragment) (objects.FileTransportStatus, bool) {
	obj, ok := fileObject(frag, 6)
	if !ok {
		return objects.FileTransportStatus{}, false
	}
	st, err := objects.ParseFileTransportStatus(obj)
	if err != nil {
		return objects.FileTransportStatus{}, false
	}
	return st, true
}

// checkSupported turns "I do not implement this" into an error the caller can
// classify, rather than leaving it as a response with nothing in it.
func (t *transfer) checkSupported(iin app.IIN, step string) {
	if iin.Has(app.IINNoFuncCodeSupport) {
		t.fail(fmt.Errorf("master: file %s: %w", step, dnp3.ErrNotSupported))
	}
}

// newFileOpenTask opens a file and records the handle the outstation issues.
func newFileOpenTask(t *transfer, mode dnp3.FileMode, blockSize uint16, next func() *task) *task {
	return &task{
		name:     "file-open",
		funcCode: app.FuncOpenFile,
		priority: priorityCommand,
		build: func(b *app.Builder) error {
			h, err := app.FreeFormat(70, 3, objects.AppendFileCommand(nil, objects.FileCommand{
				Name:         t.name,
				Mode:         mode,
				Size:         t.size,
				MaxBlockSize: blockSize,
				RequestID:    t.requestID,
			}))
			if err != nil {
				return err
			}
			return b.AddObject(h)
		},
		onFragment: func(frag app.Fragment) {
			obj, ok := fileObject(frag, 4)
			if !ok {
				return
			}
			st, err := objects.ParseFileCommandStatus(obj)
			if err != nil {
				t.fail(fmt.Errorf("master: file open: %w", err))
				return
			}
			if st.Status != dnp3.FileSuccess {
				t.fail(fmt.Errorf("master: opening %q: %w%s",
					t.name, st.Status.Err(), statusText(st.Text)))
				return
			}

			t.handle = st.Handle
			t.size = st.Size
			// The outstation may come back with a smaller block than was
			// asked for, and its answer is the one that counts.
			t.blockSize = blockSize
			if st.MaxBlockSize > 0 && st.MaxBlockSize < blockSize {
				t.blockSize = st.MaxBlockSize
			}
		},
		onDone: func(iin app.IIN) {
			t.checkSupported(iin, "open")
			if t.err == nil && t.handle == 0 {
				t.fail(fmt.Errorf("master: opening %q: the outstation returned no handle", t.name))
			}
		},
		next: func() *task {
			if t.err != nil {
				return nil
			}
			return next()
		},
	}
}

// newFileReadTask asks for one block and writes it out.
func newFileReadTask(t *transfer, next func() *task) *task {
	return &task{
		name:     "file-read",
		funcCode: app.FuncRead,
		priority: priorityCommand,
		build: func(b *app.Builder) error {
			h, err := app.FreeFormat(70, 5, objects.AppendFileTransport(nil, objects.FileTransport{
				Handle: t.handle,
				Block:  t.block,
			}))
			if err != nil {
				return err
			}
			return b.AddObject(h)
		},
		onFragment: func(frag app.Fragment) {
			if st, ok := statusObject(frag); ok {
				t.fail(fmt.Errorf("master: reading %q block %d: %w%s",
					t.name, t.block, st.Status.Err(), statusText(st.Text)))
				return
			}

			obj, ok := fileObject(frag, 5)
			if !ok {
				t.fail(fmt.Errorf("master: reading %q block %d: the response carried no file data",
					t.name, t.block))
				return
			}
			block, err := objects.ParseFileTransport(obj)
			if err != nil {
				t.fail(fmt.Errorf("master: reading %q: %w", t.name, err))
				return
			}
			if block.Block != t.block {
				// Writing it anyway would put the file together in the wrong
				// order, which no checksum here would catch.
				t.fail(fmt.Errorf("master: reading %q: block %d arrived where %d was expected",
					t.name, block.Block, t.block))
				return
			}

			if len(block.Data) > 0 {
				if _, err := t.dst.Write(block.Data); err != nil {
					t.fail(fmt.Errorf("master: writing %q out: %w", t.name, err))
					return
				}
				t.transferred += uint64(len(block.Data))
			}
			t.block++
			t.last = block.Last
		},
		onDone: func(iin app.IIN) { t.checkSupported(iin, "read") },
		next: func() *task {
			if t.err != nil {
				return nil
			}
			return next()
		},
	}
}

// newFileWriteTask sends one block.
func newFileWriteTask(t *transfer, data []byte, last bool, next func() *task) *task {
	return &task{
		name:     "file-write",
		funcCode: app.FuncWrite,
		priority: priorityCommand,
		build: func(b *app.Builder) error {
			h, err := app.FreeFormat(70, 5, objects.AppendFileTransport(nil, objects.FileTransport{
				Handle: t.handle,
				Block:  t.block,
				Last:   last,
				Data:   data,
			}))
			if err != nil {
				return err
			}
			return b.AddObject(h)
		},
		onFragment: func(frag app.Fragment) {
			st, ok := statusObject(frag)
			if !ok {
				// Some outstations acknowledge a block with an empty response.
				// Taking silence for success is the only reading that lets
				// them work, and a real failure still surfaces at the close.
				return
			}
			if st.Status != dnp3.FileSuccess {
				t.fail(fmt.Errorf("master: writing %q block %d: %w%s",
					t.name, t.block, st.Status.Err(), statusText(st.Text)))
			}
		},
		onDone: func(iin app.IIN) {
			t.checkSupported(iin, "write")
			if t.err == nil {
				t.block++
				t.transferred += uint64(len(data))
				t.last = last
			}
		},
		next: func() *task {
			if t.err != nil {
				return nil
			}
			return next()
		},
	}
}

// newFileCloseTask ends a transfer.
func newFileCloseTask(t *transfer) *task {
	return &task{
		name:     "file-close",
		funcCode: app.FuncCloseFile,
		priority: priorityCommand,
		build: func(b *app.Builder) error {
			h, err := app.FreeFormat(70, 4, objects.AppendFileCommandStatus(nil, objects.FileCommandStatus{
				Handle:    t.handle,
				RequestID: t.requestID,
			}))
			if err != nil {
				return err
			}
			return b.AddObject(h)
		},
		onFragment: func(frag app.Fragment) {
			obj, ok := fileObject(frag, 4)
			if !ok {
				return
			}
			st, err := objects.ParseFileCommandStatus(obj)
			if err != nil {
				t.fail(fmt.Errorf("master: closing %q: %w", t.name, err))
				return
			}
			if st.Status != dnp3.FileSuccess {
				// A close that fails on a write means the file did not land,
				// however well the blocks went.
				t.fail(fmt.Errorf("master: closing %q: %w%s",
					t.name, st.Status.Err(), statusText(st.Text)))
			}
		},
		onDone: func(app.IIN) { t.closed = true },
	}
}

// newFileDeleteTask removes a file.
func newFileDeleteTask(t *transfer) *task {
	return &task{
		name:     "file-delete",
		funcCode: app.FuncDeleteFile,
		priority: priorityCommand,
		build: func(b *app.Builder) error {
			h, err := app.FreeFormat(70, 3, objects.AppendFileCommand(nil, objects.FileCommand{
				Name:      t.name,
				Mode:      dnp3.FileModeNull,
				RequestID: t.requestID,
			}))
			if err != nil {
				return err
			}
			return b.AddObject(h)
		},
		onFragment: func(frag app.Fragment) {
			obj, ok := fileObject(frag, 4)
			if !ok {
				return
			}
			st, err := objects.ParseFileCommandStatus(obj)
			if err != nil {
				t.fail(fmt.Errorf("master: deleting %q: %w", t.name, err))
				return
			}
			if st.Status != dnp3.FileSuccess {
				t.fail(fmt.Errorf("master: deleting %q: %w%s",
					t.name, st.Status.Err(), statusText(st.Text)))
			}
		},
		onDone: func(iin app.IIN) { t.checkSupported(iin, "delete") },
	}
}

// newFileInfoTask describes a file without transferring it.
func newFileInfoTask(t *transfer, info *dnp3.FileInfo) *task {
	return &task{
		name:     "file-info",
		funcCode: app.FuncGetFileInfo,
		priority: priorityCommand,
		build: func(b *app.Builder) error {
			// The request is the answer's own object with only the name
			// filled in, which is how the standard asks the question.
			h, err := app.FreeFormat(70, 7, objects.AppendFileDescriptor(nil, objects.FileDescriptor{
				Name:      t.name,
				RequestID: t.requestID,
			}))
			if err != nil {
				return err
			}
			return b.AddObject(h)
		},
		onFragment: func(frag app.Fragment) {
			if obj, ok := fileObject(frag, 4); ok {
				// The outstation answered with a status instead of a
				// descriptor, which is how it says the file is not there.
				if st, err := objects.ParseFileCommandStatus(obj); err == nil {
					t.fail(fmt.Errorf("master: file info for %q: %w%s",
						t.name, st.Status.Err(), statusText(st.Text)))
					return
				}
			}

			obj, ok := fileObject(frag, 7)
			if !ok {
				t.fail(fmt.Errorf("master: file info for %q: the response carried no descriptor", t.name))
				return
			}
			d, err := objects.ParseFileDescriptor(obj)
			if err != nil {
				t.fail(fmt.Errorf("master: file info for %q: %w", t.name, err))
				return
			}
			*info = d.Info()
		},
		onDone: func(iin app.IIN) { t.checkSupported(iin, "info") },
	}
}

// statusText renders an outstation's explanation, when it gave one.
func statusText(s string) string {
	if s == "" {
		return ""
	}
	return " (" + s + ")"
}
