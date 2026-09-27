package workersvc

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// zw is the zero-width breaker the sanitizer inserts, spelled out so an expectation shows it.
const zw = "\u200B"

// closingProbe is the GitLab-superset closing pattern, used here only as an oracle: it must find
// NOTHING in any sanitized output. It is written independently of prDescClosing (plain \s, no
// emphasis tolerance) so a regression in the production pattern is not masked by sharing it.
var closingProbe = regexp.MustCompile(`(?i)\b(clos(e[sd]?|ing)|fix(e[sd]|ing)?|resolv(e[sd]?|ing)|implement(s|ed|ing)?)\b:?\s+(issues?\s+)?(#\d+|[\w.-]+(/[\w.-]+)*#\d+|https?://\S*/(issues|work_items)/\d+)`)

func TestSanitizePrDescriptionTextMarkup(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"plain text untouched", "Adds a retry to the uploader.", "Adds a retry to the uploader."},
		{"raw html tags stripped", "Adds <b>bold</b> and <script>alert(1)</script> text", "Adds bold and alert(1) text"},
		{"self-closing and attrs", `x <img src="a.png" onerror="y"/> z`, "x z"},
		{"html comment removed", "before <!-- hidden --> after", "before after"},
		{"forged uzi start marker", "<!-- uzi:description:start v1 -->Hello", "Hello"},
		{"forged uzi end marker", "Hello <!-- uzi:completion:end -->", "Hello"},
		{"review bot marker", "ok <!-- This is an auto-generated comment: summarize by coderabbit.ai -->", "ok"},
		{"unterminated comment runs to end", "keep this <!-- uzi:description:end v1 and the rest", "keep this"},
		{"nested comment trick", "a <!<!---->-- b --> c", "a c"},
		{"stray close arrow folded", "a --> b", "a -> b"},
		{"entity encoded comment", "a &lt;!-- uzi:completion:start v1 --&gt; b", "a b"},
		{"double encoded tag", "a &amp;lt;script&amp;gt;x b", "a x b"},
		{"unterminated tag opener dropped", "value <div class=x and more", "value div class=x and more"},
		{"comparison kept", "keeps a < b and b > a", "keeps a < b and b > a"},
		{"inline image removed", "see ![diagram](https://x.test/a.png) here", "see here"},
		{"reference image removed", "see ![diagram][d1] here", "see here"},
		{"inline link keeps text", "read [the docs](https://evil.test/x) now", "read the docs now"},
		{"reference link keeps text", "read [the docs][1] now", "read the docs now"},
		{"link definition dropped", "text\n[1]: https://evil.test", "text"},
		{"autolink becomes bare url", "see <https://example.com/a?b=1> ok", "see https://example.com/a?b=1 ok"},
		{"bare url kept", "see https://example.com/page", "see https://example.com/page"},
		{"newlines collapse", "one\n\n## Heading\n- two", "one ## Heading - two"},
		{"bidi and control stripped", "a\u202Eb\x1b[31mc", "ab\\[31mc"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := SanitizePrDescriptionText(c.in, 600)
			if got != c.want {
				t.Fatalf("SanitizePrDescriptionText(%q)\n got: %q\nwant: %q", c.in, got, c.want)
			}
			assertPrDescInert(t, got)
		})
	}
}

