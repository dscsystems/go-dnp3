package outstation

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/internal/stack"
	"github.com/dscsystems/go-dnp3/objects"
)

// File transfer is the one part of an outstation that reaches outside its own
// point database. Everything else answers from memory the application already
// owns; this hands a master a path and lets it read, write or delete what is
// behind it. That is a large amount of authority to grant over a protocol with
// no authentication of its own, which is why it is off unless a handler is
// configured, and why the handler this package ships cannot be talked out of
// its directory.

// Defaults for [FileConfig].
const (
	// DefaultFileBlockSize is the block size an outstation offers when a
	// master asks for more than it wants to send at once. It is well inside a
	// 2048-octet fragment, leaving room for the headers around it.
	DefaultFileBlockSize = 1024

	// DefaultFileTimeout closes a transfer the master has stopped talking
	// about. Without it a master that goes away mid-file leaves the outstation
	// holding a handle — and, on a device that allows one transfer at a time,
	// refusing every later one.
	DefaultFileTimeout = 60 * time.Second
)

// FileConfig parameterises file transfer.
type FileConfig struct {
	// Handler serves the requests. Nil disables file transfer entirely: the
	// outstation answers the file function codes the way a device without them
	// does, with NO_FUNC_CODE_SUPPORT.
	Handler FileHandler

	// MaxBlockSize caps the block size, whatever the master asks for. Zero
	// uses [DefaultFileBlockSize].
	MaxBlockSize uint16

	// Timeout closes a transfer that has gone quiet. Zero uses
	// [DefaultFileTimeout].
	Timeout time.Duration
}

func (c *FileConfig) applyDefaults() {
	if c.MaxBlockSize == 0 {
		c.MaxBlockSize = DefaultFileBlockSize
	}
	if c.Timeout <= 0 {
		c.Timeout = DefaultFileTimeout
	}
}

// FileHandler serves the file transfer requests a master makes.
//
// Every method returns a [dnp3.FileStatus] rather than an error, because the
// status is what goes on the wire: a master distinguishes a missing file from
// a denied one, and flattening both into "failed" would leave it unable to
// tell whether retrying could ever work.
//
// The session calls these from its own goroutine, one at a time, so an
// implementation needs no locking of its own.
type FileHandler interface {
	// Info describes a file without opening it. It is what decides whether a
	// read is served as a file or as a directory listing.
	Info(name string) (dnp3.FileInfo, dnp3.FileStatus)

	// List returns the entries of a directory. The session encodes them; an
	// implementation never sees the wire format.
	List(name string) ([]dnp3.FileInfo, dnp3.FileStatus)

	// OpenRead opens a file for reading. The session closes the reader when
	// the transfer ends, times out, or the connection drops.
	OpenRead(name string) (io.ReadCloser, dnp3.FileInfo, dnp3.FileStatus)

	// OpenWrite opens a file for writing. size is the length the master says
	// it will send, which may be zero when it does not know; mode is
	// [dnp3.FileModeWrite] to replace the file or [dnp3.FileModeAppend] to add
	// to it.
	OpenWrite(name string, mode dnp3.FileMode, size uint32) (io.WriteCloser, dnp3.FileStatus)

	// Delete removes a file.
	Delete(name string) dnp3.FileStatus
}

// RejectingFileHandler refuses every request.
//
// It is what a handler that has not been wired up should be: an outstation
// that answers a file request with anything other than a refusal is claiming
// to have served it.
type RejectingFileHandler struct{}

func (RejectingFileHandler) Info(string) (dnp3.FileInfo, dnp3.FileStatus) {
	return dnp3.FileInfo{}, dnp3.FilePermissionDenied
}

func (RejectingFileHandler) List(string) ([]dnp3.FileInfo, dnp3.FileStatus) {
	return nil, dnp3.FilePermissionDenied
}

func (RejectingFileHandler) OpenRead(string) (io.ReadCloser, dnp3.FileInfo, dnp3.FileStatus) {
	return nil, dnp3.FileInfo{}, dnp3.FilePermissionDenied
}

func (RejectingFileHandler) OpenWrite(string, dnp3.FileMode, uint32) (io.WriteCloser, dnp3.FileStatus) {
	return nil, dnp3.FilePermissionDenied
}

func (RejectingFileHandler) Delete(string) dnp3.FileStatus { return dnp3.FilePermissionDenied }

