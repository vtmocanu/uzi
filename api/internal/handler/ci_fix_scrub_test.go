package handler

import (
	"strings"
	"testing"
)

// The failure-snapshot log scrubber (PRD #6) must strip every known token SHAPE a
// teammate's CI log might print, before the tail is frozen onto the run — the forge
// driver's error-only, connection-PAT-only redactor does not cover a success body.
func TestScrubKnownTokensRedactsTokenFamilies(t *testing.T) {
	const secret = "abcdef0123456789ABCDEFghij" //gitleaks:allow // fake token body: the scrubber must strip it, never a real secret
	cases := map[string]string{
		"glpat":         "leaked glpat-" + secret + " here",
		"gloas":         "gloas-" + secret,
		"glrt":          "glrt-" + secret,
		"glcbt":         "glcbt-" + secret,
		"glptt":         "glptt-" + secret,
		"glsoat":        "glsoat-" + secret,
		"gldt":          "gldt-" + secret,
		"sk-ant":        "ANTHROPIC_API_KEY=sk-ant-" + secret,
		"private-token": "PRIVATE-TOKEN: " + secret,
		"authorization": "Authorization: Bearer " + secret,
		"bare-bearer":   "sent Bearer " + secret + " upstream",
	}
	for name, in := range cases {
		out := scrubKnownTokens(in)
		if strings.Contains(out, secret) {
			t.Errorf("%s: token body survived the scrub: %q", name, out)
		}
		if !strings.Contains(out, "[REDACTED]") {
			t.Errorf("%s: expected a [REDACTED] placeholder, got %q", name, out)
		}
	}

	// A header line's whole value is redacted (not just the first word after the
	// colon), so "Authorization: Bearer <token>" never leaks the token tail.
	if strings.Contains(scrubKnownTokens("Authorization: Bearer "+secret), secret) {
		t.Error("the Authorization header value must be redacted to end-of-line")
	}

	// Benign log text is untouched (no false positives on ordinary failure output).
	benign := "=== RUN TestFoo\n--- FAIL: TestFoo (nil guard removed)\nexit status 1\n"
	if got := scrubKnownTokens(benign); got != benign {
		t.Errorf("benign log must be untouched, got %q", got)
	}
}
