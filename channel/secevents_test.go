package channel

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dscsystems/go-dnp3/secevent"
)

type sinkLog struct {
	mu sync.Mutex
	ev []secevent.Event
}

func (s *sinkLog) HandleEvent(e secevent.Event) { s.mu.Lock(); s.ev = append(s.ev, e); s.mu.Unlock() }

func (s *sinkLog) wait(t *testing.T, k secevent.Kind) secevent.Event {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		for _, e := range s.ev {
			if e.Kind == k {
				s.mu.Unlock()
				return e
			}
		}
		s.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var got []secevent.Kind
	for _, e := range s.ev {
		got = append(got, e.Kind)
	}
	t.Fatalf("no %s; got %v", k, got)
	return secevent.Event{}
}

// attach wires a policy's emitter to ch the way a session would.
func attach(t *testing.T, ch Channel, p *secevent.Policy) *sinkLog {
	t.Helper()
	log := &sinkLog{}
	p.Sink = secevent.SyncSink(log)
	em, done := p.Open("outstation")
	t.Cleanup(done)
	ch.(secevent.Attachable).AttachSecurity(em)
	return log
}

// serveLoop accepts and closes connections until the test ends.
func serveLoop(t *testing.T, ch Channel) string {
	t.Helper()
	go func() {
		for {
			c, err := ch.Connect(t.Context())
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	return waitAddr(t, ch)
}

func TestAllowlistRejectsBeforeServing(t *testing.T) {
	server := TCPServer("127.0.0.1:0")
	t.Cleanup(func() { _ = server.Close() })
	log := attach(t, server, &secevent.Policy{
		AllowedPeers:   []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")},
		RejectUnlisted: true,
	})
	addr := serveLoop(t, server)
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	e := log.wait(t, secevent.KindConnUnexpected)
	if e.Action != secevent.Rejected || e.Peer.RemoteAddr.String() != c.LocalAddr().String() {
		t.Fatalf("event = %+v", e)
	}
}

func TestAcceptRateIsReported(t *testing.T) {
	server := TCPServer("127.0.0.1:0")
	t.Cleanup(func() { _ = server.Close() })
	log := attach(t, server, &secevent.Policy{AcceptRate: secevent.Rate{N: 2, Per: time.Minute}})
	addr := serveLoop(t, server)
	for range 3 {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		_ = c.SetReadDeadline(time.Now().Add(time.Second))
		_, _ = c.Read(make([]byte, 1)) // wait for the server to accept and close it
		_ = c.Close()
	}
	if e := log.wait(t, secevent.KindConnRate); e.Action != secevent.Observed {
		t.Fatalf("event = %+v", e)
	}
}

func tlsServerWithLog(t *testing.T) (addr string, log *sinkLog, ca, cCert, cKey string) {
	t.Helper()
	_, ca, sCert, sKey, cCert, cKey := certs(t)
	server, err := TLSServer("127.0.0.1:0", TLSConfig{
		CertFile: sCert, KeyFile: sKey, CAFile: ca, HandshakeTimeout: 300 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	log = attach(t, server, &secevent.Policy{})
	return serveLoop(t, server), log, ca, cCert, cKey
}

func TestTLSFailuresAreClassified(t *testing.T) {
	t.Run("own certificate expiring", func(t *testing.T) {
		_, log, _, _, _ := tlsServerWithLog(t)
		log.wait(t, secevent.KindTLSCertExpiring) // the test certificates last a day
	})
	t.Run("plaintext DNP3", func(t *testing.T) {
		addr, log, _, _, _ := tlsServerWithLog(t)
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		_, _ = c.Write([]byte{0x05, 0x64, 0x05, 0xC0, 0x01, 0x00, 0x0A, 0x00, 0x00, 0x00})
		if e := log.wait(t, secevent.KindTLSPlaintextOnTLSPort); e.Peer.RemoteAddr.String() != c.LocalAddr().String() {
			t.Fatalf("event = %+v", e)
		}
	})
	t.Run("http probe", func(t *testing.T) {
		addr, log, _, _, _ := tlsServerWithLog(t)
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		_, _ = c.Write([]byte("GET / HTTP/1.0\r\n\r\n"))
		log.wait(t, secevent.KindTLSNonTLSProbe)
	})
	t.Run("silent peer", func(t *testing.T) {
		addr, log, _, _, _ := tlsServerWithLog(t)
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		log.wait(t, secevent.KindTLSHandshakeTimeout)
	})
	t.Run("no client certificate", func(t *testing.T) {
		addr, log, ca, _, _ := tlsServerWithLog(t)
		pool := x509.NewCertPool()
		pem, err := os.ReadFile(ca)
		if err != nil {
			t.Fatal(err)
		}
		pool.AppendCertsFromPEM(pem)
		c, err := tls.Dial("tcp", addr, &tls.Config{RootCAs: pool, ServerName: "localhost"})
		if err == nil {
			_, _ = c.Read(make([]byte, 1)) // TLS 1.3 reports the refusal on first read
			_ = c.Close()
		}
		log.wait(t, secevent.KindTLSNoClientCert)
	})
	t.Run("client certificate from another authority", func(t *testing.T) {
		addr, log, ca, _, _ := tlsServerWithLog(t)
		_, _, _, _, otherCert, otherKey := certs(t)
		client, err := TLSClient(addr, TLSConfig{CertFile: otherCert, KeyFile: otherKey, CAFile: ca, ServerName: "localhost"}, NoRetry)
		if err != nil {
			t.Fatal(err)
		}
		clientLog := attach(t, client, &secevent.Policy{CertExpiryWarning: -1})
		if c, err := client.Connect(t.Context()); err == nil {
			_, _ = c.Read(make([]byte, 1))
			_ = c.Close()
		}
		e := log.wait(t, secevent.KindTLSCertRejected)
		if e.Action != secevent.Rejected || e.Peer.TLS == nil || e.Peer.TLS.FingerprintSHA256 == "" {
			t.Fatalf("server event = %+v", e)
		}
		_ = clientLog
	})
}

// Behind a session, a client whose certificate the server refuses learns it
// on the first read, and the session reports what the peer said.
func TestTLSRefusalSeenByTheClientSession(t *testing.T) {
	addr, _, ca, _, _ := tlsServerWithLog(t)
	_, _, _, _, otherCert, otherKey := certs(t)
	client, err := TLSClient(addr, TLSConfig{CertFile: otherCert, KeyFile: otherKey, CAFile: ca, ServerName: "localhost"}, NoRetry)
	if err != nil {
		t.Fatal(err)
	}
	c, err := client.Connect(t.Context())
	if err != nil {
		if ev, ok := secevent.ClassifyTLS(err); !ok || ev.Kind != secevent.KindTLSLocalCertRejected {
			t.Fatalf("dial error %v classified as %+v", err, ev)
		}
		return
	}
	defer c.Close()
	_, err = c.Read(make([]byte, 1))
	ev, ok := secevent.ClassifyTLS(err)
	if !ok || ev.Kind != secevent.KindTLSLocalCertRejected {
		t.Fatalf("read error %v classified as %+v ok=%v", err, ev, ok)
	}
}

// An application check that refuses a peer whose certificate is valid is
// reported as a certificate rejection, with the check's reason.
func TestTLSApplicationCheckRefuses(t *testing.T) {
	_, ca, sCert, sKey, cCert, cKey := certs(t)
	checked := make(chan string, 1)
	server, err := TLSServer("127.0.0.1:0", TLSConfig{
		CertFile: sCert, KeyFile: sKey, CAFile: ca,
		VerifyConnection: func(st tls.ConnectionState) error {
			select {
			case checked <- st.PeerCertificates[0].Subject.CommonName:
			default:
			}
			return errors.New("not on the pinned list")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	log := attach(t, server, &secevent.Policy{CertExpiryWarning: -1})
	addr := serveLoop(t, server)

	client, err := TLSClient(addr, TLSConfig{CertFile: cCert, KeyFile: cKey, CAFile: ca, ServerName: "localhost"}, NoRetry)
	if err != nil {
		t.Fatal(err)
	}
	if c, err := client.Connect(t.Context()); err == nil {
		_, _ = c.Read(make([]byte, 1))
		_ = c.Close()
	}
	e := log.wait(t, secevent.KindTLSCertRejected)
	if e.Value != "application" || !strings.Contains(e.Detail, "not on the pinned list") || e.Peer.TLS == nil {
		t.Fatalf("event = %+v", e)
	}
	if cn := <-checked; cn == "" {
		t.Error("the check saw no certificate")
	}
}
