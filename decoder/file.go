package decoder

import (
	"fmt"
	"strings"

	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/objects"
)

// Group 70 renders differently from everything else here because it is not
// measurements. A file exchange is a conversation, and what an engineer needs
// out of a capture is the thread of it: which file, which handle, which block,
// and what the outstation said about it.
//
// The index a Value carries is the object's position in the header rather than
// a point index — group 70 has no point indexes — which is what keeps a header
// carrying several descriptors readable.

// decodeFileObjects renders the group 70 objects a header carries.
func decodeFileObjects(h app.ObjectHeader) ([]Value, bool) {
	objs, err := app.FreeFormatObjects(h)
	if err != nil {
		return nil, false
	}

	out := make([]Value, 0, len(objs))
	for i, obj := range objs {
		text, ok := fileObjectText(h.Variation, obj)
		if !ok {
			continue
		}
		out = append(out, Value{Index: uint16(i), Value: text})
	}
	return out, len(out) > 0
}

// fileObjectText renders one object. A malformed one is reported as such
// rather than dropped: a capture exists to show what arrived, and the fact
// that a device sent something undecodable is the most interesting thing on
// the line when it happens.
func fileObjectText(variation uint8, obj []byte) (string, bool) {
	switch variation {
	case 2:
		a, err := objects.ParseFileAuth(obj)
		if err != nil {
			return malformed(err), true
		}
		// The password is deliberately not rendered. A capture is a file that
		// gets pasted into tickets.
		return fmt.Sprintf("auth user=%q key=%#08x", a.User, a.Key), true

	case 3:
		c, err := objects.ParseFileCommand(obj)
		if err != nil {
			return malformed(err), true
		}
		s := fmt.Sprintf("%s %q req=%d", c.Mode, c.Name, c.RequestID)
		if c.Size > 0 {
			s += fmt.Sprintf(" size=%d", c.Size)
		}
		if c.MaxBlockSize > 0 {
			s += fmt.Sprintf(" block=%d", c.MaxBlockSize)
		}
		return s, true

	case 4:
		st, err := objects.ParseFileCommandStatus(obj)
		if err != nil {
			return malformed(err), true
		}
		s := fmt.Sprintf("handle=%#08x req=%d → %s", st.Handle, st.RequestID, st.Status)
		if st.Size > 0 {
			s += fmt.Sprintf(" size=%d", st.Size)
		}
		if st.MaxBlockSize > 0 {
			s += fmt.Sprintf(" block=%d", st.MaxBlockSize)
		}
		return s + optionalText(st.Text), true

	case 5:
		t, err := objects.ParseFileTransport(obj)
		if err != nil {
			return malformed(err), true
		}
		s := fmt.Sprintf("handle=%#08x block=%d", t.Handle, t.Block)
		if t.Last {
			s += " last"
		}
		// The data itself is summarised, not dumped: a capture of a firmware
		// image would otherwise be megabytes of hex nobody reads.
		return s + fmt.Sprintf(" data=%dB", len(t.Data)), true

	case 6:
		st, err := objects.ParseFileTransportStatus(obj)
		if err != nil {
			return malformed(err), true
		}
		s := fmt.Sprintf("handle=%#08x block=%d", st.Handle, st.Block)
		if st.Last {
			s += " last"
		}
		return s + " → " + st.Status.String() + optionalText(st.Text), true

	case 7:
		d, err := objects.ParseFileDescriptor(obj)
		if err != nil {
			return malformed(err), true
		}
		s := fmt.Sprintf("%s %q %s %d octets", d.Type, d.Name, d.Permissions, d.Size)
		if !d.Created.IsZero() {
			s += " " + d.Created.Format("2006-01-02 15:04:05")
		}
		return s, true

	case 8:
		return fmt.Sprintf("file specification %q", strings.TrimRight(string(obj), "\x00")), true

	default:
		return "", false
	}
}

func malformed(err error) string { return "malformed: " + err.Error() }

func optionalText(s string) string {
	if s == "" {
		return ""
	}
	return " (" + s + ")"
}
