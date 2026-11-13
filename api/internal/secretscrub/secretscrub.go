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

import (
	"regexp"
	"sort"
	"strings"
)

// The credential families uzi handles. Kept close to slacksvc's originals so the
// extraction is behaviour-preserving, then widened (PRD #954 M1, change A):
//
//   - slackToken covers xoxb- bot / xapp- app-level tokens (plus the other xox*
//     families); slackRefresh covers xoxe- refresh tokens, which fall OUTSIDE the
//     x(?:ox[bpoas]|app)- shape;
//   - anthropic covers sk-ant- keys;
//   - history-only openai covers classic sk- keys and project / service-account keys. Its
//     16-character minimum applies to the body AFTER any proj- / svcacct- prefix,
//     preserving short display prefixes. Classic bodies are alphanumeric; scoped
//     bodies also admit URL-safe '_' and '-'. No upper bound: redact the whole
//     credential rather than leave a suffix when provider lengths change;
//   - gitlabPAT covers the GitLab 9-family set (glpat-/gloas-/glrt-/glcbt-/glptt-/
//     glsoat-/glimt-/glagent-/gldt-), matching the snapshot list — widened from
//     glpat- alone to close a live outbound-Slack scrub gap;
//   - githubClassic covers GitHub classic PATs (ghp_/gho_/ghu_/ghs_/ghr_) and
//     githubFineGrained the fine-grained github_pat_ family — the GitHub driver has
//     been live since 2026-08-08 and both lists previously missed it;
//   - uziToken covers uzi's own Bearer credentials — uzw_ (worker join token), uzc_
//     (user CLI token), uza_ (admin_ro CLI token) (PRD #64 Risk 14) and uzp_ (product
//     token, PRD #1907), uzf_ (per-run fetch credential, #2035), and uzs_ (OAuth
//     client secret, PRD #1910). The class letter r is reserved for uzr_ OAuth
//     refresh tokens (PRD #1910 M3), scrubbed from the start so a token minted
//     later is never briefly unscrubbed.
//
// The {16,} body on the anchored families avoids matching the short "uzc_a1b2" /
// "ghp_a1b2" display prefixes, which are not secrets.
var (
	slackTokenPattern      = regexp.MustCompile(`x(?:ox[bpoas]|app)-[A-Za-z0-9-]+`)
	slackRefreshPattern    = regexp.MustCompile(`xoxe-[A-Za-z0-9-]+`)
	anthropicPattern       = regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]+`)
	openaiPattern          = regexp.MustCompile(`sk-(?:(?:proj|svcacct)-[A-Za-z0-9_-]{16,}|[A-Za-z0-9]{16,})`)
	gitlabPATPattern       = regexp.MustCompile(`gl(pat|oas|rt|cbt|ptt|soat|imt|agent|dt)-[A-Za-z0-9_-]{16,}`)
	githubClassicPattern   = regexp.MustCompile(`gh[pousr]_[A-Za-z0-9_]{16,}`)
	githubFineGrainPattern = regexp.MustCompile(`github_pat_[A-Za-z0-9_]{16,}`)
	uziTokenPattern        = regexp.MustCompile(`uz[capfrsw]_[A-Za-z0-9_-]{16,}`)
)

// scrubPatterns is Scrub's pattern set. Order no longer matters: RedactOverlapping
// matches every family on the original input.
var scrubPatterns = []*regexp.Regexp{
	slackTokenPattern, slackRefreshPattern, anthropicPattern, gitlabPATPattern,
	githubClassicPattern, githubFineGrainPattern, uziTokenPattern,
}

// historyPatterns is ScrubHistory's set: Scrub's families plus the history-only
// OpenAI pattern.
var historyPatterns = []*regexp.Regexp{
	slackTokenPattern, slackRefreshPattern, anthropicPattern, openaiPattern,
	gitlabPATPattern, githubClassicPattern, githubFineGrainPattern, uziTokenPattern,
}

// Scrub replaces every recognised secret family with a placeholder.
func Scrub(s string) string {
	return RedactOverlapping(s, scrubPatterns, "[redacted]")
}

// ScrubHistory applies conservative, overlap-safe credential redaction only to
// history displays. It deliberately recognizes credential shapes within text;
// callers scrub the complete normalized value before applying display bounds.
func ScrubHistory(s string) string {
	return RedactOverlapping(s, historyPatterns, "[redacted]")
}

// overlapWindow bounds how far back RedactOverlapping rescans after a match to
// find another match of the same pattern starting inside it. It must be at least
// the longest such overlap any registered pattern allows: a prefix character
// outside its body class (sk- classic: 2 bytes; issuedraft gh[pousr]_ with an
// alphanumeric body: 3) or a fixed-length body (AKIA: 19, AIza: 38). A pattern whose
// overlap is unbounded must instead be written so its greedy match covers any
// match starting inside it (the issuedraft JWT shape consumes trailing segments).
const overlapWindow = 64

// RedactOverlapping replaces every span any pattern matches in the ORIGINAL s with
// placeholder, merging overlapping spans into one placeholder. Applying patterns
// one after another is unsafe: a family matched inside another credential's body
// (a uzp_ body containing ghs_<16 chars>) is replaced first, and the shortened
// remainder of the outer credential no longer matches its own pattern, so its
// tail survives. Every multi-pattern credential scrubber in this package and its
// callers goes through here. Patterns must not match the empty string.
//
// Work is linear in len(s) times overlapWindow: after each match the scan resumes
// at most overlapWindow bytes before its end, never at its start, so a body of
// repeated prefixes ("uzc_uzc_...") cannot make it quadratic.
func RedactOverlapping(s string, patterns []*regexp.Regexp, placeholder string) string {
	var matches [][]int
	for _, pattern := range patterns {
		for offset := 0; offset < len(s); {
			match := pattern.FindStringIndex(s[offset:])
			if match == nil {
				break
			}
			match[0] += offset
			match[1] += offset
			matches = append(matches, match)
			offset = max(match[0]+1, match[1]-overlapWindow)
		}
	}
	if len(matches) == 0 {
		return s
	}
	sort.Slice(matches, func(i, j int) bool {
		return matches[i][0] < matches[j][0]
	})

	var result strings.Builder
	previousEnd := 0
	start, end := matches[0][0], matches[0][1]
	for _, match := range matches[1:] {
		if match[0] < end {
			if match[1] > end {
				end = match[1]
			}
			continue
		}
		result.WriteString(s[previousEnd:start])
		result.WriteString(placeholder)
		previousEnd = end
		start, end = match[0], match[1]
	}
	result.WriteString(s[previousEnd:start])
	result.WriteString(placeholder)
	result.WriteString(s[end:])
	return result.String()
}

// ScrubSlackTokens replaces only Slack token substrings — the bot/app/user families
// AND the xoxe- refresh family, which falls outside the x(ox|app)- shape. slack-go's
// validation errors return Slack's error code (e.g. "invalid_auth"), not the token, so
// this is defense-in-depth for error strings rather than a primary control.
func ScrubSlackTokens(s string) string {
	return RedactOverlapping(s, slackPatterns, "[redacted]")
}

var slackPatterns = []*regexp.Regexp{slackTokenPattern, slackRefreshPattern}
