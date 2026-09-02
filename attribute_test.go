package dnp3_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/channel"
	"github.com/dscsystems/go-dnp3/master"
	"github.com/dscsystems/go-dnp3/objects"
	"github.com/dscsystems/go-dnp3/outstation"
)

// Device attributes are the one exchange where the variation carries meaning
// rather than encoding, so the thing to prove end to end is that an attribute
// arrives identified as well as valued: the right number, the right set, and a
// value of the right type.

// attributePair starts a master and an outstation reporting the given
// attributes.
func attributePair(t *testing.T, attrs []dnp3.Attribute) *master.Session {
	t.Helper()

	mch, och := channel.Pipe()

	out := outstation.New(outstation.Config{
		LocalAddr:  10,
		RemoteAddr: 1,
		Database: outstation.DatabaseConfig{
			Binary: 6, Analog: 3, Counter: 2, BinaryOutputStatus: 4,
		},
		ConfirmTimeout: time.Second,
		Attributes:     attrs,
	}, nil, nil)

	m := master.New(master.Config{
		LocalAddr: 1, RemoteAddr: 10, ResponseTimeout: 2 * time.Second,
	}, nil)

	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = out.Run(ctx, och) }()
	go func() { defer wg.Done(); _ = m.Run(ctx, mch) }()

	t.Cleanup(func() {
		cancel()
		_ = mch.Close()
		_ = och.Close()
		wg.Wait()
	})

	waitFor(t, 3*time.Second, func() bool { return m.Connected() })
	return m
}

