package objects

import (
	"encoding/binary"
	"fmt"
	"time"

	"github.com/dscsystems/go-dnp3"
)

// Group 70 is hand-written for a different reason from the commands above: its
// objects are variable length. Every other object in the table has a size the
// generator can state, so a parser knows how many octets to take before it
// looks at them. A file command carries a name, a transport object carries a
// block of file data, and both are as long as they are — which is why the
// qualifier that carries them puts an explicit size in front of each object.
//
// These are the only codecs in this package that can fail. The rest are total
// functions over a buffer the framing layer has already measured; here the
// object measures itself, and a device that declares a name longer than the
// object holding it has to be refused rather than sliced.

// ErrFileObject means a group 70 object could not be decoded. It wraps
// [dnp3.ErrMalformed], so a caller can classify it without importing this
// package.
var ErrFileObject = fmt.Errorf("%w: file object", dnp3.ErrMalformed)

// Fixed portions of the group 70 objects, in octets. Each is also the offset
// at which the object's variable part — a name, a password, a block of file
// data — begins.
const (
	FileAuthSize            = 12 // g70v2
	FileCommandSize         = 26 // g70v3
	FileCommandStatusSize   = 13 // g70v4
	FileTransportSize       = 8  // g70v5
	FileTransportStatusSize = 9  // g70v6
	FileDescriptorSize      = 20 // g70v7
)

// fileLastBlock is the top bit of a block number, set on the final block of a
// transfer. Without it neither end could tell a short last block from a block
// that merely happened to be short.
const fileLastBlock uint32 = 1 << 31

// ---------- g70v2: file authentication ----------

// FileAuth is a group 70 variation 2 authentication object. A master sends it
// to exchange credentials for the authentication key that a later open
// command carries.
type FileAuth struct {
	User     string
	Password string
	// Key is the authentication key: zero in the request, and the value the
	// outstation issues in its reply.
	Key uint32
}

// ParseFileAuth decodes a group 70 variation 2 object.
func ParseFileAuth(buf []byte) (FileAuth, error) {
	if len(buf) < FileAuthSize {
		return FileAuth{}, fmt.Errorf("%w: g70v2 is %d octets, needs %d",
			ErrFileObject, len(buf), FileAuthSize)
	}
	user, err := sliceString(buf, "user name",
		binary.LittleEndian.Uint16(buf[0:2]), binary.LittleEndian.Uint16(buf[2:4]), FileAuthSize)
	if err != nil {
		return FileAuth{}, err
	}
	password, err := sliceString(buf, "password",
		binary.LittleEndian.Uint16(buf[4:6]), binary.LittleEndian.Uint16(buf[6:8]), FileAuthSize)
	if err != nil {
		return FileAuth{}, err
	}
	return FileAuth{
		User:     user,
		Password: password,
		Key:      binary.LittleEndian.Uint32(buf[8:12]),
	}, nil
}

// AppendFileAuth encodes a group 70 variation 2 object.
func AppendFileAuth(dst []byte, a FileAuth) []byte {
	dst = binary.LittleEndian.AppendUint16(dst, FileAuthSize)
	dst = binary.LittleEndian.AppendUint16(dst, uint16(len(a.User)))
	dst = binary.LittleEndian.AppendUint16(dst, uint16(FileAuthSize+len(a.User)))
	dst = binary.LittleEndian.AppendUint16(dst, uint16(len(a.Password)))
	dst = binary.LittleEndian.AppendUint32(dst, a.Key)
	dst = append(dst, a.User...)
	return append(dst, a.Password...)
}

// ---------- g70v3: file command ----------

// FileCommand is a group 70 variation 3 object: the request that opens or
// deletes a file.
type FileCommand struct {
	Name string
	// Created is the file's creation time, which a master sets when writing a
	// file and leaves zero otherwise.
	Created     time.Time
	Permissions dnp3.FilePermissions
	// Key is the authentication key from a prior authentication exchange.
	// Zero where the outstation does not require one.
	Key uint32
	// Size is the length of a file being written. It is zero when reading,
	// where the outstation is the one that knows.
	Size uint32
	Mode dnp3.FileMode
	// MaxBlockSize is the largest block the master will accept. The outstation
	// answers with the size it will actually use, which may be smaller.
	MaxBlockSize uint16
	// RequestID ties a response to the request that caused it.
	RequestID uint16
}

