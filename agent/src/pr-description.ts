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
// Two kinds of text reach a block, and they are escaped differently:
//
//   - API-SANITIZED fields (summary, changes, scope notes, review pointers, verification commands):
//     already escaped by the api's sanitizer (D7: markdown block syntax, `<`, links, mentions,
//     closing keywords broken by U+200B). They are published AS RETURNED, never re-escaped and never
//     Cf-stripped (stripping the U+200B breakers would re-form the directives). The renderer accepts
//     them only as a real SanitizedPrDescriptionFields instance (checked at runtime with `is()`).
//   - DETERMINISTIC interpolations (branch names, milestone ids / titles / reasons, accepted criteria,
//     gate directories, the ci_fix pipeline ref): run data the api never sanitized, some of it
//     attacker- or owner-typed. They go through escapeInline (markdown escaping, `@` and closing-
//     keyword neutralisation with U+200B like the api, a leading `/`) or codeSpan (a code span whose
//     closing keywords are broken the same way).
//
// closingDirectiveFor is the whole-body interlock scan (D10 rule 1): it ports the api sanitizer's
// prDescClosing pattern and its marker-stripped views, so every form the sanitizer would neutralise
// is caught, and extends it to GitLab's reference lists (`Closes #1, #2 and #3`).

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

// The keyword half of prDescClosing, verbatim (group 1 of the Go pattern). `\b` on both sides.
const KEYWORD_SRC = String.raw`\b(clos(?:e[sd]?|ing)|fix(?:e[sd]|ing)?|resolv(?:e[sd]?|ing)|implement(?:s|ed|ing)?)\b`;
// One reference, the alternatives of prDescClosing's last group, each capturing what resolution
// needs: 1 = `#N`, 2 = `gh-N`, 3/4 = `path#N`, 5 = an external-tracker key's number, 6/7 = an
// issue or work-item URL (the part before `/issues|work_items/`, and N).
const REF_SRC =
  String.raw`(?:#(\d+)|gh-(\d+)|([\w.-]+(?:\/[\w.-]+)*)#(\d+)|[A-Za-z][A-Za-z0-9_]+-(\d+)|(https?:\/\/[^\s<>()]*?)\/(?:issues|work_items)\/(\d+))`;
const SP = String.raw`[\s\p{Z}]`;
const KEYWORD_RE = new RegExp(KEYWORD_SRC, "giu");
/** The first reference after a keyword, exactly prDescClosing's tail (sticky at the keyword end). */
const FIRST_REF_RE = new RegExp(`${SP}*:?${SP}*(?:issues?${SP}*)?${REF_SRC}`, "iuy");
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

// The entity spellings of `#` a human could type so the raw text hides the reference from a scan
// while the rendered text shows it. The api decodes every entity before it neutralises; here a
// fourth view decodes these (the only character a reference cannot do without).
const HASH_ENTITY_RE = /&(?:#0*35|#x0*23|num);/giu;

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
  if (m[3] !== undefined && sameNumber(m[4], iid)) return pathCouldBe(m[3], repoPath);
  // An external-tracker key (`ABC-12`): GitLab reads it only with an external tracker, which a uzi
  // issue run never uses, but a matching number is still treated as closing (fail closed).
  if (sameNumber(m[5], iid)) return true;
  if (m[6] !== undefined && sameNumber(m[7], iid)) return urlCouldBe(m[6], repoPath);
  return false;
}

/**
 * True when `body` carries a closing directive that could close issue `issueIid` of the repo at
 * `repoPath` (`owner/repo` or `group/sub/project`; omit it to treat every qualified reference as
 * possibly this repo's). The scan is the api sanitizer's prDescClosing pattern over the same three
 * views (raw, markers removed, markers blanked) plus a view with `#` entities decoded, and each
 * keyword's GitLab reference list is followed, so `Resolves #7, #8 and #9` closes #9 here too.
 *
 * The U+200B breaker the api inserts after a keyword's first letter is NOT removed: a forge does not
 * remove it either, so a sanitized field is correctly read as non-closing.
 */
