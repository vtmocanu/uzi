// Gated, allowlisted DinD prune under data-root pressure (issue #1759 M3).
//
// A docker-tier worker's DinD daemon keeps images, layers and build cache on the
// `dind-data` volume, which fills up across runs. The dind-meter sidecar publishes a
// statfs sample (dind-meter.ts); this module watches it and, when the volume is under
// sustained pressure AND the worker is provably idle AND the api says the worker holds
// no unpublished work, runs a short, frozen list of docker prune commands.
//
// Safety posture, in order of the checks an attempt passes:
//   1. Enablement: UZI_DIND_PRUNE_ENABLED === "true" AND a wired dockerHost
//      (createDindPrune returns undefined otherwise, so no loop ever runs).
//   2. Pressure: two CONSECUTIVE, distinct, fresh samples at or above 85% of bytes OR
//      inodes. A missing/invalid/stale sample resets the streak.
//   3. Hysteresis: after any attempt, nothing re-triggers until a fresh sample is below
//      75% on BOTH bytes and inodes, or an hour has passed.
//   4. Custody (fail-closed): the last heartbeat said retaining_unpublished_work ===
//      false, received within two heartbeat intervals. Anything else skips.
//   5. Exclusive idle: close the claim gate, wait (bounded) for in-flight claims to
//      settle, require no active run, chat or registry entry, re-check custody, and
//      require `docker ps -q` to list nothing. Any failure releases the gate at once and
//      retries next tick WITHOUT consuming the backoff.
//   6. Commands: a frozen argv table, a runtime guard in the exec wrapper that refuses
//      any volume/system/container/rm token, per-command and overall time budgets.
//
// Stopped containers are NEVER removed (no `container prune`, no `rm`): the run harness
// creates no labelled containers this worker could prove it owns, so any stopped
// container may be a user's deliberate state. `image prune` only removes images no
// container (running or stopped) references, and volumes are never touched.
//
// The commands run as the WORKER uid, not through the runner-uid wrapper: they carry no
// model or repo input (the argv is a frozen constant, validated again at exec time),
// they talk to the daemon only over DOCKER_HOST (the daemon performs the deletion in its
// own data root), and the child gets a minimal env (PATH, HOME, DOCKER_HOST) so no
// worker secret reaches it. The runner-uid split exists to fence model-driven commands
// away from worker secrets, and none are involved here.

import { execFile } from "node:child_process";
import type { Config } from "./config.js";
import type { Logger } from "./log.js";
import { readDindMeterSample, type DindMeterSample } from "./dind-meter.js";
import { errMessage, sleep as abortableSleep } from "./util.js";

/** Pressure threshold: a sample at or above this fraction of bytes OR inodes is "over". */
const HIGH_WATERMARK = 0.85;
/** Re-arm threshold: after an attempt, a sample below this on BOTH bytes and inodes re-arms. */
const LOW_WATERMARK = 0.75;
/** Consecutive distinct over-threshold samples needed to trigger (debounce). */
const TRIGGER_STREAK = 2;
/** After an attempt (any outcome), re-trigger no sooner than this unless re-armed by LOW_WATERMARK. */
const BACKOFF_MS = 60 * 60_000;
/** Evaluation cadence: matches the meter's 30s sampling interval. */
const TICK_MS = 30_000;
/** Bound on waiting for claims that were in flight when the gate closed. */
const CLAIM_DRAIN_TIMEOUT_MS = 60_000;
const CLAIM_DRAIN_POLL_MS = 250;
/** The custody flag is trusted for at most this many heartbeat intervals after receipt. */
const CUSTODY_MAX_HEARTBEATS = 2;
/** Per-command timeout (the child is killed when it elapses). */
const COMMAND_TIMEOUT_MS = 5 * 60_000;
/** Whole-prune budget: remaining steps are abandoned once it is spent. */
const PRUNE_BUDGET_MS = 15 * 60_000;
/** How long to wait for a post-prune sample newer than the prune step (two meter intervals). */
const POST_SAMPLE_WAIT_MS = 2 * 30_000;
const POST_SAMPLE_POLL_MS = 5_000;
/** Largest stdout accepted from one docker command (a big prune lists every deleted layer). */
const MAX_STDOUT_BYTES = 16 * 1024 * 1024;

