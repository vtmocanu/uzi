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
// closingDirectiveFor is the whole-body interlock scan (D10 rule 1): the api sanitizer's
// prDescClosing pattern (with Forgejo's `!N` references) over the raw text and over a RENDERED view
// that mirrors the api's prDescNormalize (entities decoded to a fixed point, format characters but
// U+200B dropped, HTML comments, tags, images and link targets removed), each in the api's three
// marker views, and extended to GitLab's reference lists (`Closes #1, #2 and #3`). It is a detector
// for text a forge may read as closing, so where the two could disagree it errs towards "closing".

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
// so no shorter segment could match), and a URL's part before `/issues/` is at most 300 characters
// (GitLab's own bound). Groups are numbered absolutely: no pattern embedding REF_SRC captures before it.
const REF_SRC =
  String.raw`(?:[#!](\d+)|gh-(\d+)|((?=([\w.-]{1,255}))\4(?:\/(?=([\w.-]{1,255}))\5)*)[#!](\d+)|[A-Za-z][A-Za-z0-9_]+-(\d+)|(https?:\/\/[^\s<>()]{0,300}?)\/(?:issues|work_items)\/(\d+))`;
const SP = String.raw`[\s\p{Z}]`;
const KEYWORD_RE = new RegExp(KEYWORD_SRC, "giu");
/** The first reference after a keyword, prDescClosing's tail (sticky at the keyword end). The Go
 *  `SP*:?SP*` is spelled `SP*(?::SP*)?` (the same language): two adjacent stars over one class
 *  backtrack quadratically on a long space run in V8. */
const FIRST_REF_RE = new RegExp(`${SP}*(?::${SP}*)?(?:issues?${SP}*)?${REF_SRC}`, "iuy");
/** A further reference of a GitLab reference list (`#1, #2 and issue #3`): a comma and/or `and`
 *  separator, or plain whitespace, then an optional `issue(s)`. */
const NEXT_REF_RE = new RegExp(`(?:${SP}*,${SP}*(?:and${SP}+)?|${SP}+and${SP}+|${SP}+)(?:issues?${SP}*)?${REF_SRC}`, "iuy");

/** The characters a closing-directive VIEW removes or blanks (prDescClosingMarkers). */
const CLOSING_MARKERS = "*_~`[]\\";

interface View {
  text: string;
  /** Index in the original string of each view index (undefined ⇒ identity). */
  map?: number[];
}

/** The three views the api scans (neutralizeClosingDirectives): the text itself, the markers
 *  REMOVED (`Fix**es** #12` reads `Fixes #12`), and the markers BLANKED (`a_Fixes #12`). */
