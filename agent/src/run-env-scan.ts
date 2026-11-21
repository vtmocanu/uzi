// The fail-closed exact-value scan of a run's secret variables (PRD 4416 M2, "Exact-value scan").
//
// Before each scanned publish every object the publication would carry (commits, trees, blobs: messages,
// paths and contents, in every commit of the outgoing range, not only the final tree) is searched for the
// REGISTERED forms of the run's secret values (run-env-forms.ts: the value, its JSON-escaped form and the
// three base64 alignments). Plain values are never needles. The verdict is one of:
//   clean    the range was read in full and holds no needle; it names the exact {tip, excludes} it covers
//   hit      at least one needle occurs; findings carry the variable NAME (never a value) under `uzi-run-env`
//   withhold the range could not be proven clean (deadline, git error, unresolved ref, over the byte cap,
//            the needles could not be resolved): the caller must not publish, and keeps its custody
// There is no fourth outcome and no fail-open path. This module takes no lock and holds no state.

import type { GitCache } from "./git.js";
import type { RunEnvRedactResponse } from "./protocol.js";
import { runEnvScanNeedles, type ScanNeedle } from "./run-env-forms.js";
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
 *    for a run with no governing run (neither a lead-lane run nor a cross-check child). A 404 (the run is
 *    deleted or is not this worker's), a 409 (vault locked or a receipted version
 *    unavailable), a network or decode error all withhold: nothing local can prove the run holds no secret
 *    versions, so the publish is refused now and retried later.
 */
export async function resolveScanNeedles(opts: {
  live: readonly ScanNeedle[] | undefined;
  fetchProtected: () => Promise<RunEnvRedactResponse>;
}): Promise<NeedleResolution> {
  if (opts.live !== undefined) return { kind: "needles", needles: [...opts.live] };
  let body: RunEnvRedactResponse;
  try {
    body = await opts.fetchProtected();
  } catch (e) {
    const status = typeof e === "object" && e !== null && typeof (e as { status?: unknown }).status === "number"
      ? (e as { status: number }).status : undefined;
    return { kind: "withhold", reason: status === undefined ? "protection_unavailable" : `protection_http_${status}` };
  }
  if (!body.protected) return { kind: "needles", needles: [] };
  return { kind: "needles", needles: runEnvScanNeedles(undefined, body.versions) };
}

/** The names of the variables whose forms occur in `text` (a raw patch). Used before the patch is redacted. */
export function textHitVariables(text: string, needles: readonly ScanNeedle[]): string[] {
  return needles.filter((n) => n.forms.some((f) => f.length > 0 && text.includes(f))).map((n) => n.name);
}
