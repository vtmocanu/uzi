// Package issuedraft is the deterministic, LLM-free renderer for the "file an issue
// from a judge recommendation" flow (PRD #68). It turns rows uzi already holds
// (a recommendation + its review + the judged run) into a GitLab issue title + body,
// and it owns the write-boundary sanitizers (Decision 10) as a single Go source of
// truth so the M2 draft renderer and the M3 POST handler apply exactly the same
// controls.
//
// Trust model (PRD #46 / #68 Decision 10): rationale_md, summary_md and target are
// LLM output over an untrusted, user-controlled worker trace. Rendered naively into a
// GitLab issue they can (a) break out of a code context to inject a live link or an
// auto-loading image beacon a human GitLab viewer's browser fetches directly (camo is
// off by default on self-managed GitLab), and (b) start a column-0 "/"-line that
// GitLab executes as a quick-action (/label, /assign, /close, ...). The defenses,
// strongest first:
//
//   - FenceBlock / SafeInlineCode wrap every untrusted field in a BREAKOUT-PROOF fence
//     — a backtick delimiter STRICTLY LONGER than the longest backtick run in the
//     content — so hostile text cannot close the fence early (ingest preserves
//     backticks, so a naive ``` fence is defeated by fence-breakout). GitLab's Banzai
//     quick_action pipeline also excludes fenced blocks, so a correct fence neutralizes
//     quick-actions in the same stroke.
//   - StripUnfencedSlashLines drops every line whose first non-space character is "/"
//     OUTSIDE a fenced block — a backstop behind the fence, and the load-bearing
//     control at M3 where the client's edited body may not have preserved any fence.
//   - ScrubSecretShapes is a best-effort superset of the 4-family ingest scrub, since
//     this flow can egress text into an issue on a DIFFERENT project than the run's
//     repo. Defense-in-depth only; the human gate is the primary control.
//
// The renderer is pure (no DB, no I/O) so the security-critical shape is unit-testable.
package issuedraft

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Input is the fully-resolved, DB-free input to Render. The handler maps store rows
// into it; keeping it plain makes the renderer trivially testable and decoupled.
type Input struct {
	// Recommendation (untrusted free text except Category/Confidence, which are enums).
	Category    string
	Target      string
	RationaleMd string
	Confidence  string // "", "low", "medium", "high"

	// Review (Verdict is an enum; SummaryMd is untrusted; JudgeModel is worker-supplied
	// but capped + control-char-stripped at ingest — fenced inline defensively anyway).
	Verdict    string
	SummaryMd  string
	JudgeModel string
	ReviewDate time.Time

	// The judged run, for the Context section + the deep link.
	RunKind   string // "issue" | "ci_fix"
	RunStatus string
	RepoPath  string // path_with_namespace of the judged run's repo (forge-controlled)
	IssueIID  int64  // 0 when the run has no issue iid
	RunURL    string // server-built public URL to the judged run; "" drops the link

	// Provenance (Decision 8/10): who is filing, whose worker produced the text, and
	// the producing run. Labels only — never a forge identity.
	RequestingUser      string
	ProducingUser       string
	ProducingRunShortID string
}

// Draft is Render's output: the title + body that seed the editable draft, and the
// prominent provenance line the panel shows (Decision 8) so an admin filing another
// user's review text sees whose text it is.
type Draft struct {
	Title       string
	Description string
	Provenance  string
}

// titleMax caps the derived title. GitLab tolerates long titles, but target is capped
// at 255 bytes at ingest and the prefix is short, so this only ever trims pathological
// input.
const titleMax = 255

var categoryLabels = map[string]string{
	"enable_tool":         "Enable a tool",
	"install_worker_tool": "Install a worker tool",
	"adjust_template":     "Adjust a template",
	"improve_agent":       "Improve an agent",
	"add_agent":           "Add an agent",
	"improve_uzi":         "Improve uzi",
}

var verdictLabels = map[string]string{
	"ideal":  "Ideal",
	"ok":     "OK",
	"issues": "Issues found",
}

// CategoryLabel is the human label for a recommendation category enum, or the raw
// value if it is somehow unknown (the DB CHECK keeps it in-set).
func CategoryLabel(c string) string {
	if l, ok := categoryLabels[c]; ok {
		return l
	}
	return c
}

func verdictLabel(v string) string {
	if l, ok := verdictLabels[v]; ok {
		return l
	}
	return v
}