func TestSanitizePrDescriptionTextMentions(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"leading mention", "@alice please look", "@" + zw + "alice please look"},
		{"mid-sentence mention", "thanks @bob-smith for this", "thanks @" + zw + "bob-smith for this"},
		{"group mention", "cc @org/team-a", "cc @" + zw + "org/team-a"},
		{"after punctuation", "(@carol) and @dave.", "(@" + zw + "carol) and @" + zw + "dave."},
		{"email untouched", "mail a@b.com or first.last@example.org", "mail a@b.com or first.last@example.org"},
		{"lone at untouched", "meet @ noon", "meet @ noon"},
		{"entity-encoded mention", "ping &#64;eve", "ping @" + zw + "eve"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := SanitizePrDescriptionText(c.in, 600); got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

// TestSanitizePrDescriptionClosingKeywords covers every keyword form the GitLab default pattern
// accepts (a superset of GitHub's and Forgejo's) against every reference syntax, with and
// without a colon and in mixed case: none may survive as a closing directive.
func TestSanitizePrDescriptionClosingKeywords(t *testing.T) {
	keywords := []string{
		"Close", "Closes", "closed", "Closing",
		"Fix", "Fixes", "fixed", "Fixing",
		"Resolve", "Resolves", "resolved", "Resolving",
		"Implement", "Implements", "implemented", "Implementing",
		"CLOSES", "fIxEs",
	}
	refs := []string{
		"#12", "issue #12", "issues #12", "Issue #12",
		"owner/repo#12", "group/sub/project#12", "project#12",
		"https://gitlab.example.com/g/p/-/issues/12",
		"https://github.com/o/r/issues/12",
		"https://codeberg.org/o/r/issues/12",
		"https://gitlab.example.com/g/p/-/work_items/12",
	}
	for _, kw := range keywords {
		for _, ref := range refs {
			for _, colon := range []string{"", ":"} {
				in := "This PR " + kw + colon + " " + ref + " today."
				got := SanitizePrDescriptionText(in, 600)
				if m := closingProbe.FindString(got); m != "" {
					t.Errorf("closing directive survived: in=%q out=%q match=%q", in, got, m)
				}
				first, size := utf8.DecodeRuneInString(kw)
				wantPrefix := "This PR " + string(first) + zw + kw[size:]
				if !strings.HasPrefix(got, wantPrefix) {
					t.Errorf("keyword not neutralised in place: in=%q out=%q", in, got)
				}
			}
		}
	}

	extra := []string{
		"**Fixes** #12",
		"Fixes\n#12",
		"Resolves:   #7, #8 and #9",
		"closes GH-12",
		"fix [#12](https://github.com/o/r/issues/12)",
		"Fixes &#35;12",
	}
	for _, in := range extra {
		got := SanitizePrDescriptionText(in, 600)
		if m := closingProbe.FindString(strings.ReplaceAll(strings.ReplaceAll(got, "*", ""), "GH-", "#")); m != "" {
			t.Errorf("closing directive survived: in=%q out=%q match=%q", in, got, m)
		}
		if !strings.Contains(got, zw) {
			t.Errorf("no breaker inserted: in=%q out=%q", in, got)
		}
	}

	// Negatives: a plain reference, or a keyword with no reference, is left exactly as written.
	for _, in := range []string{
		"See #12 for context.",
		"This fixes a crash in the parser.",
		"Prefix affixes #12",
		"The fixture #12 is new.",
	} {
		if got := SanitizePrDescriptionText(in, 600); got != in {
			t.Errorf("non-directive text changed: in=%q out=%q", in, got)
		}
	}
}

// TestSanitizePrDescriptionIdempotent pins that sanitizing output that was not cut changes
// nothing, so a worker re-posting stored fields (a refresh's context) round-trips.
func TestSanitizePrDescriptionIdempotent(t *testing.T) {
	inputs := []string{
		"Fixes #12 for @alice <!-- uzi:description:start v1 --> [x](y) ![i](j) <b>b</b>",
		"Closing: issues owner/repo#3 and a --> b @@x",
		"a &amp;lt;!-- x",
		"# Heading with [brackets] and ```fence``` and ~~~tilde",
		"1. first _Fixes_ #9 and Fix**es** #10",
		"- item \\[already\\] escaped \\",
		"trailing backslash \\",
		"> quote [r]: https://x.test and ![r] then [r]",
		"*** ",
		"a ------------> b <!-- c",
		"[]:x",
		"[a\\](b) and [c\\][d]",
		"/close",
		"Map<string, int> and x < y and a <= b",
	}
	for _, in := range inputs {
		once := SanitizePrDescriptionText(in, 600)
		twice := SanitizePrDescriptionText(once, 600)
		if once != twice {
			t.Errorf("not idempotent:\n in: %q\n 1x: %q\n 2x: %q", in, once, twice)
		}
		assertPrDescInert(t, once)
	}
	// A cut output may lose the breaker of a keyword whose reference the cut removed; the
	// second pass must still be inert and stable from there on.
	long := strings.Repeat("Implements #1 @z ", 80)
	once := SanitizePrDescriptionText(long, 600)
	twice := SanitizePrDescriptionText(once, 600)
	assertPrDescInert(t, twice)
	if thrice := SanitizePrDescriptionText(twice, 600); thrice != twice {
		t.Errorf("cut output not stable on the third pass:\n 2x: %q\n 3x: %q", twice, thrice)
	}
}

// TestSanitizePrDescriptionByteCaps pins that the layout caps are BYTES of the final text,
// breakers and escapes included, that a cut never splits a rune, and that a cut never leaves a
// keyword with its reference but without its breaker, or a mention without its breaker.
func TestSanitizePrDescriptionByteCaps(t *testing.T) {
	for _, maxBytes := range []int{PrDescSummaryMaxBytes, PrDescItemMaxBytes, PrDescVerifyCommandMaxBytes, 7, 1} {
		for _, in := range []string{
			strings.Repeat("é", 700),
			strings.Repeat("@ab ", 300),
			strings.Repeat("Fixes #1 ", 200),
			strings.Repeat("[x] ", 300),
			strings.Repeat("😀", 300),
			strings.Repeat("a", maxBytes-1) + " @bob",
		} {
			got := SanitizePrDescriptionText(in, maxBytes)
			if len(got) > maxBytes {
				t.Errorf("cap %d: got %d bytes: %q", maxBytes, len(got), got)
			}
			if !utf8.ValidString(got) {
				t.Errorf("cap %d: invalid UTF-8: %q", maxBytes, got)
			}
			if len(in) > maxBytes && got != "" && !strings.HasSuffix(got, "…") {
				t.Errorf("cap %d: a cut must end with an ellipsis: %q", maxBytes, got)
			}
			assertPrDescInert(t, got)
			if regexp.MustCompile(`@[A-Za-z0-9_-]`).MatchString(got) {
				t.Errorf("cap %d: a mention lost its breaker: %q", maxBytes, got)
			}
		}
	}
	if got := SanitizePrDescriptionText("short", PrDescSummaryMaxBytes); got != "short" {
		t.Fatalf("short text changed: %q", got)
	}
	// Exactly at the cap before the breaker is inserted: the breaker pushes it over, so it is cut.
	exact := strings.Repeat("a", PrDescItemMaxBytes-5) + " @bob"
	if got := SanitizePrDescriptionText(exact, PrDescItemMaxBytes); len(got) > PrDescItemMaxBytes {
		t.Fatalf("breaker overflowed the cap: %d bytes", len(got))
	}
}

// TestSanitizePrDescriptionSecretsSplitByMarkup: a credential split by markup the sanitizer
// removes must be scrubbed once the markup is gone (the scrub re-runs after the strip). The
// fragments are joined at runtime; no token-shaped literal is in source.
func TestSanitizePrDescriptionSecretsSplitByMarkup(t *testing.T) {
	a10, b10 := strings.Repeat("A", 10), strings.Repeat("B", 10)
	ghBody := "notARealToken" + "0123456789abcdefghijklm"
	cases := []struct{ in, body string }{
		{"glpat-" + a10 + "<b></b>" + b10, b10},
		{"glpat-" + a10 + "<!---->" + b10, b10},
		{"glpat-" + a10 + "[x](y)" + b10, b10},
		{"ghp_" + "<!-- x -->" + ghBody, ghBody[len(ghBody)-12:]},
		{"ghp_" + "&lt;b&gt;&lt;/b&gt;" + ghBody, ghBody[len(ghBody)-12:]},
	}
	for _, c := range cases {
		got := SanitizePrDescriptionText("token "+c.in+" leaked", 600)
		if strings.Contains(got, c.body) {
			t.Errorf("split secret re-joined unredacted: in=%q out=%q", c.in, got)
		}
	}
}

// TestSanitizePrDescriptionEntityFixedPoint: entities are decoded to a true fixed point, so a
// deeply encoded mention or reference is handled like the character it renders as, and an
// entity re-formed by a markup removal is decoded too.
func TestSanitizePrDescriptionEntityFixedPoint(t *testing.T) {
	enc := func(s string, levels int) string {
		for i := 1; i < levels; i++ {
			s = strings.Replace(s, "&", "&amp;", 1)
		}
		return s
	}
	mention := SanitizePrDescriptionText("ping "+enc("&#64;", 5)+"eve", 600)
	if mention != "ping @"+zw+"eve" {
		t.Errorf("5-level encoded mention = %q", mention)
	}
	closing := SanitizePrDescriptionText("Fixes "+enc("&#35;", 5)+"12", 600)
	if closing != "F"+zw+"ixes #12" {
		t.Errorf("5-level encoded closing ref = %q", closing)
	}
	reformed := SanitizePrDescriptionText("ping &<b></b>#64;eve", 600)
	if reformed != "ping @"+zw+"eve" {
		t.Errorf("markup-split entity = %q", reformed)
	}
	for _, out := range []string{mention, closing, reformed} {
		if regexp.MustCompile(`&#?[A-Za-z0-9]+;`).MatchString(out) {
			t.Errorf("an entity survived: %q", out)
		}
	}
}

// TestSanitizePrDescriptionImagesAndReferences: no image in any form, and no reference link or
// definition can form, including a definition split across lines (renderable by micromark /
// CommonMark as `![r]` + `[r]:\nhttps://...`).
func TestSanitizePrDescriptionImagesAndReferences(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"shortcut image with split definition", "see ![r] here\n\n[r]:\nhttps://evil.test/x.png", "see here"},
		{"definition only", "[r]:\n  https://evil.test/track", ""},
		{"full reference image", "a ![alt][r] b", "a b"},
		{"collapsed reference image", "a ![alt][] b", "a b"},
		{"inline image", "a ![alt](https://evil.test/x.png) b", "a b"},
		{"collapsed link keeps text", "a [text][] b", "a text b"},
		{"shortcut link bracket escaped", "a [r] b\n[r]: https://evil.test", "a \\[r\\] b"},
		{"definition mid-text", "text [r]: https://evil.test more", "text more"},
	}
	for _, c := range cases {
		got := SanitizePrDescriptionText(c.in, 600)
		if got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
		if strings.Contains(got, "evil") {
			t.Errorf("%s: a target survived: %q", c.name, got)
		}
		assertPrDescInert(t, got)
	}
}

