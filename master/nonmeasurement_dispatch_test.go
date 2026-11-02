package master

import (
	"encoding/binary"
	"math"
	"testing"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/objects"
)

// nonMeasurementCollector implements the optional handlers for the objects
// that have no measurement type.
type nonMeasurementCollector struct {
	NopHandler
	intervals  []dnp3.Indexed[dnp3.TimeAndInterval]
	deadbands  []dnp3.Indexed[float64]
	statistics []dnp3.Indexed[SecurityStatistic]
	infos      []HeaderInfo
}

func (c *nonMeasurementCollector) HandleTimeAndInterval(info HeaderInfo, v []dnp3.Indexed[dnp3.TimeAndInterval]) {
	c.infos = append(c.infos, info)
	c.intervals = append(c.intervals, v...)
}

func (c *nonMeasurementCollector) HandleDeadband(info HeaderInfo, v []dnp3.Indexed[float64]) {
	c.infos = append(c.infos, info)
	c.deadbands = append(c.deadbands, v...)
}

func (c *nonMeasurementCollector) HandleSecurityStatistic(info HeaderInfo, v []dnp3.Indexed[SecurityStatistic]) {
	c.infos = append(c.infos, info)
	c.statistics = append(c.statistics, v...)
}

// rangeHeader is a start-stop header with no index prefix, the shape static
// data arrives in.
func rangeHeader(group, variation uint8, start, stop uint32, data []byte) app.ObjectHeader {
	return app.ObjectHeader{
		Group: group, Variation: variation,
		Qualifier: app.MakeQualifier(app.PrefixNone, app.RangeStartStop8),
		Range:     app.Range{Spec: app.RangeStartStop8, Start: start, Stop: stop, Count: stop - start + 1},
		Data:      data,
	}
}

func TestTimeAndIntervalDispatch(t *testing.T) {
	c := &nonMeasurementCollector{}
	s := New(Config{}, c)

	at := time.UnixMilli(1_700_000_000_123).UTC()
	// Encoded as the outstation encodes a g50v4 read.
	data := objects.AppendTime48(nil, dnp3.Now(at))
	data = binary.LittleEndian.AppendUint32(data, 3600)
	data = append(data, 5)
	s.dispatch(rangeHeader(50, 4, 2, 2, data), objects.Context{})

	if len(c.intervals) != 1 {
		t.Fatalf("got %d time-and-interval values, want 1", len(c.intervals))
	}
	got := c.intervals[0]
	if got.Index != 2 || !got.Value.Time.Time.Equal(at) || got.Value.Interval != 3600 || got.Value.Units != 5 {
		t.Errorf("got %+v", got)
	}
	if c.infos[0].GV != objects.GV(50, 4) {
		t.Errorf("header GV = %v", c.infos[0].GV)
	}
}

func TestDeadbandDispatch(t *testing.T) {
	c := &nonMeasurementCollector{}
	s := New(Config{}, c)

	s.dispatch(rangeHeader(34, 1, 0, 1, []byte{10, 0, 0xFF, 0xFF}), objects.Context{})
	s.dispatch(rangeHeader(34, 2, 3, 3, binary.LittleEndian.AppendUint32(nil, 70000)), objects.Context{})
	s.dispatch(rangeHeader(34, 3, 4, 4, binary.LittleEndian.AppendUint32(nil, math.Float32bits(0.5))), objects.Context{})

	want := []dnp3.Indexed[float64]{{Index: 0, Value: 10}, {Index: 1, Value: 65535}, {Index: 3, Value: 70000}, {Index: 4, Value: 0.5}}
	if len(c.deadbands) != len(want) {
		t.Fatalf("got %v, want %v", c.deadbands, want)
	}
	for i := range want {
		if c.deadbands[i] != want[i] {
			t.Errorf("deadband %d = %+v, want %+v", i, c.deadbands[i], want[i])
		}
	}
}

func TestSecurityStatisticDispatch(t *testing.T) {
	c := &nonMeasurementCollector{}
	s := New(Config{}, c)

	// g121v1 static, as the outstation encodes it: flags, association, value.
	static := []byte{byte(dnp3.Online), 0, 0}
	static = binary.LittleEndian.AppendUint32(static, 42)
	s.dispatch(rangeHeader(121, 1, 5, 5, static), objects.Context{})

	// g122v2 event with an index prefix and a time.
	at := time.UnixMilli(1_700_000_000_000).UTC()
	event := []byte{7, byte(dnp3.Online), 3, 0}
	event = binary.LittleEndian.AppendUint32(event, 9)
	event = objects.AppendTime48(event, dnp3.Now(at))
	s.dispatch(app.ObjectHeader{
		Group: 122, Variation: 2,
		Qualifier: app.MakeQualifier(app.PrefixIndex1, app.RangeCount8),
		Range:     app.Range{Spec: app.RangeCount8, Count: 1},
		Data:      event,
	}, objects.Context{})

	if len(c.statistics) != 2 {
		t.Fatalf("got %d statistics, want 2", len(c.statistics))
	}
	if got := c.statistics[0]; got.Index != 5 || got.Value.Value != 42 || got.Value.Flags != dnp3.Online || c.infos[0].IsEvent() {
		t.Errorf("static = %+v (%+v)", got, c.infos[0])
	}
	if got := c.statistics[1]; got.Index != 7 || got.Value.Value != 9 || got.Value.Association != 3 || !got.Value.Time.Time.Equal(at) || !c.infos[1].IsEvent() {
		t.Errorf("event = %+v (%+v)", got, c.infos[1])
	}
}

// A handler that implements none of the optional interfaces is not called
// and does not panic: the objects are dropped, as they always were.
func TestNonMeasurementDispatchWithoutOptionalHandlers(t *testing.T) {
	s := New(Config{}, NopHandler{})
	s.dispatch(rangeHeader(34, 1, 0, 0, []byte{1, 0}), objects.Context{})
	s.dispatch(rangeHeader(121, 1, 0, 0, make([]byte, 7)), objects.Context{})
	s.dispatch(rangeHeader(50, 4, 0, 0, make([]byte, 11)), objects.Context{})
}

func TestRestartTaskReadsDelay(t *testing.T) {
	var ms uint32
	task := newRestartTask(dnp3.ColdRestart, &ms)
	task.onFragment(app.Fragment{Objects: []app.ObjectHeader{{Group: 52, Variation: 1, Data: []byte{3, 0}}}})
	if ms != 3000 {
		t.Errorf("g52v1 delay = %d ms, want 3000", ms)
	}
	task.onFragment(app.Fragment{Objects: []app.ObjectHeader{{Group: 52, Variation: 2, Data: []byte{0xF4, 0x01}}}})
	if ms != 500 {
		t.Errorf("g52v2 delay = %d ms, want 500", ms)
	}
}
