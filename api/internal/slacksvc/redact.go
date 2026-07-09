package slacksvc

import "regexp"

// secretPattern matches the Slack token shapes uzi handles: xoxb- bot tokens and
// xapp- app-level tokens. It is deliberately narrow (M1 only needs to guarantee a
// save-time validation error never echoes the submitted token); the M2 redactor
// widens the pattern set for log/error scrubbing and the outbound scrub pass.
var secretPattern = regexp.MustCompile(`x(?:ox[bpoas]|app)-[A-Za-z0-9-]+`)

// ScrubTokens replaces any Slack token substring with a placeholder. slack-go's
// validation errors return Slack's error code (e.g. "invalid_auth"), not the
// token, so this is defense-in-depth: even if a token ever reached an error
// string, it would not surface to the admin or a log.
func ScrubTokens(s string) string {
	return secretPattern.ReplaceAllString(s, "[redacted]")
}
