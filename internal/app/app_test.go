package app

import (
	"bytes"
	"errors"
	"testing"
)

func TestControlRoundTrip(t *testing.T) {
	for i := range 256 {
		b := byte(i)
		if got := ParseControl(b).Byte(); got != b {
			t.Fatalf("control %#02x round-tripped to %#02x", b, got)
		}
	}
}

func TestControlFields(t *testing.T) {
	tests := []struct {
		b    byte
		want Control
	}{
		{0xC0, Control{Fir: true, Fin: true, Seq: 0}},
		{0xC1, Control{Fir: true, Fin: true, Seq: 1}},
		{0xE2, Control{Fir: true, Fin: true, Con: true, Seq: 2}},
		{0xF3, Control{Fir: true, Fin: true, Con: true, Uns: true, Seq: 3}},
		{0x8F, Control{Fir: true, Seq: 15}},
	}
	for _, tc := range tests {
		if got := ParseControl(tc.b); got != tc.want {
			t.Errorf("ParseControl(%#02x) = %+v, want %+v", tc.b, got, tc.want)
		}
	}
}

func TestIINRoundTrip(t *testing.T) {
	for i := range 256 {
		for j := range 256 {
			iin := ParseIIN(byte(i), byte(j))
			a, b := iin.Octets()
			if a != byte(i) || b != byte(j) {
				t.Fatalf("IIN(%#02x,%#02x) round-tripped to (%#02x,%#02x)", i, j, a, b)
			}
		}
	}
}

func TestIINSemantics(t *testing.T) {
	// A restarted outstation with class 1 events waiting and a full buffer.
	iin := IINDeviceRestart | IINClass1Events | IINEventBufferOverflow

	if !iin.Has(IINDeviceRestart) {
		t.Error("DEVICE_RESTART not detected")
	}
	if !iin.HasEvents() {
		t.Error("event classes not detected")
	}
	if iin.EventClasses() != IINClass1Events {
		t.Errorf("EventClasses = %v, want class 1 only", iin.EventClasses())
	}
	// EVENT_BUFFER_OVERFLOW is an outstation condition, not a rejection of
	// the request, so it must not count as a request error.
	if iin.HasError() {
		t.Error("a buffer overflow is not a request error")
	}
	if !(iin | IINParameterError).HasError() {
		t.Error("PARAMETER_ERROR should be a request error")
	}

	got := iin.String()
	for _, want := range []string{"CLASS_1_EVENTS", "DEVICE_RESTART", "EVENT_BUFFER_OVERFLOW"} {
		if !bytes.Contains([]byte(got), []byte(want)) {
			t.Errorf("String() = %q, missing %s", got, want)
		}
	}
	if IIN(0).String() != "—" {
		t.Error("an empty IIN should render as an em dash")
	}
}

func TestIINOctetOrder(t *testing.T) {
	// IIN1 is the low octet and travels first. Swapping them would make an
	// outstation's restart flag read as a config-corrupt flag.
	iin := ParseIIN(0x80, 0x08) // DEVICE_RESTART, EVENT_BUFFER_OVERFLOW
	if !iin.Has(IINDeviceRestart) {
		t.Error("first octet is not IIN1")
	}
	if !iin.Has(IINEventBufferOverflow) {
		t.Error("second octet is not IIN2")
	}
}

func TestFuncCodeClassification(t *testing.T) {
	tests := []struct {
		fc       FuncCode
		response bool
		noReply  bool
		control  bool
		name     string
	}{
		{FuncRead, false, false, false, "READ"},
		{FuncResponse, true, false, false, "RESPONSE"},
		{FuncUnsolicitedResponse, true, false, false, "UNSOLICITED_RESPONSE"},
		{FuncDirectOperate, false, false, true, "DIRECT_OPERATE"},
		{FuncDirectOperateNR, false, true, true, "DIRECT_OPERATE_NR"},
		{FuncSelect, false, false, true, "SELECT"},
		{FuncOperate, false, false, true, "OPERATE"},
		{FuncImmedFreezeNR, false, true, false, "IMMED_FREEZE_NR"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.fc.IsResponse(); got != tc.response {
				t.Errorf("IsResponse() = %v, want %v", got, tc.response)
			}
			if got := tc.fc.NoReply(); got != tc.noReply {
				t.Errorf("NoReply() = %v, want %v", got, tc.noReply)
			}
			if got := tc.fc.IsControl(); got != tc.control {
				t.Errorf("IsControl() = %v, want %v", got, tc.control)
			}
			if got := tc.fc.String(); got != tc.name {
				t.Errorf("String() = %q, want %q", got, tc.name)
			}
		})
	}
	if FuncCode(200).IsKnown() {
		t.Error("200 is not a defined function code")
	}
	if got := FuncCode(200).String(); got != "FUNC_200" {
		t.Errorf("unknown code = %q", got)
	}
}

