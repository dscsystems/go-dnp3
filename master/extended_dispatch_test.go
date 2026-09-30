package master

import (
	"bytes"
	"testing"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/objects"
)

func TestFrozenAnalogAndTerminalDispatch(t *testing.T) {
	h := NewChannelHandler(2)
	s := New(Config{}, h)
	codec, _ := objects.AnalogCodec(objects.GV(33, 7))
	s.dispatch(app.ObjectHeader{Group: 33, Variation: 7, Qualifier: app.MakeQualifier(app.PrefixIndex1, app.RangeCount8), Range: app.Range{Spec: app.RangeCount8, Count: 1}, Data: codec.Write([]byte{9}, dnp3.Analog{Value: 12.5, Flags: dnp3.Online}, objects.Context{})}, objects.Context{})
	u := <-h.Updates()
	if u.Type != dnp3.TypeFrozenAnalog || u.Index != 9 || u.FrozenAnalog.Value != 12.5 || !u.Info.IsEvent() {
		t.Fatal(u)
	}
	s.dispatch(app.ObjectHeader{Group: 113, Variation: 2, Qualifier: app.MakeQualifier(app.PrefixIndex1, app.RangeCount8), Range: app.Range{Spec: app.RangeCount8, Count: 1}, Data: []byte{7, 'o', 'k'}}, objects.Context{})
	u = <-h.Updates()
	if u.Type != dnp3.TypeVirtualTerminal || u.Index != 7 || string(u.OctetString) != "ok" {
		t.Fatal(u)
	}
}

type datasetCollector struct {
	NopHandler
	values [][]byte
}

func (h *datasetCollector) HandleDataset(_ HeaderInfo, data []byte) {
	h.values = append(h.values, data)
}

func TestDatasetDispatchOwnsSnapshots(t *testing.T) {
	h := &datasetCollector{}
	s := New(Config{}, h)
	value := []byte{1, 3, 6, 0, 0, 0, 0, 0, 0, 1, 42}
	hdr, err := app.FreeFormat(88, 1, value)
	if err != nil {
		t.Fatal(err)
	}
	s.dispatch(hdr, objects.Context{})
	want := append([]byte(nil), value...)
	clear(hdr.Data)
	if len(h.values) != 1 || !bytes.Equal(h.values[0], want) {
		t.Fatal(h.values)
	}
}
