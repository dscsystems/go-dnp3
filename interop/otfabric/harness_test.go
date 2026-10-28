//go:build interop

// Package otfabric runs this stack against the reference implementations of
// github.com/otfabric/dnp3-interop: opendnp3, Step Function I/O dnp3 and
// dnp3py, packaged as containers with one command-line contract.
//
// It complements the parent package, which builds opendnp3 from source. Here
// nothing is built: the published images are pulled, the expected values come
// from the fixture inside the image (print-fixture), and what an image cannot
// do it says itself (print-capabilities), so a scenario it lacks is skipped
// rather than failed.
//
// Both directions run. Our master is pointed at each reference outstation
// (server), and each reference master (client) is pointed at an outstation
// built here from the same fixture.
//
//	make interop-otfabric
//
// Environment:
//
//	OTFABRIC_TAG        release tag of the images (default v0.5.1)
//	OTFABRIC_REGISTRY   image registry prefix (default ghcr.io/otfabric)
//	OTFABRIC_ADAPTERS   comma-separated adapters to run (default opendnp3,stepfunc,dnp3py)
//	INTEROP_ALLOW_SKIP  skip, instead of failing, when Docker is unavailable
//
// The stepfunc image is not published: build it from the dnp3-interop
// repository (make image-stepfunc VERSION=<tag>). Without it the adapter is
// skipped. dnp3py serves only its own fixture, so it takes part as a master
// only.
package otfabric

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	defaultTag      = "v0.5.1"
	defaultRegistry = "ghcr.io/otfabric"
	readyFile       = "/run/dnp3-interop/ready"

	// The baseline fixture's link addresses.
	outstationAddr = 1024
	masterAddr     = 1
)

func env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// adapter is one reference implementation.
type adapter struct {
	name  string
	image string

	once sync.Once
	err  error
	caps capabilities
	fix  *fixture
}

// capabilities is the part of print-capabilities the tests use.
type capabilities struct {
	Roles struct {
		Server bool `json:"server"`
		Client bool `json:"client"`
	} `json:"roles"`
	Features         map[string]bool `json:"features"`
	ClientOperations []string        `json:"clientOperations"`
}

// has reports whether the image declares a feature.
func (c capabilities) has(feature string) bool { return c.Features[feature] }

// canDo reports whether the image's master has the operation.
func (c capabilities) canDo(op string) bool {
	for _, o := range c.ClientOperations {
		if o == op {
			return true
		}
	}
	return false
}

var adapters = map[string]*adapter{}

func init() {
	registry := env("OTFABRIC_REGISTRY", defaultRegistry)
	tag := env("OTFABRIC_TAG", defaultTag)
	for _, name := range []string{"opendnp3", "stepfunc", "dnp3py"} {
		adapters[name] = &adapter{name: name, image: registry + "/dnp3-interop-" + name + ":" + tag}
	}
}

// selected lists the adapters to run, in a stable order.
func selected() []*adapter {
	var out []*adapter
	for _, name := range strings.Split(env("OTFABRIC_ADAPTERS", "opendnp3,stepfunc,dnp3py"), ",") {
		if a, ok := adapters[strings.TrimSpace(name)]; ok {
			out = append(out, a)
		}
	}
	return out
}

