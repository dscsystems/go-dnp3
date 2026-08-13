package decoder

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/internal/link"
	"github.com/dscsystems/go-dnp3/internal/transport"
)

var update = flag.Bool("update", false, "regenerate testdata/sample.hex")

// frame wraps a payload in a link frame.
func frame(t *testing.T, master bool, fn link.Function, dest, src uint16, payload []byte) []byte {
	t.Helper()
	h := link.Header{
		Control: link.Control{Dir: master, Prm: true, Func: fn},
		Dest:    dest, Src: src,
		Length: uint8(link.MinLength + len(payload)),
	}
	b, err := link.Encode(nil, h, payload)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// segment wraps an application fragment in a single transport segment.
func segment(t *testing.T, frag []byte) []byte {
	t.Helper()
	var s transport.Segmenter
	all := s.SegmentAll(frag)
	if len(all) != 1 {
		t.Fatalf("fragment needed %d segments, want 1", len(all))
	}
	return all[0]
}

// integrityRequest is the poll every master sends first.
func integrityRequest(t *testing.T) []byte {
	t.Helper()
	frag := app.BuildRequest(nil,
		app.Control{Fir: true, Fin: true, Seq: 0}, app.FuncRead,
		app.ReadAllObjects(60, 2), app.ReadAllObjects(60, 3),
		app.ReadAllObjects(60, 4), app.ReadAllObjects(60, 1))
	return frame(t, true, link.FuncUnconfirmedUserData, 10, 1, segment(t, frag))
}

// integrityResponse is a plausible answer: four binaries and two analogs from
// an outstation that has just restarted.
func integrityResponse(t *testing.T) []byte {
	t.Helper()
	frag := app.BuildResponse(nil,
		app.Control{Fir: true, Fin: true, Seq: 0}, app.FuncResponse,
		app.IINDeviceRestart|app.IINClass1Events,
		app.ObjectHeader{
			Group: 1, Variation: 2,
			Qualifier: app.MakeQualifier(app.PrefixNone, app.RangeStartStop8),
			Range:     app.Range{Spec: app.RangeStartStop8, Start: 0, Stop: 3, Count: 4},
			Data:      []byte{0x81, 0x01, 0x81, 0x01},
		},
		app.ObjectHeader{
			Group: 30, Variation: 2,
			Qualifier: app.MakeQualifier(app.PrefixNone, app.RangeStartStop8),
			Range:     app.Range{Spec: app.RangeStartStop8, Start: 0, Stop: 1, Count: 2},
			Data:      []byte{0x01, 0x2C, 0x01, 0x01, 0x90, 0x01},
		})
	return frame(t, false, link.FuncUnconfirmedUserData, 1, 10, segment(t, frag))
}

// crobRequest trips the breaker at index 3.
func crobRequest(t *testing.T) []byte {
	t.Helper()
	frag := app.BuildRequest(nil,
		app.Control{Fir: true, Fin: true, Seq: 1}, app.FuncDirectOperate,
		app.ObjectHeader{
			Group: 12, Variation: 1,
			Qualifier: app.MakeQualifier(app.PrefixIndex1, app.RangeCount8),
			Range:     app.Range{Spec: app.RangeCount8, Count: 1},
			// index 3, PULSE_ON|TRIP, count 1, on 1000ms, off 0, status 0
			Data: []byte{3, 0x41, 1, 0xE8, 0x03, 0, 0, 0, 0, 0, 0, 0},
		})
	return frame(t, true, link.FuncUnconfirmedUserData, 10, 1, segment(t, frag))
}

func TestDecodeIntegrityRequest(t *testing.T) {
	tr, n, err := DecodeFrame(nil, integrityRequest(t))
	if err != nil {
		t.Fatalf("DecodeFrame: %v", err)
	}
	if n == 0 {
		t.Fatal("consumed no octets")
	}

	if tr.Link.Src != 1 || tr.Link.Dest != 10 {
		t.Errorf("link addresses = %d→%d, want 1→10", tr.Link.Src, tr.Link.Dest)
	}
	if tr.Transport == nil {
		t.Fatal("no transport layer decoded")
	}
	if !tr.Transport.Header.Fir || !tr.Transport.Header.Fin {
		t.Errorf("transport header = %v, want FIR and FIN", tr.Transport.Header)
	}
	if tr.App == nil {
		t.Fatal("no application layer decoded")
	}
	if tr.App.Err != nil {
		t.Fatalf("application layer: %v", tr.App.Err)
	}
	if tr.App.Header.Func != app.FuncRead {
		t.Errorf("func = %v, want READ", tr.App.Header.Func)
	}
	if len(tr.App.Objects) != 4 {
		t.Fatalf("got %d object headers, want 4", len(tr.App.Objects))
	}
	for i, want := range []uint8{2, 3, 4, 1} {
		if o := tr.App.Objects[i]; o.Group != 60 || o.Variation != want {
			t.Errorf("header %d = g%dv%d, want g60v%d", i, o.Group, o.Variation, want)
		}
	}
}

func TestDecodeIntegrityResponse(t *testing.T) {
	tr, _, err := DecodeFrame(nil, integrityResponse(t))
	if err != nil {
		t.Fatalf("DecodeFrame: %v", err)
	}
	if tr.App == nil || tr.App.Err != nil {
		t.Fatalf("application layer: %+v", tr.App)
	}

	if !tr.App.Header.IIN.Has(app.IINDeviceRestart) {
		t.Errorf("IIN = %v, want DEVICE_RESTART", tr.App.Header.IIN)
	}
	if len(tr.App.Objects) != 2 {
		t.Fatalf("got %d object headers, want 2", len(tr.App.Objects))
	}
	if o := tr.App.Objects[0]; o.Count() != 4 || len(o.Data) != 4 {
		t.Errorf("binaries = %d objects, %d octets", o.Count(), len(o.Data))
	}
	if o := tr.App.Objects[1]; o.Count() != 2 || len(o.Data) != 6 {
		t.Errorf("analogs = %d objects, %d octets", o.Count(), len(o.Data))
	}
}

func TestDecodeCROB(t *testing.T) {
	tr, _, err := DecodeFrame(nil, crobRequest(t))
	if err != nil {
		t.Fatalf("DecodeFrame: %v", err)
	}
	if tr.App.Header.Func != app.FuncDirectOperate {
		t.Errorf("func = %v, want DIRECT_OPERATE", tr.App.Header.Func)
	}
	o := tr.App.Objects[0]
	if o.Group != 12 || o.Variation != 1 {
		t.Errorf("object = g%dv%d, want g12v1", o.Group, o.Variation)
	}
	if len(o.Data) != 12 {
		t.Errorf("data = %d octets, want 12 (one index octet plus an eleven octet CROB)", len(o.Data))
	}
}

// TestDecodeLinkOnlyFrame confirms a link ACK produces no transport or
// application layer. Inventing one would put phantom fragments in a log.
func TestDecodeLinkOnlyFrame(t *testing.T) {
	h := link.Header{
		Control: link.Control{Prm: false, Func: link.FuncAck},
		Dest:    1, Src: 10, Length: link.MinLength,
	}
	wire, err := link.Encode(nil, h, nil)
	if err != nil {
		t.Fatal(err)
	}

	tr, _, err := DecodeFrame(nil, wire)
	if err != nil {
		t.Fatalf("DecodeFrame: %v", err)
	}
	if tr.Transport != nil {
		t.Error("a link ACK has no transport layer above it")
	}
	if tr.App != nil {
		t.Error("a link ACK has no application layer above it")
	}
}

// TestDecoderReassemblesMultiFrameFragment is the end-to-end proof of the size
// budget: a 2048-octet fragment crosses nine frames, and only the ninth
// completes it.
func TestDecoderReassemblesMultiFrameFragment(t *testing.T) {
	// Build a response big enough to need multiple segments: 400 analog
	// points at three octets each.
	const points = 400
	frag := app.BuildResponse(nil,
		app.Control{Fir: true, Fin: true, Seq: 0}, app.FuncResponse, 0,
		app.ObjectHeader{
			Group: 30, Variation: 2,
			Qualifier: app.MakeQualifier(app.PrefixNone, app.RangeStartStop16),
			Range:     app.Range{Spec: app.RangeStartStop16, Start: 0, Stop: points - 1, Count: points},
			Data:      make([]byte, points*3),
		})

	var s transport.Segmenter
	segs := s.SegmentAll(frag)
	if len(segs) < 3 {
		t.Fatalf("test fragment only needed %d segments; it should span several", len(segs))
	}

	var stream []byte
	for _, seg := range segs {
		stream = append(stream, frame(t, false, link.FuncUnconfirmedUserData, 1, 10, seg)...)
	}

	d := New(DirRx, nil)
	var traces []Trace
	d.Feed(stream, func(tr Trace) { traces = append(traces, tr) })

	if len(traces) != len(segs) {
		t.Fatalf("decoded %d frames, want %d", len(traces), len(segs))
	}
	for i, tr := range traces {
		last := i == len(traces)-1
		if tr.Transport == nil {
			t.Fatalf("frame %d has no transport layer", i)
		}
		if got := tr.App != nil; got != last {
			t.Errorf("frame %d: application layer present = %v, want %v", i, got, last)
		}
	}

	final := traces[len(traces)-1]
	if final.App.Err != nil {
		t.Fatalf("reassembled fragment failed to parse: %v", final.App.Err)
	}
	if o := final.App.Objects[0]; o.Count() != points {
		t.Errorf("reassembled %d points, want %d", o.Count(), points)
	}
}

// TestDecoderFedOneOctetAtATime is the worst case a socket can produce.
func TestDecoderFedOneOctetAtATime(t *testing.T) {
	stream := append(integrityRequest(t), integrityResponse(t)...)

	d := New(DirRx, nil)
	var n int
	for i := range stream {
		d.Feed(stream[i:i+1], func(Trace) { n++ })
	}
	if n != 2 {
		t.Errorf("decoded %d frames, want 2", n)
	}
}

func TestDecoderSurvivesGarbage(t *testing.T) {
	stream := []byte{0xDE, 0xAD, 0xBE, 0xEF}
	stream = append(stream, integrityRequest(t)...)
	stream = append(stream, 0xFF, 0xFF)
	stream = append(stream, integrityResponse(t)...)

	d := New(DirRx, nil)
	var n int
	d.Feed(stream, func(tr Trace) {
		if tr.App != nil && tr.App.Err == nil {
			n++
		}
	})
	if n != 2 {
		t.Errorf("recovered %d good frames from a noisy stream, want 2", n)
	}
}

func TestRenderIncludesEveryLayer(t *testing.T) {
	tr, _, err := DecodeFrame(nil, integrityResponse(t))
	if err != nil {
		t.Fatal(err)
	}

	var b strings.Builder
	tr.Render(&b, true)
	out := b.String()

	for _, want := range []string{
		"link", "transport", "application",
		"RESPONSE", "DEVICE_RESTART", "g1v2", "g30v2",
		"0000", // the hex dump's first offset
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered output is missing %q:\n%s", want, out)
		}
	}
}

