// Package termsafe holds uzi's terminal-safety predicate: the one place that decides
// which runes are allowed to reach a terminal, the two renderers that strip them, and
// the validator that refuses to store them in the first place.
//
// WHY IT IS A PACKAGE OF ITS OWN, and it is the same reason secretscrub is one: the
// rule is needed on both sides of a deliberate one-way dependency. The renderer half
// belongs to the CLI (api/internal/uzicli, api/cmd/uzi). The validator half belongs to
// api/internal/handler, which must NOT import a CLI package — that layering is
// backwards, and it would drag the CLI client and its cobra surface into the server's
// build graph to reach one rune test. So the classification lives here, in a leaf
// package with nothing but stdlib below it, and both sides derive from it.
//
// THE INVARIANT THIS PACKAGE EXISTS TO HOLD (#169): Validate rejects exactly what
// CellText changes. Formally, for every string s,
//
//	Validate(field, s) == nil   ⟺   CellText(s) == s
//
// so a name that passes the validator round-trips to a terminal display unchanged, and
// a name that would render as something other than what the user typed never lands in
// the database. TestValidateAgreesWithCellText pins that biconditional over a named
// corpus AND over every rune in Unicode; it is the whole guarantee, and without it the
// two halves drift the way issue #180's review found three hand-written copies of one
// list drifting.
//
// THREAT (#180). A server the user pointed --url at supplies run titles, worker names,
// token labels, judge prose, error messages and build info, and the CLI printed all of
// it raw. `\x1b[2J\x1b[1;1H` clears the screen and homes the cursor; `\x1b]0;…\x07`
// rewrites the window title; a bidi override reorders a name so it READS as another
// one. Impact is terminal display manipulation — hiding or spoofing prior output — not
// code execution, and it needs a hostile or compromised server, which the CLI's
// credentialSafeBase scheme guard still constrains to https:// or loopback.
//
// THREAT (#169), which is the reason the validator half exists and is NOT the same
// threat. `uzi admin workers` and `uzi admin cli-tokens` print names their OWNER chose
// beside a DIFFERENT user's OwnerEmail, so a crafted name is terminal control injection
// into another user's session, from an ordinary non-admin account and with no hostile
// server anywhere. The render boundary above already blunts it; the validator stops it
// being storable at all. Both are wanted: the render boundary is the trust boundary and
// covers rows stored before the validator existed, which no validator change could
// retroactively clean.
//
// REJECT ON WRITE, DO NOT STRIP ON WRITE. A silent strip hands the user back a name
// they never typed with no feedback, and it hides an attack instead of surfacing one.
// Both handlers that call Validate already 400 on an empty or over-long name, so a 400
// on an unsafe one is the idiom they are already written in rather than a new one.
//
// STRIP, NOT ESCAPE, at the RENDER boundary — the opposite call, deliberately, because
// escaping is the better default for a security-relevant renderer in general: it makes
// tampering visible instead of silent. Three things outweigh that here.
//
//  1. The house predicate already ships and already strips (sanitizeTTY, PRD #112 M3).
//     A boundary that ESCAPED underneath call sites that STRIP would produce output
//     matching neither, and their tests assert on stripped text.
//  2. Escaping expands. A label of 200 escapes becomes 800 columns and shreds the
//     tabwriter rail — which is itself an attack surface (#169: a forged table row in
//     a listing an admin reads to make decisions). The cure re-introduces the layout
//     half of the disease.
//  3. The visibility argument is already served, better, by a channel that exists:
//     --json is UNSANITIZED, so it carries the server's bytes verbatim and is one flag
//     away. Making the human table a second, worse forensic channel buys nothing.
//
// 🔴 ON POINT 3 — THE PROPERTY IS BYTE-PRESERVATION, NOT ESCAPING. That clause used to
// read "--json escapes control bytes losslessly (ESC arrives as the six characters
// backslash-u-0-0-1-b)". The ESC example is real; the generalisation from it is not.
// Measured on encoding/json (issue #144, two independent derivations agreeing on all
// eight rows): C0 ESC U+001B is escaped, and DEL U+007F, C1 CSI U+009B, bidi RLO
// U+202E, zero-width space U+200B, ZWJ U+200D, BOM U+FEFF and soft hyphen U+00AD are
// all NOT. So encoding/json escapes the C0 range (plus U+2028/U+2029) and passes every
// other member of Unsafe through untouched: none of Cf at all, and two of the three
// control families. Stated by FAMILY, and deliberately not as a fraction — DEL and C1
// pass through entire while C0 is escaped entire, so per CODEPOINT it is 33 of 65,
// about half. A fraction here would pair a range unit with a family denominator, which
// is the imprecision this very paragraph exists to correct.
//
// The decision point 3 supports is unaffected, and the correction arguably serves it
// better: what a forensic reader wants is the exact bytes the server sent, which is
// what being unsanitized delivers, and an encoder escaping only some of them would be
// the lossy option.
//
// The corollary, stated because the old wording hid it: --json is byte-preserving, NOT
// terminal-safe. A caller piping it straight to a terminal is outside everything this
// package provides. (An escaping fact only — whether a terminal HONOURS a UTF-8-encoded
// U+009B as a CSI introducer depends on its 8-bit control handling, and is not tested
// here.)
//
// TARGET IS CONTROL CHARACTERS, NOT "NON-ASCII". Both tests in Unsafe are Unicode
// CATEGORY predicates over runes, so accented Latin, CJK and emoji pass through
// byte-identical (measured). The one real casualty is stated on Unsafe itself.
//
// NOT YET THE MODULE'S ONLY SPELLING, and saying so is the point of this paragraph —
// a comment claiming a closed hazard is how the next reader skips checking.
// workersvc.hasUnsafeChar, handler.sanitizeSelfReported, handler.validateSecretLabel
// and workersvc.sanitizeMemoryField each still carry their own copy of some part of
// this rule, for their own reasons (different whitespace policy, different action on a
// hit, an extra U+FFFD test). Converging them is worth doing and was out of scope for
// #169, which is a two-consumer change. What this package guarantees today is that the
// CLI renderer and the name validators cannot disagree — not that nothing else in the
// module spells it a fourth way.
package termsafe

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Unsafe reports whether r is a rune that must not reach a terminal.
//
// The pair is the one workersvc.hasUnsafeChar settled on. unicode.IsControl covers C0
// (0x00-0x1F), C1 (0x80-0x9F) and DEL (0x7F) — which a hand-rolled `r < 0x20` range test
// misses. unicode.In(r, unicode.Cf) covers the format characters no range test can
// enumerate: the bidi overrides U+202A-202E, the isolates U+2066-2069, U+200F, the
// zero-widths, the BOM and SHY. The bidi half is the one that matters most — it visually
// reorders text, so a name can be made to read as another.
//
// Cc and Cf are DISJOINT categories, which is exactly why both tests are needed and why
// neither subsumes the other: IsControl alone never sees U+202E.
//
// TWO THINGS IT DOES NOT DO, so nobody reads it as more than it is:
//
//   - It CLASSIFIES ZWJ EMOJI SEQUENCES AS UNSAFE. U+200D is Cf, so "👨‍👩‍👧" is stripped
//     into three separate emoji by the renderer and refused outright by the validator
//     (measured, not inferred; U+FE0F is Mn and survives, so ordinary
//     variation-selector emoji such as "❤️" are untouched — TestVariationSelectorAndZWJ
//     pins both). That is the price of stripping the bidi overrides by category, and it
//     is the right trade for a name in a cross-tenant admin table: a broken family emoji
//     is cosmetic, a reordered worker name is a spoof. Refusing it on write is more
//     honest than storing a name that can only ever display wrong.
//   - Combining marks are Mn, not Cf. "Zalgo" text stays a grapheme-WIDTH problem,
//     which a width-aware layout fixes and a stripper cannot.
func Unsafe(r rune) bool {
	return unicode.IsControl(r) || unicode.In(r, unicode.Cf)
}

