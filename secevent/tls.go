package secevent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
)

// ClassifyTLS turns a TLS handshake error into the event that says what went
// wrong: a certificate refused, none presented, ours refused by the peer,
// plaintext DNP3 or something else entirely on the TLS port, a timeout. ok
// is false for an error that is not about TLS at all.
func ClassifyTLS(err error) (Event, bool) {
	var (
		aerr  *VerifyError
		verr  *tls.CertificateVerificationError
		rerr  tls.RecordHeaderError
		uaerr x509.UnknownAuthorityError
		cierr x509.CertificateInvalidError
		herr  x509.HostnameError
		operr *net.OpError
	)
	switch {
	case errors.As(err, &aerr):
		ev := Event{Kind: KindTLSCertRejected, Err: err, Value: "application",
			Detail: "refused by the application's check: " + aerr.Err.Error()}
		if aerr.Leaf != nil {
			ev.Peer.TLS = leafInfo(aerr.Leaf)
			ev.Detail += " (subject " + ev.Peer.TLS.Subject + ")"
		}
		return ev, true

	case errors.As(err, &rerr):
		if rerr.RecordHeader[0] == 0x05 && rerr.RecordHeader[1] == 0x64 {
			return Event{Kind: KindTLSPlaintextOnTLSPort, Err: err,
				Detail: "the peer spoke plaintext DNP3 to the TLS port"}, true
		}
		return Event{Kind: KindTLSNonTLSProbe, Err: err,
			Detail: fmt.Sprintf("the peer sent %q, which is not TLS", printable(rerr.RecordHeader[:]))}, true

	case errors.As(err, &verr):
		ev := Event{Kind: KindTLSCertRejected, Err: err}
		if len(verr.UnverifiedCertificates) > 0 {
			ev.Peer.TLS = leafInfo(verr.UnverifiedCertificates[0])
		}
		switch {
		case errors.As(err, &uaerr):
			ev.Detail = "certificate signed by an unknown authority"
		case errors.As(err, &cierr):
			ev.Detail = "certificate invalid: " + certInvalidReason(cierr.Reason)
		case errors.As(err, &herr):
			ev.Detail = "certificate does not match the expected name"
		default:
			ev.Detail = "certificate failed verification"
		}
		if ev.Peer.TLS != nil {
			ev.Detail += " (subject " + ev.Peer.TLS.Subject + ")"
		}
		return ev, true

	case strings.Contains(err.Error(), "didn't provide a certificate"):
		return Event{Kind: KindTLSNoClientCert, Err: err,
			Detail: "the client presented no certificate"}, true

	case errors.As(err, &operr) && operr.Op == "remote error":
		msg := operr.Err.Error()
		for _, a := range []string{"bad certificate", "unknown certificate authority", "expired certificate",
			"certificate required", "unsupported certificate", "certificate revoked", "unknown certificate"} {
			if strings.Contains(msg, a) {
				return Event{Kind: KindTLSLocalCertRejected, Err: err,
					Detail: "the peer refused our certificate: " + a}, true
			}
		}
		return Event{Kind: KindTLSHandshakeFailed, Err: err,
			Detail: "the peer aborted the handshake: " + msg}, true

	case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded):
		return Event{Kind: KindTLSHandshakeTimeout, Err: err,
			Detail: "the handshake did not complete in time"}, true

	case strings.HasPrefix(err.Error(), "tls:"):
		return Event{Kind: KindTLSHandshakeFailed, Err: err, Detail: err.Error()}, true
	}
	return Event{}, false
}

func certInvalidReason(r x509.InvalidReason) string {
	switch r {
	case x509.Expired:
		return "expired or not yet valid"
	case x509.NotAuthorizedToSign:
		return "issuer not authorised to sign"
	case x509.CANotAuthorizedForThisName:
		return "issuer not authorised for this name"
	case x509.TooManyIntermediates:
		return "too many intermediates"
	case x509.IncompatibleUsage:
		return "key usage does not permit this use"
	case x509.NameMismatch:
		return "issuer name mismatch"
	}
	return fmt.Sprintf("reason %d", int(r))
}

func leafInfo(c *x509.Certificate) *TLSPeer {
	return TLSPeerOf(tls.ConnectionState{PeerCertificates: []*x509.Certificate{c}})
}

func printable(b []byte) string {
	out := make([]byte, len(b))
	for i, c := range b {
		if c < 0x20 || c > 0x7e {
			c = '.'
		}
		out[i] = c
	}
	return string(out)
}

// VerifyError is the handshake error when an application's own check (see
// channel.TLSConfig.VerifyConnection) refused a peer whose certificate was
// otherwise valid. Leaf is the peer's certificate, when it sent one.
type VerifyError struct {
	Leaf *x509.Certificate
	Err  error
}

func (e *VerifyError) Error() string {
	return "tls: peer refused by the application's check: " + e.Err.Error()
}

func (e *VerifyError) Unwrap() error { return e.Err }
