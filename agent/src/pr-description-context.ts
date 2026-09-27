// PRD #1798 M5 (D6/D7): the budgeted, REDACTED input of the PR-description editor pass.
//
// The editor pass (summary-runner.ts generateDeliverySummary) writes plain English from the lead's
// claims AND the final diff. Everything it reads is attacker-shapeable (issue, PRD, plan, claims,
// commit messages, file names, diff content), so this module:
//
//   1. runs EVERY part through the run's secret redactor (makeTextRedactor, redact.ts) BEFORE it is
//      cut, so a secret straddling a cut can never leave half of itself in the prompt;
//   2. caps each part in UTF-8 bytes and marks a cut part with "[truncated]";
//   3. keeps the whole input under one aggregate budget (120 KiB): the fixed parts first, then diff
//      hunks fill what is left, code first, then tests, never generated or vendored files;
//   4. reports which parts were cut (`truncated`), so the prompt can forbid exhaustive claims;
//   5. renders path names escaped (see renderPath), so a crafted file name cannot forge a row.
//
// Git access is injected (DeliveryContextGit): the size-line reads of pr-size.ts (merge-base,
// NUL numstat, check-attr at the head) are reused as-is, and the two extra reads this module needs
// (commit subjects, one path's diff) go through `readBare`, a BOUNDED `git -C <bare> <args>` read
// (GitCache.readBare) that stops at a byte bound instead of holding a huge output in memory.
// Every git read happens before `deadlineMs` (D2: the context build and the model pass share one
// budget): once it is reached no further read is issued and the affected parts are flagged.
// The builder never throws: a failed git read leaves that part "(unavailable)" and flags it
// truncated, so the model is never told a partial view is complete.

import type { BoundedRead, BoundedReadOptions } from "./git.js";
import type { Logger } from "./log.js";
import { classifyPath, parseNumstatZ, type PathAttributes, type SizeBucket, type SizeLineGit } from "./pr-size.js";
import type { TextRedactor } from "./redact.js";
import type { PrSummaryClaim } from "./signals.js";
import type { Delta } from "./summary-runner.js";
import { errMessage } from "./util.js";

/** Aggregate input budget (D6), in UTF-8 bytes. */
export const DELIVERY_CONTEXT_BUDGET_BYTES = 120 * 1024;
/** Per-part caps (D6), in UTF-8 bytes. */
const ISSUE_CAP = 16 * 1024;
const PRD_CAP = 32 * 1024;
const PLAN_CAP = 6 * 1024;
/** The previously published fields (D12 refresh context): their own part, so they never push the
 *  plan out. */
const PREVIOUS_CAP = 4 * 1024;
const CLAIMS_CAP = 8 * 1024;
/** Commit subjects: at most this many (D6), each clipped, the list byte-capped. */
const MAX_COMMITS = 100;
const COMMIT_SUBJECT_CAP = 200;
const COMMITS_CAP = 12 * 1024;
/** The bound on the raw `git log` read: room for the newest subjects at their full clip plus slack.
 *  A read cut by it drops its last, incomplete record. */
const COMMITS_READ_BYTES = 64 * 1024;
/** Changed paths: at most this many listed one by one (D6), else aggregated per top-level
 *  directory; the rendered inventory is byte-capped either way. */
const MAX_LISTED_PATHS = 2_000;
const PATHS_CAP = 24 * 1024;
/** One file's hunks never take more than this, so one huge file cannot starve the rest. */
const PER_PATH_DIFF_CAP = 16 * 1024;
/** Extra bytes read past a per-path cap. A read cut at the bound loses this tail after redaction
 *  (see buildDeliveryContext), which is what keeps a secret straddling the READ boundary out. */
const READ_MARGIN_BYTES = 4 * 1024;
/** Bound on per-path `git diff` reads, so a many-file PR cannot turn into thousands of spawns. */
const MAX_DIFF_READS = 200;
/** Below this many bytes left, no further hunk is worth a git read. */
const MIN_USEFUL_DIFF_BYTES = 256;

export const TRUNCATED_MARKER = "[truncated]";
const UNAVAILABLE = "(unavailable)";
const ATTRS_UNAVAILABLE = "(unavailable: attributes could not be read)";

