package outstation

import (
	"testing"

	"github.com/dscsystems/go-dnp3"
)

func testDB(cfg DatabaseConfig) (*Database, *EventBuffer) {
	buf := NewEventBuffer(EventBufferConfig{MaxEvents: 1000})
	return NewDatabase(cfg, buf), buf
}

func TestUpdateStoresAndReads(t *testing.T) {
	db, _ := testDB(DatabaseConfig{Binary: 4, Analog: 4, Counter: 2})

	db.UpdateBinary(1, dnp3.Binary{Value: true, Flags: dnp3.Online})
	db.UpdateAnalog(2, dnp3.Analog{Value: 42.5, Flags: dnp3.Online})
	db.UpdateCounter(0, dnp3.Counter{Value: 77, Flags: dnp3.Online})

	if v, _, ok := db.Binary(1); !ok || !v.Value {
		t.Errorf("binary 1 = %+v, ok=%v", v, ok)
	}
	if v, _, ok := db.Analog(2); !ok || v.Value != 42.5 {
		t.Errorf("analog 2 = %+v, ok=%v", v, ok)
	}
	if v, _, ok := db.Counter(0); !ok || v.Value != 77 {
		t.Errorf("counter 0 = %+v, ok=%v", v, ok)
	}
}

// TestUpdateOutOfRangeIsIgnored covers a configuration that names more points
// than the database holds. Silently ignoring beats panicking an outstation at
// three in the morning because a config file drifted.
func TestUpdateOutOfRangeIsIgnored(t *testing.T) {
	db, buf := testDB(DatabaseConfig{Binary: 2, DefaultClass: dnp3.Class1})

	db.UpdateBinary(99, dnp3.Binary{Value: true, Flags: dnp3.Online})

	if _, _, ok := db.Binary(99); ok {
		t.Error("index 99 should not exist")
	}
	if buf.Total() != 0 {
		t.Error("an out-of-range update generated an event")
	}
}

func TestBinaryEventsOnValueOrFlagChange(t *testing.T) {
	db, buf := testDB(DatabaseConfig{Binary: 2, DefaultClass: dnp3.Class1})

	db.UpdateBinary(0, dnp3.Binary{Value: false, Flags: dnp3.Online})
	if buf.Total() != 1 {
		t.Fatalf("the first update produced %d events, want 1", buf.Total())
	}

	// The same value again is not a change.
	db.UpdateBinary(0, dnp3.Binary{Value: false, Flags: dnp3.Online})
	if buf.Total() != 1 {
		t.Errorf("an unchanged value produced an event: %d total", buf.Total())
	}

	db.UpdateBinary(0, dnp3.Binary{Value: true, Flags: dnp3.Online})
	if buf.Total() != 2 {
		t.Errorf("a value change produced %d events, want 2 total", buf.Total())
	}

	// A quality change with the same value is still a change worth reporting:
	// a point going comm-lost matters even if its last value did not move.
	db.UpdateBinary(0, dnp3.Binary{Value: true, Flags: dnp3.Online | dnp3.CommLost})
	if buf.Total() != 3 {
		t.Errorf("a quality change produced %d events, want 3 total", buf.Total())
	}
}

func TestClassNoneSuppressesEvents(t *testing.T) {
	db, buf := testDB(DatabaseConfig{Binary: 2, DefaultClass: dnp3.ClassNone})

	db.UpdateBinary(0, dnp3.Binary{Value: true, Flags: dnp3.Online})
	db.UpdateBinary(0, dnp3.Binary{Value: false, Flags: dnp3.Online})

	if buf.Total() != 0 {
		t.Errorf("a point in no event class produced %d events", buf.Total())
	}
	// The static value still updates — suppressing events is not suppressing
	// the point.
	if v, _, _ := db.Binary(0); v.Value {
		t.Error("the static value did not update")
	}
}

// TestAnalogDeadbandMeasuredFromLastReported is the bug this design exists to
// prevent.
//
// Comparing against the last *stored* value lets a point drift indefinitely in
// deadband-sized steps without ever reporting: each step is under the
// threshold, so no event fires, and a slow ramp toward a trip limit stays
// invisible. The comparison has to be against what the master was actually
// told.
func TestAnalogDeadbandMeasuredFromLastReported(t *testing.T) {
	db, buf := testDB(DatabaseConfig{Analog: 1, DefaultClass: dnp3.Class1})
	db.Configure(dnp3.TypeAnalog, 0, PointConfig{Class: dnp3.Class1, Deadband: 5})

	db.UpdateAnalog(0, dnp3.Analog{Value: 100, Flags: dnp3.Online})
	if buf.Total() != 1 {
		t.Fatalf("the first update produced %d events, want 1", buf.Total())
	}

	// Ten steps of 1.0 each: every step is inside the deadband, but together
	// they move the value by 10, twice the threshold.
	for i := range 10 {
		db.UpdateAnalog(0, dnp3.Analog{Value: 101 + float64(i), Flags: dnp3.Online})
	}

	if buf.Total() < 2 {
		t.Error("a value that drifted 10 units past a deadband of 5 never reported; " +
			"the deadband is being measured from the last stored value, not the last reported one")
	}

	// And the reported value is now current, so a small move does not report.
	before := buf.Total()
	db.UpdateAnalog(0, dnp3.Analog{Value: 110.5, Flags: dnp3.Online})
	if buf.Total() != before {
		t.Error("a move inside the deadband produced an event")
	}
}

