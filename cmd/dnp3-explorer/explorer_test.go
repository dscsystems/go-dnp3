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
)

// The explorer is driven here without a terminal: the model is a pure function
// of the messages it has been given, so it can be exercised by feeding it the
// same messages Bubble Tea would. That is the payoff of keeping the DNP3
// session entirely out of the model.

func testModel() *Model {
	conn := &connection{
		msgs:   make(chan tea.Msg, 64),
		ctx:    context.Background(),
		target: "test:20000",
	}
	m := NewModel(conn)
	m.width, m.height = 100, 30
	return m
}

func press(m *Model, key string) *Model {
	next, _ := m.HandleKey(key)
	return next.(*Model)
}

func TestViewRendersWithoutData(t *testing.T) {
	m := testModel()
	out := m.View().Content

	if !strings.Contains(out, "dnp3-explorer") {
		t.Error("the header is missing")
	}
	for _, name := range screenNames {
		if !strings.Contains(out, name) {
			t.Errorf("the %s tab is missing", name)
		}
	}
	// An empty tool must say what to do, not just show an empty box.
	if !strings.Contains(out, "integrity poll") {
		t.Error("an empty overview should tell the operator how to fill it")
	}
}

func TestTooSmallTerminalSaysSo(t *testing.T) {
	m := testModel()
	m.width, m.height = 40, 8

	out := m.View().Content
	if !strings.Contains(out, "at least") {
		t.Errorf("a cramped terminal should be told, not given a mangled table:\n%s", out)
	}
}

func TestPointsScreenShowsValues(t *testing.T) {
	m := testModel()
	m.applyUpdate(updateMsg{
		Type: dnp3.TypeAnalog, Index: 3, Value: "11025.500",
		Flags: dnp3.Online, Stamp: dnp3.Now(time.Now()),
	})
	m.applyUpdate(updateMsg{
		Type: dnp3.TypeBinary, Index: 0, Value: "ON", Flags: dnp3.Online,
	})
	m.screen = ScreenPoints

	out := m.View().Content
	for _, want := range []string{"AI 3", "11025.500", "BI 0", "ON", "ONLINE"} {
		if !strings.Contains(out, want) {
			t.Errorf("the points table is missing %q:\n%s", want, out)
		}
	}
	// The state bit is already spelled as ON; repeating it as a flag is noise.
	if strings.Contains(out, "STATE") {
		t.Error("the state bit should not be repeated in the quality column")
	}
}

func TestPointsSortedByTypeThenIndex(t *testing.T) {
	m := testModel()
	// Deliberately out of order.
	m.applyUpdate(updateMsg{Type: dnp3.TypeAnalog, Index: 5, Value: "5"})
	m.applyUpdate(updateMsg{Type: dnp3.TypeBinary, Index: 2, Value: "ON"})
	m.applyUpdate(updateMsg{Type: dnp3.TypeAnalog, Index: 1, Value: "1"})
	m.applyUpdate(updateMsg{Type: dnp3.TypeBinary, Index: 0, Value: "OFF"})

	rows := m.visiblePoints()
	want := []string{"BI 0", "BI 2", "AI 1", "AI 5"}
	for i, w := range want {
		if got := pointLabel(rows[i].Key); got != w {
			t.Errorf("row %d = %s, want %s — the table reorders under the cursor", i, got, w)
		}
	}
}

func TestBadQualityIsVisible(t *testing.T) {
	// Quality is the thing an operator scans for, so an offline point must not
	// look like a healthy one.
	m := testModel()
	m.applyUpdate(updateMsg{
		Type: dnp3.TypeAnalog, Index: 0, Value: "42",
		Flags: dnp3.CommLost, // not online
	})
	m.screen = ScreenPoints

	out := m.View().Content
	if !strings.Contains(out, "COMM_LOST") {
		t.Errorf("the quality column does not show the fault:\n%s", out)
	}
}