// TestSanitizePrDescriptionBlockSyntax: a field cannot open a heading, quote, table, list, code
// block or thematic break, and a fence run anywhere is escaped.
func TestSanitizePrDescriptionBlockSyntax(t *testing.T) {
	cases := []struct{ in, want string }{
		{"```go\nrm -rf /\n```", "\\`\\`\\`go rm -rf / \\`\\`\\`"},
		{"~~~ swallow the rest", "\\~\\~\\~ swallow the rest"},
		{"# Title", "\\# Title"},
		{"### Title", "\\### Title"},
		{"> quoted", "\\> quoted"},
		{"| a | b |", "\\| a | b |"},
		{"- item", "\\- item"},
		{"+ item", "\\+ item"},
		{"* item", "\\* item"},
		{"1. first", "1\\. first"},
		{"12) twelfth", "12\\) twelfth"},
		{"===", "\\==="},
		{"---", "\\---"},
		{"***", "\\***"},
		{"_ _ _", "\\_ _ _"},
		{"**Bold** start", "**Bold** start"},
		{"`code` start", "`code` start"},
		{"mid ```` run", "mid \\`\\`\\`\\` run"},
		{"already \\``` escaped", "already \\`\\`\\` escaped"},
		{"a \\\\``` b", "a \\\\\\`\\`\\` b"},
		{"ends with \\", "ends with \\\\"},
		{"version 1.2 ok", "version 1.2 ok"},
	}
	for _, c := range cases {
		if got := SanitizePrDescriptionText(c.in, 600); got != c.want {
			t.Errorf("SanitizePrDescriptionText(%q)\n got: %q\nwant: %q", c.in, got, c.want)
		}
	}
}

