package channel

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// certs generates a throwaway CA and two leaf certificates signed by it,
// writing them under a temporary directory.
//
// Real certificates rather than a stubbed handshake: the point of these tests
// is that the channel actually negotiates mutual TLS, and a fake would only
// prove the plumbing compiles.
func certs(t *testing.T) (dir, caFile, serverCert, serverKey, clientCert, clientKey string) {
	t.Helper()
	dir = t.TempDir()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "go-dnp3 test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}

	caFile = filepath.Join(dir, "ca.pem")
	writePEM(t, caFile, "CERTIFICATE", caDER)

	leaf := func(name string, serial int64, server bool) (certPath, keyPath string) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(serial),
			Subject:      pkix.Name{CommonName: name},
			NotBefore:    time.Now().Add(-time.Hour),
			NotAfter:     time.Now().Add(24 * time.Hour),
			KeyUsage:     x509.KeyUsageDigitalSignature,
		}
		if server {
			tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
			tmpl.DNSNames = []string{"localhost"}
			tmpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
		} else {
			tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
		}

		der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		certPath = filepath.Join(dir, name+".pem")
		keyPath = filepath.Join(dir, name+".key")
		writePEM(t, certPath, "CERTIFICATE", der)

		keyDER, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		writePEM(t, keyPath, "EC PRIVATE KEY", keyDER)
		return certPath, keyPath
	}

	serverCert, serverKey = leaf("server", 2, true)
	clientCert, clientKey = leaf("client", 3, false)
	return dir, caFile, serverCert, serverKey, clientCert, clientKey
}

func writePEM(t *testing.T, path, blockType string, der []byte) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := pem.Encode(f, &pem.Block{Type: blockType, Bytes: der}); err != nil {
		t.Fatal(err)
	}
}

func TestTLSMutualAuthentication(t *testing.T) {
	_, ca, sCert, sKey, cCert, cKey := certs(t)

	server, err := TLSServer("127.0.0.1:0", TLSConfig{
		CertFile: sCert, KeyFile: sKey, CAFile: ca,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })

	// Go performs the TLS handshake lazily, on the first read or write. The
	// server side has to drive it concurrently with the client's dial, or the
	// two sit waiting for each other's handshake records.
	accepted := make(chan io.ReadWriteCloser, 1)
	go func() {
		c, err := server.Connect(t.Context())
		if err != nil {
			return
		}
		if err := handshake(t.Context(), c); err != nil {
			return
		}
		accepted <- c
	}()

	var addr string
	for range 200 {
		if a := ServerAddr(server); a != nil {
			addr = a.String()
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if addr == "" {
		t.Fatal("the TLS server never bound")
	}

	client, err := TLSClient(addr, TLSConfig{
		CertFile: cCert, KeyFile: cKey, CAFile: ca, ServerName: "localhost",
	}, NoRetry)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })

	cc, err := client.Connect(t.Context())
	if err != nil {
		t.Fatalf("TLS dial: %v", err)
	}
	defer cc.Close()

	sc := <-accepted
	defer sc.Close()

	// A DNP3 link reset frame, to prove real octets cross the tunnel.
	frame := []byte{0x05, 0x64, 0x05, 0xC0, 0x0A, 0x00, 0x01, 0x00, 0xB1, 0xAC}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := cc.Write(frame); err != nil {
			t.Errorf("write: %v", err)
		}
	}()

	buf := make([]byte, len(frame))
	if _, err := io.ReadFull(sc, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	wg.Wait()

	if string(buf) != string(frame) {
		t.Errorf("got % x, want % x", buf, frame)
	}
}

