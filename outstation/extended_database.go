package outstation

import (
	"time"

	"github.com/dscsystems/go-dnp3"
)

// FrozenAnalog returns an analog snapshot independent of the running input.
func (db *Database) FrozenAnalog(index uint16) (dnp3.Analog, PointConfig, bool) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return get(db.frozenAnalog, index)
}

// UpdateFrozenAnalog stores a snapshot and applies its own event deadband.
func (db *Database) UpdateFrozenAnalog(index uint16, v dnp3.Analog) {
	db.mu.Lock()
	defer db.mu.Unlock()
	if int(index) < len(db.frozenAnalog) {
		db.setFrozenAnalog(index, v)
	}
}

func (db *Database) setFrozenAnalog(index uint16, v dnp3.Analog) {
	p := &db.frozenAnalog[index]
	flagsChanged := p.value.Flags != v.Flags
	p.value = v
	if p.shouldReport(v.Value, flagsChanged) {
		db.raise(p.cfg, Event{Type: dnp3.TypeFrozenAnalog, Index: index,
			Variation: p.cfg.EventVariation, FrozenAnalog: v, Time: v.Time})
	}
}

// FreezeAnalogs snapshots every analog that has a frozen counterpart.
func (db *Database) FreezeAnalogs() {
	db.freezeAnalogsRange(0, 0xffff, dnp3.Unsynchronized(time.Now()), false)
}

func (db *Database) freezeAnalogsRange(start, stop uint16, at dnp3.Timestamp, clear bool) {
	db.mu.Lock()
	defer db.mu.Unlock()
	for i := int(start); i <= int(stop) && i < min(len(db.analog), len(db.frozenAnalog)); i++ {
		v := db.analog[i].value
		v.Time = at
		db.setFrozenAnalog(uint16(i), v)
		if clear {
			p := &db.analog[i]
			v.Value = 0
			p.value = v
			if p.shouldReport(0, false) {
				db.raise(p.cfg, Event{Type: dnp3.TypeAnalog,
					Index: uint16(i), Variation: p.cfg.EventVariation, Analog: v, Time: at})
			}
		}
	}
}

// TimeAndInterval returns an indexed schedule value.
func (db *Database) TimeAndInterval(index uint16) (dnp3.TimeAndInterval, bool) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if int(index) >= len(db.timeAndInterval) {
		return dnp3.TimeAndInterval{}, false
	}
	return db.timeAndInterval[index], true
}

// UpdateTimeAndInterval stores a schedule value; these values generate no events.
func (db *Database) UpdateTimeAndInterval(index uint16, v dnp3.TimeAndInterval) bool {
	db.mu.Lock()
	defer db.mu.Unlock()
	if int(index) >= len(db.timeAndInterval) {
		return false
	}
	db.timeAndInterval[index] = v
	return true
}

// UpdateVirtualTerminal queues new terminal input as a group 113 event.
func (db *Database) UpdateVirtualTerminal(index uint16, v []byte) {
	db.mu.Lock()
	defer db.mu.Unlock()
	if int(index) >= len(db.terminal) || len(v) == 0 || len(v) > 255 {
		return
	}
	p := &db.terminal[index]
	p.value = append(dnp3.OctetString(nil), v...)
	db.raise(p.cfg, Event{Type: dnp3.TypeVirtualTerminal, Index: index,
		Variation: uint8(len(v)), OctetString: p.value})
}