func TestHeaderRequestVsResponse(t *testing.T) {
	// A request header is two octets; a response adds the IIN. Getting the
	// length wrong shifts every object header that follows.
	req := []byte{0xC0, 0x01}
	h, n, err := ParseHeader(req)
	if err != nil {
		t.Fatal(err)
	}
	if n != RequestHeaderSize {
		t.Errorf("request header consumed %d octets, want %d", n, RequestHeaderSize)
	}
	if h.Func != FuncRead || h.IIN != 0 {
		t.Errorf("header = %+v", h)
	}

	resp := []byte{0xC0, 0x81, 0x80, 0x00}
	h, n, err = ParseHeader(resp)
	if err != nil {
		t.Fatal(err)
	}
	if n != ResponseHeaderSize {
		t.Errorf("response header consumed %d octets, want %d", n, ResponseHeaderSize)
	}
	if h.Func != FuncResponse {
		t.Errorf("func = %v, want RESPONSE", h.Func)
	}
	if !h.IIN.Has(IINDeviceRestart) {
		t.Errorf("IIN = %v, want DEVICE_RESTART", h.IIN)
	}
}

func TestParseHeaderShort(t *testing.T) {
	for _, buf := range [][]byte{nil, {0xC0}, {0xC0, 0x81}, {0xC0, 0x81, 0x00}} {
		if _, _, err := ParseHeader(buf); !errors.Is(err, ErrShortFragment) {
			t.Errorf("ParseHeader(% x) err = %v, want ErrShortFragment", buf, err)
		}
	}
}

func TestQualifierFields(t *testing.T) {
	tests := []struct {
		q      Qualifier
		prefix IndexPrefix
		spec   RangeSpec
	}{
		{0x00, PrefixNone, RangeStartStop8},
		{0x01, PrefixNone, RangeStartStop16},
		{0x06, PrefixNone, RangeAllObjects},
		{0x07, PrefixNone, RangeCount8},
		{0x17, PrefixIndex1, RangeCount8},
		{0x28, PrefixIndex2, RangeCount16},
		{0x5B, PrefixSize2, RangeVariable},
	}
	for _, tc := range tests {
		if got := tc.q.IndexPrefix(); got != tc.prefix {
			t.Errorf("%#02x prefix = %v, want %v", uint8(tc.q), got, tc.prefix)
		}
		if got := tc.q.RangeSpec(); got != tc.spec {
			t.Errorf("%#02x range = %v, want %v", uint8(tc.q), got, tc.spec)
		}
		if got := MakeQualifier(tc.prefix, tc.spec); got != tc.q {
			t.Errorf("MakeQualifier(%v,%v) = %#02x, want %#02x", tc.prefix, tc.spec, uint8(got), uint8(tc.q))
		}
	}
}

func TestRangeOctets(t *testing.T) {
	tests := []struct {
		spec RangeSpec
		want int
	}{
		{RangeStartStop8, 2}, {RangeStartStop16, 4}, {RangeStartStop32, 8},
		{RangeAllObjects, 0},
		{RangeCount8, 1}, {RangeCount16, 2}, {RangeCount32, 4},
		{RangeVariable, 1},
	}
	for _, tc := range tests {
		if got := tc.spec.Octets(); got != tc.want {
			t.Errorf("%v.Octets() = %d, want %d", tc.spec, got, tc.want)
		}
	}
}