// ---------- a handler over a directory ----------

// DirFileHandler serves one directory and nothing above it.
//
// It is built on [os.Root], so a master asking for "../../etc/passwd" — or for
// a symlink pointing there — is refused by the operating system rather than by
// string matching this package would have to get right. Path traversal is the
// obvious attack on file transfer, and the only defence worth having is one
// that does not depend on sanitising the name.
type DirFileHandler struct {
	root *os.Root

	// ReadOnly refuses writes and deletes. A device that exposes its
	// configuration for inspection but not for modification sets it.
	ReadOnly bool
}

// OpenDir returns a handler rooted at dir.
func OpenDir(dir string) (*DirFileHandler, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	return &DirFileHandler{root: root}, nil
}

// Close releases the directory.
func (h *DirFileHandler) Close() error { return h.root.Close() }

// resolve turns a DNP3 file name into a path within the root.
//
// DNP3 names are POSIX-shaped and usually absolute, but a root has no notion
// of one: "/config" means the config entry in this directory, not on the
// machine. So the name is cleaned as though the served directory were the
// whole filesystem, which is what a device does and what a master expects.
//
// That also settles traversal: cleaning an absolute path collapses every ".."
// against the root, so "/../../etc/passwd" resolves to "etc/passwd" inside the
// served directory rather than to anything outside it. The name never leaves
// the root, and os.Root refuses anything that tries to — a symlink pointing
// out — whatever this function returns.
func resolve(name string) string {
	clean := path.Clean("/" + strings.ReplaceAll(name, `\`, "/"))
	clean = strings.TrimPrefix(clean, "/")
	if clean == "" {
		return "."
	}
	return clean
}

// statusForError maps a filesystem error to what the master should be told.
func statusForError(err error) dnp3.FileStatus {
	switch {
	case err == nil:
		return dnp3.FileSuccess
	case errors.Is(err, fs.ErrNotExist):
		return dnp3.FileNotFound
	case errors.Is(err, fs.ErrPermission):
		return dnp3.FilePermissionDenied
	default:
		// Anything else, including a symlink os.Root refused to follow out of
		// the served directory. FATAL rather than a guess: the master learns
		// the operation will not work, which is the actionable part.
		return dnp3.FileFatal
	}
}

func (h *DirFileHandler) Info(name string) (dnp3.FileInfo, dnp3.FileStatus) {
	st, err := h.root.Stat(resolve(name))
	if err != nil {
		return dnp3.FileInfo{}, statusForError(err)
	}
	return infoFor(path.Base(name), st), dnp3.FileSuccess
}

func (h *DirFileHandler) List(name string) ([]dnp3.FileInfo, dnp3.FileStatus) {
	f, err := h.root.Open(resolve(name))
	if err != nil {
		return nil, statusForError(err)
	}
	defer f.Close()

	entries, err := f.ReadDir(-1)
	if err != nil {
		return nil, statusForError(err)
	}

	out := make([]dnp3.FileInfo, 0, len(entries))
	for _, e := range entries {
		st, err := e.Info()
		if err != nil {
			// A file that vanished between the listing and the stat. Reporting
			// the rest is better than failing the whole directory.
			continue
		}
		out = append(out, infoFor(e.Name(), st))
	}
	return out, dnp3.FileSuccess
}

func (h *DirFileHandler) OpenRead(name string) (io.ReadCloser, dnp3.FileInfo, dnp3.FileStatus) {
	p := resolve(name)
	st, err := h.root.Stat(p)
	if err != nil {
		return nil, dnp3.FileInfo{}, statusForError(err)
	}
	if st.IsDir() {
		// A directory is read as a listing, which the session builds. Opening
		// one as a stream would hand back the raw directory octets.
		return nil, dnp3.FileInfo{}, dnp3.FileInvalidMode
	}

	f, err := h.root.Open(p)
	if err != nil {
		return nil, dnp3.FileInfo{}, statusForError(err)
	}
	return f, infoFor(path.Base(name), st), dnp3.FileSuccess
}

func (h *DirFileHandler) OpenWrite(name string, mode dnp3.FileMode, _ uint32) (io.WriteCloser, dnp3.FileStatus) {
	if h.ReadOnly {
		return nil, dnp3.FilePermissionDenied
	}
	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if mode == dnp3.FileModeAppend {
		flags = os.O_WRONLY | os.O_CREATE | os.O_APPEND
	}
	f, err := h.root.OpenFile(resolve(name), flags, 0o644)
	if err != nil {
		return nil, statusForError(err)
	}
	return f, dnp3.FileSuccess
}

func (h *DirFileHandler) Delete(name string) dnp3.FileStatus {
	if h.ReadOnly {
		return dnp3.FilePermissionDenied
	}
	return statusForError(h.root.Remove(resolve(name)))
}

// infoFor converts a filesystem entry to what the protocol reports.
//
// The modification time stands in for the creation time: DNP3 has a field for
// the second and POSIX keeps only the first, and a master comparing a file it
// wrote against what came back cares about when the content last changed.
func infoFor(name string, st fs.FileInfo) dnp3.FileInfo {
	info := dnp3.FileInfo{
		Name:        name,
		Type:        dnp3.FileTypeSimple,
		Created:     st.ModTime(),
		Permissions: permissionsFor(st.Mode()),
	}
	if st.IsDir() {
		info.Type = dnp3.FileTypeDirectory
	}
	if size := st.Size(); size > 0 {
		// The protocol's size field is 32 bits. A file larger than that cannot
		// be described, and reporting a truncated length would have the master
		// stop reading early and believe it had the whole thing.
		info.Size = uint32(min(size, int64(^uint32(0))))
	}
	return info
}

// permissionsFor maps the POSIX bits onto the protocol's, which are the same
// nine bits in the same order.
func permissionsFor(mode fs.FileMode) dnp3.FilePermissions {
	return dnp3.FilePermissions(mode.Perm() & 0o777)
}

// ---------- the transfer in flight ----------

// transfer is the one file a session has open.
//
// One at a time is deliberate. A device with a handle table has to expire
// entries, refuse the master that opened too many, and answer for handles it
// has forgotten; a device with a single slot answers "too many files open" and
// is done. Masters open one file at a time in practice.
type transfer struct {
	handle uint32
	name   string
	mode   dnp3.FileMode

	// r and br read a file out. br is buffered so the session can look one
	// octet ahead, which is the only way to know that a full-sized block is
	// the last one.
	r  io.ReadCloser
	br *bufio.Reader
	w  io.WriteCloser

	// block is the next block number expected in, or to be sent out.
	block     uint32
	blockSize uint16
	size      uint32

	// deadline is when an idle transfer is abandoned.
	deadline time.Time
	// done is set once the last block has passed, so a master that keeps
	// asking is told the file is finished rather than being served past its
	// end.
	done bool
}

// close releases whichever end of the transfer is open.
func (t *transfer) close() error {
	switch {
	case t.r != nil:
		return t.r.Close()
	case t.w != nil:
		return t.w.Close()
	}
	return nil
}

// ---------- request handling ----------

// fileObject returns the group 70 object header a request carries, if any.
func fileObject(frag app.Fragment) (app.ObjectHeader, bool) {
	for _, h := range frag.Objects {
		if h.Group == 70 {
			return h, true
		}
	}
	return app.ObjectHeader{}, false
}

// fileEnabled reports whether file transfer is configured. When it is not, the
// outstation answers the file function codes the way a device that does not
// implement them does.
func (s *Session) fileEnabled() bool { return s.cfg.Files.Handler != nil }

// unsupportedFile answers a file request on an outstation without a handler.
func (s *Session) unsupportedFile(w io.Writer, r stack.Received, req app.Header) error {
	s.bump(func(st *Stats) { st.UnknownFunction++ })
	s.iin = s.iin.Set(app.IINNoFuncCodeSupport)
	if r.Broadcast {
		return nil
	}
	return s.respond(w, r, req, nil)
}

// respondFile answers with a single group 70 object.
func (s *Session) respondFile(w io.Writer, r stack.Received, req app.Header, variation uint8, obj []byte) error {
	h, err := app.FreeFormat(70, variation, obj)
	if err != nil {
		s.iin = s.iin.Set(app.IINParameterError)
		return s.respond(w, r, req, nil)
	}
	return s.respond(w, r, req, app.AppendObjectHeader(nil, h))
}

// commandStatus answers an open, close, delete or abort.
func (s *Session) commandStatus(w io.Writer, r stack.Received, req app.Header, st objects.FileCommandStatus) error {
	s.bump(func(stats *Stats) {
		if st.Status != dnp3.FileSuccess {
			stats.FileErrors++
		}
	})
	return s.respondFile(w, r, req, 4, objects.AppendFileCommandStatus(nil, st))
}

// transportStatus answers a written block.
func (s *Session) transportStatus(w io.Writer, r stack.Received, req app.Header, st objects.FileTransportStatus) error {
	s.bump(func(stats *Stats) {
		if st.Status != dnp3.FileSuccess {
			stats.FileErrors++
		}
	})
	return s.respondFile(w, r, req, 6, objects.AppendFileTransportStatus(nil, st))
}

// onOpenFile opens a file and hands back a handle.
func (s *Session) onOpenFile(w io.Writer, r stack.Received, frag app.Fragment) error {
	if !s.fileEnabled() {
		return s.unsupportedFile(w, r, frag.Header)
	}

	cmd, err := s.parseFileCommand(frag)
	if err != nil {
		s.log.Warn("malformed file open", "err", err)
		s.iin = s.iin.Set(app.IINParameterError)
		return s.respond(w, r, frag.Header, nil)
	}
	reply := objects.FileCommandStatus{RequestID: cmd.RequestID}

	if s.file != nil {
		// One transfer at a time. Answering with the handle of the transfer
		// already running would have two masters writing the same file.
		s.log.Warn("file open refused; another transfer is in flight",
			"requested", cmd.Name, "open", s.file.name)
		reply.Status = dnp3.FileTooManyOpen
		return s.commandStatus(w, r, frag.Header, reply)
	}

	t, status := s.openTransfer(cmd)
	if status != dnp3.FileSuccess {
		reply.Status = status
		return s.commandStatus(w, r, frag.Header, reply)
	}

	s.handleSeq++
	t.handle = s.handleSeq
	t.deadline = s.appl.Now().Add(s.cfg.Files.Timeout)
	s.file = t

	s.bump(func(st *Stats) { st.FilesOpened++ })
	s.log.Info("file opened",
		"name", cmd.Name, "mode", cmd.Mode, "handle", t.handle,
		"size", t.size, "block_size", t.blockSize)

	reply.Handle = t.handle
	reply.Size = t.size
	reply.MaxBlockSize = t.blockSize
	return s.commandStatus(w, r, frag.Header, reply)
}

// openTransfer asks the handler for the file and prepares the transfer.
func (s *Session) openTransfer(cmd objects.FileCommand) (*transfer, dnp3.FileStatus) {
	h := s.cfg.Files.Handler
	t := &transfer{
		name:      cmd.Name,
		mode:      cmd.Mode,
		blockSize: s.negotiateBlockSize(cmd.MaxBlockSize),
	}

	switch cmd.Mode {
	case dnp3.FileModeRead:
		info, status := h.Info(cmd.Name)
		if status != dnp3.FileSuccess {
			return nil, status
		}

		if info.IsDir() {
			// A directory is read as a file whose contents are its entries.
			// Building them here rather than in the handler keeps the wire
			// format out of every implementation.
			content, status := s.directoryContents(cmd)
			if status != dnp3.FileSuccess {
				return nil, status
			}
			t.r = io.NopCloser(bytes.NewReader(content))
			t.size = uint32(len(content))
		} else {
			rc, _, status := h.OpenRead(cmd.Name)
			if status != dnp3.FileSuccess {
				return nil, status
			}
			t.r = rc
			t.size = info.Size
		}
		t.br = bufio.NewReaderSize(t.r, int(t.blockSize)+1)

	case dnp3.FileModeWrite, dnp3.FileModeAppend:
		wc, status := h.OpenWrite(cmd.Name, cmd.Mode, cmd.Size)
		if status != dnp3.FileSuccess {
			return nil, status
		}
		t.w = wc
		t.size = cmd.Size

	default:
		return nil, dnp3.FileInvalidMode
	}
	return t, dnp3.FileSuccess
}

// directoryContents encodes a directory listing as the file a master reads.
func (s *Session) directoryContents(cmd objects.FileCommand) ([]byte, dnp3.FileStatus) {
	entries, status := s.cfg.Files.Handler.List(cmd.Name)
	if status != dnp3.FileSuccess {
		return nil, status
	}
	var content []byte
	for _, e := range entries {
		content = objects.AppendFileDescriptor(content, objects.DescriptorFor(e, cmd.RequestID))
	}
	return content, dnp3.FileSuccess
}

// negotiateBlockSize settles on a block size both ends can carry.
//
// The master states the largest it will accept and the outstation the largest
// it will send; the smaller wins. The fragment cap is the third constraint and
// the one a device forgets: a block that does not fit in a response fragment
// cannot be sent at all.
func (s *Session) negotiateBlockSize(requested uint16) uint16 {
	size := s.cfg.Files.MaxBlockSize
	if requested > 0 && requested < size {
		size = requested
	}
	// Room for the application header, the object header and the transport
	// object's own fixed part.
	if room := s.cfg.MaxTxFragment - 32; room > 0 && int(size) > room {
		size = uint16(room)
	}
	return size
}

// parseFileCommand pulls a g70v3 out of a request.
func (s *Session) parseFileCommand(frag app.Fragment) (objects.FileCommand, error) {
	h, ok := fileObject(frag)
	if !ok {
		return objects.FileCommand{}, errors.New("outstation: the request carries no group 70 object")
	}
	obj, err := app.FirstFreeFormatObject(h)
	if err != nil {
		return objects.FileCommand{}, err
	}
	return objects.ParseFileCommand(obj)
}

// onCloseFile ends a transfer.
func (s *Session) onCloseFile(w io.Writer, r stack.Received, frag app.Fragment) error {
	if !s.fileEnabled() {
		return s.unsupportedFile(w, r, frag.Header)
	}

	h, ok := fileObject(frag)
	if !ok {
		s.iin = s.iin.Set(app.IINParameterError)
		return s.respond(w, r, frag.Header, nil)
	}
	obj, err := app.FirstFreeFormatObject(h)
	if err != nil {
		s.iin = s.iin.Set(app.IINParameterError)
		return s.respond(w, r, frag.Header, nil)
	}
	req, err := objects.ParseFileCommandStatus(obj)
	if err != nil {
		s.log.Warn("malformed file close", "err", err)
		s.iin = s.iin.Set(app.IINParameterError)
		return s.respond(w, r, frag.Header, nil)
	}

	reply := objects.FileCommandStatus{Handle: req.Handle, RequestID: req.RequestID}
	if s.file == nil || s.file.handle != req.Handle {
		reply.Status = dnp3.FileInvalidHandle
		return s.commandStatus(w, r, frag.Header, reply)
	}

	name := s.file.name
	if err := s.closeFile(); err != nil {
		s.log.Warn("closing a transferred file failed", "name", name, "err", err)
		// The master needs to know: a write whose close failed has not landed,
		// whatever the individual blocks reported.
		reply.Status = dnp3.FileFatal
		return s.commandStatus(w, r, frag.Header, reply)
	}
	s.log.Info("file closed", "name", name, "handle", req.Handle)
	return s.commandStatus(w, r, frag.Header, reply)
}

// onDeleteFile removes a file.
func (s *Session) onDeleteFile(w io.Writer, r stack.Received, frag app.Fragment) error {
	if !s.fileEnabled() {
		return s.unsupportedFile(w, r, frag.Header)
	}

	cmd, err := s.parseFileCommand(frag)
	if err != nil {
		s.log.Warn("malformed file delete", "err", err)
		s.iin = s.iin.Set(app.IINParameterError)
		return s.respond(w, r, frag.Header, nil)
	}

	reply := objects.FileCommandStatus{RequestID: cmd.RequestID}
	if s.file != nil && s.file.name == cmd.Name {
		// Deleting the file being transferred would leave the transfer writing
		// to something with no name.
		reply.Status = dnp3.FileLocked
		return s.commandStatus(w, r, frag.Header, reply)
	}

	reply.Status = s.cfg.Files.Handler.Delete(cmd.Name)
	if reply.Status == dnp3.FileSuccess {
		s.log.Info("file deleted", "name", cmd.Name)
	}
	return s.commandStatus(w, r, frag.Header, reply)
}

// onAbortFile abandons a transfer without finishing it.
//
// Abort differs from close in what it means for a write: a closed file is
// complete and an aborted one is not. Both release the handle, which is what
// the master is really asking for.
func (s *Session) onAbortFile(w io.Writer, r stack.Received, frag app.Fragment) error {
	if !s.fileEnabled() {
		return s.unsupportedFile(w, r, frag.Header)
	}

	reply := objects.FileCommandStatus{}
	if h, ok := fileObject(frag); ok {
		if obj, err := app.FirstFreeFormatObject(h); err == nil {
			// An abort is addressed by handle, in the same object a close
			// uses. A master that sends the open command instead is answered
			// on the transfer in flight.
			if st, err := objects.ParseFileCommandStatus(obj); err == nil {
				reply.Handle = st.Handle
				reply.RequestID = st.RequestID
			}
		}
	}

	if s.file == nil {
		reply.Status = dnp3.FileNotOpen
		return s.commandStatus(w, r, frag.Header, reply)
	}
	if reply.Handle != 0 && reply.Handle != s.file.handle {
		reply.Status = dnp3.FileInvalidHandle
		return s.commandStatus(w, r, frag.Header, reply)
	}

	name := s.file.name
	_ = s.closeFile()
	s.bump(func(st *Stats) { st.FilesAborted++ })
	s.log.Info("file transfer aborted", "name", name)
	return s.commandStatus(w, r, frag.Header, reply)
}

// onGetFileInfo describes a file without transferring it.
func (s *Session) onGetFileInfo(w io.Writer, r stack.Received, frag app.Fragment) error {
	if !s.fileEnabled() {
		return s.unsupportedFile(w, r, frag.Header)
	}

	h, ok := fileObject(frag)
	if !ok {
		s.iin = s.iin.Set(app.IINParameterError)
		return s.respond(w, r, frag.Header, nil)
	}
	obj, err := app.FirstFreeFormatObject(h)
	if err != nil {
		s.iin = s.iin.Set(app.IINParameterError)
		return s.respond(w, r, frag.Header, nil)
	}

	// The request names the file in the same descriptor object the answer
	// comes back in, with everything but the name left empty.
	req, err := objects.ParseFileDescriptor(obj)
	if err != nil {
		s.log.Warn("malformed file info request", "err", err)
		s.iin = s.iin.Set(app.IINParameterError)
		return s.respond(w, r, frag.Header, nil)
	}

	info, status := s.cfg.Files.Handler.Info(req.Name)
	if status != dnp3.FileSuccess {
		s.bump(func(st *Stats) { st.FileErrors++ })
		return s.commandStatus(w, r, frag.Header, objects.FileCommandStatus{
			RequestID: req.RequestID,
			Status:    status,
		})
	}

	// The name reported is the handler's, not the path that was asked for: a
	// descriptor names the file the way a directory listing does, and the
	// master already knows what it requested.
	return s.respondFile(w, r, frag.Header, 7,
		objects.AppendFileDescriptor(nil, objects.DescriptorFor(info, req.RequestID)))
}

// onFileRead serves the next block of a file.
func (s *Session) onFileRead(w io.Writer, r stack.Received, frag app.Fragment, h app.ObjectHeader) error {
	if !s.fileEnabled() {
		return s.unsupportedFile(w, r, frag.Header)
	}

	obj, err := app.FirstFreeFormatObject(h)
	if err != nil {
		s.iin = s.iin.Set(app.IINParameterError)
		return s.respond(w, r, frag.Header, nil)
	}
	req, err := objects.ParseFileTransport(obj)
	if err != nil {
		s.log.Warn("malformed file read", "err", err)
		s.iin = s.iin.Set(app.IINParameterError)
		return s.respond(w, r, frag.Header, nil)
	}

	fail := func(status dnp3.FileStatus) error {
		return s.transportStatus(w, r, frag.Header, objects.FileTransportStatus{
			Handle: req.Handle, Block: req.Block, Status: status,
		})
	}

	t := s.file
	switch {
	case t == nil:
		return fail(dnp3.FileNotOpen)
	case t.handle != req.Handle:
		return fail(dnp3.FileInvalidHandle)
	case t.mode != dnp3.FileModeRead:
		return fail(dnp3.FileInvalidMode)
	case t.done:
		// The last block has already gone. Serving another would be inventing
		// a file end that the master would then append to what it has.
		return fail(dnp3.FileNotOpen)
	case req.Block != t.block:
		// The stream cannot rewind, so a master asking for anything but the
		// next block is asking for something that cannot be given.
		s.log.Warn("file read out of sequence", "want", t.block, "got", req.Block)
		return fail(dnp3.FileBlockSequence)
	}

	t.deadline = s.appl.Now().Add(s.cfg.Files.Timeout)

	data, last, err := readBlock(t)
	if err != nil {
		s.log.Warn("reading a file block failed", "name", t.name, "err", err)
		_ = s.closeFile()
		return fail(dnp3.FileFatal)
	}

	block := objects.FileTransport{
		Handle: t.handle,
		Block:  t.block,
		Last:   last,
		Data:   data,
	}
	t.block++
	t.done = last

	s.bump(func(st *Stats) { st.FileBlocksSent++ })
	return s.respondFile(w, r, frag.Header, 5, objects.AppendFileTransport(nil, block))
}

// readBlock takes the next block off a transfer, reporting whether it is the
// last.
//
// The look-ahead is what makes "last" honest. A block that comes back exactly
// full is indistinguishable from the end of the file until something says
// otherwise, and a master that never sees the last-block flag waits for a
// block that is not coming.
func readBlock(t *transfer) ([]byte, bool, error) {
	buf := make([]byte, t.blockSize)

	n, err := io.ReadFull(t.br, buf)
	switch {
	case err == nil:
		// A full block. Peek to find out whether anything follows it.
		if _, perr := t.br.Peek(1); perr != nil {
			if errors.Is(perr, io.EOF) {
				return buf, true, nil
			}
			return nil, false, perr
		}
		return buf, false, nil

	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		// A short block, or none at all: an empty file still owes the master
		// one last block, or it would never learn the transfer had finished.
		return buf[:n], true, nil

	default:
		return nil, false, err
	}
}

// onFileWrite accepts one block of a file being written.
func (s *Session) onFileWrite(w io.Writer, r stack.Received, frag app.Fragment, h app.ObjectHeader) error {
	if !s.fileEnabled() {
		return s.unsupportedFile(w, r, frag.Header)
	}

	obj, err := app.FirstFreeFormatObject(h)
	if err != nil {
		s.iin = s.iin.Set(app.IINParameterError)
		return s.respond(w, r, frag.Header, nil)
	}
	block, err := objects.ParseFileTransport(obj)
	if err != nil {
		s.log.Warn("malformed file write", "err", err)
		s.iin = s.iin.Set(app.IINParameterError)
		return s.respond(w, r, frag.Header, nil)
	}

	reply := objects.FileTransportStatus{
		Handle: block.Handle,
		Block:  block.Block,
		Last:   block.Last,
	}

	t := s.file
	switch {
	case t == nil:
		reply.Status = dnp3.FileNotOpen
	case t.handle != block.Handle:
		reply.Status = dnp3.FileInvalidHandle
	case t.mode != dnp3.FileModeWrite && t.mode != dnp3.FileModeAppend:
		reply.Status = dnp3.FileInvalidMode
	case block.Block != t.block:
		// Blocks must arrive in order: the writer appends, so a gap would be
		// silently filled with whatever came next.
		s.log.Warn("file write out of sequence", "want", t.block, "got", block.Block)
		reply.Status = dnp3.FileBlockSequence
	case len(block.Data) > int(t.blockSize):
		reply.Status = dnp3.FileWriteBlockSize
	default:
		t.deadline = s.appl.Now().Add(s.cfg.Files.Timeout)
		if _, err := t.w.Write(block.Data); err != nil {
			s.log.Warn("writing a file block failed", "name", t.name, "err", err)
			_ = s.closeFile()
			reply.Status = dnp3.FileFatal
			break
		}
		t.block++
		t.done = block.Last
		s.bump(func(st *Stats) { st.FileBlocksReceived++ })
	}

	return s.transportStatus(w, r, frag.Header, reply)
}

// closeFile ends the transfer in flight.
func (s *Session) closeFile() error {
	t := s.file
	if t == nil {
		return nil
	}
	s.file = nil
	return t.close()
}

// checkFileTimeout abandons a transfer the master has stopped talking about.
func (s *Session) checkFileTimeout(now time.Time) {
	t := s.file
	if t == nil || now.Before(t.deadline) {
		return
	}
	s.log.Warn("file transfer timed out; the handle is being released",
		"name", t.name, "handle", t.handle)
	_ = s.closeFile()
	s.bump(func(st *Stats) { st.FileTimeouts++ })
}
