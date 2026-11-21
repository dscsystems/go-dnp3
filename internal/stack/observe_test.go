package stack

import (
	"bytes"
	"testing"

	"github.com/dscsystems/go-dnp3/internal/link"
	"github.com/dscsystems/go-dnp3/internal/transport"
)

func frame(t *testing.T, c link.Control, dest, src uint16, payload []byte) []byte {
	t.Helper()
	b, err := link.Encode(nil, link.Header{Control: c, Dest: dest, Src: src}, payload)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestObserverSeesDroppedFrames(t *testing.T) {
	s := New(Config{LocalAddr: 10, RemoteAddr: 1})
	var got []Observation
	var refused []uint16
	s.SetObserver(func(o Observation) { got = append(got, o) }, func(h link.Header) bool {
		if h.Src == 1 {
			return true
		}
		refused = append(refused, h.Src)
		return false
	})
	primary := func(f link.Function) link.Control { return link.Control{Dir: true, Prm: true, Func: f} }
	var w bytes.Buffer
	in := [][]byte{
		frame(t, primary(link.FuncRequestLinkStatus), 99, 1, nil),                         // foreign dest
		frame(t, link.Control{Prm: true, Func: link.FuncRequestLinkStatus}, 10, 1, nil),   // wrong DIR
		frame(t, primary(link.FuncRequestLinkStatus), 10, 0xFFF5, nil),                    // invalid source
		frame(t, primary(link.FuncRequestLinkStatus), 10, 7, nil),                         // refused
		frame(t, primary(7), 10, 1, nil),                                                  // unsupported
		frame(t, primary(link.FuncUnconfirmedUserData), 10, 1, []byte{0x01, 0xC0, 0x01}),  // no FIR
		frame(t, link.Control{Dir: true, Prm: false, Func: link.FuncAck}, 10, 3, nil),     // reply from elsewhere
		frame(t, primary(link.FuncUnconfirmedUserData), 0xFFFF, 1, []byte{0xC0, 0xC0, 1}), // broadcast
		{0x01, 0x02, 0x03}, // garbage, flushed by the next frame
		frame(t, primary(link.FuncRequestLinkStatus), 10, 1, nil),
	}
	for _, b := range in {
		if err := s.Receive(&w, b, func(Received) {}); err != nil {
			t.Fatal(err)
		}
	}
	want := map[ObsKind]bool{ObsForeignDest: false, ObsWrongDir: false, ObsInvalidSource: false,
		ObsSecondary: false, ObsTransport: false, ObsUnexpectedSource: false, ObsBroadcast: false, ObsParser: false}
	for _, o := range got {
		want[o.Kind] = true
		switch o.Kind {
		case ObsSecondary:
			if o.Note != link.NoteUnsupported && o.Note != link.NoteRequestLinkStatus {
				t.Errorf("secondary note %d", o.Note)
			}
		case ObsTransport:
			if o.Discard != transport.DiscardNoFIR {
				t.Errorf("discard %v, want no FIR", o.Discard)
			}
		case ObsParser:
			if o.Parser.BytesDiscarded != 3 {
				t.Errorf("parser delta %+v, want 3 octets discarded", o.Parser)
			}
		}
	}
	for k, seen := range want {
		if !seen {
			t.Errorf("no observation of kind %d", k)
		}
	}
	if len(refused) != 1 || refused[0] != 7 {
		t.Errorf("refused %v, want [7]", refused)
	}
}

func TestNoObserverNoCallbacks(t *testing.T) {
	s := New(Config{LocalAddr: 10, RemoteAddr: 1})
	var w bytes.Buffer
	b := frame(t, link.Control{Dir: true, Prm: true, Func: link.FuncRequestLinkStatus}, 99, 1, nil)
	if err := s.Receive(&w, b, func(Received) {}); err != nil {
		t.Fatal(err)
	}
}