// TestSanitizePrDescriptionCommentDelimiters: no `-->` / `<!--` survives in any length.
func TestSanitizePrDescriptionCommentDelimiters(t *testing.T) {
	for _, in := range []string{"a ------------> b", "a --!> b", "a <!------ b", "x --> y ---> z", "<!<!---->--"} {
		got := SanitizePrDescriptionText(in, 600)
		assertPrDescInert(t, got)
		if strings.Contains(got, "-->") || strings.Contains(got, "--!>") {
			t.Errorf("comment close survived: in=%q out=%q", in, got)
		}
	}
	if got := SanitizePrDescriptionText("a ------------> b", 600); got != "a -> b" {
		t.Errorf("long arrow = %q, want %q", got, "a -> b")
	}
}

// TestSanitizePrDescriptionClosingEmphasisForms: emphasis, brackets, a missing space and
// Unicode spaces around a closing keyword cannot hide it.
func TestSanitizePrDescriptionClosingEmphasisForms(t *testing.T) {
	for _, in := range []string{
		"_Fixes_ #12", "__Fixes__ #12", "Fix**es** #12", "Fixes:#12", "Fixes#12",
		"Fixes\u2003#12", "Fixes\u3000#12", "Fixes\u00A0#12", "Fixes [#12]", "~~Fixes~~ #12",
		"`Fixes` #12", "**Fixes:** #12", "a_Fixes #12", "Closes PROJ-12",
	} {
		got := SanitizePrDescriptionText(in, 600)
		if !strings.Contains(got, zw) {
			t.Errorf("no breaker: in=%q out=%q", in, got)
		}
		if m := closingViewProbe(got); m != "" {
			t.Errorf("closing directive survived: in=%q out=%q match=%q", in, got, m)
		}
	}
}