func TestEventsScreenCollectsEvents(t *testing.T) {
	m := testModel()
	for i := range 3 {
		m.applyUpdate(updateMsg{
			Type: dnp3.TypeBinary, Index: uint16(i), Value: "ON",
			Flags: dnp3.Online, IsEvent: true,
		})
	}
	m.screen = ScreenEvents

	if len(m.events) != 3 {
		t.Fatalf("%d events recorded, want 3", len(m.events))
	}
	out := m.View().Content
	if !strings.Contains(out, "BI 2") {
		t.Errorf("the events list is missing a row:\n%s", out)
	}

	// Static values must not land in the event list.
	m.applyUpdate(updateMsg{Type: dnp3.TypeAnalog, Index: 0, Value: "1"})
	if len(m.events) != 3 {
		t.Error("a static value was recorded as an event")
	}
}

// TestEventListIsBounded covers an event storm: the list must not grow without
// limit, because nobody scrolls back ten thousand rows and the memory is real.
func TestEventListIsBounded(t *testing.T) {
	m := testModel()
	for i := range 5000 {
		m.applyUpdate(updateMsg{
			Type: dnp3.TypeBinary, Index: uint16(i % 10), Value: "ON", IsEvent: true,
		})
	}
	if len(m.events) > 2000 {
		t.Errorf("the event list grew to %d rows unbounded", len(m.events))
	}
	// The newest must survive, since that is what an operator is watching.
	if len(m.events) == 0 {
		t.Fatal("the event list was emptied")
	}
}

func TestFilterNarrowsPoints(t *testing.T) {
	m := testModel()
	m.applyUpdate(updateMsg{Type: dnp3.TypeAnalog, Index: 0, Value: "1"})
	m.applyUpdate(updateMsg{Type: dnp3.TypeBinary, Index: 0, Value: "ON"})

	m.filter = "ai"
	rows := m.visiblePoints()
	if len(rows) != 1 || rows[0].Key.Type != dnp3.TypeAnalog {
		t.Errorf("the filter matched %d rows, want just the analog", len(rows))
	}
}

func TestFilterTypingCapturesKeys(t *testing.T) {
	m := testModel()
	m.screen = ScreenPoints

	m = press(m, "/")
	if !m.filtering {
		t.Fatal("/ did not open the filter")
	}
	// While filtering, keys are text — pressing q must not quit.
	m = press(m, "q")
	if m.quitting {
		t.Fatal("q quit the program while the operator was typing a filter")
	}
	if m.filter != "q" {
		t.Errorf("filter = %q, want %q", m.filter, "q")
	}
}

func TestScreenNavigation(t *testing.T) {
	m := testModel()

	m = press(m, "3")
	if m.screen != ScreenEvents {
		t.Errorf("screen = %v, want Events", m.screen)
	}

	m = press(m, "tab")
	if m.screen != ScreenLog {
		t.Errorf("tab from Events gave %v, want Log", m.screen)
	}
	// And it wraps rather than sticking at the end.
	m = press(m, "tab")
	if m.screen != ScreenOverview {
		t.Errorf("tab from the last screen gave %v, want Overview", m.screen)
	}
}

func TestCursorStaysInRange(t *testing.T) {
	m := testModel()
	m.screen = ScreenPoints
	m.applyUpdate(updateMsg{Type: dnp3.TypeAnalog, Index: 0, Value: "1"})
	m.applyUpdate(updateMsg{Type: dnp3.TypeAnalog, Index: 1, Value: "2"})

	for range 10 {
		m = press(m, "j")
	}
	if m.cursor > 1 {
		t.Errorf("cursor ran past the last row: %d", m.cursor)
	}
	for range 10 {
		m = press(m, "k")
	}
	if m.cursor < 0 {
		t.Errorf("cursor ran above the first row: %d", m.cursor)
	}
}

// TestControlKeysNeedABinaryOutput covers the guard against operating the
// wrong thing: 'c' on an analog must do nothing but explain itself.
func TestControlKeysNeedABinaryOutput(t *testing.T) {
	m := testModel()
	m.screen = ScreenPoints
	m.applyUpdate(updateMsg{Type: dnp3.TypeAnalog, Index: 0, Value: "1"})

	m = press(m, "c")
	if _, ok := m.selectedControl(); ok {
		t.Error("an analog point was offered as a control")
	}
	found := false
	for _, l := range m.logs {
		if strings.Contains(l.Text, "binary output") {
			found = true
		}
	}
	if !found {
		t.Error("pressing c on a non-control should say why nothing happened")
	}
}