// SanitizeTTY strips the unsafe runes from s, while sparing \t and \n so multi-line free
// text (judge summaries, rationale blocks) still renders as the author wrote it. Use it
// for FLOWING text; use CellText for anything landing in a fixed-width column.
//
// Invalid UTF-8 becomes U+FFFD (strings.Map's decode behaviour), which is a strictly
// better thing to put on a terminal than a raw undecodable byte — and it is one of the
// four ways CellText can change its input, so Validate rejects invalid UTF-8 to match.
func SanitizeTTY(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' {
			return r
		}
		if Unsafe(r) {
			return -1
		}
		return r
	}, s)
}

// SanitizeBounded is the persist-time guard for untrusted FLOWING markdown — the worker's
// report_md and the judge's summary_md / recommendation rationale. It trims, strips the
// Unsafe runes but SPARES \n and \t so multi-line markdown survives, applies a byte bound
// AFTER each WHOLE rune (so a multi-byte rune is never split at the cut), then trims again.
//
// The skip predicate keeps \n and \t and drops everything else Unsafe classifies; because
// \n and \t are themselves control characters (Cc), the two explicit exceptions are what
// let markdown structure through while terminal escapes and bidi overrides do not.
//
// The Cf strip is Trojan-Source defense (issue #124 / CVE-2021-42574): judge/worker output
// is LLM-derived text shaped by whatever the run looked at, so a bidi override (U+202E and
// its family) could otherwise make an approving sentence render inside a rejecting review,
// or make a stored coordinate read as a different one. It consumes the accepted cost the
// Cf strip carries: a ZWJ family emoji degrades into its component glyphs (the case Unsafe
// documents), and ZWNJ (U+200C), orthographically required in some scripts, is dropped too
// — knowingly, because letting a bidi override persist at rest is the worse of the two.
//
// The \n / \t exception is exactly why this is a SEPARATE function from the single-line
// sanitizeSelfReported, whose sinks are identifiers where a newline is itself something to
// drop. The trim runs AGAIN at the end because the leading TrimSpace happens BEFORE the
// strip and Cf is not White_Space, so an edge format character shields adjacent whitespace
// from the first trim; removing the Cf then leaves that whitespace to clean up.
//
// Invalid UTF-8 becomes U+FFFD (the range decode substitutes it), matching SanitizeTTY.
func SanitizeBounded(s string, max int) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		if r != '\n' && r != '\t' && Unsafe(r) {
			continue
		}
		b.WriteRune(r)
		if b.Len() >= max {
			break
		}
	}
	return strings.TrimSpace(b.String())
}

