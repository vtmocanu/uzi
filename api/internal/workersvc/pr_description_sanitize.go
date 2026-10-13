package workersvc

import (
	"context"
	"errors"
	"html"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/secretscrub"
	"github.com/vtmocanu/uzi/api/internal/termsafe"
)

// PR-description field limits (PRD #1798 D7 / target layout). Two kinds of bound:
//
//   - RAW caps bound what a worker may send at all. Over them the request is refused (400):
//     the worker mirrors the layout limits before posting, so an over-cap body is a bug or an
//     attack, never a legitimate description.
//   - LAYOUT caps bound what is published, in BYTES of the final sanitized text (after every
//     inserted U+200B breaker and backslash escape). A longer text is cut at a rune boundary
//     and ends with "…" inside the cap; a list longer than its layout cap keeps its first
//     entries. The worker mirrors these numbers (see /tmp/plan1798/m4a-shapes.md).
const (
	MaxPrDescSummaryRawBytes      = 4000
	MaxPrDescItemRawBytes         = 1000
	MaxPrDescListRawEntries       = 50
	MaxPrDescDiagramRawEntries    = 50
	MaxPrDescDiagramRawLabelBytes = 1000

	PrDescSummaryMaxBytes       = 600
	PrDescItemMaxBytes          = 200
	PrDescMaxChanges            = 5
	PrDescMaxScopeNotes         = 8
	PrDescMaxReviewPointers     = 2
	PrDescMaxVerification       = 20
	PrDescVerifyCommandMaxBytes = 200
)

// ErrPrDescriptionInvalid is a malformed stage request (an unknown enum, an over-cap raw field,
// a bad sha, a missing snapshot value) → 400.
var ErrPrDescriptionInvalid = errors.New("pr description is invalid")

var validPrDescScopeKinds = map[string]bool{"added": true, "changed": true, "dropped": true, "deferred": true}

var prDescHexSHA = regexp.MustCompile(`^[0-9a-fA-F]{7,64}$`)

// zeroWidthSpace is the inert breaker the sanitizer inserts after `@` and inside a closing
// keyword. It is a Cf rune: a consumer that strips Cf (termsafe) re-forms the original text,
// so the agent renderer must publish the api-returned fields WITHOUT a Cf strip.
const zeroWidthSpace = "\u200B"

const prDescEllipsis = "…"

// prDescRedacted is the text a field item is replaced with, whole, when its rendered view
// carries a secret the literal scrub missed (the placeholder secretscrub itself writes).
const prDescRedacted = "[redacted]"

// prDescMaxPasses bounds every fixed-point loop in the sanitizer: the outer normalization loop,
// the entity decode and the comment removal. Each pass is linear in the text, so a field costs at
// most a constant number of linear passes; input that has not converged within the bound
// (adversarially deep nesting or entity encoding, never real prose) sanitizes to "".
const prDescMaxPasses = 8

