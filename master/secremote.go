package master

import (
	"fmt"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/internal/stack"
	"github.com/dscsystems/go-dnp3/secevent"
)

// secMaster is the master's state for its security events. It lives on the
// session goroutine and is reset for each connection.
type secMaster struct {
	// unsolOff is the classes the master has successfully disabled for
	// unsolicited reporting on this connection.
	unsolOff dnp3.Class
	// prevIIN is the last response's indications, for rising edges.
	prevIIN app.IIN
	// lastSeq is the sequence number of the last completed solicited
	// response, whose repeats are not unrequested responses.
	lastSeq    uint8
	hasLastSeq bool
}

// restartRate is how often the outstation may report a restart before
// remote.restart becomes a Warning.
var restartRate = secevent.Rate{N: 1, Per: time.Hour}

// respEvent returns an event about a response fragment.
func respEvent(k secevent.Kind, r stack.Received, h app.Header) secevent.Event {
	return secevent.Event{
		Kind:    k,
		Fields:  secevent.HasLinkAddrs | secevent.HasAppFunc | secevent.HasAppSeq | secevent.HasIIN,
		LinkSrc: r.Source, LinkDest: r.Dest,
		AppFunc: uint8(h.Func), AppSeq: h.Control.Seq, IIN: uint16(h.IIN),
		Raw: r.Fragment,
	}
}

// emitResp reports an event about a response fragment.
func (s *Session) emitResp(k secevent.Kind, r stack.Received, h app.Header, action secevent.Action, detail string) {
	if !s.sec.Enabled() {
		return
	}
	e := respEvent(k, r, h)
	e.Action, e.Detail = action, detail
	s.emit(e)
}

// remoteIndications reports rising edges of the outstation's indications
// that say something happened to it.
func (s *Session) remoteIndications(r stack.Received, h app.Header) {
	if !s.sec.Enabled() {
		return
	}
	iin, prev := h.IIN, s.secm.prevIIN
	s.secm.prevIIN = iin
	rose := func(bit app.IIN) bool { return iin.Has(bit) && !prev.Has(bit) }
	for _, c := range []struct {
		bit    app.IIN
		kind   secevent.Kind
		detail string
	}{
		{app.IINConfigCorrupt, secevent.KindRemoteConfigCorrupt, "the outstation reports its configuration corrupt"},
		{app.IINDeviceTrouble, secevent.KindRemoteDeviceTrouble, "the outstation reports device trouble"},
		{app.IINLocalControl, secevent.KindRemoteLocalControl, "the outstation reports points in local control"},
		{app.IINEventBufferOverflow, secevent.KindRemoteEventOverflow, "the outstation's event buffer overflowed: events were lost"},
	} {
		if rose(c.bit) {
			s.emitResp(c.kind, r, h, secevent.Observed, c.detail)
		}
	}
}

// remoteRestart reports the outstation restarting outside the startup
// sequence.
func (s *Session) remoteRestart(r stack.Received, h app.Header) {
	if !s.sec.Enabled() {
		return
	}
	e := respEvent(secevent.KindRemoteRestart, r, h)
	e.Detail = "the outstation reports it restarted"
	if s.sec.Over("remote.restart", restartRate) {
		e.Severity = secevent.Warning
		e.Detail += ", again within the hour"
	}
	s.emit(e)
}

// remoteRejected reports the outstation refusing a request of ours.
func (s *Session) remoteRejected(r stack.Received, frag app.Fragment, t *task) {
	if !s.sec.Enabled() {
		return
	}
	if errs := frag.Header.IIN & app.RequestErrorMask; errs != 0 {
		e := respEvent(secevent.KindRemoteRejected, r, frag.Header)
		e.Value = errs.String()
		e.Detail = fmt.Sprintf("%s refused: %v", t.name, errs)
		s.emit(e)
	}
	switch t.funcCode {
	case app.FuncSelect, app.FuncOperate, app.FuncDirectOperate:
		for i, st := range parseCommandStatuses(frag) {
			if st.OK() {
				continue
			}
			e := respEvent(secevent.KindRemoteRejected, r, frag.Header)
			e.Fields |= secevent.HasCommandStatus
			e.CommandStatus, e.Value = st, st.String()
			e.Detail = fmt.Sprintf("%s: command %d refused with %v", t.name, i, st)
			s.emit(e)
		}
	}
}

// unsolicitedMask notes a successful enable or disable of unsolicited
// reporting.
func (s *Session) unsolicitedMask(t *task) {
	switch t.funcCode {
	case app.FuncDisableUnsolicited:
		s.secm.unsolOff |= t.unsolMask
	case app.FuncEnableUnsolicited:
		s.secm.unsolOff &^= t.unsolMask
	}
}

// unsolicitedWhileDisabled reports event data arriving unsolicited although
// the master disabled every class. Event objects do not say which class
// they belong to, so only that case can be told for certain. The null
// unsolicited response that announces a restart carries nothing, and is
// exempt.
func (s *Session) unsolicitedWhileDisabled(r stack.Received, frag app.Fragment) {
	if !s.sec.Enabled() || s.secm.unsolOff&dnp3.Class123 != dnp3.Class123 || len(frag.Objects) == 0 {
		return
	}
	s.emitResp(secevent.KindUnsolWhileDisabled, r, frag.Header, secevent.Observed,
		fmt.Sprintf("unsolicited response with %d object header(s) although unsolicited reporting is disabled", len(frag.Objects)))
}
