package slacksvc

import "testing"

func TestScrubTokens(t *testing.T) {
	cases := map[string]struct {
		in       string
		contains string // must remain
		absent   string // must be scrubbed out
	}{
		"bot token in a sentence": {
			in:       "auth failed for xoxb-123-abc-DEF while calling Slack",
			contains: "auth failed",
			absent:   "xoxb-123-abc-DEF",
		},
		"app token": {
			in:     "socket open rejected: xapp-notarealapptoken",
			absent: "xapp-notarealapptoken",
		},
		"no token is untouched": {
			in:       "invalid_auth",
			contains: "invalid_auth",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := ScrubTokens(tc.in)
			if tc.absent != "" && contains(got, tc.absent) {
				t.Errorf("token not scrubbed: %q still in %q", tc.absent, got)
			}
			if tc.contains != "" && !contains(got, tc.contains) {
				t.Errorf("scrub removed non-secret content: %q missing from %q", tc.contains, got)
			}
		})
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