function closingViews(s: string): View[] {
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
  MediumSpace: "\u205F", NewLine: "\n", Tab: "\t",
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

/** HTML comments, terminated or not, left to right in one linear pass: a terminated comment goes;
 *  an unterminated opener goes with everything after it (prDescHTMLComment, then
 *  prDescUnterminatedCmnt; the caller's loop removes a comment one removal exposes). */
function stripComments(s: string): string {
  let out = "";
  let i = 0;
  for (;;) {
    const open = s.indexOf("<!--", i);
    if (open < 0) return out + s.slice(i);
    out += s.slice(i, open);
    const close = s.indexOf("-->", open + 4);
    if (close < 0) return out;
    i = close + 3;
  }
}

/** prDescImage (`![alt](t)`, `![alt][r]`, `![alt][]`, `![alt]`), removed whole, in one linear pass. */
function stripImages(s: string): string {
  if (!s.includes("![")) return s;
  const n = s.length;
  const rb = nextIndex(s, "]");
  const rp = nextIndex(s, ")");
  let out = "";
  let i = 0;
  for (;;) {
    const at = s.indexOf("![", i);
    if (at < 0) break;
    const j = rb[at + 2]!;
    if (j >= n) break; // no `]` after it, so none after any later `![` either
    let end = j + 1;
    if (s[end] === "(" && rp[end + 1]! < n) end = rp[end + 1]! + 1;
    else if (s[end] === "[" && rb[end + 1]! < n) end = rb[end + 1]! + 1;
    out += s.slice(i, at);
    i = end;
  }
  return out + s.slice(i);
}

/** prDescLinkInline (`[text](target)`) and prDescLinkRef (`[text][ref]`): the text is kept, the
 *  target dropped; a label holding a backslash is not a link here (as in the api). Linear. */
function stripLinks(s: string, kind: "inline" | "ref"): string {
  if (!s.includes("[")) return s;
  const n = s.length;
  const rb = nextIndex(s, "]");
  const bs = nextIndex(s, "\\");
  const rp = nextIndex(s, ")");
  let out = "";
  let i = 0;
  let from = 0;
  for (;;) {
    const at = s.indexOf("[", from);
    if (at < 0) break;
    const j = rb[at + 1]!;
    if (j >= n) break;
    const closeAt = kind === "inline" ? (s[j + 1] === "(" ? rp[j + 2]! : n) : s[j + 1] === "[" ? rb[j + 2]! : n;
    if (bs[at + 1]! < j || closeAt >= n) {
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

/** stripPrDescMarkup: one pass of each rule, in the api's order. */
function stripMarkup(s: string): string {
  if (s.includes("<!--")) s = stripComments(s);
  if (s.includes("[")) s = stripLinks(stripLinks(stripImages(s), "inline"), "ref");
  if (s.includes("<")) s = s.replace(AUTOLINK_RE, "$1").replace(HTML_TAG_RE, "").replace(COMMENT_OPEN_RE, "");
  if (s.includes("--")) s = s.replace(COMMENT_CLOSE_RE, "->");
  return s;
}

/**
 * The text as a forge renders it, for the closing scan: prDescNormalize's fixed-point loop
 * (entities decoded to their own fixed point, controls and format characters stripped, HTML
 * comments, known tags, images and link targets removed) with two differences: U+200B is kept
 * (see stripFormat), and whitespace is not collapsed (the closing pattern reads any space run).
 * undefined when MAX_PASSES passes did not converge (the caller fails closed).
 */
function renderedView(s: string): string | undefined {
  for (let i = 0; i < MAX_PASSES; i++) {
    const prev = s;
    s = stripMarkup(stripFormat(decodeEntities(s)));
    if (s === prev) return s;
  }
  return undefined;
}

/** For each index, the end of the run of ASCII path characters (`[A-Za-z0-9_.-]`) starting there. */
function pathRunEnds(text: string): Int32Array {
  const n = text.length;
  const out = new Int32Array(n + 1).fill(n);
  for (let i = n - 1; i >= 0; i--) {
    const c = text.charCodeAt(i);
    const path = (c >= 48 && c <= 57) || (c >= 65 && c <= 90) || (c >= 97 && c <= 122) || c === 95 || c === 46 || c === 45;
    out[i] = path ? out[i + 1]! : i;
  }
  return out;
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
 * and `Fixes <span>o/r#7</span>` all read as closing), each in the api's three marker views (raw,
 * markers removed, markers blanked), and each keyword's GitLab reference list is followed, so
 * `Resolves #7, #8 and #9` closes #9 here too.
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
  const views = new Set(closingViews(body).map((v) => v.text));
  if (rendered !== body) for (const v of closingViews(rendered)) views.add(v.text);
  for (const text of views) {
    let runs: Int32Array | undefined;
    for (const kw of text.matchAll(KEYWORD_RE)) {
      const end = kw.index + kw[0].length;
      // A keyword glued to `.` / `-` (`fix-fix-…`) can only be followed by a path reference that
      // starts right there; when that run of path characters is over 255 long no reference can
      // match, so skip the regex, which would otherwise pay up to 255 steps per such keyword.
      if (text[end] === "." || text[end] === "-") {
        runs ??= pathRunEnds(text);
        if (runs[end]! - end > 255) continue;
      }
      let re = FIRST_REF_RE;
      re.lastIndex = end;
      let m = re.exec(text);
      while (m) {
        if (refResolves(m, issueIid, repoPath)) return true;
        // Follow a GitLab reference list from where this reference ended (sticky regexes).
        NEXT_REF_RE.lastIndex = re.lastIndex;
        re = NEXT_REF_RE;
        m = re.exec(text);
      }
    }
  }
  return false;
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
  for (const view of closingViews(s)) {
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
  const t = breakMentions(breakClosingKeywords(flat, "directives")).replace(/<!--/gu, `<${ZWSP}!--`);
  let longest = 0;
  for (const m of t.matchAll(/`+/gu)) longest = Math.max(longest, m[0].length);
  const fence = "`".repeat(longest + 1);
  const pad = t.startsWith("`") || t.endsWith("`") ? " " : "";
  return `${fence}${pad}${t}${pad}${fence}`;
}

/** A URL the api sent (the ci_fix pipeline URL), as a bare http(s) URL a forge autolinks when it is
 *  plainly one (no whitespace, no markdown or HTML syntax, no closing directive), else as a code
 *  span, so an odd value can never inject markup, a link or a directive into the completion block. */
export function urlOrCodeSpan(url: string): string {
  const t = url.trim();
  const plain = /^https?:\/\/[^\s<>()[\]`*~\\"'{}|$!@]+$/iu.test(t) && breakClosingKeywords(t, "directives") === t;
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
  /** The deterministic size line (`**Size:** …`, pr-size.ts); absent/empty ⇒ no size line. */
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
  const t = input.sizeLine?.replace(/[\r\n]+/gu, " ").trim();
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
 * caller passes no head or target (mrDescription at MR creation today) the region is the size line
 * ALONE, with no provenance line: bodies created now carry exactly
 * `REGION_START + "\n" + sizeLine + "\n" + REGION_END`. That shape is uzi's own deterministic region
 * (no model text, nothing to verify against a head), and the publisher (next unit) must treat it as
 * such, not as a human-edited or foreign region.
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
  | { check: { name: string; status: "passed" | "failed" | "skipped"; detail: string } };

/** A kind section: its lines in order. An empty section renders nothing. */
export type KindSection = readonly KindSectionLine[];

function checkEmoji(status: "passed" | "failed" | "skipped"): string {
  return status === "passed" ? "✅" : status === "failed" ? "❌" : "⚠️";
}

/** A fixed line, defensively: flattened to one line, `<` encoded (no raw HTML or comment, so no
 *  forged block marker), a leading `/` escaped (no GitLab quick action), and closing keywords that
 *  form a directive and `@mentions` broken with U+200B. Markdown emphasis, code and a leading `>`,
 *  `#` or `-` are the wording's own and are kept. */
function renderFixedLine(s: string): string {
  let t = flatten(s).replace(/</gu, "&lt;");
  if (t.startsWith("/")) t = `\${t}`;
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
   *  accepted, gates) is NOT rendered, exactly as a per-kind body replaced it before. */
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
  /** The completion-unverified banner (PRD #1225), rendered first inside the block. */
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

/**
 * Render the completion block. Its content keeps today's wording and meaning (mrDescription before
 * PRD #1798), inside the markers: the banner, then either the kind's one-liner (D14) with its
 * sections or the issue arm (Related / Closes / partial / accepted / gates), then the agents line,
 * the bridge sentence, the staleness line and the footer.
 */
export function renderCompletionBlock(input: CompletionBlockInput): string {
  const paras: string[] = [];
  const banner = input.banner?.trim();
  if (banner) paras.push(banner);
  if (input.kindLine !== undefined) {
    paras.push(input.kindLine);
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
  | { kind: "malformed"; reason: "duplicate" | "unbalanced" | "unknown_version" | "oversize" };

// The head of any uzi block marker, however spelled: detection is lenient so a mangled marker is
// reported malformed rather than silently ignored; only the exact forms above are accepted. The
// marker then runs to the first `>` after this head, which must close it as `-->` (the former
// `[^>]*?-->` tail, found with one forward pointer instead of a rescan per candidate).
const MARKER_HEAD_RE = /<!--[\s\p{Z}]*uzi:(description|completion):(start|end)\b/iuy;

// A fence line (CommonMark: up to 3 spaces of indent, optionally inside `>` quotes, then 3+
// backticks or tildes). An opener's info string may not hold a backtick when its fence is backticks.
const FENCE_OPEN_RE = /^((?:[ \t]{0,3}>)*)[ \t]{0,3}(`{3,}|~{3,})([^\n]*)$/u;
const FENCE_CLOSE_RE = /^(?:[ \t]{0,3}>)*[ \t]{0,3}(`{3,}|~{3,})[ \t]*$/u;
const QUOTE_PREFIX_RE = /^(?:[ \t]{0,3}>)*/u;
// A line that ends a paragraph for inline-code purposes: blank (inside quotes too), or the start of
// an HTML comment block (every uzi marker line is one).
const PARA_BREAK_RE = /^(?:[ \t]{0,3}>)*(?:[ \t]*$|[ \t]{0,3}<!--)/u;

interface Line {
  start: number;
  /** The line's text without its `\n` (and without a trailing `\r`). */
  text: string;
  /** The offset just past the line's `\n` (or the body's end). */
  next: number;
}

function linesOf(body: string): Line[] {
  const out: Line[] = [];
  let start = 0;
  while (start <= body.length) {
    const nl = body.indexOf("\n", start);
    const end = nl < 0 ? body.length : nl;
    const text = body.slice(start, end).replace(/\r$/u, "");
    out.push({ start, text, next: nl < 0 ? body.length : nl + 1 });
    if (nl < 0) break;
    start = nl + 1;
  }
  return out;
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
 * One deliberate difference from CommonMark: a fence with no closing fence (and not ended by its
 * block quote ending) is NOT treated as code. CommonMark runs it to the end of the document, which
 * would let one stray fence in human text above uzi's blocks hide them, and uzi would then lose its
 * own blocks; here the opener is ordinary text. Linear: fence closure is decided from suffix tables.
 */
function codeRanges(body: string): Array<[number, number]> {
  const lines = linesOf(body);
  const n = lines.length;
  // For each line index i: the longest closing fence of each kind strictly after i, and the lowest
  // quote depth strictly after i, so "does this opener ever close" is O(1).
  const maxTick = new Int32Array(n + 1);
  const maxTilde = new Int32Array(n + 1);
  const minDepth = new Int32Array(n + 1).fill(2 ** 30);
  const depth = lines.map((l) => quoteDepth(l.text));
  for (let i = n - 1; i >= 0; i--) {
    const c = FENCE_CLOSE_RE.exec(lines[i]!.text);
    const len = c ? c[1]!.length : 0;
    maxTick[i] = Math.max(maxTick[i + 1]!, c && c[1]![0] === "`" ? len : 0);
    maxTilde[i] = Math.max(maxTilde[i + 1]!, c && c[1]![0] === "~" ? len : 0);
    minDepth[i] = Math.min(minDepth[i + 1]!, depth[i]!);
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
      const closes = (ch === "`" ? maxTick[i + 1]! : maxTilde[i + 1]!) >= len || (d > 0 && minDepth[i + 1]! < d);
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
          const c = FENCE_CLOSE_RE.exec(l.text);
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
}

/** Every uzi block marker outside code, in order. Linear: each `<!--` is tested once, and the
 *  closing `>` is found with a pointer that only moves forward. */
function findMarkers(body: string): FoundMarker[] {
  const code = codeRanges(body);
  const found: FoundMarker[] = [];
  let r = 0;
  let gt = -1;
  for (let at = body.indexOf("<!--"); at >= 0; at = body.indexOf("<!--", at + 1)) {
    while (r < code.length && code[r]![1] <= at) r++;
    if (r < code.length && code[r]![0] <= at) continue;
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
    });
  }
  return found;
}

/** Split `body` around uzi's two owned blocks. `none`: no uzi block marker at all (a legacy PR).
 *  `malformed`: a duplicate marker, an unbalanced or nested pair, a region after the completion
 *  block, a start marker of an unknown version / an inexact spelling, or a body over
 *  FORGE_BODY_MAX_CHARS (`oversize`: no forge returns one, so it is not scanned). Marker-shaped text
 *  inside a fenced code block or an inline code span is text, not a marker (a quoted copy of uzi's
 *  markers in human text neither breaks the parse nor moves the blocks). */
export function parseOwnedBlocks(body: string): ParsedBody {
  if (body.length > FORGE_BODY_MAX_CHARS) return { kind: "malformed", reason: "oversize" };
  const found = findMarkers(body);
  if (found.length === 0) return { kind: "none" };
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
 * (D10: a removed region is a human removal, a missing completion block means a whole-body rewrite).
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
