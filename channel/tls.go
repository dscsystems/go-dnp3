package channel

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"
)

// TLSConfig describes a mutually authenticated TLS channel.
//
// Mutual authentication is not optional here. DNP3 carries controls that
// operate plant, and a channel that authenticates only the server lets anyone
// who can reach the port issue them. IEC 62351-3 requires both sides to
// present certificates, and this refuses to build a configuration that does
// not.
type TLSConfig struct {
	// CertFile and KeyFile are this end's certificate and private key.
	CertFile string
	KeyFile  string
	// CAFile is the authority that signs the peer's certificate.
	CAFile string

	// ServerName is the name to verify against the peer's certificate. For a
	// client it defaults to the dialled host.
	ServerName string

	// MinVersion is the lowest TLS version to accept. Zero uses TLS 1.2, the
	// floor IEC 62351 sets.
	MinVersion uint16
}

// buildTLS turns a TLSConfig into a crypto/tls configuration.
func (c TLSConfig) build(server bool) (*tls.Config, error) {
	if c.CertFile == "" || c.KeyFile == "" {
		return nil, fmt.Errorf("channel: TLS requires a certificate and key")
	}
	if c.CAFile == "" {
		return nil, fmt.Errorf("channel: TLS requires a CA certificate to verify the peer; " +
			"a DNP3 channel that does not authenticate its peer lets anyone who can reach the port operate plant")
	}

	cert, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("channel: loading the TLS key pair: %w", err)
	}

	caPEM, err := os.ReadFile(c.CAFile)
	if err != nil {
		return nil, fmt.Errorf("channel: reading the CA certificate: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("channel: %s contains no usable certificates", c.CAFile)
	}

	minVersion := c.MinVersion
	if minVersion == 0 {
		minVersion = tls.VersionTLS12
	}

	cfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   minVersion,
		ServerName:   c.ServerName,
		// Both directions verify the peer against the same authority.
		RootCAs:   pool,
		ClientCAs: pool,
	}
	if server {
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return cfg, nil
}

// TLSClient returns a channel that dials addr over TLS, retrying with backoff.
func TLSClient(addr string, tlsCfg TLSConfig, retry Retry) (Channel, error) {
	if tlsCfg.ServerName == "" {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("channel: %q is not host:port: %w", addr, err)
		}
		tlsCfg.ServerName = host
	}
	cfg, err := tlsCfg.build(false)
	if err != nil {
		return nil, err
	}
	return &tlsClient{addr: addr, cfg: cfg, retry: retry}, nil
}

type tlsClient struct {
	addr    string
	cfg     *tls.Config
	retry   Retry
	attempt int

	mu     sync.Mutex
	closed bool
}

func (c *tlsClient) Connect(ctx context.Context) (io.ReadWriteCloser, error) {
	dialer := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: 10 * time.Second},
		Config:    c.cfg,
	}

	for {
		c.mu.Lock()
		closed := c.closed
		c.mu.Unlock()
		if closed {
			return nil, ErrClosed
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		conn, err := dialer.DialContext(ctx, "tcp", c.addr)
		if err == nil {
			c.attempt = 0
			return conn, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if c.retry.Min <= 0 {
			return nil, err
		}
		if serr := c.retry.sleep(ctx, c.attempt); serr != nil {
			return nil, serr
		}
		c.attempt++
	}
}

func (c *tlsClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

func (c *tlsClient) String() string { return "tls-client " + c.addr }

// TLSServer returns a channel that accepts mutually authenticated TLS
// connections on addr, serving one at a time.
func TLSServer(addr string, tlsCfg TLSConfig) (Channel, error) {
	cfg, err := tlsCfg.build(true)
	if err != nil {
		return nil, err
	}
	return &tlsServer{addr: addr, cfg: cfg}, nil
}

type tlsServer struct {
	addr string
	cfg  *tls.Config

	mu sync.Mutex
	// The raw listener is kept rather than a tls.Listener so accepts can be
	// interrupted with a deadline; crypto/tls wraps the listener and does not
	// expose one. Each accepted connection is wrapped by hand instead.
	listener *net.TCPListener
	closed   bool
}

func (s *tlsServer) listen() (*net.TCPListener, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	if s.listener != nil {
		return s.listener, nil
	}
	l, err := net.Listen("tcp", s.addr)
	if err != nil {
		return nil, err
	}
	tl, ok := l.(*net.TCPListener)
	if !ok {
		_ = l.Close()
		return nil, fmt.Errorf("channel: expected a TCP listener for %q", s.addr)
	}
	s.listener = tl
	return tl, nil
}

// Addr returns the bound address, which is how a test that asked for port zero
// finds out what it got.
func (s *tlsServer) Addr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener == nil {
		return nil
	}
	return s.listener.Addr()
}

func (s *tlsServer) Connect(ctx context.Context) (io.ReadWriteCloser, error) {
	l, err := s.listen()
	if err != nil {
		return nil, err
	}
	raw, err := acceptContext(ctx, l)
	if err != nil {
		return nil, err
	}
	return tls.Server(raw, s.cfg), nil
}

func (s *tlsServer) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.listener != nil {
		return s.listener.Close()
	}
	return nil
}

func (s *tlsServer) String() string { return "tls-server " + s.addr }

// ServerAddr returns the bound address of a listening channel, or nil if it is
// not a server or has not bound yet.
func ServerAddr(c Channel) net.Addr {
	switch s := c.(type) {
	case *tcpServer:
		return s.Addr()
	case *tlsServer:
		return s.Addr()
	case *udpChannel:
		return s.Addr()
	}
	return nil
}
