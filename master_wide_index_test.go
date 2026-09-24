package dnp3_test

import (
	"context"
	"encoding/binary"
	"sync"
	"testing"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/channel"
	"github.com/dscsystems/go-dnp3/internal/app"
	"github.com/dscsystems/go-dnp3/internal/stack"
	"github.com/dscsystems/go-dnp3/master"
)

// indexRecorder records the index of every binary the master delivers.
type indexRecorder struct {
	master.NopHandler
	mu      sync.Mutex
	indexes []uint32
}

func (r *indexRecorder) HandleBinary(_ master.HeaderInfo, vs []dnp3.Indexed[dnp3.Binary]) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, v := range vs {
		r.indexes = append(r.indexes, v.Index)
	}
}

func (r *indexRecorder) got() []uint32 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]uint32(nil), r.indexes...)
}

// An outstation may address points with a four-octet range or prefix, so a
// master has to be able to report every index that can arrive. Narrowing to
// 16 bits delivered point 70000 as point 4464 — a real measurement attributed
// to a different point, with nothing to say so.
func TestMasterDeliversIndexesAbove16Bits(t *testing.T) {
	const (
		ranged   = 70000 // by a 32-bit start-stop range
		prefixed = 70010 // by a 32-bit index prefix
	)

	mch, och := channel.Pipe()
	rec := &indexRecorder{}
	m := master.New(master.Config{LocalAddr: 1, RemoteAddr: 10, ResponseTimeout: 3 * time.Second}, rec)

	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = m.Run(ctx, mch) }()

	conn, err := och.Connect(ctx)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	var mu sync.Mutex
	armed := false

	// Answers everything with an empty response until armed, then answers one
	// request with two binaries at indexes beyond 16 bits.
	wg.Add(1)
	go func() {
		defer wg.Done()
		st := stack.New(stack.Config{LocalAddr: 10, RemoteAddr: 1, IsMaster: false})
		buf := make([]byte, stack.ReadChunk)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				_ = st.Receive(conn, buf[:n], func(r stack.Received) {
					frag, perr := app.ParseFragment(nil, r.Fragment)
					if perr != nil || frag.Header.Func == app.FuncConfirm {
						return
					}
					mu.Lock()
					fire := armed
					armed = false
					mu.Unlock()

					var body []byte
					if fire {
						// g1v2 over a 32-bit start-stop range.
						body = app.AppendObjectHeader(body, app.ObjectHeader{
							Group: 1, Variation: 2,
							Qualifier: app.MakeQualifier(app.PrefixNone, app.RangeStartStop32),
							Range:     app.Range{Spec: app.RangeStartStop32, Start: ranged, Stop: ranged, Count: 1},
							Data:      []byte{0x81},
						})
						// g1v2 with a 32-bit index prefix.
						data := binary.LittleEndian.AppendUint32(nil, prefixed)
						body = app.AppendObjectHeader(body, app.ObjectHeader{
							Group: 1, Variation: 2,
							Qualifier: app.MakeQualifier(app.PrefixIndex4, app.RangeCount32),
							Range:     app.Range{Spec: app.RangeCount32, Count: 1},
							Data:      append(data, 0x81),
						})
					}
					_ = st.SendTo(conn, r.Source, response(
						app.Control{Fir: true, Fin: true, Seq: frag.Header.Control.Seq}, body))
				})
			}
			if err != nil {
				return
			}
		}
	}()

	t.Cleanup(func() {
		cancel()
		_ = conn.Close()
		_ = mch.Close()
		_ = och.Close()
		wg.Wait()
	})

	waitFor(t, 3*time.Second, func() bool { return m.Connected() })
	time.Sleep(300 * time.Millisecond) // let the startup sequence finish

	mu.Lock()
	armed = true
	mu.Unlock()

	pollCtx, pollCancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer pollCancel()
	if err := m.IntegrityPoll(pollCtx); err != nil {
		t.Fatalf("integrity poll: %v", err)
	}

	got := rec.got()
	if len(got) != 2 || got[0] != ranged || got[1] != prefixed {
		t.Errorf("delivered indexes %v, want [%d %d]", got, ranged, prefixed)
	}
}
