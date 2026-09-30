package objects

import (
	"testing"
	"time"

	"github.com/dscsystems/go-dnp3"
)

func TestBinaryCommandEventGoldenBytes(t *testing.T) {
	// The status sits in the low seven bits and the commanded state in bit 7.
	got := AppendCommandEvent(nil, 13, 1, dnp3.CommandEvent{Status: dnp3.CommandStatus(4), State: true})
	if len(got) != 1 || got[0] != 0x84 {
		t.Errorf("g13v1 = % x, want 84", got)
	}
}

func TestCommandEventsRoundTrip(t *testing.T) {
	at := dnp3.Now(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC))
	cases := []struct {
		group, variation uint8
		in               dnp3.CommandEvent
		timed            bool
	}{
		{13, 1, dnp3.CommandEvent{Status: dnp3.CommandSuccess, State: true}, false},
		{13, 2, dnp3.CommandEvent{Status: dnp3.CommandSuccess, State: false}, true},
		{43, 1, dnp3.CommandEvent{Analog: true, Value: -70000}, false},
		{43, 2, dnp3.CommandEvent{Analog: true, Value: 1234}, false},
		{43, 3, dnp3.CommandEvent{Analog: true, Value: 70000}, true},
		{43, 4, dnp3.CommandEvent{Analog: true, Value: -5}, true},
		{43, 5, dnp3.CommandEvent{Analog: true, Value: 1.5}, false},
		{43, 6, dnp3.CommandEvent{Analog: true, Value: 1e100}, false},
		{43, 7, dnp3.CommandEvent{Analog: true, Value: 2.5}, true},
		{43, 8, dnp3.CommandEvent{Analog: true, Value: -2.5e10}, true},
	}
	for _, c := range cases {
		in := c.in
		if c.timed {
			in.Time = at
		}
		buf := AppendCommandEvent(nil, c.group, c.variation, in)
		if size, _ := CommandEventSize(c.group, c.variation); len(buf) != size {
			t.Errorf("g%dv%d encoded to %d octets, want %d", c.group, c.variation, len(buf), size)
		}
		out, ok := ParseCommandEvent(c.group, c.variation, buf)
		if !ok {
			t.Errorf("g%dv%d did not parse", c.group, c.variation)
			continue
		}
		if out.Status != in.Status || out.State != in.State || out.Value != in.Value ||
			out.Analog != in.Analog || !out.Time.Time.Equal(in.Time.Time) {
			t.Errorf("g%dv%d: got %+v, want %+v", c.group, c.variation, out, in)
		}
	}
}

// An integer variation saturates rather than wrapping.
func TestAnalogCommandEventSaturates(t *testing.T) {
	buf := AppendCommandEvent(nil, 43, 2, dnp3.CommandEvent{Analog: true, Value: 1e9})
	out, _ := ParseCommandEvent(43, 2, buf)
	if out.Value != 32767 {
		t.Errorf("1e9 in g43v2 read back as %v, want 32767", out.Value)
	}
}

func TestCommandEventRejectsUnknownVariation(t *testing.T) {
	if _, ok := ParseCommandEvent(43, 9, make([]byte, 20)); ok {
		t.Error("g43v9 parsed")
	}
	if got := AppendCommandEvent(nil, 13, 3, dnp3.CommandEvent{}); len(got) != 0 {
		t.Error("g13v3 encoded")
	}
	if _, ok := ParseCommandEvent(43, 3, make([]byte, 3)); ok {
		t.Error("a truncated g43v3 parsed")
	}
}
