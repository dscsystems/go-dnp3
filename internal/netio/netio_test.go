package netio

import (
	"bytes"
	"net"
	"testing"
	"time"
)

func TestWriterTimesOutAPeerThatDoesNotRead(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	w := WithWriteTimeout(a, 50*time.Millisecond)
	if _, err := w.Write([]byte("x")); err == nil {
		t.Fatal("a write nobody reads succeeded")
	}
	if !w.Stalled() {
		t.Fatal("the missed deadline was not recorded")
	}
}

func TestWriterWithoutTimeoutPassesThrough(t *testing.T) {
	var buf bytes.Buffer
	w := WithWriteTimeout(&buf, time.Second) // no deadlines on a buffer
	if _, err := w.Write([]byte("abc")); err != nil || buf.String() != "abc" || w.Stalled() {
		t.Fatalf("err=%v buf=%q stalled=%v", err, buf.String(), w.Stalled())
	}
}

type batcher struct{ begun, ended int }

func (b *batcher) Write(p []byte) (int, error) { return len(p), nil }
func (b *batcher) BeginMessage()               { b.begun++ }
func (b *batcher) EndMessage() error           { b.ended++; return nil }

func TestWriterPassesMessageBracketingThrough(t *testing.T) {
	b := &batcher{}
	w := WithWriteTimeout(b, time.Second)
	w.BeginMessage()
	_ = w.EndMessage()
	if b.begun != 1 || b.ended != 1 {
		t.Fatalf("begin=%d end=%d", b.begun, b.ended)
	}
}
