package dnp3_test

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
	"sync"
	"testing"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/channel"
	"github.com/dscsystems/go-dnp3/master"
	"github.com/dscsystems/go-dnp3/outstation"
)

// These tests run the whole stack over real sockets rather than the in-process
// pipe. The pipe proves the protocol; these prove the channels, which is where
// the differences that actually bite live — a TLS handshake that never
// completes, a UDP socket that loses its binding on reconnect.

// runPair starts a master and outstation on the given channels and polls.
func runOverChannels(t *testing.T, mch, och channel.Channel) {
	t.Helper()

	coll := newCollector()
	out := outstation.New(outstation.Config{
		LocalAddr: 10, RemoteAddr: 1,
		Database:       outstation.DatabaseConfig{Binary: 8, Analog: 4},
		ConfirmTimeout: time.Second,
	}, nil, nil)

	m := master.New(master.Config{
		LocalAddr: 1, RemoteAddr: 10, ResponseTimeout: 5 * time.Second,
	}, coll)

	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _ = out.Run(ctx, och) }()
	go func() { defer wg.Done(); _ = m.Run(ctx, mch) }()
	t.Cleanup(func() {
		cancel()
		_ = mch.Close()
		_ = och.Close()
		wg.Wait()
	})

	waitFor(t, 10*time.Second, func() bool { return m.Connected() })

	out.Update(func(db *outstation.Database) {
		db.UpdateBinary(3, dnp3.Binary{Value: true, Flags: dnp3.Online})
		db.UpdateAnalog(1, dnp3.Analog{Value: 4242, Flags: dnp3.Online})
	})

	pollCtx, pollCancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer pollCancel()
	if err := m.IntegrityPoll(pollCtx); err != nil {
		t.Fatalf("integrity poll: %v", err)
	}

	binary, analog, _ := coll.snapshot()
	if !binary[3].Value {
		t.Error("binary 3 did not arrive")
	}
	if analog[1].Value != 4242 {
		t.Errorf("analog 1 = %v, want 4242", analog[1].Value)
	}
}

func TestSessionOverTCP(t *testing.T) {
	server := channel.TCPServer("127.0.0.1:0")
	t.Cleanup(func() { _ = server.Close() })

	// Start the outstation first so the listener binds, then find the port.
	addr := bindAndWait(t, server, func() {
		coll := newCollector()
		_ = coll
	})

	client := channel.TCPClient(addr, channel.DefaultRetry)
	t.Cleanup(func() { _ = client.Close() })

	runOverChannels(t, client, server)
}

// bindAndWait forces a listening channel to bind and returns its address.
//
// The listener binds lazily on the first Connect, so something has to call it;
// the outstation session will call it again and get the same listener.
func bindAndWait(t *testing.T, server channel.Channel, _ func()) string {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())

	// The accept has to have finished before this returns, not merely been
	// cancelled. Cancelling asks it to stop, and the deadline that actually
	// stops it is set by another goroutine; until that has run, this accept is
	// still first in line for the next connection — and the next connection is
	// the session's, which this would take and drop on the floor.
	//
	// The session would then wait to accept a client that already believes it
	// is connected, and the test would fail two response timeouts later with
	// nothing in the log to say why.
	accepted := make(chan io.Closer, 1)
	go func() {
		conn, err := server.Connect(ctx)
		if err != nil {
			accepted <- nil
			return
		}
		accepted <- conn
	}()

	var addr string
	for range 400 {
		if a := channel.ServerAddr(server); a != nil {
			addr = a.String()
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()

	if conn := <-accepted; conn != nil {
		_ = conn.Close()
	}
	if addr == "" {
		t.Fatal("the server never bound")
	}
	return addr
}

func TestSessionOverTLS(t *testing.T) {
	ca, sCert, sKey, cCert, cKey := testCerts(t)

	server, err := channel.TLSServer("127.0.0.1:0", channel.TLSConfig{
		CertFile: sCert, KeyFile: sKey, CAFile: ca,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })

	addr := bindAndWait(t, server, nil)

	client, err := channel.TLSClient(addr, channel.TLSConfig{
		CertFile: cCert, KeyFile: cKey, CAFile: ca, ServerName: "localhost",
	}, channel.DefaultRetry)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })

	runOverChannels(t, client, server)
}

func TestSessionOverUDP(t *testing.T) {
	och := channel.UDPChannel(channel.UDPConfig{LocalAddr: "127.0.0.1:0"})
	t.Cleanup(func() { _ = och.Close() })

	// Bind the outstation end so its port is known.
	if _, err := och.Connect(t.Context()); err != nil {
		t.Fatal(err)
	}
	addr := channel.ServerAddr(och)
	if addr == nil {
		t.Fatal("the UDP channel did not bind")
	}

	mch := channel.UDPChannel(channel.UDPConfig{
		LocalAddr: "127.0.0.1:0", RemoteAddr: addr.String(),
	})
	t.Cleanup(func() { _ = mch.Close() })

	runOverChannels(t, mch, och)
}

// testCerts generates a CA and a mutually trusting pair, mirroring what the
// channel package's own tests do but from outside the package.
func testCerts(t *testing.T) (caFile, serverCert, serverKey, clientCert, clientKey string) {
	t.Helper()
	dir := t.TempDir()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "go-dnp3 integration CA"},
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
	caCert, _ := x509.ParseCertificate(caDER)

	caFile = filepath.Join(dir, "ca.pem")
	writeBlock(t, caFile, "CERTIFICATE", caDER)

	leaf := func(name string, serial int64, server bool) (string, string) {
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
		cp := filepath.Join(dir, name+".pem")
		kp := filepath.Join(dir, name+".key")
		writeBlock(t, cp, "CERTIFICATE", der)
		kd, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		writeBlock(t, kp, "EC PRIVATE KEY", kd)
		return cp, kp
	}

	serverCert, serverKey = leaf("server", 2, true)
	clientCert, clientKey = leaf("client", 3, false)
	return caFile, serverCert, serverKey, clientCert, clientKey
}

func writeBlock(t *testing.T, path, typ string, der []byte) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := pem.Encode(f, &pem.Block{Type: typ, Bytes: der}); err != nil {
		t.Fatal(err)
	}
}