func TestParseRangeDerivesCount(t *testing.T) {
	tests := []struct {
		name  string
		spec  RangeSpec
		buf   []byte
		start uint32
		stop  uint32
		count uint32
	}{
		{"8-bit 0..9", RangeStartStop8, []byte{0, 9}, 0, 9, 10},
		{"8-bit single point", RangeStartStop8, []byte{5, 5}, 5, 5, 1},
		{"16-bit 0..999", RangeStartStop16, []byte{0x00, 0x00, 0xE7, 0x03}, 0, 999, 1000},
		{"count8", RangeCount8, []byte{7}, 0, 0, 7},
		{"count16", RangeCount16, []byte{0x00, 0x01}, 0, 0, 256},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, n, err := parseRange(tc.spec, tc.buf)
			if err != nil {
				t.Fatal(err)
			}
			if n != tc.spec.Octets() {
				t.Errorf("consumed %d octets, want %d", n, tc.spec.Octets())
			}
			if r.Start != tc.start || r.Stop != tc.stop || r.Count != tc.count {
				t.Errorf("range = %+v, want start=%d stop=%d count=%d", r, tc.start, tc.stop, tc.count)
			}
		})
	}
}

func TestParseRangeRejectsStopBelowStart(t *testing.T) {
	// A stop below the start is not a zero-length range, it is malformed.
	// Treating it as zero-length would silently skip objects the peer sent.
	if _, _, err := parseRange(RangeStartStop8, []byte{9, 3}); !errors.Is(err, ErrBadRange) {
		t.Errorf("err = %v, want ErrBadRange", err)
	}
}

func TestParseRangeFullThirtyTwoBitRange(t *testing.T) {
	// 0..0xFFFFFFFF is 2^32 objects, which overflows the uint32 count. It must
	// be rejected rather than wrapping to zero.
	buf := []byte{0, 0, 0, 0, 0xFF, 0xFF, 0xFF, 0xFF}
	if _, _, err := parseRange(RangeStartStop32, buf); !errors.Is(err, ErrBadRange) {
		t.Errorf("err = %v, want ErrBadRange for a 2^32 object range", err)
	}
}

func TestParseRangeTruncated(t *testing.T) {
	if _, _, err := parseRange(RangeStartStop16, []byte{0, 0}); !errors.Is(err, ErrTruncated) {
		t.Errorf("err = %v, want ErrTruncated", err)
	}
}

// ---------- Whole fragments ----------

func TestParseIntegrityPollRequest(t *testing.T) {
	// The request every master sends first: read class 1, 2, 3 then 0, each
	// as an all-objects header of group 60.
	frag := []byte{
		0xC0, 0x01, // FIR|FIN seq 0, READ
		60, 2, 0x06,
		60, 3, 0x06,
		60, 4, 0x06,
		60, 1, 0x06,
	}

	f, err := ParseRequest(nil, frag)
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}
	if f.Header.Func != FuncRead {
		t.Errorf("func = %v, want READ", f.Header.Func)
	}
	if len(f.Objects) != 4 {
		t.Fatalf("got %d object headers, want 4", len(f.Objects))
	}
	for i, want := range []uint8{2, 3, 4, 1} {
		o := f.Objects[i]
		if o.Group != 60 || o.Variation != want {
			t.Errorf("header %d = g%dv%d, want g60v%d", i, o.Group, o.Variation, want)
		}
		if o.Qualifier.RangeSpec() != RangeAllObjects {
			t.Errorf("header %d qualifier = %v, want all-objects", i, o.Qualifier)
		}
		if len(o.Data) != 0 {
			t.Errorf("header %d carries %d octets of data, want none", i, len(o.Data))
		}
	}
}