export function closingDirectiveFor(body: string, issueIid: number, repoPath?: string): boolean {
  if (!Number.isSafeInteger(issueIid) || issueIid < 0) return false;
  const decoded = body.replace(HASH_ENTITY_RE, "#");
  const views = closingViews(body).map((v) => v.text);
  if (decoded !== body) views.push(...closingViews(decoded).map((v) => v.text));
  for (const text of views) {
    for (const kw of text.matchAll(KEYWORD_RE)) {
      let re = FIRST_REF_RE;
      re.lastIndex = kw.index + kw[0].length;
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

/**
 * Escape one deterministic string for inline markdown (D7): control and format runes dropped,
 * whitespace collapsed to single spaces (a newline could open a block), markdown / HTML / GitLab
 * inline syntax backslash-escaped, a leading block token or `/` escaped, then every closing keyword
 * and `@mention` broken with U+200B exactly as the api's sanitizer does.
 */
export function escapeInline(s: string): string {
  let t = s.replace(/[\s\p{Z}]+/gu, " ").replace(INVISIBLE_RE, "").trim();
  t = t.replace(INLINE_SPECIAL_RE, (c) => `\\${c}`);
  const ordered = LEADING_ORDERED_RE.exec(t);
  if (ordered) t = `${ordered[1]}\\${ordered[2]}${t.slice(ordered[0].length)}`;
  else if (/^[#>+\-=/]/u.test(t)) t = `\\${t}`;
  return breakMentions(breakClosingKeywords(t, "all"));
}

/**
 * Render one deterministic string as an inline code span (branch names, ids, directories): the
 * backtick fence is one longer than the longest backtick run inside, padded with a space when the
 * text starts or ends with a backtick, and a closing keyword that forms a directive (or ends the
 * text) is broken with U+200B, because a closing-directive scan that drops code markers (the api's
 * own removed view) reads straight through a code span. An `@mention` is broken as in escapeInline
 * (a forge does not link one inside code, but the breaker is invisible and costs nothing). Controls and format runes are dropped and whitespace collapsed.
 */
export function codeSpan(s: string): string {
  const t = breakMentions(breakClosingKeywords(s.replace(/[\s\p{Z}]+/gu, " ").replace(INVISIBLE_RE, "").trim(), "directives"));
  const longest = Math.max(0, ...[...t.matchAll(/`+/gu)].map((m) => m[0].length));
  const fence = "`".repeat(longest + 1);
  const pad = t.startsWith("`") || t.endsWith("`") ? " " : "";
  return `${fence}${pad}${t}${pad}${fence}`;
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

const RUNG2_NOTE = "Summary written by the agent, not checked against the diff.";

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
  if (input.source === "lead_only") out.push(...(out.length ? [""] : []), RUNG2_NOTE);
  return out;
}

function fieldsRegion(fields: SanitizedPrDescriptionFields, input: RegionInput): string {
  const blocks: string[][] = [];
  const summary = fields.summary.trim();
  if (summary) blocks.push([summary]);
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

export interface CompletionBlockInput {
  /** The run's issue, for `Related to #N.` / `Closes #N` / the partial lines (issue arm only). */
  issueIid?: number | null;
  /** The source branch, for the footer. */
  branch: string;
  /** D14: a kind's one-line completion sentence. When set, the issue arm (Related/Closes, partial,
   *  accepted, gates) is NOT rendered, exactly as a per-kind body replaced it before. */
  kindLine?: string;
  /** Deterministic sections a kind carries verbatim after its line (the self_improve evidence, the
   *  prompt guard-critical paths). Worker-generated, not escaped. */
  kindSections?: readonly (string | undefined)[];
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
    for (const s of input.kindSections ?? []) {
      const t = s?.replace(/^\n+|\n+$/gu, "");
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
  | { kind: "malformed"; reason: "duplicate" | "unbalanced" | "unknown_version" };

// Any uzi block marker, however spelled: detection is lenient so a mangled marker is reported
// malformed rather than silently ignored; only the exact forms above are accepted.
const ANY_MARKER_RE = /<!--[\s\p{Z}]*uzi:(description|completion):(start|end)\b[^>]*?-->/giu;

/** Split `body` around uzi's two owned blocks. `none`: no uzi block marker at all (a legacy PR).
 *  `malformed`: a duplicate marker, an unbalanced or nested pair, a region after the completion
 *  block, or a start marker of an unknown version / an inexact spelling. */
export function parseOwnedBlocks(body: string): ParsedBody {
  const found: { block: "description" | "completion"; edge: "start" | "end"; at: number; text: string }[] = [];
  for (const m of body.matchAll(ANY_MARKER_RE)) {
    found.push({
      block: m[1]!.toLowerCase() as "description" | "completion",
      edge: m[2]!.toLowerCase() as "start" | "end",
      at: m.index,
      text: m[0],
    });
  }
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
