package slacksvc

import (
	"regexp"

	"github.com/slack-go/slack/slackutilsx"
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

// Secret-value patterns uzi must never let reach a log or Slack. slackToken
// covers xoxb- bot / xapp- app-level tokens (plus the other xox* families);
// anthropicKey and gitlabPAT cover the credentials that ride run/agent context
// so the outbound scrub is defense-in-depth against any of them leaking into a
// Slack message (PRD #25: sk-ant-, glpat-, xoxb-, xapp-).
var (
	slackTokenPattern = regexp.MustCompile(`x(?:ox[bpoas]|app)-[A-Za-z0-9-]+`)
	anthropicPattern  = regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]+`)
	gitlabPATPattern  = regexp.MustCompile(`glpat-[A-Za-z0-9_-]+`)
	// uziTokenPattern covers uzi's own Bearer credentials — uzw_ (worker join token),
	// uzc_ (user CLI token) and uza_ (admin_ro CLI token) (PRD #64 Risk 14). The CLI
	// PRD tells users to put UZI_TOKEN in a GitLab CI variable, which creates the
	// echo-into-a-trace path this scrub defends: a uzc_/uza_ minted here must never
	// survive into an outbound Slack message. The {16,} body avoids matching the short
	// "uzc_a1b2" display prefix (only 4 body chars), which is not a secret.
	uziTokenPattern = regexp.MustCompile(`uz[caw]_[A-Za-z0-9_-]{16,}`)
)

// ScrubTokens replaces any Slack token substring with a placeholder. slack-go's
// validation errors return Slack's error code (e.g. "invalid_auth"), not the
// token, so this is defense-in-depth: even if a token ever reached an error
// string, it would not surface to the admin or a log.
func ScrubTokens(s string) string {
	return slackTokenPattern.ReplaceAllString(s, "[redacted]")
}

// ScrubSecrets is the widened outbound scrub (PRD #25 M2, extended PRD #64 M5):
// every secret family uzi handles — Slack tokens, the per-user Anthropic key, a
// GitLab PAT, and uzi's own uzw_/uzc_/uza_ Bearer credentials. Everything sent to
// Slack passes through it as a last line of defense so a credential that somehow
// reached a status/title string never leaves the box.
func ScrubSecrets(s string) string {
	s = slackTokenPattern.ReplaceAllString(s, "[redacted]")
	s = anthropicPattern.ReplaceAllString(s, "[redacted]")
	s = gitlabPATPattern.ReplaceAllString(s, "[redacted]")
	s = uziTokenPattern.ReplaceAllString(s, "[redacted]")
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
