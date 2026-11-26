// The fail-closed exact-value scan of a run's secret variables (PRD 4416 M2, "Exact-value scan").
//
// Before each scanned publish every object the publication would carry (commits, trees, blobs: messages,
// paths and contents, in every commit of the outgoing range, not only the final tree) is searched for the
// REGISTERED forms of the run's secret values (run-env-forms.ts: the value in its raw, JSON, HTML-safe and
// percent-encoded forms, the standard and URL-safe base64 alignments, and for a multi-line value its
// diff-shaped forms). Plain values are never needles. The verdict is one of:
//   clean    the range was read in full and holds no needle; it names the exact {tip, excludes} it covers
//   hit      at least one needle occurs; findings carry the variable NAME (never a value) under `uzi-run-env`
//   withhold the range could not be proven clean (deadline, git error, unresolved ref, over the byte cap,
//            the needles could not be resolved): the caller must not publish, and keeps its custody
// There is no fourth outcome. The one place a publish proceeds without needles is resolveScanNeedles: a
// run whose own claim carried no secret variables, an api answer of `{protected:false}`, or a missing route
// with a local marker proving this worker never handed the run variables. This module takes no lock and
// holds no state.

import type { GitCache } from "./git.js";
import { literalsPresent } from "./redact.js";
import type { RunEnvRedactResponse } from "./protocol.js";
import { runEnvClaimForms, type ScanNeedle } from "./run-env-forms.js";
import type { SecretFinding } from "./secret-scan-guard.js";

export type { ScanNeedle } from "./run-env-forms.js";

/** The range a scan covers: every object reachable from `tip` and not from any of `excludes`. */
export interface ScanRange {
  readonly tip: string;
  readonly excludes: readonly string[];
  /** Also exclude everything reachable from the bare's `origin` remote-tracking refs (a branch push). */
  readonly excludeRemoteTracking?: boolean;
}

export type RunEnvScanVerdict =
  | { kind: "clean"; range: ScanRange }
  | { kind: "hit"; findings: SecretFinding[] }
  | { kind: "withhold"; reason: string };

export interface RunEnvScanLimits {
  readonly deadlineMs: number;
  /** The most object bytes the scan may read; counted over the bytes actually streamed. */
  readonly maxBytes?: number;
}

/** Scans `range` in `barePath` for `needles`. No needles is trivially clean for that range. Never returns
 *  clean unless every object of the range was read; a boundary abort from the git layer propagates. */
export async function scanRunEnvRange(
  git: Pick<GitCache, "scanObjectsForRunEnv">,
  barePath: string,
  range: ScanRange,
  needles: readonly ScanNeedle[],
  limits: RunEnvScanLimits,
): Promise<RunEnvScanVerdict> {
  if (needles.length === 0) return { kind: "clean", range };
  const res = await git.scanObjectsForRunEnv(barePath, {
    tip: range.tip, excludes: range.excludes, ...(range.excludeRemoteTracking ? { excludeRemoteTracking: true } : {}),
  }, needles, {
    deadlineMs: limits.deadlineMs,
    ...(limits.maxBytes === undefined ? {} : { maxBytes: limits.maxBytes }),
  });
  if (!res.trusted) return { kind: "withhold", reason: res.reason };
  if (res.findings.length > 0) return { kind: "hit", findings: res.findings };
  return { kind: "clean", range };
}

/** Whether some clean verdict covers exactly the candidate about to be published: same tip, and, when the
 *  caller names the exclusion set the publication uses, the same exclusion set. A mismatch means the published
 *  bytes are not the scanned bytes: refuse. */
export function scanCoversCandidate(
  verdicts: readonly RunEnvScanVerdict[], candidate: { tip: string; excludes?: readonly string[] },
): boolean {
  return verdicts.some((v) => {
    if (v.kind !== "clean" || v.range.tip !== candidate.tip) return false;
    if (candidate.excludes === undefined) return true;
    const a = [...v.range.excludes].sort();
    const b = [...candidate.excludes].sort();
    return a.length === b.length && a.every((x, i) => x === b[i]);
  });
}

