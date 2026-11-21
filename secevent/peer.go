package secevent

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/netip"
	"time"
)

// MACSource says where a peer's MAC address came from.
type MACSource uint8

// MAC sources.
const (
	// MACNone means no MAC address is known.
	MACNone MACSource = iota
	// MACNeighbor is the peer's own address from the neighbour table.
	MACNeighbor
	// MACGateway is the next hop's address: the peer is not on-link.
	MACGateway
	// MACStatic came from configuration.
	MACStatic
	// MACCustom came from an application-supplied resolver.
	MACCustom
)

var macSourceNames = [...]string{"none", "neighbor", "gateway", "static", "custom"}

func (m MACSource) String() string {
	if int(m) < len(macSourceNames) {
		return macSourceNames[m]
	}
	return "unknown"
}

// MarshalText implements encoding.TextMarshaler.
func (m MACSource) MarshalText() ([]byte, error) { return []byte(m.String()), nil }

// PeerInfo identifies the other end of a connection.
type PeerInfo struct {
	Network     string           `json:"network,omitempty"` // "tcp", "udp", "serial", "pipe"
	RemoteAddr  netip.AddrPort   `json:"remote_addr,omitzero"`
	LocalAddr   netip.AddrPort   `json:"local_addr,omitzero"`
	MAC         net.HardwareAddr `json:"-"`
	MACSource   MACSource        `json:"mac_source,omitzero"`
	OnLink      bool             `json:"on_link,omitempty"`
	Interface   string           `json:"interface,omitempty"`
	TLS         *TLSPeer         `json:"tls,omitempty"`
	ConnectedAt time.Time        `json:"connected_at,omitzero"`
	SerialPort  string           `json:"serial_port,omitempty"`
	// LinkAddr is the DNP3 source address last seen from this peer, zero
	// until one is known.
	LinkAddr uint16 `json:"link_addr,omitempty"`
}

// MarshalJSON renders the MAC address in its usual text form.
func (p PeerInfo) MarshalJSON() ([]byte, error) {
	type plain PeerInfo
	var mac string
	if p.MAC != nil {
		mac = p.MAC.String()
	}
	return json.Marshal(struct {
		plain
		MAC string `json:"mac,omitempty"`
	}{plain(p), mac})
}

// TLSPeer describes the TLS session and the peer's certificate.
type TLSPeer struct {
	Version           uint16    `json:"version"`
	CipherSuite       uint16    `json:"cipher_suite"`
	ServerName        string    `json:"server_name,omitempty"`
	Subject           string    `json:"subject,omitempty"`
	Issuer            string    `json:"issuer,omitempty"`
	SerialHex         string    `json:"serial,omitempty"`
	NotAfter          time.Time `json:"not_after,omitzero"`
	FingerprintSHA256 string    `json:"fingerprint_sha256,omitempty"`
	Verified          bool      `json:"verified"`
}

// IsZero reports whether nothing is known about the peer.
func (p PeerInfo) IsZero() bool { return p.addressless() && p.TLS == nil }

// addressless reports whether the peer has no address, port or MAC, though
// it may have a certificate: what a failed TLS dial knows.
func (p PeerInfo) addressless() bool {
	return p.Network == "" && !p.RemoteAddr.IsValid() && p.SerialPort == "" && p.MAC == nil
}

// String renders the peer compactly: its address, port or pipe.
func (p PeerInfo) String() string {
	switch {
	case p.RemoteAddr.IsValid():
		return p.RemoteAddr.String()
	case p.SerialPort != "":
		return p.SerialPort
	default:
		return p.Network
	}
}

// PeerInfoer is implemented by connections that know their peer.
type PeerInfoer interface {
	PeerInfo() PeerInfo
}

// PeerOf describes the peer of a connection a channel produced.
//
// A connection that implements [PeerInfoer] answers for itself. Otherwise the
// connection is unwrapped (through Unwrap() net.Conn) looking for a
// *tls.Conn and a net.Conn, whose addresses and TLS state are used. Anything
// else yields only the connection time.
func PeerOf(rw io.ReadWriteCloser) PeerInfo {
	if p, ok := rw.(PeerInfoer); ok {
		return p.PeerInfo()
	}
	info := PeerInfo{ConnectedAt: time.Now()}
	var cur any = rw
	for range 4 {
		if tc, ok := cur.(*tls.Conn); ok {
			info.TLS = TLSPeerOf(tc.ConnectionState())
		}
		if nc, ok := cur.(net.Conn); ok {
			fillAddrs(&info, nc)
		}
		u, ok := cur.(interface{ Unwrap() net.Conn })
		if !ok {
			break
		}
		cur = u.Unwrap()
	}
	return info
}

func fillAddrs(info *PeerInfo, nc net.Conn) {
	if info.Network != "" && info.Network != "pipe" {
		return
	}
	info.RemoteAddr = addrPort(nc.RemoteAddr())
	info.LocalAddr = addrPort(nc.LocalAddr())
	if a := nc.RemoteAddr(); a != nil {
		info.Network = a.Network()
	}
}

func addrPort(a net.Addr) netip.AddrPort {
	switch a := a.(type) {
	case *net.TCPAddr:
		return unmapped(a.AddrPort())
	case *net.UDPAddr:
		return unmapped(a.AddrPort())
	}
	return netip.AddrPort{}
}

func unmapped(ap netip.AddrPort) netip.AddrPort {
	return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
}

// TLSPeerOf summarises a TLS connection state. It records the leaf
// certificate's identity and fingerprint, never the certificate itself.
func TLSPeerOf(st tls.ConnectionState) *TLSPeer {
	p := &TLSPeer{
		Version:     st.Version,
		CipherSuite: st.CipherSuite,
		ServerName:  st.ServerName,
		Verified:    len(st.VerifiedChains) > 0,
	}
	if len(st.PeerCertificates) > 0 {
		leaf := st.PeerCertificates[0]
		sum := sha256.Sum256(leaf.Raw)
		p.Subject = leaf.Subject.String()
		p.Issuer = leaf.Issuer.String()
		p.SerialHex = leaf.SerialNumber.Text(16)
		p.NotAfter = leaf.NotAfter
		p.FingerprintSHA256 = hex.EncodeToString(sum[:])
	}
	return p
}
