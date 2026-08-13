package channel

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"go.bug.st/serial"
)

// Serial is where DNP3 came from, and where its link layer earns its keep.
//
// A serial line has no framing of its own, no delivery guarantee and no
// ordering beyond what arrives: the 0x0564 delimiter, the per-block CRCs and
// the link-layer confirmation exist precisely because of it. Enable
// UseLinkConfirms on a session using this channel — without it a corrupted
// frame is simply lost, with nothing to notice or repair it.

// Parity selects the parity checking mode.
type Parity string

// Parity modes.
const (
	ParityNone  Parity = "none"
	ParityOdd   Parity = "odd"
	ParityEven  Parity = "even"
	ParityMark  Parity = "mark"
	ParitySpace Parity = "space"
)

// StopBits selects the number of stop bits.
type StopBits string

// Stop bit modes.
const (
	StopBits1       StopBits = "1"
	StopBits1Point5 StopBits = "1.5"
	StopBits2       StopBits = "2"
)

// SerialConfig describes a serial port.
type SerialConfig struct {
	// Device is the port name: /dev/ttyUSB0, COM3, and so on.
	Device string
	// Baud is the line rate. Zero uses 9600, the DNP3 convention.
	Baud int
	// DataBits is the character size. Zero uses 8.
	DataBits int
	// Parity defaults to none.
	Parity Parity
	// StopBits defaults to one.
	StopBits StopBits

	// ReadTimeout bounds a blocking read so a session's context can be
	// noticed. Zero uses one second.
	//
	// It is not a protocol timeout: an idle line legitimately produces
	// nothing for minutes at a time, and a read returning empty is not an
	// error.
	ReadTimeout time.Duration
}

func (c *SerialConfig) applyDefaults() {
	if c.Baud == 0 {
		c.Baud = 9600
	}
	if c.DataBits == 0 {
		c.DataBits = 8
	}
	if c.Parity == "" {
		c.Parity = ParityNone
	}
	if c.StopBits == "" {
		c.StopBits = StopBits1
	}
	if c.ReadTimeout <= 0 {
		c.ReadTimeout = time.Second
	}
}

func (c SerialConfig) mode() (*serial.Mode, error) {
	m := &serial.Mode{
		BaudRate: c.Baud,
		DataBits: c.DataBits,
	}

	switch c.Parity {
	case ParityNone:
		m.Parity = serial.NoParity
	case ParityOdd:
		m.Parity = serial.OddParity
	case ParityEven:
		m.Parity = serial.EvenParity
	case ParityMark:
		m.Parity = serial.MarkParity
	case ParitySpace:
		m.Parity = serial.SpaceParity
	default:
		return nil, fmt.Errorf("channel: unknown parity %q", c.Parity)
	}

	switch c.StopBits {
	case StopBits1:
		m.StopBits = serial.OneStopBit
	case StopBits1Point5:
		m.StopBits = serial.OnePointFiveStopBits
	case StopBits2:
		m.StopBits = serial.TwoStopBits
	default:
		return nil, fmt.Errorf("channel: unknown stop bits %q", c.StopBits)
	}
	return m, nil
}

// SerialChannel returns a channel over a serial port, reopening it with
// backoff if it goes away — which happens routinely with USB adapters.
func SerialChannel(cfg SerialConfig, retry Retry) Channel {
	cfg.applyDefaults()
	return &serialChannel{cfg: cfg, retry: retry}
}

type serialChannel struct {
	cfg     SerialConfig
	retry   Retry
	attempt int

	mu     sync.Mutex
	closed bool
	port   serial.Port
}

func (c *serialChannel) Connect(ctx context.Context) (io.ReadWriteCloser, error) {
	mode, err := c.cfg.mode()
	if err != nil {
		return nil, err
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

		port, oerr := serial.Open(c.cfg.Device, mode)
		if oerr == nil {
			if err := port.SetReadTimeout(c.cfg.ReadTimeout); err != nil {
				_ = port.Close()
				return nil, fmt.Errorf("channel: setting the read timeout: %w", err)
			}
			c.mu.Lock()
			c.port = port
			c.mu.Unlock()
			c.attempt = 0
			return &serialConn{port: port}, nil
		}

		if c.retry.Min <= 0 {
			return nil, fmt.Errorf("channel: opening %s: %w", c.cfg.Device, oerr)
		}
		if serr := c.retry.sleep(ctx, c.attempt); serr != nil {
			return nil, serr
		}
		c.attempt++
	}
}

func (c *serialChannel) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	if c.port != nil {
		err := c.port.Close()
		c.port = nil
		return err
	}
	return nil
}

func (c *serialChannel) String() string {
	return fmt.Sprintf("serial %s %d/%d%s%s",
		c.cfg.Device, c.cfg.Baud, c.cfg.DataBits,
		strings.ToUpper(string(c.cfg.Parity)[:1]), c.cfg.StopBits)
}

// serialConn adapts a serial port to the channel's stream contract.
type serialConn struct{ port serial.Port }

// Read returns when octets arrive or the port's read timeout expires.
//
// A timeout yields zero octets and no error, which io.Reader permits but
// callers rarely expect. Returning io.EOF instead would tell the session the
// line had gone away, and an idle DNP3 link is silent by design — a polled
// outstation says nothing at all between polls.
func (s *serialConn) Read(p []byte) (int, error) {
	for {
		n, err := s.port.Read(p)
		if err != nil {
			return n, err
		}
		if n > 0 {
			return n, nil
		}
		// Zero octets and no error: the read timed out on an idle line. Loop
		// rather than reporting a spurious end of stream.
	}
}

func (s *serialConn) Write(p []byte) (int, error) { return s.port.Write(p) }

func (s *serialConn) Close() error { return s.port.Close() }

// ListSerialPorts returns the serial ports the system reports, which is what a
// configuration tool offers a user rather than making them guess a device
// name.
func ListSerialPorts() ([]string, error) {
	return serial.GetPortsList()
}