/** A full object name: SHA-1 (40 hex) or SHA-256 (64 hex). */
const OBJECT_ID_RE = /^(?:[0-9a-f]{40}|[0-9a-f]{64})$/;

/** The git reads the builder needs. GitCache provides all of them. */
export interface DeliveryContextGit extends SizeLineGit {
  /** A bounded read in the bare (GitCache.readBare): stdout up to `maxBytes`, `truncated` when the
   *  bound cut it; rejects on a non-zero exit, a timeout or an abort. */
  readBare(barePath: string, args: readonly string[], opts: BoundedReadOptions): Promise<BoundedRead>;
}

/** The fields of a previously published description (D12 refresh context). Structural on purpose:
 *  it accepts a SanitizedPrDescriptionFields as well as raw fields; either way it is only ever
 *  prompt INPUT, redacted and fenced like everything else, never published from here. */
export interface PreviousDescriptionFields {
  readonly summary: string;
  readonly changes: readonly string[];
  readonly scope_notes: readonly { readonly kind: string; readonly text: string }[];
  readonly review_pointers: readonly string[];
}

export interface DeliveryContextInput {
  git: DeliveryContextGit;
  barePath: string;
  /** The PR's target branch (resolved like the size line's: the bare's origin-tracking ref). */
  targetBranch: string;
  /** The exact head being published (40- or 64-hex; anything else leaves the git parts unavailable). */
  headSha: string;
  /** D2: the absolute deadline (epoch ms) of the whole publication budget (SummaryRunner
   *  deliverySummaryDeadline). No git read is issued at or after it, each read's timeout is the time
   *  left, and a part it stops is flagged truncated. The model pass runs on what is left after. */
  deadlineMs: number;
  /** Aborting stops the build's git reads like a reached deadline. */
  signal?: AbortSignal;
  /** Injectable clock (epoch ms) for the deadline; default Date.now. */
  now?: () => number;
  /** The run's secret redactor, built from the claim's secrets (makeTextRedactor). */
  redact: TextRedactor;
  issueTitle: string;
  issueBody: string;
  /** The linked PRD text, e.g. from resolvePrdInput on the worktree; null when none. */
  prdText?: string | null;
  /** PRD #362 plan summary + deltas (ExecutorResult.summaryPlan / summaryDeltas). */
  summaryPlan?: string;
  summaryDeltas?: readonly Delta[];
  /** The claim's approved plan markdown, used only when there is no plan summary. */
  planMd?: string | null;
  /** The lead's structured claims (D4). */
  prSummary?: PrSummaryClaim;
  /** The PR's previously published fields, for a refresh (D12). */
  previous?: PreviousDescriptionFields | null;
  log?: Pick<Logger, "warn">;
}

/** Which parts were cut (or could not be read), so the prompt forbids exhaustive claims. */
export interface DeliveryTruncation {
  issue: boolean;
  prd: boolean;
  plan: boolean;
  previous: boolean;
  claims: boolean;
  commits: boolean;
  paths: boolean;
  diff: boolean;
}

/** The redacted, capped prompt input. Every string here has passed the redactor. A null part is
 *  absent from the run (no PRD, no plan, no previous description, no claims) and is left out of
 *  the prompt. */
export interface DeliveryContext {
  issue: string;
  prd: string | null;
  plan: string | null;
  previous: string | null;
  claims: string | null;
  commits: string;
  paths: string;
  diff: string;
  truncated: DeliveryTruncation;
  /** Total UTF-8 bytes of all parts; never above {@link DELIVERY_CONTEXT_BUDGET_BYTES}. */
  bytes: number;
}

const UTF8 = new TextEncoder();
const byteLen = (s: string): number => UTF8.encode(s).length;

/** The longest prefix of `s` within `maxBytes` UTF-8 bytes, cut on a code-point boundary. */
function utf8Prefix(s: string, maxBytes: number): string {
  if (maxBytes <= 0) return "";
  if (byteLen(s) <= maxBytes) return s;
  let used = 0;
  let out = "";
  for (const ch of s) {
    const n = byteLen(ch);
    if (used + n > maxBytes) break;
    used += n;
    out += ch;
  }
  return out;
}

