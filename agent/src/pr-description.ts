// PRD #1798 D5/D7/D8/D10/D12/D14/D15: the pure renderer of uzi's two owned PR-description blocks.
//
// uzi owns exactly two marked blocks in a PR body (D10):
//
//   - the DESCRIPTION REGION (`<!-- uzi:description:start v1 -->` … `<!-- uzi:description:end -->`):
//     the plain-English summary, the deterministic size line, "What changed", "Verification",
//     "Scope and review notes" and the provenance line (renderRegion);
//   - the COMPLETION BLOCK (`<!-- uzi:completion:start v1 -->` … `<!-- uzi:completion:end -->`): the
//     deterministic text the completion interlock owns (renderCompletionBlock): `Closes #N` only
//     when told, `Related to #N.`, the partial / accepted warnings (PRD #1227), the completion-
//     unverified banner, the gates-unverified section, the history-bridge sentence (PRD #1416), the
//     agents line, the per-kind one-liner (D14), the staleness line (D12) and the footer.
//
// Everything outside both blocks belongs to humans and other bots and is preserved byte-for-byte
// (parseOwnedBlocks / composeBody). No I/O happens here: the publisher (next unit) reads the forge,
// stages / binds / acks through the api and writes.
//
// Three kinds of text reach a block, and they are escaped differently:
//
//   - API-SANITIZED fields (summary, changes, scope notes, review pointers, verification commands):
//     already escaped by the api's sanitizer (D7: markdown block syntax, `<`, links, mentions,
//     closing keywords broken by U+200B). They are published AS RETURNED, never re-escaped and never
//     Cf-stripped (stripping the U+200B breakers would re-form the directives). The renderer accepts
//     them only as a real SanitizedPrDescriptionFields instance (checked at runtime with `is()`).
//   - DETERMINISTIC interpolations (branch names, milestone ids / titles / reasons, accepted criteria,
//     gate directories, the ci_fix pipeline ref, the changed paths a kind section lists): run data the
//     api never sanitized, some of it attacker- or owner-typed. They go through escapeInline
//     (markdown escaping, `@` and closing-keyword neutralisation with U+200B like the api, a leading
//     `/`) or codeSpan (a code span whose closing keywords, mentions and `<!--` are broken the same
//     way). A kind section (KindSection) carries them in typed slots that this module escapes; its
//     fixed lines are the worker's own wording, and are still neutralised line by line.
//   - FIXED wording of this module and its callers (the footer, the agents line, the partial
//     warnings, the unverified banner).
//
// closingDirectiveFor is the whole-body scan behind the interlock (D10 rule 1; the interlock runs it
// through closingDirectiveOutsideCompletion, which leaves out uzi's own completion block only when it
// is identical to the block uzi rendered, line endings aside): the api sanitizer's prDescClosing
// pattern (with Forgejo's `!N` references), extended to GitLab's reference lists (`Closes #1, #2 and
// #3`), over several views of the body (the raw text, a RENDERED view that mirrors the api's
// prDescNormalize, a decoded-raw view, a tag-stripped view and the two composed into one), each
// in the api's three marker views. Where the views could disagree with a forge it errs towards
// "closing".
//
// Threat model. The scan detects closing directives present in the body when uzi writes it: plainly
// written ones, the api sanitizer's normalised forms, and common markup and entity splits (a keyword
// split by an entity, a tag, a comment, a processing instruction, CDATA, a declaration or an empty
// link). It is NOT a guarantee against deliberate obfuscation by someone with edit rights on the PR
// description: that person can add a plain `Closes #N` at any time after uzi's last write, so the scan
// cannot be a boundary against them. Anyone without edit rights reaches the body only through the
// api sanitizer (model and lead text) and this module's escaping (deterministic interpolations), D7.
// Known gaps, documented and accepted (pinned in the hardening tests), including: a link destination
// with balanced parentheses, escapes, a quoted `)` in its title or a pointy destination holding `)`
// (`[Fix](a(b)c)es #7`, `[Fix](<1)>)es #7`); a custom tag with a quoted `>` in an attribute
// (`Fix<foo title="a>b">es #7`); and, in the block parser rather than the scan, fenced code opened
// inside a list item (codeRanges: a marker pair a forge shows as code can still be adopted, which
// never hides a directive from the interlock, see codeRanges).

import { createHash } from "node:crypto";
import { SanitizedPrDescriptionFields } from "./client.js";
import type { ClaimConfig, PrDescriptionSource } from "./protocol.js";

// ── Markers ─────────────────────────────────────────────────────────────────────────────────

export const REGION_START = "<!-- uzi:description:start v1 -->";
export const REGION_END = "<!-- uzi:description:end -->";
export const COMPLETION_START = "<!-- uzi:completion:start v1 -->";
export const COMPLETION_END = "<!-- uzi:completion:end -->";
/** The staleness line's inner marker pair, inside the completion block (D12). */
export const STALENESS_START = "<!-- uzi:staleness -->";
export const STALENESS_END = "<!-- /uzi:staleness -->";

/** D15: the conservative whole-body cap (GitHub's description limit, applied to every forge),
 *  in UTF-16 code units, which is never fewer than the characters a forge counts. */
export const BODY_CAP_CHARS = 65_536;
/** The rendered region cap, in UTF-8 bytes, markers included: over it the region falls back to the
 *  size line and provenance only. */
export const REGION_CAP_BYTES = 6 * 1024;

const ZWSP = "\u200B";

// ── Closing directives (port of api/internal/workersvc/pr_description_sanitize.go) ──────────

/** The largest description a forge accepts (GitLab's 1,048,576-character limit, the largest of the
 *  three forges), in UTF-16 code units. Longer text cannot be a forge body: closingDirectiveFor
 *  reports it as closing and parseOwnedBlocks as malformed, so no scan runs over it (fail closed). */
export const FORGE_BODY_MAX_CHARS = 1_048_576;

// The keyword half of prDescClosing, verbatim (group 1 of the Go pattern). `\b` on both sides.
const KEYWORD_SRC = String.raw`\b(clos(?:e[sd]?|ing)|fix(?:e[sd]|ing)?|resolv(?:e[sd]?|ing)|implement(?:s|ed|ing)?)\b`;
// One reference, the alternatives of prDescClosing's last group, each capturing what resolution
// needs (refResolves): 1 = `#N` / `!N`, 2 = `gh-N`, 3/6 = `path#N` / `path!N`, 7 = an external-
// tracker key's number, 8/9 = an issue or work-item URL (the part before `/issues|work_items/`, and
// N); 4 and 5 are internal. Forgejo reads `!N` after a close keyword and numbers issues and pull
// requests together, so `Fixes !7` closes issue 7. Bounds keep every scan linear in V8's
// backtracking engine (the api's RE2 is linear without them): a path segment is at most 255
// characters (no forge allows a longer namespace or project name) and is matched atomically (the
// `(?=(x))\N` idiom: a segment is always followed by `/`, `#` or `!`, none of which it can hold,
// so no shorter segment could match); a path has at most 22 segments (a GitLab group may have up to
// 20 ancestor groups, so a project path is at most 21 groups and the project: GitHub and Forgejo
// allow 2), so the path alternative reads at most 22 * 256 characters from any start; and a URL's
// part before `/issues/` is at most 300 characters (GitLab's own bound). Groups are numbered
// absolutely: no pattern embedding REF_SRC captures before it.
//
// That bound is still a large constant per keyword: in a keyword-dense chain (`fix-fix-…/`, a
// keyword every four characters) every keyword's path alternative reads the rest of the chain.
// So closingDirectiveFor never runs the path alternative as a regex after a keyword: PathRefs
// decides it in O(1) per start from tables built once per view, and firstRef runs REF_NUM_SRC and
// REF_TAIL_SRC (the alternatives before and after it) on their own, in the same order. A reference
// list's continuation (NEXT_REF_RE) keeps the whole regex: it starts after a space or a comma,
// which no path holds, so no two continuations read the same path.
const REF_NUM_SRC = String.raw`[#!](\d+)|gh-(\d+)`;
const REF_PATH_SRC = String.raw`((?=([\w.-]{1,255}))\4(?:\/(?=([\w.-]{1,255}))\5){0,21})[#!](\d+)`;
const REF_TAIL_SRC = String.raw`[A-Za-z][A-Za-z0-9_]+-(\d+)|(https?:\/\/[^\s<>()]{0,300}?)\/(?:issues|work_items)\/(\d+)`;
const REF_SRC = `(?:${REF_NUM_SRC}|${REF_PATH_SRC}|${REF_TAIL_SRC})`;
/** REF_SRC's first two alternatives alone (groups: 1 = `#N` / `!N`, 2 = `gh-N`). */
const REF_NUM_RE = new RegExp(`(?:${REF_NUM_SRC})`, "iuy");
/** REF_SRC's last two alternatives alone (groups: 1 = a tracker key's number, 2/3 = a URL's part
 *  before `/issues|work_items/`, and N). */
const REF_TAIL_RE = new RegExp(`(?:${REF_TAIL_SRC})`, "iuy");
const SP = String.raw`[\s\p{Z}]`;
const KEYWORD_RE = new RegExp(KEYWORD_SRC, "giu");
/** closingDirectiveFor's own copy, driven by exec and lastIndex: an early return leaves its
 *  lastIndex set, which String.prototype.matchAll would inherit if the two shared one regex. */
const KEYWORD_SCAN_RE = new RegExp(KEYWORD_SRC, "giu");
/** The first reference after a keyword, prDescClosing's tail (sticky at the keyword end). The Go
 *  `SP*:?SP*` is spelled `SP*(?::SP*)?` (the same language): two adjacent stars over one class
 *  backtrack quadratically on a long space run in V8. breakClosingKeywords runs it over short
 *  deterministic strings; closingDirectiveFor runs the same match as firstRef. */
const FIRST_REF_RE = new RegExp(`${SP}*(?::${SP}*)?(?:issues?${SP}*)?${REF_SRC}`, "iuy");
/** A further reference of a GitLab reference list (`#1, #2 and issue #3`): a comma and/or `and`
 *  separator, or plain whitespace, then an optional `issue(s)`. */
const NEXT_REF_RE = new RegExp(`(?:${SP}*,${SP}*(?:and${SP}+)?|${SP}+and${SP}+|${SP}+)(?:issues?${SP}*)?${REF_SRC}`, "iuy");