func TestParseBinaryInputResponse(t *testing.T) {
	// A response carrying four group 1 variation 2 binary inputs over indexes
	// 0..3, each a single flags octet.
	frag := []byte{
		0xC1, 0x81, 0x00, 0x00, // FIR|FIN seq 1, RESPONSE, no indications
		1, 2, 0x00, // g1v2, 8-bit start/stop
		0, 3, // indexes 0..3
		0x01, 0x81, 0x01, 0x81, // four flags octets
	}

	f, err := ParseResponse(nil, frag)
	if err != nil {
		t.Fatalf("ParseResponse: %v", err)
	}
	if len(f.Objects) != 1 {
		t.Fatalf("got %d object headers, want 1", len(f.Objects))
	}
	o := f.Objects[0]
	if o.Count() != 4 {
		t.Errorf("count = %d, want 4", o.Count())
	}
	if len(o.Data) != 4 {
		t.Errorf("data = %d octets, want 4", len(o.Data))
	}
	if !bytes.Equal(o.Data, []byte{0x01, 0x81, 0x01, 0x81}) {
		t.Errorf("data = % x", o.Data)
	}
	if o.Range.IndexOf(2) != 2 {
		t.Errorf("IndexOf(2) = %d, want 2", o.Range.IndexOf(2))
	}
}

// TestParsePackedBinaryInputs covers the bit-packed case: group 1 variation 1
// puts one point per bit, so ten points occupy two octets rather than ten.
func TestParsePackedBinaryInputs(t *testing.T) {
	frag := []byte{
		0xC0, 0x81, 0x00, 0x00,
		1, 1, 0x00, // g1v1, packed
		0, 9, // ten points, indexes 0..9
		0b10101010, 0b00000011, // two octets hold all ten bits
	}

	f, err := ParseFragment(nil, frag)
	if err != nil {
		t.Fatalf("ParseFragment: %v", err)
	}
	o := f.Objects[0]
	if o.Count() != 10 {
		t.Errorf("count = %d, want 10", o.Count())
	}
	if len(o.Data) != 2 {
		t.Errorf("packed data = %d octets, want 2 for ten one-bit points", len(o.Data))
	}
}

func TestParseAnalogEventsWithIndexPrefix(t *testing.T) {
	// Events come back with per-object index prefixes and a count, since the
	// indexes that changed are not contiguous.
	frag := []byte{
		0xC0, 0x81, 0x02, 0x00, // RESPONSE, class 1 events available
		32, 2, 0x17, // g32v2 (flags + 16-bit), 1-octet index prefix, 1-octet count
		2,                // two objects
		5, 0x01, 0x64, 0, // index 5, flags online, value 100
		9, 0x01, 0xC8, 0, // index 9, flags online, value 200
	}

	f, err := ParseFragment(nil, frag)
	if err != nil {
		t.Fatalf("ParseFragment: %v", err)
	}
	o := f.Objects[0]
	if o.Count() != 2 {
		t.Errorf("count = %d, want 2", o.Count())
	}
	// Each object is a one-octet index prefix plus three octets of data.
	if len(o.Data) != 8 {
		t.Errorf("data = %d octets, want 8", len(o.Data))
	}
	if !o.Qualifier.IndexPrefix().IsIndex() {
		t.Error("qualifier should carry an index prefix")
	}
}

func TestParseCROBRequest(t *testing.T) {
	// A direct-operate of a latch-on CROB at index 3.
	frag := []byte{
		0xC0, 0x05, // DIRECT_OPERATE
		12, 1, 0x17, // g12v1, 1-octet index prefix, 1-octet count
		1,                      // one object
		3,                      // index 3
		0x03,                   // LATCH_ON
		1,                      // count
		0xE8, 0x03, 0x00, 0x00, // on time 1000ms
		0x00, 0x00, 0x00, 0x00, // off time
		0x00, // status
	}

	f, err := ParseRequest(nil, frag)
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}
	o := f.Objects[0]
	if o.Group != 12 || o.Variation != 1 {
		t.Errorf("object = g%dv%d, want g12v1", o.Group, o.Variation)
	}
	// One index octet plus eleven octets of CROB.
	if len(o.Data) != 12 {
		t.Errorf("data = %d octets, want 12", len(o.Data))
	}
}