/**
 * THE ONLY docker argv this module ever runs, frozen. Deliberately absent: `volume prune`,
 * `system prune`, `--volumes`, `container prune`, `rm`/`rmi` of anything by name.
 */
export const DIND_PRUNE_ARGV = Object.freeze({
  /** Running containers; any output aborts the attempt. */
  listRunning: Object.freeze(["ps", "-q"]),
  /** Dangling (untagged, unreferenced) images. */
  pruneDangling: Object.freeze(["image", "prune", "-f"]),
  /** The BuildKit build cache. */
  pruneBuildCache: Object.freeze(["builder", "prune", "-f"]),
  /** Every image no container references, older than a day. Only when still over after the two above. */
  pruneUnusedImages: Object.freeze(["image", "prune", "-a", "-f", "--filter", "until=24h"]),
});

const ALLOWED_ARGV: readonly (readonly string[])[] = Object.values(DIND_PRUNE_ARGV);

/** Tokens no docker argv from this module may carry, checked at exec time. */
const FORBIDDEN_TOKENS = ["volume", "volumes", "--volumes", "system", "container", "rm"];

/**
 * The exec-time guard: throws unless `argv` carries no forbidden token (exact, case-
 * insensitive, or as a `token=value` flag) AND is exactly one of {@link DIND_PRUNE_ARGV}.
 */
export function assertDockerArgvAllowed(argv: readonly string[]): void {
  for (const arg of argv) {
    const t = arg.toLowerCase();
    if (FORBIDDEN_TOKENS.some((f) => t === f || t.startsWith(`${f}=`))) {
      throw new Error(`dind-prune: refusing forbidden docker argv token ${JSON.stringify(arg)}`);
    }
  }
  const allowed = ALLOWED_ARGV.some((a) => a.length === argv.length && a.every((v, i) => v === argv[i]));
  if (!allowed) throw new Error("dind-prune: refusing a docker argv outside the frozen allowlist");
}

// Module-load assertion: the table itself must pass the guard, so an edit that adds a
// forbidden command fails at import (every worker boot and every test), not at 3am.
for (const argv of ALLOWED_ARGV) assertDockerArgvAllowed(argv);

/** Runs `docker <argv>` and resolves its stdout; rejects on non-zero exit or timeout. */
export type DockerExec = (
  argv: readonly string[],
  opts: { env: NodeJS.ProcessEnv; timeoutMs: number; signal?: AbortSignal },
) => Promise<string>;

const execDocker: DockerExec = (argv, opts) =>
  new Promise((resolve, reject) => {
    execFile(
      "docker",
      [...argv],
      {
        env: opts.env,
        timeout: opts.timeoutMs,
        killSignal: "SIGKILL",
        maxBuffer: MAX_STDOUT_BYTES,
        ...(opts.signal ? { signal: opts.signal } : {}),
      },
      (err, stdout) => (err ? reject(err) : resolve(String(stdout))),
    );
  });

/**
 * The claim gate the two claim loops and the prune share. A claim loop calls
 * {@link tryEnterClaim} before its HTTP claim and {@link exitClaim} once the claimed
 * work (if any) is in its active set; the prune closes the gate, waits for in-flight
 * claims to reach zero, and only then inspects the active sets, so a claim can never
 * slip a run in between the idle check and the prune.
 */
export class DindPruneGate {
  private closed = false;
  private inFlight = 0;

  /** False while the prune holds the gate; otherwise counts one in-flight claim. */
  tryEnterClaim(): boolean {
    if (this.closed) return false;
    this.inFlight++;
    return true;
  }

  /** Ends an in-flight claim started by a successful {@link tryEnterClaim}. */
  exitClaim(): void {
    if (this.inFlight > 0) this.inFlight--;
  }