// ParseFileCommand decodes a group 70 variation 3 object.
func ParseFileCommand(buf []byte) (FileCommand, error) {
	if len(buf) < FileCommandSize {
		return FileCommand{}, fmt.Errorf("%w: g70v3 is %d octets, needs %d",
			ErrFileObject, len(buf), FileCommandSize)
	}
	name, err := sliceString(buf, "file name",
		binary.LittleEndian.Uint16(buf[0:2]), binary.LittleEndian.Uint16(buf[2:4]), FileCommandSize)
	if err != nil {
		return FileCommand{}, err
	}
	return FileCommand{
		Name:         name,
		Created:      parseFileTime(buf[4:10]),
		Permissions:  dnp3.FilePermissions(binary.LittleEndian.Uint16(buf[10:12])),
		Key:          binary.LittleEndian.Uint32(buf[12:16]),
		Size:         binary.LittleEndian.Uint32(buf[16:20]),
		Mode:         dnp3.FileMode(binary.LittleEndian.Uint16(buf[20:22])),
		MaxBlockSize: binary.LittleEndian.Uint16(buf[22:24]),
		RequestID:    binary.LittleEndian.Uint16(buf[24:26]),
	}, nil
}

// AppendFileCommand encodes a group 70 variation 3 object.
func AppendFileCommand(dst []byte, c FileCommand) []byte {
	dst = binary.LittleEndian.AppendUint16(dst, FileCommandSize)
	dst = binary.LittleEndian.AppendUint16(dst, uint16(len(c.Name)))
	dst = appendFileTime(dst, c.Created)
	dst = binary.LittleEndian.AppendUint16(dst, uint16(c.Permissions))
	dst = binary.LittleEndian.AppendUint32(dst, c.Key)
	dst = binary.LittleEndian.AppendUint32(dst, c.Size)
	dst = binary.LittleEndian.AppendUint16(dst, uint16(c.Mode))
	dst = binary.LittleEndian.AppendUint16(dst, c.MaxBlockSize)
	dst = binary.LittleEndian.AppendUint16(dst, c.RequestID)
	return append(dst, c.Name...)
}

// ---------- g70v4: file command status ----------

// FileCommandStatus is a group 70 variation 4 object: what an outstation
// answers an open, close or delete with, and what a master sends to close a
// file it has open.
type FileCommandStatus struct {
	// Handle identifies the open file for the rest of the transfer. It is the
	// outstation's to choose, and a master must send back exactly what it was
	// given.
	Handle uint32
	// Size is the file's length, which is how a master reading a file learns
	// how much to expect.
	Size uint32
	// MaxBlockSize is the block size the outstation has settled on.
	MaxBlockSize uint16
	RequestID    uint16
	Status       dnp3.FileStatus
	// Text is an optional human-readable explanation. Devices use it to say
	// what a status code could not.
	Text string
}

// ParseFileCommandStatus decodes a group 70 variation 4 object.
func ParseFileCommandStatus(buf []byte) (FileCommandStatus, error) {
	if len(buf) < FileCommandStatusSize {
		return FileCommandStatus{}, fmt.Errorf("%w: g70v4 is %d octets, needs %d",
			ErrFileObject, len(buf), FileCommandStatusSize)
	}
	return FileCommandStatus{
		Handle:       binary.LittleEndian.Uint32(buf[0:4]),
		Size:         binary.LittleEndian.Uint32(buf[4:8]),
		MaxBlockSize: binary.LittleEndian.Uint16(buf[8:10]),
		RequestID:    binary.LittleEndian.Uint16(buf[10:12]),
		Status:       dnp3.FileStatus(buf[12]),
		Text:         string(buf[FileCommandStatusSize:]),
	}, nil
}

// AppendFileCommandStatus encodes a group 70 variation 4 object.
func AppendFileCommandStatus(dst []byte, s FileCommandStatus) []byte {
	dst = binary.LittleEndian.AppendUint32(dst, s.Handle)
	dst = binary.LittleEndian.AppendUint32(dst, s.Size)
	dst = binary.LittleEndian.AppendUint16(dst, s.MaxBlockSize)
	dst = binary.LittleEndian.AppendUint16(dst, s.RequestID)
	dst = append(dst, byte(s.Status))
	return append(dst, s.Text...)
}

// ---------- g70v5: file transport ----------

