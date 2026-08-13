// Package master implements the DNP3 master role: the station that polls
// outstations, receives their events, and issues commands.
package master

import (
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/objects"
)

// ResponseInfo describes the fragment a set of measurements arrived in.
type ResponseInfo struct {
	// IIN is the internal indications the outstation reported.
	IIN app.IIN
	// Unsolicited reports whether the fragment arrived unsolicited rather
	// than in answer to a poll.
	Unsolicited bool
	// Sequence is the application sequence number.
	Sequence uint8
	// Received is when the fragment was decoded.
	Received time.Time
}

// HeaderInfo describes the object header a measurement came from.
//
// Consumers need this more often than it looks: the same analog point read as
// a static value and received as an event mean different things to a
// historian, and only the group tells them apart.
type HeaderInfo struct {
	GV objects.GroupVar
	// Kind distinguishes a static value from an event.
	Kind objects.Kind
	// Class is the event class, when the measurement came from a class poll.
	Class dnp3.Class
}

// IsEvent reports whether the measurement came from an event object.
func (h HeaderInfo) IsEvent() bool { return h.Kind == objects.KindEvent }

// Handler receives the measurements a master decodes.
//
// BeginFragment and EndFragment bracket every fragment, so a consumer that
// needs a consistent set — a database transaction, a UI repaint — has somewhere
// to open and close it.
//
// Handler methods are called from the session goroutine. A slow handler
// delays the session's polling, so anything expensive belongs behind a queue;
// [ChannelHandler] is that queue.
type Handler interface {
	BeginFragment(info ResponseInfo)
	EndFragment(info ResponseInfo)

	HandleBinary(info HeaderInfo, values []dnp3.Indexed[dnp3.Binary])
	HandleDoubleBit(info HeaderInfo, values []dnp3.Indexed[dnp3.DoubleBitBinary])
	HandleCounter(info HeaderInfo, values []dnp3.Indexed[dnp3.Counter])
	HandleFrozenCounter(info HeaderInfo, values []dnp3.Indexed[dnp3.FrozenCounter])
	HandleAnalog(info HeaderInfo, values []dnp3.Indexed[dnp3.Analog])
	HandleBinaryOutputStatus(info HeaderInfo, values []dnp3.Indexed[dnp3.BinaryOutputStatus])
	HandleAnalogOutputStatus(info HeaderInfo, values []dnp3.Indexed[dnp3.AnalogOutputStatus])
	// HandleOctetString receives group 110 and 111 objects: the point names,
	// firmware versions and serial numbers a device reports as text rather
	// than as measurements.
	HandleOctetString(info HeaderInfo, values []dnp3.Indexed[dnp3.OctetString])
}

// NopHandler discards everything. Embed it to implement only the methods you
// care about.
type NopHandler struct{}

func (NopHandler) BeginFragment(ResponseInfo) {}
func (NopHandler) EndFragment(ResponseInfo)   {}

func (NopHandler) HandleBinary(HeaderInfo, []dnp3.Indexed[dnp3.Binary])             {}
func (NopHandler) HandleDoubleBit(HeaderInfo, []dnp3.Indexed[dnp3.DoubleBitBinary]) {}
func (NopHandler) HandleCounter(HeaderInfo, []dnp3.Indexed[dnp3.Counter])           {}
func (NopHandler) HandleFrozenCounter(HeaderInfo, []dnp3.Indexed[dnp3.FrozenCounter]) {
}
func (NopHandler) HandleAnalog(HeaderInfo, []dnp3.Indexed[dnp3.Analog]) {}
func (NopHandler) HandleBinaryOutputStatus(HeaderInfo, []dnp3.Indexed[dnp3.BinaryOutputStatus]) {
}
func (NopHandler) HandleAnalogOutputStatus(HeaderInfo, []dnp3.Indexed[dnp3.AnalogOutputStatus]) {
}
func (NopHandler) HandleOctetString(HeaderInfo, []dnp3.Indexed[dnp3.OctetString]) {}