  /** Whether the prune currently holds the gate (claim loops sleep a poll and retry). */
  claimsClosed(): boolean {
    return this.closed;
  }

  close(): void {
    this.closed = true;
  }

  open(): void {
    this.closed = false;
  }

  inFlightClaims(): number {
    return this.inFlight;
  }
}

/** What one evaluation did. `busy` (an idle/custody/container check failed at attempt
 *  time) retries next tick without consuming the backoff; completed/failed/insufficient
 *  start it. */
type DindPruneOutcome =
  | "no-sample"
  | "backoff"
  | "below"
  | "pending"
  | "custody"
  | "busy"
  | "completed"
  | "failed"
  | "insufficient"
  | "aborted";

export interface DindPruneOptions {
  dockerHost: string;
  heartbeatIntervalMs: number;
  /** Used in the outcome log tokens until the api-assigned worker id is known. */
  workerName: string;
  gate: DindPruneGate;
  /** True only when no run, chat or registry entry is active. */
  isIdle: () => boolean;
  log: Logger;
  readSample?: () => DindMeterSample | null;
  exec?: DockerExec;
  /** Wall clock, epoch ms. */
  now?: () => number;
  sleep?: (ms: number, signal?: AbortSignal) => Promise<void>;
}

/**
 * Build the prune controller, or undefined when this worker must never prune: the flag
 * is not exactly "true" (config.dindPruneEnabled) or no docker daemon is wired.
 */
export function createDindPrune(
  config: Pick<Config, "dindPruneEnabled" | "dockerWiring" | "heartbeatIntervalMs" | "workerName">,
  deps: Omit<DindPruneOptions, "dockerHost" | "heartbeatIntervalMs" | "workerName">,
): DindPruneController | undefined {
  const dockerHost = config.dockerWiring?.dockerHost;
  if (config.dindPruneEnabled !== true || !dockerHost) return undefined;
  return new DindPruneController({
    ...deps,
    dockerHost,
    heartbeatIntervalMs: config.heartbeatIntervalMs,
    workerName: config.workerName,
  });
}

function isOver(s: DindMeterSample): boolean {
  return s.bytesUsed / s.bytesTotal >= HIGH_WATERMARK || s.inodesUsed / s.inodesTotal >= HIGH_WATERMARK;
}

function isBelowLow(s: DindMeterSample): boolean {
  return s.bytesUsed / s.bytesTotal < LOW_WATERMARK && s.inodesUsed / s.inodesTotal < LOW_WATERMARK;
}

function ratios(s: DindMeterSample): Record<string, number> {
  return {
    bytes_pct: Math.round((s.bytesUsed / s.bytesTotal) * 1000) / 10,
    inodes_pct: Math.round((s.inodesUsed / s.inodesTotal) * 1000) / 10,
  };
}

export class DindPruneController {
  readonly gate: DindPruneGate;
  private readonly dockerHost: string;
  private readonly custodyMaxAgeMs: number;
  private readonly workerName: string;
  private readonly isIdle: () => boolean;
  private readonly log: Logger;
  private readonly readSample: () => DindMeterSample | null;
  private readonly exec: DockerExec;
  private readonly now: () => number;
  private readonly sleep: (ms: number, signal?: AbortSignal) => Promise<void>;

  private workerId?: string;
  private custody?: { retaining: boolean | undefined; atMs: number };
  /** Consecutive distinct over-threshold samples. */
  private streak = 0;
  /** The epoch of the last sample counted, so a re-read of the same file never counts twice. */
  private lastEpoch?: number;
  /** False after an attempt until re-armed by LOW_WATERMARK or BACKOFF_MS. */
  private armed = true;
  private lastAttemptAtMs?: number;
  /** Once-per-episode log latches. */
  private custodyLogged = false;
  private busyLogged = false;

