package multidrop

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/dscsystems/go-dnp3/channel"
	"github.com/dscsystems/go-dnp3/internal/link"
	"github.com/dscsystems/go-dnp3/secevent"
)

type busEvents struct {
	mu     sync.Mutex
	events []secevent.Event
}

func (b *busEvents) HandleEvent(e secevent.Event) {
	b.mu.Lock()
	b.events = append(b.events, e)
	b.mu.Unlock()
}

func (b *busEvents) wait(t *testing.T, k secevent.Kind) secevent.Event {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		b.mu.Lock()
		for _, e := range b.events {
			if e.Kind == k {
				b.mu.Unlock()
				return e
			}
		}
		b.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no %s event", k)
	return secevent.Event{}
}

func TestBusReportsLineEvents(t *testing.T) {
	ev := &busEvents{}
	far, near := channel.Pipe()
	bus := New(near, Config{Security: &secevent.Policy{Sink: secevent.SyncSink(ev)}})
	t.Cleanup(func() { _ = bus.Close() })

	st, err := bus.Add(Station{LocalAddr: 10, RemoteAddr: 1})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if _, err := st.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	line, err := far.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = line.Close() })

	if e := ev.wait(t, secevent.KindConnConnected); e.Role != "bus" || e.Transport != "multidrop" {
		t.Errorf("connected event %+v", e)
	}

	frame := func(dest uint16) []byte {
		b, err := link.Encode(nil, link.Header{Control: link.Control{Dir: true, Prm: true, Func: link.FuncRequestLinkStatus},
			Dest: dest, Src: 1}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	bad := frame(10)
	bad[8] ^= 0xFF
	for _, b := range [][]byte{frame(77), bad, frame(10)} {
		if _, err := line.Write(b); err != nil {
			t.Fatal(err)
		}
	}
	if e := ev.wait(t, secevent.KindLinkForeignDest); e.LinkDest != 77 {
		t.Errorf("foreign dest event %+v", e)
	}
	if e := ev.wait(t, secevent.KindLinkCRCError); e.Severity != secevent.Notice {
		t.Errorf("CRC error severity %v on a bus", e.Severity)
	}
}
