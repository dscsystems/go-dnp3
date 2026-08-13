package dnp3

import (
	"testing"
	"time"
)

func TestFlagsIsGood(t *testing.T) {
	tests := []struct {
		name  string
		flags Flags
		want  bool
	}{
		{"online only", Online, true},
		{"online with a type-specific bit", Online | OverRange, true},
		{"offline", 0, false},
		{"online but restarting", Online | Restart, false},
		{"online but comm lost", Online | CommLost, false},
		{"online but locally forced", Online | LocalForced, false},
		{"online but remotely forced", Online | RemoteForced, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.flags.IsGood(); got != tc.want {
				t.Errorf("IsGood() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFlagsStringForNamesUpperBitsByType(t *testing.T) {
	// Bit 0x20 is CHATTER_FILTER on a binary, ROLLOVER on a counter and
	// OVER_RANGE on an analog. Rendering it as the wrong one would mislead an
	// operator reading a log at 3am.
	f := Online | 0x20
	tests := []struct {
		typ  PointType
		want string
	}{
		{TypeBinary, "ONLINE|CHATTER_FILTER"},
		{TypeCounter, "ONLINE|ROLLOVER"},
		{TypeAnalog, "ONLINE|OVER_RANGE"},
	}
	for _, tc := range tests {
		if got := f.StringFor(tc.typ); got != tc.want {
			t.Errorf("StringFor(%s) = %q, want %q", tc.typ, got, tc.want)
		}
	}
	if got := Flags(0).String(); got != "—" {
		t.Errorf("empty flags = %q, want an em dash", got)
	}
}

func TestFlagsSetClearHas(t *testing.T) {
	f := Flags(0).Set(Online).Set(CommLost)
	if !f.Has(Online | CommLost) {
		t.Error("Has should require every bit in the mask")
	}
	if f.Has(Online | Restart) {
		t.Error("Has must not match on a partial overlap")
	}
	if !f.HasAny(Online | Restart) {
		t.Error("HasAny should match on a partial overlap")
	}
	if f.Clear(CommLost) != Online {
		t.Error("Clear did not remove the bit")
	}
}

func TestDNP3TimeRoundTrip(t *testing.T) {
	// DNP3 carries 48 bits of milliseconds since the UNIX epoch, so any
	// realistic instant survives, but only to millisecond resolution.
	want := time.Date(2026, 8, 13, 14, 30, 45, 123_000_000, time.UTC)
	got := DNP3ToTime(TimeToDNP3(want))
	if !got.Equal(want) {
		t.Errorf("round trip = %v, want %v", got, want)
	}
}

func TestTimeToDNP3Clamps(t *testing.T) {
	// Times outside the encodable range must clamp rather than wrap. A wrap
	// would put an event decades away from where it belongs.
	if got := TimeToDNP3(time.Date(1900, 1, 1, 0, 0, 0, 0, time.UTC)); got != 0 {
		t.Errorf("pre-epoch time encoded as %d, want 0", got)
	}
	far := time.UnixMilli(MaxDNP3Time).Add(24 * time.Hour)
	if got := TimeToDNP3(far); got != uint64(MaxDNP3Time) {
		t.Errorf("far-future time encoded as %d, want %d", got, MaxDNP3Time)
	}
}

func TestDNP3ToTimeIgnoresHighBits(t *testing.T) {
	// Only the low 48 bits are on the wire; a decoder handed more must mask
	// rather than produce a nonsense instant.
	base := uint64(1_755_093_045_123)
	if !DNP3ToTime(base | 1<<50).Equal(DNP3ToTime(base)) {
		t.Error("high bits above 48 were not masked off")
	}
}

func TestTimestampQuality(t *testing.T) {
	if NoTime().IsValid() {
		t.Error("NoTime should be invalid")
	}
	if !Now(time.Now()).IsValid() {
		t.Error("Now should be valid")
	}
	if Now(time.Now()).Quality != TimestampSynchronized {
		t.Error("Now should be synchronized")
	}
	if Unsynchronized(time.Now()).Quality != TimestampUnsynchronized {
		t.Error("Unsynchronized should be unsynchronized")
	}
	if got := NoTime().String(); got != "—" {
		t.Errorf("invalid timestamp = %q, want an em dash", got)
	}
}

func TestControlCodeFields(t *testing.T) {
	tests := []struct {
		name   string
		code   ControlCode
		op     ControlCode
		isTrip bool
		close  bool
		str    string
	}{
		{"latch on", ControlLatchOn, ControlLatchOn, false, false, "LATCH_ON"},
		{"latch off", ControlLatchOff, ControlLatchOff, false, false, "LATCH_OFF"},
		{"pulse on trip", ControlPulseOn | ControlTrip, ControlPulseOn, true, false, "PULSE_ON|TRIP"},
		{"pulse on close", ControlPulseOn | ControlClose, ControlPulseOn, false, true, "PULSE_ON|CLOSE"},
		{"nul", ControlNUL, ControlNUL, false, false, "NUL"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.code.OpType(); got != tc.op {
				t.Errorf("OpType() = %v, want %v", got, tc.op)
			}
			if got := tc.code.IsTrip(); got != tc.isTrip {
				t.Errorf("IsTrip() = %v, want %v", got, tc.isTrip)
			}
			if got := tc.code.IsClose(); got != tc.close {
				t.Errorf("IsClose() = %v, want %v", got, tc.close)
			}
			if got := tc.code.String(); got != tc.str {
				t.Errorf("String() = %q, want %q", got, tc.str)
			}
		})
	}
}

func TestControlCodeTripAndCloseAreExclusive(t *testing.T) {
	// Both bits set is not "trip and close", it is an invalid encoding. A
	// device that treats it as either could operate the wrong coil.
	both := ControlLatchOn | ControlTrip | ControlClose
	if both.IsTrip() || both.IsClose() {
		t.Error("both coil bits set must select neither coil")
	}
}

func TestClassMask(t *testing.T) {
	if ClassAll != Class0|Class1|Class2|Class3 {
		t.Error("ClassAll should be an integrity poll")
	}
	if Class123.Has(Class0) {
		t.Error("Class123 must not include static data")
	}
	if got := ClassAll.String(); got != "0+1+2+3" {
		t.Errorf("ClassAll = %q, want \"0+1+2+3\"", got)
	}
	if got := ClassNone.String(); got != "none" {
		t.Errorf("ClassNone = %q, want \"none\"", got)
	}
	if got := Class1.String(); got != "1" {
		t.Errorf("Class1 = %q, want \"1\"", got)
	}
}

func TestCommandStatusString(t *testing.T) {
	if !CommandSuccess.OK() {
		t.Error("SUCCESS should report OK")
	}
	if CommandNoSelect.OK() {
		t.Error("NO_SELECT should not report OK")
	}
	if got := CommandNoSelect.String(); got != "NO_SELECT" {
		t.Errorf("String() = %q, want NO_SELECT", got)
	}
	if got := CommandStatus(200).String(); got != "CommandStatus(200)" {
		t.Errorf("unknown status = %q", got)
	}
}

func TestAnalogFits(t *testing.T) {
	tests := []struct {
		v    float64
		in16 bool
		in32 bool
	}{
		{0, true, true},
		{32767, true, true},
		{32768, false, true},
		{-32768, true, true},
		{-32769, false, true},
		{2147483647, false, true},
		{2147483648, false, false},
		{1.5, false, false}, // fractional values lose precision in an integer variation
	}
	for _, tc := range tests {
		if got := AnalogFitsIn16(tc.v); got != tc.in16 {
			t.Errorf("AnalogFitsIn16(%v) = %v, want %v", tc.v, got, tc.in16)
		}
		if got := AnalogFitsIn32(tc.v); got != tc.in32 {
			t.Errorf("AnalogFitsIn32(%v) = %v, want %v", tc.v, got, tc.in32)
		}
	}
}

func TestDoubleBitString(t *testing.T) {
	// The wire encoding is fixed by the standard; pin it so a reordering of
	// the constants cannot go unnoticed.
	if DoubleBitIntermediate != 0 || DoubleBitDeterminedOff != 1 ||
		DoubleBitDeterminedOn != 2 || DoubleBitIndeterminate != 3 {
		t.Fatal("double-bit wire encoding changed")
	}
	if got := DoubleBitDeterminedOn.String(); got != "On" {
		t.Errorf("String() = %q, want On", got)
	}
}