/** The variable names a verdict's findings carry, deduplicated, in first-seen order. */
export function hitVariableNames(findings: readonly SecretFinding[]): string[] {
  return [...new Set(findings.flatMap((f) => (typeof f.variable === "string" ? [f.variable] : [])))];
}

/** The `code` of a JSON error body, or undefined when the body is not a JSON object with a string `code`. */
function errorCode(body: unknown): string | undefined {
  if (typeof body !== "string") return undefined;
  try {
    const parsed: unknown = JSON.parse(body);
    const code = typeof parsed === "object" && parsed !== null ? (parsed as { code?: unknown }).code : undefined;
    return typeof code === "string" ? code : undefined;
  } catch {
    return undefined;
  }
}

/** The live-set marker and withhold reason of a claim whose forms passed the form budget (no needle set exists). */
export const FORMS_OVER_BUDGET = "forms_over_budget" as const;

export type NeedleResolution =
  | { kind: "needles"; needles: ScanNeedle[] }
  | { kind: "withhold"; reason: string };

/**
 * Where the needles of a publish come from.
 *  - A live flight uses the set its own claim carried (`live`), including an empty set: a run whose claim
 *    carried no secret variables has nothing to mask.
 *  - With no flight (the run is terminal, the worker restarted, or a held or terminal publication lane runs
 *    after the flight is gone) the api is asked. `{protected:false}` means no needles, and is also the answer
 *    for a run with no governing run (neither a lead-lane run nor a cross-check child).
 *  - A 404 carrying the code `run_env_run_not_found` (the run is deleted or is not this worker's) withholds.
 *    A 409 carrying `run_env_redact_closed` (the protection window is closed) withholds, as does any other
 *    409 (vault locked, a receipted version unavailable), a network or decode error, and any other status.
 *  - A 404 with any other code withholds. A 404 with no code means the route itself is absent (a proxy or an older api answered). It yields
 *    no needles only when the local marker proves this worker never handed the run variables
 *    (`runEnvSeen` resolves `false`); a marker that exists, a read error (`"unknown"`) or a missing check
 *    withholds.
 * A withheld publish is refused now and retried later.
 */
export async function resolveScanNeedles(opts: {
  live: readonly ScanNeedle[] | typeof FORMS_OVER_BUDGET | undefined;
  fetchProtected: () => Promise<RunEnvRedactResponse>;
  /** Whether this worker ever handed the run variables: see run-env-seen.ts. */
  runEnvSeen?: () => Promise<boolean | "unknown">;
}): Promise<NeedleResolution> {
  if (opts.live === FORMS_OVER_BUDGET) return { kind: "withhold", reason: FORMS_OVER_BUDGET };
  if (opts.live !== undefined) return { kind: "needles", needles: [...opts.live] };
  let body: RunEnvRedactResponse;
  try {
    body = await opts.fetchProtected();
  } catch (e) {
    const status = typeof e === "object" && e !== null && typeof (e as { status?: unknown }).status === "number"
      ? (e as { status: number }).status : undefined;
    if (status === undefined) return { kind: "withhold", reason: "protection_unavailable" };
    const code = errorCode((e as { body?: unknown }).body);
    if (status === 404 && code === "run_env_run_not_found") return { kind: "withhold", reason: "protection_run_not_found" };
    if (status === 409 && code === "run_env_redact_closed") return { kind: "withhold", reason: "protection_closed" };
    if (status === 404 && code === undefined && opts.runEnvSeen !== undefined) {
      let seen: boolean | "unknown";
      try {
        seen = await opts.runEnvSeen();
      } catch {
        seen = "unknown";
      }
      if (seen === false) return { kind: "needles", needles: [] };
      return { kind: "withhold", reason: seen === true ? "protection_route_missing_variables_seen" : "protection_route_missing_marker_unreadable" };
    }
    return { kind: "withhold", reason: `protection_http_${status}` };
  }
  if (!body.protected) return { kind: "needles", needles: [] };
  const forms = runEnvClaimForms(undefined, body.versions);
  if (forms.overBudget) return { kind: "withhold", reason: FORMS_OVER_BUDGET };
  return { kind: "needles", needles: forms.needles };
}

/** The prefixes of the extended header lines patchImages accepts between a `diff --git` line and the first
 *  hunk of a file. Any other line there makes the patch unparseable. */