func docker(args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.Command("docker", args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err != nil {
		err = fmt.Errorf("docker %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), err
}

// load makes the image available and reads what it says about itself, once.
// An image that is not there and cannot be pulled skips the test: the
// stepfunc image exists only where it was built.
func (a *adapter) load(t *testing.T) {
	t.Helper()
	a.once.Do(func() {
		if _, err := docker("image", "inspect", a.image); err != nil {
			if _, err := docker("pull", "--quiet", a.image); err != nil {
				hint := ""
				if a.name == "stepfunc" {
					hint = " (not published; build it with `make image-stepfunc VERSION=" + env("OTFABRIC_TAG", defaultTag) + "` in dnp3-interop)"
				}
				a.err = fmt.Errorf("image %s is not available%s: %w", a.image, hint, err)
				return
			}
		}
		out, err := docker("run", "--rm", a.image, "print-capabilities")
		if err == nil {
			err = json.Unmarshal([]byte(out), &a.caps)
		}
		if err != nil {
			a.err = fmt.Errorf("print-capabilities: %w", err)
			return
		}
		// dnp3py serves its own fixture only; the others serve the baseline.
		fixtureName := "baseline"
		if a.name == "dnp3py" {
			fixtureName = "dnp3py"
		}
		out, err = docker("run", "--rm", a.image, "print-fixture", fixtureName)
		if err == nil {
			a.fix, err = parseFixture([]byte(out))
		}
		if err != nil {
			a.err = fmt.Errorf("print-fixture %s: %w", fixtureName, err)
		}
	})
	if a.err != nil {
		t.Skip(a.err)
	}
}

// baseline returns the baseline fixture, taken from this image when it serves
// it and from the first image that does otherwise.
func baseline(t *testing.T) *fixture {
	t.Helper()
	for _, a := range selected() {
		if a.name == "dnp3py" {
			continue
		}
		a.load(t)
		return a.fix
	}
	t.Skip("no adapter serving the baseline fixture is selected")
	return nil
}

// refServer is a running reference outstation.
type refServer struct {
	name string
	port string
}

// startServer runs the image's outstation and returns when it is ready. The
// readiness file is the signal, never a delay.
func (a *adapter) startServer(t *testing.T) *refServer {
	t.Helper()
	a.load(t)
	if !a.caps.Roles.Server {
		t.Skipf("%s has no outstation", a.name)
	}

	args := []string{"run", "-d", "--rm", "-p", "127.0.0.1:0:20000", "--label", "dnp3-interop-go-dnp3=1"}
	cmdArgs := []string{"server"}
	if a.name == "dnp3py" {
		cmdArgs = append(cmdArgs, "--fixture", "/fixtures/dnp3py/fixture.json")
	}
	out, err := docker(append(append(args, a.image), cmdArgs...)...)
	if err != nil {
		t.Fatalf("start %s: %v", a.name, err)
	}
	s := &refServer{name: strings.TrimSpace(out)}
	t.Cleanup(func() { _, _ = docker("rm", "-f", s.name) })

	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := docker("exec", s.name, "test", "-f", readyFile); err == nil {
			break
		}
		if time.Now().After(deadline) {
			logs, _ := docker("logs", s.name)
			t.Fatalf("%s outstation not ready after 30 s:\n%s", a.name, logs)
		}
		time.Sleep(100 * time.Millisecond)
	}

	out, err = docker("port", s.name, "20000/tcp")
	if err != nil {
		t.Fatal(err)
	}
	line, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	s.port = line[strings.LastIndex(line, ":")+1:]
	return s
}

// event is one line of a reference server's stdout.
type event map[string]any

func (e event) str(key string) string { s, _ := e[key].(string); return s }

// events returns what the server has printed so far, in order.
func (s *refServer) events(t *testing.T) []event {
	t.Helper()
	out, err := docker("logs", s.name)
	if err != nil {
		t.Fatal(err)
	}
	var evs []event
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var e event
		if json.Unmarshal([]byte(line), &e) == nil && e["event"] != nil {
			evs = append(evs, e)
		}
	}
	return evs
}

// commands returns the server's command events with the given function.
func (s *refServer) commands(t *testing.T, function string) []event {
	var out []event
	for _, e := range s.events(t) {
		if e.str("event") == "command" && e.str("function") == function {
			out = append(out, e)
		}
	}
	return out
}

// object is one measurement in a client result document.
type object struct {
	Group       int      `json:"group"`
	Variation   int      `json:"variation"`
	Index       int      `json:"index"`
	Value       any      `json:"value"`
	Flags       []string `json:"flags"`
	Unsolicited bool     `json:"unsolicited"`
}

// result is a client result document.
type result struct {
	OK    bool `json:"ok"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Connected      bool     `json:"connected"`
	IIN            []string `json:"iin"`
	Objects        []object `json:"objects"`
	CommandStatus  *string  `json:"commandStatus"`
	RestartDelayMs *float64 `json:"restartDelayMs"`
}

func (r result) errCode() string {
	if r.Error == nil {
		return ""
	}
	return r.Error.Code
}

func (r result) hasIIN(name string) bool {
	for _, n := range r.IIN {
		if n == name {
			return true
		}
	}
	return false
}

// hostFor says how a container reaches a listener on this machine: the host
// network on Linux, the Docker Desktop alias elsewhere.
func hostFor() (bindHost, containerHost string, dockerArgs []string) {
	if runtime.GOOS == "linux" {
		return "127.0.0.1", "127.0.0.1", []string{"--network", "host"}
	}
	return "0.0.0.0", "host.docker.internal", []string{"--add-host", "host.docker.internal:host-gateway"}
}

// client runs one operation of the image's master against host:port and
// returns the result document. The exit code must agree with the document.
func (a *adapter) client(t *testing.T, port string, op string, flags ...string) result {
	t.Helper()
	a.load(t)
	if !a.caps.Roles.Client {
		t.Skipf("%s has no master", a.name)
	}
	if !a.caps.canDo(op) {
		t.Skipf("%s does not declare the client operation %s", a.name, op)
	}

	_, host, netArgs := hostFor()
	args := append([]string{"run", "--rm"}, netArgs...)
	args = append(args, a.image, "client", op, "--host", host, "--port", port)
	args = append(args, flags...)

	var stdout bytes.Buffer
	cmd := exec.Command("docker", args...)
	cmd.Stdout = &stdout
	exit := 0
	if err := cmd.Run(); err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("docker %s: %v", strings.Join(args, " "), err)
		}
		exit = ee.ExitCode()
	}

	var res result
	var raw string
	for _, line := range strings.Split(stdout.String(), "\n") {
		if strings.HasPrefix(line, "{") {
			raw = line
		}
	}
	if raw == "" {
		t.Fatalf("%s client %s printed no result document (exit %d): %q", a.name, op, exit, stdout.String())
	}
	if err := json.Unmarshal([]byte(raw), &res); err != nil {
		t.Fatalf("%s client %s: bad result document: %v: %s", a.name, op, err, raw)
	}
	if res.OK != (exit == 0) {
		t.Errorf("%s client %s: ok=%v but exit code %d", a.name, op, res.OK, exit)
	}
	return res
}

// dockerAvailable reports whether a daemon is reachable.
func dockerAvailable() bool {
	_, err := docker("info", "--format", "{{.ServerVersion}}")
	return err == nil
}

// TestMain refuses to report success for a run that tested nothing.
func TestMain(m *testing.M) {
	if !dockerAvailable() {
		if os.Getenv("INTEROP_ALLOW_SKIP") != "" {
			fmt.Println("interop: docker is not available; skipping")
			return
		}
		fmt.Fprintln(os.Stderr, "interop: docker is not available, so nothing was tested "+
			"(set INTEROP_ALLOW_SKIP=1 to skip instead)")
		os.Exit(1)
	}
	code := m.Run()
	// Containers the tests left behind (a killed run) are labelled.
	if out, err := docker("ps", "-aq", "--filter", "label=dnp3-interop-go-dnp3=1"); err == nil {
		for _, id := range strings.Fields(out) {
			_, _ = docker("rm", "-f", id)
		}
	}
	os.Exit(code)
}
