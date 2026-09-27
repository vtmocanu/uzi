package workersvc

import (
	"errors"
	"regexp"
	"strings"
	"testing"
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
		{"bidi and control stripped", "a\u202Eb\x1b[31mc", "ab[31mc"},
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

// TestSanitizePrDescriptionIdempotent pins that sanitizing sanitized output changes nothing, so a
// worker re-posting stored fields (a refresh's context) round-trips.
func TestSanitizePrDescriptionIdempotent(t *testing.T) {
	inputs := []string{
		"Fixes #12 for @alice <!-- uzi:description:start v1 --> [x](y) ![i](j) <b>b</b>",
		"Closing: issues owner/repo#3 and a --> b @@x",
		"a &amp;lt;!-- x",
	}
	for _, in := range inputs {
		once := SanitizePrDescriptionText(in, 600)
		twice := SanitizePrDescriptionText(once, 600)
		if once != twice {
			t.Errorf("not idempotent:\n in: %q\n 1x: %q\n 2x: %q", in, once, twice)
		}
		assertPrDescInert(t, once)
	}
	// A trimmed output may lose the breaker of a keyword whose reference the trim cut off; the
	// second pass must still be inert and stable from there on.
	long := strings.Repeat("Implements #1 @z ", 80)
	once := SanitizePrDescriptionText(long, 600)
	twice := SanitizePrDescriptionText(once, 600)
	assertPrDescInert(t, twice)
	if thrice := SanitizePrDescriptionText(twice, 600); thrice != twice {
		t.Errorf("trimmed output not stable on the third pass:\n 2x: %q\n 3x: %q", twice, thrice)
	}
}

func TestSanitizePrDescriptionTrim(t *testing.T) {
	got := SanitizePrDescriptionText(strings.Repeat("é", 700), PrDescSummaryMaxChars)
	if n := utf8.RuneCountInString(got); n != PrDescSummaryMaxChars {
		t.Fatalf("trimmed to %d runes, want %d", n, PrDescSummaryMaxChars)
	}
	if !strings.HasSuffix(got, "…") || !utf8.ValidString(got) {
		t.Fatalf("trim must end with an ellipsis and stay valid UTF-8, got suffix %q", got[len(got)-8:])
	}
	if got := SanitizePrDescriptionText("short", PrDescSummaryMaxChars); got != "short" {
		t.Fatalf("short text changed: %q", got)
	}
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
		fields, err := SanitizePrDescriptionFields(apitypes.PrDescriptionFields{
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
	out, err := SanitizePrDescriptionFields(in)
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

	empty, err := SanitizePrDescriptionFields(apitypes.PrDescriptionFields{})
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
		if _, err := SanitizePrDescriptionFields(c.f); !errors.Is(err, ErrPrDescriptionInvalid) {
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
}