// TestTLSRejectsUnauthenticatedPeer is the property that matters. DNP3 carries
// controls that operate plant; a channel that lets an unverified peer connect
// lets anyone who can reach the port issue them.
func TestTLSRejectsUnauthenticatedPeer(t *testing.T) {
	_, ca, sCert, sKey, _, _ := certs(t)
	// A second, unrelated authority: its client certificate must be refused.
	_, otherCA, _, _, otherCert, otherKey := certs(t)
	_ = otherCA

	server, err := TLSServer("127.0.0.1:0", TLSConfig{
		CertFile: sCert, KeyFile: sKey, CAFile: ca,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })

	go func() {
		conn, err := server.Connect(t.Context())
		if err != nil {
			return
		}
		// Drive the handshake so it completes or fails rather than hanging.
		_ = handshake(t.Context(), conn)
		_ = conn.Close()
	}()

	var addr string
	for range 200 {
		if a := ServerAddr(server); a != nil {
			addr = a.String()
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if addr == "" {
		t.Fatal("the TLS server never bound")
	}

	client, err := TLSClient(addr, TLSConfig{
		CertFile: otherCert, KeyFile: otherKey, CAFile: ca, ServerName: "localhost",
	}, NoRetry)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })

	conn, err := client.Connect(t.Context())
	if err == nil {
		// Some handshake failures surface on the first read rather than at
		// dial, so push a byte through before deciding.
		_, err = conn.Write([]byte{0x05})
		if err == nil {
			buf := make([]byte, 1)
			_ = setReadDeadlineIfPossible(conn, 2*time.Second)
			_, err = conn.Read(buf)
		}
		_ = conn.Close()
	}
	if err == nil {
		t.Error("a client certificate from an unrelated authority was accepted")
	}
}

// TestTLSRequiresPeerVerification pins the refusal to build a configuration
// with no CA. Making peer verification opt-in would make it forgettable.
func TestTLSRequiresPeerVerification(t *testing.T) {
	_, _, sCert, sKey, _, _ := certs(t)

	_, err := TLSServer("127.0.0.1:0", TLSConfig{CertFile: sCert, KeyFile: sKey})
	if err == nil {
		t.Fatal("a TLS channel with no CA should be refused")
	}
	if !strings.Contains(err.Error(), "authenticate") {
		t.Errorf("the error should explain why: %v", err)
	}

	if _, err := TLSClient("host:20000", TLSConfig{CAFile: "x"}, NoRetry); err == nil {
		t.Error("a TLS channel with no certificate should be refused")
	}
}

func TestTLSMinimumVersion(t *testing.T) {
	_, ca, sCert, sKey, _, _ := certs(t)

	cfg, err := TLSConfig{CertFile: sCert, KeyFile: sKey, CAFile: ca}.build(true)
	if err != nil {
		t.Fatal(err)
	}
	// IEC 62351 sets the floor at TLS 1.2.
	if cfg.MinVersion < 0x0303 {
		t.Errorf("MinVersion = %#04x, want at least TLS 1.2 (0x0303)", cfg.MinVersion)
	}
	if cfg.ClientAuth == 0 {
		t.Error("a server must require a client certificate")
	}
}

func TestTLSBadFiles(t *testing.T) {
	_, ca, sCert, sKey, _, _ := certs(t)

	for _, tc := range []struct {
		name string
		cfg  TLSConfig
	}{
		{"missing cert", TLSConfig{CertFile: "/nope", KeyFile: sKey, CAFile: ca}},
		{"missing CA", TLSConfig{CertFile: sCert, KeyFile: sKey, CAFile: "/nope"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.cfg.build(false); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

// handshake completes a TLS handshake explicitly, since Go otherwise defers it
// to the first read or write.
func handshake(ctx context.Context, c io.ReadWriteCloser) error {
	if hs, ok := c.(interface{ HandshakeContext(context.Context) error }); ok {
		return hs.HandshakeContext(ctx)
	}
	return nil
}

// deadlineSetter is satisfied by the underlying net.Conn.
type deadlineSetter interface{ SetReadDeadline(time.Time) error }

// setReadDeadlineIfPossible bounds a read, for tests that must not block
// forever on a handshake that is going to fail.
func setReadDeadlineIfPossible(rw io.ReadWriteCloser, d time.Duration) error {
	if ds, ok := rw.(deadlineSetter); ok {
		return ds.SetReadDeadline(time.Now().Add(d))
	}
	return nil
}
