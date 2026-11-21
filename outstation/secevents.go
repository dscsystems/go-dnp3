package outstation

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

// ReportCyberEvent reports an event on behalf of code that holds the session,
// such as a command handler refusing a control for a reason only it knows.
// The session fills in its own and the connection's identity. It is safe from
// any goroutine and does nothing without [Config.Security].
func (s *Session) ReportCyberEvent(e secevent.Event) { s.sec.Emit(e) }

// PeerInfo describes the master's end of the current connection. It reports
// false when no connection is being served or reporting is off.
func (s *Session) PeerInfo() (secevent.PeerInfo, bool) { return s.sec.Peer() }

// SecurityStats returns the counters of the session's security events.
func (s *Session) SecurityStats() secevent.Stats { return s.sec.Stats() }

// writeStalled ends a connection whose peer stopped reading.
func (s *Session) writeStalled() ending {
	s.log.Warn("write timed out; dropping the connection")
	s.emit(secevent.Event{Kind: secevent.KindDoSWriteStall, Action: secevent.Disconnected,
		Detail: "a write missed WriteTimeout: the peer stopped reading"})
	return ending{reason: "write timed out"}
}

// connectionOpened reports a new connection, and returns false when the
// policy refuses it.
func (s *Session) connectionOpened(conn io.ReadWriteCloser) bool {
	return s.sec.Connected(conn, secevent.KindConnAccepted)
}

func (s *Session) connectionClosed(end ending) {
	if !s.sec.Enabled() {
		s.sec.Disconnected()
		return
	}
	link, _ := s.stack.Stats()
	if link.FramesDecoded == 0 && end.reason != "shutdown" {
		s.emit(secevent.Event{Kind: secevent.KindConnNoData,
			Detail: "the connection ended without one valid link frame"})
	}
	d := s.sec.Emitter()
	took := s.sec.Disconnected()
	d.Emit(secevent.Event{
		Kind:   secevent.KindConnClosed,
		Detail: end.reason + "; lasted " + took.Round(1e6).String(),
		Err:    end.err,
	})
}