var (
	// HTML comments, including an unterminated opener that runs to the end of the text.
	prDescHTMLComment      = regexp.MustCompile(`(?s)<!--.*?-->`)
	prDescUnterminatedCmnt = regexp.MustCompile(`(?s)<!--.*$`)
	// Markdown images in every form: inline `![alt](target)`, full `![alt][ref]`, collapsed
	// `![alt][]` and shortcut `![alt]`. The whole image goes, alt text included.
	prDescImage = regexp.MustCompile(`!\[[^\]]*\](?:\([^)]*\)|\[[^\]]*\])?`)
	// Markdown links keep their text and lose their target (inline, full and collapsed).
	// A label holding a backslash is left for the bracket escape (it neutralises it either way),
	// so an already-escaped `\[x\](y)` is not re-read as a link on a second pass. A reference
	// DEFINITION (`[label]: target`) is left as prose: every remaining `[` / `]` is escaped in
	// step 3, so neither a definition nor a reference to one can form in the output.
	prDescLinkInline = regexp.MustCompile(`\[([^\]\\]*)\]\([^)]*\)`)
	prDescLinkRef    = regexp.MustCompile(`\[([^\]\\]*)\]\[[^\]]*\]`)
	// Angle-bracket autolinks become the bare URL text.
	prDescAutolink = regexp.MustCompile(`<((?i:https?|ftp)://[^\s<>]+)>`)
	// A complete, well-formed HTML open or closing tag whose name is a known HTML element, in
	// CommonMark's raw-HTML attribute grammar. Removing it is only for readability (`<b>x</b>`
	// reads `x`): every `<` left after this is encoded as `&lt;` in step 3, so no tag, comment,
	// declaration or processing instruction of any name can survive. Text that is not such a tag
	// (`Map<string, int>`, `Vec<T>`, `a<b && c>d`) keeps its `<` and so stays readable.
	prDescHTMLTag = regexp.MustCompile(`<(?:` + prDescHTMLNames + `)(?:` + prDescHTMLAttr + `)*[\s\p{Z}]*/?>` +
		`|</(?:` + prDescHTMLNames + `)[\s\p{Z}]*>`)
	// Comment delimiters that survived everything above (`a ----> b`, `--!>`) fold to `->`.
	prDescCommentClose = regexp.MustCompile(`-{2,}!?>`)
	prDescCommentOpen  = regexp.MustCompile(`<!-{2,}`)
	prDescSpaces       = regexp.MustCompile(`[\s\p{Z}]+`)

	// Code-fence runs anywhere (the renderer may place a field at the start of a line).
	prDescFenceRun = regexp.MustCompile("`{3,}|~{3,}")
	// A leading ordered-list marker `12.` / `12)`: group 1 is the delimiter to escape.
	prDescLeadingOrdered = regexp.MustCompile(`^\d{1,9}([.)])`)

	// Closing directives, the GitLab-superset pattern (approver point 4) widened to err on the
	// side of neutralising: every GitLab default keyword form (a superset of GitHub's and
	// Forgejo's), case-insensitive, then any run of ASCII or Unicode spaces with an optional
	// colon (none needed: `Fixes:#12`), an optional `issue`/`issues`, then a same-project,
	// cross-project, external-tracker or URL reference. It runs over views of the text with the
	// emphasis, code, bracket and escape markers removed or blanked (see
	// neutralizeClosingDirectives), so `_Fixes_ #12`, `Fix**es** #12` and `Fixes [#12]` match.
	// Group 1 is the keyword; the whole match is only a locator. A reference is `#N` or `!N` (Forgejo
	// reads `[#!]N` and `owner/repo[#!]N` after a close keyword, and its issues and pull requests share
	// one numbering, so `Fixes !7` closes issue 7), with or without a path.
	prDescClosing = regexp.MustCompile(`(?i)\b(clos(?:e[sd]?|ing)|fix(?:e[sd]|ing)?|resolv(?:e[sd]?|ing)|implement(?:s|ed|ing)?)\b` +
		`[\s\p{Z}]*:?[\s\p{Z}]*(?:issues?[\s\p{Z}]*)?` +
		`(?:[#!]\d+|gh-\d+|[\w.-]+(?:/[\w.-]+)*[#!]\d+|[A-Za-z][A-Za-z0-9_]+-\d+|https?://[^\s<>()]*?/(?:issues|work_items)/\d+)`)
)

// prDescHTMLNames are the HTML element names prDescHTMLTag removes, lowercase only, so a
// PascalCase generic (`Promise<Data>`, `List<Map>`, `Vec<U>`) is not read as an element. A tag
// in any other case (`<DIV>`) or with a name outside the list is not removed, only encoded as
// `&lt;`, which is equally inert.
const prDescHTMLNames = `(?:abbr|address|area|article|aside|audio|base|bdi|bdo|big|blockquote|body|br|button|` +
	`canvas|caption|center|cite|code|col|colgroup|data|datalist|dd|del|details|dfn|dialog|dir|div|dl|dt|em|` +
	`embed|fieldset|figcaption|figure|font|footer|form|frame|frameset|h[1-6]|head|header|hgroup|hr|html|` +
	`iframe|img|input|ins|kbd|label|legend|li|link|main|map|mark|marquee|math|menu|meta|meter|nav|noscript|` +
	`object|ol|optgroup|option|output|param|picture|pre|progress|rp|rt|ruby|samp|script|section|select|` +
	`slot|small|source|span|strike|strong|style|sub|summary|sup|svg|table|tbody|td|template|textarea|` +
	`tfoot|th|thead|time|title|tr|track|tt|ul|var|video|wbr)|[abipqsu]`

