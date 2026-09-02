package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/channel"
	"github.com/dscsystems/go-dnp3/master"
	"github.com/dscsystems/go-dnp3/objects"
)

// The Device panel is the answer to the question the tool is named for, so
// what it must never do is claim something the device did not say: an
// unreadable attribute, a device with none at all, and a device that has been
// replaced by another at the same address all have to be distinguishable.

func deviceAttrs() []dnp3.Attribute {
	return []dnp3.Attribute{
		objects.StringAttribute(attrVendorName, "DSC Systems"),
		objects.StringAttribute(attrProductName, "RTU-9000"),
		objects.StringAttribute(attrSoftwareVersion, "2.1.0"),
		objects.StringAttribute(attrSerialNumber, "SN-7"),
		objects.UintAttribute(226, 32),
		objects.UintAttribute(220, 16),
		objects.UintAttribute(attrMaxTxFragment, 2048),
	}
}

func TestDevicePanel(t *testing.T) {
	m := testModel()
	m.applyAttributes(attributesMsg{attrs: deviceAttrs()})

	panel := strings.Join(m.overviewDevice(20), "\n")

	// The nameplate comes first, in the order somebody reads it off the box.
	for _, want := range []string{"DSC Systems", "RTU-9000", "2.1.0", "SN-7"} {
		if !strings.Contains(panel, want) {
			t.Errorf("the panel does not show %q:\n%s", want, panel)
		}
	}
	if vendor, product := strings.Index(panel, "DSC Systems"), strings.Index(panel, "RTU-9000"); vendor > product {
		t.Error("the product is listed above the vendor")
	}
	if product, version := strings.Index(panel, "RTU-9000"), strings.Index(panel, "2.1.0"); product > version {
		t.Error("the version is listed above the product")
	}

	// The counts are one row rather than one row each.
	if !strings.Contains(panel, "32 BI") || !strings.Contains(panel, "16 AI") {
		t.Errorf("the point counts are not summarised:\n%s", panel)
	}
	if strings.Contains(panel, "number of binary inputs") {
		t.Error("a count got a row of its own")
	}
	if !strings.Contains(panel, "2048 tx") {
		t.Errorf("the fragment size is missing:\n%s", panel)
	}
}

// A panel with less room than content says so rather than silently dropping
// the tail.
func TestDevicePanelTruncates(t *testing.T) {
	m := testModel()
	m.applyAttributes(attributesMsg{attrs: deviceAttrs()})

	panel := strings.Join(m.overviewDevice(3), "\n")
	if lines := len(m.overviewDevice(3)); lines != 3 {
		t.Errorf("panel drew %d rows for a budget of 3", lines)
	}
	if !strings.Contains(panel, "more") {
		t.Errorf("the panel hides rows without saying so:\n%s", panel)
	}
	// What survives is the nameplate.
	if !strings.Contains(panel, "DSC Systems") {
		t.Errorf("the vendor was dropped:\n%s", panel)
	}
}

// A device that does not implement attributes is a fact to report, not an
// error to bury: an operator should not have to read the log to find out.
func TestDevicePanelUnsupported(t *testing.T) {
	m := testModel()
	m.applyAttributes(attributesMsg{unsupported: true})

	panel := strings.Join(m.overviewDevice(10), "\n")
	if !strings.Contains(panel, "no attributes") {
		t.Errorf("panel = %q", panel)
	}
	if got := m.deviceSummary(); got != "not reported" {
		t.Errorf("summary = %q", got)
	}
}

func TestDevicePanelError(t *testing.T) {
	m := testModel()
	m.applyAttributes(attributesMsg{err: "dnp3: timeout"})

	panel := strings.Join(m.overviewDevice(10), "\n")
	if !strings.Contains(panel, "timeout") {
		t.Errorf("the panel hides the error: %q", panel)
	}
	if !strings.Contains(panel, "press a") {
		t.Errorf("the panel does not say how to retry: %q", panel)
	}
}

