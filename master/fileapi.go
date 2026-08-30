package master

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/objects"
)

// The file methods below follow the same rule as the rest of the request API:
// safe to call from any goroutine, and each one waits for the exchange to
// finish. A transfer is many requests rather than one, so it holds the session
// for its whole duration — see the note in file.go.

// MaxFileSize caps what [Session.ReadFileBytes] and [Session.ReadDirectory]
// will accumulate in memory, so a device that reports an implausible size — or
// never stops sending blocks — cannot exhaust the master.
//
// Use [Session.ReadFile] with a file on disk for anything larger.
const MaxFileSize = 16 << 20

// nextRequestID issues the identifier that ties a response to its request.
//
// It wraps at sixteen bits, as the field does. Reuse is harmless: it
// disambiguates two exchanges in flight, and a session has one.
func (s *Session) nextRequestID() uint16 {
	return uint16(s.fileSeq.Add(1))
}

// ReadFile reads a file from the outstation into dst.
//
// The transfer is open, read, close. A failure part way through still closes
// the file: an outstation left holding a handle refuses the next transfer
// until its own timeout expires, which on a device that allows one at a time
// means the master has locked itself out.
//
// The number of octets written to dst is returned even when the transfer
// fails, so a caller can tell a file that arrived short from one that never
// started.
func (s *Session) ReadFile(ctx context.Context, name string, dst io.Writer) (int64, error) {
	if name == "" {
		return 0, fmt.Errorf("master: %w: empty file name", dnp3.ErrBadConfig)
	}
	if dst == nil {
		return 0, fmt.Errorf("master: %w: nil destination", dnp3.ErrBadConfig)
	}

	t := &transfer{name: name, requestID: s.nextRequestID(), dst: dst}

	// The read step chains to itself until the outstation marks a block final,
	// then to the close. Building the next task in the closure is what lets a
	// transfer of unknown length be expressed without knowing how many steps
	// it will take.
	var readStep func() *task
	readStep = func() *task {
		return newFileReadTask(t, func() *task {
			if t.last {
				return newFileCloseTask(t)
			}
			return readStep()
		})
	}

	err := s.runTransfer(ctx, t, newFileOpenTask(t, dnp3.FileModeRead, s.fileBlockSize(), readStep))
	return int64(t.transferred), err
}

// ReadFileBytes reads a file into memory, up to [MaxFileSize].
func (s *Session) ReadFileBytes(ctx context.Context, name string) ([]byte, error) {
	var buf bytes.Buffer
	if _, err := s.ReadFile(ctx, name, &limitedWriter{w: &buf, max: MaxFileSize, name: name}); err != nil {
		return buf.Bytes(), err
	}
	return buf.Bytes(), nil
}

// WriteFile writes size octets read from src to the named file on the
// outstation.
//
// The size has to be declared up front because the open command carries it:
// an outstation deciding whether it has room for a firmware image asks before
// the first block, not after the last.
func (s *Session) WriteFile(ctx context.Context, name string, src io.Reader, size uint32) error {
	if name == "" {
		return fmt.Errorf("master: %w: empty file name", dnp3.ErrBadConfig)
	}
	if src == nil {
		return fmt.Errorf("master: %w: nil source", dnp3.ErrBadConfig)
	}

	t := &transfer{name: name, requestID: s.nextRequestID(), src: src, size: size}

	// Each block is read from src as its task is built, so a large file is
	// never held in memory whole.
	var writeStep func() *task
	writeStep = func() *task {
		buf := make([]byte, t.blockSize)
		n, err := io.ReadFull(src, buf)
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			t.fail(fmt.Errorf("master: reading %q to send: %w", name, err))
			return nil
		}

		// Short means the source is exhausted, so this is the final block. An
		// empty file still sends one: the last-block flag is what tells the
		// outstation the transfer is complete.
		last := n < len(buf)
		return newFileWriteTask(t, buf[:n], last, func() *task {
			if t.last {
				return newFileCloseTask(t)
			}
			return writeStep()
		})
	}

	return s.runTransfer(ctx, t, newFileOpenTask(t, dnp3.FileModeWrite, s.fileBlockSize(), writeStep))
}

