package secmon

import (
	"testing"
	"time"
)

func TestDupWindow(t *testing.T) {
	d := NewDupWindow(2, time.Minute)
	now := time.Unix(1000, 0)
	a, b, c := [32]byte{1}, [32]byte{2}, [32]byte{3}
	if _, dup := d.Seen(a, 1, now); dup {
		t.Fatal("first sighting reported")
	}
	if _, dup := d.Seen(a, 1, now); dup {
		t.Fatal("same connection reported")
	}
	if prev, dup := d.Seen(a, 2, now); !dup || prev != 1 {
		t.Fatalf("other connection: prev %d dup %t", prev, dup)
	}
	if _, dup := d.Seen(a, 3, now.Add(2*time.Minute)); dup {
		t.Fatal("expired entry reported")
	}
	d.Seen(b, 1, now.Add(2*time.Minute))
	d.Seen(c, 1, now.Add(2*time.Minute))
	if d.Len() != 2 {
		t.Fatalf("len %d, want bound 2", d.Len())
	}
	if _, dup := d.Seen(a, 9, now.Add(2*time.Minute)); dup {
		t.Fatal("evicted entry reported")
	}
}