// The summary line names the device, because the address says where the tool
// is pointed and not what answered.
func TestDeviceSummary(t *testing.T) {
	m := testModel()
	if got := m.deviceSummary(); got != "—" {
		t.Errorf("before any read the summary is %q", got)
	}

	m.applyAttributes(attributesMsg{attrs: deviceAttrs()})
	got := m.deviceSummary()
	for _, want := range []string{"DSC Systems", "RTU-9000", "SN-7"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary %q does not contain %q", got, want)
		}
	}
}

// A dropped link forgets what the device said: the address may be answered by
// something else the next time it comes up.
func TestDeviceForgottenOnReconnect(t *testing.T) {
	m := testModel()
	m.connected = true
	m.applyAttributes(attributesMsg{attrs: deviceAttrs()})

	m.applyStatus(statusMsg{connected: false})
	if len(m.device.attrs) != 0 {
		t.Error("the old device's attributes survived the link going down")
	}
	if m.device.asked {
		t.Error("the next connection will not ask again")
	}

	// And coming back up asks once.
	cmd := m.applyStatus(statusMsg{connected: true})
	if cmd == nil {
		t.Fatal("reconnecting did not schedule an attribute read")
	}
	if again := m.applyStatus(statusMsg{connected: true}); again != nil {
		t.Error("a second status message scheduled a second read")
	}
}

// Attributes the library has no name for are shown by number rather than
// guessed at, and a device's own set is kept distinct.
func TestDevicePanelUnknownAttributes(t *testing.T) {
	m := testModel()
	m.applyAttributes(attributesMsg{attrs: []dnp3.Attribute{
		objects.StringAttribute(17, "private"),
		{Set: 4, Variation: 250, Type: dnp3.AttrVisibleString, Text: "other set"},
	}})

	panel := strings.Join(m.overviewDevice(10), "\n")
	if !strings.Contains(panel, "attribute 17") {
		t.Errorf("an unnamed attribute is not numbered:\n%s", panel)
	}
	if !strings.Contains(panel, "set 4") {
		t.Errorf("an attribute outside the standard set is not marked:\n%s", panel)
	}
	// It must not be labelled "product name and model" just because 250 is.
	if strings.Contains(panel, "product") {
		t.Errorf("a non-standard set borrowed a standard name:\n%s", panel)
	}
}

// The whole path, against the demo device: the key, the request, the panel.
func TestDeviceAttributesEndToEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	mch, och := channel.Pipe()
	demo := newDemoOutstation(discardLogger())

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = demo.session.Run(ctx, och) }()

	conn := &connection{msgs: make(chan tea.Msg, 64), ctx: ctx}
	sess := master.New(master.Config{
		LocalAddr: 1, RemoteAddr: 10, ResponseTimeout: 5 * time.Second,
	}, nil)
	conn.adopt(sess, link{Demo: true, Local: 1, Remote: 10})

	wg.Add(1)
	go func() { defer wg.Done(); _ = sess.Run(ctx, mch) }()
	t.Cleanup(func() {
		cancel()
		_ = mch.Close()
		_ = och.Close()
		wg.Wait()
	})

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !sess.Connected() {
		time.Sleep(5 * time.Millisecond)
	}
	if !sess.Connected() {
		t.Fatal("the demo outstation never connected")
	}

	m := testModel()
	m.conn = conn

	// Pressing the key issues the read.
	m, cmd := pressCmd(m, "a")
	if cmd == nil {
		t.Fatal("a issued no command")
	}
	msg, ok := cmd().(attributesMsg)
	if !ok {
		t.Fatalf("the command returned %T", msg)
	}
	if msg.err != "" || msg.unsupported {
		t.Fatalf("read failed: %+v", msg)
	}
	m.applyAttributes(msg)

	panel := strings.Join(m.overviewDevice(20), "\n")
	for _, want := range []string{"GO-DNP3 DEMO RTU", "DSC Systems", "1.0.0-demo"} {
		if !strings.Contains(panel, want) {
			t.Errorf("the panel does not show %q:\n%s", want, panel)
		}
	}
	// The counts come from the demo device's own database, not from anything
	// configured: six binaries and six analogs is what it has.
	if !strings.Contains(panel, "6 BI") || !strings.Contains(panel, "6 AI") {
		t.Errorf("the derived counts are wrong:\n%s", panel)
	}
}