func TestReadDeviceAttributes(t *testing.T) {
	m := attributePair(t, []dnp3.Attribute{
		objects.StringAttribute(252, "DSC Systems"),
		objects.StringAttribute(250, "GO-DNP3 RTU"),
		objects.StringAttribute(248, "SN-00042"),
		objects.StringAttribute(242, "1.2.3"),
	})

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	attrs, err := m.ReadAttributes(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	byVar := map[uint8]dnp3.Attribute{}
	for _, a := range attrs {
		byVar[a.Variation] = a
	}

	for variation, want := range map[uint8]string{
		252: "DSC Systems",
		250: "GO-DNP3 RTU",
		248: "SN-00042",
		242: "1.2.3",
	} {
		got, ok := byVar[variation]
		if !ok {
			t.Errorf("attribute %d did not come back", variation)
			continue
		}
		if got.Value() != want {
			t.Errorf("attribute %d = %q, want %q", variation, got.Value(), want)
		}
		if got.Type != dnp3.AttrVisibleString {
			t.Errorf("attribute %d has type %s", variation, got.Type)
		}
		if got.Set != dnp3.AttrSetStandard {
			t.Errorf("attribute %d came back in set %d", variation, got.Set)
		}
	}

	// The counts the outstation derives from its own database have to match
	// the database a master is about to poll, or they are worse than absent.
	for variation, want := range map[uint8]int64{
		outstation.AttrBinaryInputCount:  6,
		outstation.AttrAnalogInputCount:  3,
		outstation.AttrCounterCount:      2,
		outstation.AttrBinaryOutputCount: 4,
	} {
		got, ok := byVar[variation]
		if !ok {
			t.Errorf("derived attribute %d did not come back", variation)
			continue
		}
		if got.Number != want {
			t.Errorf("attribute %d = %d, want %d", variation, got.Number, want)
		}
		if got.Type != dnp3.AttrUnsignedInt {
			t.Errorf("attribute %d has type %s, want a number", variation, got.Type)
		}
	}

	// A point type the device does not have is not reported at all: "none"
	// and "I did not say" are different answers.
	if _, ok := byVar[outstation.AttrDoubleBitInputCount]; ok {
		t.Error("a point type with no points was reported")
	}
}

func TestReadOneDeviceAttribute(t *testing.T) {
	m := attributePair(t, []dnp3.Attribute{
		objects.StringAttribute(250, "GO-DNP3 RTU"),
		objects.StringAttribute(252, "DSC Systems"),
	})

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	a, err := m.ReadAttribute(ctx, dnp3.AttrSetStandard, 250)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if a.Value() != "GO-DNP3 RTU" || a.Variation != 250 {
		t.Errorf("got %+v", a)
	}
	if a.Name() != "product name and model" {
		t.Errorf("name %q", a.Name())
	}

	// One the device does not have is a clear refusal rather than an empty
	// answer a caller has to interpret.
	if _, err := m.ReadAttribute(ctx, dnp3.AttrSetStandard, 199); !errors.Is(err, dnp3.ErrNotSupported) {
		t.Errorf("error %v, want one wrapping dnp3.ErrNotSupported", err)
	}
}

// Every attribute type has to survive the round trip, because a device that
// reports its point count as a string and its name as octets is well within
// the standard.
func TestAttributeTypesOverTheWire(t *testing.T) {
	when := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	m := attributePair(t, []dnp3.Attribute{
		{Variation: 200, Type: dnp3.AttrVisibleString, Text: "text"},
		{Variation: 201, Type: dnp3.AttrUnsignedInt, Number: 65000},
		{Variation: 202, Type: dnp3.AttrSignedInt, Number: -12345},
		{Variation: 203, Type: dnp3.AttrFloat, Real: 1.5},
		{Variation: 204, Type: dnp3.AttrOctetString, Octets: []byte{0xDE, 0xAD}},
		{Variation: 205, Type: dnp3.AttrTime, Time: when},
	})

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	attrs, err := m.ReadAttributes(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	byVar := map[uint8]dnp3.Attribute{}
	for _, a := range attrs {
		byVar[a.Variation] = a
	}

	if got := byVar[200]; got.Text != "text" {
		t.Errorf("string %q", got.Text)
	}
	if got := byVar[201]; got.Number != 65000 {
		t.Errorf("uint %d", got.Number)
	}
	if got := byVar[202]; got.Number != -12345 {
		t.Errorf("int %d", got.Number)
	}
	if got := byVar[203]; got.Real != 1.5 {
		t.Errorf("float %v", got.Real)
	}
	if got := byVar[204]; got.Value() != "DE AD" {
		t.Errorf("octets %q", got.Value())
	}
	if got := byVar[205]; !got.Time.Equal(when) {
		t.Errorf("time %v, want %v", got.Time, when)
	}
}

// A device with attributes in a set of its own keeps them apart from the
// standard's, and a master asking for one set does not get the other.
func TestAttributeSets(t *testing.T) {
	m := attributePair(t, []dnp3.Attribute{
		objects.StringAttribute(250, "standard set"),
		{Set: 7, Variation: 250, Type: dnp3.AttrVisibleString, Text: "private set"},
	})

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	std, err := m.ReadAttributes(ctx)
	if err != nil {
		t.Fatalf("standard set: %v", err)
	}
	for _, a := range std {
		if a.Set != 0 {
			t.Errorf("set %d came back from a read of set 0", a.Set)
		}
		if a.Variation == 250 && a.Value() != "standard set" {
			t.Errorf("attribute 250 = %q", a.Value())
		}
	}

	private, err := m.ReadAttributeSet(ctx, 7)
	if err != nil {
		t.Fatalf("set 7: %v", err)
	}
	if len(private) != 1 {
		t.Fatalf("set 7 returned %d attributes, want 1", len(private))
	}
	if private[0].Value() != "private set" || private[0].Set != 7 {
		t.Errorf("got %+v", private[0])
	}
	// An attribute outside the standard set is named by its number, because
	// nothing in this library knows what a device calls its own.
	if name := private[0].Name(); !strings.Contains(name, "set 7") {
		t.Errorf("name %q does not identify the set", name)
	}
}

// An outstation configured with no attributes still answers, because the
// counts it derives from its own database are always true of it.
func TestAttributesAlwaysReportTheDatabase(t *testing.T) {
	m := attributePair(t, nil)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	attrs, err := m.ReadAttributes(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(attrs) == 0 {
		t.Fatal("nothing came back")
	}

	for _, a := range attrs {
		if a.Variation == outstation.AttrBinaryInputCount && a.Number != 6 {
			t.Errorf("binary input count = %d, want 6", a.Number)
		}
		if a.Variation == outstation.AttrMaxRxFragment && a.Number == 0 {
			t.Error("the receive fragment size was reported as zero")
		}
	}
}

// Reading attributes must not disturb the measurement path: they travel in the
// same fragment shape as everything else, and the framing layer has to walk
// past one to reach the other.
func TestAttributesDoNotBreakPolling(t *testing.T) {
	m := attributePair(t, []dnp3.Attribute{objects.StringAttribute(250, "RTU")})

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	if _, err := m.ReadAttributes(ctx); err != nil {
		t.Fatalf("attributes: %v", err)
	}
	if err := m.IntegrityPoll(ctx); err != nil {
		t.Fatalf("poll after an attribute read: %v", err)
	}
	if _, err := m.ReadAttributes(ctx); err != nil {
		t.Fatalf("attributes after a poll: %v", err)
	}
}