// CellText is SanitizeTTY plus the two folds a COLUMN needs, for text landing in a table
// cell.
//
// Tab and newline are in scope here and out of scope in SanitizeTTY, because in a cell
// they are not content — they are layout control. text/tabwriter reads \t as a column
// separator and \n as a row terminator, so one embedded newline in a worker name FORGES
// A TABLE ROW in a listing an admin reads to make decisions (#169), and one embedded tab
// walks every following column right. Folding to a space keeps the word break the
// character was doing while removing its effect on the rail.
//
// It deliberately does NOT truncate. package main's cellText caps at 200 for columns
// whose width it owns; a shared boundary that silently shortened every cell would
// corrupt legitimately long values (run titles, emails) to buy nothing security-wise.
// That matters to Validate too: a length cap here would make the biconditional above
// depend on a width, and the handlers own their own caps.
func CellText(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' {
			return ' '
		}
		return r
	}, SanitizeTTY(s))
	// Folding can strand an edge space where the newline used to be.
	return strings.TrimSpace(s)
}

// Validate reports why s would not survive CellText unchanged, or nil when it would.
// field names the request field in the returned message, so one predicate serves
// "name", "client_desc" and whatever comes next without a second spelling of the rule.
//
// It is the exact complement of CellText and nothing more, which is a constraint on what
// may be added here rather than a description of what is here today. In particular it
// deliberately does NOT check:
//
//   - EMPTINESS. CellText("") == "", so an empty string is round-trip-clean and this
//     returns nil for it. Every caller already rejects empty with its own message, and
//     folding that in would break the biconditional for the one input where the two
//     questions genuinely differ.
//   - LENGTH. Same reason, plus the caps differ per field (200 bytes for a worker name,
//     64 for a token label).
//   - U+FFFD ITSELF. handler.validateSecretLabel rejects a literal replacement character
//     and this does not, and the divergence is deliberate: CellText passes an
//     already-decoded U+FFFD through unchanged, so rejecting it would break the
//     biconditional. The undecodable INPUT that produces one is rejected below.
//
// The four clauses are the four ways CellText can move: an invalid byte becomes U+FFFD,
// a control character is stripped or folded, a format character is stripped, and edge
// whitespace is trimmed. The trim clause is unreachable from today's callers, which all
// strings.TrimSpace before calling — it is here so the biconditional is a property of
// this function rather than of its callers' discipline.
func Validate(field, s string) error {
	if !utf8.ValidString(s) {
		return fmt.Errorf("%s must be valid UTF-8", field)
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return fmt.Errorf("%s must not contain control characters (tabs, newlines, "+
				"terminal escape sequences): they can rewrite or forge a line in a listing "+
				"another user reads", field)
		}
		if unicode.In(r, unicode.Cf) {
			return fmt.Errorf("%s must not contain invisible formatting characters "+
				"(zero-width spaces and joiners, bidirectional overrides, the byte-order "+
				"mark): they let two different entries look identical, or make one read as "+
				"another. This also rules out multi-part emoji such as 👨‍👩‍👧, which are "+
				"joined by one of these characters, so use a plain name instead", field)
		}
	}
	if strings.TrimSpace(s) != s {
		return fmt.Errorf("%s must not begin or end with whitespace", field)
	}
	return nil
}
