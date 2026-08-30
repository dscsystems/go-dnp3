//go:build interop

// Package interop runs this stack against other DNP3 implementations.
//
// Agreeing with yourself is not interoperability. The integration tests prove
// our master and our outstation understand each other, which two
// implementations sharing a misreading of the standard would also manage. These
// tests point our master at somebody else's outstation.
//
// They need containers built first, and are behind a build tag because of it:
//
//	make interop-build   # clone and compile opendnp3
//	make interop         # run these tests
//	make interop-reverse # drive opendnp3 against our outstation
//
// opendnp3 is the de-facto reference implementation, 
// archived at end of life in September 2022 and pinned here to
// its final release.
package interop

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dscsystems/go-dnp3"
	"github.com/dscsystems/go-dnp3/channel"
	"github.com/dscsystems/go-dnp3/master"
)

// peer describes another implementation's outstation.
type peer struct {
	name string
	// image and args start its container.
	image string
	args  []string
	// port is the host port the container's 20000 is published on.
	port string
	// address is the outstation's link address, which the two projects'
	// examples do not agree on.
	address uint16
	// points is how many of each type its demo database holds.
	points int
}

var peers = []peer{
	{
		name:    "opendnp3",
		image:   "go-dnp3-interop-opendnp3",
		args:    []string{"/src/opendnp3/build/cpp/examples/outstation/outstation-demo"},
		port:    "20500",
		address: 10,
		points:  10,
	},
}

// start runs a peer's outstation and returns when it is listening.
func (p peer) start(t *testing.T) {
	t.Helper()

	name := "interop-" + p.name
	_ = exec.Command("docker", "rm", "-f", name).Run()

	args := append([]string{
		"run", "-d", "--name", name,
		"-p", "127.0.0.1:" + p.port + ":20000",
		// Both demos read stdin to drive their databases; without a TTY they
		// see EOF and spin.
		"-i", p.image,
	}, p.args...)

	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Skipf("cannot start %s (run `make interop-build` first): %v: %s", p.name, err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })

	time.Sleep(2 * time.Second)
}

// collector records what a master receives.
type collector struct {
	master.NopHandler

	mu      sync.Mutex
	binary  map[uint16]dnp3.Binary
	analog  map[uint16]dnp3.Analog
	counter map[uint16]dnp3.Counter
	strings map[uint16]string
}

func newCollector() *collector {
	return &collector{
		binary:  map[uint16]dnp3.Binary{},
		analog:  map[uint16]dnp3.Analog{},
		counter: map[uint16]dnp3.Counter{},
		strings: map[uint16]string{},
	}
}

func (c *collector) HandleBinary(_ master.HeaderInfo, vs []dnp3.Indexed[dnp3.Binary]) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, v := range vs {
		c.binary[v.Index] = v.Value
	}
}

func (c *collector) HandleAnalog(_ master.HeaderInfo, vs []dnp3.Indexed[dnp3.Analog]) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, v := range vs {
		c.analog[v.Index] = v.Value
	}
}

func (c *collector) HandleCounter(_ master.HeaderInfo, vs []dnp3.Indexed[dnp3.Counter]) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, v := range vs {
		c.counter[v.Index] = v.Value
	}
}

func (c *collector) HandleOctetString(_ master.HeaderInfo, vs []dnp3.Indexed[dnp3.OctetString]) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, v := range vs {
		c.strings[v.Index] = string(v.Value)
	}
}

func (c *collector) counts() (b, a, ct, s int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.binary), len(c.analog), len(c.counter), len(c.strings)
}

// connect points our master at a peer.
func connect(t *testing.T, p peer) (*master.Session, *collector) {
	t.Helper()

	coll := newCollector()
	m := master.New(master.Config{
		LocalAddr: 1, RemoteAddr: p.address,
		ResponseTimeout:       5 * time.Second,
		IntegrityOnStartup:    true,
		DisableUnsolOnStartup: true,
	}, coll)

	ch := channel.TCPClient("127.0.0.1:"+p.port, channel.DefaultRetry)
	ctx, cancel := context.WithCancel(t.Context())

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = m.Run(ctx, ch) }()
	t.Cleanup(func() {
		cancel()
		_ = ch.Close()
		wg.Wait()
	})

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && !m.Connected() {
		time.Sleep(20 * time.Millisecond)
	}
	if !m.Connected() {
		t.Fatalf("never connected to %s", p.name)
	}
	return m, coll
}

