import { describe, it, expect } from "vitest";
import { stripUnsafeChars } from "./safeText";

// Issue #124. The corpus below is deliberately the SAME set the api pins for its own
// untrusted-text predicate (api/internal/workersvc/agent_selection_test.go:98-110 — an ANSI
// escape, U+202E, a zero-width joiner), extended with the rest of the class the issue names.
// Convergence is the point: two answers to "which characters are unsafe in untrusted
// display text" is how one surface quietly stops matching the other.
//
// EVERY hostile character here is written as a `\u` ESCAPE, never as a literal. The api's
// Go fixture makes the opposite choice and can afford to; this file cannot, for two reasons
// that are both about the file being read rather than run. `sourceBytes.test.ts` fails the
// suite on a literal NUL anywhere under web/src, because git then classifies the file as
// binary and its diff stops existing (PRD #98 review B1) — measured here: the first cut of
// this file used literals and reddened that gate. And a literal U+202E would reorder THIS
// source in the reviewer's editor, which is the very attack under test.
const RLO = "\u202E"; // RIGHT-TO-LEFT OVERRIDE
const ZWSP = "\u200B"; // ZERO WIDTH SPACE
const ESC = "\u001B"; // the ANSI-escape introducer

describe("stripUnsafeChars", () => {
  it("removes the bidi overrides and isolates that reorder what a human reads", () => {
    // Trojan Source (CVE-2021-42574). U+202E is category Cf, NOT Cc, so an IsControl-only
    // scrub — which is exactly what the api runs at review ingest — lets it straight
    // through. This is the codepoint the whole issue is about.
    expect(stripUnsafeChars(`safe${RLO}dnammoc`)).toBe("safednammoc");
    // The embedding/override quartet (U+202A–202D) and the isolates (U+2066–2069): every
    // one of them changes rendering order, so covering only U+202E leaves the class open.
    for (const cp of [0x202a, 0x202b, 0x202c, 0x202d, 0x202e, 0x2066, 0x2067, 0x2068, 0x2069]) {
      const r = String.fromCodePoint(cp);
      expect(stripUnsafeChars(`a${r}b`), `U+${cp.toString(16).toUpperCase()} must not survive`).toBe("ab");
    }
  });

  it("removes the zero-width characters that also defeat search over the rendered text", () => {
    // U+200B ZWSP, U+200C ZWNJ, U+200D ZWJ, U+FEFF BOM, U+00AD SOFT HYPHEN. A reader
    // cannot find text that is on screen once one of these sits inside a word.
    for (const cp of [0x200b, 0x200c, 0x200d, 0xfeff, 0x00ad]) {
      expect(stripUnsafeChars(`sh${String.fromCodePoint(cp)}ellcheck`)).toBe("shellcheck");
    }
  });

  it("removes control characters, including the ANSI escape the api's corpus names", () => {
    expect(stripUnsafeChars(`${ESC}[31mALERT${ESC}[0m`)).toBe("[31mALERT[0m");
    expect(stripUnsafeChars("a\u0000b\u0007c\u007Fd")).toBe("abcd");
    // C1 (U+0080–U+009F) is inside Go's IsControl domain — its implementation stops at
    // Latin-1 — so it is inside ours.
    expect(stripUnsafeChars("a\u0085b\u009Fc")).toBe("abc");
  });

  it("PRESERVES newline and tab, which the pre-wrap surfaces need", () => {
    // Stripping these would mangle legitimate multi-line judge output. This matches the
    // api's own review-ingest scrubber, which exempts both.
    expect(stripUnsafeChars("line one\nline two\tcolumn")).toBe("line one\nline two\tcolumn");
    expect(stripUnsafeChars("\n\t")).toBe("\n\t");
    // …but not carriage return, which the api drops too.
    expect(stripUnsafeChars("a\r\nb")).toBe("a\nb");
  });

  it("leaves ordinary text — including non-Latin scripts and emoji — byte-identical", () => {
    // The rule is a category test, not an ASCII allowlist: legitimate Arabic or Hebrew
    // judge output must survive intact, or the fix trades one unreadable surface for
    // another. (Both scripts are RTL natively; it is the OVERRIDE characters that lie,
    // not the letters.)
    for (const s of [
      "shellcheck",
      "api/internal/forge/gitlab.go",
      "مرحبا", // Arabic, natively RTL
      "עברית", // Hebrew, natively RTL
      "日本語",
      "🙂 ok", // an astral-plane emoji: surrogate pairs must not be split
      "a — b",
    ]) {
      expect(stripUnsafeChars(s)).toBe(s);
    }
  });

  it("is a no-op on the empty string and on text with nothing to strip", () => {
    expect(stripUnsafeChars("")).toBe("");
    const clean = "Lost time to a missing tool.";
    expect(stripUnsafeChars(clean)).toBe(clean);
  });

  it("removes EVERY occurrence, not just the first (the regex is global)", () => {
    // A `replace` without /g fixes the first character and ships the rest, which reads as
    // a working fix in any test that plants exactly one.
    expect(stripUnsafeChars(`${RLO}a${RLO}b${ZWSP}c`)).toBe("abc");
  });
});