// Render templates the deterministic draft. Every untrusted field is fenced; the whole
// body is then run through the write-boundary pass (SanitizeFiledBody) so the M2 draft
// is already inert and byte-identical to what M3 re-derives from the same input.
func Render(in Input) Draft {
	title := SanitizeTitle(deriveTitle(in))

	var b strings.Builder
	b.WriteString("## What the judge found\n\n")
	b.WriteString(FenceBlock(in.RationaleMd))
	b.WriteString("\n## Context\n\n")

	// Recommendation line: enum label (trusted) + fenced-inline target (untrusted).
	b.WriteString("- Recommendation: **")
	b.WriteString(CategoryLabel(in.Category))
	b.WriteString("**")
	if strings.TrimSpace(in.Target) != "" {
		b.WriteString(" — ")
		b.WriteString(SafeInlineCode(in.Target))
	}
	if in.Confidence != "" {
		b.WriteString(" (")
		b.WriteString(in.Confidence)
		b.WriteString(" confidence)")
	}
	b.WriteString("\n")

	b.WriteString("- Verdict on the judged run: **")
	b.WriteString(verdictLabel(in.Verdict))
	b.WriteString("**\n")

	b.WriteString("- Judged run: ")
	b.WriteString(runReference(in))
	b.WriteString("\n")

	b.WriteString("- Retrospective by ")
	b.WriteString(SafeInlineCode(in.JudgeModel))
	if !in.ReviewDate.IsZero() {
		b.WriteString(", ")
		b.WriteString(in.ReviewDate.UTC().Format("2006-01-02"))
	}
	b.WriteString("\n\n")

	b.WriteString("## Judge's summary of the run\n\n")
	b.WriteString(FenceBlock(in.SummaryMd))

	b.WriteString("\n---\n")
	b.WriteString(footer(in))

	return Draft{
		Title:       title,
		Description: SanitizeFiledBody(b.String()),
		Provenance:  provenance(in),
	}
}

func deriveTitle(in Input) string {
	label := CategoryLabel(in.Category)
	if t := strings.TrimSpace(in.Target); t != "" {
		return label + ": " + t
	}
	return label
}

// runReference is the "Judged run" line: a deep link when a public URL is known, plain
// text otherwise. RepoPath is the forge project path (not worker free text) and IssueIID
// an integer, so the link label is safe; the URL is server-built from the public base.
func runReference(in Input) string {
	kind := in.RunKind
	if kind == "" {
		kind = "run"
	}
	// RepoPath is forge-controlled and sits in a markdown link LABEL, outside any fence
	// (audit Low-2). GitLab project paths exclude the label/destination metacharacters
	// today, but strip them anyway so a breakout never rests solely on that external
	// constraint. kind/status are trusted enums; IssueIID an integer.
	where := kind + " run on " + linkLabelSafe(in.RepoPath)
	if in.IssueIID > 0 {
		where += "#" + strconv.FormatInt(in.IssueIID, 10)
	}
	status := ""
	if in.RunStatus != "" {
		status = " (" + in.RunStatus + ")"
	}
	if in.RunURL == "" {
		return where + status
	}
	return "[" + where + "](" + in.RunURL + ")" + status
}

func footer(in Input) string {
	// RequestingUser/ProducingUser are user-controlled display handles interpolated into
	// markdown PROSE outside any fence (audit Low-2): a hostile display name could inject
	// a live link, or an "@name" that fires a real GitLab mention/ping. Fence each in
	// breakout-proof inline code so it renders as inert text and no mention resolves.
	who := "a user"
	if in.RequestingUser != "" {
		who = SafeInlineCode(in.RequestingUser)
	}
	src := "a run retrospective"
	if in.ProducingUser != "" {
		src = "a retrospective of " + SafeInlineCode(in.ProducingUser) + "'s run"
		if in.ProducingRunShortID != "" {
			src += " " + in.ProducingRunShortID
		}
	}
	return "Opened by uzi on behalf of @" + who + ", from " + src +
		". The quoted text above is LLM-authored and unverified, and is fenced " +
		"(not blockquoted) so links, image beacons and quick-actions render inert."
}

// linkLabelSafe strips the markdown link-label/destination metacharacters and collapses
// whitespace so an untrusted value cannot break out of a `[label](url)` construct.
func linkLabelSafe(s string) string {
	return collapseWS(linkUnsafe.ReplaceAllString(s, ""))
}

var linkUnsafe = regexp.MustCompile("[\\[\\]()`]")

