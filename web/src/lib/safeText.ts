// Issue #124: strip Unicode format characters out of untrusted free text on its way to a
// renderer.
//
// React escapes HTML, and RunView.test.tsx pins that — but escaping is not the whole
// threat. The browser implements the bidirectional algorithm, so U+202E RIGHT-TO-LEFT
// OVERRIDE and its relatives reorder what a human READS while leaving the string bytes
// alone. Judge output is LLM-derived text influenced by whatever the run looked at, so an
// approving sentence can be made to render inside a rejecting review, or a recommendation
// target made to visually name a different file than the one it points at. That is Trojan
// Source (CVE-2021-42574) in the review surface. Zero-width characters (U+200B, the ZWJ/ZWNJ
// pair, U+FEFF, U+00AD SOFT HYPHEN) are the same category and additionally defeat search
// over the rendered string: the reader cannot find text that is on screen.
//
// The predicate CONVERGES on `sanitizeTTY` (api/cmd/uzi/run.go:524), because there should
// be exactly one answer in this codebase to "which characters are unsafe in untrusted
// display text" — and sanitizeTTY is the RIGHT copy of that answer to converge on, not
// `hasUnsafeChar` (workersvc/agent_selection.go:236). Both share the predicate
// `unicode.IsControl(r) || unicode.In(r, unicode.Cf)`; the difference is what they do with
// it, and this surface needs sanitizeTTY's half:
//
//   * sanitizeTTY STRIPS and already spares `\t` and `\n`. That is a RENDERER scrub, which
//     is what this is: a review the user already paid for must stay readable, there is
//     nothing to refuse to, and the surfaces are `whitespace-pre-wrap` so dropping the two
//     whitespace characters would mangle legitimate multi-line judge output.
//   * hasUnsafeChar REJECTS the whole value and has no whitespace exception, because it
//     guards an input gate that does not need one. Re-deriving the exception from it would
//     be inventing a third rule and calling it convergence.
//
// The corpus in safeText.test.ts deliberately mirrors sanitizeTTY's own
// (api/cmd/uzi/tui_render_test.go:61), including its two controls: a property loop
// asserting nothing that survives is Cc or Cf, and an astral-plane passthrough that catches
// an over-broad predicate.
//
// WHY THE WEB SIDE AT ALL: the two review-POST ingest scrubbers — `sanitizeReviewText`
// (handler/judge_worker.go) for summary/rationale, `sanitizeSelfReported`
// (handler/worker_protocol.go) for `target` and `judge_model` — WERE `IsControl`-only, and
// Cc and Cf are disjoint, so every character named above survived ingest and was persisted.
// They strip Cf now too. That does not make this redundant: rows written BEFORE that still
// render here, and no ingest fix can reach them.
//
// NOT applied at the API boundary, deliberately. `target` is a COORDINATE: the page matches
// dispositions and filed issues by (category, target) and posts that pair back. Normalizing
// it on the way in would send a value the server cannot find. So this is a display-time
// transform, applied per render site, and the raw value stays the identity.

/**
 * Cc (control) plus Cf (format), except the two whitespace characters the pre-wrap
 * surfaces need. `\p{Cc}` is exactly Go's `unicode.IsControl` domain: C0, DEL and C1, all
 * of which are Latin-1, which is where Go's implementation stops looking.
 *
 * The negative lookahead is what carves out `\n` and `\t`: a character class cannot
 * subtract, and enumerating the surviving Cc ranges by hand invites an off-by-one on the
 * C1 block.
 *
 * THREE WAYS TO GET THIS WRONG, none of which throws. Measured on node 26, on the literal
 * string `<RLO>a<RLO>b<ZWSP>c`, against which the shipped form yields `"abc"`:
 *
 *   * Without `u` this is not a property escape at all — it degrades to a character class
 *     of the LITERAL characters `p { C c } f`. That is worse than the no-op it looks like:
 *     it strips none of the hostile input and eats ordinary letters, yielding
 *     `"<RLO>a<RLO>b<ZWSP>"` — the `c` is gone and every override survived. (Confirmed
 *     directly: `/[\p{Cc}]/g.test("pCc")` is `true` and `/[\p{Cc}\p{Cf}]/g.test("\u202E")`
 *     is `false`.)
 *   * Without `g` only the first offender goes: `"a<RLO>b<ZWSP>c"`. That reads as a working
 *     fix in any test that plants exactly one, which is why there is a case for it below.
 *   * `\p{C}` would be over-broad — it is `Cc|Cf|Cn|Co|Cs`, so it also swallows PRIVATE-USE
 *     (`Co`) and unassigned (`Cn`) code points: legitimate text merely unknown to this
 *     engine's Unicode table. `Co` is the load-bearing half of the corpus, because Unicode
 *     will never assign the private-use area, so that fixture cannot rot the way a
 *     currently-unassigned code point can. Measured on `a\uE000b`: this form keeps it,
 *     `\p{C}` yields `"ab"`.
 */
const UNSAFE_CHAR = /(?![\n\t])[\p{Cc}\p{Cf}]/gu;

/**
 * Remove characters that change how text READS without changing what it says.
 *
 * Lone surrogates (Cs) are NOT removed: the api's predicate does not cover them either,
 * and convergence is worth more here than an extra category that no judge output produces
 * and that React renders as a replacement character anyway.
 */
export function stripUnsafeChars(s: string): string {
  return s.replace(UNSAFE_CHAR, "");
}