// prDescHTMLAttr is one CommonMark raw-HTML attribute: whitespace, a name, and an optional
// unquoted, single-quoted or double-quoted value.
const prDescHTMLAttr = `[\s\p{Z}]+[A-Za-z_:][A-Za-z0-9_.:-]*` +
	"(?:[\\s\\p{Z}]*=[\\s\\p{Z}]*(?:[^\\s\\p{Z}\"'=<>`]+|'[^']*'|\"[^\"]*\"))?"

// prDescClosingMarkers are the characters a closing-directive VIEW removes or blanks: markdown
// emphasis/strikethrough/code markers, brackets, and the backslash escapes this sanitizer adds.
const prDescClosingMarkers = "*_~`[]\\"

// SanitizePrDescriptionText renders one untrusted, model- or lead-authored PR-description
// string safe to publish inside uzi's description region (PRD #1798 D7), bounded to maxBytes
// bytes. The api's result is the ONLY text the renderer may publish. Steps, in order:
//
//  1. To a fixed point (loop until nothing changes, at most prDescMaxPasses passes, each linear;
//     if that bound is hit the result is "", fail closed): HTML entities are decoded to their
//     own fixed point, control / bidi / format runes are stripped, markup is removed
//     (stripPrDescMarkup), and whitespace (newlines included) collapses to single spaces.
//     Running the decode on every pass means an entity re-formed by a removal (`&<b></b>#64;`)
//     is decoded and then handled like the character it renders as. The loop only ends on a
//     pass that changed nothing, so its result has no entity left that html.UnescapeString
//     would decode. Then, on that converged text, the RENDERED views are checked for secrets
//     (prDescRenderedSecret): a token split by inline markdown (`*`, `~`, a code span, a
//     boundary `_`, a backslash escape) re-joins when rendered, so if a view shows a secret the
//     literal scrub would not fully hide, the whole field becomes "[redacted]". Otherwise the
//     literal secret shapes are scrubbed (secretscrub.Scrub). The scrub runs after the markup
//     is gone, so a token split by markup (`glpat-AAAA<b></b>BBBB`) is scrubbed whole.
//  2. The text is cut to fit maxBytes (rune-safe, "…" marks a cut). Since step 3 grows the
//     text, a binary search over the cut length finds a large cut whose step-3 result fits
//     maxBytes; the growth is not monotonic in the cut length, so the kept cut may land a few
//     bytes short of the largest one that fits.
//  3. Markdown block syntax is neutralised with backslash escapes: a leading `#`, `>`, `|`,
//     `=`, fence, list or ordered-list marker, every code-fence run of three or more backticks
//     or tildes, every `[` / `]` (no link, image, reference or definition can form), and a
//     trailing odd backslash, and a leading `/` (a GitLab quick action). Every `<` is encoded
//     as `&lt;`, so no HTML tag, comment, declaration or autolink can form (the renderer
//     publishes the text as markdown, which shows `&lt;` as `<`). Closing directives are
//     neutralised by a zero-width space after the keyword's first letter, and mentions by one
//     after the `@` (breakMentions). These run on the CUT text, so a breaker is never cut off a
//     keyword that still has its reference.
//
// Each field is meant to be one paragraph or one list item, so model text is kept from adding
// headings, quotes, lists, code blocks or sections of its own. The tests
// (pr_description_sanitize_test.go, assertPrDescInert) check every output for `<!--`, `-->`, a
// raw `<`, a live bracket and a closing directive, and check that output which was not cut is
// returned unchanged by a second pass.
func SanitizePrDescriptionText(s string, maxBytes int) string {
	s, ok := prDescNormalize(s)
	if !ok || maxBytes <= 0 {
		return ""
	}
	if prDescRenderedSecret(s) {
		s = prDescRedacted
	} else {
		s = secretscrub.Scrub(s)
	}
	if out := finalizePrDescText(cutPrDescBytes(s, maxBytes)); len(out) <= maxBytes {
		return out
	}
	// Step 3 grows the text (escapes, breakers, `&lt;`), so find the largest cut that still fits.
	// A zero budget cuts to "", which always fits.
	best := ""
	for lo, hi := 1, maxBytes-1; lo <= hi; {
		mid := lo + (hi-lo)/2
		if out := finalizePrDescText(cutPrDescBytes(s, mid)); len(out) <= maxBytes {
			best, lo = out, mid+1
		} else {
			hi = mid - 1
		}
	}
	return best
}

