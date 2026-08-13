package decoder

import (
	"strings"
	"testing"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/internal/link"
	"github.com/dscsystems/go-dnp3/internal/transport"
	"github.com/dscsystems/go-dnp3/objects"
)

// respond wraps object headers in a response fragment and a link frame, then
// decodes it, returning the application layer.
func respond(t *testing.T, iin app.IIN, headers ...app.ObjectHeader) *AppInfo {
	t.Helper()

	frag := app.BuildResponse(nil,
		app.Control{Fir: true, Fin: true, Seq: 0}, app.FuncResponse, iin, headers...)

	var s transport.Segmenter
	segs := s.SegmentAll(frag)
	if len(segs) != 1 {
		t.Fatalf("test fragment needed %d segments; keep it to one", len(segs))
	}

	h := link.Header{
		Control: link.Control{Prm: true, Func: link.FuncUnconfirmedUserData},
		Dest:    1, Src: 10, Length: uint8(link.MinLength + len(segs[0])),
	}
	wire, err := link.Encode(nil, h, segs[0])
	if err != nil {
		t.Fatal(err)
	}

	tr, _, err := DecodeFrame(nil, wire)
	if err != nil {
		t.Fatalf("DecodeFrame: %v", err)
	}
	if tr.App == nil {
		t.Fatal("no application layer decoded")
	}
	if tr.App.Err != nil {
		t.Fatalf("application layer: %v", tr.App.Err)
	}
	return tr.App
}

func rangeHeader(group, variation uint8, start, stop uint32, data []byte) app.ObjectHeader {
	return app.ObjectHeader{
		Group: group, Variation: variation,
		Qualifier: app.MakeQualifier(app.PrefixNone, app.RangeStartStop8),
		Range:     app.Range{Spec: app.RangeStartStop8, Start: start, Stop: stop, Count: stop - start + 1},
		Data:      data,
	}
}

func TestDecodeBinaryValues(t *testing.T) {
	ai := respond(t, 0, rangeHeader(1, 2, 0, 3, []byte{0x81, 0x01, 0x81, 0x01}))

	vals := ai.Values[0]
	if len(vals) != 4 {
		t.Fatalf("decoded %d values, want 4", len(vals))
	}
	for i, want := range []string{"ON", "OFF", "ON", "OFF"} {
		if vals[i].Value != want {
			t.Errorf("index %d = %q, want %q", i, vals[i].Value, want)
		}
		if vals[i].Index != uint16(i) {
			t.Errorf("value %d carries index %d", i, vals[i].Index)
		}
		if vals[i].Type != dnp3.TypeBinary {
			t.Errorf("value %d type = %v, want Binary", i, vals[i].Type)
		}
	}
}

func TestDecodeAnalogValues(t *testing.T) {
	// 300 and 400 as 16-bit analogs with flags.
	ai := respond(t, 0, rangeHeader(30, 2, 0, 1, []byte{0x01, 0x2C, 0x01, 0x01, 0x90, 0x01}))

	vals := ai.Values[0]
	if len(vals) != 2 {
		t.Fatalf("decoded %d values, want 2", len(vals))
	}
	if vals[0].Value != "300" || vals[1].Value != "400" {
		t.Errorf("values = %q, %q, want 300, 400", vals[0].Value, vals[1].Value)
	}
}

// TestDecodePackedBinaries covers the encoding where the unit is the range,
// not the object: ten points in two octets.
func TestDecodePackedBinaries(t *testing.T) {
	ai := respond(t, 0, rangeHeader(1, 1, 0, 9, []byte{0b10101010, 0b00000011}))

	vals := ai.Values[0]
	if len(vals) != 10 {
		t.Fatalf("decoded %d values, want 10", len(vals))
	}
	want := []string{"OFF", "ON", "OFF", "ON", "OFF", "ON", "OFF", "ON", "ON", "ON"}
	for i, w := range want {
		if vals[i].Value != w {
			t.Errorf("bit %d = %q, want %q", i, vals[i].Value, w)
		}
	}
}

func TestDecodeValuesWithIndexPrefix(t *testing.T) {
	// Events arrive with per-object indexes because the points that changed
	// are not contiguous.
	h := app.ObjectHeader{
		Group: 32, Variation: 2,
		Qualifier: app.MakeQualifier(app.PrefixIndex1, app.RangeCount8),
		Range:     app.Range{Spec: app.RangeCount8, Count: 2},
		Data: []byte{
			5, 0x01, 0x64, 0x00, // index 5, online, 100
			9, 0x01, 0xC8, 0x00, // index 9, online, 200
		},
	}
	ai := respond(t, app.IINClass1Events, h)

	vals := ai.Values[0]
	if len(vals) != 2 {
		t.Fatalf("decoded %d values, want 2", len(vals))
	}
	if vals[0].Index != 5 || vals[1].Index != 9 {
		t.Errorf("indexes = %d, %d, want 5, 9", vals[0].Index, vals[1].Index)
	}
	if vals[0].Value != "100" || vals[1].Value != "200" {
		t.Errorf("values = %q, %q", vals[0].Value, vals[1].Value)
	}
}

