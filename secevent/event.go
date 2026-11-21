package secevent

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/internal/app"
)

// FieldMask says which of an Event's protocol context fields are meaningful,
// since zero is a valid value for most of them.
type FieldMask uint32

// Field bits.
const (
	HasLinkAddrs FieldMask = 1 << iota
	HasLinkFunc
	HasTransportSeq
	HasAppFunc
	HasAppSeq
	HasIIN
	HasObject
	HasIndex
	HasCommandStatus
	HasControlCode
	HasUser
	HasFileStatus
)

// Event is one security-relevant observation.
//
// Kind, Category, Severity and Technique default from the kind's catalogue
// entry; the emitter fills Time, Protocol, Role, Transport, ConnID and Peer.
// Call sites set only what varies.
type Event struct {
	Time      time.Time
	Kind      Kind
	Category  Category
	Severity  Severity
	Action    Action
	Protocol  string // always "dnp3"
	Role      string // "master", "outstation" or "bus"
	Transport string // "tcp", "tls", "udp", "serial", "pipe" or "multidrop"
	ConnID    uint64 // correlates the events of one connection
	Peer      PeerInfo
	Local     string // local address or serial port

	// Protocol context. Fields says which of these are set.
	Fields        FieldMask
	LinkSrc       uint16
	LinkDest      uint16
	LinkFunc      uint8
	TransportSeq  uint8
	AppFunc       uint8
	AppSeq        uint8
	ExpectedSeq   uint8
	IIN           uint16
	Group         uint8
	Variation     uint8
	Qualifier     uint8
	Index         uint32
	CommandStatus dnp3.CommandStatus
	ControlCode   dnp3.ControlCode
	User          uint16
	FileStatus    dnp3.FileStatus
	FileName      string
	Value         string

	// Count is how many occurrences this event stands for: 1 for a single
	// one, more when repeats were aggregated between FirstSeen and Time.
	Count     int
	FirstSeen time.Time
	// Detail is a one-line English explanation. It never carries key
	// material, passwords or full certificates.
	Detail string
	// Err is the underlying error, an errors.Is-able sentinel where one exists.
	Err error
	// Raw is a copy of the offending frame or fragment, truncated.
	Raw []byte
	// Technique is a MITRE ATT&CK for ICS hint for routing, not attribution.
	Technique string
}

// FuncName returns the name of a DNP3 application function code.
func FuncName(code uint8) string { return app.FuncCode(code).String() }

// String renders the event as one line.
func (e Event) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s %s", e.Time.Format(time.RFC3339Nano), e.Severity, e.Kind)
	if e.Action != Observed {
		fmt.Fprintf(&b, " action=%s", e.Action)
	}
	if e.Role != "" {
		fmt.Fprintf(&b, " role=%s", e.Role)
	}
	if e.ConnID != 0 {
		fmt.Fprintf(&b, " conn=%d", e.ConnID)
	}
	if p := e.Peer.String(); p != "" {
		fmt.Fprintf(&b, " peer=%s", p)
	}
	if e.Fields&HasLinkAddrs != 0 {
		fmt.Fprintf(&b, " src=%d dest=%d", e.LinkSrc, e.LinkDest)
	}
	if e.Fields&HasAppFunc != 0 {
		fmt.Fprintf(&b, " func=%s", FuncName(e.AppFunc))
	}
	if e.Fields&HasObject != 0 {
		fmt.Fprintf(&b, " object=g%dv%d", e.Group, e.Variation)
	}
	if e.Fields&HasIndex != 0 {
		fmt.Fprintf(&b, " index=%d", e.Index)
	}
	if e.Fields&HasCommandStatus != 0 {
		fmt.Fprintf(&b, " status=%s", e.CommandStatus)
	}
	if e.Count > 1 {
		fmt.Fprintf(&b, " count=%d", e.Count)
	}
	if e.Detail != "" {
		fmt.Fprintf(&b, " detail=%q", e.Detail)
	}
	return b.String()
}

