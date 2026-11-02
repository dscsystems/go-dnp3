package conformance

import (
	"bytes"
	"testing"

	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/outstation"
)

// A deadband a master wrote can be read back, in each variation.
func TestReadAnalogDeadbands(t *testing.T) {
	h := newHarness(t, outstation.Config{Database: smallDB()}, nil) // 3 analogs

	// 100.5 into analog 1, as a single-precision float.
	resp := h.request(app.FuncWrite, app.ObjectHeader{
		Group: 34, Variation: 3,
		Qualifier: app.MakeQualifier(app.PrefixIndex1, app.RangeCount8),
		Range:     app.Range{Spec: app.RangeCount8, Count: 1},
		Data:      []byte{1, 0x00, 0x00, 0xC9, 0x42},
	})
	if resp.Header.IIN.HasError() {
		t.Fatalf("the deadband write was refused; IIN = %v", resp.Header.IIN)
	}

	tests := []struct {
		name      string
		req       app.ObjectHeader
		variation uint8
		data      []byte
	}{
		{"all objects, default variation", app.ReadAllObjects(34, 0), 3,
			[]byte{0, 0, 0, 0, 0x00, 0x00, 0xC9, 0x42, 0, 0, 0, 0}},
		{"range, 16-bit", app.ReadRange(34, 1, 1, 1), 1, []byte{101, 0}},
		{"range past the end, 32-bit", app.ReadRange(34, 2, 1, 200), 2,
			[]byte{101, 0, 0, 0, 0, 0, 0, 0}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := h.request(app.FuncRead, tc.req)
			if resp.Header.IIN.HasError() {
				t.Fatalf("IIN = %v", resp.Header.IIN)
			}
			if len(resp.Objects) != 1 {
				t.Fatalf("%d object headers, want 1", len(resp.Objects))
			}
			o := resp.Objects[0]
			if o.Group != 34 || o.Variation != tc.variation || !bytes.Equal(o.Data, tc.data) {
				t.Errorf("got g%dv%d % x, want g34v%d % x", o.Group, o.Variation, o.Data, tc.variation, tc.data)
			}
		})
	}
}

// A variation group 34 does not have is refused rather than answered empty.
func TestReadAnalogDeadbandUnknownVariation(t *testing.T) {
	h := newHarness(t, outstation.Config{Database: smallDB()}, nil)
	resp := h.request(app.FuncRead, app.ReadAllObjects(34, 4))
	if !resp.Header.IIN.Has(app.IINObjectUnknown) {
		t.Errorf("IIN = %v, want OBJECT_UNKNOWN", resp.Header.IIN)
	}
}
