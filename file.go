package dnp3

import (
	"fmt"
	"time"
)

// File transfer — group 70 — is how a master reads and writes the files on an
// outstation: configuration, firmware images, event logs, and the directory
// listings that say what is there.
//
// It is unlike the rest of DNP3. Everything else in the protocol names a point
// by index and carries a measurement; file transfer names a path, opens a
// handle, and moves opaque octets in numbered blocks. The types here are the
// vocabulary of that exchange; the wire encodings live in the objects package,
// and the sequencing in master and outstation.

// FileType says whether a directory entry is a file or another directory.
type FileType uint16

// File types.
const (
	FileTypeDirectory FileType = 0
	FileTypeSimple    FileType = 1
)

func (t FileType) String() string {
	switch t {
	case FileTypeDirectory:
		return "directory"
	case FileTypeSimple:
		return "file"
	default:
		return fmt.Sprintf("FileType(%d)", uint16(t))
	}
}

// FileMode is what a file is opened for.
type FileMode uint16

// File open modes.
const (
	// FileModeNull opens nothing. It is what a command that names a file
	// without transferring it — a delete — carries.
	FileModeNull FileMode = 0
	// FileModeRead opens an existing file for reading.
	FileModeRead FileMode = 1
	// FileModeWrite creates or truncates a file for writing.
	FileModeWrite FileMode = 2
	// FileModeAppend opens a file for writing at its end.
	FileModeAppend FileMode = 3
)

func (m FileMode) String() string {
	switch m {
	case FileModeNull:
		return "null"
	case FileModeRead:
		return "read"
	case FileModeWrite:
		return "write"
	case FileModeAppend:
		return "append"
	default:
		return fmt.Sprintf("FileMode(%d)", uint16(m))
	}
}

// FileStatus is the outcome an outstation reports for a file operation.
//
// The set is the standard's, including the gap between 9 and 16: the codes are
// fixed by IEEE 1815 table 4-8 and are not renumbered here, because a capture
// has to be readable against the specification.
type FileStatus uint8

// File operation status codes.
const (
	FileSuccess          FileStatus = 0
	FilePermissionDenied FileStatus = 1
	// FileInvalidMode means the requested mode is not one the outstation
	// allows for that file — writing a read-only configuration, say.
	FileInvalidMode FileStatus = 2
	FileNotFound    FileStatus = 3
	// FileLocked means another master has the file open.
	FileLocked      FileStatus = 4
	FileTooManyOpen FileStatus = 5
	// FileInvalidHandle means the handle is unknown, which is what a master
	// sees after the outstation has timed the transfer out.
	FileInvalidHandle  FileStatus = 6
	FileWriteBlockSize FileStatus = 7
	FileCommLost       FileStatus = 8
	FileCannotAbort    FileStatus = 9
	FileNotOpen        FileStatus = 16
	// FileHandleExpired means the outstation closed the file itself after the
	// master went quiet for longer than its inactivity timeout.
	FileHandleExpired FileStatus = 17
	FileBufferOverrun FileStatus = 18
	// FileFatal means the transfer failed for a reason the outstation cannot
	// describe more precisely; the file is not usable.
	FileFatal FileStatus = 19
	// FileBlockSequence means a block arrived out of order, which invalidates
	// everything written so far.
	FileBlockSequence FileStatus = 20
	FileUndefined     FileStatus = 255
)

var fileStatusNames = map[FileStatus]string{
	FileSuccess:          "success",
	FilePermissionDenied: "permission denied",
	FileInvalidMode:      "invalid mode",
	FileNotFound:         "file not found",
	FileLocked:           "file locked",
	FileTooManyOpen:      "too many files open",
	FileInvalidHandle:    "invalid handle",
	FileWriteBlockSize:   "invalid block size",
	FileCommLost:         "communications lost",
	FileCannotAbort:      "cannot abort",
	FileNotOpen:          "file not open",
	FileHandleExpired:    "handle expired",
	FileBufferOverrun:    "buffer overrun",
	FileFatal:            "fatal error",
	FileBlockSequence:    "block sequence error",
	FileUndefined:        "undefined error",
}

func (s FileStatus) String() string {
	if n, ok := fileStatusNames[s]; ok {
		return n
	}
	return fmt.Sprintf("FileStatus(%d)", uint8(s))
}

// Err returns nil for [FileSuccess] and an error wrapping [ErrFileTransfer]
// otherwise, so a caller can test the outcome with errors.Is and still print
// what the outstation actually said.
func (s FileStatus) Err() error {
	if s == FileSuccess {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrFileTransfer, s)
}

// FilePermissions are the access bits of a file, laid out as POSIX lays them
// out: three groups of read, write and execute.
type FilePermissions uint16

// Permission bits, from IEEE 1815 table 4-7.
const (
	PermWorldExecute FilePermissions = 0x0001
	PermWorldWrite   FilePermissions = 0x0002
	PermWorldRead    FilePermissions = 0x0004
	PermGroupExecute FilePermissions = 0x0008
	PermGroupWrite   FilePermissions = 0x0010
	PermGroupRead    FilePermissions = 0x0020
	PermOwnerExecute FilePermissions = 0x0040
	PermOwnerWrite   FilePermissions = 0x0080
	PermOwnerRead    FilePermissions = 0x0100
)

// String renders the bits the way ls does, so a directory listing from a
// device reads like one from a filesystem.
func (p FilePermissions) String() string {
	bits := [9]struct {
		mask FilePermissions
		char byte
	}{
		{PermOwnerRead, 'r'}, {PermOwnerWrite, 'w'}, {PermOwnerExecute, 'x'},
		{PermGroupRead, 'r'}, {PermGroupWrite, 'w'}, {PermGroupExecute, 'x'},
		{PermWorldRead, 'r'}, {PermWorldWrite, 'w'}, {PermWorldExecute, 'x'},
	}
	out := make([]byte, len(bits))
	for i, b := range bits {
		if p&b.mask != 0 {
			out[i] = b.char
		} else {
			out[i] = '-'
		}
	}
	return string(out)
}

// FileInfo describes one file or directory on an outstation.
type FileInfo struct {
	// Name is the path the outstation knows the file by. In a directory
	// listing it is the entry's own name, not the full path.
	Name string
	Type FileType
	Size uint32
	// Created is the file's creation time. It is the zero time when the
	// outstation does not keep one, which many embedded devices do not.
	Created     time.Time
	Permissions FilePermissions
}

// IsDir reports whether the entry is a directory.
func (f FileInfo) IsDir() bool { return f.Type == FileTypeDirectory }

func (f FileInfo) String() string {
	kind := "-"
	if f.IsDir() {
		kind = "d"
	}
	return fmt.Sprintf("%s%s %8d %s", kind, f.Permissions, f.Size, f.Name)
}
