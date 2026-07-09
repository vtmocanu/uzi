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

// socketURLPattern matches a Socket Mode websocket URL, and ticketPattern its
// ?ticket= query credential. The connection URL Slack hands back
// (wss://…?ticket=…) is itself a credential the token patterns would miss, so
// the manager's log paths scrub it too (PRD #25 M2 hygiene).
var (
	socketURLPattern = regexp.MustCompile(`wss://[^\s"']+`)
	ticketPattern    = regexp.MustCompile(`ticket=[^&\s"']+`)
)

// Redact scrubs, for safe logging, both Slack tokens AND the Socket Mode
// connection URL / ticket. The Manager passes every error string through it
// before logging, so neither a token nor the wss ticket can ever reach a log.
func Redact(s string) string {
	s = ScrubTokens(s)
	s = socketURLPattern.ReplaceAllString(s, "wss://[redacted]")
	s = ticketPattern.ReplaceAllString(s, "ticket=[redacted]")
	return s
}