/** The characters a closing-directive VIEW removes or blanks (prDescClosingMarkers). */
const CLOSING_MARKERS = "*_~`[]\\";
const CLOSING_MARKERS_RE = /[*_~`[\]\\]/gu;

interface View {
  text: string;
  /** Index in the original string of each view index (undefined ⇒ identity). */
  map?: number[];
}

/** The three views the api scans (neutralizeClosingDirectives): the text itself, the markers
 *  REMOVED (`Fix**es** #12` reads `Fixes #12`), and the markers BLANKED (`a_Fixes #12`). `withMap`
 *  adds the removed view's index map (breakClosingKeywords maps a match back; the scan needs none). */
function closingViews(s: string, withMap = false): View[] {
  if (!withMap) return [{ text: s }, { text: s.replace(CLOSING_MARKERS_RE, "") }, { text: s.replace(CLOSING_MARKERS_RE, " ") }];
  let removed = "";
  const map: number[] = [];
  let blanked = "";
  for (let i = 0; i < s.length; i++) {
    const c = s[i]!;
    if (CLOSING_MARKERS.includes(c)) {
      blanked += " ";
      continue;
    }
    blanked += c;
    removed += c;
    map.push(i);
  }
  return [{ text: s }, { text: removed, map }, { text: blanked }];
}

// ── The rendered view (a mirror of the api's prDescNormalize) ──

/** prDescMaxPasses: every fixed-point loop is bounded; input that has not converged by then is
 *  treated as closing (the api sanitizes it to ""). */
const MAX_PASSES = 8;

// The named entities whose character can matter to a closing directive or to the markup the view
// removes: spaces, `#` / `!`, path and URL punctuation, the view's marker characters, `<` / `>` /
// `&`, and the invisible format characters (U+200B among them; it stays, see below). A name not
// here is left as written, which only keeps text the forge would show differently (letters and
// digits have no named entity).
const NAMED_ENTITIES: Readonly<Record<string, string>> = {
  amp: "&", AMP: "&", lt: "<", LT: "<", gt: ">", GT: ">", quot: '"', QUOT: '"', apos: "'",
  num: "#", excl: "!", sol: "/", colon: ":", period: ".", comma: ",", semi: ";", lowbar: "_", UnderBar: "_",
  ast: "*", midast: "*", grave: "`", DiacriticalGrave: "`", lsqb: "[", lbrack: "[", rsqb: "]", rbrack: "]",
  bsol: "\\", commat: "@", lpar: "(", rpar: ")", equals: "=", plus: "+", verbar: "|", vert: "|", VerticalLine: "|",
  lcub: "{", lbrace: "{", rcub: "}", rbrace: "}", dollar: "$", percnt: "%", quest: "?", Hat: "^",
  nbsp: "\u00A0", NonBreakingSpace: "\u00A0", ensp: "\u2002", emsp: "\u2003", emsp13: "\u2004", emsp14: "\u2005",
  numsp: "\u2007", puncsp: "\u2008", thinsp: "\u2009", ThinSpace: "\u2009", hairsp: "\u200A", VeryThinSpace: "\u200A",
  MediumSpace: "\u205F", ThickSpace: "\u205F\u200A", NewLine: "\n", Tab: "\t",
  shy: "\u00AD", zwnj: "\u200C", zwj: "\u200D", lrm: "\u200E", rlm: "\u200F", NoBreak: "\u2060",
  ApplyFunction: "\u2061", af: "\u2061", InvisibleTimes: "\u2062", it: "\u2062", InvisibleComma: "\u2063", ic: "\u2063",
  ZeroWidthSpace: "\u200B", NegativeVeryThinSpace: "\u200B", NegativeThinSpace: "\u200B",
  NegativeMediumSpace: "\u200B", NegativeThickSpace: "\u200B",
};
// The legacy names HTML (and Go's html.UnescapeString) also decode with no `;`.
const LEGACY_ENTITIES = ["amp", "AMP", "lt", "LT", "gt", "GT", "quot", "QUOT", "nbsp", "shy"];
const ENTITY_RE = /&(?:#([0-9]+)|#[xX]([0-9A-Fa-f]+)|([A-Za-z][A-Za-z0-9]{0,31}))(;?)/gu;

function codePointText(cp: number): string {
  if (!Number.isFinite(cp) || cp === 0 || cp > 0x10ffff || (cp >= 0xd800 && cp <= 0xdfff)) return "\uFFFD";
  return String.fromCodePoint(cp);
}

function decodeEntitiesOnce(s: string): string {
  return s.replace(ENTITY_RE, (all, dec: string | undefined, hex: string | undefined, name: string | undefined, semi: string) => {
    if (dec !== undefined) {
      const d = dec.replace(/^0+/u, "");
      return codePointText(d.length > 8 ? Infinity : Number(d || "0")) ;
    }
    if (hex !== undefined) {
      const h = hex.replace(/^0+/u, "");
      return codePointText(h.length > 7 ? Infinity : Number.parseInt(h || "0", 16));
    }
    const n = name!;
    if (semi && Object.hasOwn(NAMED_ENTITIES, n)) return NAMED_ENTITIES[n]!;
    // No `;` (or an unknown name): the longest legacy name that prefixes it, as HTML decodes.
    let best = "";
    for (const l of LEGACY_ENTITIES) if (n.startsWith(l) && l.length > best.length) best = l;
    return best ? NAMED_ENTITIES[best]! + all.slice(1 + best.length) : all;
  });
}

/** decodePrDescEntities: decode until the text stops changing, within MAX_PASSES passes. */
function decodeEntities(s: string): string {
  for (let i = 0; i < MAX_PASSES && s.includes("&"); i++) {
    const u = decodeEntitiesOnce(s);
    if (u === s) return s;
    s = u;
  }
  return s;
}

/** termsafe.SanitizeBounded's strip (controls but `\n` / `\t`, and every format character), EXCEPT
 *  U+200B: the api inserts it as its closing-keyword breaker and every forge reads it as breaking
 *  the keyword, so keeping it is what lets a sanitized field read as non-closing here. */
function stripFormat(s: string): string {
  return s.replace(/[\p{Cc}\p{Cf}]/gu, (c) => (c === "\n" || c === "\t" || c === ZWSP ? c : ""));
}

/** The index of the next `ch` at or after each position (length n + 2, `n` when there is none), so
 *  a scanner can ask "where does this bracket close" in O(1) instead of rescanning. */
function nextIndex(s: string, ch: string): Int32Array {
  const n = s.length;
  const out = new Int32Array(n + 2).fill(n);
  for (let i = n - 1; i >= 0; i--) out[i] = s[i] === ch ? i : out[i + 1]!;
  return out;
}

// A line that ends a paragraph for link purposes (a forge forms no link or image across it): a
// blank line (inside block quotes too), or a line that starts an HTML block which can interrupt a
// paragraph (CommonMark types 1 to 6: `<!--`, `<?`, `<!X`, `<![CDATA[`, and the block tags). Type 7
// (any other complete tag) cannot interrupt a paragraph, so it is not a break.
const HTML_BLOCK_NAMES =
  String.raw`address|article|aside|base|basefont|blockquote|body|caption|center|col|colgroup|dd|details|dialog|` +
  String.raw`dir|div|dl|dt|fieldset|figcaption|figure|footer|form|frame|frameset|h[1-6]|head|header|hr|html|` +
  String.raw`iframe|legend|li|link|main|menu|menuitem|nav|noframes|ol|optgroup|option|p|param|search|section|` +
  String.raw`summary|table|tbody|td|tfoot|th|thead|title|tr|track|ul`;
const HTML_BLOCK_START_SRC =
  String.raw`<(?:!--|\?|!\[CDATA\[|![A-Za-z]|\/?(?:${HTML_BLOCK_NAMES})(?=[\s/>]|$)|(?:pre|script|style|textarea)(?=[\s>]|$))`;
const LINK_BREAK_LINE_RE = new RegExp(String.raw`^(?: {0,3}>)*(?:[ \t]*$| {0,3}${HTML_BLOCK_START_SRC})`, "iu");
/** A removed tag or comment that opened an HTML block (checked on the removed text alone). */
const HTML_BLOCK_OPEN_RE = new RegExp(`^${HTML_BLOCK_START_SRC}`, "iu");

/** For each index i, the start of the first link-breaking line (LINK_BREAK_LINE_RE) that starts
 *  after i, or `n` when there is none (length n + 1). One pass over the lines. */
function nextLinkBreak(s: string): Int32Array {
  const n = s.length;
  const out = new Int32Array(n + 1).fill(n);
  const breaks = linesOf(s)
    .filter((l) => LINK_BREAK_LINE_RE.test(l.text))
    .map((l) => l.start);
  let next = n;
  let b = breaks.length - 1;
  for (let i = n; i >= 0; i--) {
    out[i] = next;
    while (b >= 0 && breaks[b]! >= i) next = Math.min(next, breaks[b--]!);
  }
  return out;
}

/** Whether `at` is a line start, after at most three spaces of indentation (where an HTML block
 *  can open). */
function blockLineStart(s: string, at: number): boolean {
  let i = at;
  while (i > 0 && at - i < 3 && s[i - 1] === " ") i--;
  return i === 0 || s[i - 1] === "\n" || s[i - 1] === "\r";
}

/** HTML comments, terminated or not, left to right in one linear pass: a terminated comment goes;
 *  an unterminated opener goes with everything after it (prDescHTMLComment, then
 *  prDescUnterminatedCmnt; the caller's loop removes a comment one removal exposes). A comment that
 *  opens its line opened an HTML block, which ends a paragraph: it leaves a line break behind, so
 *  stripLinks / stripImages still stop there once it is gone. */
function stripComments(s: string): string {
  let out = "";
  let i = 0;
  for (;;) {
    const open = s.indexOf("<!--", i);
    if (open < 0) return out + s.slice(i);
    out += s.slice(i, open);
    const close = s.indexOf("-->", open + 4);
    if (close < 0) return out;
    if (blockLineStart(s, open)) out += "\n";
    i = close + 3;
  }
}

/** prDescImage (`![alt](t)`, `![alt][r]`, `![alt][]`, `![alt]`), removed whole, in one linear pass.
 *  An image never spans a blank line or a line that opens an HTML block (nextLinkBreak): a forge
 *  ends the paragraph there, so no image forms and the text after it stays. */
function stripImages(s: string): string {
  if (!s.includes("![")) return s;
  const n = s.length;
  const rb = nextIndex(s, "]");
  const rp = nextIndex(s, ")");
  const brk = nextLinkBreak(s);
  let out = "";
  let i = 0;
  let from = 0;
  for (;;) {
    const at = s.indexOf("![", from);
    if (at < 0) break;
    const j = rb[at + 2]!;
    if (j >= n) break; // no `]` after it, so none after any later `![` either
    const lim = brk[at]!;
    if (j >= lim) {
      from = at + 2;
      continue;
    }
    let end = j + 1;
    if (s[end] === "(" && rp[end + 1]! < lim) end = rp[end + 1]! + 1;
    else if (s[end] === "[" && rb[end + 1]! < lim) end = rb[end + 1]! + 1;
    out += s.slice(i, at);
    i = from = end;
  }
  return out + s.slice(i);
}

/** prDescLinkInline (`[text](target)`) and prDescLinkRef (`[text][ref]`): the text is kept, the
 *  target dropped; a label holding a backslash is not a link here (as in the api), nor is one that
 *  spans a blank line or a line that opens an HTML block (nextLinkBreak: a forge ends the paragraph
 *  there, so `[x](` then a blank line then `Fixes #7)` is no link and the directive stays). Linear. */
function stripLinks(s: string, kind: "inline" | "ref"): string {
  if (!s.includes("[")) return s;
  const n = s.length;
  const rb = nextIndex(s, "]");
  const bs = nextIndex(s, "\\");
  const rp = nextIndex(s, ")");
  const brk = nextLinkBreak(s);
  let out = "";
  let i = 0;
  let from = 0;
  for (;;) {
    const at = s.indexOf("[", from);
    if (at < 0) break;
    const j = rb[at + 1]!;
    if (j >= n) break;
    const closeAt = kind === "inline" ? (s[j + 1] === "(" ? rp[j + 2]! : n) : s[j + 1] === "[" ? rb[j + 2]! : n;
    if (bs[at + 1]! < j || closeAt >= brk[at]!) {
      from = at + 1;
      continue;
    }
    out += s.slice(i, at) + s.slice(at + 1, j);
    i = from = closeAt + 1;
  }
  return out + s.slice(i);
}

// prDescHTMLNames / prDescHTMLAttr / prDescHTMLTag, verbatim: a known lowercase element name, so a
// PascalCase generic (`Promise<Data>`) is not read as an element.
const HTML_NAMES =
  String.raw`(?:abbr|address|area|article|aside|audio|base|bdi|bdo|big|blockquote|body|br|button|` +
  String.raw`canvas|caption|center|cite|code|col|colgroup|data|datalist|dd|del|details|dfn|dialog|dir|div|dl|dt|em|` +
  String.raw`embed|fieldset|figcaption|figure|font|footer|form|frame|frameset|h[1-6]|head|header|hgroup|hr|html|` +
  String.raw`iframe|img|input|ins|kbd|label|legend|li|link|main|map|mark|marquee|math|menu|meta|meter|nav|noscript|` +
  String.raw`object|ol|optgroup|option|output|param|picture|pre|progress|rp|rt|ruby|samp|script|section|select|` +
  String.raw`slot|small|source|span|strike|strong|style|sub|summary|sup|svg|table|tbody|td|template|textarea|` +
  String.raw`tfoot|th|thead|time|title|tr|track|tt|ul|var|video|wbr)|[abipqsu]`;
const HTML_ATTR = String.raw`${SP}+[A-Za-z_:][A-Za-z0-9_.:-]*(?:${SP}*=${SP}*(?:[^\s\p{Z}"'=<>\x60]+|'[^']*'|"[^"]*"))?`;
const HTML_TAG_RE = new RegExp(`<(?:${HTML_NAMES})(?:${HTML_ATTR})*${SP}*\\/?>|<\\/(?:${HTML_NAMES})${SP}*>`, "gu");
const AUTOLINK_RE = /<((?:[Hh][Tt][Tt][Pp][Ss]?|[Ff][Tt][Pp]):\/\/[^\s<>]+)>/gu;
const COMMENT_OPEN_RE = /<!-{2,}/gu;
// prDescCommentClose (`-{2,}!?>`), anchored at the start of a dash run by the lookbehind: the same
// matches (a leftmost match always starts a run), without V8 re-scanning a long run at every dash.
const COMMENT_CLOSE_RE = /(?<!-)-{2,}!?>/gu;

/** HTML_TAG_RE's replacement: a tag goes, but one that opens an HTML block at the start of its line
 *  leaves a line break behind (as stripComments does), so the next pass's link and image stripping
 *  still stops at the paragraph end it made. */
function dropTag(tag: string, at: number, s: string): string {
  return blockLineStart(s, at) && HTML_BLOCK_OPEN_RE.test(tag) ? "\n" : "";
}

/** stripPrDescMarkup: one pass of each rule, in the api's order. */
function stripMarkup(s: string): string {
  if (s.includes("<!--")) s = stripComments(s);
  if (s.includes("[")) s = stripLinks(stripLinks(stripImages(s), "inline"), "ref");
  if (s.includes("<")) s = s.replace(AUTOLINK_RE, "$1").replace(HTML_TAG_RE, dropTag).replace(COMMENT_OPEN_RE, "");
  if (s.includes("--")) s = s.replace(COMMENT_CLOSE_RE, "->");
  return s;
}

/**
 * The text as a forge renders it, for the closing scan: prDescNormalize's fixed-point loop
 * (entities decoded to their own fixed point, controls and format characters stripped, HTML
 * comments, known tags, images and link targets removed) with three differences: U+200B is kept
 * (see stripFormat), and whitespace is not collapsed (the closing pattern reads any space run).
 * Entities are decoded everywhere, code spans and autolinks included: a code-span model that could
 * disagree with the forge would leave a directive the forge acts on undecoded here, so the scan reads
 * a code span as ordinary text (conservative; uzi's own completion block is excluded by
 * closingDirectiveOutsideCompletion instead). undefined when MAX_PASSES passes did not converge (the caller fails closed).
 */
function renderedView(s: string): string | undefined {
  for (let i = 0; i < MAX_PASSES; i++) {
    const prev = s;
    s = stripMarkup(stripFormat(decodeEntities(s)));
    if (s === prev) return s;
  }
  return undefined;
}

// ── The fail-toward-closing views ──
//
// renderedView emulates markdown (links, images, comments), and a gap in that emulation is a
// directive a forge acts on that it would miss. The views below emulate less (decodedRawView strips
// nothing, tagStrippedView strips HTML constructs only), and composedView renders the tag-stripped
// view, which covers a split that needs both an HTML construct and a link removed. They
// only ADD texts to the OR-ed scan, so they can only turn a non-closing verdict into a closing one.
// They narrow the gaps; they do not close every one (see the module header's threat model).

/** Line endings as `\n` and every format character but the U+200B breaker (see stripFormat) removed;
 *  controls other than `\r` are kept. */
function lfNoFormat(s: string): string {
  return toLf(s).replace(/\p{Cf}/gu, (c) => (c === ZWSP ? c : ""));
}

/** The decoded-raw view: entities decoded to their fixed point, line endings and format characters
 *  as lfNoFormat, and NO structural stripping (a link's target, a comment's body and an image's alt
 *  text all stay as text). */
function decodedRawView(s: string): string {
  return lfNoFormat(decodeEntities(lfNoFormat(s)));
}

/** A finder for the next `needle` at or after a position, for positions asked in increasing order:
 *  a search that found nothing (or found a match at or after the position) is reused, so every call
 *  together scans `s` at most once. */
function forwardFinder(s: string, needle: string): (from: number) => number {
  let lastFrom = -1;
  let lastAt = -1;
  return (from) => {
    if (lastFrom >= 0 && from >= lastFrom && (lastAt < 0 || lastAt >= from)) return lastAt;
    lastFrom = from;
    lastAt = s.indexOf(needle, from);
    return lastAt;
  };
}

function isAsciiLetter(c: string | undefined): boolean {
  return c !== undefined && ((c >= "A" && c <= "Z") || (c >= "a" && c <= "z"));
}

/**
 * Every HTML construct a forge could hide, removed in one linear left-to-right pass over literal
 * (not entity-decoded) text: a tag (`<name…>`, `</name…>`, any name), a comment (`<!--` … `-->`,
 * `<!-->` and `<!--->` included; an unterminated `<!--` goes to the end of its line), a processing
 * instruction (`<?` … `?>`), CDATA (`<![CDATA[` … `]]>`) and a declaration (`<!X` … `>`). A `<`
 * after an odd run of backslashes is escaped text, not markup, and stays. An unterminated tag, PI,
 * CDATA or declaration stays as text. Links and images are not touched. The text must already be
 * LF-only (lfNoFormat).
 */
function stripAllHtml(s: string): string {
  if (!s.includes("<")) return s;
  const n = s.length;
  const gt = nextIndex(s, ">");
  const nl = nextIndex(s, "\n");
  const commentEnd = forwardFinder(s, "-->");
  const piEnd = forwardFinder(s, "?>");
  const cdataEnd = forwardFinder(s, "]]>");
  let out = "";
  let i = 0;
  let backslashes = 0;
  for (let at = 0; at < n; at++) {
    const c = s[at]!;
    if (c === "\\") {
      backslashes++;
      continue;
    }
    const escaped = backslashes % 2 === 1;
    backslashes = 0;
    if (c !== "<" || escaped) continue;
    let end = -1; // the index just past the construct, or -1 when there is none here
    const c1 = s[at + 1];
    if (s.startsWith("<!--", at)) {
      const close = commentEnd(at + 2);
      end = close >= 0 ? close + 3 : nl[at]!;
    } else if (c1 === "?") {
      const close = piEnd(at + 2);
      if (close >= 0) end = close + 2;
    } else if (s.startsWith("<![CDATA[", at)) {
      const close = cdataEnd(at + 9);
      if (close >= 0) end = close + 3;
    } else if ((c1 === "!" && isAsciiLetter(s[at + 2])) || isAsciiLetter(c1) || (c1 === "/" && isAsciiLetter(s[at + 2]))) {
      const close = gt[at + 1]!;
      if (close < n) end = close + 1;
    }
    if (end < 0) continue;
    out += s.slice(i, at);
    i = end;
    at = end - 1;
  }
  return out + s.slice(i);
}

/** The decoded tag-stripped view: stripAllHtml over the literal text, then decoded as decodedRawView.
 *  THIS view strips a construct only where it is written literally, so an encoded `&lt;x>` stays
 *  text here (as in a sanitized field). That is not true of the scan as a whole: renderedView decodes
 *  before it strips known tags, so an encoded known tag is still read (`Clo&lt;b>ses #7` is closing),
 *  while an encoded custom tag (`Clo&lt;x>ses #7`) stays split. Link and image syntax stays as text. */
function tagStrippedView(s: string): string {
  return decodedRawView(stripAllHtml(lfNoFormat(s)));
}

/** The composed view: known tags removed from the literal text first (HTML_TAG_RE, which reads a
 *  quoted `>` in an attribute as part of the tag, so `<span title="x>y">` goes whole), then every
 *  other HTML construct (tagStrippedView), then the rendered view's link, image and known-tag
 *  stripping over what is left: `Fi<foo>xes [](u)#7` and `Fi<span title="x>y">xes <foo>#7` read as
 *  closing. Custom tags and other constructs are removed only where written literally, so
 *  `Clo&lt;x>ses` stays split; known tags are also read after decoding (the renderedView step), so
 *  `Clo&lt;b>ses` does not. One pass of the known-tag strip and stripAllHtml, then renderedView to
 *  its fixed point; undefined when the rendered view does not converge. */
function composedView(s: string): string | undefined {
  const lf = lfNoFormat(s);
  return renderedView(tagStrippedView(lf.includes("<") ? lf.replace(HTML_TAG_RE, dropTag) : lf));
}

/** Whether a UTF-16 code unit is in REF_SRC's path class `[\w.-]` under its `iu` flags: ASCII
 *  letters, digits, `_`, `.`, `-`, and the two non-ASCII characters that case-fold to ASCII word
 *  letters (U+017F LATIN SMALL LETTER LONG S and U+212A KELVIN SIGN). */
function isPathCode(c: number): boolean {
  return (
    (c >= 48 && c <= 57) || (c >= 65 && c <= 90) || (c >= 97 && c <= 122) || c === 95 || c === 46 || c === 45 || c === 0x17f || c === 0x212a
  );
}

/** A segment's length bound and the extra segments a path may have (REF_PATH_SRC). */
const PATH_SEGMENT_MAX = 255;
const PATH_EXTRA_SEGMENTS = 21;

/**
 * REF_PATH_SRC decided without a regex, for one view. Its segments are atomic, so from a start the
 * path is forced: the maximal `[\w.-]` run (at most 255 long), then either `[#!]` and a digit (the
 * reference) or `/` and the next maximal run, up to 21 more times. Tables built right to left in
 * one pass give, for every run end, how many further segments reach a `[#!]\d` and where it is, so
 * `at(start)` is O(1), and the digits after one `#` / `!` are read once however many keywords share
 * it (a keyword-dense chain ending in one reference).
 */
class PathRefs {
  private readonly runEnd: Int32Array;
  /** At a run end q: the further segments before the reference's `[#!]` (255 when none). */
  private readonly more: Uint8Array;
  /** At a run end q with a finite `more`: the index of that `[#!]`. */
  private readonly hash: Int32Array;
  private readonly digits = new Map<number, { end: number; same: boolean }>();
  private readonly repoMax: number | undefined;

  constructor(
    private readonly text: string,
    private readonly iid: number,
    private readonly repoPath: string | undefined,
  ) {
    const n = text.length;
    this.runEnd = new Int32Array(n + 1);
    this.more = new Uint8Array(n + 1).fill(255);
    this.hash = new Int32Array(n + 1).fill(-1);
    this.runEnd[n] = n;
    for (let q = n - 1; q >= 0; q--) {
      const c = text.charCodeAt(q);
      if (isPathCode(c)) {
        this.runEnd[q] = this.runEnd[q + 1]!;
        continue;
      }
      this.runEnd[q] = q;
      if (c === 35 || c === 33) {
        const d = text.charCodeAt(q + 1);
        if (d >= 48 && d <= 57) {
          this.more[q] = 0;
          this.hash[q] = q;
        }
      } else if (c === 47 && q + 1 < n && isPathCode(text.charCodeAt(q + 1))) {
        const e = this.runEnd[q + 1]!;
        if (e - (q + 1) <= PATH_SEGMENT_MAX && this.more[e]! < PATH_EXTRA_SEGMENTS) {
          this.more[q] = this.more[e]! + 1;
          this.hash[q] = this.hash[e]!;
        }
      }
    }
    // pathCouldBe never holds for a path longer than the repo path plus a `.git` suffix (normalizePath
    // removes at most that; lowercasing keeps every path character one code unit).
    this.repoMax = repoPath ? normalizePath(repoPath).length + 4 : undefined;
  }

  /** The path alternative at `start`: where the match ends and whether it resolves, or undefined. */
  at(start: number): RefHit | undefined {
    if (start >= this.text.length || !isPathCode(this.text.charCodeAt(start))) return undefined;
    const e = this.runEnd[start]!;
    if (e - start > PATH_SEGMENT_MAX || this.more[e]! > PATH_EXTRA_SEGMENTS) return undefined;
    const h = this.hash[e]!;
    let d = this.digits.get(h);
    if (!d) {
      let end = h + 1;
      while (end < this.text.length && this.text.charCodeAt(end) >= 48 && this.text.charCodeAt(end) <= 57) end++;
      d = { end, same: sameNumber(this.text.slice(h + 1, end), this.iid) };
      this.digits.set(h, d);
    }
    const resolves =
      d.same && (this.repoMax === undefined || h - start <= this.repoMax) && pathCouldBe(this.text.slice(start, h), this.repoPath);
    return { end: d.end, resolves };
  }
}

interface RefHit {
  /** Where the reference ends (the regex's lastIndex). */
  end: number;
  resolves: boolean;
}

const SP_RUN_RE = new RegExp(`${SP}*`, "uy");
const ISSUE_WORD_RE = /issue/iuy;
const S_LETTER_RE = /s/iuy;

function spaceRunEnd(text: string, at: number): number {
  SP_RUN_RE.lastIndex = at;
  SP_RUN_RE.exec(text);
  return SP_RUN_RE.lastIndex;
}

/**
 * The positions FIRST_REF_RE's reference can start at after `SP*(?::SP*)?(?:issues?SP*)?` from
 * `at`, in the order its backtracking tries them. Only starts where a reference could begin are
 * listed: every other one the prefix reaches holds a space or `:`, which no alternative starts with.
 */
function refStarts(text: string, at: number): number[] {
  // The common case, answered without a regex: a printable ASCII character other than `:` and `i`
  // (no space, no colon and no `issue` to skip), so the reference can only start right here.
  const c = text.charCodeAt(at);
  if (c > 32 && c < 127 && c !== 58 && c !== 73 && c !== 105) return [at];
  const a = spaceRunEnd(text, at);
  const y = text[a] === ":" ? spaceRunEnd(text, a + 1) : a;
  ISSUE_WORD_RE.lastIndex = y;
  if (!ISSUE_WORD_RE.test(text)) return [y];
  S_LETTER_RE.lastIndex = y + 5;
  if (S_LETTER_RE.test(text)) return [spaceRunEnd(text, y + 6), y + 5, y];
  return [spaceRunEnd(text, y + 5), y];
}

/** FIRST_REF_RE from a keyword's end, with its path alternative decided by PathRefs: at each start
 *  in backtracking order, the alternatives in REF_SRC's order; the first that matches wins (nothing
 *  follows the reference, so no later backtracking can change it). */
function firstRef(text: string, at: number, paths: PathRefs, iid: number, repoPath: string | undefined): RefHit | undefined {
  for (const z of refStarts(text, at)) {
    // Each regex runs only where its first character is: REF_NUM_SRC starts with `#`, `!` or `g`,
    // REF_TAIL_SRC with a letter (`[A-Za-z]` or `h`, which `iu` also matches as U+017F / U+212A).
    const c = text.charCodeAt(z);
    if (c === 35 || c === 33 || c === 103 || c === 71) {
      REF_NUM_RE.lastIndex = z;
      const m = REF_NUM_RE.exec(text);
      if (m) return { end: REF_NUM_RE.lastIndex, resolves: sameNumber(m[1], iid) || sameNumber(m[2], iid) };
    }
    const path = paths.at(z);
    if (path) return path;
    if (!((c >= 65 && c <= 90) || (c >= 97 && c <= 122) || c === 0x17f || c === 0x212a)) continue;
    REF_TAIL_RE.lastIndex = z;
    const m = REF_TAIL_RE.exec(text);
    if (m) {
      const resolves = sameNumber(m[1], iid) || (m[2] !== undefined && sameNumber(m[3], iid) && urlCouldBe(m[2], repoPath));
      return { end: REF_TAIL_RE.lastIndex, resolves };
    }
  }
  return undefined;
}

function sameNumber(digits: string | undefined, iid: number): boolean {
  return digits !== undefined && /^\d+$/.test(digits) && Number(digits) === iid;
}

function normalizePath(p: string): string {
  return p
    .trim()
    .replace(/\.git$/iu, "")
    .replace(/^\/+|\/+$/gu, "")
    .toLowerCase();
}

/** A `path#N` reference could resolve to the repo when `path` is the repo's full path or a
 *  segment-aligned suffix of it (GitLab's `project#N` / `sub/project#N` short forms). An unknown
 *  repo path resolves every qualified reference (fail towards "closing"). */
function pathCouldBe(path: string, repoPath: string | undefined): boolean {
  if (!repoPath) return true;
  const p = normalizePath(path);
  const r = normalizePath(repoPath);
  return p === r || r.endsWith(`/${p}`);
}

/** An issue URL could resolve to the repo when its path, with GitLab's `/-` separator removed,
 *  ends with the repo path (the host is not compared: fail towards "closing"). */
function urlCouldBe(prefix: string, repoPath: string | undefined): boolean {
  if (!repoPath) return true;
  const path = prefix.replace(/^https?:\/\/[^/]*/iu, "").replace(/\/-$/u, "");
  const p = normalizePath(path);
  const r = normalizePath(repoPath);
  return p === r || p.endsWith(`/${r}`) || r.endsWith(`/${p}`);
}

function refResolves(m: RegExpExecArray, iid: number, repoPath: string | undefined): boolean {
  if (sameNumber(m[1], iid) || sameNumber(m[2], iid)) return true;
  if (m[3] !== undefined && sameNumber(m[6], iid)) return pathCouldBe(m[3], repoPath);
  // An external-tracker key (`ABC-12`): GitLab reads it only with an external tracker, which a uzi
  // issue run never uses, but a matching number is still treated as closing (fail closed).
  if (sameNumber(m[7], iid)) return true;
  if (m[8] !== undefined && sameNumber(m[9], iid)) return urlCouldBe(m[8], repoPath);
  return false;
}

/**
 * True when `body` carries a closing directive that could close issue `issueIid` of the repo at
 * `repoPath` (`owner/repo` or `group/sub/project`; omit it to treat every qualified reference as
 * possibly this repo's). The scan is the api sanitizer's prDescClosing pattern, over the raw text
 * and over its rendered view (renderedView: `Fi&#120;es #7`, `Fix<b></b>es #7`, `Clo<!-- -->ses #5`
 * and `Fixes <span>o/r#7</span>` all read as closing), its decoded-raw view (decodedRawView: a
 * directive inside a link target, an unterminated comment or another construct renderedView strips),
 * its decoded tag-stripped view (tagStrippedView: `Fixes <?x?>#7`, `Fi&#120;es <foo>#7`) and that
 * view composed with the rendered view (composedView: `Fi<foo>xes [](u)#7`), each in the api's
 * three marker views (raw, markers removed, markers blanked), and each keyword's GitLab reference
 * list is followed, so `Resolves #7, #8 and #9` closes #9 here too.
 *
 * What it catches, and what it does not (the module header's threat model): directives present in
 * the body when uzi writes it, written plainly, in the api sanitizer's normalised forms, or split by
 * common markup and entities. It is not a boundary against someone with edit rights on the PR
 * description, who can add a plain `Closes #N` after uzi's last write. Accepted gaps include: a link
 * destination with balanced parentheses, escapes, a quoted `)` in its title or a pointy destination
 * holding `)` (`[Fix](a(b)c)es #7`, `[Fix](<1)>)es #7`), a custom tag with a quoted `>` in an
 * attribute (`Fix<foo title="a>b">es #7`), and (in parseOwnedBlocks, not here) list-item fences.
 *
 * Fails closed (true): an issueIid that is not a non-negative safe integer, a body over
 * FORGE_BODY_MAX_CHARS, or a rendered view that does not converge.
 *
 * The U+200B breaker the api inserts after a keyword's first letter is NOT removed: a forge does not
 * remove it either, so a sanitized field is correctly read as non-closing.
 */
export function closingDirectiveFor(body: string, issueIid: number, repoPath?: string): boolean {
  if (!Number.isSafeInteger(issueIid) || issueIid < 0) return true;
  if (body.length > FORGE_BODY_MAX_CHARS) return true;
  const rendered = renderedView(body);
  if (rendered === undefined) return true;
  const views = new Set<string>();
  const tagStripped = tagStrippedView(body);
  const composed = composedView(body);
  if (composed === undefined) return true;
  for (const base of new Set([body, rendered, decodedRawView(body), tagStripped, composed])) {
    for (const v of closingViews(base)) views.add(v.text);
  }
  for (const text of views) {
    let paths: PathRefs | undefined;
    // The positions a reference-list continuation (NEXT_REF_RE) has already been tried at in this
    // view. A continuation's outcome depends only on where it starts, and a walk that found a
    // resolving reference has already returned, so a walk that reaches a visited position would only
    // repeat a walk that found nothing and stops there. Without this, a list built of keyword+
    // reference items (`Fixes fix#1 fix#1 …`) is re-walked from every inner keyword: quadratic.
    // Skipping a keyword that starts inside an already-walked list would NOT be equivalent: `fix#7`
    // is a path reference (`fix`, which may not be this repo) to the outer walk, but its own keyword
    // then reads a bare `#7`; here that inner keyword's first reference is still tried.
    const tried = new Uint8Array(text.length + 1);
    KEYWORD_SCAN_RE.lastIndex = 0;
    for (let kw = KEYWORD_SCAN_RE.exec(text); kw; kw = KEYWORD_SCAN_RE.exec(text)) {
      const at = KEYWORD_SCAN_RE.lastIndex;
      paths ??= new PathRefs(text, issueIid, repoPath);
      const hit = firstRef(text, at, paths, issueIid, repoPath);
      if (!hit) continue;
      if (hit.resolves) return true;
      // Follow a GitLab reference list from where this reference ended (a sticky regex).
      let next = hit.end;
      while (!tried[next]) {
        tried[next] = 1;
        NEXT_REF_RE.lastIndex = next;
        const m = NEXT_REF_RE.exec(text);
        if (!m) break;
        if (refResolves(m, issueIid, repoPath)) return true;
        next = NEXT_REF_RE.lastIndex;
      }
    }
  }
  return false;
}

/**
 * closingDirectiveFor over `body` with uzi's own completion block removed: the interlock's scan.
 *
 * Precondition: `expectedCompletion` is the exact completion block uzi rendered for this run
 * (renderCompletionBlock's output, both markers included), never text read back from the forge
 * (not parseOwnedBlocks(body).completion: a block compared with itself always matches, so a
 * directive inserted into it would be removed unscanned). The block is removed only when
 * parseOwnedBlocks(body) is `ok` AND its completion block equals `expectedCompletion` once line
 * endings (`\r\n`, a lone `\r`) are normalised to `\n` on both sides (a body edited in a forge's
 * web UI may come back with CRLF), and otherwise byte for byte. A body read from the forge is
 * attacker-editable, so a forged block, or uzi's block with a directive inserted into it, is never
 * trusted and the whole body is scanned instead (fail closed). Every other parse outcome (`none`,
 * `malformed`, no completion block, a completion block that differs) scans the whole body too.
 *
 * Why removal is needed at all: uzi's block is non-closing by construction (its tests prove it), but
 * the scan decodes entities inside code spans too, so a branch like `Fix&#101;s&#32;&#35;7` shown in
 * the block's code span would otherwise read as closing.
 *
 * When the block is removed, three texts are scanned and any closing one is closing: the text before
 * it (the region and everything around it) and the text after it, each alone, and the two joined
 * around the bare marker pair. The split scans read each side as the forge does, where uzi's block
 * (an HTML block) ends the paragraph; the joined scan reads a construct that spans the removal. The
 * joined scan may join a keyword before the block to a reference after it, which the forge would not
 * read as one directive: a conservative false positive, and acceptable.
 *
 * `parsed`, when given, MUST be parseOwnedBlocks(body) (a caller that already parsed this exact body
 * passes it so the body is parsed once); it is never a parse of another text.
 */
export function closingDirectiveOutsideCompletion(
  body: string,
  issueIid: number,
  expectedCompletion: string,
  repoPath?: string,
  parsed: ParsedBody = parseOwnedBlocks(body),
): boolean {
  if (parsed.kind !== "ok" || parsed.completion === undefined || toLf(parsed.completion) !== toLf(expectedCompletion)) {
    return closingDirectiveFor(body, issueIid, repoPath);
  }
  const head = parsed.before + (parsed.region ?? "") + parsed.between;
  return (
    closingDirectiveFor(`${head}${COMPLETION_START}\n${COMPLETION_END}${parsed.after}`, issueIid, repoPath) ||
    closingDirectiveFor(head, issueIid, repoPath) ||
    closingDirectiveFor(parsed.after, issueIid, repoPath)
  );
}

/** CommonMark's three line endings (`\r\n`, `\n`, a lone `\r`) as `\n`; nothing else changes. */
function toLf(s: string): string {
  return s.replace(/\r\n?/gu, "\n");
}

/**
 * Insert U+200B after the first letter of closing keywords in `s`, found in any of the three api
 * views. `all`: EVERY keyword, whether or not a reference follows it, so an interpolation that ends
 * with a keyword cannot borrow a reference from the fixed text after it (the breaker is invisible in
 * rendered prose). `directives`: only a keyword a reference follows (the api's own rule) or one that
 * ends the text, so a code span keeps a branch like `fix/login` copyable.
 */
function breakClosingKeywords(s: string, mode: "all" | "directives"): string {
  const at = new Set<number>();
  for (const view of closingViews(s, true)) {
    for (const m of view.text.matchAll(KEYWORD_RE)) {
      if (mode === "directives") {
        const end = m.index + m[0].length;
        FIRST_REF_RE.lastIndex = end;
        const trailing = /^[\s\p{Z}:]*$/u.test(view.text.slice(end));
        if (!trailing && !FIRST_REF_RE.test(view.text)) continue;
      }
      at.add(view.map ? view.map[m.index]! : m.index);
    }
  }
  if (at.size === 0) return s;
  let out = "";
  for (let i = 0; i < s.length; i++) {
    out += s[i];
    if (at.has(i)) out += ZWSP;
  }
  return out;
}

/** The api's breakMentions: U+200B after every `@` not preceded by an ASCII letter or digit, unless
 *  it ends the text or a space follows it. */
function breakMentions(s: string): string {
  let out = "";
  for (let i = 0; i < s.length; i++) {
    out += s[i];
    if (s[i] !== "@" || (i > 0 && /[A-Za-z0-9]/u.test(s[i - 1]!))) continue;
    const next = s[i + 1];
    if (next === undefined || next === " ") continue;
    out += ZWSP;
  }
  return out;
}

// ── Escaping for deterministic interpolations (D7) ─────────────────────────────────────────

// C0/C1 controls, bidi overrides/isolates and the other format runes a reader cannot see. They are
// dropped (a deterministic string has no business carrying them); U+200B is re-added by the
// breakers below only where they put it.
const INVISIBLE_RE = /[\p{Cc}\p{Cf}\u2028\u2029]/gu;
// Markdown inline syntax, GitLab inline math (`$`) and inline-diff braces, and HTML: each is
// backslash-escaped wherever it occurs (a CommonMark backslash escape renders the character).
const INLINE_SPECIAL_RE = /[\\`*_[\]<>|~$&{}!]/gu;
// A leading block token (heading, quote, list, setext, a GitLab `/quick_action`) or an ordered-list
// marker, escaped when the text is the first thing on a line.
const LEADING_ORDERED_RE = /^(\d{1,9})([.)])/u;

/** One line of plain text: every whitespace run (newlines included, so no block can open) becomes
 *  one space, invisible runes are dropped, and the ends are trimmed. */
function flatten(s: string): string {
  return s.replace(/[\s\p{Z}]+/gu, " ").replace(INVISIBLE_RE, "").replace(/ {2,}/gu, " ").trim();
}

/**
 * Escape one deterministic string for inline markdown (D7): control and format runes dropped,
 * whitespace collapsed to single spaces (a newline could open a block), markdown / HTML / GitLab
 * inline syntax backslash-escaped, a leading block token or `/` escaped, then every closing keyword
 * and `@mention` broken with U+200B exactly as the api's sanitizer does.
 */
export function escapeInline(s: string): string {
  let t = flatten(s);
  t = t.replace(INLINE_SPECIAL_RE, (c) => `\\${c}`);
  const ordered = LEADING_ORDERED_RE.exec(t);
  if (ordered) t = `${ordered[1]}\\${ordered[2]}${t.slice(ordered[0].length)}`;
  else if (/^[#>+\-=/]/u.test(t)) t = `\\${t}`;
  return breakMentions(breakClosingKeywords(t, "all"));
}

/** What codeSpan renders for a string that is empty once flattened (an empty span renders as two
 *  literal backticks). */
const EMPTY_CODE = "(empty)";

/**
 * Render one deterministic string as an inline code span (branch names, ids, directories, changed
 * paths): the backtick fence is one longer than the longest backtick run inside, padded with a
 * space when the text starts or ends with a backtick, and a closing keyword that forms a directive
 * (or ends the text) is broken with U+200B, because a closing-directive scan that drops code markers
 * (the api's own removed view) reads straight through a code span. An `@mention` is broken as in
 * escapeInline (a forge does not link one inside code, but the breaker is invisible and costs
 * nothing), and so is every `<!--`, so a marker-shaped path can never be read as one of uzi's block
 * markers. Controls and format runes are dropped and whitespace collapsed (so a newline in a path
 * cannot start a line, a quick action included). An empty string renders as "(empty)".
 */
export function codeSpan(s: string): string {
  const flat = flatten(s);
  if (flat === "") return EMPTY_CODE;
  // [<]: see MARKER_HEAD_RE (semgrep parse)
  const t = breakMentions(breakClosingKeywords(flat, "directives")).replace(/[<]!--/gu, `<${ZWSP}!--`);
  let longest = 0;
  for (const m of t.matchAll(/`+/gu)) longest = Math.max(longest, m[0].length);
  const fence = "`".repeat(longest + 1);
  const pad = t.startsWith("`") || t.endsWith("`") ? " " : "";
  return `${fence}${pad}${t}${pad}${fence}`;
}