const FILE_HEADER_PREFIXES = [
  "index ", "--- ", "+++ ", "old mode ", "new mode ", "new file mode ", "deleted file mode ",
  "similarity index ", "dissimilarity index ", "rename from ", "rename to ", "copy from ", "copy to ",
];

const HUNK_HEADER = /^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@/;

/**
 * The pre-image and post-image text of every hunk of a unified diff, rebuilt per file and per hunk. The
 * post-image is the hunk's context and `+` lines, the pre-image its context and `-` lines, each with its
 * prefix stripped and every line terminated by "\n" (a CRLF line keeps its "\r"). A `\ No newline at end of
 * file` marker removes the terminator of the line before it from the image(s) that line belongs to; it is not a
 * content line. Hunks are never joined with one another. Lines between a `diff --git` line and its first hunk
 * are file headers and carry no content, provided each starts with one of FILE_HEADER_PREFIXES.
 *
 * Returns "unparseable" for anything it cannot rebuild exactly: a binary patch, a combined diff, a line that
 * is not a hunk line inside or between hunks, a line between a `diff --git` line and its first hunk that is
 * not a recognised file header line, or a hunk whose line counts do not match its header. A caller that must
 * not ship a patch it cannot read withholds it.
 */
export function patchImages(patch: string): { images: string[] } | "unparseable" {
  const lines = patch.split("\n");
  if (lines[lines.length - 1] === "") lines.pop();
  const images: string[] = [];
  let inFile = false;
  let afterHunk = false;
  let i = 0;
  while (i < lines.length) {
    const line = lines[i]!;
    if (line.startsWith("diff --git ")) {
      inFile = true;
      afterHunk = false;
      i++;
      continue;
    }
    if (line.startsWith("diff --cc ") || line.startsWith("diff --combined ") || line.startsWith("@@@")) return "unparseable";
    if (!inFile) {
      // Text before the first file header carries no hunk: only a hunk header makes it unreadable.
      if (line.startsWith("@@")) return "unparseable";
      i++;
      continue;
    }
    const head = HUNK_HEADER.exec(line);
    if (head === null) {
      if (line.startsWith("GIT binary patch") || (line.startsWith("Binary files ") && line.endsWith(" differ"))) return "unparseable";
      if (line.startsWith("@@")) return "unparseable";
      // After a hunk only another hunk or the next file may follow.
      if (afterHunk) return "unparseable";
      if (!FILE_HEADER_PREFIXES.some((p) => line.startsWith(p))) return "unparseable";
      i++;
      continue;
    }
    let oldLeft = head[2] === undefined ? 1 : Number(head[2]);
    let newLeft = head[4] === undefined ? 1 : Number(head[4]);
    const pre: string[] = [];
    const post: string[] = [];
    let preOpen = true;
    let postOpen = true;
    let last: " " | "-" | "+" | undefined;
    i++;
    while (i < lines.length && (oldLeft > 0 || newLeft > 0 || lines[i]!.startsWith("\\"))) {
      const l = lines[i]!;
      if (l.startsWith("\\")) {
        if (last === undefined) return "unparseable";
        if (last !== "+") preOpen = false;
        if (last !== "-") postOpen = false;
        i++;
        continue;
      }
      const prefix = l === "" ? " " : l[0]!;
      const body = l === "" ? "" : l.slice(1);
      if (prefix === " ") {
        if (oldLeft === 0 || newLeft === 0) return "unparseable";
        pre.push(body);
        post.push(body);
        oldLeft--;
        newLeft--;
      } else if (prefix === "-") {
        if (oldLeft === 0) return "unparseable";
        pre.push(body);
        oldLeft--;
      } else if (prefix === "+") {
        if (newLeft === 0) return "unparseable";
        post.push(body);
        newLeft--;
      } else {
        return "unparseable";
      }
      last = prefix as " " | "-" | "+";
      i++;
    }
    if (oldLeft !== 0 || newLeft !== 0) return "unparseable";
    afterHunk = true;
    // preOpen / postOpen went false at a no-newline marker that followed a line of that image.
    const image = (rows: string[], terminated: boolean): string => (rows.length === 0 ? "" : rows.join("\n") + (terminated ? "\n" : ""));
    for (const text of [image(pre, preOpen), image(post, postOpen)]) if (text !== "") images.push(text);
  }
  return { images };
}