func TestParseMultipleObjectHeaders(t *testing.T) {
	// A realistic integrity response: binaries, then counters, then analogs.
	frag := []byte{0xC0, 0x81, 0x00, 0x00}
	frag = append(frag, 1, 2, 0x00, 0, 1, 0x01, 0x01)                // 2 binaries, 1 octet each
	frag = append(frag, 20, 1, 0x00, 0, 0, 0x01, 1, 0, 0, 0)         // 1 counter, 5 octets
	frag = append(frag, 30, 2, 0x00, 0, 1, 0x01, 10, 0, 0x01, 20, 0) // 2 analogs, 3 octets each

	f, err := ParseFragment(nil, frag)
	if err != nil {
		t.Fatalf("ParseFragment: %v", err)
	}
	if len(f.Objects) != 3 {
		t.Fatalf("got %d object headers, want 3", len(f.Objects))
	}
	for i, want := range []uint8{1, 20, 30} {
		if f.Objects[i].Group != want {
			t.Errorf("header %d = group %d, want %d", i, f.Objects[i].Group, want)
		}
	}
	// Offsets must point back at the original octets, or a hex viewer
	// highlights the wrong bytes.
	if f.Objects[0].Offset != 4 {
		t.Errorf("first object header offset = %d, want 4", f.Objects[0].Offset)
	}
	if f.Objects[1].Offset != 11 {
		t.Errorf("second object header offset = %d, want 11", f.Objects[1].Offset)
	}
}

// TestReadHeaderCarriesNoData pins the asymmetry that the same object header
// means different things in a request and a response. In a READ it names
// sixteen analog points; in a RESPONSE it introduces their values. A parser
// that sized the READ from the object table would run off the end of every
// read request it ever saw.
func TestReadHeaderCarriesNoData(t *testing.T) {
	header := []byte{30, 2, 0x00, 0, 15} // g30v2, indexes 0..15

	read, err := ParseRequest(nil, append([]byte{0xC0, 0x01}, header...))
	if err != nil {
		t.Fatalf("READ request: %v", err)
	}
	if o := read.Objects[0]; len(o.Data) != 0 {
		t.Errorf("READ header carries %d octets of data, want none", len(o.Data))
	}
	if read.Objects[0].Count() != 16 {
		t.Errorf("count = %d, want 16", read.Objects[0].Count())
	}

	// The same header in a response introduces 16 × 3 octets of analog data.
	body := append([]byte{0xC0, 0x81, 0, 0}, header...)
	body = append(body, make([]byte, 16*3)...)
	resp, err := ParseResponse(nil, body)
	if err != nil {
		t.Fatalf("RESPONSE: %v", err)
	}
	if got := len(resp.Objects[0].Data); got != 48 {
		t.Errorf("RESPONSE header carries %d octets, want 48", got)
	}
}

func TestCarriesObjectData(t *testing.T) {
	tests := []struct {
		fc   FuncCode
		want bool
	}{
		{FuncRead, false},
		{FuncAssignClass, false},
		{FuncEnableUnsolicited, false},
		{FuncDisableUnsolicited, false},
		{FuncImmedFreeze, false},
		{FuncConfirm, false},
		{FuncWrite, true},
		{FuncDirectOperate, true},
		{FuncSelect, true},
		{FuncResponse, true},
		{FuncUnsolicitedResponse, true},
	}
	for _, tc := range tests {
		if got := tc.fc.CarriesObjectData(); got != tc.want {
			t.Errorf("%v.CarriesObjectData() = %v, want %v", tc.fc, got, tc.want)
		}
	}
}

// TestClassPollHeadersInRequests covers the enable-unsolicited request, whose
// group 60 headers name classes rather than carrying them.
func TestClassPollHeadersInRequests(t *testing.T) {
	frag := []byte{0xC0, 0x14, 60, 2, 0x06, 60, 3, 0x06, 60, 4, 0x06}

	f, err := ParseRequest(nil, frag)
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}
	if f.Header.Func != FuncEnableUnsolicited {
		t.Errorf("func = %v, want ENABLE_UNSOLICITED", f.Header.Func)
	}
	if len(f.Objects) != 3 {
		t.Errorf("got %d headers, want 3", len(f.Objects))
	}
}

