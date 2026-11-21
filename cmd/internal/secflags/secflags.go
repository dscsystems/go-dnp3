// Package secflags gives the command-line tools their security event flags.
package secflags

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/netip"
	"os"
	"strconv"
	"strings"

	"github.com/dscsystems/go-dnp3/secevent"
)

// Flags are the parsed security event options.
type Flags struct {
	outstation bool

	events         string
	minSeverity    string
	allowPeers     prefixList
	rejectUnlisted bool

	allowedMasters         addrList
	rejectUnexpectedMaster bool
	readOnly               bool
	floodAction            string
}

// Register adds the flags to fs. The outstation-only ones, which concern a
// server channel and the requests it serves, are added when outstation is set.
func Register(fs *flag.FlagSet, outstation bool) *Flags {
	f := &Flags{outstation: outstation}
	fs.StringVar(&f.events, "events", "", "report security events as JSON lines: `json` for stderr, or a file to append to")
	fs.StringVar(&f.minSeverity, "events-min", "info", "report only events at or above `severity` (info, notice, warning, alert, critical)")
	if outstation {
		fs.Var(&f.allowPeers, "allow-peer", "expect connections only from `address` or prefix; repeatable")
		fs.BoolVar(&f.rejectUnlisted, "reject-unlisted", false, "close connections from addresses not given by -allow-peer")
		fs.Var(&f.allowedMasters, "allowed-master", "serve only master link `address`; repeatable")
		fs.BoolVar(&f.rejectUnexpectedMaster, "reject-unexpected-master", false, "drop frames from masters not given by -allowed-master")
		fs.BoolVar(&f.readOnly, "read-only", false, "refuse controls, restarts, configuration changes and file writes (time writes still allowed)")
		fs.StringVar(&f.floodAction, "flood-action", "observe", "what to do beyond the flood limits: `observe`, throttle or disconnect")
	}
	return f
}

// Policy builds the policy the flags describe, or nil when none was given.
// The returned function flushes and closes the event stream; call it after
// every session using the policy has stopped.
func (f *Flags) Policy() (*secevent.Policy, func() error, error) {
	noop := func() error { return nil }
	if !f.any() {
		return nil, noop, nil
	}
	min, err := parseSeverity(f.minSeverity)
	if err != nil {
		return nil, noop, err
	}
	action, err := parseAction(f.floodAction)
	if err != nil {
		return nil, noop, err
	}
	if f.rejectUnlisted && len(f.allowPeers) == 0 {
		return nil, noop, errors.New("-reject-unlisted needs -allow-peer")
	}
	if f.rejectUnexpectedMaster && len(f.allowedMasters) == 0 {
		return nil, noop, errors.New("-reject-unexpected-master needs -allowed-master")
	}

	p := &secevent.Policy{
		AllowedPeers:           f.allowPeers,
		RejectUnlisted:         f.rejectUnlisted,
		RejectUnexpectedMaster: f.rejectUnexpectedMaster,
		FloodAction:            action,
	}
	if len(f.allowedMasters) > 0 {
		p.AllowedMasters = f.allowedMasters
	}
	if f.readOnly {
		p.Functions = &secevent.FunctionPolicy{ReadOnly: true, AllowTimeWrite: true}
	}

	closer := noop
	if f.events != "" {
		var w io.Writer = os.Stderr
		var file *os.File
		if f.events != "json" {
			if file, err = os.OpenFile(f.events, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640); err != nil {
				return nil, noop, fmt.Errorf("opening the event file: %w", err)
			}
			w = file
		}
		var sink secevent.Sink = secevent.JSONLinesSink(w)
		if min > secevent.Info {
			sink = secevent.FilterSink(min, nil, sink)
		}
		// One dispatcher for every session, so a master polling many sites
		// writes one ordered stream.
		d := secevent.NewDispatcher(sink, secevent.DispatcherOptions{})
		p.Dispatcher = d
		closer = func() error {
			err := d.Close()
			if file != nil {
				err = errors.Join(err, file.Close())
			}
			return err
		}
	}
	return p, closer, nil
}

func (f *Flags) any() bool {
	return f.events != "" || len(f.allowPeers) > 0 || f.rejectUnlisted ||
		len(f.allowedMasters) > 0 || f.rejectUnexpectedMaster || f.readOnly ||
		(f.floodAction != "" && f.floodAction != "observe")
}

func parseSeverity(s string) (secevent.Severity, error) {
	for _, v := range []secevent.Severity{secevent.Info, secevent.Notice, secevent.Warning, secevent.Alert, secevent.Critical} {
		if strings.EqualFold(s, v.String()) {
			return v, nil
		}
	}
	return 0, fmt.Errorf("-events-min: unknown severity %q", s)
}

func parseAction(s string) (secevent.Action, error) {
	switch strings.ToLower(s) {
	case "", "observe":
		return secevent.Observed, nil
	case "throttle":
		return secevent.Throttled, nil
	case "disconnect":
		return secevent.Disconnected, nil
	}
	return 0, fmt.Errorf("-flood-action: want observe, throttle or disconnect, not %q", s)
}

// prefixList collects repeated address or prefix flags.
type prefixList []netip.Prefix

func (l *prefixList) String() string {
	parts := make([]string, len(*l))
	for i, p := range *l {
		parts[i] = p.String()
	}
	return strings.Join(parts, ",")
}

func (l *prefixList) Set(v string) error {
	for _, s := range strings.Split(v, ",") {
		s = strings.TrimSpace(s)
		if p, err := netip.ParsePrefix(s); err == nil {
			*l = append(*l, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(s)
		if err != nil {
			return fmt.Errorf("not an address or prefix: %q", s)
		}
		a = a.Unmap()
		*l = append(*l, netip.PrefixFrom(a, a.BitLen()))
	}
	return nil
}

// addrList collects repeated link address flags.
type addrList []uint16

func (l *addrList) String() string {
	parts := make([]string, len(*l))
	for i, a := range *l {
		parts[i] = strconv.Itoa(int(a))
	}
	return strings.Join(parts, ",")
}

func (l *addrList) Set(v string) error {
	for _, s := range strings.Split(v, ",") {
		n, err := strconv.ParseUint(strings.TrimSpace(s), 10, 16)
		if err != nil {
			return fmt.Errorf("not a link address: %q", s)
		}
		*l = append(*l, uint16(n))
	}
	return nil
}
