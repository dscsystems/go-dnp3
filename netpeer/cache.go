package netpeer

import (
	"bytes"
	"container/list"
	"encoding/json"
	"io"
	"net"
	"net/netip"
	"sync"
	"time"
)

// Defaults for a [Cache].
const (
	DefaultHistoryTTL   = 30 * 24 * time.Hour
	DefaultCacheEntries = 4096
	DefaultSharedWindow = time.Minute
)

// ChangeKind says what changed about a peer.
type ChangeKind uint8

// Changes a [Cache] reports.
const (
	// MACChanged: the IP was last seen with a different MAC.
	MACChanged ChangeKind = iota + 1
	// IPChanged: the MAC was last seen at a different IP.
	IPChanged
	// MACShared: the MAC is in use at another on-link IP at the same time.
	MACShared
	// LinkAddrChanged: the IP used a different DNP3 link address before.
	LinkAddrChanged
	// CertChanged: the IP presented a different TLS certificate before.
	CertChanged
)

// Change is one difference between what a peer shows now and what the cache
// remembers.
type Change struct {
	Kind        ChangeKind
	IP          netip.Addr
	OldIP       netip.Addr
	OldMAC      net.HardwareAddr
	NewMAC      net.HardwareAddr
	OldLinkAddr uint16
	NewLinkAddr uint16
	OldCert     string
	NewCert     string
	// LastSeen is when the previous value was last observed.
	LastSeen time.Time
}

// Observation is what is known about a peer at one moment. Zero fields are
// unknown and are neither compared nor recorded.
type Observation struct {
	IP     netip.Addr
	MAC    net.HardwareAddr
	Source Source
	// LinkAddr is the DNP3 source address; HasLinkAddr says it is set, since
	// zero is a valid address.
	LinkAddr    uint16
	HasLinkAddr bool
	CertSHA256  string
	Time        time.Time
}

// Cache remembers, per IP, the MAC, DNP3 link address and TLS certificate
// last seen, and reports when they change. It is bounded and safe for
// concurrent use.
//
// Only MACs from the neighbour table (SourceNeighbor, SourceStatic,
// SourceCustom) are remembered against an IP: a gateway's MAC says nothing
// about the peer behind it.
type Cache struct {
	mu           sync.Mutex
	ttl          time.Duration
	max          int
	sharedWindow time.Duration
	byIP         map[netip.Addr]*list.Element
	byMAC        map[string]netip.Addr // MAC → IP it was last seen at
	order        *list.List            // of *entry, least recently seen first
}

type entry struct {
	IP          netip.Addr       `json:"ip"`
	MAC         net.HardwareAddr `json:"-"`
	MACText     string           `json:"mac,omitempty"`
	MACSeen     time.Time        `json:"mac_seen,omitzero"`
	LinkAddr    uint16           `json:"link_addr"`
	HasLinkAddr bool             `json:"has_link_addr,omitempty"`
	Cert        string           `json:"cert,omitempty"`
	FirstSeen   time.Time        `json:"first_seen"`
	LastSeen    time.Time        `json:"last_seen"`
}

// NewCache returns a cache that forgets entries not seen for ttl and holds at
// most max of them. Zero values take the defaults.
func NewCache(ttl time.Duration, max int) *Cache {
	if ttl <= 0 {
		ttl = DefaultHistoryTTL
	}
	if max <= 0 {
		max = DefaultCacheEntries
	}
	return &Cache{ttl: ttl, max: max, sharedWindow: DefaultSharedWindow,
		byIP: map[netip.Addr]*list.Element{}, byMAC: map[string]netip.Addr{}, order: list.New()}
}

var (
	defaultCacheOnce sync.Once
	defaultCache     *Cache
)

// DefaultCache returns the process-wide cache.
func DefaultCache() *Cache {
	defaultCacheOnce.Do(func() { defaultCache = NewCache(0, 0) })
	return defaultCache
}

func peerMAC(s Source) bool { return s == SourceNeighbor || s == SourceStatic || s == SourceCustom }

