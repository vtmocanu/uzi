package slacksvc

import (
	"regexp"

	"github.com/slack-go/slack/slackutilsx"

	"github.com/vtmocanu/uzi/api/internal/secretscrub"
)

// EscapeMrkdwn neutralizes the Slack mrkdwn control characters (& < >) in an
// UNTRUSTED dynamic field before it is interpolated into a message that also
// carries trusted <url|label> deep-link markup. Without it a forge- or
// worker-controlled value (an issue title, repo path, failure reason, or a linked
// account label) could smuggle a clickable <https://phishing|Open in uzi> link or
// a <@Uxxx> mention into the trusted bot DM. Apply it to each dynamic field
// individually — NEVER to the whole rendered string, which would also break the
// intended deep-link markup. It is orthogonal to ScrubSecrets (which redacts
// credential patterns, not markup); both run on outbound text.
func EscapeMrkdwn(s string) string {
	return slackutilsx.EscapeMessage(s)
}

// The secret-value patterns moved to internal/secretscrub in PRD #88 M1, unchanged.
// They had to become reachable from workersvc.SubmitInput, which scrubs a
// clarification ANSWER before persisting it (D-G) — and slacksvc is deliberately kept
// out of the core service's import graph, so the shared thing moved down rather than
// the importer reaching sideways. ScrubTokens/ScrubSecrets stay here as the names
// every Slack call site already uses.

// ScrubTokens replaces any Slack token substring with a placeholder. slack-go's
// validation errors return Slack's error code (e.g. "invalid_auth"), not the
// token, so this is defense-in-depth: even if a token ever reached an error
// string, it would not surface to the admin or a log.
func ScrubTokens(s string) string {
	return secretscrub.ScrubSlackTokens(s)
}

// ScrubSecrets is the widened outbound scrub (PRD #25 M2, extended PRD #64 M5):
// every secret family uzi handles — Slack tokens, the per-user Anthropic key, a
// GitLab PAT, and uzi's own uzw_/uzc_/uza_ Bearer credentials. Everything sent to
// Slack passes through it as a last line of defense so a credential that somehow
// reached a status/title string never leaves the box.
func ScrubSecrets(s string) string {
	return secretscrub.Scrub(s)
}

// socketURLPattern matches a Socket Mode websocket URL, and ticketPattern its
// ?ticket= query credential. The connection URL Slack hands back
// (wss://…?ticket=…) is itself a credential the token patterns would miss, so
// the manager's log paths scrub it too (PRD #25 M2 hygiene).
var (
	socketURLPattern = regexp.MustCompile(`wss://[^\s"']+`)
	ticketPattern    = regexp.MustCompile(`ticket=[^&\s"']+`)
)

// Redact scrubs, for safe logging, every secret family AND the Socket Mode
// connection URL / ticket. The Manager passes every error string through it
// before logging, so neither a token, an Anthropic/GitLab credential, nor the
// wss ticket can ever reach a log.
func Redact(s string) string {
	s = ScrubSecrets(s)
	s = socketURLPattern.ReplaceAllString(s, "wss://[redacted]")
	s = ticketPattern.ReplaceAllString(s, "ticket=[redacted]")
	return s
}