// closingViewProbe looks for a closing directive in s with the markdown markers removed and any
// space run optional: the widest reading a forge could apply.
func closingViewProbe(s string) string {
	view := strings.Map(func(r rune) rune {
		if strings.ContainsRune("*_~`[]\\", r) {
			return -1
		}
		return r
	}, s)
	return regexp.MustCompile(`(?i)\b(clos(e[sd]?|ing)|fix(e[sd]|ing)?|resolv(e[sd]?|ing)|implement(s|ed|ing)?)\b[\s\p{Z}]*:?[\s\p{Z}]*(issues?[\s\p{Z}]*)?(#\d+|[\w.-]+(/[\w.-]+)*#\d+|[A-Z]+-\d+)`).FindString(view)
}

// TestSanitizePrDescriptionScrubsSecrets feeds credential-shaped strings assembled from
// fragments at runtime (never one literal in source) and requires the token body gone.
func TestSanitizePrDescriptionScrubsSecrets(t *testing.T) {
	secrets := []string{
		"glpat-" + "notAReal" + "012345678901",
		"ghp_" + "notARealToken" + "0123456789abcdefghijklm",
		"sk-ant-" + "oat01-" + "notARealAnthropic" + "0123456789abcdefghij",
	}
	for _, secret := range secrets {
		body := secret[len(secret)-12:]
		got := SanitizePrDescriptionText("token "+secret+" leaked", 600)
		if strings.Contains(got, body) {
			t.Errorf("secret body survived sanitization: %q", got)
		}
		fields, err := SanitizePrDescriptionFields(context.Background(), apitypes.PrDescriptionFields{
			Summary:      "uses " + secret,
			Changes:      []string{"set " + secret},
			Verification: []apitypes.PrDescriptionVerification{{Command: "curl -H " + secret, Result: "pass", VerifiedAtSha: "abcdef1"}},
		})
		if err != nil {
			t.Fatalf("fields: %v", err)
		}
		for _, s := range []string{fields.Summary, fields.Changes[0], fields.Verification[0].Command} {
			if strings.Contains(s, body) {
				t.Errorf("secret body survived in a field: %q", s)
			}
		}
	}
}

