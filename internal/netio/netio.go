// Package netio holds small connection helpers the sessions share.
package netio

import (
	"errors"
	"io"
	"os"
	"time"
)

type writeDeadliner interface {
	SetWriteDeadline(time.Time) error
}

type messageWriter interface {
	BeginMessage()
	EndMessage() error
}

// Writer bounds every write to a connection with a deadline.
//
// A session writes on its own goroutine, so a peer that stops reading — a TCP
// zero window — would otherwise park that goroutine in Write for good: no
// timers, no confirms, no unsolicited reporting, no application updates. With
// a deadline the write fails instead, and the session drops the connection.
//
// A Writer is used from one goroutine, like the stack it feeds.
type Writer struct {
	w       io.Writer
	dl      writeDeadliner
	timeout time.Duration
	stalled bool
}

// WithWriteTimeout wraps w so each write must finish within timeout. It
// returns w's wrapper only when the timeout is positive and w can take a
// deadline; otherwise the Writer passes writes straight through.
func WithWriteTimeout(w io.Writer, timeout time.Duration) *Writer {
	out := &Writer{w: w}
	if dl, ok := w.(writeDeadliner); ok && timeout > 0 {
		out.dl, out.timeout = dl, timeout
	}
	return out
}

// Write writes p under the deadline.
func (w *Writer) Write(p []byte) (int, error) {
	if w.dl != nil {
		_ = w.dl.SetWriteDeadline(time.Now().Add(w.timeout))
	}
	n, err := w.w.Write(p)
	if err != nil && errors.Is(err, os.ErrDeadlineExceeded) {
		w.stalled = true
	}
	return n, err
}

// Stalled reports whether a write has missed its deadline. The connection is
// unusable after that: a TLS connection in particular cannot recover from a
// write that timed out part way through a record.
func (w *Writer) Stalled() bool { return w.stalled }

// BeginMessage and EndMessage pass the message bracketing of a datagram
// transport through, so wrapping does not change how frames are grouped.
func (w *Writer) BeginMessage() {
	if mw, ok := w.w.(messageWriter); ok {
		mw.BeginMessage()
	}
}

// EndMessage ends a message started with BeginMessage.
func (w *Writer) EndMessage() error {
	if mw, ok := w.w.(messageWriter); ok {
		return mw.EndMessage()
	}
	return nil
}