// prDescNormalize runs the fixed-point loop of step 1 of SanitizePrDescriptionText (everything
// but the secret checks). It reports false when prDescMaxPasses passes did not converge.
func prDescNormalize(s string) (string, bool) {
	for i := 0; i < prDescMaxPasses; i++ {
		prev := s
		s = decodePrDescEntities(s)
		s = termsafe.SanitizeBounded(s, 3*len(s)+1)
		s = stripPrDescMarkup(s)
		s = strings.TrimSpace(prDescSpaces.ReplaceAllString(s, " "))
		if s == prev {
			return s, true
		}
	}
	return "", false
}

// decodePrDescEntities decodes HTML entities until the text stops changing, within
// prDescMaxPasses passes (the caller's outer loop repeats it if the bound ever cut it short, and
// fails closed if that loop does not converge either).
func decodePrDescEntities(s string) string {
	for i := 0; i < prDescMaxPasses && strings.Contains(s, "&"); i++ {
		u := html.UnescapeString(s)
		if u == s {
			return s
		}
		s = u
	}
	return s
}

// stripPrDescMarkup removes HTML and markdown link/image syntax, one pass of each rule; the
// caller loops to the fixed point (a removal can expose a new instance of an earlier rule:
// `<!<!---->--` becomes `<!--`).
func stripPrDescMarkup(s string) string {
	// Each rule runs only when the text holds a substring every one of its matches needs, so a
	// field pays only for the markup it carries.
	if strings.Contains(s, "<!--") {
		// Terminated comments first, to their own fixed point (within prDescMaxPasses passes), so
		// a comment exposed by removing another (`<!<!---->-- x -->`) is removed as a comment
		// rather than read as unterminated. Past the bound, the unterminated rule below removes
		// from the first surviving `<!--` to the end: more text goes, never less.
		for i := 0; i < prDescMaxPasses; i++ {
			next := prDescHTMLComment.ReplaceAllString(s, "")
			if next == s {
				break
			}
			s = next
		}
		s = prDescUnterminatedCmnt.ReplaceAllString(s, "")
	}
	if strings.Contains(s, "[") {
		s = prDescImage.ReplaceAllString(s, "")
		s = prDescLinkInline.ReplaceAllString(s, "$1")
		s = prDescLinkRef.ReplaceAllString(s, "$1")
	}
	if strings.Contains(s, "<") {
		s = prDescAutolink.ReplaceAllString(s, "$1")
		s = prDescHTMLTag.ReplaceAllString(s, "")
		s = prDescCommentOpen.ReplaceAllString(s, "")
	}
	if strings.Contains(s, "--") {
		s = prDescCommentClose.ReplaceAllString(s, "->")
	}
	return s
}

// prDescRenderedSecret reports whether a markdown renderer would show secret material the
// literal scrub does not hide. s is the converged step-1 text, NOT yet scrubbed, with no format
// rune left (U+200B included). Three rendered views:
//
//   - backslash escapes resolved (`\_` renders `_`, `\-` renders `-`);
//   - additionally the emphasis, strikethrough and code-span markers `*`, `~` and a backtick
//     removed (`A*B*C` renders the letters ABC side by side);
//   - additionally every run of `_` that is NOT intraword removed (`_glpat_-x` renders
//     `glpat-x` with an emphasised `glpat`). An intraword run (a letter or digit on both sides)
//     is kept: CommonMark never reads it as emphasis, and removing it would join ordinary
//     identifiers (`max_app-config`) into false secret shapes. The second view keeps every `_`,
//     so the families whose prefix holds one (`ghp_`, `github_pat_`) are matched there.
//
// A view reveals a secret when scrubbing the view differs from the view of the scrubbed text:
// either the view forms a secret s does not hold literally (`_glpat_-…`), or the literal scrub
// hides only part of one the view shows whole (`xoxb-1234-_5678_` scrubs to `[redacted]_5678_`,
// which renders the token's tail). Two more views model GitLab only (prDescGitLabView): its
// inline-diff markers and inline-math `$` removed, then the second and third views' removals.
// The check can also fire on an item that already holds a
// literal secret next to a marker; the whole item is then redacted, which only hides more.
func prDescRenderedSecret(s string) bool {
	scrubbed := secretscrub.Scrub(s)
	for _, view := range []func(string) string{
		func(t string) string { return prDescDropRunes(t, "\\") },
		func(t string) string { return prDescDropRunes(t, "\\*~`") },
		func(t string) string { return prDescDropBoundaryUnderscores(prDescDropRunes(t, "\\*~`")) },
		prDescGitLabView,
		func(t string) string { return prDescDropBoundaryUnderscores(prDescGitLabView(t)) },
	} {
		vs := view(scrubbed)
		if secretscrub.Scrub(vs) != vs || secretscrub.Scrub(view(s)) != vs {
			return true
		}
	}
	return false
}