func TestSanitizePrDescriptionFieldsLimitsAndValidation(t *testing.T) {
	long := strings.Repeat("x", 300)
	in := apitypes.PrDescriptionFields{
		Summary:        "Adds retries. Fixes #4.",
		Changes:        []string{"a", "b", "", "<!-- -->", "c", "d", "e", "f", long},
		ReviewPointers: []string{"p1", "p2", "p3"},
		ScopeNotes: []apitypes.PrDescriptionScopeNote{
			{Kind: "added", Text: "n1"}, {Kind: "deferred", Text: "n2"}, {Kind: "dropped", Text: ""},
		},
		Verification: []apitypes.PrDescriptionVerification{{Command: "task gate:api", Result: "pass", VerifiedAtSha: "ABCDEF1234567"}},
	}
	out, err := SanitizePrDescriptionFields(context.Background(), in)
	if err != nil {
		t.Fatalf("sanitize: %v", err)
	}
	if got := strings.Join(out.Changes, ","); got != "a,b,c,d,e" {
		t.Errorf("changes = %q, want the first five non-empty entries", got)
	}
	if len(out.ReviewPointers) != PrDescMaxReviewPointers || out.ReviewPointers[1] != "p2" {
		t.Errorf("review pointers = %q", out.ReviewPointers)
	}
	if len(out.ScopeNotes) != 2 || out.ScopeNotes[1].Kind != "deferred" {
		t.Errorf("scope notes = %+v (empty text must be dropped)", out.ScopeNotes)
	}
	if out.Verification[0].VerifiedAtSha != "abcdef1234567" {
		t.Errorf("verified_at_sha must be lowercased, got %q", out.Verification[0].VerifiedAtSha)
	}
	if strings.Contains(out.Summary, "Fixes #4") {
		t.Errorf("summary closing directive survived: %q", out.Summary)
	}

	empty, err := SanitizePrDescriptionFields(context.Background(), apitypes.PrDescriptionFields{})
	if err != nil || empty.Changes == nil || empty.ScopeNotes == nil || empty.ReviewPointers == nil || empty.Verification == nil {
		t.Fatalf("empty fields must sanitize to non-nil slices, got %+v err=%v", empty, err)
	}

	bad := []struct {
		name string
		f    apitypes.PrDescriptionFields
	}{
		{"summary over raw cap", apitypes.PrDescriptionFields{Summary: strings.Repeat("s", MaxPrDescSummaryRawBytes+1)}},
		{"change over raw cap", apitypes.PrDescriptionFields{Changes: []string{strings.Repeat("c", MaxPrDescItemRawBytes+1)}}},
		{"too many changes", apitypes.PrDescriptionFields{Changes: make([]string, MaxPrDescListRawEntries+1)}},
		{"unknown scope kind", apitypes.PrDescriptionFields{ScopeNotes: []apitypes.PrDescriptionScopeNote{{Kind: "risk", Text: "t"}}}},
		{"bad result", apitypes.PrDescriptionFields{Verification: []apitypes.PrDescriptionVerification{{Command: "c", Result: "ok", VerifiedAtSha: "abcdef1"}}}},
		{"short sha", apitypes.PrDescriptionFields{Verification: []apitypes.PrDescriptionVerification{{Command: "c", Result: "pass", VerifiedAtSha: "abc"}}}},
		{"non-hex sha", apitypes.PrDescriptionFields{Verification: []apitypes.PrDescriptionVerification{{Command: "c", Result: "fail", VerifiedAtSha: "zzzzzzz"}}}},
	}
	for _, c := range bad {
		if _, err := SanitizePrDescriptionFields(context.Background(), c.f); !errors.Is(err, ErrPrDescriptionInvalid) {
			t.Errorf("%s: err = %v, want ErrPrDescriptionInvalid", c.name, err)
		}
	}
}

// assertPrDescInert is the invariant every sanitized string holds: no comment delimiter, no
// tag-shaped `<`, no closing directive.
func assertPrDescInert(t *testing.T, s string) {
	t.Helper()
	if strings.Contains(s, "<!--") || strings.Contains(s, "-->") {
		t.Errorf("output carries a comment delimiter: %q", s)
	}
	if regexp.MustCompile(`<[/!?A-Za-z]`).MatchString(s) {
		t.Errorf("output carries a tag-shaped '<': %q", s)
	}
	if m := closingProbe.FindString(s); m != "" {
		t.Errorf("output carries a closing directive %q: %q", m, s)
	}
	// Every bracket is escaped (an odd run of backslashes precedes it), so no link, image,
	// reference or definition can form.
	backslashes := 0
	for i := 0; i < len(s); i++ {
		if (s[i] == '[' || s[i] == ']') && backslashes%2 == 0 {
			t.Errorf("output carries a live bracket at %d: %q", i, s)
		}
		if s[i] == '\\' {
			backslashes++
		} else {
			backslashes = 0
		}
	}
}

// prDescAdversarial returns worst-case sanitizer inputs of exactly n bytes: shapes that made a
// fixed-point pass remove one character at a time (quadratic in the field), and deep nesting of
// comments and entities.
func prDescAdversarial(n int) []string {
	fill := func(unit string, tail string) string {
		body := strings.Repeat(unit, (n-len(tail))/len(unit)+1)[:n-len(tail)]
		return body + tail
	}
	return []string{
		strings.Repeat("<", n-1) + "a",
		"</" + strings.Repeat("<", n-3) + "/",
		strings.Repeat("[", n),
		fill("&amp;", ""),
		"&" + fill("amp;", "lt;")[1:],
		fill("*_", ""),
		fill("<!", "---->"),
		fill("<!--", ""),
		fill("<a", ">"),
		fill("[x](", ""),
		fill("![", ""),
		fill("@_", ""),
		fill("Fixes #1 ", ""),
	}
}

