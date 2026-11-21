package sechub

import (
	"fmt"

	"github.com/dscsystems/go-dnp3/internal/link"
	"github.com/dscsystems/go-dnp3/internal/stack"
	"github.com/dscsystems/go-dnp3/internal/transport"
	"github.com/dscsystems/go-dnp3/secevent"
)

// Observe installs the hub on a stack: link-layer and transport anomalies
// become events, and, for an outstation whose policy lists AllowedMasters,
// primary frames from other sources are reported or refused. It installs
// nothing when reporting is off, so the stack pays only nil checks.
func (h *Hub) Observe(s *stack.Stack, outstation bool) {
	if h.base == nil {
		s.SetObserver(nil, nil)
		return
	}
	var admit func(link.Header) bool
	if outstation && h.policy.AllowedMasters != nil {
		admit = h.admitMaster
	}
	s.SetObserver(h.linkObserved, admit)
}

func (h *Hub) admitMaster(hd link.Header) bool {
	if h.policy.MasterAllowed(hd.Src) {
		return true
	}
	e := linkEvent(secevent.KindLinkUnexpectedMaster, hd)
	e.Detail = fmt.Sprintf("primary frame from link address %d, which is not an allowed master", hd.Src)
	if h.policy.RejectUnexpectedMaster {
		e.Action = secevent.Rejected
		e.Detail += "; dropped"
	}
	h.Emit(e)
	return !h.policy.RejectUnexpectedMaster
}

func linkEvent(k secevent.Kind, hd link.Header) secevent.Event {
	return secevent.Event{
		Kind:     k,
		Fields:   secevent.HasLinkAddrs | secevent.HasLinkFunc,
		LinkSrc:  hd.Src,
		LinkDest: hd.Dest,
		LinkFunc: uint8(hd.Control.Func),
	}
}

// linkObserved turns a stack observation into an event.
func (h *Hub) linkObserved(o stack.Observation) {
	em := h.Emitter()
	shared := h.policy.SharedLink(em.Transport())
	// Noise on a serial line is the line; on a network transport, which
	// checksums its own segments, it is a broken or hostile sender.
	noisy := secevent.Warning
	if shared {
		noisy = secevent.Notice
	}
	hd := o.Header
	switch o.Kind {
	case stack.ObsParser:
		p := o.Parser
		if n := p.HeaderCRCErrors + p.BodyCRCErrors; n > 0 {
			em.Emit(secevent.Event{Kind: secevent.KindLinkCRCError, Severity: noisy, Action: secevent.Ignored,
				Detail: fmt.Sprintf("%d frame(s) failed the CRC check", n)})
		}
		if p.BadLength > 0 {
			em.Emit(secevent.Event{Kind: secevent.KindLinkBadLength, Severity: noisy, Action: secevent.Ignored,
				Detail: fmt.Sprintf("%d frame(s) with a length octet out of range", p.BadLength)})
		}
		// The count includes the octets of the frames reported above, which
		// the parser skips over the same way.
		em.Emit(secevent.Event{Kind: secevent.KindLinkGarbage, Severity: noisy, Action: secevent.Ignored,
			Detail: fmt.Sprintf("%d octet(s) discarded looking for a frame start", p.BytesDiscarded)})

	case stack.ObsForeignDest:
		if shared {
			return
		}
		e := linkEvent(secevent.KindLinkForeignDest, hd)
		e.Action = secevent.Ignored
		e.Detail = fmt.Sprintf("frame for link address %d on a point-to-point link", hd.Dest)
		em.Emit(e)

	case stack.ObsWrongDir:
		e := linkEvent(secevent.KindLinkWrongDir, hd)
		e.Action = secevent.Ignored
		e.Detail = "frame whose DIR bit claims our own role"
		em.Emit(e)

	case stack.ObsInvalidSource:
		e := linkEvent(secevent.KindLinkInvalidSource, hd)
		e.Action = secevent.Ignored
		e.Detail = fmt.Sprintf("frame from link address %#04x, which no station may hold", hd.Src)
		em.Emit(e)

	case stack.ObsUnexpectedSource:
		e := linkEvent(secevent.KindLinkUnexpectedSource, hd)
		e.Action = secevent.Ignored
		e.Detail = fmt.Sprintf("reply from link address %d, not the station we are talking to", hd.Src)
		em.Emit(e)

	case stack.ObsSecondary:
		h.secondaryNote(em, o)

	case stack.ObsTransport:
		h.transportDiscard(em, o, shared)

	case stack.ObsBroadcast:
		e := linkEvent(secevent.KindLinkBroadcast, hd)
		if shared {
			e.Severity = secevent.Info
		}
		e.Detail = fmt.Sprintf("broadcast to %#04x", hd.Dest)
		em.Emit(e)

	case stack.ObsSelfAddress:
		e := linkEvent(secevent.KindLinkSelfAddress, hd)
		e.Detail = "request to the self address 0xFFFC"
		em.Emit(e)
	}
}

func (h *Hub) secondaryNote(em *secevent.Emitter, o stack.Observation) {
	var e secevent.Event
	switch o.Note {
	case link.NoteInvalidControl:
		e = linkEvent(secevent.KindLinkInvalidControl, o.Header)
		e.Action = secevent.Ignored
		e.Detail = "control field contradicts the function or the length"
	case link.NoteNotReset:
		e = linkEvent(secevent.KindLinkNotReset, o.Header)
		e.Action = secevent.Rejected
		e.Detail = "confirmed user data before the link was reset"
	case link.NoteFCBRepeat:
		e = linkEvent(secevent.KindSeqFCBRepeat, o.Header)
		e.Action = secevent.Ignored
		e.Detail = "confirmed user data repeating the frame count bit; acknowledged, not delivered"
	case link.NoteTestNack:
		e = linkEvent(secevent.KindSeqTestLinkNack, o.Header)
		e.Action = secevent.Rejected
		e.Detail = "TEST_LINK_STATES before a reset or with the wrong frame count bit"
	case link.NoteUnsupported:
		e = linkEvent(secevent.KindLinkUnsupportedFunction, o.Header)
		e.Action = secevent.Rejected
		e.Detail = fmt.Sprintf("primary link function %d is not supported", o.Header.Control.Func)
	case link.NoteBroadcastFunction:
		e = linkEvent(secevent.KindLinkInvalidControl, o.Header)
		e.Action = secevent.Ignored
		e.Detail = "broadcast of a link function other than user data"
	default:
		// Resets and status requests are ordinary; flood detection (P8)
		// counts them.
		return
	}
	em.Emit(e)
}

func (h *Hub) transportDiscard(em *secevent.Emitter, o stack.Observation, shared bool) {
	e := linkEvent(secevent.KindTransportDiscard, o.Header)
	e.Action = secevent.Ignored
	switch o.Discard {
	case transport.DiscardOverflow:
		e.Kind = secevent.KindAccessOversizedRequest
		e.Detail = "fragment larger than the receive limit; discarded"
	case transport.DiscardBadSequence:
		e.Value = "bad_sequence"
		e.Detail = "transport segment out of sequence"
		if !shared {
			// TCP does not reorder: a gap is injection or a broken peer.
			e.Severity = secevent.Warning
		}
	case transport.DiscardEmptySegment:
		e.Value = "empty"
		e.Detail = "transport segment with no header"
	case transport.DiscardNoFIR:
		e.Value = "no_fir"
		e.Detail = "transport continuation without a first segment"
	case transport.DiscardUnexpectedFIR:
		e.Value = "unexpected_fir"
		e.Detail = "new fragment started before the last one finished"
	default:
		return
	}
	em.Emit(e)
}