/** A URL the api sent (the ci_fix pipeline URL), as a bare http(s) URL a forge autolinks when it is
 *  plainly one (no whitespace, no markdown or HTML syntax, no character entity, no closing
 *  directive), else as a code span, so an odd value can never inject markup, a link or a directive
 *  into the completion block. An entity (`fixes:&#35;7`) is refused because a forge decodes it in a
 *  bare URL's text; in a code span it shows as written. (closingDirectiveFor decodes it in a code
 *  span too; the interlock excludes uzi's exact rendered completion block,
 *  closingDirectiveOutsideCompletion.) */
export function urlOrCodeSpan(url: string): string {
  const t = url.trim();
  const plain =
    /^https?:\/\/[^\s<>()[\]`*~\\"'{}|$!@]+$/iu.test(t) && decodeEntitiesOnce(t) === t && breakClosingKeywords(t, "directives") === t;
  return plain ? t : codeSpan(t);
}

/** The first 7 characters of a hex SHA, lowercased; anything else is shown escaped in a code span. */
function shortSha(sha: string): string {
  const t = sha.trim();
  return /^[0-9a-f]{7,64}$/iu.test(t) ? `\`${t.slice(0, 7).toLowerCase()}\`` : codeSpan(t);
}

/** Whether two SHAs (either may be abbreviated) name the same commit. */
function sameSha(a: string, b: string): boolean {
  const x = a.trim().toLowerCase();
  const y = b.trim().toLowerCase();
  if (x.length < 7 || y.length < 7) return x === y;
  return x.startsWith(y) || y.startsWith(x);
}

// ── Description region (D5, D8, D12) ────────────────────────────────────────────────────────

/** The deterministic inputs of a region: nothing here is model- or lead-authored. */
export interface RegionInput {
  /** The deterministic size block (`**Size:** …`, pr-size.ts; one line or a multi-line table);
   *  absent/empty ⇒ no size block. */
  sizeLine?: string | null;
  /** The head the region describes, for the provenance line and the Verification SHAs. */
  headSha?: string;
  /** The PR's target branch, for the provenance line. */
  targetBranch?: string;
  /** The staged version's source: `lead_only` adds the D8 rung-2 note; `deterministic_only`
   *  renders no fields at all. */
  source?: PrDescriptionSource;
}

/** Why a region carries no fields. */
export type RegionFallback = "no_fields" | "not_sanitized" | "deterministic_only" | "region_cap";

export interface RenderedRegion {
  /** The exact region text, markers included. */
  text: string;
  /** True when the fields were rendered; false for the size-and-provenance-only region. */
  withFields: boolean;
  /** Set when withFields is false. */
  fallback?: RegionFallback;
}

const RUNG2_NOTE = "_Summary written by the agent, not checked against the diff._";

const SCOPE_LABEL: Record<SanitizedPrDescriptionFields["scope_notes"][number]["kind"], string> = {
  added: "Added",
  changed: "Changed",
  dropped: "Dropped",
  deferred: "Deferred",
};

function sizeLineOf(input: RegionInput): string | undefined {
  const t = input.sizeLine?.replace(/\r\n?/gu, "\n").trim();
  return t ? t : undefined;
}

function provenanceOf(input: RegionInput): string | undefined {
  const head = input.headSha?.trim();
  const target = input.targetBranch?.trim();
  if (!head || !target) return undefined;
  return `Describes ${shortSha(head)} against ${codeSpan(target)}.`;
}

function wrapRegion(parts: string[]): string {
  return [REGION_START, ...parts, REGION_END].join("\n");
}

/**
 * The region with no fields: the size line and the provenance line, each only when known. When the
 * caller passes no head or target the region is the size line ALONE, with no provenance line
 * (`REGION_START + "\n" + sizeLine + "\n" + REGION_END`), the shape bodies created before the
 * publisher carry. That shape is uzi's own deterministic region (no model text, nothing to verify
 * against a head), and the publisher treats it as such, not as a human-edited or foreign region.
 */
function deterministicRegion(input: RegionInput): string {
  const parts: string[] = [];
  const size = sizeLineOf(input);
  const prov = provenanceOf(input);
  if (size) parts.push(size);
  if (prov) parts.push(...(parts.length ? [""] : []), prov);
  return wrapRegion(parts);
}

function verificationLines(fields: SanitizedPrDescriptionFields, input: RegionInput): string[] {
  const out: string[] = [];
  // Group the reported checks by the SHA they were reported at, in first-seen order (D5).
  const groups = new Map<string, { sha: string; lines: string[] }>();
  for (const e of fields.verification) {
    const key = e.verified_at_sha.trim().toLowerCase();
    if (!key) continue;
    let g = groups.get(key);
    if (!g) groups.set(key, (g = { sha: e.verified_at_sha, lines: [] }));
    g.lines.push(`- ${e.result === "pass" ? "passed" : "failed"}: ${e.command}`);
  }
  for (const g of groups.values()) {
    const head = input.headSha?.trim();
    const differs = head !== undefined && head !== "" && !sameSha(g.sha, head);
    out.push(
      ...(out.length ? [""] : []),
      `Reported by the agent at ${shortSha(g.sha)}${differs ? ` (the PR head is ${shortSha(head)})` : ""}:`,
      ...g.lines,
    );
  }
  return out;
}

function fieldsRegion(fields: SanitizedPrDescriptionFields, input: RegionInput): string {
  const blocks: string[][] = [];
  const summary = fields.summary.trim();
  if (summary) blocks.push([summary]);
  // D8 rung 2: the note qualifies the summary, so it follows it, never under "### Verification"
  // (which a lead_only version with no reported checks would otherwise render with only the note).
  if (input.source === "lead_only") blocks.push([RUNG2_NOTE]);
  const size = sizeLineOf(input);
  if (size) blocks.push([size]);
  if (fields.changes.length) blocks.push(["### What changed", ...fields.changes.map((c) => `- ${c}`)]);
  const verification = verificationLines(fields, input);
  if (verification.length) blocks.push(["### Verification", ...verification]);
  const notes = [
    ...fields.scope_notes.map((n) => `- ${SCOPE_LABEL[n.kind]}: ${n.text}`),
    ...fields.review_pointers.map((p) => `- Review: ${p}`),
  ];
  if (notes.length) blocks.push(["### Scope and review notes", ...notes]);
  const prov = provenanceOf(input);
  if (prov) blocks.push([prov]);
  return wrapRegion(blocks.map((b) => b.join("\n")).join("\n\n").split("\n"));
}

/**
 * Render the description region (D5 layout). `fields` must be a real SanitizedPrDescriptionFields
 * instance from a WorkerClient decoder; it is checked with `SanitizedPrDescriptionFields.is()` at
 * runtime, and anything else (a plain object, `Object.assign({}, s, raw)`, `structuredClone(s)`, a
 * Proxy, an `any`) renders the size-and-provenance-only region instead (fail closed). A rendered
 * region over REGION_CAP_BYTES (6 KiB) also falls back to it. The fields are published as the api
 * returned them: no re-escaping.
 */
export function renderRegion(input: RegionInput, fields?: SanitizedPrDescriptionFields): RenderedRegion {
  if (fields === undefined) return { text: deterministicRegion(input), withFields: false, fallback: "no_fields" };
  if (!SanitizedPrDescriptionFields.is(fields)) {
    return { text: deterministicRegion(input), withFields: false, fallback: "not_sanitized" };
  }
  if (input.source === "deterministic_only") {
    return { text: deterministicRegion(input), withFields: false, fallback: "deterministic_only" };
  }
  const text = fieldsRegion(fields, input);
  if (Buffer.byteLength(text, "utf8") > REGION_CAP_BYTES) {
    return { text: deterministicRegion(input), withFields: false, fallback: "region_cap" };
  }
  return { text, withFields: true };
}

/** sha256 (hex) of the exact region text, markers included, with CRLF normalised to LF (a forge
 *  may hand the body back with CRLF line ends). */
export function regionSha256(region: string): string {
  return createHash("sha256").update(region.replace(/\r\n/gu, "\n"), "utf8").digest("hex");
}

// ── Completion block (D10, D12, D14) ────────────────────────────────────────────────────────

/** The fixed footer (maintainer-approved 2026-09-27). */
function completionFooter(branch: string): string {
  return `Opened by uzi from ${codeSpan(branch)}. A human reviews and merges; uzi never merges.`;
}

/** The one-line agents note (PRD layout): no agent list, only the provenance of the review. */
const REPO_AGENTS_LINE = "Internally reviewed by the repository's own agents, not uzi's built-in reviewer.";

/** The history-bridge sentence (PRD #1416 Part D), unchanged. */
const BRIDGE_SENTENCE =
  "This branch contains a history bridge: a published commit was restored as an ancestor so the branch fast-forwards without a force-push, and `git log --first-parent` still reads as the intended history.";

/**
 * One line of a kind section (D14: the self_improve evidence, the prompt run's guard-critical
 * paths), as structured data, so this renderer escapes every run-derived value itself rather than
 * trusting a pre-rendered string:
 *
 *   - `fixed`: the caller's own constant wording (a heading, a warning sentence, "" for a blank
 *     line). Still neutralised line by line (renderFixedLine), so a fixed line can never open a
 *     quick action, a mention, a closing directive or raw HTML even if run data leaks into one.
 *   - `path`: a changed file path (from `git diff --name-only -z`, so agent-chosen: newlines,
 *     backticks and `<` survive). Rendered as a quoted list item holding a codeSpan.
 *   - `check`: one self_improve check result; the name and detail are escaped with escapeInline.
 */
export type KindSectionLine =
  | { fixed: string }
  | { path: string }
  | { check: { name: string; status: CheckStatus; detail: string } };

/** A self_improve check's outcome (self-improve.ts's CheckResult uses this one definition). */
export type CheckStatus = "passed" | "failed" | "skipped";

/** A kind section: its lines in order. An empty section renders nothing. */
export type KindSection = readonly KindSectionLine[];

function checkEmoji(status: CheckStatus): string {
  return status === "passed" ? "✅" : status === "failed" ? "❌" : "⚠️";
}

/** A fixed line, defensively: flattened to one line, `<` encoded (no raw HTML or comment, so no
 *  forged block marker), a leading `/` escaped (no GitLab quick action), and closing keywords that
 *  form a directive and `@mentions` broken with U+200B. Markdown emphasis, code and a leading `>`,
 *  `#` or `-` are the wording's own and are kept. */
function renderFixedLine(s: string): string {
  let t = flatten(s).replace(/</gu, "&lt;");
  if (t.startsWith("/")) t = `\\${t}`;
  return breakMentions(breakClosingKeywords(t, "directives"));
}

function renderKindSectionLine(line: KindSectionLine): string {
  if ("path" in line) return `> - ${codeSpan(line.path)}`;
  if ("check" in line) {
    const c = line.check;
    const status = c.status === "passed" || c.status === "failed" ? c.status : "skipped";
    return `- ${checkEmoji(status)} ${escapeInline(c.name)} — ${status} (${escapeInline(c.detail)})`;
  }
  return renderFixedLine(line.fixed);
}

/** A kind section as markdown, leading and trailing blank lines dropped; "" when it has no text. */
function renderKindSection(section: KindSection): string {
  const lines = section.map(renderKindSectionLine);
  while (lines.length && lines[0] === "") lines.shift();
  while (lines.length && lines[lines.length - 1] === "") lines.pop();
  return lines.join("\n");
}

export interface CompletionBlockInput {
  /** The run's issue, for `Related to #N.` / `Closes #N` / the partial lines (issue arm only). */
  issueIid?: number | null;
  /** The source branch, for the footer. */
  branch: string;
  /** D14: a kind's one-line completion sentence. When set, the issue arm (Related/Closes, partial,
   *  accepted, gates) is NOT rendered, exactly as a per-kind body replaced it before. Rendered as
   *  given (it carries the caller's own escaped spans) but for its line breaks, which become spaces,
   *  and `<!--`, which is broken (neutraliseMarkers). */
  kindLine?: string;
  /** Sections a kind carries after its line (the self_improve evidence, the prompt run's
   *  guard-critical paths), as structured KindSection data that this renderer escapes. */
  kindSections?: readonly (KindSection | undefined)[];
  /** Render `Closes #N`. An owner partial or a scope-capped run never closes, whatever this says. */
  closes: boolean;
  completionScope?: ClaimConfig["completion_scope"];
  scopeCapped?: { completedCount: number; total?: number };
  /** The run used the repository's own agents (PRD #37 D3b). */
  repoAgents?: boolean;
  gatesUnverified?: readonly string[];
  gatesDiscoveryTruncated?: boolean;
  /** The pushed history contains an ancestry bridge (PRD #1416). */
  bridged?: boolean;
  /** The completion-unverified banner (PRD #1225), rendered first inside the block, trimmed, with
   *  `<!--` broken (neutraliseMarkers); its lines are kept (a banner may be a multi-line quote). */
  banner?: string;
  /** D12: the head the published region describes vs. the PR's current head; a staleness line is
   *  rendered only when they differ. */
  staleness?: { describedSha: string; headSha: string };
}

/** Issue #293 M2: an "unverified gates" note, or "" when every component's deps installed AND
 *  discovery saw the whole tree. The truncation caveat fires even when `dirs` is empty. */
function gatesUnverifiedSection(dirs?: readonly string[], discoveryTruncated?: boolean): string {
  const named = dirs ?? [];
  if (named.length === 0 && !discoveryTruncated) return "";
  const parts: string[] = [];
  if (named.length > 0) {
    const list = named.map((d) => codeSpan(d)).join(", ");
    parts.push(
      `JS dependencies did not install in: ${list}. Gates that need them (e.g. \`vitest\`, \`knip\`) could not run on this change, so treat those gates as unverified, not passing.`,
    );
  }
  if (discoveryTruncated) {
    parts.push(
      "Dependency discovery stopped at its scan cap, so components beyond it were never checked and their gates may also be unverified.",
    );
  }
  return `> ⚠️ **Quality gates unverified.** ${parts.join(" ")}`;
}

/** D12: the deterministic staleness line in its inner marker pair, or "" when the region is current. */
function stalenessLine(describedSha: string, headSha: string): string {
  if (!describedSha.trim() || !headSha.trim() || sameSha(describedSha, headSha)) return "";
  return [
    STALENESS_START,
    `This description may be outdated: it describes ${shortSha(describedSha)}; the PR head is ${shortSha(headSha)}.`,
    STALENESS_END,
  ].join("\n");
}

function issueArm(input: CompletionBlockInput): string[] {
  const iid = input.issueIid;
  const deferred = input.completionScope?.deferred ?? [];
  const accepted = input.completionScope?.accepted ?? [];
  const isOwnerPartial = deferred.length > 0;
  // Only the normal arm (last below) can render `Closes #N`: an owner partial (PRD #1227) and an
  // operator scope cap (PRD #634) never close, whatever `closes` says, structurally here and not only
  // in the create-then-verify flow.
  const lines = isOwnerPartial
    ? [
        `Implements part of #${iid} (partial delivery — owner scope decision; this MR does NOT close the issue).`,
        "",
        "> ⚠️ **Partial delivery — owner scope decision (PRD #1227).** The owner reduced this run's",
        "> completion scope. The milestone(s) below were DEFERRED BY THE OWNER and are NOT delivered by",
        "> this merge request, so it does not close the issue:",
        ...deferred.map((d) => `> - ${codeSpan(d.milestone_id)} — ${escapeInline(d.title)}: ${escapeInline(d.reason)}`),
      ]
    : input.scopeCapped
      ? [
          `Implements part of #${iid} (partial delivery — see the scope note below; this MR does NOT close the issue).`,
          "",
          "> ⚠️ **Partial delivery — operator scope directive.** The operator narrowed this run's",
          `> scope mid-flight. ${input.scopeCapped.completedCount}${typeof input.scopeCapped.total === "number" ? ` of ${input.scopeCapped.total}` : ""} approved milestone(s) were completed and`,
          "> are included here; any remaining milestones were deferred to a follow-up run. Review this",
          "> as a partial implementation — it does not complete the issue.",
        ]
      : [
          // #1801: NOT `Implements issue #N.` (a GitLab closing keyword).
          `Related to #${iid}.`,
          ...(input.closes ? ["", `Closes #${iid}`] : []),
        ];
  if (accepted.length > 0) {
    lines.push(
      "",
      "> ⚠️ **Accepted unmet criteria — owner decision (PRD #1227).** The owner accepted the following",
      "> unmet criteria as-is with the reason given; they are NOT met by this merge request:",
      ...accepted.map((a) => `> - ${codeSpan(a.id)} — ${escapeInline(a.text)}: ${escapeInline(a.reason)}`),
    );
  }
  const gates = gatesUnverifiedSection(input.gatesUnverified, input.gatesDiscoveryTruncated);
  if (gates) lines.push("", gates);
  return lines;
}

// Every line break a forge reads (CommonMark's three, and the Unicode line and paragraph
// separators), so a kind line stays the one line D14 promises.
const LINE_BREAKS_RE = /\r\n|[\r\n\u2028\u2029]/gu;

/** `<!--` broken with U+200B, as codeSpan does, so a caller's text (the kind line, the banner) can
 *  never carry a block marker, or any HTML comment, into the completion block. Text without `<!--`
 *  is returned unchanged, so today's fixed wording is byte-identical. */
function neutraliseMarkers(s: string): string {
  // [<]: see MARKER_HEAD_RE (semgrep parse)
  return s.replace(/[<]!--/gu, `<${ZWSP}!--`);
}

/**
 * Render the completion block. Its content keeps the MR body's wording and meaning from before
 * PRD #1798 (runner.ts mrCompletionBlock is its caller), inside the markers: the banner, then either the kind's one-liner (D14) with its
 * sections or the issue arm (Related / Closes / partial / accepted / gates), then the agents line,
 * the bridge sentence, the staleness line and the footer.
 */
export function renderCompletionBlock(input: CompletionBlockInput): string {
  const paras: string[] = [];
  const banner = input.banner?.trim();
  if (banner) paras.push(neutraliseMarkers(banner));
  if (input.kindLine !== undefined) {
    paras.push(neutraliseMarkers(input.kindLine.replace(LINE_BREAKS_RE, " ")));
  } else {
    paras.push(issueArm(input).join("\n"));
  }
  if (input.repoAgents) paras.push(REPO_AGENTS_LINE);
  if (input.kindLine !== undefined) {
    for (const section of input.kindSections ?? []) {
      const t = section ? renderKindSection(section) : "";
      if (t) paras.push(t);
    }
  }
  if (input.bridged) paras.push(BRIDGE_SENTENCE);
  if (input.staleness) {
    const s = stalenessLine(input.staleness.describedSha, input.staleness.headSha);
    if (s) paras.push(s);
  }
  paras.push(`---\n${completionFooter(input.branch)}`);
  return [COMPLETION_START, paras.join("\n\n"), COMPLETION_END].join("\n");
}

// ── Parsing and composing a body (D10, D15) ─────────────────────────────────────────────────

/** A body split around uzi's blocks. `region` / `completion` are the exact block text, markers
 *  included; `before + (region ?? "") + between + (completion ?? "") + after === body`. */
export interface OwnedBlocks {
  before: string;
  region?: string;
  between: string;
  completion?: string;
  after: string;
}

export type ParsedBody =
  | { kind: "none" }
  | ({ kind: "ok" } & OwnedBlocks)
  | {
      kind: "malformed";
      reason: "duplicate" | "unbalanced" | "unknown_version" | "oversize" | "marker_in_code" | "misplaced";
    };

// The head of any uzi block marker, however spelled: detection is lenient so a mangled marker is
// reported malformed rather than silently ignored; only the exact forms above are accepted. The
// marker then runs to the first `>` after this head, which must close it as `-->` (the former
// `[^>]*?-->` tail, found with one forward pointer instead of a rescan per candidate).
// `[<]` rather than a bare `<` before `!--`: a regex literal that BEGINS with `<!--` (right after the
// opening `/`) trips semgrep's TypeScript parser (sast:semgrep, --strict), which fails the whole file.
const MARKER_HEAD_RE = /[<]!--[\s\p{Z}]*uzi:(description|completion):(start|end)\b/iuy;

// A fence line (CommonMark: up to 3 SPACES of indent, optionally inside `>` quotes, then 3+
// backticks or tildes). A tab is never fence indentation here. That is right for a TOP-LEVEL line
// only: CommonMark expands a leading tab to a 4-column stop, so a top-level tab-indented ``` is
// indented code or paragraph text, not a fence; after a `>` the quote marker's optional space is
// taken from the tab's columns, so `> \t```` IS a fence to CommonMark and not to this model (the
// fail-closed checks in findMarkers cover what this model gets wrong). An opener's info string may
// not hold a backtick when its fence is backticks.
const FENCE_OPEN_RE = /^((?: {0,3}>)*) {0,3}(`{3,}|~{3,})([^\n]*)$/u;
const FENCE_CLOSE_RE = /^(?: {0,3}>)* {0,3}(`{3,}|~{3,})[ \t]*$/u;
const QUOTE_PREFIX_RE = /^(?: {0,3}>)*/u;
// A line that ends a paragraph for inline-code purposes: blank (inside quotes too), or the start of
// an HTML comment block (every uzi marker line is one).
const PARA_BREAK_RE = /^(?: {0,3}>)*(?:[ \t]*$| {0,3}<!--)/u;

interface Line {
  start: number;
  /** The line's text without its line ending. */
  text: string;
  /** The offset just past the line's ending (or the body's end). */
  next: number;
}

/** The body's lines. A line ends at `\r\n`, `\n` or a lone `\r` (CommonMark's three line endings),
 *  so a `\r`-joined body is read line by line as a forge reads it. */
function linesOf(body: string): Line[] {
  const out: Line[] = [];
  const n = body.length;
  let start = 0;
  for (;;) {
    let end = start;
    while (end < n && body[end] !== "\n" && body[end] !== "\r") end++;
    const next = end >= n ? n : body[end] === "\r" && body[end + 1] === "\n" ? end + 2 : end + 1;
    out.push({ start, text: body.slice(start, end), next });
    if (end >= n) return out;
    start = next;
  }
}

function quoteDepth(text: string): number {
  const m = QUOTE_PREFIX_RE.exec(text)![0];
  let d = 0;
  for (const c of m) if (c === ">") d++;
  return d;
}

/**
 * The spans of `body` that are code, where a marker-shaped string is text, not a marker (L2): fenced
 * code blocks (``` or ~~~, inside block quotes too) and inline code spans (a backtick run closed by
 * the next run of the same length within one paragraph). Sorted, disjoint, [start, end).
 *
 * `closers` picks which lines may close a fence. `depth` (CommonMark): only a fence at the opener's
 * quote depth, so `> ```` inside a top-level fence is a code line. `any`: a fence at any quote depth
 * (the earlier model). Neither is CommonMark in every context (lists, lazy continuation, HTML blocks
 * are not modelled), and each is wrong where the other is right, so findMarkers fails closed on a
 * marker that EITHER model puts in code.
 *
 * One deliberate difference from CommonMark: a fence with no closing fence (and not ended by its
 * block quote ending) is NOT treated as code. CommonMark runs it to the end of the document, which
 * would let one stray fence in human text above uzi's blocks hide them, and uzi would then lose its
 * own blocks; here the opener is ordinary text. Linear: fence closure is decided from suffix tables.
 *
 * Known, accepted limitation: fences inside list items (`* ~~~`, an indented `   ```` or a longer
 * ````` fence opened in a list item) are not modelled, so a marker pair the forge shows as code can
 * still be adopted as uzi's blocks. This never yields a closing false negative: the interlock
 * (closingDirectiveOutsideCompletion) removes a completion block only when it is identical to the
 * block uzi rendered (line endings aside), and scans the whole body otherwise.
 */
function codeRanges(body: string, closers: "depth" | "any"): Array<[number, number]> {
  const lines = linesOf(body);
  const n = lines.length;
  const depth = lines.map((l) => quoteDepth(l.text));
  // The quote depth a closer on line i is filed under (every closer shares one key under `any`).
  const key = (i: number) => (closers === "depth" ? depth[i]! : 0);
  // For each line index i: the longest closing fence of each kind strictly after i that may close a
  // fence opened on line i (under `depth`, one at line i's quote depth), and the lowest quote depth
  // strictly after i, so "does this opener ever close" is O(1).
  const maxTick = new Int32Array(n);
  const maxTilde = new Int32Array(n);
  const minDepth = new Int32Array(n + 1).fill(2 ** 30);
  const closersAt = new Map<number, { tick: number; tilde: number }>();
  for (let i = n - 1; i >= 0; i--) {
    const after = closersAt.get(key(i));
    maxTick[i] = after?.tick ?? 0;
    maxTilde[i] = after?.tilde ?? 0;
    minDepth[i] = Math.min(minDepth[i + 1]!, depth[i]!);
    const c = FENCE_CLOSE_RE.exec(lines[i]!.text);
    if (!c) continue;
    const len = c[1]!.length;
    const tick = Math.max(maxTick[i]!, c[1]![0] === "`" ? len : 0);
    const tilde = Math.max(maxTilde[i]!, c[1]![0] === "~" ? len : 0);
    closersAt.set(key(i), { tick, tilde });
  }
  const ranges: Array<[number, number]> = [];
  const tick = nextIndex(body, "`");
  let para: { from: number; to: number } | undefined;
  const flushPara = () => {
    if (para && tick[para.from]! < para.to) for (const r of inlineCodeRanges(body, tick, para.from, para.to)) ranges.push(r);
    para = undefined;
  };
  for (let i = 0; i < n; i++) {
    const line = lines[i]!;
    const open = FENCE_OPEN_RE.exec(line.text);
    if (open && !(open[2]![0] === "`" && open[3]!.includes("`"))) {
      const ch = open[2]![0]!;
      const len = open[2]!.length;
      const d = depth[i]!;
      const closes = (ch === "`" ? maxTick[i]! : maxTilde[i]!) >= len || (d > 0 && minDepth[i + 1]! < d);
      if (closes) {
        flushPara();
        let j = i + 1;
        let end = body.length;
        for (; j < n; j++) {
          const l = lines[j]!;
          if (d > 0 && depth[j]! < d) {
            end = l.start; // the quote ended, and the fence with it; line j is ordinary text
            j--;
            break;
          }
          const c = key(j) === key(i) ? FENCE_CLOSE_RE.exec(l.text) : null;
          if (c && c[1]![0] === ch && c[1]!.length >= len) {
            end = l.next;
            break;
          }
        }
        ranges.push([line.start, end]);
        i = j;
        continue;
      }
    }
    if (PARA_BREAK_RE.test(line.text)) {
      flushPara();
      continue;
    }
    para = para ? { from: para.from, to: line.next } : { from: line.start, to: line.next };
  }
  flushPara();
  return ranges;
}

/** The inline code spans in body[from, to) (one paragraph): an opening backtick run is closed by the
 *  next run of exactly its length; one with no such run is literal. A run preceded by an odd number
 *  of backslashes opens one backtick shorter. Linear: `tick` (nextIndex of a backtick) finds the
 *  runs without rescanning the body, and one forward pointer per run length finds each closer. */
function inlineCodeRanges(body: string, tick: Int32Array, from: number, to: number): Array<[number, number]> {
  const runs: Array<{ start: number; len: number }> = [];
  for (let i = tick[from]!; i < to; i = tick[i]!) {
    let j = i;
    while (j < to && body[j] === "`") j++;
    runs.push({ start: i, len: j - i });
    i = j;
  }
  const byLen = new Map<number, number[]>();
  runs.forEach((r, k) => {
    const list = byLen.get(r.len);
    if (list) list.push(k);
    else byLen.set(r.len, [k]);
  });
  const ptr = new Map<number, number>();
  const out: Array<[number, number]> = [];
  for (let k = 0; k < runs.length; k++) {
    const r = runs[k]!;
    let start = r.start;
    let len = r.len;
    let slashes = 0;
    while (start - slashes - 1 >= from && body[start - slashes - 1] === "\\") slashes++;
    if (slashes % 2 === 1) {
      start++;
      len--;
    }
    if (len === 0) continue;
    const list = byLen.get(len);
    if (!list) continue;
    let p = ptr.get(len) ?? 0;
    while (p < list.length && list[p]! <= k) p++;
    ptr.set(len, p);
    if (p >= list.length) continue;
    const close = runs[list[p]!]!;
    out.push([start, close.start + close.len]);
    k = list[p]!;
  }
  return out;
}

interface FoundMarker {
  block: "description" | "completion";
  edge: "start" | "end";
  at: number;
  text: string;
  /** At column 0 and alone on its line, as uzi writes every marker. */
  wholeLine: boolean;
}

const EXACT_MARKERS = [REGION_START, REGION_END, COMPLETION_START, COMPLETION_END];

/** Whether `at` starts a line (the body's start, or just after `\n` or `\r`). */
function atLineStart(body: string, at: number): boolean {
  return at === 0 || body[at - 1] === "\n" || body[at - 1] === "\r";
}

/** Whether `at` ends a line (the body's end, or a `\n` or `\r`). */
function atLineEnd(body: string, at: number): boolean {
  return at === body.length || body[at] === "\n" || body[at] === "\r";
}

/** Whether the line at `at` is exactly one of uzi's block markers, as uzi writes it: at column 0,
 *  alone on its line (any of the three line endings after it). */
function wholeLineMarkerAt(body: string, at: number): boolean {
  if (!atLineStart(body, at)) return false;
  return EXACT_MARKERS.some((m) => body.startsWith(m, at) && atLineEnd(body, at + m.length));
}

/** Every uzi block marker outside code (codeRanges under the `depth` closer model), in order, or
 *  "marker_in_code" when a whole-line marker (wholeLineMarkerAt) is inside a code range under
 *  EITHER closer model: each model misreads some fences a forge reads the other way (a list item's
 *  fence is not modelled), so a marker either could hide is refused. Linear: each `<!--` is tested
 *  once, the code ranges are walked with pointers that only move forward, and so is the closing
 *  `>`. */
function findMarkers(body: string): FoundMarker[] | "marker_in_code" {
  const code = codeRanges(body, "depth");
  const alt = codeRanges(body, "any");
  const found: FoundMarker[] = [];
  let r = 0;
  let ra = 0;
  let gt = -1;
  for (let at = body.indexOf("<!--"); at >= 0; at = body.indexOf("<!--", at + 1)) {
    while (r < code.length && code[r]![1] <= at) r++;
    while (ra < alt.length && alt[ra]![1] <= at) ra++;
    const inAlt = ra < alt.length && alt[ra]![0] <= at;
    if (inAlt && wholeLineMarkerAt(body, at)) return "marker_in_code";
    if (r < code.length && code[r]![0] <= at) {
      // Fail closed. codeRanges approximates CommonMark (no HTML blocks, no lazy continuation, no
      // list or indented-code context), so it can see code where a forge renders none: `<details>`
      // then ``` is raw HTML to a forge, not a fence. Human text could then hide uzi's REAL blocks
      // inside a "fence" here and promote a forged pair written after it. uzi never writes a marker
      // inside code, so a whole-line marker in a detected code range makes the whole parse
      // malformed, which the publisher (next unit) is to treat as skipped_malformed: the region is
      // skipped and the completion block falls back per D10. A marker quoted mid-line (inline code,
      // an indented or `>`-quoted line in a fence) is still text and still ignored (L2).
      if (wholeLineMarkerAt(body, at)) return "marker_in_code";
      continue;
    }
    MARKER_HEAD_RE.lastIndex = at;
    const head = MARKER_HEAD_RE.exec(body);
    if (!head) continue;
    const headEnd = at + head[0].length;
    if (gt < headEnd) gt = body.indexOf(">", headEnd);
    if (gt < 0) break; // no `>` left: no later marker can close either
    if (gt - 2 < headEnd || body.slice(gt - 2, gt) !== "--") continue;
    found.push({
      block: head[1]!.toLowerCase() as "description" | "completion",
      edge: head[2]!.toLowerCase() as "start" | "end",
      at,
      text: body.slice(at, gt + 1),
      wholeLine: atLineStart(body, at) && atLineEnd(body, gt + 1),
    });
  }
  return found;
}

/** Split `body` around uzi's two owned blocks. `none`: no uzi block marker at all (a legacy PR).
 *  `malformed`: a duplicate marker, an unbalanced or nested pair, a region after the completion
 *  block, a start marker of an unknown version / an inexact spelling, or a body over
 *  FORGE_BODY_MAX_CHARS (`oversize`: no forge returns one, so it is not scanned), one of uzi's
 *  exact markers alone on a line at column 0 inside a detected code range (`marker_in_code`, fail
 *  closed: see findMarkers), or a marker outside code that is not alone on its line at column 0
 *  (`misplaced`: indented or mid-line). Other marker-shaped text inside a fenced code block or an
 *  inline code span is text, not a marker (a copy quoted mid-line or indented there neither breaks
 *  the parse nor moves the blocks). A line ends at `\r\n`, `\n` or a lone `\r`. */
export function parseOwnedBlocks(body: string): ParsedBody {
  if (body.length > FORGE_BODY_MAX_CHARS) return { kind: "malformed", reason: "oversize" };
  const found = findMarkers(body);
  if (found === "marker_in_code") return { kind: "malformed", reason: "marker_in_code" };
  if (found.length === 0) return { kind: "none" };
  // uzi writes every marker at column 0, alone on its line. One anywhere else (indented, as a
  // 4-space indented-code copy that codeRanges does not model, or mid-line outside code) is not
  // uzi's, and adopting it could promote a forged pair: fail closed.
  if (found.some((f) => !f.wholeLine)) return { kind: "malformed", reason: "misplaced" };
  const exact = { description: { start: REGION_START, end: REGION_END }, completion: { start: COMPLETION_START, end: COMPLETION_END } };
  const spans: Partial<Record<"description" | "completion", { start: number; end: number }>> = {};
  for (const block of ["description", "completion"] as const) {
    const starts = found.filter((f) => f.block === block && f.edge === "start");
    const ends = found.filter((f) => f.block === block && f.edge === "end");
    if (starts.length > 1 || ends.length > 1) return { kind: "malformed", reason: "duplicate" };
    if (starts.length !== ends.length) return { kind: "malformed", reason: "unbalanced" };
    if (starts.length === 0) continue;
    const s = starts[0]!;
    const e = ends[0]!;
    if (s.text !== exact[block].start) return { kind: "malformed", reason: "unknown_version" };
    if (e.text !== exact[block].end) return { kind: "malformed", reason: "unknown_version" };
    if (e.at < s.at) return { kind: "malformed", reason: "unbalanced" };
    spans[block] = { start: s.at, end: e.at + e.text.length };
  }
  const r = spans.description;
  const c = spans.completion;
  if (r && c && !(r.end <= c.start)) return { kind: "malformed", reason: "unbalanced" };
  if (r && c) {
    return {
      kind: "ok",
      before: body.slice(0, r.start),
      region: body.slice(r.start, r.end),
      between: body.slice(r.end, c.start),
      completion: body.slice(c.start, c.end),
      after: body.slice(c.end),
    };
  }
  if (r) return { kind: "ok", before: body.slice(0, r.start), region: body.slice(r.start, r.end), between: "", after: body.slice(r.end) };
  const cc = c!;
  return { kind: "ok", before: body.slice(0, cc.start), between: "", completion: body.slice(cc.start, cc.end), after: body.slice(cc.end) };
}

/**
 * Rebuild a body from a parse, replacing only the blocks named in `next`; every other byte
 * (before, between, after, and a block not replaced) is kept exactly. Returns undefined when `next`
 * names a block the parsed body does not carry: re-inserting a removed block is a caller decision
 * (D10: a removed region is a human removal; an own-mode publication appends a missing completion
 * block in place, keeping the region and all other text, and a refresh leaves it missing (D17);
 * only the interlock's reconcile rewrites the body whole, when the block is still missing at its read).
 */
export function composeBody(parts: OwnedBlocks, next: { region?: string; completion?: string }): string | undefined {
  if (next.region !== undefined && parts.region === undefined) return undefined;
  if (next.completion !== undefined && parts.completion === undefined) return undefined;
  return (
    parts.before + (next.region ?? parts.region ?? "") + parts.between + (next.completion ?? parts.completion ?? "") + parts.after
  );
}

/** A fresh body (no preserved text): the region, when there is one, then the completion block. */
export function renderBody(region: string | undefined, completion: string): string {
  return region ? `${region}\n\n${completion}` : completion;
}

/**
 * D15: the body `compose(region)` builds, unless it exceeds BODY_CAP_CHARS, in which case the
 * region is replaced by `sizeOnlyRegion` (the size line and provenance, no model text). Preserved
 * text and the completion block are never truncated, so a body can stay over the cap when they
 * alone exceed it. `capped` reports that the region was dropped.
 */
export function capBody(
  compose: (region: string) => string | undefined,
  region: string,
  sizeOnlyRegion: string,
): { body: string | undefined; capped: boolean } {
  const body = compose(region);
  if (body === undefined || body.length <= BODY_CAP_CHARS || region === sizeOnlyRegion) return { body, capped: false };
  return { body: compose(sizeOnlyRegion), capped: true };
}