// prDescFullBody is a stage body at every raw cap: the summary, and every list at its entry cap
// with every entry at the item cap, entries drawn from items in rotation.
func prDescFullBody(summary string, items []string) apitypes.PrDescriptionFields {
	in := apitypes.PrDescriptionFields{Summary: summary}
	for j := 0; j < MaxPrDescListRawEntries; j++ {
		in.Changes = append(in.Changes, items[j%len(items)])
		in.ReviewPointers = append(in.ReviewPointers, items[(j+1)%len(items)])
		in.ScopeNotes = append(in.ScopeNotes, apitypes.PrDescriptionScopeNote{Kind: "added", Text: items[(j+2)%len(items)]})
		in.Verification = append(in.Verification, apitypes.PrDescriptionVerification{Command: items[(j+3)%len(items)], Result: "pass", VerifiedAtSha: "abcdef1"})
	}
	return in
}

func prDescProse(n int) string {
	return strings.Repeat("Adds a retry to the uploader, see the notes in docs/retry.md for details. ", n/70+1)[:n]
}

// TestSanitizePrDescriptionWorstCaseIsLinear (H-A): every adversarial shape, and a full stage body
// at every raw cap built from all of them, sanitizes in time proportional to its size. Each is
// timed against the same-size plain prose in the same process (so -race and a loaded host scale
// both sides) and must stay within 10x of it plus a small constant, with an absolute backstop.
// Before the fix one 4000-byte `<`-run took seconds (thousands of times plain prose), and a full
// body of them tens of seconds.
func TestSanitizePrDescriptionWorstCaseIsLinear(t *testing.T) {
	timeIt := func(f func()) time.Duration {
		start := time.Now()
		f()
		return time.Since(start)
	}
	summaries := prDescAdversarial(MaxPrDescSummaryRawBytes)
	prose := prDescProse(MaxPrDescSummaryRawBytes)
	base := timeIt(func() { SanitizePrDescriptionText(prose, PrDescSummaryMaxBytes) })
	for i, summary := range summaries {
		var out string
		elapsed := timeIt(func() { out = SanitizePrDescriptionText(summary, PrDescSummaryMaxBytes) })
		if elapsed > 10*base+50*time.Millisecond {
			t.Errorf("shape %d (%.20q…): one %d-byte field took %v (plain prose %v)", i, summary, len(summary), elapsed, base)
		}
		assertPrDescInert(t, out)
	}

	plain := prDescFullBody(prose, []string{prDescProse(MaxPrDescItemRawBytes)})
	worst := prDescFullBody(summaries[0], prDescAdversarial(MaxPrDescItemRawBytes))
	var out apitypes.PrDescriptionFields
	var err error
	baseBody := timeIt(func() { _, _ = SanitizePrDescriptionFields(context.Background(), plain) })
	elapsed := timeIt(func() { out, err = SanitizePrDescriptionFields(context.Background(), worst) })
	if err != nil {
		t.Fatalf("full body: %v", err)
	}
	t.Logf("full stage body: worst case %v, plain prose %v", elapsed, baseBody)
	if elapsed > 10*baseBody+200*time.Millisecond || elapsed > 10*time.Second {
		t.Errorf("worst-case full stage body took %v (plain prose %v)", elapsed, baseBody)
	}
	for _, s := range append(append([]string{out.Summary}, out.Changes...), out.ReviewPointers...) {
		assertPrDescInert(t, s)
	}
}