/** Cut `s` to at most `maxBytes` UTF-8 bytes on a code-point boundary, ending with the marker.
 *  `force` marks it cut even when it fits (the source was already cut upstream). */
function capBytes(s: string, maxBytes: number, force = false): { text: string; truncated: boolean } {
  if (!force && byteLen(s) <= maxBytes) return { text: s, truncated: false };
  const suffix = `\n${TRUNCATED_MARKER}`;
  return { text: utf8Prefix(s, maxBytes - byteLen(suffix)) + suffix, truncated: true };
}

/** Redact FIRST, then cap: the cut can never split a secret the redactor would have matched. */
function part(redact: TextRedactor, raw: string, cap: number): { text: string; truncated: boolean } {
  return capBytes(redact(raw), cap);
}

/** A character a path must not carry into the prompt as is: controls (newline, tab, ESC...),
 *  format characters (bidi overrides and isolates, zero-width joiners), line/paragraph
 *  separators, and the quote and backslash the escaped form itself uses. */
const PATH_UNSAFE_RE = /[\p{Cc}\p{Cf}\p{Zl}\p{Zp}"\\]/u;

/** Render a (redacted) path for the prompt like git's quotePath: verbatim when it is plain, else
 *  double-quoted with every unsafe character escaped, so a crafted name (a newline forging an
 *  inventory row, a bidi override reordering one) reads as one inert token. */
function renderPath(p: string): string {
  if (!PATH_UNSAFE_RE.test(p)) return p;
  let out = '"';
  for (const ch of p) {
    if (ch === '"' || ch === "\\") out += `\\${ch}`;
    else if (ch === "\n") out += "\\n";
    else if (ch === "\t") out += "\\t";
    else if (ch === "\r") out += "\\r";
    else if (PATH_UNSAFE_RE.test(ch)) {
      const cp = ch.codePointAt(0)!;
      out += cp > 0xffff ? `\\u{${cp.toString(16)}}` : `\\u${cp.toString(16).padStart(4, "0")}`;
    } else out += ch;
  }
  return `${out}"`;
}

function renderClaims(c: PrSummaryClaim): string {
  // Rendered as labelled data lines, never as markdown the model might echo as structure.
  const lines: string[] = [];
  if (c.what) lines.push(`what: ${c.what}`);
  if (c.why) lines.push(`why: ${c.why}`);
  for (const x of c.changes ?? []) lines.push(`change: ${x}`);
  for (const v of c.verification ?? []) lines.push(`reported check (${v.result}): ${v.command}`);
  for (const n of c.scope_notes ?? []) lines.push(`scope note (${n.kind}): ${n.text}`);
  for (const p of c.review_pointers ?? []) lines.push(`review pointer: ${p}`);
  return lines.join("\n");
}

function renderPrevious(p: PreviousDescriptionFields): string {
  const lines = [`previous summary: ${p.summary}`];
  for (const x of p.changes) lines.push(`previous change: ${x}`);
  for (const n of p.scope_notes) lines.push(`previous scope note (${n.kind}): ${n.text}`);
  for (const r of p.review_pointers) lines.push(`previous review pointer: ${r}`);
  return lines.join("\n");
}

function renderPlan(input: DeliveryContextInput): string | null {
  const planSummary = input.summaryPlan?.trim();
  if (planSummary) {
    const deltas = (input.summaryDeltas ?? []).map((d) => `- ${d.kind}: ${d.text}`);
    return (
      "Plan context, not delivered scope (what the plan intended, not what the branch contains):\n" +
      [planSummary, ...deltas].join("\n")
    );
  }
  if (input.planMd?.trim()) {
    return "Plan context, not delivered scope (the approved plan, not what the branch contains):\n" + input.planMd;
  }
  return null;
}

/** Rename record of `git diff -z --numstat -M`: new path → old path (parseNumstatZ keeps only the
 *  new one; the per-path diff needs both, or a renamed file would read as wholly added). */
function renamesOf(numstatZ: string): Map<string, string> {
  const tokens = numstatZ.split("\0");
  const renames = new Map<string, string>();
  for (let i = 0; i < tokens.length; i++) {
    const rec = tokens[i]!;
    const t1 = rec.indexOf("\t");
    const t2 = t1 < 0 ? -1 : rec.indexOf("\t", t1 + 1);
    if (t2 >= 0 && rec.slice(t2 + 1) === "" && i + 2 < tokens.length) {
      renames.set(tokens[i + 2]!, tokens[i + 1]!);
      i += 2;
    }
  }
  return renames;
}

interface PathRow {
  path: string;
  added: number;
  deleted: number;
  binary: boolean;
  bucket: SizeBucket;
}

function renderPathRow(redact: TextRedactor, r: PathRow): string {
  const counts = r.binary ? "binary" : `+${r.added} -${r.deleted}`;
  return `${renderPath(redact(r.path))} ${counts} [${r.bucket}]`;
}

/** Aggregate per top-level directory ("." for a root file), with file and line counts. */
function aggregatePaths(redact: TextRedactor, rows: readonly PathRow[]): string {
  const dirs = new Map<string, { files: number; added: number; deleted: number }>();
  for (const r of rows) {
    const slash = r.path.indexOf("/");
    const top = slash < 0 ? "." : r.path.slice(0, slash + 1);
    const d = dirs.get(top) ?? { files: 0, added: 0, deleted: 0 };
    d.files += 1;
    d.added += r.added;
    d.deleted += r.deleted;
    dirs.set(top, d);
  }
  const lines = [...dirs.entries()]
    .sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0))
    .map(([dir, d]) => `${renderPath(redact(dir))} ${d.files} files +${d.added} -${d.deleted}`);
  return `${rows.length} changed files, aggregated per top-level directory:\n${lines.join("\n")}`;
}