func provenance(in Input) string {
	if in.ProducingUser == "" {
		return ""
	}
	s := "from " + in.ProducingUser + "'s worker"
	if in.ProducingRunShortID != "" {
		s += ", run " + in.ProducingRunShortID
	}
	return s
}

// ── Write-boundary sanitizers (Decision 10) — the single source of truth M3 re-runs ──

// normalizeNewlines folds "\r\n" and bare "\r" to "\n" (audit Medium-1). GitLab
// CRLF-normalizes before its markdown/quick-action processing, so every write-boundary
// control here must see the same line breaks it will — otherwise a fence closed with a
// trailing "\r" reads as unclosed here (and a following column-0 "/label" survives the
// strip) while GitLab executes it. Applied at BOTH public entry points below, so the
// whole downstream fence/strip/scan chain is CRLF-safe by construction.
func normalizeNewlines(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

// SanitizeFiledBody is the idempotent write-boundary pass over an issue body: normalize
// line endings (CRLF-safe), strip unfenced "/"-lines, then scrub secret shapes. Render
// applies it to the templated draft; the M3 POST handler re-applies it to the
// CLIENT-supplied body, which may have been edited or replaced (the "the draft was inert"
// invariant does not survive the round-trip, so it must be re-established server-side).
func SanitizeFiledBody(body string) string {
	return ScrubSecretShapes(StripUnfencedSlashLines(normalizeNewlines(body)))
}

// SanitizeTitle collapses a title to a single line, removes any leading whitespace/"/"
// so it cannot open with a quick-action character (GitLab does not process quick-actions
// in titles, but a future Forgejo driver might — Decision 10 title caveat), and scrubs
// secret shapes. It never deletes the title (unlike the line-strip), only neutralizes
// its head, so a hostile leading "/" yields a defanged title rather than an empty one.
func SanitizeTitle(s string) string {
	s = collapseWS(normalizeNewlines(s))
	s = strings.TrimLeft(s, " /")
	s = ScrubSecretShapes(s)
	if len(s) > titleMax {
		s = strings.TrimSpace(s[:titleMax])
	}
	return s
}

// FenceBlock wraps s in a fenced code block whose delimiter is a backtick run STRICTLY
// LONGER than the longest backtick run in s (min 3), so s cannot break out of the fence.
func FenceBlock(s string) string {
	delim := strings.Repeat("`", fenceLen(s))
	s = strings.TrimRight(s, "\n")
	return delim + "\n" + s + "\n" + delim + "\n"
}

// SafeInlineCode wraps s in an inline code span with a breakout-proof backtick delimiter
// and CommonMark padding, collapsing any newline/tab so the span stays on one line.
func SafeInlineCode(s string) string {
	s = collapseWS(s)
	n := longestBacktickRun(s) + 1
	if n < 1 {
		n = 1
	}
	delim := strings.Repeat("`", n)
	return delim + " " + s + " " + delim
}

// fenceLen is the fence-block delimiter length for content s: longest backtick run + 1,
// floored at the CommonMark minimum of 3.
func fenceLen(s string) int {
	n := longestBacktickRun(s) + 1
	if n < 3 {
		n = 3
	}
	return n
}

func longestBacktickRun(s string) int {
	best, cur := 0, 0
	for i := 0; i < len(s); i++ {
		if s[i] == '`' {
			cur++
			if cur > best {
				best = cur
			}
		} else {
			cur = 0
		}
	}
	return best
}

// collapseWS replaces every run of ASCII whitespace (incl. newlines/tabs) with a single
// space and trims the ends — used for one-line contexts (titles, inline code).
func collapseWS(s string) string {
	return strings.TrimSpace(wsRun.ReplaceAllString(s, " "))
}

var wsRun = regexp.MustCompile(`\s+`)

// StripUnfencedSlashLines drops every line whose first non-space character is "/" while
// NOT inside a fenced code block (Decision 3/10). Content inside a fence is kept verbatim
// (GitLab does not run quick-actions there, and deleting a fenced line would corrupt the
// displayed code). Fence tracking is a CommonMark-ish approximation: a line that is >=3
// identical fence chars (` or ~), up to 3 leading spaces, opens/closes a block; a closing
// fence must be the same char and at least as long as the opener.
func StripUnfencedSlashLines(body string) string {
	// Normalize line endings FIRST (audit Medium-1) so the fence tracker sees the same
	// line breaks GitLab does — a closing fence "```\r" must read as CLOSED, not as an
	// unclosed fence with info="\r" that keeps a following "/label" line unstripped. Also
	// applied at the SanitizeFiledBody entry; kept here too so a direct caller is safe.
	body = normalizeNewlines(body)
	lines := strings.Split(body, "\n")
	out := make([]string, 0, len(lines))
	var fenceChar byte // 0 = not in a fence
	var fenceLen int
	for _, ln := range lines {
		if fenceChar == 0 {
			if c, n, ok := fenceOpen(ln); ok {
				fenceChar, fenceLen = c, n
				out = append(out, ln)
				continue
			}
			if firstNonSpaceIsSlash(ln) {
				continue // drop an unfenced quick-action line
			}
			out = append(out, ln)
			continue
		}
		out = append(out, ln)
		if fenceCloses(ln, fenceChar, fenceLen) {
			fenceChar, fenceLen = 0, 0
		}
	}
	return strings.Join(out, "\n")
}

// leadingSpaces counts up to 3 leading spaces (CommonMark allows a fence to be indented
// by at most 3); returns -1 if there are 4+ (which would be an indented code block, not
// a fence).
func leadingSpaces(s string) int {
	n := 0
	for n < len(s) && s[n] == ' ' {
		n++
	}
	if n > 3 {
		return -1
	}
	return n
}

func fenceRun(s string) (byte, int, string) {
	sp := leadingSpaces(s)
	if sp < 0 || sp >= len(s) {
		return 0, 0, ""
	}
	c := s[sp]
	if c != '`' && c != '~' {
		return 0, 0, ""
	}
	i := sp
	for i < len(s) && s[i] == c {
		i++
	}
	return c, i - sp, strings.TrimRight(s[i:], " \t")
}

// fenceOpen reports whether ln opens a fenced block (>=3 fence chars; a backtick info
// string may not itself contain a backtick).
func fenceOpen(ln string) (byte, int, bool) {
	c, n, info := fenceRun(ln)
	if c == 0 || n < 3 {
		return 0, 0, false
	}
	if c == '`' && strings.Contains(info, "`") {
		return 0, 0, false
	}
	return c, n, true
}

// fenceCloses reports whether ln closes a block opened with openChar/openLen: same char,
// length >= opener, and nothing but the fence chars on the line.
func fenceCloses(ln string, openChar byte, openLen int) bool {
	c, n, info := fenceRun(ln)
	return c == openChar && n >= openLen && info == ""
}

func firstNonSpaceIsSlash(s string) bool {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ', '\t':
			continue
		case '/':
			return true
		default:
			return false
		}
	}
	return false
}

