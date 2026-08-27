package workersvc

import "testing"

// TestRecommendsDeniedExecutable pins the deterministic net's matcher (issue #167): a target
// naming a denylisted credential-bearing CLI is caught even in a mixed free-text token list
// ("file, glab") and even in path form ("/usr/local/bin/gh"), while a clean tool is not.
func TestRecommendsDeniedExecutable(t *testing.T) {
	cases := []struct {
		target string
		want   bool
	}{
		{"glab", true},
		{"file, glab", true},
		{"/usr/local/bin/gh", true},
		{"aws", true},
		{"az", true},
		{"file", false},
		{"", false},
		{"ripgrep, jq", false},
	}
	for _, tc := range cases {
		if got := recommendsDeniedExecutable(tc.target); got != tc.want {
			t.Errorf("recommendsDeniedExecutable(%q) = %v, want %v", tc.target, got, tc.want)
		}
	}
}