// FileTransport is a group 70 variation 5 object: one block of a file.
type FileTransport struct {
	Handle uint32
	// Block counts from zero and rises by one per block. Last marks the final
	// block of the transfer, which is what tells the receiver the file is
	// complete rather than merely paused.
	Block uint32
	Last  bool
	Data  []byte
}

// ParseFileTransport decodes a group 70 variation 5 object. The returned Data
// aliases buf.
func ParseFileTransport(buf []byte) (FileTransport, error) {
	if len(buf) < FileTransportSize {
		return FileTransport{}, fmt.Errorf("%w: g70v5 is %d octets, needs %d",
			ErrFileObject, len(buf), FileTransportSize)
	}
	block := binary.LittleEndian.Uint32(buf[4:8])
	return FileTransport{
		Handle: binary.LittleEndian.Uint32(buf[0:4]),
		Block:  block &^ fileLastBlock,
		Last:   block&fileLastBlock != 0,
		Data:   buf[FileTransportSize:],
	}, nil
}

// AppendFileTransport encodes a group 70 variation 5 object.
func AppendFileTransport(dst []byte, t FileTransport) []byte {
	block := t.Block &^ fileLastBlock
	if t.Last {
		block |= fileLastBlock
	}
	dst = binary.LittleEndian.AppendUint32(dst, t.Handle)
	dst = binary.LittleEndian.AppendUint32(dst, block)
	return append(dst, t.Data...)
}

// ---------- g70v6: file transport status ----------

// FileTransportStatus is a group 70 variation 6 object: the acknowledgement of
// one written block.
type FileTransportStatus struct {
	Handle uint32
	Block  uint32
	Last   bool
	Status dnp3.FileStatus
	Text   string
}

// ParseFileTransportStatus decodes a group 70 variation 6 object.
func ParseFileTransportStatus(buf []byte) (FileTransportStatus, error) {
	if len(buf) < FileTransportStatusSize {
		return FileTransportStatus{}, fmt.Errorf("%w: g70v6 is %d octets, needs %d",
			ErrFileObject, len(buf), FileTransportStatusSize)
	}
	block := binary.LittleEndian.Uint32(buf[4:8])
	return FileTransportStatus{
		Handle: binary.LittleEndian.Uint32(buf[0:4]),
		Block:  block &^ fileLastBlock,
		Last:   block&fileLastBlock != 0,
		Status: dnp3.FileStatus(buf[8]),
		Text:   string(buf[FileTransportStatusSize:]),
	}, nil
}

// AppendFileTransportStatus encodes a group 70 variation 6 object.
func AppendFileTransportStatus(dst []byte, s FileTransportStatus) []byte {
	block := s.Block &^ fileLastBlock
	if s.Last {
		block |= fileLastBlock
	}
	dst = binary.LittleEndian.AppendUint32(dst, s.Handle)
	dst = binary.LittleEndian.AppendUint32(dst, block)
	dst = append(dst, byte(s.Status))
	return append(dst, s.Text...)
}

// ---------- g70v7: file descriptor ----------

// FileDescriptor is a group 70 variation 7 object: what a file is, rather than
// what it contains. It answers a file-info request, and a directory's contents
// are nothing but a run of these.
type FileDescriptor struct {
	Name        string
	Type        dnp3.FileType
	Size        uint32
	Created     time.Time
	Permissions dnp3.FilePermissions
	RequestID   uint16
}

// ParseFileDescriptor decodes a group 70 variation 7 object.
func ParseFileDescriptor(buf []byte) (FileDescriptor, error) {
	d, _, err := parseFileDescriptor(buf)
	return d, err
}

