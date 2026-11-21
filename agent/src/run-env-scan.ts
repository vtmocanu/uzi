// The run-env exact-value scan (PRD #4416, "Exact-value scan"). A secret variable's value can leave
// the worker inside a published commit (a message, a file, a filename, a merge result, a base64 login
// entry). Nothing outside the worker knows these values, so unlike the gitleaks path
// (agent/src/secret-scan-guard.ts), which fails open to forge push protection, this scan FAILS CLOSED:
// a timeout, a git error, an unresolved ref, an over-cap stream or an unreachable form source withholds.
//
// This module is the policy layer: it resolves WHICH forms to search (a live flight's claim set, else
// the API's maintenance endpoint), runs the git object walk (GitCache.scanObjectsForRunEnv /
// scanBundleForRunEnv in agent/src/git.ts), maps every result to clean / hit / withhold, and states
// the binding rule (the verdict belongs to the exact scanned candidate). No value is ever returned,
// logged or put in a reason; a hit carries the variable NAME only.

import type { GitCache } from "./git.js";
import type { SecretFinding } from "./secret-scan-guard.js";
import { runEnvNamedForms, type RunEnvNamedForms } from "./run-env-forms.js";
import type { ClaimRunEnvRedact } from "./protocol.js";

/** Most bytes one scan may stream or one bundle may hold; over it the publish is withheld, never sent unscanned. */
export const RUN_ENV_SCAN_MAX_BYTES = 256 * 1024 * 1024;
/** Default per-scan deadline when the caller names none. */
export const RUN_ENV_SCAN_DEADLINE_MS = 60_000;

/** What a scan proved about a range. `scanned` is what the verdict binds to. */
export interface ScannedRange {
  readonly tip: string;
  readonly excludes: readonly string[];
}

export type RunEnvScanOutcome =
  /** Every form was searched in every object of the range and none occurs. `scanned` is the binding. */
  | { kind: "clean"; scanned: ScannedRange }
  /** At least one form occurs. Findings carry commits and variable NAMES; `variables` is the distinct names. */
  | { kind: "hit"; findings: SecretFinding[]; variables: string[] }
  /** The scan could not prove the range clean. The publish must be withheld. `reason` is a fixed token. */
  | { kind: "withhold"; reason: string };

type RunEnvScanGit = Pick<GitCache, "scanObjectsForRunEnv">;

/** Distinct variable names of a hit, in first-seen order. */
function namesOf(findings: readonly SecretFinding[]): string[] {
  const names: string[] = [];
  for (const f of findings) if (typeof f.variable === "string" && !names.includes(f.variable)) names.push(f.variable);
  return names;
}

/**
 * Scan every object `tip --not excludes` introduces (commits, trees and blobs: messages, paths and
 * contents of every commit of the range, not only the final tree) for the registered forms of
 * `vars`. With no variables nothing is searched and no git child runs. `maxBytes` counts the bytes actually
 * streamed. Fail closed: any untrusted result, and a thrown error that `rethrow` does not claim, is `withhold`.
 * `rethrow` lets the caller keep its own abort/boundary errors propagating.
 */
export async function scanRunEnvRange(
  git: RunEnvScanGit,
  barePath: string,
  range: ScannedRange & { excludeRemoteTracking?: boolean },
  vars: RunEnvNamedForms,
  opts: { deadlineMs?: number; maxBytes?: number; rethrow?: (error: unknown) => boolean } = {},
): Promise<RunEnvScanOutcome> {
  if (vars.length === 0) return { kind: "clean", scanned: { tip: range.tip, excludes: [...range.excludes] } };
  try {
    const res = await git.scanObjectsForRunEnv(barePath, range, vars, {
      deadlineMs: opts.deadlineMs ?? RUN_ENV_SCAN_DEADLINE_MS,
      maxBytes: opts.maxBytes ?? RUN_ENV_SCAN_MAX_BYTES,
    });
    if (!res.trusted) return { kind: "withhold", reason: res.reason };
    if (res.findings.length > 0) return { kind: "hit", findings: res.findings, variables: namesOf(res.findings) };
    return { kind: "clean", scanned: { tip: range.tip, excludes: [...range.excludes] } };
  } catch (e) {
    if (opts.rethrow?.(e)) throw e;
    return { kind: "withhold", reason: "scan_threw" };
  }
}

/** The binding rule: a clean verdict covers the candidate only when the candidate is exactly the scanned
 *  tip and its outgoing exclusion set equals the scanned one (order and duplicates aside). */
export function sameScannedCandidate(scanned: ScannedRange, candidate: ScannedRange): boolean {
  if (scanned.tip !== candidate.tip) return false;
  const a = new Set(scanned.excludes);
  const b = new Set(candidate.excludes);
  return a.size === b.size && [...a].every((x) => b.has(x));
}

/** A raw patch (the `preserved_patch` text) contains a registered form: the variable names it holds. */
export function runEnvPatchHits(vars: RunEnvNamedForms, patch: string): string[] {
  const names: string[] = [];
  for (const v of vars) if (v.forms.some((f) => f.length > 0 && patch.includes(f))) names.push(v.name);
  return names;
}

/** Why a publish is withheld, as a fixed token (never a value, path or server text). */
export type RunEnvFormsResolution =
  | { kind: "forms"; vars: RunEnvNamedForms; source: "flight" | "endpoint" }
  | { kind: "withhold"; reason: string };

export interface RunEnvFormsDeps {
  /** The forms of a LIVE flight of `runId` (its claim's `run_env` secrets plus `run_env_redact`), else undefined. */
  live: (runId: string) => RunEnvNamedForms | undefined;
  /** GET /api/worker/runs/{id}/run-env-redact (agent/src/client.ts getRunEnvRedact). */
  client: { getRunEnvRedact(runId: string, signal?: AbortSignal): Promise<{ protected: boolean; versions: ClaimRunEnvRedact[] }> };
}

function classifyEndpointFailure(error: unknown): string {
  const e = error as { name?: unknown; status?: unknown; body?: unknown } | null;
  if (e && e.name === "RunEnvClaimError") return "run_env_malformed";
  if (e && e.name === "RequestError" && typeof e.status === "number") {
    if (e.status === 404) return "run_env_run_not_found";
    if (e.status === 409) {
      const body = typeof e.body === "string" ? e.body : "";
      if (body.includes("vault_locked")) return "run_env_vault_locked";
      if (body.includes("run_env_history_unavailable")) return "run_env_history_unavailable";
      return "run_env_conflict";
    }
    return "run_env_endpoint_error";
  }
  return "run_env_unreachable";
}

/**
 * The forms to search for a run's publication: a live flight's own claim set, otherwise (a restart, a
 * terminal pack, a recovery sweep with no claim) the maintenance endpoint. `protected: false` is the one
 * answer that means "nothing to scan". A 404, a 409, a network error, a timeout or a malformed body is
 * `withhold`: the caller keeps custody and retries later. An error is never read as unprotected.
 */
export async function runEnvFormsFor(runId: string, deps: RunEnvFormsDeps, signal?: AbortSignal): Promise<RunEnvFormsResolution> {
  const live = deps.live(runId);
  if (live !== undefined) return { kind: "forms", vars: live, source: "flight" };
  try {
    const res = await deps.client.getRunEnvRedact(runId, signal);
    if (!res.protected) return { kind: "forms", vars: [], source: "endpoint" };
    return { kind: "forms", vars: runEnvNamedForms(res.versions), source: "endpoint" };
  } catch (e) {
    return { kind: "withhold", reason: classifyEndpointFailure(e) };
  }
}
