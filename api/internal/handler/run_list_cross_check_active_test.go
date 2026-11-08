package handler

import "testing"

func TestCrossCheckActive(t *testing.T) {
	for _, tc := range []struct {
		plan, code bool
		want       string // "" is nil
	}{
		{false, false, ""},
		{true, false, "plan"},
		{false, true, "code"},
		{true, true, "plan"},
	} {
		got := crossCheckActive(tc.plan, tc.code)
		switch {
		case tc.want == "" && got != nil:
			t.Errorf("plan=%v code=%v: got %q, want nil", tc.plan, tc.code, *got)
		case tc.want != "" && (got == nil || *got != tc.want):
			t.Errorf("plan=%v code=%v: got %v, want %q", tc.plan, tc.code, got, tc.want)
		}
	}
}
