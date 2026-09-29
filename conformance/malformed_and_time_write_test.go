package conformance

import (
	"sync"
	"testing"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/outstation"
)

// clockApp records the time a master writes.
type clockApp struct {
	outstation.NopApplication
	mu  sync.Mutex
	set []time.Time
}

func (c *clockApp) WriteAbsoluteTime(t time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.set = append(c.set, t)
	return true
}

func (c *clockApp) written() []time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Time(nil), c.set...)
}

// Both 0x17 and 0x28 are legal qualifiers for a write. The index prefix is part
// of the object data, so reading the timestamp from the first octet took the
// index for the low octet of the time and set a clock centuries out.
func TestWriteTimeWithIndexPrefixedQualifierSetsTheRightClock(t *testing.T) {
	want := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	ms := dnp3.TimeToDNP3(want)
	stamp := []byte{byte(ms), byte(ms >> 8), byte(ms >> 16),
		byte(ms >> 24), byte(ms >> 32), byte(ms >> 40)}

	tests := []struct {
		name string
		hdr  app.ObjectHeader
	}{
		{"0x07 no prefix", app.ObjectHeader{
			Qualifier: app.MakeQualifier(app.PrefixNone, app.RangeCount8),
			Range:     app.Range{Spec: app.RangeCount8, Count: 1},
			Data:      stamp,
		}},
		{"0x17 one-octet index", app.ObjectHeader{
			Qualifier: app.MakeQualifier(app.PrefixIndex1, app.RangeCount8),
			Range:     app.Range{Spec: app.RangeCount8, Count: 1},
			Data:      append([]byte{0}, stamp...),
		}},
		{"0x28 two-octet index", app.ObjectHeader{
			Qualifier: app.MakeQualifier(app.PrefixIndex2, app.RangeCount16),
			Range:     app.Range{Spec: app.RangeCount16, Count: 1},
			Data:      append([]byte{0, 0}, stamp...),
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ca := &clockApp{}
			h := newHarnessWithApp(t, outstation.Config{Database: smallDB()}, ca)

			tc.hdr.Group, tc.hdr.Variation = 50, 1
			h.request(app.FuncWrite, tc.hdr)

			got := ca.written()
			if len(got) != 1 {
				t.Fatalf("clock written %d times, want once", len(got))
			}
			if !got[0].Equal(want) {
				t.Errorf("clock set to %v, want %v", got[0], want)
			}
		})
	}
}

// A request the outstation cannot parse used to be answered with nothing, so a
// master writing an object it does not implement timed out and retried
// forever. When the application header is readable there is a sequence number
// to answer on.
func TestUnparseableObjectSectionGetsAResponse(t *testing.T) {
	tests := []struct {
		name string
		fc   app.FuncCode
		hdr  app.ObjectHeader
		want app.IIN
	}{
		{"write of an unknown object", app.FuncWrite, app.ObjectHeader{
			Group: 99, Variation: 7,
			Qualifier: app.MakeQualifier(app.PrefixNone, app.RangeCount8),
			Range:     app.Range{Spec: app.RangeCount8, Count: 1},
			Data:      []byte{1, 2},
		}, app.IINObjectUnknown},
		{"freeze at time, which is not implemented", app.FuncFreezeAtTime, app.ObjectHeader{
			Group: 99, Variation: 7,
			Qualifier: app.MakeQualifier(app.PrefixNone, app.RangeCount8),
			Range:     app.Range{Spec: app.RangeCount8, Count: 1},
		}, app.IINNoFuncCodeSupport},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, outstation.Config{Database: smallDB()}, nil)

			resp := h.request(tc.fc, tc.hdr)

			if !resp.Header.IIN.Has(tc.want) {
				t.Errorf("IIN = %v, want %v", resp.Header.IIN, tc.want)
			}
			if len(resp.Objects) != 0 {
				t.Errorf("response carried %d objects, want a null response", len(resp.Objects))
			}
		})
	}
}

// A READ of a variation the group does not have is not an empty database.
func TestReadOfUnsupportedVariationSetsObjectUnknown(t *testing.T) {
	h := newHarness(t, outstation.Config{Database: smallDB()}, nil)

	resp := h.request(app.FuncRead, app.ReadRange(1, 99, 0, 1))
	if !resp.Header.IIN.Has(app.IINObjectUnknown) {
		t.Errorf("IIN = %v, want OBJECT_UNKNOWN", resp.Header.IIN)
	}
}
