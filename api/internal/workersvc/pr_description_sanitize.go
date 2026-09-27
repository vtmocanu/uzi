package workersvc

import (
	"context"
	"errors"
	"html"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/secretscrub"
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
	MaxPrDescSummaryRawBytes = 4000
	MaxPrDescItemRawBytes    = 1000
	MaxPrDescListRawEntries  = 50

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
	// so an already-escaped `\[x\](y)` is not re-read as a link on a second pass.
	prDescLinkInline = regexp.MustCompile(`\[([^\]\\]*)\]\([^)]*\)`)
	prDescLinkRef    = regexp.MustCompile(`\[([^\]\\]*)\]\[[^\]]*\]`)
	// A reference definition `[label]: target`, ANYWHERE and with any whitespace (newlines
	// included) between the colon and the target, so a definition split across lines goes too.
	// Remaining brackets are escaped later, so no definition or shortcut reference can form in
	// the output either way.
	prDescLinkDef = regexp.MustCompile(`\[[^\]\\]+\]:[\s\p{Z}]*\S*`)
	// Angle-bracket autolinks become the bare URL text.
	prDescAutolink = regexp.MustCompile(`<((?i:https?|ftp)://[^\s<>]+)>`)
	// Anything tag-shaped: an element, a closing tag, a declaration or a processing instruction.
	prDescTag = regexp.MustCompile(`<[/!?]?[A-Za-z][^<>]*>`)
	// A `<` that could still open markup once a later `>` arrives (the renderer's own markers
	// follow the region), so it is dropped: a whole run of `<` in one match, so `<<<<a` costs
	// one pass, not one per `<`. A `<` before a space, digit or `=` is not tag-like and stays.
	prDescOpenAngle = regexp.MustCompile(`<+([/!?A-Za-z])`)
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
	// Group 1 is the keyword; the whole match is only a locator.
	prDescClosing = regexp.MustCompile(`(?i)\b(clos(?:e[sd]?|ing)|fix(?:e[sd]|ing)?|resolv(?:e[sd]?|ing)|implement(?:s|ed|ing)?)\b` +
		`[\s\p{Z}]*:?[\s\p{Z}]*(?:issues?[\s\p{Z}]*)?` +
		`(?:#\d+|gh-\d+|[\w.-]+(?:/[\w.-]+)*#\d+|[A-Za-z][A-Za-z0-9_]+-\d+|https?://[^\s<>()]*?/(?:issues|work_items)/\d+)`)
)

// prDescClosingMarkers are the characters a closing-directive VIEW removes or blanks: markdown
// emphasis/strikethrough/code markers, brackets, and the backslash escapes this sanitizer adds.
const prDescClosingMarkers = "*_~`[]\\"

// SanitizePrDescriptionText renders one untrusted, model- or lead-authored PR-description
// string safe to publish inside uzi's description region (PRD #1798 D7), bounded to maxBytes
// bytes. The api's result is the ONLY text the renderer may publish. Steps, in order:
//
//  1. To a fixed point (loop until nothing changes, at most prDescMaxPasses passes, each linear;
//     if that bound is hit the result is "", fail closed): HTML entities are decoded
//     to their own fixed point, control / bidi / format runes are stripped and secret shapes
//     scrubbed (ScrubUntrustedText), markup is removed (stripPrDescMarkup), and whitespace
//     (newlines included) collapses to single spaces. Running the scrub on every pass means a
//     token split by markup (`glpat-AAAA<b></b>BBBB`) is scrubbed once the markup is gone, and
//     running the decode on every pass means an entity re-formed by a removal
//     (`&<b></b>#64;`) is decoded and then handled like the character it renders as. The loop
//     only ends on a pass that changed nothing, so its result has no entity left that
//     html.UnescapeString would decode. Then the RENDERED view is checked for secrets
//     (prDescRenderedSecret): a token split by inline markdown (`*`, `~`, a code span, a
//     backslash escape) re-joins when rendered, so if that view carries a secret the whole
//     field becomes "[redacted]".
//  2. The text is cut to fit maxBytes (rune-safe, "…" marks a cut), with headroom re-measured
//     after step 3 until the final text fits.
//  3. Markdown block syntax is neutralised with backslash escapes: a leading `#`, `>`, `|`,
//     `=`, fence, list or ordered-list marker, every code-fence run of three or more backticks
//     or tildes, every `[` / `]` (no link, image, reference or definition can form), and a
//     trailing odd backslash, and a leading `/` (a GitLab quick action). Closing directives are
//     neutralised by a zero-width space after the keyword's first letter, and mentions by one
//     after the `@` (breakMentions). These run on the CUT text, so a breaker is never cut off a
//     keyword that still has its reference.
//
// Each field is meant to be one paragraph or one list item, so model text is kept from adding
// headings, quotes, lists, code blocks or sections of its own. The tests
// (pr_description_sanitize_test.go, assertPrDescInert) check every output for `<!--`, `-->`, a
// tag-shaped `<`, a live bracket and a closing directive, and check that output which was not
// cut is returned unchanged by a second pass.
func SanitizePrDescriptionText(s string, maxBytes int) string {
	s, ok := prDescNormalize(s)
	if !ok || maxBytes <= 0 {
		return ""
	}
	if prDescRenderedSecret(s) {
		s = prDescRedacted
	}
	budget := maxBytes
	for budget > 0 {
		out := finalizePrDescText(cutPrDescBytes(s, budget))
		if len(out) <= maxBytes {
			return out
		}
		budget -= len(out) - maxBytes
	}
	return ""
}

// prDescNormalize runs step 1 of SanitizePrDescriptionText to its fixed point. It reports false
// when prDescMaxPasses passes did not converge.
func prDescNormalize(s string) (string, bool) {
	for i := 0; i < prDescMaxPasses; i++ {
		prev := s
		s = decodePrDescEntities(s)
		s = ScrubUntrustedText(s)
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
		s = prDescLinkDef.ReplaceAllString(s, "")
	}
	if strings.Contains(s, "<") {
		s = prDescAutolink.ReplaceAllString(s, "$1")
		s = prDescTag.ReplaceAllString(s, "")
		s = prDescOpenAngle.ReplaceAllString(s, "$1")
		s = prDescCommentOpen.ReplaceAllString(s, "")
	}
	if strings.Contains(s, "--") {
		s = prDescCommentClose.ReplaceAllString(s, "->")
	}
	return s
}

// prDescRenderedSecret reports whether a secret appears in the text as a markdown renderer shows
// it, where the literal scrub (already applied) did not see one. s has no format rune left (the
// scrub's termsafe pass strips Cf, U+200B included). Two views: backslash escapes resolved
// (`\_` renders `_`, `\-` renders `-`), and additionally the inline emphasis, strikethrough and
// code-span markers `*`, `~` and a backtick removed (`A*B*C` renders the letters ABC side by
// side). `_` is kept in the second view: CommonMark never reads an intraword `_` as emphasis, and removing it would join
// unrelated identifiers into false secret shapes.
func prDescRenderedSecret(s string) bool {
	view := func(drop string) string {
		return strings.Map(func(r rune) rune {
			if strings.ContainsRune(drop, r) {
				return -1
			}
			return r
		}, s)
	}
	for _, v := range []string{view("\\"), view("\\*~`")} {
		if secretscrub.Scrub(v) != v {
			return true
		}
	}
	return false
}

// finalizePrDescText applies step 3: block-syntax escapes, then closing-directive and mention
// breakers.
func finalizePrDescText(s string) string {
	return breakMentions(neutralizeClosingDirectives(escapePrDescMarkdown(s)))
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
	return out, nil
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
