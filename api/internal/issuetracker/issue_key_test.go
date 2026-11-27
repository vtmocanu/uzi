package issuetracker

import "testing"

func TestIssueKey(t *testing.T) {
	t.Parallel()
	cases := []struct {
		short string
		n     int64
		want  string
	}{
		{"ABC", 12, "ABC-12"},
		{"P2", 1, "P2-1"},
		{"ABC", 0, "ABC-0"},
	}
	for _, tc := range cases {
		if got := IssueKey(tc.short, tc.n); got != tc.want {
			t.Errorf("IssueKey(%q, %d) = %q, want %q", tc.short, tc.n, got, tc.want)
		}
	}
}
