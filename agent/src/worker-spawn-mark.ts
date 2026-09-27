// issue #1783 (R2/R4) — the run-attempt marker and the worker spawn mark.
//
// The run-quiescence reaper (run-quiescence.ts) decides which runner-uid processes belong to
// which run from two environment facts, set by the WORKER when it spawns something:
//
//   - The ATTEMPT MARKER (`UZI_RUN_ATTEMPT=<runId>:<attemptId>`, with `UZI_RUN_CLONE` and
//     `UZI_RUN_CLONE_KEY`) rides only the agent CLI's env (buildSdkEnv), so everything the
//     agent starts inherits it. It is positive attribution to one execution attempt.
//   - The WORKER MARK (`UZI_WORKER_SPAWN=<nonce>`) rides only the runner-uid processes the
//     worker itself starts from WORKER-AUTHORED, fixed argv (a runner-uid git with hooks pinned
//     off, a kill or probe helper, the quiescence helper). A process carrying THIS worker's
//     nonce and NO attempt marker is never run-owned: the reaper skips it, so a concurrent
//     worker git op survives a reap. A spawn that executes repo- or agent-authored code (a
//     self-improve check, a dependency install, provisioning builds, a Codex provider or command
//     root) is deliberately NOT marked: anything it leaks must stay reapable.
//
// Threat model: the mark defends against ACCIDENTAL escape only. The nonce is random per worker
// process and never enters a model-directed env, but it is not a secret against a deliberate
// forgery: a runner-uid process can read a live marked sibling's environ from procfs. A
// deliberate forgery is out of scope here. The reaper narrows it anyway: a process that carries
// an attempt marker is never exempted by the mark.
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
 * removed. Use it ONLY for a runner-uid process the worker starts from worker-authored, fixed
 * argv; never for one that executes repo- or agent-authored code (see the header).
 */
export function workerSpawnEnv(env: NodeJS.ProcessEnv | undefined = process.env): NodeJS.ProcessEnv {
  const out: NodeJS.ProcessEnv = { ...env };
  delete out[RUN_ATTEMPT_ENV];
  delete out[RUN_CLONE_ENV];
  delete out[RUN_CLONE_KEY_ENV];
  out[WORKER_SPAWN_ENV] = NONCE;
  return out;
}

// ─── Worker-launched long-lived runner-uid roots ──────────────────────────────────────────

const workerRunnerRoots = new Set<number>();

/**
 * Record a long-lived runner-uid ROOT the worker itself launched (a Codex provider supervisor).
 * Such a root makes itself non-dumpable, so its env and cwd read back EACCES; the reaper then
 * attributes it (and its descendants, by ppid chain, group or session) through this registry
 * instead of reporting it `unverified`. Returns the unregister function; call it once the root
 * has exited.
 */
export function registerWorkerRunnerRoot(pid: number | undefined): () => void {
  if (pid === undefined || !Number.isInteger(pid) || pid <= 1) return () => undefined;
  workerRunnerRoots.add(pid);
  return () => {
    workerRunnerRoots.delete(pid);
  };
}

/** The live worker-launched runner-uid roots (see {@link registerWorkerRunnerRoot}). */
export function workerRunnerRootPids(): number[] {
  return [...workerRunnerRoots];
}