func TestDecodeAbsoluteTimestamps(t *testing.T) {
	when := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	data := append([]byte{0x81}, timeOctets(when)...)

	ai := respond(t, app.IINClass1Events, app.ObjectHeader{
		Group: 2, Variation: 2,
		Qualifier: app.MakeQualifier(app.PrefixIndex1, app.RangeCount8),
		Range:     app.Range{Spec: app.RangeCount8, Count: 1},
		Data:      append([]byte{7}, data...),
	})

	v := ai.Values[0][0]
	if v.Index != 7 {
		t.Errorf("index = %d, want 7", v.Index)
	}
	if !v.Time.IsValid() {
		t.Fatal("event carried no timestamp")
	}
	if !v.Time.Time.Equal(when) {
		t.Errorf("time = %v, want %v", v.Time.Time, when)
	}
}

// TestCTOThreadedThroughFragment is the reason value decoding walks the object
// headers in order rather than decoding each independently: a group 51 object
// sets the base that the relative-time events after it are measured from.
func TestCTOThreadedThroughFragment(t *testing.T) {
	base := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)

	cto := app.ObjectHeader{
		Group: 51, Variation: 1,
		Qualifier: app.MakeQualifier(app.PrefixNone, app.RangeCount8),
		Range:     app.Range{Spec: app.RangeCount8, Count: 1},
		Data:      timeOctets(base),
	}
	// A relative-time binary event 1500 ms after the CTO.
	events := app.ObjectHeader{
		Group: 2, Variation: 3,
		Qualifier: app.MakeQualifier(app.PrefixIndex1, app.RangeCount8),
		Range:     app.Range{Spec: app.RangeCount8, Count: 1},
		Data:      []byte{4, 0x81, 0xDC, 0x05},
	}

	ai := respond(t, app.IINClass1Events, cto, events)

	if len(ai.Values) != 2 {
		t.Fatalf("got %d value groups, want 2", len(ai.Values))
	}
	vals := ai.Values[1]
	if len(vals) != 1 {
		t.Fatalf("decoded %d events, want 1", len(vals))
	}

	want := base.Add(1500 * time.Millisecond)
	if !vals[0].Time.IsValid() {
		t.Fatal("the relative-time event was not anchored to the CTO")
	}
	if !vals[0].Time.Time.Equal(want) {
		t.Errorf("time = %v, want %v", vals[0].Time.Time, want)
	}
}

// TestRelativeTimeWithoutCTOStaysInvalid is the other half: an offset with no
// base is not a time, and filing it at the epoch would look like real data.
func TestRelativeTimeWithoutCTOStaysInvalid(t *testing.T) {
	ai := respond(t, app.IINClass1Events, app.ObjectHeader{
		Group: 2, Variation: 3,
		Qualifier: app.MakeQualifier(app.PrefixIndex1, app.RangeCount8),
		Range:     app.Range{Spec: app.RangeCount8, Count: 1},
		Data:      []byte{4, 0x81, 0xDC, 0x05},
	})

	v := ai.Values[0][0]
	if v.Time.IsValid() {
		t.Errorf("time = %v, want invalid without a preceding CTO", v.Time)
	}
	if v.Value != "ON" {
		t.Errorf("the value should still decode: %q", v.Value)
	}
}

func TestDecodeCROBCommand(t *testing.T) {
	frag := app.BuildRequest(nil,
		app.Control{Fir: true, Fin: true, Seq: 1}, app.FuncDirectOperate,
		app.ObjectHeader{
			Group: 12, Variation: 1,
			Qualifier: app.MakeQualifier(app.PrefixIndex1, app.RangeCount8),
			Range:     app.Range{Spec: app.RangeCount8, Count: 1},
			Data:      []byte{3, 0x41, 1, 0xE8, 0x03, 0, 0, 0, 0, 0, 0, 0},
		})

	var s transport.Segmenter
	seg := s.SegmentAll(frag)[0]
	h := link.Header{
		Control: link.Control{Dir: true, Prm: true, Func: link.FuncUnconfirmedUserData},
		Dest:    10, Src: 1, Length: uint8(link.MinLength + len(seg)),
	}
	wire, _ := link.Encode(nil, h, seg)

	tr, _, err := DecodeFrame(nil, wire)
	if err != nil {
		t.Fatal(err)
	}

	vals := tr.App.Values[0]
	if len(vals) != 1 {
		t.Fatalf("decoded %d commands, want 1", len(vals))
	}
	if vals[0].Index != 3 {
		t.Errorf("index = %d, want 3", vals[0].Index)
	}
	for _, want := range []string{"PULSE_ON", "TRIP", "count=1", "on=1000ms", "SUCCESS"} {
		if !strings.Contains(vals[0].Value, want) {
			t.Errorf("command text %q is missing %q", vals[0].Value, want)
		}
	}
}