// TestSanitizePrDescriptionFieldsHonoursCancel: a cancelled request stops sanitizing.
func TestSanitizePrDescriptionFieldsHonoursCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := SanitizePrDescriptionFields(ctx, apitypes.PrDescriptionFields{Summary: "x", Changes: []string{"y"}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// TestSanitizePrDescriptionSecretsSplitByInlineMarkdown (B-A/M-A): a credential split by inline
// markdown that re-joins when RENDERED (emphasis, a code span, strikethrough, a backslash escape)
// replaces the whole field item with the redaction placeholder. Fragments are
// joined at runtime; no token-shaped literal is in source.
func TestSanitizePrDescriptionSecretsSplitByInlineMarkdown(t *testing.T) {
	gl, gh := "gl"+"pat-", "gh"+"p_"
	a10, b9, b10 := strings.Repeat("A", 10), strings.Repeat("B", 9), strings.Repeat("B", 10)
	cases := []string{
		gl + a10 + "*B*" + b9,
		gl + a10 + "`B`" + b9,
		gl + a10 + "**B**" + b9,
		gl + a10 + "~~B~~" + b9,
		gl + a10 + "\\-" + b9,
		gl + a10 + "\\_" + b9,
		gh + a10 + "\\_" + b10,
	}
	const want = "\\[redacted\\]"
	for _, in := range cases {
		for _, text := range []string{in, "token " + in + " leaked"} {
			if got := SanitizePrDescriptionText(text, 600); got != want {
				t.Errorf("SanitizePrDescriptionText(%q) = %q, want %q", text, got, want)
			}
		}
		fields, err := SanitizePrDescriptionFields(context.Background(), apitypes.PrDescriptionFields{
			Summary: "ok", Changes: []string{"first", "uses " + in},
		})
		if err != nil {
			t.Fatalf("fields: %v", err)
		}
		if fields.Summary != "ok" || len(fields.Changes) != 2 || fields.Changes[0] != "first" || fields.Changes[1] != want {
			t.Errorf("only the item carrying %q must be redacted: %+v", in, fields)
		}
	}
	// A zero-width rune is stripped before the literal scrub, which then catches the token.
	if got := SanitizePrDescriptionText("t "+gl+a10+"\u200B"+b10, 600); strings.Contains(got, b10) {
		t.Errorf("zero-width split secret survived: %q", got)
	}
	// Ordinary inline markdown and identifiers are not secrets.
	for _, in := range []string{"a *b* c `d` ~~e~~ and snake_case_name", "max_app-config and x\\-y", "ghp_ short"} {
		if got := SanitizePrDescriptionText(in, 600); strings.Contains(got, "redacted") {
			t.Errorf("false positive: %q -> %q", in, got)
		}
	}
}

// TestSanitizePrDescriptionMentionTable (B-B/L-B): every `@` not preceded by an ASCII letter or
// digit is broken, whatever follows it; emails and a lone `@` are left as written.
func TestSanitizePrDescriptionMentionTable(t *testing.T) {
	cases := []struct{ in, want string }{
		{"_@alice_", "_@" + zw + "alice_"},
		{"__@alice__", "__@" + zw + "alice__"},
		{"hello _@alice_ there", "hello _@" + zw + "alice_ there"},
		{"*@alice*", "*@" + zw + "alice*"},
		{"@\\_alice", "@" + zw + "\\_alice"},
		{"@\\.bob", "@" + zw + "\\.bob"},
		{"@.foo", "@" + zw + ".foo"},
		{"x @@y", "x @" + zw + "@" + zw + "y"},
		{"a_b@x.com and a.b@example.com", "a_b@x.com and a.b@example.com"},
		{"first.last@example.org", "first.last@example.org"},
		{"meet @ noon", "meet @ noon"},
		{"ends with @", "ends with @"},
	}
	for _, c := range cases {
		got := SanitizePrDescriptionText(c.in, 600)
		if got != c.want {
			t.Errorf("SanitizePrDescriptionText(%q) = %q, want %q", c.in, got, c.want)
		}
		if again := SanitizePrDescriptionText(got, 600); again != got {
			t.Errorf("not idempotent: %q -> %q", got, again)
		}
	}
}

// TestSanitizePrDescriptionQuickActions (M-B): a field cannot open a GitLab quick action.
func TestSanitizePrDescriptionQuickActions(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/close", "\\/close"},
		{"/merge", "\\/merge"},
		{"  /target_branch main", "\\/target_branch main"},
		{"/approve", "\\/approve"},
		{"\n/close", "\\/close"},
		{"text\n/close", "text /close"},
		{"a/b path", "a/b path"},
	}
	for _, c := range cases {
		got := SanitizePrDescriptionText(c.in, 600)
		if got != c.want {
			t.Errorf("SanitizePrDescriptionText(%q) = %q, want %q", c.in, got, c.want)
		}
		if again := SanitizePrDescriptionText(got, 600); again != got {
			t.Errorf("not idempotent: %q -> %q", got, again)
		}
	}
}

// TestValidPrDescSizeUnavailable (N5): an unavailable size with all-zero buckets (exactly what
// the worker sends when the size cannot be computed) is accepted; a non-zero bucket is not.
func TestValidPrDescSizeUnavailable(t *testing.T) {
	if !validPrDescSize(&apitypes.PrDescriptionSize{Unavailable: true}) {
		t.Fatal("unavailable size with all-zero buckets must be accepted")
	}
	if validPrDescSize(&apitypes.PrDescriptionSize{Unavailable: true, Docs: apitypes.PrDescriptionSizeBucket{Deleted: 1}}) {
		t.Fatal("unavailable size with a non-zero bucket must be refused")
	}
}
