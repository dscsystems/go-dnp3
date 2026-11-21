package secmon

import (
	"container/list"
	"time"
)

// DupWindow remembers content digests for a while, and which connection
// last sent each, so content replayed from another connection is noticed.
// It is bounded: past max entries the oldest is forgotten. Not safe for
// concurrent use.
type DupWindow struct {
	window time.Duration
	max    int
	order  *list.List // of *dupEntry, oldest first
	byKey  map[[32]byte]*list.Element
}

type dupEntry struct {
	key  [32]byte
	conn uint64
	at   time.Time
}

// NewDupWindow returns a window remembering up to max digests for window.
func NewDupWindow(max int, window time.Duration) *DupWindow {
	return &DupWindow{window: window, max: max, order: list.New(), byKey: make(map[[32]byte]*list.Element)}
}

// Seen records key as sent on conn at now. It returns the connection that
// sent the same key earlier within the window, when that was another one.
func (d *DupWindow) Seen(key [32]byte, conn uint64, now time.Time) (prev uint64, dup bool) {
	for e := d.order.Front(); e != nil; e = d.order.Front() {
		v := e.Value.(*dupEntry)
		if now.Sub(v.at) <= d.window {
			break
		}
		d.order.Remove(e)
		delete(d.byKey, v.key)
	}
	if e, ok := d.byKey[key]; ok {
		v := e.Value.(*dupEntry)
		prev, dup = v.conn, v.conn != conn
		v.conn, v.at = conn, now
		d.order.MoveToBack(e)
		return prev, dup
	}
	if d.order.Len() >= d.max {
		old := d.order.Front()
		d.order.Remove(old)
		delete(d.byKey, old.Value.(*dupEntry).key)
	}
	d.byKey[key] = d.order.PushBack(&dupEntry{key: key, conn: conn, at: now})
	return 0, false
}

// Len returns how many digests are remembered.
func (d *DupWindow) Len() int { return d.order.Len() }
