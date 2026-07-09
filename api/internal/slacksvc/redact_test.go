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

func TestScrubSecretsCoversAllFamilies(t *testing.T) {
	in := "leak xoxb-bottok xapp-apptok sk-ant-not-a-real-key glpat-not-a-real-pat end"
	got := ScrubSecrets(in)
	for _, secret := range []string{"xoxb-bottok", "xapp-apptok", "sk-ant-not-a-real-key", "glpat-not-a-real-pat"} {
		if contains(got, secret) {
			t.Errorf("ScrubSecrets left %q in %q", secret, got)
		}
	}
	if !contains(got, "leak") || !contains(got, "end") {
		t.Errorf("ScrubSecrets removed non-secret content: %q", got)
	}
}

func TestRedactScrubsSocketURLAndTicket(t *testing.T) {
	in := `connect failed to wss://wss-primary.slack.com/link/?ticket=SECRETTICKET123&app_id=A0 for xoxb-tok`
	got := Redact(in)
	for _, secret := range []string{"SECRETTICKET123", "wss-primary.slack.com", "xoxb-tok"} {
		if contains(got, secret) {
			t.Errorf("Redact left %q in %q", secret, got)
		}
	}
	// A plain message with no credentials is unchanged.
	if got := Redact("connection_error: timeout"); got != "connection_error: timeout" {
		t.Errorf("Redact altered a clean string: %q", got)
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
