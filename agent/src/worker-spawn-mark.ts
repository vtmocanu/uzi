// issue #1783 (R2/R4) — the run-attempt marker and the worker spawn mark.
//
// The run-quiescence reaper (run-quiescence.ts) decides which runner-uid processes belong to
// which run from two environment facts, set by the WORKER when it spawns something:
//
//   - The ATTEMPT MARKER (`UZI_RUN_ATTEMPT=<runId>:<attemptId>`, with `UZI_RUN_CLONE` and
//     `UZI_RUN_CLONE_KEY`) rides only the agent CLI's env (buildSdkEnv), so everything the
//     agent starts inherits it. It is positive attribution to one execution attempt.
//   - The WORKER MARK (`UZI_WORKER_SPAWN=<nonce>`) rides every runner-uid process the worker
//     itself starts for its own purposes (a runner-uid git, a check, an install, a kill
//     helper). The nonce is random per worker process, so a process the agent forges it into
//     cannot guess it, and a process carrying THIS worker's nonce is never run-owned: the
//     reaper skips it, so a concurrent worker git op survives a reap.
//
// A worker spawn must never carry an attempt marker: workerSpawnEnv strips all three.

import { randomBytes } from "node:crypto";

/** `<runId>:<attemptId>` of the attempt whose agent started this process tree. */
export const RUN_ATTEMPT_ENV = "UZI_RUN_ATTEMPT";
/** The absolute clone path of that attempt. */
export const RUN_CLONE_ENV = "UZI_RUN_CLONE";
/** `<repoDir>/<key>` of that attempt's clone. */
export const RUN_CLONE_KEY_ENV = "UZI_RUN_CLONE_KEY";
/** This worker process's spawn nonce. */
export const WORKER_SPAWN_ENV = "UZI_WORKER_SPAWN";

const NONCE = randomBytes(16).toString("hex");

/** The random per-worker-process nonce carried as {@link WORKER_SPAWN_ENV}. */
export function workerSpawnNonce(): string {
  return NONCE;
}

/**
 * `env` (default: this process's env) plus the worker mark, with every run-attempt marker
 * removed. Use it for the env of every runner-uid process the worker starts for itself.
 */
export function workerSpawnEnv(env: NodeJS.ProcessEnv | undefined = process.env): NodeJS.ProcessEnv {
  const out: NodeJS.ProcessEnv = { ...env };
  delete out[RUN_ATTEMPT_ENV];
  delete out[RUN_CLONE_ENV];
  delete out[RUN_CLONE_KEY_ENV];
  out[WORKER_SPAWN_ENV] = NONCE;
  return out;
}