// jsonEvent fixes the JSON field names. They are public API, like Kind.
type jsonEvent struct {
	Time          time.Time  `json:"time"`
	Kind          Kind       `json:"kind"`
	Category      Category   `json:"category"`
	Severity      Severity   `json:"severity"`
	Action        Action     `json:"action"`
	Protocol      string     `json:"protocol,omitempty"`
	Role          string     `json:"role,omitempty"`
	Transport     string     `json:"transport,omitempty"`
	ConnID        uint64     `json:"conn_id,omitempty"`
	Peer          *PeerInfo  `json:"peer,omitempty"`
	Local         string     `json:"local,omitempty"`
	LinkSrc       *uint16    `json:"link_src,omitempty"`
	LinkDest      *uint16    `json:"link_dest,omitempty"`
	LinkFunc      *uint8     `json:"link_func,omitempty"`
	TransportSeq  *uint8     `json:"transport_seq,omitempty"`
	AppFunc       string     `json:"app_func,omitempty"`
	AppSeq        *uint8     `json:"app_seq,omitempty"`
	ExpectedSeq   *uint8     `json:"expected_seq,omitempty"`
	IIN           *uint16    `json:"iin,omitempty"`
	Group         *uint8     `json:"group,omitempty"`
	Variation     *uint8     `json:"variation,omitempty"`
	Qualifier     *uint8     `json:"qualifier,omitempty"`
	Index         *uint32    `json:"index,omitempty"`
	CommandStatus string     `json:"command_status,omitempty"`
	ControlCode   *uint8     `json:"control_code,omitempty"`
	User          *uint16    `json:"user,omitempty"`
	FileStatus    string     `json:"file_status,omitempty"`
	FileName      string     `json:"file_name,omitempty"`
	Value         string     `json:"value,omitempty"`
	Count         int        `json:"count,omitempty"`
	FirstSeen     *time.Time `json:"first_seen,omitempty"`
	Detail        string     `json:"detail,omitempty"`
	Err           string     `json:"error,omitempty"`
	Raw           []byte     `json:"raw,omitempty"`
	Technique     string     `json:"technique,omitempty"`
}

func ptr[T any](v T) *T { return &v }

// MarshalJSON renders the event with stable field names, leaving out context
// fields that are not set.
func (e Event) MarshalJSON() ([]byte, error) {
	j := jsonEvent{
		Time: e.Time, Kind: e.Kind, Category: e.Category, Severity: e.Severity, Action: e.Action,
		Protocol: e.Protocol, Role: e.Role, Transport: e.Transport, ConnID: e.ConnID,
		Local: e.Local, FileName: e.FileName, Value: e.Value, Detail: e.Detail,
		Raw: e.Raw, Technique: e.Technique,
	}
	if !e.Peer.IsZero() {
		j.Peer = &e.Peer
	}
	if e.Fields&HasLinkAddrs != 0 {
		j.LinkSrc, j.LinkDest = ptr(e.LinkSrc), ptr(e.LinkDest)
	}
	if e.Fields&HasLinkFunc != 0 {
		j.LinkFunc = ptr(e.LinkFunc)
	}
	if e.Fields&HasTransportSeq != 0 {
		j.TransportSeq = ptr(e.TransportSeq)
	}
	if e.Fields&HasAppFunc != 0 {
		j.AppFunc = FuncName(e.AppFunc)
	}
	if e.Fields&HasAppSeq != 0 {
		j.AppSeq, j.ExpectedSeq = ptr(e.AppSeq), ptr(e.ExpectedSeq)
	}
	if e.Fields&HasIIN != 0 {
		j.IIN = ptr(e.IIN)
	}
	if e.Fields&HasObject != 0 {
		j.Group, j.Variation, j.Qualifier = ptr(e.Group), ptr(e.Variation), ptr(e.Qualifier)
	}
	if e.Fields&HasIndex != 0 {
		j.Index = ptr(e.Index)
	}
	if e.Fields&HasCommandStatus != 0 {
		j.CommandStatus = e.CommandStatus.String()
	}
	if e.Fields&HasControlCode != 0 {
		j.ControlCode = ptr(uint8(e.ControlCode))
	}
	if e.Fields&HasUser != 0 {
		j.User = ptr(e.User)
	}
	if e.Fields&HasFileStatus != 0 {
		j.FileStatus = e.FileStatus.String()
	}
	if e.Count > 1 {
		j.Count = e.Count
		if !e.FirstSeen.IsZero() {
			j.FirstSeen = ptr(e.FirstSeen)
		}
	}
	if e.Err != nil {
		j.Err = e.Err.Error()
	}
	return json.Marshal(j)
}
