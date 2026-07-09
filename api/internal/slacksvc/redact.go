package slacksvc

import "regexp"

// Secret-value patterns uzi must never let reach a log or Slack. slackToken
// covers xoxb- bot / xapp- app-level tokens (plus the other xox* families);
// anthropicKey and gitlabPAT cover the credentials that ride run/agent context
// so the outbound scrub is defense-in-depth against any of them leaking into a
// Slack message (PRD #25: sk-ant-, glpat-, xoxb-, xapp-).
var (
	slackTokenPattern = regexp.MustCompile(`x(?:ox[bpoas]|app)-[A-Za-z0-9-]+`)
	anthropicPattern  = regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]+`)
	gitlabPATPattern  = regexp.MustCompile(`glpat-[A-Za-z0-9_-]+`)
)

// ScrubTokens replaces any Slack token substring with a placeholder. slack-go's
// validation errors return Slack's error code (e.g. "invalid_auth"), not the
// token, so this is defense-in-depth: even if a token ever reached an error
// string, it would not surface to the admin or a log.
func ScrubTokens(s string) string {
	return slackTokenPattern.ReplaceAllString(s, "[redacted]")
}

// ScrubSecrets is the widened outbound scrub (PRD #25 M2): every secret family
// uzi handles — Slack tokens, the per-user Anthropic key, and a GitLab PAT.
// Everything sent to Slack passes through it as a last line of defense so a
// credential that somehow reached a status/title string never leaves the box.
func ScrubSecrets(s string) string {
	s = slackTokenPattern.ReplaceAllString(s, "[redacted]")
	s = anthropicPattern.ReplaceAllString(s, "[redacted]")
	s = gitlabPATPattern.ReplaceAllString(s, "[redacted]")
	return s
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