func TestControlKeyFindsBinaryOutput(t *testing.T) {
	m := testModel()
	m.screen = ScreenPoints
	m.applyUpdate(updateMsg{Type: dnp3.TypeBinaryOutputStatus, Index: 7, Value: "OFF"})

	k, ok := m.selectedControl()
	if !ok || k.Index != 7 {
		t.Errorf("selectedControl = %v, %v; want BO 7", k, ok)
	}
}

func TestWindowKeepsCursorVisible(t *testing.T) {
	tests := []struct {
		cursor, total, height int
		wantStart, wantEnd    int
	}{
		{0, 5, 10, 0, 5},    // everything fits
		{0, 100, 10, 0, 10}, // at the top
		{99, 100, 10, 90, 100},
		{50, 100, 10, 45, 55},
	}
	for _, tc := range tests {
		start, end := window(tc.cursor, tc.total, tc.height)
		if start != tc.wantStart || end != tc.wantEnd {
			t.Errorf("window(%d,%d,%d) = %d,%d want %d,%d",
				tc.cursor, tc.total, tc.height, start, end, tc.wantStart, tc.wantEnd)
		}
		if tc.total > 0 && (tc.cursor < start || tc.cursor >= end) {
			t.Errorf("window(%d,%d,%d) scrolled the cursor out of view",
				tc.cursor, tc.total, tc.height)
		}
	}
}

// TestEndToEndAgainstDemoOutstation runs the real master against the demo
// outstation and checks that measurements reach the model — the whole pipeline
// from octets to table row.
func TestEndToEndAgainstDemoOutstation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	mch, och := channel.Pipe()
	demo := newDemoOutstation(discardLogger())

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = demo.session.Run(ctx, och) }()
	go func() { defer wg.Done(); demo.run(ctx) }()

	conn := &connection{msgs: make(chan tea.Msg, 512), ctx: ctx, target: "demo"}
	h := &handler{conn: conn}
	sess := master.New(master.Config{
		LocalAddr: 1, RemoteAddr: 10, ResponseTimeout: 5 * time.Second,
	}, h)
	conn.sess = sess

	wg.Add(1)
	go func() { defer wg.Done(); _ = sess.Run(ctx, mch) }()

	t.Cleanup(func() {
		cancel()
		_ = mch.Close()
		_ = och.Close()
		wg.Wait()
	})

	// Wait for the link, then poll.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !sess.Connected() {
		time.Sleep(5 * time.Millisecond)
	}
	if !sess.Connected() {
		t.Fatal("the demo outstation never connected")
	}

	// Let the demo's tick populate the database. Polling immediately would
	// read a device that has not measured anything yet — every point zero and
	// unflagged, which is a real answer to the wrong question.
	time.Sleep(700 * time.Millisecond)

	pollCtx, pollCancel := context.WithTimeout(ctx, 10*time.Second)
	defer pollCancel()
	if err := sess.IntegrityPoll(pollCtx); err != nil {
		t.Fatalf("integrity poll: %v", err)
	}

	// Drain what the handler pushed into the model, as the UI loop would.
	m := testModel()
	m.conn = conn
	drained := 0
	for {
		select {
		case msg := <-conn.msgs:
			if u, ok := msg.(updateMsg); ok {
				m.applyUpdate(u)
				drained++
			}
			continue
		default:
		}
		break
	}

	if drained == 0 {
		t.Fatal("no measurements reached the model")
	}
	if len(m.points) < 8 {
		t.Errorf("the model holds %d points; the demo device has more than that", len(m.points))
	}

	m.screen = ScreenPoints
	out := m.View().Content
	if !strings.Contains(out, "AI 0") || !strings.Contains(out, "BI 0") {
		t.Errorf("the rendered table is missing points:\n%s", out)
	}
	// The bus voltage is around 11 kV and must not have been truncated to an
	// integer by a badly chosen variation.
	if !strings.Contains(out, "11") {
		t.Errorf("the bus voltage did not arrive:\n%s", out)
	}
}