// WriteFileBytes writes data to the named file on the outstation.
func (s *Session) WriteFileBytes(ctx context.Context, name string, data []byte) error {
	return s.WriteFile(ctx, name, bytes.NewReader(data), uint32(len(data)))
}

// ReadDirectory lists a directory on the outstation.
//
// A directory is read exactly as a file is; what comes back is a run of file
// descriptors rather than arbitrary octets. Pass the path the outstation knows
// its root by, which is conventionally "/".
func (s *Session) ReadDirectory(ctx context.Context, name string) ([]dnp3.FileInfo, error) {
	content, err := s.ReadFileBytes(ctx, name)
	if err != nil {
		return nil, err
	}
	entries, err := objects.ParseDirectory(content)
	if err != nil {
		return entries, fmt.Errorf("master: listing %q: %w", name, err)
	}
	return entries, nil
}

// DeleteFile removes a file from the outstation.
func (s *Session) DeleteFile(ctx context.Context, name string) error {
	if name == "" {
		return fmt.Errorf("master: %w: empty file name", dnp3.ErrBadConfig)
	}
	t := &transfer{name: name, requestID: s.nextRequestID()}
	if err := s.run(ctx, newFileDeleteTask(t)); err != nil {
		return err
	}
	return t.err
}

// FileInfo describes a file on the outstation without transferring it.
//
// Not every device implements the request. One that does not answers with
// NO_FUNC_CODE_SUPPORT, which comes back as [dnp3.ErrNotSupported]; reading the
// parent directory is the fallback that works everywhere.
func (s *Session) FileInfo(ctx context.Context, name string) (dnp3.FileInfo, error) {
	if name == "" {
		return dnp3.FileInfo{}, fmt.Errorf("master: %w: empty file name", dnp3.ErrBadConfig)
	}

	var info dnp3.FileInfo
	t := &transfer{name: name, requestID: s.nextRequestID()}
	if err := s.run(ctx, newFileInfoTask(t, &info)); err != nil {
		return dnp3.FileInfo{}, err
	}
	return info, t.err
}

// runTransfer runs a chained transfer and makes sure the file is closed.
func (s *Session) runTransfer(ctx context.Context, t *transfer, first *task) error {
	err := s.run(ctx, first)
	if err == nil {
		err = t.err
	}
	if err == nil {
		return nil
	}

	// The chain stopped early. If a handle was issued, the outstation is
	// holding the file open and will keep refusing transfers until its own
	// timeout expires — so the close is attempted even when the caller's
	// context is already done, and bounded so a dead link cannot hang here.
	if t.handle != 0 && !t.closed {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.ResponseTimeout)
		defer cancel()
		if cerr := s.run(closeCtx, newFileCloseTask(t)); cerr != nil {
			s.log.Warn("could not close a failed transfer", "file", t.name, "err", cerr)
		}
	}
	return err
}

// fileBlockSize is the largest block the master will ask for.
//
// Both fragment caps bound it, because one block size covers both directions:
// a read block arrives in a response and a write block goes out in a request,
// and a master that sized only for what it can receive would negotiate a block
// it could not then send.
func (s *Session) fileBlockSize() uint16 {
	if s.cfg.FileBlockSize > 0 {
		return s.cfg.FileBlockSize
	}
	// Room for the application header, the object header and the transport
	// object's fixed part, with margin.
	room := min(s.cfg.MaxRxFragment, s.cfg.MaxTxFragment) - 32
	room = max(room, 64)
	room = min(room, 0xFFFF)
	return uint16(room)
}

// limitedWriter stops an outstation from filling memory with a file whose
// declared size bore no relation to what it sent.
type limitedWriter struct {
	w       io.Writer
	max     int64
	written int64
	name    string
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.written+int64(len(p)) > l.max {
		return 0, fmt.Errorf("master: %q exceeds the %d octet limit for reading a file into memory",
			l.name, l.max)
	}
	n, err := l.w.Write(p)
	l.written += int64(n)
	return n, err
}