// prDescInlineDiffMarkers removes the markers of GitLab's inline-diff filter
// (lib/banzai/filter/inline_diff_filter.rb): `{+ … +}`, `{- … -}`, `[+ … +]` and `[- … -]` render
// their content with no marker and need no word boundary, so `A{+B+}C` renders ABC. Every marker
// is removed whether or not it is paired, which can only reveal more.
var prDescInlineDiffMarkers = strings.NewReplacer("{+", "", "+}", "", "[+", "", "+]", "", "{-", "", "-}", "", "[-", "", "-]", "")

// prDescGitLabView is the rendered view of a GitLab description: backslash escapes resolved (they
// are decoded before the inline-diff filter, so `\[+x+\]` is a marker pair), `$` removed (inline
// math renders its content), the inline-diff markers removed, then the emphasis, strikethrough and
// code-span markers removed.
func prDescGitLabView(s string) string {
	return prDescDropRunes(prDescInlineDiffMarkers.Replace(prDescDropRunes(s, "\\$")), "*~`")
}

// prDescDropRunes returns s without any rune in drop.
func prDescDropRunes(s, drop string) string {
	return strings.Map(func(r rune) rune {
		if strings.ContainsRune(drop, r) {
			return -1
		}
		return r
	}, s)
}

// prDescDropBoundaryUnderscores removes every run of `_` that does not have a letter or digit
// on both sides (the runs CommonMark may read as emphasis).
func prDescDropBoundaryUnderscores(s string) string {
	if !strings.Contains(s, "_") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] != '_' {
			b.WriteByte(s[i])
			i++
			continue
		}
		j := i
		for j < len(s) && s[j] == '_' {
			j++
		}
		before, _ := utf8.DecodeLastRuneInString(s[:i])
		after, _ := utf8.DecodeRuneInString(s[j:])
		if i > 0 && j < len(s) && prDescWordRune(before) && prDescWordRune(after) {
			b.WriteString(s[i:j])
		}
		i = j
	}
	return b.String()
}

func prDescWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

// finalizePrDescText applies step 3: block-syntax escapes, `<` encoded as `&lt;`, then
// closing-directive and mention breakers. The encoding is idempotent under re-sanitization:
// step 1 decodes `&lt;` back to the same `<`, which is removed or kept exactly as it was the
// first time and then encoded again.
func finalizePrDescText(s string) string {
	s = strings.ReplaceAll(escapePrDescMarkdown(s), "<", "&lt;")
	return breakMentions(neutralizeClosingDirectives(s))
}

// cutPrDescBytes bounds s to maxBytes bytes at a rune boundary, ending a cut with "…" inside
// the bound (trailing space before the ellipsis is dropped).
func cutPrDescBytes(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	keep := maxBytes - len(prDescEllipsis)
	if keep <= 0 {
		return ""
	}
	for keep > 0 && !utf8.RuneStart(s[keep]) {
		keep--
	}
	return strings.TrimSpace(s[:keep]) + prDescEllipsis
}

