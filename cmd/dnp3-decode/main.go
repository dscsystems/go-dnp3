// Copyright (C) 2026 Ricardo Olsen / DSC Systems.
//
// This program is free software: you can redistribute it and/or modify it
// under the terms of the GNU General Public License as published by the Free
// Software Foundation, either version 3 of the License, or (at your option)
// any later version. It is distributed WITHOUT ANY WARRANTY; without even the
// implied warranty of MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.
// See the GNU General Public License for more details, in the LICENSE file at
// the root of this repository or at <https://www.gnu.org/licenses/>.

// Command dnp3-decode renders DNP3 octets as a decoded protocol tree.
//
// It reads hex from the command line, from a file, or from standard input, and
// prints the link, transport and application layers of every frame it finds.
//
//	dnp3-decode 05 64 05 C0 0A 00 01 00 B1 AC
//	dnp3-decode -x 0564050ac0...
//	tcpdump -i lo -s0 -x port 20000 | dnp3-decode -
//	dnp3-decode -f capture.hex
//
// Input is read leniently but not blindly: '#' starts a comment, a hex-dump
// offset column and ASCII gutter are recognised and dropped, and only whole
// hex tokens become octets. Output pasted from Wireshark, a vendor log or a
// device console usually works without editing.
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/dscsystems/go-dnp3/decoder"
	"github.com/dscsystems/go-dnp3/internal/link"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "dnp3-decode:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		file    = flag.String("f", "", "read hex from `file` instead of the command line")
		showHex = flag.Bool("x", false, "include a hex dump of each frame")
		quiet   = flag.Bool("q", false, "suppress the summary line")
		stream  = flag.Bool("s", false, "treat input as one continuous stream, reassembling multi-frame fragments")
	)
	flag.Usage = usage
	flag.Parse()

	data, err := readInput(*file, flag.Args())
	if err != nil {
		return err
	}
	if len(data) == 0 {
		usage()
		return errors.New("no input")
	}

	var out strings.Builder
	frames, errs := decode(&out, data, *showHex, *stream)

	fmt.Print(out.String())
	if !*quiet {
		fmt.Printf("\n%d frame(s), %d error(s), %d octets\n", frames, errs, len(data))
	}
	if errs > 0 {
		os.Exit(2)
	}
	return nil
}

// decode renders every frame in data, returning how many frames and errors it
// found.
func decode(out *strings.Builder, data []byte, showHex, stream bool) (frames, errs int) {
	if stream {
		// Streaming mode keeps transport state across frames, so a fragment
		// split over several frames is reassembled and its application layer
		// decoded on the frame that completes it.
		d := decoder.New(decoder.DirUnknown, nil)
		d.Feed(data, func(t decoder.Trace) {
			frames++
			if t.Err != nil || (t.App != nil && t.App.Err != nil) {
				errs++
			}
			t.Render(out, showHex)
			out.WriteByte('\n')
		})
		lst, tst := d.Stats()
		if lst.BytesDiscarded > 0 || tst.SegmentsDiscarded > 0 {
			fmt.Fprintf(out, "discarded %d octet(s) at the link layer, %d segment(s) at the transport layer\n",
				lst.BytesDiscarded, tst.SegmentsDiscarded)
			errs++
		}
		return frames, errs
	}

	// Frame-at-a-time mode: each frame is decoded independently, which is what
	// you want when pasting frames captured out of context.
	for off := 0; off < len(data); {
		t, n, err := decoder.DecodeFrame(nil, data[off:])
		if err != nil {
			if errors.Is(err, link.ErrShortFrame) {
				fmt.Fprintf(out, "-- %d trailing octet(s) do not form a complete frame\n", len(data)-off)
				return frames, errs + 1
			}
			// Not a frame at this offset. Skip an octet and look for the next
			// delimiter, the same way the streaming parser resynchronises.
			off++
			continue
		}
		frames++
		if t.App != nil && t.App.Err != nil {
			errs++
		}
		t.Render(out, showHex)
		out.WriteByte('\n')
		off += n
	}
	return frames, errs
}

// readInput gathers octets from a file, standard input, or the command line.
func readInput(file string, args []string) ([]byte, error) {
	switch {
	case file != "":
		f, err := openFile(file)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		return readHex(f)

	case len(args) == 1 && args[0] == "-":
		return readHex(os.Stdin)

	case len(args) > 0:
		return parseHex(strings.Join(args, " "))

	default:
		// No arguments and a piped stdin is the common shell idiom, so read it
		// rather than printing usage at someone who clearly meant to pipe.
		st, err := os.Stdin.Stat()
		if err == nil && st.Mode()&os.ModeCharDevice == 0 {
			return readHex(os.Stdin)
		}
		return nil, nil
	}
}

