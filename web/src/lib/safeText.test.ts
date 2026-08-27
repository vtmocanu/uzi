import { describe, it, expect } from "vitest";
import { readFileSync } from "node:fs";
import { stripUnsafeChars } from "./safeText";

// Issue #124. The corpus below deliberately MIRRORS the one the api pins for `sanitizeTTY`
// (api/cmd/uzi/tui_render_test.go:61), which is the scrubber this util converges on — a
// renderer scrub that strips and spares `\t`/`\n`, not the `hasUnsafeChar` input gate that
// rejects and has no whitespace exception. Convergence is the point: two answers to "which
// characters are unsafe in untrusted display text" is how one surface quietly stops
// matching the other, and mirroring the corpus is what keeps the two answers observably
// equal rather than merely intended to be.
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
    // The ESC BYTE is what makes a sequence an escape sequence; stripping it leaves "[2J"
    // as inert literal text. That is correct, and it is what sanitizeTTY's own case asserts
    // — an earlier version of THAT test expected "ab" here and was pinning a behaviour the
    // scrubber does not and should not have.
    expect(stripUnsafeChars("a\u001B[2Jb")).toBe("a[2Jb");
    expect(stripUnsafeChars(`${ESC}[31mALERT${ESC}[0m`)).toBe("[31mALERT[0m");
    expect(stripUnsafeChars("a\u0000b\u0007c\u007Fd")).toBe("abcd");
    // C1 as a RUNE (U+009B), not as a raw byte: the raw byte is invalid UTF-8 and decodes
    // to U+FFFD, a printable replacement char. Both predicates operate on decoded code
    // points. U+0085 and U+009F bracket the block.
    expect(stripUnsafeChars("a\u009Bb")).toBe("ab");
    expect(stripUnsafeChars("a\u0085b\u009Fc")).toBe("abc");
  });

  it("removes the directional MARKS, not only the overrides (sanitizeTTY's RTL-mark case)", () => {
    // U+200E/U+200F are Cf too and are the subtlest of the family: no visible glyph, no
    // paired terminator, and they still flip the resolved direction of neutral runs.
    expect(stripUnsafeChars("a\u200Fb")).toBe("ab");
    expect(stripUnsafeChars("a\u200Eb")).toBe("ab");
  });

  it("PROPERTY: nothing that survives is Cc or Cf, whatever went in", () => {
    // Asserted independently of any per-case expectation, mirroring sanitizeTTY's own
    // property loop. A per-case table can only ever cover the codepoints someone thought
    // of; this covers the category, which is what the predicate actually claims.
    for (const input of ["a\u001B[2Jb", "a\u009Bb", "a\u007Fb", "a\u202Eb", "a\uFEFFb", "\u009B\uFFFD", ""]) {
      for (const ch of stripUnsafeChars(input)) {
        expect(/[\p{Cc}\p{Cf}]/u.test(ch), `${JSON.stringify(input)} let ${JSON.stringify(ch)} through`).toBe(false);
      }
    }
  });

  it("keeps code points that are merely NOT ASSIGNED, which is what rules out \\p{C}", () => {
    // The discriminating case, and it exists because a control found the corpus could not
    // see this: swapping the predicate for `\p{C}` passed all ten other assertions. `C` is
    // Cc|Cf|Cn|Co|Cs, so it also swallows private-use and unassigned code points — text
    // that is legitimate, merely unknown to this engine's Unicode table.
    //
    // U+E000 is the private-use area, which Unicode will never assign, so this fixture
    // cannot rot. Measured: the shipped predicate keeps it; `\p{C}` yields "ab".
    expect(stripUnsafeChars("a\uE000b")).toBe("a\uE000b");
    expect(stripUnsafeChars("a\uF8FFb")).toBe("a\uF8FFb");
    // …and a currently-unassigned code point likewise. Weaker as a fixture (a future
    // Unicode revision could assign it), so it rides alongside the PUA case, not instead.
    expect(stripUnsafeChars("a\u{E0002}b")).toBe("a\u{E0002}b");
    // But U+E0001 LANGUAGE TAG is genuinely Cf and must still go — the point is precision,
    // not leniency.
    expect(stripUnsafeChars("a\u{E0001}b")).toBe("ab");
  });

  it("passes astral-plane code points through, which catches an over-broad predicate", () => {
    // U+1D11E MUSICAL SYMBOL G CLEF, sanitizeTTY's own passthrough case. It is a surrogate
    // PAIR in JS, so a predicate written over UTF-16 code units rather than code points —
    // or one reaching for `\p{C}`, which swallows unassigned — mangles it here and nowhere
    // else in this file.
    expect(stripUnsafeChars("a\u{1D11E}b")).toBe("a\u{1D11E}b");
    expect([...stripUnsafeChars("\u{1D11E}")].length).toBe(1);
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

describe("shared termsafe corpus", () => {
  // Issue #161. The shared golden corpus (fixtures/termsafe/corpus.json) is loaded by BOTH
  // this suite and the Go termsafe test (api/internal/termsafe/termsafe_test.go), pinning the
  // JS answer to the same classification the api uses so the two independent implementations
  // cannot drift. Anchored to THIS file via import.meta.url — a BARE relative fs read would
  // resolve from process.cwd() (web/), overshooting the repo root.
  const corpus = JSON.parse(
    readFileSync(new URL("../../../fixtures/termsafe/corpus.json", import.meta.url), "utf8"),
  ) as { codepoints: { cp: string; name: string; category: string; unsafe: boolean }[] };

  // The same count guard lives in the Go test. Its purpose is to RED on an add/remove so a
  // change to the shared corpus is a deliberate, acknowledged act on both sides — bump both
  // when you intentionally add or remove a code point.
  const EXPECTED = 36;

  it("has the pinned number of shared code points, both classes present", () => {
    expect(corpus.codepoints.length).toBe(EXPECTED);
    expect(corpus.codepoints.some((e) => e.unsafe)).toBe(true);
    expect(corpus.codepoints.some((e) => !e.unsafe)).toBe(true);
  });

  it("strips exactly the corpus's unsafe code points via the production stripUnsafeChars", () => {
    // The REAL web-side pin: assert the PRODUCTION scrubber, so dropping Cf (or any category)
    // from safeText.ts's UNSAFE_CHAR reds here, mirroring how the Go test pins termsafe.Unsafe.
    // The corpus omits \n/\t/\r, so stripUnsafeChars's whitespace exception never applies and
    // "unsafe" == "stripped" holds for every entry.
    for (const e of corpus.codepoints) {
      const ch = String.fromCodePoint(parseInt(e.cp, 16));
      expect(stripUnsafeChars(`a${ch}b`), `U+${e.cp} ${e.name}`).toBe(e.unsafe ? "ab" : `a${ch}b`);
    }
  });

  it("labels each code point by the same Cc/Cf category rule the api uses", () => {
    // Secondary: the corpus's own `unsafe` column matches JS Unicode category semantics
    // /[\p{Cc}\p{Cf}]/u — the rule stripUnsafeChars is built from — keeping the fixture honest
    // independently of the production regex asserted above.
    for (const e of corpus.codepoints) {
      const ch = String.fromCodePoint(parseInt(e.cp, 16));
      expect(/[\p{Cc}\p{Cf}]/u.test(ch), `U+${e.cp} ${e.name}`).toBe(e.unsafe);
    }
  });
});
