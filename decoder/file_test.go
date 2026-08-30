package decoder

import (
	"strings"
	"testing"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/objects"
)

// header wraps one group 70 object the way it travels on the wire.
func header(t *testing.T, variation uint8, obj []byte) app.ObjectHeader {
	t.Helper()
	h, err := app.FreeFormat(70, variation, obj)
	if err != nil {
		t.Fatalf("building the header: %v", err)
	}
	return h
}

func TestDecodeFileObjects(t *testing.T) {
	cases := []struct {
		name      string
		variation uint8
		obj       []byte
		want      []string
	}{
		{
			name:      "open command",
			variation: 3,
			obj: objects.AppendFileCommand(nil, objects.FileCommand{
				Name: "/config.xml", Mode: dnp3.FileModeRead, MaxBlockSize: 512, RequestID: 4,
			}),
			want: []string{"read", `"/config.xml"`, "req=4", "block=512"},
		},
		{
			name:      "command status",
			variation: 4,
			obj: objects.AppendFileCommandStatus(nil, objects.FileCommandStatus{
				Handle: 0xABCD, Size: 900, RequestID: 4, Status: dnp3.FileNotFound, Text: "gone",
			}),
			want: []string{"handle=0x0000abcd", "req=4", "file not found", "(gone)"},
		},
		{
			name:      "a block of file data",
			variation: 5,
			obj: objects.AppendFileTransport(nil, objects.FileTransport{
				Handle: 1, Block: 7, Last: true, Data: make([]byte, 250),
			}),
			want: []string{"block=7", "last", "data=250B"},
		},
		{
			name:      "transport status",
			variation: 6,
			obj: objects.AppendFileTransportStatus(nil, objects.FileTransportStatus{
				Handle: 1, Block: 2, Status: dnp3.FileBlockSequence,
			}),
			want: []string{"block=2", "block sequence error"},
		},
		{
			name:      "directory entry",
			variation: 7,
			obj: objects.AppendFileDescriptor(nil, objects.FileDescriptor{
				Name: "logs", Type: dnp3.FileTypeDirectory, Size: 4096,
				Permissions: dnp3.PermOwnerRead | dnp3.PermOwnerExecute,
				Created:     time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC),
			}),
			want: []string{"directory", `"logs"`, "r-x------", "4096 octets", "2024-01-02"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			values, ok := DecodeValues(header(t, c.variation, c.obj), objects.Context{})
			if !ok {
				t.Fatal("the header was not decoded")
			}
			if len(values) != 1 {
				t.Fatalf("decoded %d values, want 1", len(values))
			}
			got := values[0].Value
			for _, want := range c.want {
				if !strings.Contains(got, want) {
					t.Errorf("rendering %q does not contain %q", got, want)
				}
			}
		})
	}
}

// TestDecodeFileAuthHidesThePassword: a capture ends up in tickets and chat.
func TestDecodeFileAuthHidesThePassword(t *testing.T) {
	obj := objects.AppendFileAuth(nil, objects.FileAuth{User: "operator", Password: "hunter2"})

	values, ok := DecodeValues(header(t, 2, obj), objects.Context{})
	if !ok || len(values) != 1 {
		t.Fatalf("decoded %v, %v", values, ok)
	}
	if got := values[0].Value; strings.Contains(got, "hunter2") {
		t.Errorf("the rendering leaks the password: %q", got)
	}
	if got := values[0].Value; !strings.Contains(got, "operator") {
		t.Errorf("rendering %q does not name the user", got)
	}
}

// TestDecodeMalformedFileObject: a device sending something undecodable is the
// most interesting thing on the line, so it is reported rather than dropped.
func TestDecodeMalformedFileObject(t *testing.T) {
	values, ok := DecodeValues(header(t, 3, []byte{1, 2, 3}), objects.Context{})
	if !ok || len(values) != 1 {
		t.Fatalf("decoded %v, %v", values, ok)
	}
	if got := values[0].Value; !strings.Contains(got, "malformed") {
		t.Errorf("rendering %q does not say the object was malformed", got)
	}
}

// TestDecodeSeveralDirectoryEntries covers a header carrying more than one
// object, which is what a directory listing looks like when a device sends it
// in one header rather than as file content.
func TestDecodeSeveralDirectoryEntries(t *testing.T) {
	var data []byte
	for _, name := range []string{"a.txt", "b.txt"} {
		obj := objects.AppendFileDescriptor(nil, objects.FileDescriptor{Name: name})
		data = append(data, byte(len(obj)), byte(len(obj)>>8))
		data = append(data, obj...)
	}

	h := app.ObjectHeader{
		Group: 70, Variation: 7,
		Qualifier: app.FreeFormatQualifier,
		Range:     app.Range{Spec: app.RangeVariable, Count: 2},
		Data:      data,
	}

	values, ok := DecodeValues(h, objects.Context{})
	if !ok {
		t.Fatal("the header was not decoded")
	}
	if len(values) != 2 {
		t.Fatalf("decoded %d values, want 2", len(values))
	}
	if !strings.Contains(values[1].Value, "b.txt") {
		t.Errorf("the second entry rendered as %q", values[1].Value)
	}
}
