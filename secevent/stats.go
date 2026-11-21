package secevent

import "maps"

// Stats counts what a dispatcher saw. It is a snapshot, safe to keep.
type Stats struct {
	// ByKind and ByCategory count every event submitted, including the
	// repeats folded into aggregates.
	ByKind     map[Kind]uint64
	ByCategory map[Category]uint64
	// Delivered counts events handed to the sink, Aggregated the repeats
	// folded into another event, and Dropped the events lost to a full queue.
	Delivered  uint64
	Aggregated uint64
	Dropped    uint64
	SinkPanics uint64
}

func newStats() Stats {
	return Stats{ByKind: map[Kind]uint64{}, ByCategory: map[Category]uint64{}}
}

func (s *Stats) count(e Event) {
	s.ByKind[e.Kind]++
	s.ByCategory[e.Category]++
}

// Stats returns a snapshot of the dispatcher's counters.
func (d *Dispatcher) Stats() Stats {
	if d == nil {
		return newStats()
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	out := d.stats
	out.ByKind = maps.Clone(d.stats.ByKind)
	out.ByCategory = maps.Clone(d.stats.ByCategory)
	out.Dropped += d.dropped.Load()
	return out
}
