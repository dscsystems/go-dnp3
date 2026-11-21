package multidrop

import (
	"fmt"
	"io"

	"github.com/dscsystems/go-dnp3/internal/link"
	"github.com/dscsystems/go-dnp3/secevent"
)

// openSecurity opens the bus's emitter and attaches it to the channel.
func (b *Bus) openSecurity() {
	if b.cfg.Security == nil {
		return
	}
	if err := b.cfg.Security.Valid(); err != nil {
		// New cannot fail, so a policy it cannot use is logged and ignored.
		b.log.Error("security policy ignored", "err", err)
		return
	}
	em, done := b.cfg.Security.Open("bus")
	b.em, b.secDone = em.WithTransport("multidrop"), done
	if a, ok := b.ch.(secevent.Attachable); ok && em != nil {
		a.AttachSecurity(em)
	}
}

// emit reports a bus event. It is safe from any goroutine.
func (b *Bus) emit(e secevent.Event) {
	b.mu.Lock()
	em := b.connEm
	b.mu.Unlock()
	if em == nil {
		em = b.em
	}
	em.Emit(e)
}

// secConnected binds the bus's events to the line just connected.
func (b *Bus) secConnected(conn io.ReadWriteCloser) {
	if b.em == nil {
		return
	}
	peer := secevent.PeerOf(conn)
	em := b.em.ForConn(secevent.NextConnID(), peer).WithTransport("multidrop")
	b.mu.Lock()
	b.connEm = em
	b.mu.Unlock()
	em.Emit(secevent.Event{Kind: secevent.KindConnConnected, Detail: "bus line connected"})
}

// secDisconnected reports the line going down.
func (b *Bus) secDisconnected() {
	b.mu.Lock()
	em := b.connEm
	b.connEm = nil
	b.mu.Unlock()
	em.Emit(secevent.Event{Kind: secevent.KindConnClosed, Detail: "bus line disconnected"})
}

// secParser reports what the parser threw away since the last read. A bus
// is a shared line, so line noise is Notice.
func (b *Bus) secParser(d link.Stats) {
	if b.em == nil || d.BytesDiscarded == 0 {
		return
	}
	if n := d.HeaderCRCErrors + d.BodyCRCErrors; n > 0 {
		b.emit(secevent.Event{Kind: secevent.KindLinkCRCError, Severity: secevent.Notice, Action: secevent.Ignored,
			Detail: fmt.Sprintf("%d frame(s) failed the CRC check", n)})
	}
	if d.BadLength > 0 {
		b.emit(secevent.Event{Kind: secevent.KindLinkBadLength, Severity: secevent.Notice, Action: secevent.Ignored,
			Detail: fmt.Sprintf("%d frame(s) with a length octet out of range", d.BadLength)})
	}
	b.emit(secevent.Event{Kind: secevent.KindLinkGarbage, Severity: secevent.Notice, Action: secevent.Ignored,
		Detail: fmt.Sprintf("%d octet(s) discarded looking for a frame start", d.BytesDiscarded)})
}

// secUnrouted reports a frame no station on the bus is addressed by. On a
// bus every station hears every frame, but the bus carries all of its own
// stations: a frame for none of them is for a device not configured here —
// or for one that does not exist.
func (b *Bus) secUnrouted(h link.Header) {
	if b.em == nil {
		return
	}
	b.emit(secevent.Event{Kind: secevent.KindLinkForeignDest, Action: secevent.Ignored,
		Fields: secevent.HasLinkAddrs | secevent.HasLinkFunc, LinkSrc: h.Src, LinkDest: h.Dest, LinkFunc: uint8(h.Control.Func),
		Detail: fmt.Sprintf("frame from %d to %d, which no station on this bus is", h.Src, h.Dest)})
}

// secQueueDrop reports frames the bus had to drop.
func (b *Bus) secQueueDrop(detail string) {
	if b.em == nil {
		return
	}
	b.emit(secevent.Event{Kind: secevent.KindDoSQueueDrop, Action: secevent.Ignored, Detail: detail})
}
