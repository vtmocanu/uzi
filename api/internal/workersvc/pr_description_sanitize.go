package workersvc

import (
	"errors"
	"html"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// PR-description field limits (PRD #1798 D7 / target layout). Two kinds of bound:
//
//   - RAW caps bound what a worker may send at all. Over them the request is refused (400):
//     the worker mirrors the layout limits before posting, so an over-cap body is a bug or an
//     attack, never a legitimate description.
//   - LAYOUT caps bound what is published. The sanitized text is trimmed to them (rune-safe,
//     with an ellipsis), and a list longer than its layout cap keeps its first entries.
const (
	MaxPrDescSummaryRawBytes = 4000
	MaxPrDescItemRawBytes    = 1000
	MaxPrDescListRawEntries  = 50

	PrDescSummaryMaxChars       = 600
	PrDescItemMaxChars          = 200
	PrDescMaxChanges            = 5
	PrDescMaxScopeNotes         = 8
	PrDescMaxReviewPointers     = 2
	PrDescMaxVerification       = 20
	PrDescVerifyCommandMaxChars = 200
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

var (
	// HTML comments, including an unterminated opener that runs to the end of the text.
	prDescHTMLComment      = regexp.MustCompile(`(?s)<!--.*?-->`)
	prDescUnterminatedCmnt = regexp.MustCompile(`(?s)<!--.*$`)
	// Markdown images, inline `![alt](target)` and reference `![alt][ref]`.
	prDescImageInline = regexp.MustCompile(`!\[[^\]]*\]\([^)]*\)`)
	prDescImageRef    = regexp.MustCompile(`!\[[^\]]*\]\[[^\]]*\]`)
	// Markdown links keep their text and lose their target.
	prDescLinkInline = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	prDescLinkRef    = regexp.MustCompile(`\[([^\]]*)\]\[[^\]]*\]`)
	// A reference definition line `[ref]: target`.
	prDescLinkDef = regexp.MustCompile(`(?m)^[ \t]*\[[^\]]+\]:[ \t]*\S.*$`)
	// Angle-bracket autolinks become the bare URL text.
	prDescAutolink = regexp.MustCompile(`<((?i:https?|ftp)://[^\s<>]+)>`)
	// Anything tag-shaped: an element, a closing tag, a declaration or a processing instruction.
	prDescTag = regexp.MustCompile(`<[/!?]?[A-Za-z][^<>]*>`)
	// A `<` that could still open markup once a later `>` arrives (the renderer's own markers
	// follow the region), so it is dropped.
	prDescOpenAngle = regexp.MustCompile(`<([/!?A-Za-z])`)
	prDescSpaces    = regexp.MustCompile(`[ \t\r\n\f\v\x{00A0}]+`)

	// A mention: `@` at the start or after a non-word character, followed by a handle character.
	// `a@b.com` is not a mention (the `@` follows a word character) and stays as written.
	prDescMention = regexp.MustCompile(`(^|[^A-Za-z0-9_])@([A-Za-z0-9_-])`)

	// Closing directives, the GitLab-superset pattern (approver point 4): every GitLab default
	// keyword form (a superset of GitHub's and Forgejo's), case-insensitive, optional colon,
	// optional `issue `/`issues `, then a same-project, cross-project or URL reference.
	// Emphasis markers around the keyword and colon are tolerated so `**Fixes** #1` matches.
	// Group 1 is the keyword; the whole match is only a locator.
	prDescClosing = regexp.MustCompile(`(?i)\b(clos(?:e[sd]?|ing)|fix(?:e[sd]|ing)?|resolv(?:e[sd]?|ing)|implement(?:s|ed|ing)?)\b` +
		"[*_~`]*:?[*_~`]*" + `[\s\x{00A0}]+(?:issues?[\s\x{00A0}]+)?` + "[*_~`]*" +
		`(?:#\d+|gh-\d+|[\w.-]+(?:/[\w.-]+)*#\d+|https?://[^\s<>()\[\]]*?/(?:issues|work_items)/\d+)`)
)

// SanitizePrDescriptionText renders one untrusted, model- or lead-authored PR-description
// string safe to publish inside uzi's description region (PRD #1798 D7). The api's result is the
// ONLY text the renderer may publish. Steps, in order:
//
//  1. HTML entities are decoded (to a fixed point), so an encoded `&lt;!--`, `&#35;` or `&#64;`
//     is seen by every later step as the character it renders as.
//  2. Control / bidi / format runes are stripped and secret shapes scrubbed (ScrubUntrustedText).
//  3. HTML comments (terminated or not) are removed, so a forged `<!-- uzi:... -->` marker or a
//     review bot's marker cannot survive; images are removed; links keep only their text;
//     autolinks become bare URL text; tag-shaped markup is removed, and any `<` that could still
//     open markup is dropped. A stray `-->` is folded to `->`.
//  4. Whitespace (newlines included) collapses to single spaces: every field is one paragraph or
//     one list item, so model text cannot add headings or sections of its own.
//  5. Closing directives are neutralised by a zero-width space after the keyword's first letter,
//     and mentions by one after the `@`.
//  6. The result is trimmed to maxChars runes (an ellipsis marks a cut).
//
// The output never contains `<!--`, `-->` or a tag-shaped `<`. A second pass over output that was
// not trimmed returns it unchanged. A trim can cut a neutralised keyword off from its reference;
// a second pass then drops that keyword's breaker, which is safe because the text is no longer a
// directive.
func SanitizePrDescriptionText(s string, maxChars int) string {
	for i := 0; i < 4; i++ {
		u := html.UnescapeString(s)
		if u == s {
			break
		}
		s = u
	}
	s = ScrubUntrustedText(s)
	s = stripPrDescMarkup(s)
	s = strings.TrimSpace(prDescSpaces.ReplaceAllString(s, " "))
	s = neutralizeClosingDirectives(s)
	s = breakMentions(s)
	return trimRunes(s, maxChars)
}

// stripPrDescMarkup removes HTML and markdown link/image syntax. Each removal can expose a new
// instance of an earlier pattern (`<!<!---->--` becomes `<!--`), so it loops to a fixed point.
func stripPrDescMarkup(s string) string {
	for i := 0; i < 8; i++ {
		prev := s
		// Terminated comments first, to a fixed point, so a comment exposed by removing another
		// (`<!<!---->-- x -->`) is removed as a comment rather than read as unterminated.
		for j := 0; j < 8; j++ {
			next := prDescHTMLComment.ReplaceAllString(s, "")
			if next == s {
				break
			}
			s = next
		}
		s = prDescUnterminatedCmnt.ReplaceAllString(s, "")
		s = prDescImageInline.ReplaceAllString(s, "")
		s = prDescImageRef.ReplaceAllString(s, "")
		s = prDescLinkInline.ReplaceAllString(s, "$1")
		s = prDescLinkRef.ReplaceAllString(s, "$1")
		s = prDescLinkDef.ReplaceAllString(s, "")
		s = prDescAutolink.ReplaceAllString(s, "$1")
		s = prDescTag.ReplaceAllString(s, "")
		s = prDescOpenAngle.ReplaceAllString(s, "$1")
		s = strings.ReplaceAll(s, "-->", "->")
		if s == prev {
			break
		}
	}
	return s
}

// neutralizeClosingDirectives inserts a zero-width space after the first letter of every closing
// keyword that is followed by an issue reference, so no forge reads it as a directive.
func neutralizeClosingDirectives(s string) string {
	locs := prDescClosing.FindAllStringSubmatchIndex(s, -1)
	if len(locs) == 0 {
		return s
	}
	var b strings.Builder
	last := 0
	for _, loc := range locs {
		kwStart := loc[2]
		_, size := utf8.DecodeRuneInString(s[kwStart:])
		b.WriteString(s[last : kwStart+size])
		b.WriteString(zeroWidthSpace)
		last = kwStart + size
	}
	b.WriteString(s[last:])
	return b.String()
}

// breakMentions inserts a zero-width space after every mention `@`, leaving emails intact.
func breakMentions(s string) string {
	// ReplaceAll cannot see overlapping matches (`@a @b` shares no char, but `(@@x` would), so
	// it loops; the inserted breaker is not a handle character, so a broken mention never
	// re-matches and the loop ends.
	for i := 0; i < 4; i++ {
		next := prDescMention.ReplaceAllString(s, "${1}@"+zeroWidthSpace+"${2}")
		if next == s {
			break
		}
		s = next
	}
	return s
}

// trimRunes bounds s to maxChars runes, ending a cut with an ellipsis inside the bound.
func trimRunes(s string, maxChars int) string {
	if maxChars <= 0 || utf8.RuneCountInString(s) <= maxChars {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:maxChars-1])) + "…"
}

