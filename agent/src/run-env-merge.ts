// Merge of the repository variables a claim delivers into a lead lane's command environment
// (PRD 4416 M2). The merge is the lowest precedence: a user variable is added only when the key is
// absent, its name passes the shared name rules again (a row denied after it was saved may still
// arrive), and the key is not protected. Callers apply their worker-set keys AFTER the merge, so
// those always win. A dropped name is reported through `onDrop` with the rule it broke; the caller
// sanitises the name (runEnvInput in run-env-prompt.ts) before it reaches a run event.

import type { RunEnvVar } from "./run-env-claim.js";
import { runEnvNameViolation } from "./run-env-names.js";

/** Run kinds that receive repository variables: the lead kinds that execute repo code. A task
 *  run that reviews another run (review_target_run_id set) never does. */
const RECEIVING_KINDS: ReadonlySet<string> = new Set(["issue", "prompt", "self_improve", "ci_fix", "mr_rework", "task"]);

export interface RunEnvInput {
  vars: readonly RunEnvVar[];
  /** Called once per dropped variable with its raw name and the rule it broke. */
  onDrop?: (name: string, rule: string) => void;
}

/** The variables a claim hands its executor: empty unless the run is of a receiving kind. */
export function runEnvForClaim(
  claim: { kind?: string; review_target_run_id?: string | null; secrets: { run_env?: readonly RunEnvVar[] } },
): readonly RunEnvVar[] {
  const kind = claim.kind ?? "issue";
  if (!RECEIVING_KINDS.has(kind)) return [];
  if (kind === "task" && claim.review_target_run_id) return [];
  return claim.secrets.run_env ?? [];
}

/** Adds the user variables of `input` to `env` under the lowest-precedence rules above. */
export function mergeRunEnv(
  env: Record<string, string | undefined>,
  input: RunEnvInput | undefined,
  isProtected: (key: string) => boolean,
): void {
  if (!input) return;
  for (const v of input.vars) {
    const violation = runEnvNameViolation(v.name);
    if (violation !== null) { input.onDrop?.(v.name, violation); continue; }
    if (isProtected(v.name)) { input.onDrop?.(v.name, "protected worker key"); continue; }
    if (Object.hasOwn(env, v.name)) { input.onDrop?.(v.name, "key already set by the worker or the tool profile"); continue; }
    env[v.name] = v.value;
  }
}
