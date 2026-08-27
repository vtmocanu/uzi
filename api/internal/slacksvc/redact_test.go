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
	// uziToken is a fake uzi CLI credential (uzc_ + a ≥16-char body): the scrub must
	// strip it (PRD #64 Risk 14 — UZI_TOKEN lives in a GitLab CI variable and could
	// echo into a status/title string bound for Slack), never a real secret.
	const uziToken = "uzc_A1b2C3d4E5f6G7h8i9j0k1" //gitleaks:allow // fake uzi CLI credential fixture: the value ScrubSecrets must strip below, never a real secret
	in := "leak xoxb-bottok xapp-apptok sk-ant-not-a-real-key glpat-not-a-real-pat " + uziToken + " end"
	got := ScrubSecrets(in)
	for _, secret := range []string{"xoxb-bottok", "xapp-apptok", "sk-ant-not-a-real-key", "glpat-not-a-real-pat", uziToken} {
		if contains(got, secret) {
			t.Errorf("ScrubSecrets left %q in %q", secret, got)
		}
	}
	if !contains(got, "leak") || !contains(got, "end") {
		t.Errorf("ScrubSecrets removed non-secret content: %q", got)
	}
}

// The uzw_/uzc_/uza_ family is scrubbed for all three class prefixes, and the short
// "uzc_a1b2" display stub (a non-secret, only 4 body chars) is NOT over-matched.
func TestScrubSecretsUziTokenFamily(t *testing.T) {
	for _, prefix := range []string{"uzc_", "uza_", "uzw_"} {
		tok := prefix + "A1b2C3d4E5f6G7h8i9j0k1"
		if got := ScrubSecrets("using " + tok + " now"); contains(got, tok) {
			t.Errorf("ScrubSecrets left %q in %q", tok, got)
		}
	}
	// A display stub (uzc_ + 4 chars) is not a secret and must survive untouched.
	if got := ScrubSecrets("token uzc_a1b2 in list"); !contains(got, "uzc_a1b2") {
		t.Errorf("ScrubSecrets over-matched the short display stub: %q", got)
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