// secretShapes is a best-effort superset of the 4-family ingest scrub (slacksvc
// .ScrubSecrets: sk-ant-, glpat-, xox*/xapp-, uz[caw]_) plus the foreign-credential
// shapes an untrusted trace can carry into an issue on ANOTHER project (Decision 10):
// AWS keys, GitHub tokens, Google API keys, JWTs, PEM private-key blocks, DB URLs with
// an inline password, and an Authorization: Bearer header. Every pattern is prefix- or
// structure-anchored to keep the false-positive surface off git SHAs/UUIDs; this is
// defense-in-depth, NOT a standalone control (the human gate is primary).
var secretShapes = []*regexp.Regexp{
	regexp.MustCompile(`x(?:ox[bpoas]|app)-[A-Za-z0-9-]+`),                                // Slack
	regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]+`),                                           // Anthropic
	regexp.MustCompile(`glpat-[A-Za-z0-9_-]+`),                                            // GitLab PAT
	regexp.MustCompile(`uz[caw]_[A-Za-z0-9_-]{16,}`),                                      // uzi Bearer creds
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),                                                // AWS access key id
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{36,}`),                                      // GitHub token
	regexp.MustCompile(`AIza[0-9A-Za-z_-]{35}`),                                           // Google API key
	regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`),      // JWT
	regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`), // PEM
	regexp.MustCompile(`(?i)\b(?:postgres|postgresql|mysql|mongodb(?:\+srv)?|redis|amqp)://[^\s:@/]+:[^\s@/]+@`),  // DB URL creds
	regexp.MustCompile(`(?i)authorization:\s*bearer\s+[A-Za-z0-9._~+/=-]+`),               // bearer header
}

// ScrubSecretShapes replaces every recognized secret shape with "[redacted]".
func ScrubSecretShapes(s string) string {
	for _, re := range secretShapes {
		s = re.ReplaceAllString(s, "[redacted]")
	}
	return s
}
