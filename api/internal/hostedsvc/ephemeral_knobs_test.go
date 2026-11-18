package hostedsvc

import (
	"math"
	"testing"
)

// The settings accessor honours any non-negative env value, so the int32 narrowing for
// the trigger queries must saturate instead of wrapping negative (PR #4421 review).
func TestClampMaxPerUser(t *testing.T) {
	cases := []struct {
		in   int
		want int32
	}{
		{-1, 0},
		{0, 0},
		{5, 5},
		{math.MaxInt32, math.MaxInt32},
		{math.MaxInt32 + 1, math.MaxInt32},
		{1 << 40, math.MaxInt32},
	}
	for _, c := range cases {
		if got := clampMaxPerUser(c.in); got != c.want {
			t.Errorf("clampMaxPerUser(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}