// TestIntegrityPoll reads every peer's whole database.
func TestIntegrityPoll(t *testing.T) {
	for _, p := range peers {
		t.Run(p.name, func(t *testing.T) {
			p.start(t)
			m, coll := connect(t, p)

			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			if err := m.IntegrityPoll(ctx); err != nil {
				t.Fatalf("integrity poll: %v", err)
			}

			b, a, c, _ := coll.counts()
			t.Logf("%s: %d binaries, %d analogs, %d counters, IIN=%s",
				p.name, b, a, c, m.LastIIN())

			if b != p.points || a != p.points || c != p.points {
				t.Errorf("read %d/%d/%d points, want %d of each",
					b, a, c, p.points)
			}
			if st := m.Stats(); st.TasksFailed > 0 || st.ResponseTimeout > 0 {
				t.Errorf("%d task failures, %d timeouts against %s",
					st.TasksFailed, st.ResponseTimeout, p.name)
			}
		})
	}
}

// TestControls issues both command forms against every peer.
func TestControls(t *testing.T) {
	for _, p := range peers {
		t.Run(p.name, func(t *testing.T) {
			p.start(t)
			m, _ := connect(t, p)

			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()

			res, err := m.SelectAndOperate(ctx, master.LatchOn(3))
			if err != nil {
				t.Errorf("select-before-operate against %s: %v", p.name, err)
			} else {
				t.Logf("%s select+operate: %s", p.name, res)
			}

			res, err = m.DirectOperate(ctx, master.AnalogOutputFloat32(1, 42.5))
			if err != nil {
				t.Errorf("analog output against %s: %v", p.name, err)
			} else {
				t.Logf("%s analog output: %s", p.name, res)
			}
		})
	}
}

// TestClassPolls exercises the event path against every peer.
func TestClassPolls(t *testing.T) {
	for _, p := range peers {
		t.Run(p.name, func(t *testing.T) {
			p.start(t)
			m, _ := connect(t, p)

			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()

			for range 3 {
				if err := m.ScanClasses(ctx, dnp3.Class123); err != nil {
					t.Fatalf("class poll against %s: %v", p.name, err)
				}
			}
			if st := m.Stats(); st.TasksFailed > 0 {
				t.Errorf("%d task failures against %s", st.TasksFailed, p.name)
			}
		})
	}
}

// TestClockSync covers both synchronisation procedures.
//
// This is where an interop test earns its keep: some masters uses the
// recorded-time procedure by default, and until it was implemented here that
// peer could not set our outstation's clock at all. The reverse direction
// found it; this checks our own master against both peers.
func TestClockSync(t *testing.T) {
	for _, p := range peers {
		t.Run(p.name, func(t *testing.T) {
			p.start(t)
			m, _ := connect(t, p)

			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()

			if err := m.SyncTime(ctx); err != nil {
				t.Errorf("LAN clock write against %s: %v", p.name, err)
			}
			if err := m.SyncTimeWithDelay(ctx); err != nil {
				t.Errorf("delay-measured sync against %s: %v", p.name, err)
			}
		})
	}
}

// TestNoDecodeErrors reads everything and checks nothing was rejected.
//
// A peer's response that our parser refuses is the single most useful signal
// an interop run produces: it means one of the two has the encoding wrong, and
// the fragment is right there to look at.
func TestNoDecodeErrors(t *testing.T) {
	for _, p := range peers {
		t.Run(p.name, func(t *testing.T) {
			p.start(t)
			m, coll := connect(t, p)

			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()

			if err := m.IntegrityPoll(ctx); err != nil {
				t.Fatal(err)
			}
			for _, gv := range [][2]uint8{{1, 2}, {30, 1}, {30, 5}, {20, 1}, {10, 2}, {40, 1}} {
				if err := m.ScanRange(ctx, gv[0], gv[1], 0, uint16(p.points-1)); err != nil {
					t.Errorf("range read g%dv%d against %s: %v", gv[0], gv[1], p.name, err)
				}
			}

			b, a, c, s := coll.counts()
			t.Logf("%s: %d binaries, %d analogs, %d counters, %d strings", p.name, b, a, c, s)

			st := m.Stats()
			if st.TasksFailed > 0 {
				t.Errorf("%d tasks failed against %s", st.TasksFailed, p.name)
			}
		})
	}
}

// dockerAvailable reports whether a daemon is reachable, so the suite skips
// with an explanation rather than failing on a machine without one.
func dockerAvailable() bool {
	out, err := exec.Command("docker", "info").CombinedOutput()
	return err == nil && !strings.Contains(string(out), "Cannot connect")
}

func TestMain(m *testing.M) {
	if !dockerAvailable() {
		fmt.Println("interop: docker is not available; skipping")
		return
	}
	m.Run()
}