func TestDecodeAnalogOutputCommand(t *testing.T) {
	frag := app.BuildRequest(nil,
		app.Control{Fir: true, Fin: true}, app.FuncDirectOperate,
		app.ObjectHeader{
			Group: 41, Variation: 2,
			Qualifier: app.MakeQualifier(app.PrefixIndex1, app.RangeCount8),
			Range:     app.Range{Spec: app.RangeCount8, Count: 1},
			Data:      []byte{2, 0x2C, 0x01, 0x00}, // index 2, value 300, status 0
		})

	var s transport.Segmenter
	seg := s.SegmentAll(frag)[0]
	h := link.Header{
		Control: link.Control{Dir: true, Prm: true, Func: link.FuncUnconfirmedUserData},
		Dest:    10, Src: 1, Length: uint8(link.MinLength + len(seg)),
	}
	wire, _ := link.Encode(nil, h, seg)

	tr, _, err := DecodeFrame(nil, wire)
	if err != nil {
		t.Fatal(err)
	}
	v := tr.App.Values[0][0]
	if v.Index != 2 {
		t.Errorf("index = %d, want 2", v.Index)
	}
	if !strings.Contains(v.Value, "300") || !strings.Contains(v.Value, "int16") {
		t.Errorf("command text = %q", v.Value)
	}
}

// TestClassHeadersDecodeNoValues confirms the decoder stays quiet about
// headers that carry nothing, rather than reporting them as errors.
func TestClassHeadersDecodeNoValues(t *testing.T) {
	frag := app.BuildRequest(nil,
		app.Control{Fir: true, Fin: true}, app.FuncRead,
		app.ReadAllObjects(60, 2), app.ReadAllObjects(60, 1))

	var s transport.Segmenter
	seg := s.SegmentAll(frag)[0]
	h := link.Header{
		Control: link.Control{Dir: true, Prm: true, Func: link.FuncUnconfirmedUserData},
		Dest:    10, Src: 1, Length: uint8(link.MinLength + len(seg)),
	}
	wire, _ := link.Encode(nil, h, seg)

	tr, _, err := DecodeFrame(nil, wire)
	if err != nil {
		t.Fatal(err)
	}
	for i, vals := range tr.App.Values {
		if len(vals) != 0 {
			t.Errorf("class header %d produced %d values", i, len(vals))
		}
	}
}

func TestSynchronizedControlsTimestampQuality(t *testing.T) {
	when := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	header := app.ObjectHeader{
		Group: 2, Variation: 2,
		Qualifier: app.MakeQualifier(app.PrefixIndex1, app.RangeCount8),
		Range:     app.Range{Spec: app.RangeCount8, Count: 1},
		Data:      append([]byte{7, 0x81}, timeOctets(when)...),
	}
	frag := app.BuildResponse(nil, app.Control{Fir: true, Fin: true},
		app.FuncResponse, app.IINNeedTime, header)

	var s transport.Segmenter
	seg := s.SegmentAll(frag)[0]
	h := link.Header{
		Control: link.Control{Prm: true, Func: link.FuncUnconfirmedUserData},
		Dest:    1, Src: 10, Length: uint8(link.MinLength + len(seg)),
	}
	wire, _ := link.Encode(nil, h, seg)

	// An outstation asserting NEED_TIME has an unset clock, so its timestamps
	// must not be filed as synchronized.
	d := New(DirRx, nil)
	d.SetSynchronized(false)
	var got dnp3.TimestampQuality
	d.Feed(wire, func(tr Trace) { got = tr.App.Values[0][0].Time.Quality })

	if got != dnp3.TimestampUnsynchronized {
		t.Errorf("quality = %v, want unsynchronized", got)
	}
}

func TestValueString(t *testing.T) {
	v := Value{Index: 3, Type: dnp3.TypeBinary, Value: "ON", Flags: dnp3.Online | dnp3.StateBit}
	got := v.String()
	if !strings.Contains(got, "[3] ON") {
		t.Errorf("String() = %q", got)
	}
	// The state is already spelled out, so repeating it as a flag is noise.
	if strings.Contains(got, "STATE") {
		t.Errorf("String() = %q, should not repeat the state bit as a flag", got)
	}
	if !strings.Contains(got, "ONLINE") {
		t.Errorf("String() = %q, should keep the quality flags", got)
	}
}

func TestFormatFloat(t *testing.T) {
	tests := []struct {
		in   float64
		want string
	}{
		{300, "300"},
		{-300, "-300"},
		{0, "0"},
		{1.5, "1.5"},
		{13.7521, "13.7521"},
		{0.1, "0.1"},
	}
	for _, tc := range tests {
		if got := formatFloat(tc.in); got != tc.want {
			t.Errorf("formatFloat(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestDecodeValuesRejectsUnknownGroup(t *testing.T) {
	h := app.ObjectHeader{Group: 99, Variation: 1, Data: []byte{1, 2, 3}}
	if _, ok := DecodeValues(h, objects.Context{}); ok {
		t.Error("an unknown group should not report decoded values")
	}
}

// timeOctets encodes an instant as DNP3's six little-endian octets.
func timeOctets(t time.Time) []byte {
	ms := dnp3.TimeToDNP3(t)
	return []byte{
		byte(ms), byte(ms >> 8), byte(ms >> 16),
		byte(ms >> 24), byte(ms >> 32), byte(ms >> 40),
	}
}