// openFile exists so tests can read a capture through the same path the
// command does.
func openFile(path string) (*os.File, error) { return os.Open(path) }

func readHex(r io.Reader) ([]byte, error) {
	var all []byte
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for line := 1; sc.Scan(); line++ {
		b, err := parseHex(sc.Text())
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		all = append(all, b...)
	}
	return all, sc.Err()
}

// parseHex extracts hex octets from a line of text.
//
// The rules are deliberately explicit rather than "grab every hex digit". Two
// things make the naive approach wrong on exactly the input this tool exists
// to read. A hex dump's ASCII gutter is full of letters that are also hex
// digits — "cafe" in a log line is four of them — and a dump's offset column
// looks like two perfectly good octets. So:
//
//   - everything from a '#' onwards is a comment;
//   - columns are separated by runs of two or more spaces;
//   - a leading column of 4 to 8 hex digits is an offset and is dropped;
//   - when columns remain after that, the last is an ASCII gutter and is
//     dropped;
//   - what survives is tokenised on whitespace and , : - and each token is
//     accepted only if it is entirely hex digits, after an optional 0x.
//
// That reads `05 64 05 C0`, `0x05,0x64`, `0564 05c0`, and Wireshark's hex-dump
// export, and refuses to invent octets out of prose.
func parseHex(s string) ([]byte, error) {
	if i := strings.IndexByte(s, '#'); i >= 0 {
		s = s[:i]
	}

	cols := splitColumns(s)
	if len(cols) >= 2 && isOffsetColumn(cols[0]) {
		cols = cols[1:]
	}
	if len(cols) >= 2 {
		cols = cols[:len(cols)-1] // the ASCII gutter
	}

	var digits []byte
	for _, col := range cols {
		for _, tok := range strings.FieldsFunc(col, isSeparator) {
			tok = strings.TrimPrefix(strings.TrimPrefix(tok, "0x"), "0X")
			if tok == "" {
				continue
			}
			if !isAllHex(tok) {
				continue
			}
			digits = append(digits, tok...)
		}
	}

	if len(digits)%2 != 0 {
		return nil, fmt.Errorf("odd number of hex digits (%d) in %q", len(digits), strings.TrimSpace(s))
	}
	out := make([]byte, len(digits)/2)
	for i := range out {
		out[i] = hexVal(digits[i*2])<<4 | hexVal(digits[i*2+1])
	}
	return out, nil
}

// splitColumns splits on runs of two or more spaces, which is how hex dumps
// separate the offset, the octets and the ASCII gutter.
func splitColumns(s string) []string {
	var cols []string
	for field := range strings.SplitSeq(s, "  ") {
		if t := strings.TrimSpace(field); t != "" {
			cols = append(cols, t)
		}
	}
	return cols
}

// isOffsetColumn reports whether a column looks like a hex-dump offset: a
// single run of four to eight hex digits, optionally followed by a colon.
func isOffsetColumn(s string) bool {
	s = strings.TrimSuffix(s, ":")
	return len(s) >= 4 && len(s) <= 8 && isAllHex(s) && !strings.ContainsAny(s, " \t")
}

func isSeparator(r rune) bool {
	switch r {
	case ' ', '\t', ',', ':', '-', '|':
		return true
	}
	return false
}

func isAllHex(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		if !isHexDigit(s[i]) {
			return false
		}
	}
	return true
}

func isHexDigit(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func hexVal(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	default:
		return c - 'A' + 10
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `dnp3-decode — render DNP3 octets as a decoded protocol tree

Usage:
  dnp3-decode [flags] <hex octets...>
  dnp3-decode [flags] -
  dnp3-decode [flags] -f FILE

Flags:
  -f FILE   read hex from a file
  -x        include a hex dump of each frame
  -s        treat the input as one continuous stream and reassemble fragments
  -q        suppress the summary line

Input is read leniently but not blindly: '#' starts a comment, a hex-dump
offset column and ASCII gutter are recognised and dropped, and only whole hex
tokens become octets. Output pasted from Wireshark, a vendor log or a device
console usually works unedited.

Examples:
  dnp3-decode 05 64 05 C0 0A 00 01 00 B1 AC
  dnp3-decode -x -f capture.hex
  cat capture.hex | dnp3-decode -s
`)
}