export type PatchVerdict =
  | { withhold: false }
  | { withhold: true; reason: "raw_value" | "hunk_image_value" | "unparseable_patch" | "line_form" | "header_path" | "multi_line_secret"; variables: string[] };

/** The file header lines of a diff that name a path (git C-quotes such a path unless core.quotePath is off). */
const PATH_HEADER_PREFIXES = ["diff --git ", "--- ", "+++ ", "rename from ", "rename to ", "copy from ", "copy to ", "Binary files "];
const C_ESCAPES: Record<string, number> = { a: 7, b: 8, t: 9, n: 10, v: 11, f: 12, r: 13, '"': 34, "\\": 92 };

/** Git's C-style unquoting of one quoted path body (the text between the quotes): the escapes `\a \b \t \n \v
 *  \f \r \" \\` and three-digit octal bytes, the resulting bytes decoded as UTF-8. */
function cUnquote(body: string): string {
  const bytes: number[] = [];
  for (let i = 0; i < body.length; i++) {
    const c = body[i]!;
    if (c !== "\\") {
      // Read by code point, so an astral character is encoded as one UTF-8 sequence.
      const cp = body.codePointAt(i)!;
      for (const b of Buffer.from(String.fromCodePoint(cp), "utf8")) bytes.push(b);
      if (cp > 0xffff) i += 1;
      continue;
    }
    const next = body[i + 1] ?? "";
    const octal = /^[0-3][0-7]{2}/.exec(body.slice(i + 1, i + 4));
    if (octal) { bytes.push(parseInt(octal[0], 8)); i += 3; }
    else if (next in C_ESCAPES) { bytes.push(C_ESCAPES[next]!); i += 1; }
    else bytes.push(92);
  }
  return Buffer.from(bytes).toString("utf8");
}

const QUOTED_SEGMENT = /"((?:[^"\\]|\\.)*)"/g;
const C_QUOTE_NAMES: Record<number, string> = { 7: "\\a", 8: "\\b", 9: "\\t", 10: "\\n", 11: "\\v", 12: "\\f", 13: "\\r", 34: '\\"', 92: "\\\\" };

/** Quote text the way git quotes a path: wrapped in `"`, with `\`, `"` and control characters escaped. */
function cQuote(text: string): string {
  let out = '"';
  for (const ch of text) {
    const code = ch.codePointAt(0)!;
    if (code in C_QUOTE_NAMES) out += C_QUOTE_NAMES[code];
    else if (code < 0x20 || code === 0x7f) out += `\\${code.toString(8).padStart(3, "0")}`;
    else out += ch;
  }
  return `${out}"`;
}

/** The line with every quoted segment replaced by its decoded text. */
function decodeAllQuoted(line: string): string {
  return line.replace(QUOTED_SEGMENT, (_whole, body: string) => cUnquote(body));
}

/** Redact one raw `git status --porcelain` line. Git C-quotes a path with special or non-ASCII characters, so a
 *  secret value inside such a path is split by escapes and the redactor misses it in the quoted form. Each
 *  quoted segment is decoded first, and a segment whose decoded text meets the redactor is replaced by the
 *  redacted decoded text, re-quoted so a rename stays unambiguous. A value can also span two sides of a rename
 *  (one side quoted), so the result is accepted only when its fully decoded form (every quoted segment
 *  unquoted) and its raw form hold nothing for the redactor; otherwise the redacted fully decoded line is
 *  returned instead, unquoted. With no match the line is returned unchanged (the same string). */
export function redactPorcelainLine(line: string, redactText: (text: string) => string): string {
  const segmented = line.replace(QUOTED_SEGMENT, (whole, body: string) => {
    const decoded = cUnquote(body);
    const redacted = redactText(decoded);
    return redacted !== decoded ? cQuote(redacted) : whole;
  });
  const decodedSegmented = decodeAllQuoted(segmented);
  if (redactText(decodedSegmented) === decodedSegmented && redactText(segmented) === segmented) return segmented;
  return redactText(decodeAllQuoted(line));
}

