package app

import "testing"

func TestQualifierConsistency(t *testing.T) {
	tests := []struct {
		q    Qualifier
		want bool
	}{
		{0x00, true}, {0x01, true}, {0x06, true}, {0x07, true}, {0x08, true}, // no prefix
		{0x17, true}, {0x28, true}, {0x39, true}, {0x27, true}, // index prefix with a count
		{0x5B, true}, {0x4B, true}, {0x6B, true}, // size prefix, variable format
		{0x10, false}, {0x11, false}, {0x16, false}, // index prefix with a start-stop or all
		{0x0B, false},        // variable format with no size prefix
		{0x57, false},        // size prefix with a plain count
		{0x7B, false},        // reserved prefix
		{0x0A, false},        // reserved range
		{0x80 | 0x07, false}, // the reserved high bit
	}
	for _, tc := range tests {
		if got := tc.q.Consistent(); got != tc.want {
			t.Errorf("%v.Consistent() = %v, want %v", tc.q, got, tc.want)
		}
	}
}
