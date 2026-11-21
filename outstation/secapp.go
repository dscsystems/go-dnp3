package outstation

import (
	"errors"
	"fmt"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/internal/stack"
	"github.com/dscsystems/go-dnp3/secevent"
)

// secRequest is what the security events about the request being handled
// need to know. It lives on the session goroutine.
type secRequest struct {
	r   stack.Received
	hdr app.Header
	// objects are the request's (group, variation) pairs, for scan keys.
	objects [][2]uint8
	// pending is the request error indications already set before this
	// request, which belong to an earlier one; sent is those the response
	// to this request carried, once one went out; replay marks a response
	// repeated from the cache, which is not a new refusal.
	pending app.IIN
	sent    app.IIN
	didSend bool
	replay  bool
	// dispatched says the request reached its function's handler, and
	// unsolBefore is the unsolicited classes enabled before it did.
	dispatched  bool
	unsolBefore dnp3.Class
}

// appEvent returns an event about request r, carrying its addresses and,
// when hdr is known, its function and sequence.
func appEvent(k secevent.Kind, r stack.Received, hdr *app.Header) secevent.Event {
	e := secevent.Event{
		Kind:     k,
		Fields:   secevent.HasLinkAddrs,
		LinkSrc:  r.Source,
		LinkDest: r.Dest,
		Raw:      r.Fragment,
	}
	if hdr != nil {
		e.Fields |= secevent.HasAppFunc | secevent.HasAppSeq
		e.AppFunc = uint8(hdr.Func)
		e.AppSeq = hdr.Control.Seq
	}
	return e
}

// emitApp reports an event about request r.
func (s *Session) emitApp(k secevent.Kind, r stack.Received, hdr *app.Header, action secevent.Action, detail string, err error) {
	if !s.sec.Enabled() {
		return
	}
	e := appEvent(k, r, hdr)
	e.Action, e.Detail, e.Err = action, detail, err
	s.emit(e)
}

// beginRequest starts tracking the request about to be handled.
func (s *Session) beginRequest(r stack.Received) {
	if !s.sec.Enabled() {
		return
	}
	s.secReq = secRequest{r: r, pending: s.iin & app.RequestErrorMask}
	if hdr, _, err := app.ParseHeader(r.Fragment); err == nil {
		s.secReq.hdr = hdr
	}
}

// parsedRequest records the parsed request.
func (s *Session) parsedRequest(frag app.Fragment) {
	if !s.sec.Enabled() {
		return
	}
	s.secReq.hdr = frag.Header
	s.secReq.unsolBefore = s.unsolClasses
	s.secReq.objects = s.secReq.objects[:0]
	for _, h := range frag.Objects {
		s.secReq.objects = append(s.secReq.objects, [2]uint8{h.Group, h.Variation})
	}
}

// responseSent notes the request error indications a response carries.
func (s *Session) responseSent() {
	if !s.sec.Enabled() || s.secReq.didSend {
		return
	}
	s.secReq.didSend = true
	s.secReq.sent = s.iin & app.RequestErrorMask
}

// endRequest reports the request error indications this request earned,
// whether its response carried them or, for a request that is not answered,
// they wait for the next one. They are read off the IIN bits rather than at
// every site that sets them, so no refusal goes unreported.
func (s *Session) endRequest() {
	if !s.sec.Enabled() {
		return
	}
	q := &s.secReq
	if q.replay {
		return
	}
	errs := s.iin & app.RequestErrorMask
	if q.didSend {
		errs = q.sent
	}
	errs &^= q.pending
	s.auditControl(errs)
	if errs == 0 {
		return
	}
	hdr := &q.hdr
	fc := uint8(hdr.Func)
	if errs&app.IINNoFuncCodeSupport != 0 {
		s.emitApp(secevent.KindAccessUnsupportedFunction, q.r, hdr, secevent.Rejected,
			fmt.Sprintf("function %s is not supported", hdr.Func), nil)
		s.sec.Unknown(fmt.Sprintf("f%d", fc))
	}
	if errs&app.IINObjectUnknown != 0 {
		s.emitApp(secevent.KindAccessUnknownObject, q.r, hdr, secevent.Rejected,
			"request names an object this outstation does not support", nil)
		if len(q.objects) == 0 {
			s.sec.Unknown(fmt.Sprintf("o%d", fc))
		}
		for _, o := range q.objects {
			s.sec.Unknown(fmt.Sprintf("o%d/%d.%d", fc, o[0], o[1]))
		}
	}
	if errs&app.IINParameterError != 0 {
		s.emitApp(secevent.KindProtoParameterError, q.r, hdr, secevent.Rejected,
			"request answered PARAMETER_ERROR", nil)
	}
	if errs&app.IINAlreadyExecuting != 0 {
		s.emitApp(secevent.KindCtrlAlreadyExecuting, q.r, hdr, secevent.Rejected,
			"requested operation is already executing", nil)
	}
}

// malformedReason names the sentinel a parse error wraps.
func malformedReason(err error) string {
	for _, c := range []struct {
		err  error
		name string
	}{
		{app.ErrShortFragment, "short_fragment"},
		{app.ErrTruncated, "truncated"},
		{app.ErrBadQualifier, "bad_qualifier"},
		{app.ErrBadRange, "bad_range"},
		{app.ErrUnknownObject, "unknown_object"},
		{app.ErrFragmentTooLarge, "fragment_too_large"},
	} {
		if errors.Is(err, c.err) {
			return c.name
		}
	}
	return ""
}

// blockedByPolicy reports, and refuses, a request the function policy does
// not allow. It returns true when the request was refused.
func (s *Session) blockedByPolicy(r stack.Received, frag app.Fragment) bool {
	fp := s.cfg.Security.Functions
	if fp == nil {
		return false
	}
	fc := uint8(frag.Header.Func)
	blocked := fp.Blocks(fc)
	detail := fmt.Sprintf("function %s is refused by policy", frag.Header.Func)
	if !blocked && frag.Header.Func == app.FuncWrite {
		for _, h := range frag.Objects {
			if fp.BlocksWrite(h.Group) {
				blocked = true
				detail = fmt.Sprintf("WRITE of group %d is refused by policy", h.Group)
				break
			}
		}
	}
	if !blocked {
		return false
	}
	e := appEvent(secevent.KindAccessFunctionBlocked, r, &frag.Header)
	e.Action, e.Detail = secevent.Rejected, detail
	s.emit(e)
	return true
}

// broadcastMisuse reports a broadcast of a function the policy does not
// expect on one, and returns true when it is to be dropped.
func (s *Session) broadcastMisuse(r stack.Received, frag app.Fragment) bool {
	p := s.cfg.Security
	var groups []uint8
	if frag.Header.Func == app.FuncWrite {
		for _, h := range frag.Objects {
			groups = append(groups, h.Group)
		}
	}
	if p.BroadcastAllows(uint8(frag.Header.Func), groups) {
		return false
	}
	e := appEvent(secevent.KindAccessBroadcastMisuse, r, &frag.Header)
	e.Detail = fmt.Sprintf("%s broadcast to %#04x", frag.Header.Func, r.Dest)
	if p.RejectBroadcastMisuse {
		e.Action = secevent.Rejected
		e.Detail += "; dropped"
	}
	s.emit(e)
	return p.RejectBroadcastMisuse
}

// isCommandFunc reports whether f carries control objects.
func isCommandFunc(f app.FuncCode) bool {
	switch f {
	case app.FuncSelect, app.FuncOperate, app.FuncDirectOperate, app.FuncDirectOperateNR:
		return true
	}
	return false
}
