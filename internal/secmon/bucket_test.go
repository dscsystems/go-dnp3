package secmon

import (
	"testing"
	"time"
)

func TestBucket(t *testing.T) {
	b := NewBucket(3, time.Second)
	now := time.Now()
	for i := range 3 {
		if !b.Allow(now) {
			t.Fatalf("burst token %d refused", i)
		}
	}
	if b.Allow(now) {
		t.Fatal("a fourth token in the same instant was allowed")
	}
	if !b.Allow(now.Add(400 * time.Millisecond)) {
		t.Fatal("a token did not refill after a third of the period")
	}
	var zero Bucket
	if !zero.Allow(now) || zero.Valid() {
		t.Fatal("the zero bucket should allow everything and not be valid")
	}
}

func TestKeyedIsBounded(t *testing.T) {
	k := NewKeyed[int](1, time.Hour, 3)
	now := time.Now()
	for i := range 10 {
		if !k.Allow(i, now) {
			t.Fatalf("key %d refused its first token", i)
		}
	}
	if k.Len() != 3 {
		t.Fatalf("len = %d", k.Len())
	}
	if k.Allow(9, now) {
		t.Fatal("key 9 got a second token")
	}
}
