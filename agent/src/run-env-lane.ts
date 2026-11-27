// The worker-side mirror of the api's leadcfg.LeadLane (PRD 4416 M2): which runs have a lead agent whose
// commands may receive the run's user variables. The api's attachRunEnv already withholds the variables by
// lane, so a non-lead claim carries none; the worker applies the same predicate again before it hands the
// variables to an executor, as defense in depth against a claim that carries them anyway.

import type { ClaimRunEnvVar, RunKind } from "./protocol.js";

/** The run kinds with a lead agent (leadcfg.LeadLane's kind set). */
const LEAD_LANE_KINDS: ReadonlySet<string> = new Set<RunKind>(["issue", "ci_fix", "prompt", "self_improve", "mr_rework", "task"]);

/** The claim fields the lane depends on. `egressProfileBound` is a claim carrying `isolated_fetch`. */
export interface RunEnvLaneClaim {
  /** Absent means an issue run, as everywhere else in the runner. */
  readonly kind?: string;
  readonly review_target_run_id?: string | null;
  readonly isolated_fetch?: unknown;
}

/** Whether the run has a lead agent: a lead kind, not a review run (a review target) and not bound to an
 *  egress profile (an isolated-lane run). */
export function isRunEnvLeadLane(claim: RunEnvLaneClaim): boolean {
  if (claim.review_target_run_id) return false;
  if (claim.isolated_fetch !== undefined && claim.isolated_fetch !== null) return false;
  return LEAD_LANE_KINDS.has(claim.kind ?? "issue");
}

/** The variables a run's RunContext may carry: the claim's variables for a lead-lane run, else none. */
export function leadLaneRunEnv(claim: RunEnvLaneClaim, runEnv: readonly ClaimRunEnvVar[] | undefined): ClaimRunEnvVar[] | undefined {
  if (!runEnv || runEnv.length === 0 || !isRunEnvLeadLane(claim)) return undefined;
  return [...runEnv];
}

/** The RunContext field for a claim: `{ runEnv }` for a lead-lane run that has variables, else `{}`. Computes
 *  the lane once; the key is absent (not undefined) when there is nothing to hand over. */
export function runEnvContextField(claim: RunEnvLaneClaim, runEnv: readonly ClaimRunEnvVar[] | undefined): { runEnv?: ClaimRunEnvVar[] } {
  const vars = leadLaneRunEnv(claim, runEnv);
  return vars ? { runEnv: vars } : {};
}
