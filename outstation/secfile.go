package outstation

import (
	"fmt"
	"strings"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/internal/stack"
	"github.com/dscsystems/go-dnp3/secevent"
)

// fileEvent reports a file transfer event about request r.
func (s *Session) fileEvent(k secevent.Kind, r stack.Received, hdr app.Header, name string, status dnp3.FileStatus, detail string) {
	if !s.sec.Enabled() {
		return
	}
	e := appEvent(k, r, &hdr)
	e.FileName, e.Detail = name, detail
	if status != dnp3.FileSuccess {
		e.Fields |= secevent.HasFileStatus
		e.FileStatus = status
		e.Action = secevent.Rejected
	}
	s.emit(e)
}

// suspiciousFileName says what about a name looks like an attempt to reach
// outside the served files, if anything. The handler neutralises all of it;
// the attempt is still worth knowing about.
func suspiciousFileName(name string) string {
	switch {
	case strings.ContainsRune(name, 0):
		return "a NUL"
	case strings.ContainsRune(name, '\\'):
		return "backslashes"
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return "a '..' component"
		}
	}
	for _, c := range name {
		if c < 0x20 || c == 0x7F {
			return "control characters"
		}
	}
	return ""
}

// checkFileName reports a suspicious file name.
func (s *Session) checkFileName(r stack.Received, hdr app.Header, name string) {
	if why := suspiciousFileName(name); why != "" {
		s.fileEvent(secevent.KindFileSuspiciousName, r, hdr, name, dnp3.FileSuccess,
			fmt.Sprintf("file name with %s: a path traversal attempt?", why))
	}
}

// fileWriteDenied reports and refuses a write the function policy forbids.
func (s *Session) fileWriteDenied(r stack.Received, hdr app.Header, name string, mode dnp3.FileMode) bool {
	p := s.cfg.Security
	if p == nil || p.Functions == nil || (mode != dnp3.FileModeWrite && mode != dnp3.FileModeAppend) {
		return false
	}
	if !p.Functions.DenyFileWrite && !p.Functions.ReadOnly {
		return false
	}
	e := appEvent(secevent.KindAccessFunctionBlocked, r, &hdr)
	e.FileName, e.Action = name, secevent.Rejected
	e.Detail = "file write refused by the function policy"
	s.emit(e)
	return true
}

// fileOpened reports a transfer that opened, or did not.
func (s *Session) fileOpened(r stack.Received, hdr app.Header, name string, mode dnp3.FileMode, size uint32, dir bool, status dnp3.FileStatus) {
	if !s.sec.Enabled() {
		return
	}
	if status != dnp3.FileSuccess {
		s.fileEvent(secevent.KindFileRejected, r, hdr, name, status, fmt.Sprintf("open for %v refused: %v", mode, status))
		return
	}
	switch {
	case mode == dnp3.FileModeWrite || mode == dnp3.FileModeAppend:
		s.fileEvent(secevent.KindFileWriteOpened, r, hdr, name, status, fmt.Sprintf("file opened for %v, %d octets announced", mode, size))
		if max := s.cfg.Security.MaxFileSize; max > 0 && int64(size) > max {
			s.fileEvent(secevent.KindFileOversized, r, hdr, name, status,
				fmt.Sprintf("announced size %d exceeds the policy's %d", size, max))
		}
	case !s.sec.Audit():
	case dir:
		s.fileEvent(secevent.KindFileDirectoryList, r, hdr, name, status, "directory listed")
	default:
		s.fileEvent(secevent.KindFileReadOpened, r, hdr, name, status, fmt.Sprintf("file opened for reading, %d octets", size))
	}
}
