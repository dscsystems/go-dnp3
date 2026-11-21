// Package secmon holds the small detectors security reporting is built from:
// token buckets, keyed buckets and distinct-value windows. They are bounded:
// hostile input cannot grow any of them without limit.
package secmon

import (
	"container/list"
	"time"
)

// Bucket is a token bucket allowing n events per period, with bursts up to n.
// The zero Bucket allows nothing; build one with NewBucket. It is not safe for
// concurrent use.
type Bucket struct {
	tokens float64
	max    float64
	perSec float64
	last   time.Time
}

// NewBucket returns a full bucket allowing n events per period.
func NewBucket(n int, per time.Duration) Bucket {
	if n <= 0 || per <= 0 {
		return Bucket{}
	}
	return Bucket{tokens: float64(n), max: float64(n), perSec: float64(n) / per.Seconds()}
}

// Valid reports whether the bucket was built with a positive rate.
func (b *Bucket) Valid() bool { return b.max > 0 }

// Allow takes a token, reporting false when none is left.
func (b *Bucket) Allow(now time.Time) bool {
	if b.max == 0 {
		return true
	}
	if !b.last.IsZero() {
		b.tokens = min(b.max, b.tokens+now.Sub(b.last).Seconds()*b.perSec)
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// AllowN takes n tokens at once, reporting false — and taking none — when
// fewer are left.
func (b *Bucket) AllowN(now time.Time, n int) bool {
	if b.max == 0 {
		return true
	}
	if !b.last.IsZero() {
		b.tokens = min(b.max, b.tokens+now.Sub(b.last).Seconds()*b.perSec)
	}
	b.last = now
	if b.tokens < float64(n) {
		b.tokens = 0
		return false
	}
	b.tokens -= float64(n)
	return true
}

// Keyed holds one bucket per key, at most max of them; the least recently
// used is forgotten to make room. It is not safe for concurrent use.
type Keyed[K comparable] struct {
	n     int
	per   time.Duration
	max   int
	m     map[K]*list.Element
	order *list.List
}

type keyedEntry[K comparable] struct {
	key K
	b   Bucket
}

// NewKeyed returns per-key buckets of n events per period.
func NewKeyed[K comparable](n int, per time.Duration, max int) *Keyed[K] {
	if max <= 0 {
		max = 4096
	}
	return &Keyed[K]{n: n, per: per, max: max, m: map[K]*list.Element{}, order: list.New()}
}

// Allow takes a token from key's bucket.
func (k *Keyed[K]) Allow(key K, now time.Time) bool {
	el, ok := k.m[key]
	if !ok {
		el = k.order.PushBack(&keyedEntry[K]{key: key, b: NewBucket(k.n, k.per)})
		k.m[key] = el
		for k.order.Len() > k.max {
			old := k.order.Remove(k.order.Front()).(*keyedEntry[K])
			delete(k.m, old.key)
		}
	} else {
		k.order.MoveToBack(el)
	}
	return el.Value.(*keyedEntry[K]).b.Allow(now)
}

// Len returns how many keys are held.
func (k *Keyed[K]) Len() int { return k.order.Len() }
