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
 *    carried no secret variables has nothing to mask, and that is established locally, so a run that never
 *    receives variables is never withheld while its flight is live.
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

const HUNK_HEADER = /^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@/;

/**
 * The pre-image and post-image text of every hunk of a unified diff, rebuilt per file and per hunk. The
 * post-image is the hunk's context and `+` lines, the pre-image its context and `-` lines, each with its
 * prefix stripped and every line terminated by "\n" (a CRLF line keeps its "\r"). A `\ No newline at end of
 * file` marker removes the terminator of the line before it from the image(s) that line belongs to; it is not a
 * content line. Hunks are never joined with one another. Lines between a `diff --git` line and its first hunk
 * are file headers and carry no content.
 *
 * Returns "unparseable" for anything it cannot rebuild exactly: a binary patch, a combined diff, a line that
 * is not a hunk line inside or between hunks, or a hunk whose line counts do not match its header. A caller
 * that must not ship a patch it cannot read withholds it.
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
      // A file header line (index, ---, +++, mode, rename, similarity ...) before the first hunk.
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
  | { withhold: true; reason: "raw_value" | "hunk_image_value" | "unparseable_patch" | "line_form"; variables: string[] };

/**
 * Whether a raw (uncapped, unredacted) preserved patch may be redacted and shipped. It must not carry a
 * whole-value form of a secret variable (raw text, or the rebuilt pre- or post-image of any hunk, where a
 * value split by diff prefixes reads as one run), nor any registered line form of a multi-line secret, and it
 * must be a diff {@link patchImages} can rebuild. Variable names only; never a value.
 */
export function patchScanVerdict(
  rawPatch: string, needles: readonly ScanNeedle[], lineForms: readonly ScanNeedle[],
): PatchVerdict {
  const raw = textHitVariables(rawPatch, needles);
  if (raw.length > 0) return { withhold: true, reason: "raw_value", variables: raw };
  if (needles.length === 0 && lineForms.length === 0) return { withhold: false };
  const rebuilt = patchImages(rawPatch);
  if (rebuilt === "unparseable") return { withhold: true, reason: "unparseable_patch", variables: [] };
  const present = literalsPresent(needles.flatMap((n) => n.forms));
  const hit = new Set<string>();
  for (const image of rebuilt.images) {
    const found = present(image);
    if (found.size > 0) for (const n of needles) if (n.forms.some((f) => found.has(f))) hit.add(n.name);
  }
  if (hit.size > 0) return { withhold: true, reason: "hunk_image_value", variables: [...hit] };
  const lines = textHitVariables(rawPatch, lineForms);
  if (lines.length > 0) return { withhold: true, reason: "line_form", variables: lines };
  return { withhold: false };
}

/** The names of the variables whose forms occur in `text` (a raw patch), in needle order. Used before the
 *  patch is redacted. One pass over the text for every form of every needle. */
export function textHitVariables(text: string, needles: readonly ScanNeedle[]): string[] {
  const formsPresent = literalsPresent(needles.flatMap((n) => n.forms));
  const present = formsPresent(text);
  if (present.size === 0) return [];
  return needles.filter((n) => n.forms.some((f) => present.has(f))).map((n) => n.name);
}
