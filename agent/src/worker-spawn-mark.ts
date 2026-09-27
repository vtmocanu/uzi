// issue #1783 (R2/R4) — the run-attempt marker and the worker spawn mark.
//
// The run-quiescence reaper (run-quiescence.ts) decides which runner-uid processes belong to
// which run from two environment facts, set by the WORKER when it spawns something:
//
//   - The ATTEMPT MARKER (`UZI_RUN_ATTEMPT=<runId>:<attemptId>`, with `UZI_RUN_CLONE` and
//     `UZI_RUN_CLONE_KEY`) rides only the agent CLI's env (buildSdkEnv), so everything the
//     agent starts inherits it. It is positive attribution to one execution attempt.
//   - The WORKER MARK (`UZI_WORKER_SPAWN=<nonce>`) rides only the runner-uid processes the
//     worker itself starts from WORKER-AUTHORED, fixed argv that can execute NO repo- or
//     agent-configured code: a kill or probe helper, the quiescence helper, and a runner-clone
//     git whose subcommand touches neither the working tree nor a configurable driver
//     (rev-parse, rev-list, update-ref, config, ls-files, ls-tree, check-ignore, and the seed's
//     `clone --no-checkout`). A process carrying THIS worker's nonce and NO attempt marker is
//     never run-owned: the reaper skips it, so a concurrent worker op survives a reap.
//     Everything that can execute repo- or agent-authored code is deliberately NOT marked, so
//     whatever it leaks stays reapable: a self-improve check, a dependency install, provisioning
//     builds, a Codex provider or command root, the stub executor's git, and EVERY runner-clone
//     git that can run code the clone configures. The clone's `.git/config` and `.gitattributes`
//     are agent-writable, and `filter.<name>.*`, `diff.<name>.*`, `merge.<name>.driver` and
//     `gpg.program` cannot be pinned off (git.ts), so `status`, `add`, `commit`, `checkout`,
//     `reset`, `merge`, `rebase`, `cherry-pick`, `rm`, `clean`, `diff` and `log` run UNMARKED
//     (runnerGitCarriesWorkerMark in git.ts). A filter such a git starts after a quiescence
//     proof is therefore reapable, and every sink that runs one between its proof and a
//     credentialed step re-runs the process proof first (runner.ts).
//
// Threat model: the mark defends against ACCIDENTAL escape only. The nonce is random per worker
// process and never enters a model-directed env, but it is not a secret against a deliberate
// forgery: a runner-uid process can read a live marked sibling's environ from procfs. A
// deliberate forgery is out of scope here. The reaper narrows it anyway: a process that carries
// an attempt marker is never exempted by the mark.
//
// A worker spawn must never carry an attempt marker: workerSpawnEnv strips all three.

import { randomBytes } from "node:crypto";
import fs from "node:fs";
import path from "node:path";

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

/**
 * `env` with every run-attempt marker AND the worker mark removed: the env for a worker-started
 * runner-uid process that may execute repo- or agent-configured code (see the header), so that
 * whatever it leaks stays reapable.
 */
export function unmarkedSpawnEnv(env: NodeJS.ProcessEnv): NodeJS.ProcessEnv {
  const out: NodeJS.ProcessEnv = { ...env };
  delete out[RUN_ATTEMPT_ENV];
  delete out[RUN_CLONE_ENV];
  delete out[RUN_CLONE_KEY_ENV];
  delete out[WORKER_SPAWN_ENV];
  return out;
}

// ─── Recorded roots ───────────────────────────────────────────────────────────────────────

/**
 * A recorded process ROOT: its pid AND its start time (field 22 of the pid's procfs `stat`, in
 * clock ticks since boot), captured when the root was recorded. A pid alone is not an identity:
 * once the root exits the kernel can hand the same pid to an unrelated process, so the reaper
 * matches a root only when the pid and the live start time both agree.
 */
export interface RecordedRoot {
  pid: number;
  startTime: number;
}

/** The start time (field 22) of one procfs `stat` line, or undefined when unparsable. The comm
 *  (field 2) may itself contain ")" and spaces, so fields are counted after the LAST ")". */
export function statStartTime(stat: string): number | undefined {
  const close = stat.lastIndexOf(")");
  if (close < 0) return undefined;
  // fields[0] is field 3 (state), so field 22 is fields[19].
  const raw = stat.slice(close + 2).trim().split(/\s+/)[19];
  if (raw === undefined || !/^\d+$/.test(raw)) return undefined;
  const v = Number(raw);
  return Number.isSafeInteger(v) ? v : undefined;
}

const PROC_ROOT = path.join("/", "proc");

/** `pid`'s start time read from procfs now, or undefined when it cannot be read (the pid is gone,
 *  or this is not Linux). The `stat` file is world-readable, so this works across the uid split. */
function procStartTime(pid: number): number | undefined {
  try {
    return statStartTime(fs.readFileSync(path.join(PROC_ROOT, String(pid), "stat"), "utf8"));
  } catch {
    return undefined;
  }
}

/** Reads a pid's start time (procfs by default; injectable for a test's fake pids). */
export type StartTimeReader = (pid: number) => number | undefined;

/** `pid` as a {@link RecordedRoot}, its start time read NOW (call it at spawn), or undefined when
 *  the pid is not a plausible root or its start time cannot be read. An unrecorded root exempts
 *  nothing: the reaper then fails closed on its unreadable descendants. */
export function recordRoot(pid: number | undefined, readStartTime: StartTimeReader = procStartTime): RecordedRoot | undefined {
  if (pid === undefined || !Number.isInteger(pid) || pid <= 1) return undefined;
  const startTime = readStartTime(pid);
  return startTime === undefined || !Number.isSafeInteger(startTime) ? undefined : { pid, startTime };
}

// ─── Worker-launched long-lived runner-uid roots ──────────────────────────────────────────

const workerRunnerRoots = new Map<number, RecordedRoot>();

/**
 * Record a long-lived runner-uid ROOT the worker itself launched (a Codex provider supervisor).
 * Such a root makes itself non-dumpable, so its env and cwd read back EACCES; the reaper then
 * attributes it (and its descendants, by ppid chain, group or session) through this registry
 * instead of reporting it `unverified`. The start time is captured HERE, at registration
 * (`readStartTime` replaces the procfs read for a test's fake pid); a root whose start time cannot
 * be read is not recorded. Returns the unregister function; call it once the root has exited.
 */
export function registerWorkerRunnerRoot(pid: number | undefined, readStartTime?: StartTimeReader): () => void {
  const root = recordRoot(pid, readStartTime);
  if (!root) return () => undefined;
  workerRunnerRoots.set(root.pid, root);
  return () => {
    if (workerRunnerRoots.get(root.pid) === root) workerRunnerRoots.delete(root.pid);
  };
}

/** The live worker-launched runner-uid roots (see {@link registerWorkerRunnerRoot}). */
export function workerRunnerRootPids(): RecordedRoot[] {
  return [...workerRunnerRoots.values()];
}
