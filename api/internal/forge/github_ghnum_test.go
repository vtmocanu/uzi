package forge

import (
	"math"
	"testing"
)

// TestGhNum pins the narrowing guard the go-github client's int issue/PR-number
// argument forces on uzi's int64 IIDs (CodeQL go/incorrect-integer-conversion).
// A real issue/PR number narrows unchanged; a value past int32 range or negative
// errors instead of silently truncating on a hypothetical 32-bit build.
func TestGhNum(t *testing.T) {
	for _, tc := range []struct {
		name    string
		in      int64
		want    int
		wantErr bool
	}{
		{"typical issue number", 4172, 4172, false},
		{"one", 1, 1, false},
		{"max int32 boundary", math.MaxInt32, math.MaxInt32, false},
		{"just over int32", math.MaxInt32 + 1, 0, true},
		{"negative", -1, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ghNum(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ghNum(%d) = %d, nil; want out-of-range error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ghNum(%d) unexpected error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("ghNum(%d) = %d; want %d", tc.in, got, tc.want)
			}
		})
	}
}
