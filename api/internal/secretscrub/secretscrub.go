// Package secretscrub holds the canonical outbound secret-redaction patterns.
//
// It exists because the scrub is needed on BOTH sides of a deliberate one-way
// dependency. slacksvc has always owned it (PRD #25 M2, widened by PRD #64 M5) for
// outbound Slack text, and slacksvc is kept free of a workersvc import on purpose —
// the run-lifecycle core does not know about delivery adapters, and main wires the
// two together through thin adapters. PRD #88 M1 (D-G) needs the same scrub in
// workersvc.SubmitInput, because a clarification ANSWER is user text that is
// persisted, replayed into agent context, and mirrored to Slack, and the web and CLI
// paths applied no scrub at all. Having workersvc import slacksvc to reach one regex
// would invert that boundary and drag the Slack client into the core service, so the
// patterns live here and slacksvc delegates.
//
// This is redaction of KNOWN credential shapes, not a general sanitiser: it is a last
// line of defense, never a licence to route a secret through a surface.
package secretscrub

import "regexp"

// The credential families uzi handles. Kept verbatim from slacksvc's originals so the
// extraction is behaviour-preserving:
//
//   - slackToken covers xoxb- bot / xapp- app-level tokens (plus the other xox*
//     families);
//   - anthropic and gitlabPAT cover the credentials that ride run/agent context;
//   - uziToken covers uzi's own Bearer credentials — uzw_ (worker join token), uzc_
//     (user CLI token) and uza_ (admin_ro CLI token) (PRD #64 Risk 14). The {16,} body
//     avoids matching the short "uzc_a1b2" display prefix, which is not a secret.
var (
	slackTokenPattern = regexp.MustCompile(`x(?:ox[bpoas]|app)-[A-Za-z0-9-]+`)
	anthropicPattern  = regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]+`)
	gitlabPATPattern  = regexp.MustCompile(`glpat-[A-Za-z0-9_-]+`)
	uziTokenPattern   = regexp.MustCompile(`uz[caw]_[A-Za-z0-9_-]{16,}`)
)

// Scrub replaces every recognised secret family with a placeholder.
func Scrub(s string) string {
	s = slackTokenPattern.ReplaceAllString(s, "[redacted]")
	s = anthropicPattern.ReplaceAllString(s, "[redacted]")
	s = gitlabPATPattern.ReplaceAllString(s, "[redacted]")
	s = uziTokenPattern.ReplaceAllString(s, "[redacted]")
	return s
}

// ScrubSlackTokens replaces only Slack token substrings. slack-go's validation errors
// return Slack's error code (e.g. "invalid_auth"), not the token, so this is
// defense-in-depth for error strings rather than a primary control.
func ScrubSlackTokens(s string) string {
	return slackTokenPattern.ReplaceAllString(s, "[redacted]")
}
