package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/dscsystems/go-dnp3"
	"gopkg.in/yaml.v3"
)

// Config is a set of outstations to poll.
type Config struct {
	// Local is this master's link address, shared by every session unless a
	// site overrides it.
	Local uint16 `yaml:"local"`

	// Defaults apply to any site that does not say otherwise.
	Defaults SiteDefaults `yaml:"defaults"`

	Sites []Site `yaml:"sites"`
}

// SiteDefaults are the settings a site inherits.
type SiteDefaults struct {
	Poll         time.Duration `yaml:"poll"`
	Integrity    time.Duration `yaml:"integrity"`
	Timeout      time.Duration `yaml:"timeout"`
	KeepAlive    time.Duration `yaml:"keep_alive"`
	Unsolicited  bool          `yaml:"unsolicited"`
	LinkConfirms bool          `yaml:"link_confirms"`
	SyncClock    bool          `yaml:"sync_clock"`
}

// Site is one outstation.
type Site struct {
	// Name identifies the site in logs, CSV rows and the HTTP API.
	Name string `yaml:"name"`

	// Host is host:port for TCP or TLS; Serial is a port name. Exactly one is
	// required.
	Host   string `yaml:"host"`
	Serial string `yaml:"serial"`
	Baud   int    `yaml:"baud"`

	// Local overrides the master's link address for this site, and Address is
	// the outstation's.
	Local   uint16 `yaml:"local"`
	Address uint16 `yaml:"address"`

	// TLS enables and configures a mutually authenticated channel.
	TLS *TLSFiles `yaml:"tls"`

	Poll         time.Duration `yaml:"poll"`
	Integrity    time.Duration `yaml:"integrity"`
	Timeout      time.Duration `yaml:"timeout"`
	KeepAlive    time.Duration `yaml:"keep_alive"`
	Unsolicited  *bool         `yaml:"unsolicited"`
	LinkConfirms *bool         `yaml:"link_confirms"`
	SyncClock    *bool         `yaml:"sync_clock"`
}

// TLSFiles names the certificates for a TLS site.
type TLSFiles struct {
	Cert       string `yaml:"cert"`
	Key        string `yaml:"key"`
	CA         string `yaml:"ca"`
	ServerName string `yaml:"server_name"`
}

// DefaultConfig is a single outstation on localhost, which is what someone
// trying the tool against dnp3-outstation wants.
func DefaultConfig() *Config {
	return &Config{
		Local: 1,
		Defaults: SiteDefaults{
			Poll:      5 * time.Second,
			Integrity: 5 * time.Minute,
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
			SyncClock: true,
		},
	}
}

// LoadConfig reads a configuration file.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := DefaultConfig()

	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true) // a typo must fail rather than be silently ignored
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// Validate reports configuration problems before any connection is attempted.
//
// Failing at startup beats failing at three in the morning when a poll that
// was never actually configured turns out to be missing.
func (c *Config) Validate() error {
	if len(c.Sites) == 0 {
		return fmt.Errorf("no sites configured")
	}

	names := map[string]bool{}
	for i := range c.Sites {
		s := &c.Sites[i]
		if s.Name == "" {
			return fmt.Errorf("site %d has no name", i)
		}
		if names[s.Name] {
			return fmt.Errorf("two sites are both named %q; names identify rows in the recording", s.Name)
		}
		names[s.Name] = true

		if s.Host == "" && s.Serial == "" {
			return fmt.Errorf("site %q has neither a host nor a serial port", s.Name)
		}
		if s.Host != "" && s.Serial != "" {
			return fmt.Errorf("site %q has both a host and a serial port; pick one", s.Name)
		}
		if s.Address == 0 {
			return fmt.Errorf("site %q has no outstation address", s.Name)
		}
		if s.TLS != nil {
			if s.TLS.Cert == "" || s.TLS.Key == "" || s.TLS.CA == "" {
				return fmt.Errorf("site %q: TLS needs a cert, a key and a CA — "+
					"a channel that does not verify its peer lets anyone who can reach the port operate plant",
					s.Name)
			}
			if s.Serial != "" {
				return fmt.Errorf("site %q: TLS over a serial port is not a thing", s.Name)
			}
		}

		local := s.Local
		if local == 0 {
			local = c.Local
		}
		if local == s.Address {
			return fmt.Errorf("site %q: the master and outstation addresses are both %d", s.Name, local)
		}
	}
	return nil
}

// resolved returns a site with the defaults folded in.
func (c *Config) resolved(s Site) Site {
	if s.Local == 0 {
		s.Local = c.Local
	}
	if s.Poll == 0 {
		s.Poll = c.Defaults.Poll
	}
	if s.Integrity == 0 {
		s.Integrity = c.Defaults.Integrity
	}
	if s.Timeout == 0 {
		s.Timeout = c.Defaults.Timeout
	}
	if s.KeepAlive == 0 {
		s.KeepAlive = c.Defaults.KeepAlive
	}
	if s.Baud == 0 {
		s.Baud = 9600
	}
	if s.Unsolicited == nil {
		s.Unsolicited = &c.Defaults.Unsolicited
	}
	if s.LinkConfirms == nil {
		s.LinkConfirms = &c.Defaults.LinkConfirms
	}
	if s.SyncClock == nil {
		s.SyncClock = &c.Defaults.SyncClock
	}
	// A serial link has no transport-layer delivery guarantee, so link-layer
	// confirmation is what makes it reliable. Defaulting it on there rather
	// than making every serial configuration remember it.
	if s.Serial != "" && !*s.LinkConfirms {
		on := true
		s.LinkConfirms = &on
	}
	return s
}

// unsolMask returns the classes to enable for unsolicited reporting.
func (s Site) unsolMask() dnp3.Class {
	if s.Unsolicited != nil && *s.Unsolicited {
		return dnp3.Class123
	}
	return 0
}

func (s Site) describe() string {
	switch {
	case s.Serial != "":
		return fmt.Sprintf("serial %s at %d baud", s.Serial, s.Baud)
	case s.TLS != nil:
		return "TLS " + s.Host
	default:
		return "TCP " + s.Host
	}
}