  constructor(opts: DindPruneOptions) {
    this.gate = opts.gate;
    this.dockerHost = opts.dockerHost;
    this.custodyMaxAgeMs = CUSTODY_MAX_HEARTBEATS * opts.heartbeatIntervalMs;
    this.workerName = opts.workerName;
    this.isIdle = opts.isIdle;
    this.log = opts.log;
    this.readSample = opts.readSample ?? (() => readDindMeterSample());
    this.exec = opts.exec ?? execDocker;
    this.now = opts.now ?? Date.now;
    this.sleep = opts.sleep ?? abortableSleep;
  }

  /** The api-assigned worker id, once registration returns it. */
  setWorkerId(id: string | undefined): void {
    if (id) this.workerId = id;
  }

  /** Record the custody flag from a SUCCESSFUL heartbeat (undefined when the response
   *  did not carry a boolean). A failed heartbeat records nothing, so the last flag ages
   *  out after two intervals and the prune fails closed. */
  recordCustody(retaining: boolean | undefined): void {
    this.custody = { retaining, atMs: this.now() };
  }

  /** Evaluate every TICK_MS until abort. Never throws. */
  async loop(signal: AbortSignal): Promise<void> {
    while (!signal.aborted) {
      try {
        await this.tick(signal);
      } catch (err) {
        this.log.warn("dind-prune: evaluation failed", { error: errMessage(err) });
      }
      try {
        await this.sleep(TICK_MS, signal);
      } catch {
        // An aborted or failed sleep just ends this wait; the loop re-checks the signal.
      }
    }
  }

  /** One evaluation: read the sample, update the streak, and attempt when triggered. */
  async tick(signal?: AbortSignal): Promise<DindPruneOutcome> {
    const sample = this.readSample();
    if (!sample) {
      this.streak = 0;
      this.lastEpoch = undefined;
      return "no-sample";
    }
    if (!this.armed) {
      const below = isBelowLow(sample);
      const elapsed = this.lastAttemptAtMs !== undefined && this.now() - this.lastAttemptAtMs >= BACKOFF_MS;
      if (!below && !elapsed) {
        this.streak = 0;
        this.lastEpoch = sample.epochS;
        return "backoff";
      }
      this.armed = true;
      this.log.info("dind-prune: re-armed", { reason: below ? "below-low-watermark" : "backoff-elapsed" });
    }
    const over = isOver(sample);
    if (sample.epochS !== this.lastEpoch) {
      this.lastEpoch = sample.epochS;
      this.streak = over ? this.streak + 1 : 0;
    }
    if (!over) {
      this.custodyLogged = false;
      this.busyLogged = false;
      return "below";
    }
    if (this.streak < TRIGGER_STREAK) return "pending";
    if (!this.custodyAllowsPrune()) {
      if (!this.custodyLogged) {
        this.custodyLogged = true;
        this.log.info("dind-prune: under pressure but custody is not provably clear; skipping", {
          ...ratios(sample),
          retaining_unpublished_work: this.custody?.retaining ?? null,
        });
      }
      return "custody";
    }
    const outcome = await this.attempt(signal);
    if (outcome === "completed" || outcome === "failed" || outcome === "insufficient") {
      this.armed = false;
      this.lastAttemptAtMs = this.now();
      this.streak = 0;
      this.custodyLogged = false;
      this.busyLogged = false;
    }
    return outcome;
  }

  private custodyAllowsPrune(): boolean {
    const c = this.custody;
    return c !== undefined && c.retaining === false && this.now() - c.atMs <= this.custodyMaxAgeMs;
  }

  private label(): string {
    return this.workerId ?? this.workerName;
  }

  private deferred(reason: string): "busy" {
    if (!this.busyLogged) {
      this.busyLogged = true;
      this.log.debug("dind-prune: deferred, worker not provably idle", { reason });
    }
    return "busy";
  }