// parseFileDescriptor decodes one descriptor and reports how many octets it
// consumed, which is what lets directory contents be walked.
func parseFileDescriptor(buf []byte) (FileDescriptor, int, error) {
	if len(buf) < FileDescriptorSize {
		return FileDescriptor{}, 0, fmt.Errorf("%w: g70v7 is %d octets, needs %d",
			ErrFileObject, len(buf), FileDescriptorSize)
	}
	offset := binary.LittleEndian.Uint16(buf[0:2])
	size := binary.LittleEndian.Uint16(buf[2:4])
	name, err := sliceString(buf, "file name", offset, size, FileDescriptorSize)
	if err != nil {
		return FileDescriptor{}, 0, err
	}
	return FileDescriptor{
		Name:        name,
		Type:        dnp3.FileType(binary.LittleEndian.Uint16(buf[4:6])),
		Size:        binary.LittleEndian.Uint32(buf[6:10]),
		Created:     parseFileTime(buf[10:16]),
		Permissions: dnp3.FilePermissions(binary.LittleEndian.Uint16(buf[16:18])),
		RequestID:   binary.LittleEndian.Uint16(buf[18:20]),
		// The entry is as long as its name reaches, but never shorter than the
		// fixed part: an entry with no name declares an offset of its own
		// choosing, and a length below the fixed size would walk backwards.
	}, max(int(offset)+int(size), FileDescriptorSize), nil
}

// AppendFileDescriptor encodes a group 70 variation 7 object.
func AppendFileDescriptor(dst []byte, d FileDescriptor) []byte {
	dst = binary.LittleEndian.AppendUint16(dst, FileDescriptorSize)
	dst = binary.LittleEndian.AppendUint16(dst, uint16(len(d.Name)))
	dst = binary.LittleEndian.AppendUint16(dst, uint16(d.Type))
	dst = binary.LittleEndian.AppendUint32(dst, d.Size)
	dst = appendFileTime(dst, d.Created)
	dst = binary.LittleEndian.AppendUint16(dst, uint16(d.Permissions))
	dst = binary.LittleEndian.AppendUint16(dst, d.RequestID)
	return append(dst, d.Name...)
}

// Info converts a descriptor to the value a caller of the master API sees.
func (d FileDescriptor) Info() dnp3.FileInfo {
	return dnp3.FileInfo{
		Name:        d.Name,
		Type:        d.Type,
		Size:        d.Size,
		Created:     d.Created,
		Permissions: d.Permissions,
	}
}

// DescriptorFor builds the descriptor that reports info.
func DescriptorFor(info dnp3.FileInfo, requestID uint16) FileDescriptor {
	return FileDescriptor{
		Name:        info.Name,
		Type:        info.Type,
		Size:        info.Size,
		Created:     info.Created,
		Permissions: info.Permissions,
		RequestID:   requestID,
	}
}

// ParseDirectory decodes the contents of a directory file.
//
// Reading a directory is reading a file: the octets that come back are a run
// of file descriptors laid end to end, each one saying how long it is. That is
// why the descriptor carries a name offset at all — it is what makes the run
// walkable.
func ParseDirectory(data []byte) ([]dnp3.FileInfo, error) {
	var out []dnp3.FileInfo
	for off := 0; off < len(data); {
		d, n, err := parseFileDescriptor(data[off:])
		if err != nil {
			return out, fmt.Errorf("directory entry %d at offset %d: %w", len(out), off, err)
		}
		out = append(out, d.Info())
		off += n
	}
	return out, nil
}

// ---------- shared helpers ----------

// sliceString extracts a length-prefixed string an object points at.
//
// The offset is honoured rather than assumed: the standard puts one in the
// object precisely so a device may lay out its variable part however it likes,
// and a parser that took the fixed size on trust would misread anything that
// did.
func sliceString(buf []byte, what string, offset, size uint16, fixed int) (string, error) {
	if size == 0 {
		return "", nil
	}
	if int(offset) < fixed {
		return "", fmt.Errorf("%w: %s starts at %d, inside the object's fixed %d octets",
			ErrFileObject, what, offset, fixed)
	}
	end := int(offset) + int(size)
	if end > len(buf) {
		return "", fmt.Errorf("%w: %s runs to %d in a %d octet object",
			ErrFileObject, what, end, len(buf))
	}
	return string(buf[offset:end]), nil
}

// parseFileTime decodes a creation time, mapping the zero value to the zero
// time rather than to 1970 — a device that keeps no creation time sends zero,
// and reporting that as a date is worse than reporting nothing.
func parseFileTime(buf []byte) time.Time {
	ms := readTime48(buf)
	if ms == 0 {
		return time.Time{}
	}
	return dnp3.DNP3ToTime(ms)
}

// appendFileTime encodes a creation time, mapping the zero time back to zero.
func appendFileTime(dst []byte, t time.Time) []byte {
	if t.IsZero() {
		return appendTime48(dst, 0)
	}
	return appendTime48(dst, dnp3.TimeToDNP3(t))
}
