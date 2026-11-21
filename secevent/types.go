package secevent

import (
	"fmt"
	"strings"
)

// Severity orders events by urgency. The levels follow RFC 5424.
type Severity uint8

// Severities, least to most urgent.
const (
	Info Severity = iota + 1
	Notice
	Warning
	Alert
	Critical
)

var severityNames = [...]string{"", "info", "notice", "warning", "alert", "critical"}

func (s Severity) String() string {
	if int(s) < len(severityNames) && s != 0 {
		return severityNames[s]
	}
	return fmt.Sprintf("Severity(%d)", uint8(s))
}

// MarshalText implements encoding.TextMarshaler.
func (s Severity) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// UnmarshalText implements encoding.TextUnmarshaler.
func (s *Severity) UnmarshalText(b []byte) error {
	for i, n := range severityNames {
		if i > 0 && strings.EqualFold(n, string(b)) {
			*s = Severity(i)
			return nil
		}
	}
	return fmt.Errorf("secevent: unknown severity %q", b)
}

// Category groups kinds by the layer or concern they belong to.
type Category uint8

// Categories.
const (
	CatConnection Category = iota + 1
	CatPeer
	CatTLS
	CatLink
	CatTransport
	CatProtocol
	CatSequence
	CatAccess
	CatAuth
	CatControl
	CatCommand
	CatUnsolicited
	CatRemote
	CatFlood
	CatResource
	CatFileTransfer
	CatMeta
)

var categoryNames = [...]string{"", "connection", "peer", "tls", "link", "transport", "protocol",
	"sequence", "access", "auth", "control", "command", "unsolicited", "remote", "flood",
	"resource", "file", "meta"}

func (c Category) String() string {
	if int(c) < len(categoryNames) && c != 0 {
		return categoryNames[c]
	}
	return fmt.Sprintf("Category(%d)", uint8(c))
}

// MarshalText implements encoding.TextMarshaler.
func (c Category) MarshalText() ([]byte, error) { return []byte(c.String()), nil }

// Action says what the library did about what it observed.
type Action uint8

// Actions.
const (
	// Observed means the library took no action beyond reporting.
	Observed Action = iota
	// Ignored means the offending input was discarded and processing went on.
	Ignored
	// Rejected means a request or connection was refused.
	Rejected
	// Throttled means a request was refused because a rate limit was exceeded.
	Throttled
	// Disconnected means the connection was closed.
	Disconnected
	// Blocked means a policy refused the input before it was processed.
	Blocked
)

var actionNames = [...]string{"observed", "ignored", "rejected", "throttled", "disconnected", "blocked"}

func (a Action) String() string {
	if int(a) < len(actionNames) {
		return actionNames[a]
	}
	return fmt.Sprintf("Action(%d)", uint8(a))
}

// MarshalText implements encoding.TextMarshaler.
func (a Action) MarshalText() ([]byte, error) { return []byte(a.String()), nil }
