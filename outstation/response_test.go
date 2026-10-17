package outstation

import (
	"testing"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/objects"
)

// A database may hold a full 65536 points of one type, which makes 0xFFFF a
// real index. Stepping a uint16 index past a run that ends there wraps it to
// zero, so building the class 0 response never finishes.
//
// The build runs on its own goroutine for that reason: were it to regress,
// running it on the session goroutine would hang the whole package until the
// test timeout rather than fail here. A goroutine that never returns is
// abandoned to spin until the test binary exits, which is a cost paid only
// when this test is already failing.
func TestStaticRangeTerminatesAtTheTopOfTheIndexSpace(t *testing.T) {
	const n = 65536
	s := New(Config{Database: DatabaseConfig{Binary: n}}, nil, nil)

	done := make(chan [][]byte, 1)
	go func() {
		b := newResponseBuilder(0, objects.Context{})
		s.buildStaticRange(b, dnp3.TypeBinary, 0, 0, 0xFFFF)
		done <- b.done()
	}()

	var bodies [][]byte
	select {
	case bodies = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("building a response over 65536 points never finished: an index " +
			"stepped past 0xFFFF and wrapped back to zero")
	}

	var covered int
	var top uint32
	for _, body := range bodies {
		frag := app.AppendHeader(nil, app.Header{
			Control: app.Control{Fir: true, Fin: true},
			Func:    app.FuncResponse,
		})
		f, err := app.ParseResponse(nil, append(frag, body...))
		if err != nil {
			t.Fatalf("a built fragment does not parse: %v", err)
		}
		for _, o := range f.Objects {
			covered += int(o.Range.Count)
			top = max(top, o.Range.Stop)
		}
	}
	if covered != n || top != n-1 {
		t.Errorf("reported %d points ending at index %d, want %d ending at %d",
			covered, top, n, n-1)
	}
}

// Octet strings are reported by their own loop, which had the same wraparound
// plus one more: adding a fragment's worth of strings to an index near the top
// overflowed 16 bits and put a run's end before its start, so the loop walked
// backwards for ever. Each string here is one octet, so a single run spans
// the whole top of the index space.
func TestOctetStringsTerminateAtTheTopOfTheIndexSpace(t *testing.T) {
	const n = 65536
	s := New(Config{Database: DatabaseConfig{OctetString: n}}, nil, nil)
	for i := range n {
		s.db.UpdateOctetString(uint16(i), dnp3.OctetString("x"))
	}

	done := make(chan [][]byte, 1)
	go func() {
		b := newResponseBuilder(0, objects.Context{})
		s.buildStaticRange(b, dnp3.TypeOctetString, 0, 0, 0xFFFF)
		done <- b.done()
	}()

	var bodies [][]byte
	select {
	case bodies = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("building octet strings over 65536 points never finished: an index " +
			"wrapped past 0xFFFF")
	}

	var covered int
	var top uint32
	for _, body := range bodies {
		frag := app.AppendHeader(nil, app.Header{
			Control: app.Control{Fir: true, Fin: true},
			Func:    app.FuncResponse,
		})
		f, err := app.ParseResponse(nil, append(frag, body...))
		if err != nil {
			t.Fatalf("a built fragment does not parse: %v", err)
		}
		for _, o := range f.Objects {
			if o.Range.Stop < o.Range.Start {
				t.Fatalf("a run ends at %d before it starts at %d", o.Range.Stop, o.Range.Start)
			}
			covered += int(o.Range.Count)
			top = max(top, o.Range.Stop)
		}
	}
	if covered != n || top != n-1 {
		t.Errorf("reported %d strings ending at index %d, want %d ending at %d",
			covered, top, n, n-1)
	}
}

// An event with no encoding used to be skipped by buildEvents yet stay
// selected, so the response's confirmation deleted it as though the master had
// received it. It now comes back as unsent, is discarded, and is counted as a
// loss the overflow indication reports; the encodable event beside it is
// still the only one the confirmation removes.
func TestUnencodableEventsAreNotConfirmedAway(t *testing.T) {
	s := New(Config{Database: DatabaseConfig{Binary: 2}}, nil, nil)
	s.db.events.Add(Event{Type: dnp3.TypeBinary, Index: 0, Class: dnp3.Class1, Variation: 99})
	s.db.events.Add(Event{Type: dnp3.TypeBinary, Index: 1, Class: dnp3.Class1, Variation: 1})

	selected := s.db.events.Select(dnp3.Class1, 10)
	b := newResponseBuilder(0, objects.Context{})
	unsent := s.buildEvents(b, selected)
	if len(unsent) != 1 || unsent[0].Index != 0 {
		t.Fatalf("unsent = %+v, want only the variation-99 event", unsent)
	}
	if n := s.db.events.Discard(unsent); n != 1 {
		t.Fatalf("Discard removed %d events, want 1", n)
	}
	if !s.db.events.Overflowed() {
		t.Error("discarding an unreportable event did not set the overflow indication")
	}
	if n := s.db.events.Confirm(); n != 1 {
		t.Errorf("Confirm removed %d events, want 1", n)
	}
}

func TestConfigureRejectsUnencodableEventVariation(t *testing.T) {
	s := New(Config{Database: DatabaseConfig{Binary: 1}}, nil, nil)
	if s.db.Configure(dnp3.TypeBinary, 0, PointConfig{EventVariation: 99}) {
		t.Error("Configure accepted binary event variation 99")
	}
	if !s.db.Configure(dnp3.TypeBinary, 0, PointConfig{EventVariation: 2}) {
		t.Error("Configure refused binary event variation 2")
	}
}

// Restart used to go through Update, whose fallback for a full queue runs the
// closure on the caller's goroutine — and the closure wrote session state the
// session goroutine reads unlocked. The session state now changes only when
// the session goroutine applies the pending restart.
func TestRestartWithFullQueueLeavesSessionStateAlone(t *testing.T) {
	s := New(Config{Database: DatabaseConfig{Binary: 1}}, nil, nil)
	s.iin = s.iin.Clear(app.IINDeviceRestart)
	for len(s.updates) < cap(s.updates) {
		s.updates <- func(*Database) {}
	}
	s.Restart()
	if s.iin.Has(app.IINDeviceRestart) {
		t.Fatal("Restart changed session state off the session goroutine")
	}
	s.drainUpdates()
	if !s.iin.Has(app.IINDeviceRestart) {
		t.Error("the pending restart was not applied when the queue drained")
	}
}