// escapePrDescMarkdown backslash-escapes the markdown syntax a one-line field could still use:
// a leading block-opening token, every code-fence run, and every bracket. An escape is added
// only where the character is not already escaped (an even run of backslashes precedes it),
// which keeps the step idempotent and never turns `\[` into an escaped backslash followed by
// a live bracket. A trailing odd backslash is doubled so it cannot escape what the renderer
// writes after the field.
func escapePrDescMarkdown(s string) string {
	if s == "" {
		return s
	}
	esc := make([]bool, len(s))
	for _, loc := range prDescFenceRun.FindAllStringIndex(s, -1) {
		for i := loc[0]; i < loc[1]; i++ {
			esc[i] = true
		}
	}
	for i := 0; i < len(s); i++ {
		if s[i] == '[' || s[i] == ']' {
			esc[i] = true
		}
	}
	if m := prDescLeadingOrdered.FindStringSubmatchIndex(s); m != nil {
		esc[m[2]] = true
	} else if prDescLeadingBlockToken(s) {
		esc[0] = true
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	backslashes := 0
	for i := 0; i < len(s); i++ {
		if esc[i] && backslashes%2 == 0 {
			b.WriteByte('\\')
		}
		b.WriteByte(s[i])
		if s[i] == '\\' {
			backslashes++
		} else {
			backslashes = 0
		}
	}
	if backslashes%2 == 1 {
		b.WriteByte('\\')
	}
	return b.String()
}

// prDescLeadingBlockToken reports whether s opens with a character that starts a markdown block
// at the start of a line: an ATX heading, a block quote, a table row, a setext underline, a
// `-`/`+` list marker or thematic break, or a `*`/`_` list marker or thematic break (a leading
// `**bold**` is left alone), or a `/` that GitLab reads as a quick action (`/close`, `/merge`).
// A leading code fence is a fence run, escaped wherever it appears. Only the field's first
// character needs this: whitespace (newlines included) collapses to single spaces in step 1, so
// no other character of a field starts a line; the renderer places each field at a line start.
func prDescLeadingBlockToken(s string) bool {
	switch s[0] {
	case '#', '>', '|', '=', '-', '+', '/':
		return true
	case '*', '_':
		if len(s) == 1 || s[1] == ' ' {
			return true
		}
		return strings.Trim(s, string(s[0])+" ") == "" // a thematic break `***` / `_ _ _`
	}
	return false
}

// neutralizeClosingDirectives inserts a zero-width space after the first letter of every closing
// keyword that is followed by an issue reference, so no forge reads it as a directive. The
// pattern runs over s itself and two views of it: one with the markers in prDescClosingMarkers REMOVED
// (`Fix**es** #12` reads `Fixes #12`) and one with them BLANKED to spaces (`a_Fixes #12` reads
// `a Fixes #12`); a keyword found in either view is broken in s.
func neutralizeClosingDirectives(s string) string {
	removed := make([]byte, 0, len(s))
	idx := make([]int, 0, len(s))
	blanked := []byte(s)
	for i := 0; i < len(s); i++ {
		if strings.IndexByte(prDescClosingMarkers, s[i]) >= 0 {
			blanked[i] = ' '
			continue
		}
		removed = append(removed, s[i])
		idx = append(idx, i)
	}
	at := map[int]bool{}
	for _, loc := range prDescClosing.FindAllStringSubmatchIndex(s, -1) {
		at[loc[2]] = true
	}
	for _, loc := range prDescClosing.FindAllSubmatchIndex(removed, -1) {
		at[idx[loc[2]]] = true
	}
	for _, loc := range prDescClosing.FindAllSubmatchIndex(blanked, -1) {
		at[loc[2]] = true
	}
	if len(at) == 0 {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		_, size := utf8.DecodeRuneInString(s[i:])
		b.WriteString(s[i : i+size])
		if at[i] {
			b.WriteString(zeroWidthSpace)
		}
		i += size
	}
	return b.String()
}

// breakMentions inserts a zero-width space after every `@` that is not preceded by an ASCII
// letter or digit, whatever follows it (`_@alice_`, `@\_alice`, `@.bob`), except an `@` that
// ends the text or is followed by a space. The step is idempotent because step 1 strips every
// breaker (a Cf rune) before this re-inserts them. An email (`a_b@x.com`, `first.last@example.org`) keeps its `@` as written: a
// letter or digit precedes it, and no forge reads that as a mention.
func breakMentions(s string) string {
	if !strings.Contains(s, "@") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for i := 0; i < len(s); i++ {
		b.WriteByte(s[i])
		if s[i] != '@' || (i > 0 && isASCIIAlnum(s[i-1])) {
			continue
		}
		rest := s[i+1:]
		if rest == "" || rest[0] == ' ' {
			continue
		}
		b.WriteString(zeroWidthSpace)
	}
	return b.String()
}

func isASCIIAlnum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// SanitizePrDescriptionFields validates the raw fields a worker posts and returns the sanitized,
// layout-bounded fields that are the only publishable text (PRD #1798 D7). A raw value over its
// cap, an unknown scope-note kind, a verification result other than pass|fail, or a
// verified_at_sha that is not 7-64 hex characters is ErrPrDescriptionInvalid. Entries that are
// empty after sanitization are dropped; lists longer than their layout cap keep their first
// entries. The returned slices are never nil. ctx is checked between fields: a cancelled request
// stops sanitizing and returns ctx.Err().
func SanitizePrDescriptionFields(ctx context.Context, in apitypes.PrDescriptionFields) (apitypes.PrDescriptionFields, error) {
	return sanitizePrDescriptionFields(ctx, in, nil)
}

// sanitizePrDescriptionFields reports only a fixed rejection reason, never graph text.
func sanitizePrDescriptionFields(ctx context.Context, in apitypes.PrDescriptionFields, rejected func(string)) (apitypes.PrDescriptionFields, error) {
	out := apitypes.PrDescriptionFields{
		Changes:        []string{},
		ScopeNotes:     []apitypes.PrDescriptionScopeNote{},
		ReviewPointers: []string{},
		Verification:   []apitypes.PrDescriptionVerification{},
	}
	if len(in.Summary) > MaxPrDescSummaryRawBytes {
		return out, ErrPrDescriptionInvalid
	}
	if len(in.Changes) > MaxPrDescListRawEntries || len(in.ScopeNotes) > MaxPrDescListRawEntries ||
		len(in.ReviewPointers) > MaxPrDescListRawEntries || len(in.Verification) > MaxPrDescListRawEntries {
		return out, ErrPrDescriptionInvalid
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	out.Summary = SanitizePrDescriptionText(in.Summary, PrDescSummaryMaxBytes)

	var err error
	if out.Changes, err = sanitizePrDescList(ctx, in.Changes, PrDescMaxChanges); err != nil {
		return out, err
	}
	if out.ReviewPointers, err = sanitizePrDescList(ctx, in.ReviewPointers, PrDescMaxReviewPointers); err != nil {
		return out, err
	}
	for _, n := range in.ScopeNotes {
		if !validPrDescScopeKinds[n.Kind] || len(n.Text) > MaxPrDescItemRawBytes {
			return out, ErrPrDescriptionInvalid
		}
		if err := ctx.Err(); err != nil {
			return out, err
		}
		text := SanitizePrDescriptionText(n.Text, PrDescItemMaxBytes)
		if text == "" || len(out.ScopeNotes) >= PrDescMaxScopeNotes {
			continue
		}
		out.ScopeNotes = append(out.ScopeNotes, apitypes.PrDescriptionScopeNote{Kind: n.Kind, Text: text})
	}
	for _, v := range in.Verification {
		if (v.Result != "pass" && v.Result != "fail") || len(v.Command) > MaxPrDescItemRawBytes ||
			!prDescHexSHA.MatchString(v.VerifiedAtSha) {
			return out, ErrPrDescriptionInvalid
		}
		if err := ctx.Err(); err != nil {
			return out, err
		}
		cmd := SanitizePrDescriptionText(v.Command, PrDescVerifyCommandMaxBytes)
		if cmd == "" || len(out.Verification) >= PrDescMaxVerification {
			continue
		}
		out.Verification = append(out.Verification, apitypes.PrDescriptionVerification{
			Command: cmd, Result: v.Result, VerifiedAtSha: strings.ToLower(v.VerifiedAtSha),
		})
	}
	if in.Diagram != nil {
		if len(in.Diagram.Nodes) > MaxPrDescDiagramRawEntries || len(in.Diagram.Edges) > MaxPrDescDiagramRawEntries || len(in.Diagram.Title) > MaxPrDescDiagramRawLabelBytes {
			return out, ErrPrDescriptionInvalid
		}
		for _, n := range in.Diagram.Nodes {
			if len(n.Key) > MaxPrDescDiagramRawLabelBytes || len(n.Label) > MaxPrDescDiagramRawLabelBytes {
				return out, ErrPrDescriptionInvalid
			}
		}
		for _, e := range in.Diagram.Edges {
			if len(e.From) > MaxPrDescDiagramRawLabelBytes || len(e.To) > MaxPrDescDiagramRawLabelBytes || len(e.Label) > MaxPrDescDiagramRawLabelBytes {
				return out, ErrPrDescriptionInvalid
			}
		}
		if err := ctx.Err(); err != nil {
			return out, err
		}
		var reason string
		out.Diagram, reason = sanitizePrDescDiagram(in.Diagram)
		if reason != "" && rejected != nil {
			rejected(reason)
		}
	}
	return out, nil
}

var prDescDiagramKey = regexp.MustCompile(`^[a-z0-9_]{1,16}$`)

// sanitizePrDescDiagram drops an invalid graph as a unit. Every label is checked before and
// after the final allowlist, since punctuation replacement can expose a directive.
func sanitizePrDescDiagram(in *apitypes.PrDescriptionDiagram) (*apitypes.PrDescriptionDiagram, string) {
	if in.Kind != "flow" && in.Kind != "sequence" {
		return nil, "kind"
	}
	minNodes := 3
	if in.Kind == "sequence" {
		minNodes = 2
	}
	if len(in.Nodes) < minNodes || len(in.Nodes) > 12 || len(in.Edges) < 2 || len(in.Edges) > 20 {
		return nil, "entries"
	}
	label := func(raw string, cap int) (string, bool) {
		if len(raw) > MaxPrDescDiagramRawLabelBytes {
			return "", false
		}
		s, ok := prDescNormalize(raw)
		if !ok || prDescRenderedSecret(s) || secretscrub.Scrub(s) != s || prDescClosing.MatchString(s) || neutralizeClosingDirectives(s) != s || breakMentions(s) != s {
			return "", false
		}
		var b strings.Builder
		for _, r := range s {
			if unicode.IsLetter(r) || unicode.IsDigit(r) || r == ' ' || strings.ContainsRune(".,-_/+'()", r) {
				b.WriteRune(r)
			} else {
				b.WriteByte(' ')
			}
		}
		s = strings.Join(strings.Fields(b.String()), " ")
		if len(s) > cap {
			for len(s) > cap {
				_, size := utf8.DecodeLastRuneInString(s)
				s = s[:len(s)-size]
			}
			s = strings.TrimSpace(s)
		}
		if len(s) > cap || s == "" || prDescRenderedSecret(s) || secretscrub.Scrub(s) != s || neutralizeClosingDirectives(s) != s || breakMentions(s) != s {
			return "", false
		}
		return s, true
	}
	title := ""
	if in.Title != "" {
		var ok bool
		title, ok = label(in.Title, 80)
		if !ok {
			return nil, "title"
		}
	}
	out := &apitypes.PrDescriptionDiagram{Kind: in.Kind, Title: title, Nodes: make([]apitypes.PrDescriptionDiagramNode, 0, len(in.Nodes)), Edges: make([]apitypes.PrDescriptionDiagramEdge, 0, len(in.Edges))}
	keys := make(map[string]bool, len(in.Nodes))
	for _, n := range in.Nodes {
		if !prDescDiagramKey.MatchString(n.Key) || keys[n.Key] {
			return nil, "node key"
		}
		v, ok := label(n.Label, 60)
		if !ok {
			return nil, "node label"
		}
		keys[n.Key] = true
		out.Nodes = append(out.Nodes, apitypes.PrDescriptionDiagramNode{Key: n.Key, Label: v})
	}
	for _, e := range in.Edges {
		if !keys[e.From] || !keys[e.To] || (in.Kind == "flow" && e.From == e.To) {
			return nil, "edge endpoint"
		}
		v := ""
		if e.Label != "" {
			var ok bool
			v, ok = label(e.Label, 60)
			if !ok {
				return nil, "edge label"
			}
		}
		out.Edges = append(out.Edges, apitypes.PrDescriptionDiagramEdge{From: e.From, To: e.To, Label: v})
	}
	return out, ""
}

func sanitizePrDescList(ctx context.Context, items []string, maxEntries int) ([]string, error) {
	out := []string{}
	for _, it := range items {
		if len(it) > MaxPrDescItemRawBytes {
			return out, ErrPrDescriptionInvalid
		}
		if err := ctx.Err(); err != nil {
			return out, err
		}
		text := SanitizePrDescriptionText(it, PrDescItemMaxBytes)
		if text == "" || len(out) >= maxEntries {
			continue
		}
		out = append(out, text)
	}
	return out, nil
}
