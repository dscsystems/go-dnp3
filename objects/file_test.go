package objects

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/dscsystems/go-dnp3"
)

// The wire layouts here are fixed by IEEE 1815 table 4-6 and are what a device
// on the other end will send, so the tests assert the octets as well as the
// round trip. A round trip alone proves only that this implementation agrees
// with itself.

func TestFileCommandRoundTrip(t *testing.T) {
	created := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)
	want := FileCommand{
		Name:         "/config/settings.xml",
		Created:      created,
		Permissions:  dnp3.PermOwnerRead | dnp3.PermOwnerWrite | dnp3.PermGroupRead,
		Key:          0xDEADBEEF,
		Size:         4096,
		Mode:         dnp3.FileModeWrite,
		MaxBlockSize: 512,
		RequestID:    7,
	}

	buf := AppendFileCommand(nil, want)
	if len(buf) != FileCommandSize+len(want.Name) {
		t.Fatalf("encoded %d octets, want %d", len(buf), FileCommandSize+len(want.Name))
	}
	// The name offset is the fixed size: it is what makes the object walkable.
	if off := uint16(buf[0]) | uint16(buf[1])<<8; off != FileCommandSize {
		t.Errorf("name offset %d, want %d", off, FileCommandSize)
	}

	got, err := ParseFileCommand(buf)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !got.Created.Equal(want.Created) {
		t.Errorf("created %v, want %v", got.Created, want.Created)
	}
	got.Created, want.Created = time.Time{}, time.Time{}
	if got != want {
		t.Errorf("round trip\n got %+v\nwant %+v", got, want)
	}
}

// TestFileCommandNoCreationTime covers the device that keeps no creation time.
// Reporting its zero as 1970 would put a date on a file that has none.
func TestFileCommandNoCreationTime(t *testing.T) {
	buf := AppendFileCommand(nil, FileCommand{Name: "x", Mode: dnp3.FileModeRead})
	got, err := ParseFileCommand(buf)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !got.Created.IsZero() {
		t.Errorf("created %v, want the zero time", got.Created)
	}
}

func TestFileCommandStatusRoundTrip(t *testing.T) {
	want := FileCommandStatus{
		Handle:       0x11223344,
		Size:         1234,
		MaxBlockSize: 250,
		RequestID:    9,
		Status:       dnp3.FileNotFound,
		Text:         "no such file",
	}
	got, err := ParseFileCommandStatus(AppendFileCommandStatus(nil, want))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got != want {
		t.Errorf("round trip\n got %+v\nwant %+v", got, want)
	}
}

func TestFileTransportRoundTrip(t *testing.T) {
	for _, last := range []bool{false, true} {
		want := FileTransport{
			Handle: 42,
			Block:  3,
			Last:   last,
			Data:   []byte("the quick brown fox"),
		}
		buf := AppendFileTransport(nil, want)

		// The last-block flag is the top bit of the block number, so a block
		// number must survive it: a transfer that lost the flag would end early
		// and one that lost the number would be reassembled out of order.
		if raw := uint32(buf[7]) << 24; (raw&(1<<31) != 0) != last {
			t.Errorf("last=%v not encoded in the block number", last)
		}

		got, err := ParseFileTransport(buf)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if got.Handle != want.Handle || got.Block != want.Block || got.Last != want.Last {
			t.Errorf("round trip\n got %+v\nwant %+v", got, want)
		}
		if !bytes.Equal(got.Data, want.Data) {
			t.Errorf("data %q, want %q", got.Data, want.Data)
		}
	}
}

func TestFileTransportStatusRoundTrip(t *testing.T) {
	want := FileTransportStatus{Handle: 7, Block: 2, Last: true, Status: dnp3.FileBlockSequence}
	got, err := ParseFileTransportStatus(AppendFileTransportStatus(nil, want))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got != want {
		t.Errorf("round trip\n got %+v\nwant %+v", got, want)
	}
}

func TestFileDescriptorRoundTrip(t *testing.T) {
	want := FileDescriptor{
		Name:        "events.log",
		Type:        dnp3.FileTypeSimple,
		Size:        900,
		Created:     time.Date(2023, 7, 4, 9, 30, 0, 0, time.UTC),
		Permissions: dnp3.PermOwnerRead,
		RequestID:   3,
	}
	got, err := ParseFileDescriptor(AppendFileDescriptor(nil, want))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !got.Created.Equal(want.Created) {
		t.Errorf("created %v, want %v", got.Created, want.Created)
	}
	got.Created, want.Created = time.Time{}, time.Time{}
	if got != want {
		t.Errorf("round trip\n got %+v\nwant %+v", got, want)
	}
}