/** The names of the variables whose forms occur in a path named by a diff header line, quoted or not. A path is
 *  matched as "/" + path + "/" so a form with a leading or trailing "/" meets a root directory or a leaf too. */
function headerPathHits(patch: string, needles: readonly ScanNeedle[]): string[] {
  const hit = new Set<string>();
  const forms = needles.map((n) => ({ name: n.name, forms: n.forms.filter((f) => f.length > 0) }));
  const present = literalsPresent(forms.flatMap((n) => n.forms));
  for (const line of patch.split("\n")) {
    const prefix = PATH_HEADER_PREFIXES.find((p) => line.startsWith(p));
    if (prefix === undefined) continue;
    const quoted: string[] = [];
    const decoded = line.slice(prefix.length).replace(/"((?:[^"\\]|\\.)*)"/g, (_m, body: string) => {
      const text = cUnquote(body);
      quoted.push(text);
      return text;
    });
    // The decoded line, each quoted path alone, and the sides of a `diff --git a/.. b/..` or `Binary files .. and ..` line.
    const candidates = [decoded, ...quoted, ...decoded.split(/ (?=b\/)| and /)];
    for (const candidate of candidates) {
      const found = present(`/${candidate.replace(/\t$/, "")}/`);
      if (found.size > 0) for (const n of forms) if (n.forms.some((f) => found.has(f))) hit.add(n.name);
    }
  }
  return [...hit];
}

/**
 * Whether a raw (uncapped, unredacted) preserved patch may be redacted and shipped. It must not carry a
 * whole-value form of a secret variable (raw text, or the rebuilt pre- or post-image of any hunk, where a
 * value split by diff prefixes reads as one run), nor any registered line form of a multi-line secret, and it
 * must be a diff {@link patchImages} can rebuild. A run holding any multi-line secret (`multiLine`, the names
 * from multiLineSecretVariables, with no length floor) is always withheld. Variable names only; never a value.
 */
export function patchScanVerdict(
  rawPatch: string, needles: readonly ScanNeedle[], lineForms: readonly ScanNeedle[], multiLine: readonly string[],
): PatchVerdict {
  const raw = textHitVariables(rawPatch, needles);
  if (raw.length > 0) return { withhold: true, reason: "raw_value", variables: raw };
  const lines = textHitVariables(rawPatch, lineForms);
  if (lines.length > 0) return { withhold: true, reason: "line_form", variables: lines };
  // A run holding a multi-line secret (of any length) never ships a patch: fragments of its lines split across
  // files or separated by elided context between hunks cannot be matched reliably, whatever their length.
  if (multiLine.length > 0) return { withhold: true, reason: "multi_line_secret", variables: [...multiLine] };
  if (needles.length === 0 && lineForms.length === 0) return { withhold: false };
  // Header paths are decoded from git's C-quoting ("\303\251", "\t", "\"", "\\") and searched.
  const header = headerPathHits(rawPatch, needles);
  if (header.length > 0) return { withhold: true, reason: "header_path", variables: header };
  const rebuilt = patchImages(rawPatch);
  if (rebuilt === "unparseable") return { withhold: true, reason: "unparseable_patch", variables: [] };
  const present = literalsPresent(needles.flatMap((n) => n.forms));
  const hit = new Set<string>();
  for (const image of rebuilt.images) {
    // Each image is read as written and with CRLF and lone CR normalized to LF, as valueVariants does for a value.
    for (const text of new Set([image, image.replace(/\r\n?/g, "\n")])) {
      const found = present(text);
      if (found.size > 0) for (const n of needles) if (n.forms.some((f) => found.has(f))) hit.add(n.name);
    }
  }
  if (hit.size > 0) return { withhold: true, reason: "hunk_image_value", variables: [...hit] };
  return { withhold: false };
}

/** The names of the variables whose forms occur in `text` (a raw patch), in needle order. Used before the
 *  patch is redacted. */
export function textHitVariables(text: string, needles: readonly ScanNeedle[]): string[] {
  const formsPresent = literalsPresent(needles.flatMap((n) => n.forms));
  const present = formsPresent(text);
  if (present.size === 0) return [];
  return needles.filter((n) => n.forms.some((f) => present.has(f))).map((n) => n.name);
}