// Update is one measurement delivered through a [ChannelHandler].
type Update struct {
	Info     HeaderInfo
	Fragment ResponseInfo

	Type  dnp3.PointType
	Index uint16

	Binary        dnp3.Binary
	DoubleBit     dnp3.DoubleBitBinary
	Counter       dnp3.Counter
	FrozenCounter dnp3.FrozenCounter
	Analog        dnp3.Analog
	BinaryOutput  dnp3.BinaryOutputStatus
	AnalogOutput  dnp3.AnalogOutputStatus
	OctetString   dnp3.OctetString
}

// ChannelHandler fans measurements into a Go channel.
//
// This is what a terminal UI or a recorder consumes: the session goroutine
// stays responsive because it only ever does a non-blocking send, and the
// consumer reads at its own pace. Updates are dropped rather than blocking the
// session when the consumer falls behind, and the drop is counted — a stalled
// UI must not stall the protocol.
type ChannelHandler struct {
	NopHandler

	ch      chan Update
	dropped uint64
	info    ResponseInfo
}

// NewChannelHandler returns a handler delivering to a buffered channel.
func NewChannelHandler(buffer int) *ChannelHandler {
	if buffer <= 0 {
		buffer = 256
	}
	return &ChannelHandler{ch: make(chan Update, buffer)}
}

// Updates returns the channel measurements are delivered on.
func (h *ChannelHandler) Updates() <-chan Update { return h.ch }

// Dropped returns how many updates were discarded because the consumer was
// not keeping up.
func (h *ChannelHandler) Dropped() uint64 { return h.dropped }

func (h *ChannelHandler) BeginFragment(info ResponseInfo) { h.info = info }

func (h *ChannelHandler) send(u Update) {
	u.Fragment = h.info
	select {
	case h.ch <- u:
	default:
		h.dropped++
	}
}

func (h *ChannelHandler) HandleBinary(info HeaderInfo, values []dnp3.Indexed[dnp3.Binary]) {
	for _, v := range values {
		h.send(Update{Info: info, Type: dnp3.TypeBinary, Index: v.Index, Binary: v.Value})
	}
}

func (h *ChannelHandler) HandleDoubleBit(info HeaderInfo, values []dnp3.Indexed[dnp3.DoubleBitBinary]) {
	for _, v := range values {
		h.send(Update{Info: info, Type: dnp3.TypeDoubleBitBinary, Index: v.Index, DoubleBit: v.Value})
	}
}

func (h *ChannelHandler) HandleCounter(info HeaderInfo, values []dnp3.Indexed[dnp3.Counter]) {
	for _, v := range values {
		h.send(Update{Info: info, Type: dnp3.TypeCounter, Index: v.Index, Counter: v.Value})
	}
}

func (h *ChannelHandler) HandleFrozenCounter(info HeaderInfo, values []dnp3.Indexed[dnp3.FrozenCounter]) {
	for _, v := range values {
		h.send(Update{Info: info, Type: dnp3.TypeFrozenCounter, Index: v.Index, FrozenCounter: v.Value})
	}
}

func (h *ChannelHandler) HandleAnalog(info HeaderInfo, values []dnp3.Indexed[dnp3.Analog]) {
	for _, v := range values {
		h.send(Update{Info: info, Type: dnp3.TypeAnalog, Index: v.Index, Analog: v.Value})
	}
}

func (h *ChannelHandler) HandleBinaryOutputStatus(info HeaderInfo, values []dnp3.Indexed[dnp3.BinaryOutputStatus]) {
	for _, v := range values {
		h.send(Update{Info: info, Type: dnp3.TypeBinaryOutputStatus, Index: v.Index, BinaryOutput: v.Value})
	}
}

func (h *ChannelHandler) HandleAnalogOutputStatus(info HeaderInfo, values []dnp3.Indexed[dnp3.AnalogOutputStatus]) {
	for _, v := range values {
		h.send(Update{Info: info, Type: dnp3.TypeAnalogOutputStatus, Index: v.Index, AnalogOutput: v.Value})
	}
}

func (h *ChannelHandler) HandleOctetString(info HeaderInfo, values []dnp3.Indexed[dnp3.OctetString]) {
	for _, v := range values {
		h.send(Update{Info: info, Type: dnp3.TypeOctetString, Index: v.Index, OctetString: v.Value})
	}
}