func TestFileAuthRoundTrip(t *testing.T) {
	want := FileAuth{User: "operator", Password: "hunter2", Key: 0x01020304}
	got, err := ParseFileAuth(AppendFileAuth(nil, want))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got != want {
		t.Errorf("round trip\n got %+v\nwant %+v", got, want)
	}
}

// TestParseDirectory walks a directory's contents, which is the one place the
// name offset earns its keep: without it there is no way to know where one
// entry ends and the next begins.
func TestParseDirectory(t *testing.T) {
	var content []byte
	entries := []FileDescriptor{
		{Name: "logs", Type: dnp3.FileTypeDirectory, Permissions: dnp3.PermOwnerRead},
		{Name: "settings.xml", Type: dnp3.FileTypeSimple, Size: 2048},
		{Name: "a", Type: dnp3.FileTypeSimple, Size: 1},
	}
	for _, e := range entries {
		content = AppendFileDescriptor(content, e)
	}

	got, err := ParseDirectory(content)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != len(entries) {
		t.Fatalf("parsed %d entries, want %d", len(got), len(entries))
	}
	for i, e := range entries {
		if got[i].Name != e.Name || got[i].Type != e.Type || got[i].Size != e.Size {
			t.Errorf("entry %d = %+v, want %+v", i, got[i], e)
		}
	}
	if !got[0].IsDir() {
		t.Error("logs should be a directory")
	}
}

func TestParseDirectoryEmpty(t *testing.T) {
	got, err := ParseDirectory(nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("parsed %d entries from nothing", len(got))
	}
}

// TestFileObjectsRejectMalformed covers what a device that disagrees with the
// standard — or an attacker — can send. Each case would otherwise slice past
// the object.
func TestFileObjectsRejectMalformed(t *testing.T) {
	valid := AppendFileCommand(nil, FileCommand{Name: "abc"})

	// A name that runs past the end of the object.
	long := bytes.Clone(valid)
	long[2], long[3] = 0xFF, 0x00 // name size 255

	// A name that starts inside the fixed part, which would overlap the fields.
	overlap := bytes.Clone(valid)
	overlap[0], overlap[1] = 4, 0

	cases := []struct {
		name string
		fn   func() error
	}{
		{"command too short", func() error {
			_, err := ParseFileCommand(valid[:10])
			return err
		}},
		{"name past the end", func() error {
			_, err := ParseFileCommand(long)
			return err
		}},
		{"name inside the fixed part", func() error {
			_, err := ParseFileCommand(overlap)
			return err
		}},
		{"status too short", func() error {
			_, err := ParseFileCommandStatus(make([]byte, 4))
			return err
		}},
		{"transport too short", func() error {
			_, err := ParseFileTransport(make([]byte, 3))
			return err
		}},
		{"transport status too short", func() error {
			_, err := ParseFileTransportStatus(make([]byte, 8))
			return err
		}},
		{"descriptor too short", func() error {
			_, err := ParseFileDescriptor(make([]byte, 19))
			return err
		}},
		{"auth too short", func() error {
			_, err := ParseFileAuth(make([]byte, 11))
			return err
		}},
		{"directory entry truncated", func() error {
			_, err := ParseDirectory(make([]byte, 8))
			return err
		}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.fn()
			if err == nil {
				t.Fatal("accepted a malformed object")
			}
			// Callers classify with the shared sentinel rather than by
			// importing this package.
			if !errors.Is(err, dnp3.ErrMalformed) {
				t.Errorf("error %v does not wrap dnp3.ErrMalformed", err)
			}
		})
	}
}

func TestFilePermissionsString(t *testing.T) {
	cases := []struct {
		perms dnp3.FilePermissions
		want  string
	}{
		{0, "---------"},
		{dnp3.PermOwnerRead | dnp3.PermOwnerWrite, "rw-------"},
		{dnp3.PermOwnerRead | dnp3.PermGroupRead | dnp3.PermWorldRead, "r--r--r--"},
		{0x1FF, "rwxrwxrwx"},
	}
	for _, c := range cases {
		if got := c.perms.String(); got != c.want {
			t.Errorf("%#x = %q, want %q", uint16(c.perms), got, c.want)
		}
	}
}

func TestFileStatusErr(t *testing.T) {
	if err := dnp3.FileSuccess.Err(); err != nil {
		t.Errorf("success reported %v", err)
	}
	err := dnp3.FilePermissionDenied.Err()
	if !errors.Is(err, dnp3.ErrFileTransfer) {
		t.Errorf("error %v does not wrap dnp3.ErrFileTransfer", err)
	}
	if got := err.Error(); got == "" || !bytes.Contains([]byte(got), []byte("permission denied")) {
		t.Errorf("error %q does not name the status", got)
	}
}