// SanitizePrDescriptionFields validates the raw fields a worker posts and returns the sanitized,
// layout-bounded fields that are the only publishable text (PRD #1798 D7). A raw value over its
// cap, an unknown scope-note kind, a verification result other than pass|fail, or a
// verified_at_sha that is not 7-64 hex characters is ErrPrDescriptionInvalid. Entries that are
// empty after sanitization are dropped; lists longer than their layout cap keep their first
// entries. The returned slices are never nil.
func SanitizePrDescriptionFields(in apitypes.PrDescriptionFields) (apitypes.PrDescriptionFields, error) {
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
	out.Summary = SanitizePrDescriptionText(in.Summary, PrDescSummaryMaxChars)

	var err error
	if out.Changes, err = sanitizePrDescList(in.Changes, PrDescMaxChanges); err != nil {
		return out, err
	}
	if out.ReviewPointers, err = sanitizePrDescList(in.ReviewPointers, PrDescMaxReviewPointers); err != nil {
		return out, err
	}
	for _, n := range in.ScopeNotes {
		if !validPrDescScopeKinds[n.Kind] || len(n.Text) > MaxPrDescItemRawBytes {
			return out, ErrPrDescriptionInvalid
		}
		text := SanitizePrDescriptionText(n.Text, PrDescItemMaxChars)
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
		cmd := SanitizePrDescriptionText(v.Command, PrDescVerifyCommandMaxChars)
		if cmd == "" || len(out.Verification) >= PrDescMaxVerification {
			continue
		}
		out.Verification = append(out.Verification, apitypes.PrDescriptionVerification{
			Command: cmd, Result: v.Result, VerifiedAtSha: strings.ToLower(v.VerifiedAtSha),
		})
	}
	return out, nil
}

func sanitizePrDescList(items []string, maxEntries int) ([]string, error) {
	out := []string{}
	for _, it := range items {
		if len(it) > MaxPrDescItemRawBytes {
			return out, ErrPrDescriptionInvalid
		}
		text := SanitizePrDescriptionText(it, PrDescItemMaxChars)
		if text == "" || len(out) >= maxEntries {
			continue
		}
		out = append(out, text)
	}
	return out, nil
}