func TestParseFragmentErrors(t *testing.T) {
	tests := []struct {
		name string
		frag []byte
		want error
	}{
		{
			name: "truncated object header",
			frag: []byte{0xC0, 0x01, 60, 1},
			want: ErrTruncated,
		},
		{
			name: "data runs past the fragment",
			frag: []byte{0xC0, 0x81, 0, 0, 1, 2, 0x00, 0, 9, 0x01},
			want: ErrTruncated,
		},
		{
			name: "reserved qualifier bit",
			frag: []byte{0xC0, 0x01, 60, 1, 0x86},
			want: ErrBadQualifier,
		},
		{
			name: "reserved range specifier",
			frag: []byte{0xC0, 0x01, 60, 1, 0x0A},
			want: ErrBadQualifier,
		},
		{
			name: "reserved index prefix",
			frag: []byte{0xC0, 0x01, 60, 1, 0x70},
			want: ErrBadQualifier,
		},
		{
			name: "stop below start",
			frag: []byte{0xC0, 0x81, 0, 0, 1, 2, 0x00, 9, 0},
			want: ErrBadRange,
		},
		{
			name: "unknown group",
			frag: []byte{0xC0, 0x81, 0, 0, 99, 7, 0x00, 0, 1, 0xAA, 0xBB},
			want: ErrUnknownObject,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseFragment(nil, tc.frag)
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestParseFragmentReturnsPartialProgress(t *testing.T) {
	// A decoder is more useful showing the headers it understood before the
	// corruption than showing nothing at all.
	//
	// This has to be a response: in a READ the headers carry no data, so an
	// unrecognised group costs nothing and the parse succeeds.
	frag := []byte{0xC0, 0x81, 0x00, 0x00}
	frag = append(frag, 1, 2, 0x00, 0, 0, 0x01)         // one binary
	frag = append(frag, 30, 2, 0x00, 0, 0, 0x01, 10, 0) // one analog
	frag = append(frag, 99, 7, 0x00, 0, 1, 0xAA, 0xBB)  // unknown group

	f, err := ParseFragment(nil, frag)
	if err == nil {
		t.Fatal("expected an error for the unknown group")
	}
	if len(f.Objects) != 2 {
		t.Errorf("recovered %d headers before the error, want 2", len(f.Objects))
	}
}

func TestPackedObjectRejectsIndexPrefix(t *testing.T) {
	// Bit-packed objects share octets, so a per-object index prefix cannot be
	// expressed alongside them. Accepting it would compute a nonsense length.
	frag := []byte{0xC0, 0x81, 0, 0, 1, 1, 0x17, 2, 0, 1}
	if _, err := ParseFragment(nil, frag); !errors.Is(err, ErrBadQualifier) {
		t.Errorf("err = %v, want ErrBadQualifier", err)
	}
}

func TestParseSizePrefixedObjects(t *testing.T) {
	// A size prefix makes objects self-describing, so a parser walks them
	// without knowing the group at all — which is how file transfer objects
	// are carried.
	frag := []byte{
		0xC0, 0x81, 0, 0,
		70, 3, 0x5B, // g70v3, 2-octet size prefix, variable count
		2,                   // two objects
		0x03, 0x00, 1, 2, 3, // three octets
		0x02, 0x00, 4, 5, // two octets
	}

	f, err := ParseFragment(nil, frag)
	if err != nil {
		t.Fatalf("ParseFragment: %v", err)
	}
	o := f.Objects[0]
	if len(o.Data) != 9 {
		t.Errorf("data = %d octets, want 9", len(o.Data))
	}
	if !o.Qualifier.IndexPrefix().IsSize() {
		t.Error("qualifier should carry a size prefix")
	}
}

// TestFreeFormatInARequest covers the exception file transfer needs: a READ
// request carries no object data by the general rule, and a read of a file
// block carries a group 70 object holding the handle and the block number.
//
// Before the free-format case was hoisted above the function-code rule, the
// object's own octets were parsed as the next object header, and every file
// read a master sent was rejected as a malformed fragment.
func TestFreeFormatInARequest(t *testing.T) {
	for _, c := range []struct {
		name string
		fn   FuncCode
	}{
		{"read a file block", FuncRead},
		{"delete a file", FuncDeleteFile},
		{"file info", FuncGetFileInfo},
	} {
		t.Run(c.name, func(t *testing.T) {
			frag := []byte{
				0xC0, byte(c.fn),
				70, 5, 0x5B, // g70v5, 2-octet size prefix, variable count
				1,          // one object
				0x08, 0x00, // eight octets
				1, 0, 0, 0, // handle
				2, 0, 0, 0, // block number
			}

			f, err := ParseFragment(nil, frag)
			if err != nil {
				t.Fatalf("ParseFragment: %v", err)
			}
			if len(f.Objects) != 1 {
				t.Fatalf("parsed %d object headers, want 1", len(f.Objects))
			}
			if got := len(f.Objects[0].Data); got != 10 {
				t.Errorf("data = %d octets, want 10 (the size prefix and the object)", got)
			}
		})
	}
}

// TestReadRequestStillCarriesNoData is the other half of that exception: an
// ordinary read names points and must not be given data, or the parser runs
// off the end of every poll.
func TestReadRequestStillCarriesNoData(t *testing.T) {
	// A class 0 poll followed by a second header. If the first were treated as
	// carrying data, the second would be swallowed as its payload.
	frag := []byte{
		0xC0, byte(FuncRead),
		60, 1, 0x06,
		60, 2, 0x06,
	}
	f, err := ParseFragment(nil, frag)
	if err != nil {
		t.Fatalf("ParseFragment: %v", err)
	}
	if len(f.Objects) != 2 {
		t.Fatalf("parsed %d object headers, want 2", len(f.Objects))
	}
	for i, o := range f.Objects {
		if len(o.Data) != 0 {
			t.Errorf("header %d carries %d octets of data, want none", i, len(o.Data))
		}
	}
}

func TestSizePrefixedTruncation(t *testing.T) {
	// A size prefix claiming more octets than remain must be rejected, not
	// clamped — this is the classic length-field overflow.
	frag := []byte{0xC0, 0x81, 0, 0, 70, 3, 0x5B, 1, 0xFF, 0xFF, 1, 2}
	if _, err := ParseFragment(nil, frag); !errors.Is(err, ErrTruncated) {
		t.Errorf("err = %v, want ErrTruncated", err)
	}
}

func TestOctetStringSizeFromVariation(t *testing.T) {
	// For group 110 the variation is the string length, so no size table
	// lookup is needed.
	bits, ok := SpecSizer{}.SizeBits(110, 5)
	if !ok || bits != 40 {
		t.Errorf("g110v5 = %d bits (ok=%v), want 40", bits, ok)
	}

	frag := []byte{
		0xC0, 0x81, 0, 0,
		110, 3, 0x00, // g110v3: three-octet strings
		0, 1, // two of them
		'a', 'b', 'c', 'd', 'e', 'f',
	}
	f, err := ParseFragment(nil, frag)
	if err != nil {
		t.Fatalf("ParseFragment: %v", err)
	}
	if len(f.Objects[0].Data) != 6 {
		t.Errorf("data = %d octets, want 6", len(f.Objects[0].Data))
	}
}

func TestVariableLengthGroupsReportUnknown(t *testing.T) {
	// Device attributes and file objects are genuinely variable-length. The
	// sizer must say so rather than guess a size.
	for _, g := range []uint8{0, 70} {
		if _, ok := (SpecSizer{}).SizeBits(g, 1); ok {
			t.Errorf("group %d should report an unknown size", g)
		}
	}
}

// ---------- Building ----------

func TestBuildAndParseRoundTrip(t *testing.T) {
	built := BuildRequest(nil,
		Control{Fir: true, Fin: true, Seq: 3},
		FuncRead,
		ReadAllObjects(60, 2),
		ReadAllObjects(60, 1),
		ReadRange(30, 2, 0, 15),
	)

	f, err := ParseRequest(nil, built)
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}
	if f.Header.Control.Seq != 3 || f.Header.Func != FuncRead {
		t.Errorf("header = %v", f.Header)
	}
	if len(f.Objects) != 3 {
		t.Fatalf("got %d headers, want 3", len(f.Objects))
	}
	if o := f.Objects[2]; o.Range.Start != 0 || o.Range.Stop != 15 || o.Count() != 16 {
		t.Errorf("range header = %+v", o.Range)
	}

	// Re-encoding the parsed fragment must reproduce the original octets.
	again := BuildRequest(nil, f.Header.Control, f.Header.Func, f.Objects...)
	if !bytes.Equal(again, built) {
		t.Errorf("re-encoded = % x\noriginal     = % x", again, built)
	}
}

func TestReadRangePicksNarrowestEncoding(t *testing.T) {
	tests := []struct {
		stop uint32
		want RangeSpec
	}{
		{15, RangeStartStop8},
		{255, RangeStartStop8},
		{256, RangeStartStop16},
		{65535, RangeStartStop16},
		{65536, RangeStartStop32},
	}
	for _, tc := range tests {
		if got := ReadRange(30, 2, 0, tc.stop).Qualifier.RangeSpec(); got != tc.want {
			t.Errorf("stop=%d gave %v, want %v", tc.stop, got, tc.want)
		}
	}
}

func TestBuilderEnforcesMaxFragment(t *testing.T) {
	b := NewBuilder(20)
	if err := b.SetHeader(Header{Control: Control{Fir: true, Fin: true}, Func: FuncResponse}); err != nil {
		t.Fatal(err)
	}

	big := ObjectHeader{
		Group: 1, Variation: 2,
		Qualifier: MakeQualifier(PrefixNone, RangeStartStop8),
		Range:     Range{Spec: RangeStartStop8, Start: 0, Stop: 99, Count: 100},
		Data:      make([]byte, 100),
	}
	if b.Fits(big) {
		t.Error("Fits should report false for an oversize object")
	}
	if err := b.AddObject(big); !errors.Is(err, ErrFragmentTooLarge) {
		t.Errorf("err = %v, want ErrFragmentTooLarge", err)
	}
	// The rejection must leave the builder usable, so the caller can close
	// this fragment and start the next.
	if b.Len() != ResponseHeaderSize {
		t.Errorf("a rejected object changed the builder: len = %d", b.Len())
	}

	small := ObjectHeader{
		Group: 60, Variation: 1,
		Qualifier: MakeQualifier(PrefixNone, RangeAllObjects),
		Range:     Range{Spec: RangeAllObjects},
	}
	if err := b.AddObject(small); err != nil {
		t.Errorf("adding a small object after a rejection: %v", err)
	}
}

func TestBuilderRejectsObjectBeforeHeader(t *testing.T) {
	b := NewBuilder(0)
	if err := b.AddObject(ReadAllObjects(60, 1)); err == nil {
		t.Error("an object header before the fragment header should fail")
	}
}

func TestBuilderReset(t *testing.T) {
	b := NewBuilder(0)
	if err := b.SetHeader(Header{Func: FuncRead}); err != nil {
		t.Fatal(err)
	}
	if err := b.SetHeader(Header{Func: FuncRead}); err == nil {
		t.Error("a second SetHeader should fail")
	}
	b.Reset()
	if b.Len() != 0 {
		t.Errorf("Len after Reset = %d, want 0", b.Len())
	}
	if err := b.SetHeader(Header{Func: FuncRead}); err != nil {
		t.Errorf("SetHeader after Reset: %v", err)
	}
}

func TestBuildResponseCarriesIIN(t *testing.T) {
	built := BuildResponse(nil,
		Control{Fir: true, Fin: true, Con: true, Seq: 5},
		FuncResponse,
		IINDeviceRestart|IINClass1Events,
	)
	f, err := ParseResponse(nil, built)
	if err != nil {
		t.Fatal(err)
	}
	if !f.Header.IIN.Has(IINDeviceRestart | IINClass1Events) {
		t.Errorf("IIN = %v", f.Header.IIN)
	}
	if !f.Header.Control.Con {
		t.Error("CON was not preserved")
	}
}

func BenchmarkParseFragment(b *testing.B) {
	frag := []byte{0xC0, 0x81, 0x00, 0x00}
	frag = append(frag, 1, 2, 0x00, 0, 99)
	frag = append(frag, make([]byte, 100)...)
	frag = append(frag, 30, 2, 0x00, 0, 49)
	frag = append(frag, make([]byte, 150)...)

	b.SetBytes(int64(len(frag)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := ParseFragment(nil, frag); err != nil {
			b.Fatal(err)
		}
	}
}