  /** Exclusive acquisition, final checks, then the prune. The gate is held throughout
   *  and released in `finally` on every path. */
  private async attempt(signal?: AbortSignal): Promise<DindPruneOutcome> {
    // Cheap pre-check so a busy worker's claim loops are not paused every tick.
    if (!this.isIdle()) return this.deferred("active-work");
    this.gate.close();
    try {
      if (!(await this.waitClaimsDrained(signal))) return this.deferred("claim-in-flight");
      if (!this.isIdle()) return this.deferred("active-work");
      // Re-check: the flag may be up to one interval old and may have flipped.
      if (!this.custodyAllowsPrune()) return this.deferred("custody");
      let running: string;
      try {
        running = await this.docker(DIND_PRUNE_ARGV.listRunning, COMMAND_TIMEOUT_MS, signal);
      } catch (err) {
        this.log.debug("dind-prune: docker ps failed", { error: errMessage(err) });
        return this.deferred("docker-ps-failed");
      }
      if (running.trim() !== "") return this.deferred("running-container");
      return await this.prune(signal);
    } finally {
      this.gate.open();
    }
  }

  private async prune(signal?: AbortSignal): Promise<DindPruneOutcome> {
    const deadline = this.now() + PRUNE_BUDGET_MS;
    const worker = this.label();
    try {
      await this.step(DIND_PRUNE_ARGV.pruneDangling, deadline, signal);
      await this.step(DIND_PRUNE_ARGV.pruneBuildCache, deadline, signal);
      let post = await this.waitFreshSample(this.now(), deadline, signal);
      if (post && isOver(post)) {
        await this.step(DIND_PRUNE_ARGV.pruneUnusedImages, deadline, signal);
        post = await this.waitFreshSample(this.now(), deadline, signal);
        if (post && isOver(post)) {
          this.log.warn(`dind-prune-insufficient worker=${worker}`, ratios(post));
          return "insufficient";
        }
      }
      this.log.info(`dind-prune-completed worker=${worker}`, post ? ratios(post) : {});
      return "completed";
    } catch (err) {
      if (signal?.aborted) {
        this.log.info("dind-prune: interrupted by shutdown", {});
        return "aborted";
      }
      this.log.warn(`dind-prune-failed worker=${worker}`, { error: errMessage(err) });
      return "failed";
    }
  }

  private async step(argv: readonly string[], deadline: number, signal?: AbortSignal): Promise<void> {
    const remaining = deadline - this.now();
    if (remaining <= 0) throw new Error("prune budget exhausted");
    await this.docker(argv, Math.min(COMMAND_TIMEOUT_MS, remaining), signal);
  }

  /** The exec wrapper: the runtime argv guard, then docker with a minimal env. */
  private docker(argv: readonly string[], timeoutMs: number, signal?: AbortSignal): Promise<string> {
    assertDockerArgvAllowed(argv);
    const env: NodeJS.ProcessEnv = {
      PATH: process.env.PATH ?? "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
      HOME: process.env.HOME ?? "/tmp",
      DOCKER_HOST: this.dockerHost,
    };
    return this.exec(argv, { env, timeoutMs, ...(signal ? { signal } : {}) });
  }

  private async waitClaimsDrained(signal?: AbortSignal): Promise<boolean> {
    const until = this.now() + CLAIM_DRAIN_TIMEOUT_MS;
    while (this.gate.inFlightClaims() > 0) {
      if (signal?.aborted || this.now() >= until) return false;
      await this.sleep(CLAIM_DRAIN_POLL_MS, signal);
    }
    return true;
  }

  /** A valid sample taken strictly after `afterMs`, waiting up to POST_SAMPLE_WAIT_MS
   *  (never past the prune deadline); null when none arrives. */
  private async waitFreshSample(afterMs: number, deadline: number, signal?: AbortSignal): Promise<DindMeterSample | null> {
    const until = Math.min(this.now() + POST_SAMPLE_WAIT_MS, deadline);
    for (;;) {
      const s = this.readSample();
      if (s && s.epochS * 1000 > afterMs) return s;
      const left = until - this.now();
      if (left <= 0 || signal?.aborted) return null;
      await this.sleep(Math.min(POST_SAMPLE_POLL_MS, left), signal);
    }
  }
}