/** Diff priority (D6): non-test, non-generated first (code, then config, then docs), then tests.
 *  Generated and vendored files are never read. */
const DIFF_ORDER: readonly SizeBucket[] = ["code", "config", "docs", "tests"];

class DeadlineReached extends Error {
  constructor() {
    super("the delivery context deadline was reached");
    this.name = "DeadlineReached";
  }
}

/**
 * Build the editor pass's input for the published head. Never throws: a git failure (or the
 * deadline) leaves the affected part "(unavailable)" and flags it truncated, and is logged at warn.
 */
export async function buildDeliveryContext(input: DeliveryContextInput): Promise<DeliveryContext> {
  const { redact, git, barePath, headSha, log, signal } = input;
  const now = input.now ?? Date.now;
  const truncated: DeliveryTruncation = {
    issue: false,
    prd: false,
    plan: false,
    previous: false,
    claims: false,
    commits: false,
    paths: false,
    diff: false,
  };

  /** Milliseconds left before the deadline; 0 once it is reached or the build is aborted. */
  const timeLeft = (): number => (signal?.aborted ? 0 : Math.max(0, input.deadlineMs - now()));
  /** A size-line read (GitCache's own, unbounded in time) raced against the deadline. A read the
   *  deadline abandons finishes in the background under git.ts's own timeout; its result is dropped. */
  const withinDeadline = async <T>(read: () => Promise<T>): Promise<T> => {
    const left = timeLeft();
    if (left <= 0) throw new DeadlineReached();
    const pending = read();
    pending.catch(() => undefined);
    let timer: NodeJS.Timeout | undefined;
    const expiry = new Promise<never>((_, reject) => {
      timer = setTimeout(() => reject(new DeadlineReached()), left);
    });
    try {
      return await Promise.race([pending, expiry]);
    } finally {
      clearTimeout(timer);
    }
  };
  /** A bounded read with the time left as its timeout (the caller has checked some is left). */
  const boundedRead = (args: readonly string[], maxBytes: number): Promise<BoundedRead> =>
    git.readBare(barePath, args, { maxBytes, timeoutMs: timeLeft(), ...(signal ? { signal } : {}) });

  const issue = part(redact, `Title: ${input.issueTitle}\n\n${input.issueBody || "(no description)"}`, ISSUE_CAP);
  truncated.issue = issue.truncated;

  let prd: string | null = null;
  if (input.prdText?.trim()) {
    const p = part(redact, input.prdText, PRD_CAP);
    prd = p.text;
    truncated.prd = p.truncated;
  }

  let plan: string | null = null;
  const planRaw = renderPlan(input);
  if (planRaw !== null) {
    const p = part(redact, planRaw, PLAN_CAP);
    plan = p.text;
    truncated.plan = p.truncated;
  }

  let previous: string | null = null;
  if (input.previous) {
    const p = part(redact, renderPrevious(input.previous), PREVIOUS_CAP);
    previous = p.text;
    truncated.previous = p.truncated;
  }

  let claims: string | null = null;
  const claimsRaw = input.prSummary ? renderClaims(input.prSummary) : "";
  if (claimsRaw !== "") {
    const p = part(redact, claimsRaw, CLAIMS_CAP);
    claims = p.text;
    truncated.claims = p.truncated;
  }

  // The diff snapshot: merge-base of the target and the head, as the size line computes it. A head
  // (or a base) that is not a full object name is never handed to git: everything reads unavailable.
  let base: string | null = null;
  let rows: PathRow[] = [];
  let renames = new Map<string, string>();
  let attrsFailed = false;
  if (!OBJECT_ID_RE.test(headSha)) {
    log?.warn("delivery context: the head is not a full object name; the diff parts are unavailable");
  } else {
    try {
      const mergeBase = await withinDeadline(() => git.sizeMergeBase(barePath, input.targetBranch, headSha));
      if (!OBJECT_ID_RE.test(mergeBase)) throw new Error("the merge-base is not a full object name");
      const numstat = await withinDeadline(() => git.diffNumstatZ(barePath, mergeBase, headSha));
      const entries = parseNumstatZ(numstat);
      renames = renamesOf(numstat);
      let attrs = new Map<string, PathAttributes>();
      if (entries.length > 0) {
        try {
          attrs = await withinDeadline(() => git.checkAttrZ(barePath, headSha, [...new Set(entries.map((e) => e.path))]));
        } catch (err) {
          // Without the attributes a generated or vendored file cannot be told from code, so no hunk
          // is read at all (a path-rule guess could feed the model a huge generated file as "code").
          // The inventory is still listed, its kinds by the path rules alone.
          attrsFailed = true;
          log?.warn("delivery context: attribute lookup failed; the diff hunks are skipped", { error: errMessage(err) });
        }
      }
      rows = entries.map((e) => ({ ...e, bucket: classifyPath(e.path, attrs.get(e.path)) }));
      base = mergeBase;
    } catch (err) {
      log?.warn("delivery context: diff inventory unavailable", { error: errMessage(err) });
      rows = [];
    }
  }

  // Commit subjects, newest first. The read is bounded; a cut read drops its last record, which
  // may be incomplete (and so may hold the head of a secret the redactor could not match).
  let commits = UNAVAILABLE;
  if (base === null) {
    truncated.commits = true;
  } else if (timeLeft() <= 0) {
    truncated.commits = true;
    log?.warn("delivery context: deadline reached before the commit subjects");
  } else {
    try {
      const read = await boundedRead(
        ["log", "-z", "--format=%s", `--max-count=${MAX_COMMITS + 1}`, `${base}..${headSha}`],
        COMMITS_READ_BYTES,
      );
      let records = read.text.split("\0");
      if (read.truncated) {
        truncated.commits = true;
        records = records.slice(0, -1);
      }
      const subjects = records.filter((s) => s !== "");
      if (subjects.length > MAX_COMMITS) truncated.commits = true;
      const lines = subjects.slice(0, MAX_COMMITS).map((s) => {
        const c = capBytes(redact(s), COMMIT_SUBJECT_CAP);
        if (c.truncated) truncated.commits = true;
        return `- ${c.text}`;
      });
      const empty = read.truncated ? "(no complete commit subject within the read bound)" : "(no commits)";
      const p = capBytes(lines.length > 0 ? lines.join("\n") : empty, COMMITS_CAP);
      commits = truncated.commits && !p.truncated ? `${p.text}\n${TRUNCATED_MARKER}` : p.text;
      truncated.commits ||= p.truncated;
    } catch (err) {
      log?.warn("delivery context: commit subjects unavailable", { error: errMessage(err) });
      commits = UNAVAILABLE;
      truncated.commits = true;
    }
  }

  // Changed-path inventory with per-path numstat, or per top-level directory beyond the limit.
  let paths = UNAVAILABLE;
  if (base === null) {
    truncated.paths = true;
  } else if (rows.length === 0) {
    paths = "(no changed files)";
  } else {
    let inventory = rows.length <= MAX_LISTED_PATHS ? redact(rows.map((r) => renderPathRow(redact, r)).join("\n")) : null;
    if (inventory === null || byteLen(inventory) > PATHS_CAP) {
      inventory = redact(aggregatePaths(redact, rows));
      truncated.paths = true;
    }
    const p = capBytes(inventory, PATHS_CAP);
    paths = p.text;
    truncated.paths ||= p.truncated;
  }

  // Diff hunks fill what the fixed parts left of the aggregate budget.
  const fixedBytes = [issue.text, prd ?? "", plan ?? "", previous ?? "", claims ?? "", commits, paths].reduce(
    (n, s) => n + byteLen(s),
    0,
  );
  let remaining = DELIVERY_CONTEXT_BUDGET_BYTES - fixedBytes;
  let diff = UNAVAILABLE;
  if (base === null) {
    truncated.diff = true;
  } else if (attrsFailed) {
    diff = ATTRS_UNAVAILABLE;
    truncated.diff = true;
  } else {
    const ordered = DIFF_ORDER.flatMap((b) => rows.filter((r) => r.bucket === b));
    const chunks: string[] = [];
    let reads = 0;
    for (const row of ordered) {
      if (remaining < MIN_USEFUL_DIFF_BYTES || reads >= MAX_DIFF_READS) {
        truncated.diff = true;
        break;
      }
      if (timeLeft() <= 0) {
        log?.warn("delivery context: deadline reached; no further diff reads");
        truncated.diff = true;
        break;
      }
      const oldPath = renames.get(row.path);
      // `core.quotePath=true`: git's own headers (`diff --git`, `rename from`...) octal-escape any
      // control or non-ASCII byte in a path, whatever the bare's config says. `--literal-pathspecs`:
      // a path is data, never a pathspec pattern. `--no-ext-diff` and `--no-textconv` are
      // load-bearing: the worker's git env pins `diff.external` (git.ts gitEnv), so a plain diff
      // would print nothing, and a repo's textconv driver must not run.
      const args = [
        "-c",
        "core.quotePath=true",
        "--literal-pathspecs",
        "diff",
        "-M",
        "--no-color",
        "--no-ext-diff",
        "--no-textconv",
        base,
        headSha,
        "--",
        ...(oldPath ? [oldPath] : []),
        row.path,
      ];
      const cap = Math.min(PER_PATH_DIFF_CAP, remaining);
      reads++;
      let read: BoundedRead;
      try {
        read = await boundedRead(args, cap + READ_MARGIN_BYTES);
      } catch (err) {
        log?.warn("delivery context: path diff unavailable", { error: errMessage(err) });
        truncated.diff = true;
        continue;
      }
      if (read.text === "") continue;
      // Redact before cut. A read cut at its bound may end in the head of a secret whose tail was
      // never read, which the redactor cannot match. That head is the END of the redacted text, so
      // dropping its last READ_MARGIN_BYTES removes a head shorter than the margin (the test
      // "a per-path read is bounded..." pins one). Residual: a secret longer than
      // READ_MARGIN_BYTES straddling the read boundary can leave its head in the prompt.
      let text = redact(read.text);
      if (read.truncated) text = utf8Prefix(text, byteLen(text) - READ_MARGIN_BYTES);
      const p = capBytes(text, cap, read.truncated);
      if (p.truncated) truncated.diff = true;
      chunks.push(p.text);
      remaining -= byteLen(p.text) + 1; // + the joining newline
    }
    if (chunks.length > 0) diff = chunks.join("\n");
    else if (truncated.diff) diff = UNAVAILABLE;
    else if (rows.length > 0 && ordered.length === 0) diff = "(only generated or vendored files changed)";
    else diff = "(no diff)";
  }

  const bytes = fixedBytes + byteLen(diff);
  return { issue: issue.text, prd, plan, previous, claims, commits, paths, diff, truncated, bytes };
}
