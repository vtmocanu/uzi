package uzicli

import (
	"strings"
	"unicode"
)

// This file holds the CLI's terminal-safety predicate: the one place that decides
// which runes are allowed to reach a terminal (#180). Printer.Table, Printer.Printf
// and Printer.Println run it on every human-facing byte they emit, and package main's
// sanitizeTTY delegates to SanitizeTTY so the binary carries ONE predicate rather than
// two that can drift.
//
// THREAT. A server the user pointed --url at supplies run titles, worker names, token
// labels, judge prose, error messages and build info, and the CLI printed all of it
// raw. `\x1b[2J\x1b[1;1H` clears the screen and homes the cursor; `\x1b]0;…\x07`
// rewrites the window title; a bidi override reorders a name so it READS as another
// one. Impact is terminal display manipulation — hiding or spoofing prior output —
// not code execution, and it needs a hostile or compromised server, which the
// credentialSafeBase scheme guard still constrains to https:// or loopback.
//
// STRIP, NOT ESCAPE, and the reasoning matters because escaping is the better default
// for a security-relevant renderer in general: it makes tampering visible instead of
// silent. Three things outweigh that here.
//
//  1. The house predicate already ships and already strips (sanitizeTTY, PRD #112 M3).
//     A boundary that ESCAPED underneath call sites that STRIP would produce output
//     matching neither, and their tests assert on stripped text.
//  2. Escaping expands. A label of 200 escapes becomes 800 columns and shreds the
//     tabwriter rail — which is itself an attack surface (#169: a forged table row in
//     a listing an admin reads to make decisions). The cure re-introduces the layout
//     half of the disease.
//  3. The visibility argument is already served, better, by a channel that exists:
//     --json escapes control bytes losslessly (ESC arrives as the six characters
//     backslash-u-0-0-1-b) and is one flag away. Making the human table a second,
//     worse forensic channel buys nothing.
//
// TARGET IS CONTROL CHARACTERS, NOT "NON-ASCII". Both tests below are Unicode CATEGORY
// predicates over runes, so accented Latin, CJK and emoji pass through byte-identical
// (measured). The one real casualty is stated in SanitizeTTY's own comment.

// SanitizeTTY strips the control characters that let untrusted text drive a terminal,
// while sparing \t and \n so multi-line free text (judge summaries, rationale blocks)
// still renders as the author wrote it. Use it for FLOWING text; use CellText for
// anything landing in a fixed-width column.
//
// The pair of predicates is deliberately the same one workersvc.hasUnsafeChar settled
// on. unicode.IsControl covers C0 (0x00-0x1F), C1 (0x80-0x9F) and DEL (0x7F) — which a
// hand-rolled `r < 0x20` range test misses. unicode.In(r, unicode.Cf) covers the format
// characters no range test can enumerate: the bidi overrides U+202A-202E, the isolates
// U+2066-2069, U+200F, the zero-widths, the BOM and SHY. The bidi half is the one that
// matters most — it visually reorders text, so a name can be made to read as another.
//
// TWO THINGS IT DOES NOT DO, so nobody reads it as more than it is:
//
//   - It BREAKS ZWJ EMOJI SEQUENCES. U+200D is Cf, so "👨‍👩‍👧" renders as three separate
//     emoji rather than one family glyph (measured, not inferred; U+FE0F is Mn and
//     survives, so ordinary variation-selector emoji are untouched). That is the price
//     of stripping the bidi overrides by category, and it is the right trade for a
//     terminal: a broken family emoji is cosmetic, a reordered worker name is a spoof.
//   - Combining marks are Mn, not Cf. "Zalgo" text stays a grapheme-WIDTH problem,
//     which a width-aware layout fixes and a stripper cannot.
//
// Invalid UTF-8 becomes U+FFFD (strings.Map's decode behaviour), which is a strictly
// better thing to put on a terminal than a raw undecodable byte.
func SanitizeTTY(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' {
			return r
		}
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
			return -1
		}
		return r
	}, s)
}

// CellText is SanitizeTTY plus the two folds a COLUMN needs, for text landing in a
// table cell.
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
