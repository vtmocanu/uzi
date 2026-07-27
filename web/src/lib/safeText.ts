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
// The predicate CONVERGES on the one the api already holds untrusted repo-agent
// descriptions to — `hasUnsafeChar` in api/internal/workersvc/agent_selection.go:236,
// `unicode.IsControl(r) || unicode.In(r, unicode.Cf)` — because there should be exactly one
// answer in this codebase to "which characters are unsafe in untrusted display text". Two
// deliberate differences, both forced by this being a different KIND of boundary:
//
//   * It STRIPS rather than rejects. hasUnsafeChar guards an input gate, where refusing the
//     whole value is the right answer. This is a renderer: a review the user already paid
//     for must still be readable, and there is nothing to refuse to.
//   * It KEEPS `\n` and `\t`. The surfaces are `whitespace-pre-wrap`, so dropping them
//     would mangle legitimate multi-line judge output. This matches the api's OTHER
//     scrubber, sanitizeReviewText (handler/judge_worker.go:381), which already exempts
//     both at review ingest.
//
// WHY THE WEB SIDE AT ALL, given the api scrubs at ingest: sanitizeReviewText drops
// `IsControl` only. Cc and Cf are disjoint categories, so every character named above
// survives ingest today and reaches the browser. Closing it at ingest as well is the right
// end state and does not make this redundant — a review stored before that lands still
// renders through here.
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