func TestHexDump(t *testing.T) {
	out := HexDump([]byte("DNP3 works\x00\xff"))
	if !strings.Contains(out, "DNP3 works..") {
		t.Errorf("ASCII column wrong:\n%s", out)
	}
	if !strings.Contains(out, "44 4e 50 33") {
		t.Errorf("hex column wrong:\n%s", out)
	}
}

func TestDirectionString(t *testing.T) {
	if DirTx.String() != "TX" || DirRx.String() != "RX" || DirUnknown.String() != "--" {
		t.Error("direction names are wrong")
	}
}

// TestWriteSampleHex regenerates the sample capture shipped for dnp3-decode.
// Run with -update to refresh it after changing any encoder.
func TestWriteSampleHex(t *testing.T) {
	var b strings.Builder
	b.WriteString("# Sample DNP3 traffic, generated by decoder.TestWriteSampleHex.\n")
	b.WriteString("# Regenerate with: go test ./decoder -run TestWriteSampleHex -update\n")

	for _, s := range []struct {
		name  string
		frame []byte
	}{
		{"master 1 resets the link to outstation 10", func() []byte {
			h := link.Header{
				Control: link.Control{Dir: true, Prm: true, Func: link.FuncResetLinkStates},
				Dest:    10, Src: 1, Length: link.MinLength,
			}
			w, err := link.Encode(nil, h, nil)
			if err != nil {
				t.Fatal(err)
			}
			return w
		}()},
		{"outstation 10 acknowledges", func() []byte {
			h := link.Header{
				Control: link.Control{Prm: false, Func: link.FuncAck},
				Dest:    1, Src: 10, Length: link.MinLength,
			}
			w, err := link.Encode(nil, h, nil)
			if err != nil {
				t.Fatal(err)
			}
			return w
		}()},
		{"integrity poll: read classes 1, 2, 3 and 0", integrityRequest(t)},
		{"response: 4 binaries, 2 analogs, outstation restarted", integrityResponse(t)},
		{"direct operate: trip the breaker at index 3", crobRequest(t)},
	} {
		fmt.Fprintf(&b, "\n# %s\n", s.name)
		for i, c := range s.frame {
			if i > 0 && i%16 == 0 {
				b.WriteByte('\n')
			}
			fmt.Fprintf(&b, "%02X ", c)
		}
		b.WriteByte('\n')
	}

	path := filepath.Join("testdata", "sample.hex")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s", path)
		return
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("%s does not exist; run with -update", path)
	}
	if string(got) != b.String() {
		t.Errorf("%s is stale; regenerate with -update", path)
	}
}