func TestAnalogDeadbandZeroReportsEveryChange(t *testing.T) {
	db, buf := testDB(DatabaseConfig{Analog: 1, DefaultClass: dnp3.Class1})

	db.UpdateAnalog(0, dnp3.Analog{Value: 1, Flags: dnp3.Online})
	db.UpdateAnalog(0, dnp3.Analog{Value: 2, Flags: dnp3.Online})
	db.UpdateAnalog(0, dnp3.Analog{Value: 3, Flags: dnp3.Online})

	if buf.Total() != 3 {
		t.Errorf("%d events with a zero deadband, want 3", buf.Total())
	}
}

func TestAnalogFlagChangeReportsRegardlessOfDeadband(t *testing.T) {
	// A point going offline matters even if its value did not move enough.
	db, buf := testDB(DatabaseConfig{Analog: 1, DefaultClass: dnp3.Class1})
	db.Configure(dnp3.TypeAnalog, 0, PointConfig{Class: dnp3.Class1, Deadband: 100})

	db.UpdateAnalog(0, dnp3.Analog{Value: 50, Flags: dnp3.Online})
	before := buf.Total()

	db.UpdateAnalog(0, dnp3.Analog{Value: 50, Flags: dnp3.Online | dnp3.CommLost})
	if buf.Total() <= before {
		t.Error("a quality change inside the deadband was suppressed")
	}
}

func TestConfigureKeepsUnsetVariations(t *testing.T) {
	db, _ := testDB(DatabaseConfig{Analog: 2})

	_, before, _ := db.Analog(0)
	db.Configure(dnp3.TypeAnalog, 0, PointConfig{Class: dnp3.Class2})

	_, after, _ := db.Analog(0)
	if after.StaticVariation != before.StaticVariation {
		t.Errorf("static variation changed from %d to %d when unset in the config",
			before.StaticVariation, after.StaticVariation)
	}
	if after.EventVariation != before.EventVariation {
		t.Errorf("event variation changed from %d to %d when unset in the config",
			before.EventVariation, after.EventVariation)
	}
	if after.Class != dnp3.Class2 {
		t.Errorf("class = %v, want 2", after.Class)
	}
}

func TestConfigureOutOfRange(t *testing.T) {
	db, _ := testDB(DatabaseConfig{Analog: 2})
	if db.Configure(dnp3.TypeAnalog, 99, PointConfig{}) {
		t.Error("configuring a nonexistent point should report failure, not panic")
	}
}

func TestAssignClass(t *testing.T) {
	db, buf := testDB(DatabaseConfig{Binary: 3, DefaultClass: dnp3.Class1})

	db.AssignClass(dnp3.TypeBinary, dnp3.Class3)
	db.UpdateBinary(0, dnp3.Binary{Value: true, Flags: dnp3.Online})

	if buf.Count(dnp3.Class3) != 1 {
		t.Errorf("class 3 has %d events, want 1", buf.Count(dnp3.Class3))
	}
	if buf.Count(dnp3.Class1) != 0 {
		t.Errorf("class 1 still has %d events", buf.Count(dnp3.Class1))
	}
}

func TestFreezeCounters(t *testing.T) {
	db, _ := testDB(DatabaseConfig{Counter: 3, FrozenCounter: 3})

	db.UpdateCounter(0, dnp3.Counter{Value: 100, Flags: dnp3.Online})
	db.UpdateCounter(2, dnp3.Counter{Value: 300, Flags: dnp3.Online})
	db.FreezeCounters()

	if v, _, _ := db.FrozenCounter(0); v.Value != 100 {
		t.Errorf("frozen counter 0 = %d, want 100", v.Value)
	}
	if v, _, _ := db.FrozenCounter(2); v.Value != 300 {
		t.Errorf("frozen counter 2 = %d, want 300", v.Value)
	}

	// The running counter keeps counting after a freeze.
	db.UpdateCounter(0, dnp3.Counter{Value: 150, Flags: dnp3.Online})
	if v, _, _ := db.FrozenCounter(0); v.Value != 100 {
		t.Errorf("frozen counter moved with the running one: %d", v.Value)
	}
}

func TestFreezeWithMismatchedCounts(t *testing.T) {
	// A device with fewer frozen counters than counters must freeze what it
	// can rather than panicking.
	db, _ := testDB(DatabaseConfig{Counter: 5, FrozenCounter: 2})
	db.UpdateCounter(0, dnp3.Counter{Value: 1, Flags: dnp3.Online})
	db.FreezeCounters()

	if v, _, _ := db.FrozenCounter(0); v.Value != 1 {
		t.Errorf("frozen counter 0 = %d, want 1", v.Value)
	}
}

func TestCounts(t *testing.T) {
	cfg := DatabaseConfig{Binary: 3, Analog: 5, Counter: 2, BinaryOutputStatus: 1}
	db, _ := testDB(cfg)

	got := db.Counts()
	if got.Binary != 3 || got.Analog != 5 || got.Counter != 2 || got.BinaryOutputStatus != 1 {
		t.Errorf("counts = %+v, want %+v", got, cfg)
	}
}

func TestDefaultVariationsAreLossless(t *testing.T) {
	// The default static variations must be the widest encoding for each type.
	// A narrower default silently truncates every value the device reports,
	// which is the kind of bug that only surfaces against real data.
	db, _ := testDB(DatabaseConfig{Analog: 1, Counter: 1, Binary: 1})

	if _, c, _ := db.Analog(0); c.StaticVariation != 1 {
		t.Errorf("analog default static variation = %d, want 1 (32-bit with flags)", c.StaticVariation)
	}
	if _, c, _ := db.Counter(0); c.StaticVariation != 1 {
		t.Errorf("counter default static variation = %d, want 1 (32-bit with flags)", c.StaticVariation)
	}
	if _, c, _ := db.Binary(0); c.StaticVariation != 2 {
		t.Errorf("binary default static variation = %d, want 2 (with flags)", c.StaticVariation)
	}
}
