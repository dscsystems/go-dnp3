package master

import (
	"io"

	"github.com/dscsystems/go-dnp3/secevent"
)

// ending says why serve gave up on a connection.
type ending struct {
	reason string
	err    error
}

// emit reports a security event, if a policy is configured.
func (s *Session) emit(e secevent.Event) { s.sec.Emit(e) }

// ReportCyberEvent reports an event on behalf of code that holds the session.
// The session fills in its own and the connection's identity. It is safe from
// any goroutine and does nothing without [Config.Security].
func (s *Session) ReportCyberEvent(e secevent.Event) { s.sec.Emit(e) }

// PeerInfo describes the outstation's end of the current connection. It
// reports false when not connected or reporting is off.
func (s *Session) PeerInfo() (secevent.PeerInfo, bool) { return s.sec.Peer() }

// SecurityStats returns the counters of the session's security events.
func (s *Session) SecurityStats() secevent.Stats { return s.sec.Stats() }

func (s *Session) writeStalled() ending {
	s.log.Warn("write timed out; dropping the connection")
	s.emit(secevent.Event{Kind: secevent.KindDoSWriteStall, Action: secevent.Disconnected,
		Detail: "a write missed WriteTimeout: the outstation stopped reading"})
	return ending{reason: "write timed out"}
}

// connectionOpened reports a new connection, and returns false when the
// policy refuses it.
func (s *Session) connectionOpened(conn io.ReadWriteCloser) bool {
	return s.sec.Connected(conn, secevent.KindConnConnected)
}

func (s *Session) connectionClosed(end ending) {
	if end.err != nil {
		// TLS 1.3 completes the handshake before the server has checked the
		// client's certificate, so a refusal arrives as the first read's
		// error rather than at connect.
		if ev, ok := secevent.ClassifyTLS(end.err); ok {
			ev.Action = secevent.Disconnected
			s.emit(ev)
		}
	}
	em := s.sec.Emitter()
	took := s.sec.Disconnected()
	em.Emit(secevent.Event{
		Kind:   secevent.KindConnClosed,
		Detail: end.reason + "; lasted " + took.Round(1e6).String(),
		Err:    end.err,
	})
}