// Observe records what a peer shows now and returns what changed since it
// was last seen.
func (c *Cache) Observe(o Observation) []Change {
	if !o.IP.IsValid() {
		return nil
	}
	o.IP = o.IP.Unmap()
	if o.Time.IsZero() {
		o.Time = time.Now()
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	var changes []Change
	el, ok := c.byIP[o.IP]
	var e *entry
	if ok {
		e = el.Value.(*entry)
		if o.Time.Sub(e.LastSeen) > c.ttl {
			c.removeLocked(el)
			ok = false
		}
	}
	if !ok {
		e = &entry{IP: o.IP, FirstSeen: o.Time}
		el = c.order.PushBack(e)
		c.byIP[o.IP] = el
	} else {
		c.order.MoveToBack(el)
	}

	if o.MAC != nil && peerMAC(o.Source) {
		key := string(o.MAC)
		if e.MAC != nil && !bytes.Equal(e.MAC, o.MAC) {
			changes = append(changes, Change{Kind: MACChanged, IP: o.IP, OldMAC: e.MAC, NewMAC: o.MAC, LastSeen: e.MACSeen})
			if c.byMAC[string(e.MAC)] == o.IP {
				delete(c.byMAC, string(e.MAC))
			}
		}
		if prev, seen := c.byMAC[key]; seen && prev != o.IP {
			if pel, ok := c.byIP[prev]; ok {
				pe := pel.Value.(*entry)
				if bytes.Equal(pe.MAC, o.MAC) {
					kind := IPChanged
					if o.Time.Sub(pe.MACSeen) < c.sharedWindow {
						kind = MACShared
					}
					changes = append(changes, Change{Kind: kind, IP: o.IP, OldIP: prev, NewMAC: o.MAC, LastSeen: pe.MACSeen})
				}
			}
		}
		e.MAC = append(net.HardwareAddr(nil), o.MAC...)
		e.MACSeen = o.Time
		c.byMAC[key] = o.IP
	}
	if o.HasLinkAddr {
		if e.HasLinkAddr && e.LinkAddr != o.LinkAddr {
			changes = append(changes, Change{Kind: LinkAddrChanged, IP: o.IP, OldLinkAddr: e.LinkAddr, NewLinkAddr: o.LinkAddr, LastSeen: e.LastSeen})
		}
		e.LinkAddr, e.HasLinkAddr = o.LinkAddr, true
	}
	if o.CertSHA256 != "" {
		if e.Cert != "" && e.Cert != o.CertSHA256 {
			changes = append(changes, Change{Kind: CertChanged, IP: o.IP, OldCert: e.Cert, NewCert: o.CertSHA256, LastSeen: e.LastSeen})
		}
		e.Cert = o.CertSHA256
	}
	e.LastSeen = o.Time

	for c.order.Len() > c.max {
		c.removeLocked(c.order.Front())
	}
	return changes
}

func (c *Cache) removeLocked(el *list.Element) {
	e := el.Value.(*entry)
	c.order.Remove(el)
	delete(c.byIP, e.IP)
	if e.MAC != nil && c.byMAC[string(e.MAC)] == e.IP {
		delete(c.byMAC, string(e.MAC))
	}
}

// Len returns how many IPs the cache remembers.
func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}

// Save writes the cache as JSON, so a restart does not forget the baseline
// and miss a swap made while the process was down.
func (c *Cache) Save(w io.Writer) error {
	c.mu.Lock()
	out := make([]entry, 0, c.order.Len())
	for el := c.order.Front(); el != nil; el = el.Next() {
		e := *el.Value.(*entry)
		if e.MAC != nil {
			e.MACText = e.MAC.String()
		}
		out = append(out, e)
	}
	c.mu.Unlock()
	return json.NewEncoder(w).Encode(out)
}

// Load replaces the cache's contents with what Save wrote.
func (c *Cache) Load(r io.Reader) error {
	var in []entry
	if err := json.NewDecoder(r).Decode(&in); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.byIP = map[netip.Addr]*list.Element{}
	c.byMAC = map[string]netip.Addr{}
	c.order.Init()
	for i := range in {
		e := in[i]
		if e.MACText != "" {
			if m, err := net.ParseMAC(e.MACText); err == nil {
				e.MAC = m
				c.byMAC[string(m)] = e.IP
			}
		}
		e.MACText = ""
		c.byIP[e.IP] = c.order.PushBack(&e)
	}
	for c.order.Len() > c.max {
		c.removeLocked(c.order.Front())
	}
	return nil
}
