package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestParseHexAcceptedFormats(t *testing.T) {
	want := []byte{0x05, 0x64, 0x05, 0xC0}

	tests := []struct {
		name string
		in   string
	}{
		{"spaced upper", "05 64 05 C0"},
		{"spaced lower", "05 64 05 c0"},
		{"unspaced", "05640 5c0"},
		{"one blob", "0564 05C0"},
		{"comma separated", "05,64,05,C0"},
		{"0x prefixed", "0x05 0x64 0x05 0xC0"},
		{"0x comma separated", "0x05,0x64,0x05,0xc0"},
		{"colon separated", "05:64:05:C0"},
		{"dash separated", "05-64-05-C0"},
		{"trailing comment", "05 64 05 C0  # link header start"},
		{"leading comment only", "# nothing here"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseHex(tc.in)
			if err != nil {
				t.Fatalf("parseHex(%q): %v", tc.in, err)
			}
			if tc.name == "leading comment only" {
				if len(got) != 0 {
					t.Errorf("a comment produced %d octets", len(got))
				}
				return
			}
			if !bytes.Equal(got, want) {
				t.Errorf("parseHex(%q) = % x, want % x", tc.in, got, want)
			}
		})
	}
}

// TestParseHexWiresharkDump is the format this parser exists to handle
// correctly. The offset column looks like two valid octets and the ASCII
// gutter is full of letters that are also hex digits, so a parser that just
// grabbed every hex digit would produce garbage that still decodes to
// something — the worst possible failure.
func TestParseHexWiresharkDump(t *testing.T) {
	line := "0000   05 64 05 c0 0a 00 01 00  a5 e9                    .d........"

	got, err := parseHex(line)
	if err != nil {
		t.Fatalf("parseHex: %v", err)
	}
	want := []byte{0x05, 0x64, 0x05, 0xc0, 0x0a, 0x00, 0x01, 0x00, 0xa5, 0xe9}
	if !bytes.Equal(got, want) {
		t.Errorf("got % x\nwant % x", got, want)
	}
}

func TestParseHexRejectsProse(t *testing.T) {
	// "decoded" and "cafe" are entirely hex digits. Silently turning an
	// English sentence into octets would be worse than refusing it.
	got, err := parseHex("the cafe decoded a beef")
	if err != nil {
		// An odd digit count is a perfectly good way to refuse.
		return
	}
	// If it did parse, it must not have invented octets from the words that
	// happen to be hex — only whole even-length hex tokens are eligible, and
	// this line's tokens are of odd length or contain non-hex letters.
	if len(got) != 0 {
		t.Errorf("prose produced %d octets: % x", len(got), got)
	}
}

func TestParseHexOddDigits(t *testing.T) {
	if _, err := parseHex("05 64 0"); err == nil {
		t.Error("an odd digit count should be an error")
	}
	if err := func() error { _, err := parseHex("05 64 0"); return err }(); err != nil {
		if !strings.Contains(err.Error(), "odd number") {
			t.Errorf("error message is unhelpful: %v", err)
		}
	}
}

func TestIsOffsetColumn(t *testing.T) {
	for _, s := range []string{"0000", "00000010", "0010:", "abcd"} {
		if !isOffsetColumn(s) {
			t.Errorf("%q should look like an offset", s)
		}
	}
	for _, s := range []string{"05", "064", "0123456789", "zz00", ""} {
		if isOffsetColumn(s) {
			t.Errorf("%q should not look like an offset", s)
		}
	}
}

func TestReadHexMultilineWithComments(t *testing.T) {
	in := `# a capture
05 64 05 C0
# another frame
0A 00 01 00
`
	got, err := readHex(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{0x05, 0x64, 0x05, 0xC0, 0x0A, 0x00, 0x01, 0x00}
	if !bytes.Equal(got, want) {
		t.Errorf("got % x, want % x", got, want)
	}
}

func TestReadHexReportsLineNumber(t *testing.T) {
	_, err := readHex(strings.NewReader("05 64\n05 6\n"))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "line 2") {
		t.Errorf("error should name the offending line: %v", err)
	}
}

// TestDecodeSample runs the full pipeline over the generated sample capture,
// which is the closest thing to an end-to-end test this command has.
func TestDecodeSample(t *testing.T) {
	data, err := readHexFile(t, "../../decoder/testdata/sample.hex")
	if err != nil {
		t.Skipf("sample capture unavailable: %v", err)
	}

	var out strings.Builder
	frames, errs := decode(&out, data, false, false)

	if frames != 5 {
		t.Errorf("decoded %d frames, want 5", frames)
	}
	if errs != 0 {
		t.Errorf("%d errors decoding a known-good capture:\n%s", errs, out.String())
	}
	for _, want := range []string{
		"RESET_LINK_STATES", "ACK", "READ", "RESPONSE", "DIRECT_OPERATE",
		"g60v1", "g1v2", "g30v2", "g12v1", "DEVICE_RESTART",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output is missing %q:\n%s", want, out.String())
		}
	}
}

// TestDecodeSampleStreaming decodes the same capture with transport state
// carried across frames.
func TestDecodeSampleStreaming(t *testing.T) {
	data, err := readHexFile(t, "../../decoder/testdata/sample.hex")
	if err != nil {
		t.Skipf("sample capture unavailable: %v", err)
	}

	var out strings.Builder
	frames, _ := decode(&out, data, true, true)
	if frames != 5 {
		t.Errorf("streaming mode decoded %d frames, want 5", frames)
	}
	if !strings.Contains(out.String(), "0000") {
		t.Error("the -x hex dump did not appear")
	}
}

func readHexFile(t *testing.T, path string) ([]byte, error) {
	t.Helper()
	f, err := openFile(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readHex(f)
}
