// Pressure-only anonymous-volume prune leg for a docker-tier worker's DinD daemon (issue #3891).
//
// The cache prune (dind-prune.ts) only runs on a provably idle, custody-clear worker, so a
// worker that is always busy or custody-held never reclaims leaked anonymous volumes and its
// data root can fill. This leg is a SEPARATE controller with its own loop, streak, backoff
// and single-flight state. Under sustained data-root pressure it runs exactly one frozen
// command, `docker volume prune -f`, even while runs are active or custody is held.
//
// What it deliberately does NOT do: close or open the claim gate, check idleness or custody,
// run `docker ps`, drain claims, or touch images, build cache, containers, named volumes or
// networks. From API 1.42 the default `volume prune` removes only anonymous volumes; two
// guards keep it there: the server version is queried first (below 1.42 refuses), and the
// child's environment pins the request version with DOCKER_API_VERSION=1.42 (for THIS leg's
// children only, so an unpinned docker client cannot send a lower request version that would
// also prune unused named volumes). There is no downgrade retry: one prune exec per attempt.
//
// Maintenance coexistence: server-fenced maintenance (dind-maintenance.ts) closes the gate
// with owner "maintenance" and runs its own optional prune while holding it. This leg only
// READS the gate and defers (`maintenance-fenced`, streak kept, no backoff) while that owner
// holds it, checked synchronously immediately before each exec. If maintenance fences while a
// leg prune is already in flight, the Engine's single-prune guard may make one of the two
// fail with a conflict; maintenance then follows its existing failed-prune path unchanged.
//
// Outcomes (the prune exec only is classified; every outcome consumes the backoff, re-armed
// by a <75% sample or after 1h, except `unknown`, which always waits the full hour):
//   refused   nothing was submitted (spawn failure, version query failed/malformed/< 1.42).
//   unknown   submitted but the result is unknown (timeout kill, abort after spawn, stdout
//             overflow, a non-zero exit with no daemon error). No reclamation OR preservation
//             is claimed, and the full backoff applies even if a <75% sample arrives.
//   conflict  the daemon says a prune is already running.
//   rejected  any other daemon error response. Prune may have removed some volumes first.
//   completed exit 0.

import { readDindMeterSample, type DindMeterSample } from "./dind-meter.js";
import {
  BACKOFF_MS,
  DOCKER_BIN,
  MAX_CONSECUTIVE_GAP_S,
  TICK_MS,
  TRIGGER_STREAK,
  dockerExecEnvironment,
  execDocker,
  isBelowLow,
  isOver,
  ratios,
  type DindPruneGate,
  type DockerExec,
} from "./dind-prune.js";
import {
  DIND_VOLUME_PRUNE_ARGV,
  DIND_VOLUME_SERVER_VERSION_ARGV,
  apiVersionAtLeast142,
} from "./dind-volume-argv.js";
import type { Config } from "./config.js";
import type { Logger } from "./log.js";
import { errMessage, sleep as abortableSleep } from "./util.js";
import { asWriter } from "./hibernation-inventory.js";

/** The server-version query is cheap; a hung daemon must not hold the attempt long. */
const VERSION_TIMEOUT_MS = 30_000;
/** Per-command timeout for the prune (the client child is killed when it elapses). */
const PRUNE_TIMEOUT_MS = 5 * 60_000;
/** How long to wait for a sample newer than the prune to log the after reading. */
const AFTER_SAMPLE_WAIT_MS = 60_000;
const AFTER_SAMPLE_POLL_MS = 5_000;
/** A sample older than this is stale (matches the meter reader's window). */
const MAX_SAMPLE_AGE_MS = 90_000;
/** Bound on error text copied into a log line. */
const MAX_LOGGED_ERROR_CHARS = 300;
/** The request API version pinned in this leg's child environment (see header). */
const PINNED_API_VERSION = "1.42";

const NO_CLAIM_NOTE = "outcome not proven; no reclamation or preservation claimed";

const FROZEN_ARGV: readonly (readonly string[])[] = [DIND_VOLUME_SERVER_VERSION_ARGV, DIND_VOLUME_PRUNE_ARGV];

/** The leg's exact-match guard: throws unless `argv` is one of its two frozen argv. */
export function assertDindVolumeArgvAllowed(argv: readonly string[]): void {
  const allowed = FROZEN_ARGV.some((a) => a.length === argv.length && a.every((v, i) => v === argv[i]));
  if (!allowed) throw new Error("dind-volume-prune: refusing a docker argv outside the frozen pair");
}

// Module-load self-assertion: both frozen argv pass and nearby variants fail, so an edit that
// widens a command fails at import (every worker boot and every test).
for (const argv of FROZEN_ARGV) assertDindVolumeArgvAllowed(argv);
for (const bad of [["volume", "prune", "-a", "-f"], ["volume", "prune"], ["system", "prune", "-f"]]) {
  let refused = false;
  try {
    assertDindVolumeArgvAllowed(bad);
  } catch {
    refused = true;
  }
  if (!refused) throw new Error("dind-volume-prune: guard accepted a disallowed argv");
}

export type DindVolumePruneAttemptOutcome = "completed" | "refused" | "unknown" | "conflict" | "rejected";

/** What one evaluation did. Only {@link DindVolumePruneAttemptOutcome} values consume the backoff. */
export type DindVolumePruneOutcome =
  | "no-sample"
  | "backoff"
  | "below"
  | "pending"
  | "in-flight"
  | "maintenance-fenced"
  | "aborted"
  | DindVolumePruneAttemptOutcome;

export interface DindVolumePruneOptions {
  dockerHost: string;
  /** Used in the log tokens until the api-assigned worker id is known. */
  workerName: string;
  /** Only READ: the leg never closes or opens it. */
  gate: DindPruneGate;
  log: Logger;
  readSample?: () => DindMeterSample | null;
  exec?: DockerExec;
  /** Wall clock, epoch ms. */
  now?: () => number;
  sleep?: (ms: number, signal?: AbortSignal) => Promise<void>;
}

/**
 * Build the volume leg, or undefined when this worker must never prune: the same predicate
 * as createDindPrune (flag exactly "true" AND a wired docker daemon).
 */
export function createDindVolumePrune(
  config: Pick<Config, "dindPruneEnabled" | "dockerWiring" | "workerName">,
  deps: Omit<DindVolumePruneOptions, "dockerHost" | "workerName">,
): DindVolumePruneController | undefined {
  const dockerHost = config.dockerWiring?.dockerHost;
  if (config.dindPruneEnabled !== true || !dockerHost) return undefined;
  return new DindVolumePruneController({ ...deps, dockerHost, workerName: config.workerName });
}

interface ExecError {
  code?: unknown;
  killed?: unknown;
  signal?: unknown;
  syscall?: unknown;
  name?: unknown;
  stderr?: unknown;
}

function boundedText(text: string): string {
  // eslint-disable-next-line no-control-regex
  const flat = text.replace(/[\u0000-\u001f\u007f]+/g, " ").trim();
  return flat.length > MAX_LOGGED_ERROR_CHARS ? `${flat.slice(0, MAX_LOGGED_ERROR_CHARS)}…` : flat;
}

/** Classifies a rejected PRUNE exec, in order: spawn failure, post-submit ambiguity, daemon response. */
function classifyPruneFailure(err: unknown): "refused" | "unknown" | "conflict" | "rejected" {
  const e = (err && typeof err === "object" ? err : {}) as ExecError;
  if (typeof e.syscall === "string" && e.syscall.startsWith("spawn")) return "refused";
  if (
    e.killed === true ||
    (typeof e.signal === "string" && e.signal !== "") ||
    e.code === "ABORT_ERR" ||
    e.name === "AbortError" ||
    e.code === "ERR_CHILD_PROCESS_STDIO_MAXBUFFER"
  ) {
    return "unknown";
  }
  const text = `${errMessage(err)}\n${typeof e.stderr === "string" ? e.stderr : ""}`;
  if (text.includes("Error response from daemon:")) {
    return text.includes("a prune operation is already running") ? "conflict" : "rejected";
  }
  // A numeric non-zero exit with no daemon response (lost response, cannot connect) and any
  // unrecognised shape: the request may have been submitted, so nothing can be claimed.
  return "unknown";
}

/** The numeric "Total reclaimed space" value only (never raw output), or undefined. */
function parseReclaimed(stdout: string): string | undefined {
  const m = /^Total reclaimed space:\s*([0-9]+(?:\.[0-9]+)?)\s*([kKMGTP]?i?B)\s*$/m.exec(stdout);
  return m ? `${m[1]}${m[2]}` : undefined;
}

export class DindVolumePruneController {
  private readonly dockerHost: string;
  private readonly workerName: string;
  private readonly gate: DindPruneGate;
  private readonly log: Logger;
  private readonly readSampleRaw: () => DindMeterSample | null;
  private readonly exec: DockerExec;
  private readonly now: () => number;
  private readonly sleep: (ms: number, signal?: AbortSignal) => Promise<void>;

  private workerId?: string;
  /** Consecutive distinct over-threshold samples. */
  private streak = 0;
  /** The epoch of the last sample counted, so a re-read of the same file never counts twice. */
  private lastEpoch?: number;
  /** False after an attempt until re-armed by the low watermark or the backoff. */
  private armed = true;
  /** After `unknown` only the full backoff re-arms; a <75% sample does not. */
  private fullBackoff = false;
  private lastAttemptAtMs?: number;
  private inFlight = false;
  private fenceLogged = false;

  constructor(opts: DindVolumePruneOptions) {
    this.dockerHost = opts.dockerHost;
    this.workerName = opts.workerName;
    this.gate = opts.gate;
    this.log = opts.log;
    this.readSampleRaw = opts.readSample ?? (() => readDindMeterSample());
    this.exec = opts.exec ?? execDocker;
    this.now = opts.now ?? Date.now;
    this.sleep = opts.sleep ?? abortableSleep;
  }

  /** The api-assigned worker id, once registration returns it. */
  setWorkerId(id: string | undefined): void {
    if (id) this.workerId = id;
  }

  private label(): string {
    return this.workerId ?? this.workerName;
  }

  /** A readable, fresh, internally consistent sample, or null. */
  private readSample(): DindMeterSample | null {
    const s = this.readSampleRaw();
    if (!s) return null;
    const fields = [s.epochS, s.bytesUsed, s.bytesTotal, s.inodesUsed, s.inodesTotal];
    if (!fields.every((v) => Number.isFinite(v)) || s.epochS < 0 || s.bytesTotal <= 0 || s.inodesTotal <= 0) return null;
    if (s.bytesUsed < 0 || s.inodesUsed < 0) return null;
    if (this.now() - s.epochS * 1000 > MAX_SAMPLE_AGE_MS) return null;
    return s;
  }

  /** Evaluate every TICK_MS until abort. Never rejects. */
  async loop(signal: AbortSignal): Promise<void> {
    while (!signal.aborted) {
      try {
        await asWriter("dind_volume_prune", () => this.tick(signal));
      } catch (err) {
        try {
          this.log.warn("dind-volume-prune: evaluation failed", { error: boundedText(errMessage(err)) });
        } catch {
          // A broken logger must not end the loop or reject it.
        }
      }
      try {
        await this.sleep(TICK_MS, signal);
      } catch {
        // An aborted or failed sleep just ends this wait; the loop re-checks the signal.
      }
    }
  }

  /** One evaluation: read the sample, update the streak, and attempt when triggered. */
  async tick(signal?: AbortSignal): Promise<DindVolumePruneOutcome> {
    const sample = this.readSample();
    if (!sample) {
      this.streak = 0;
      this.lastEpoch = undefined;
      return "no-sample";
    }
    if (!this.armed) {
      const below = !this.fullBackoff && isBelowLow(sample);
      const elapsed = this.lastAttemptAtMs !== undefined && this.now() - this.lastAttemptAtMs >= BACKOFF_MS;
      if (!below && !elapsed) {
        this.streak = 0;
        this.lastEpoch = sample.epochS;
        return "backoff";
      }
      this.armed = true;
      this.fullBackoff = false;
      this.log.info("dind-volume-prune: re-armed", { reason: below ? "below-low-watermark" : "backoff-elapsed" });
    }
    const over = isOver(sample);
    if (sample.epochS !== this.lastEpoch) {
      const gap = this.lastEpoch === undefined ? 0 : sample.epochS - this.lastEpoch;
      const consecutive = this.lastEpoch !== undefined && gap > 0 && gap <= MAX_CONSECUTIVE_GAP_S;
      this.lastEpoch = sample.epochS;
      this.streak = over ? (consecutive ? this.streak + 1 : 1) : 0;
    }
    if (!over) {
      this.fenceLogged = false;
      return "below";
    }
    if (this.streak < TRIGGER_STREAK) return "pending";
    const outcome = await this.attempt(sample, signal);
    if (outcome === "in-flight" || outcome === "maintenance-fenced" || outcome === "aborted") return outcome;
    this.armed = false;
    this.fullBackoff = outcome === "unknown";
    this.lastAttemptAtMs = this.now();
    this.streak = 0;
    this.fenceLogged = false;
    return outcome;
  }

  /** True when this attempt must not exec now; logs a fence deferral once per episode. */
  private blocked(signal?: AbortSignal): "aborted" | "maintenance-fenced" | undefined {
    if (signal?.aborted) return "aborted";
    if (this.gate.heldBy("maintenance")) {
      if (!this.fenceLogged) {
        this.fenceLogged = true;
        this.log.info(`dind-volume-prune-deferred worker=${this.label()} reason=maintenance-fenced`, {});
      }
      return "maintenance-fenced";
    }
    return undefined;
  }

  private async attempt(
    before: DindMeterSample,
    signal?: AbortSignal,
  ): Promise<DindVolumePruneOutcome> {
    if (this.inFlight) return "in-flight";
    this.inFlight = true;
    try {
      return await this.run(before, signal);
    } finally {
      this.inFlight = false;
    }
  }

  private async run(before: DindMeterSample, signal?: AbortSignal): Promise<DindVolumePruneOutcome> {
    const worker = this.label();
    const stop1 = this.blocked(signal);
    if (stop1) return stop1;
    let version: string;
    try {
      version = (await this.docker(DIND_VOLUME_SERVER_VERSION_ARGV, VERSION_TIMEOUT_MS, signal)).trim();
    } catch (err) {
      if (signal?.aborted) return "aborted";
      this.log.warn(`dind-volume-prune-refused worker=${worker}`, {
        reason: "server-version-query-failed",
        error: boundedText(errMessage(err)),
      });
      return "refused";
    }
    if (!apiVersionAtLeast142(version)) {
      this.log.warn(`dind-volume-prune-refused worker=${worker}`, {
        reason: "server-api-below-1.42-or-unreadable",
        server_api_version: boundedText(version),
      });
      return "refused";
    }
    // Re-check immediately before the prune exec: maintenance may have fenced meanwhile.
    const stop2 = this.blocked(signal);
    if (stop2) return stop2;
    let stdout: string;
    try {
      stdout = await this.docker(DIND_VOLUME_PRUNE_ARGV, PRUNE_TIMEOUT_MS, signal);
    } catch (err) {
      const kind = classifyPruneFailure(err);
      const e = (err && typeof err === "object" ? err : {}) as ExecError;
      const error = boundedText(`${errMessage(err)} ${typeof e.stderr === "string" ? e.stderr : ""}`);
      if (kind === "refused") {
        this.log.warn(`dind-volume-prune-refused worker=${worker}`, { reason: "spawn-failed", error });
      } else if (kind === "conflict") {
        this.log.warn(`dind-volume-prune-conflict worker=${worker}`, { reason: "prune-already-running", error });
      } else {
        this.log.warn(`dind-volume-prune-${kind} worker=${worker}`, { note: NO_CLAIM_NOTE, error, ...ratios(before) });
      }
      return kind;
    }
    const finishedAtMs = this.now();
    const reclaimed = parseReclaimed(stdout);
    this.log.info(`dind-volume-prune-completed worker=${worker}`, {
      before_bytes_pct: ratios(before).bytes_pct,
      before_inodes_pct: ratios(before).inodes_pct,
      ...(reclaimed !== undefined ? { reclaimed } : {}),
    });
    const after = await this.waitFreshSample(finishedAtMs, signal);
    this.log.info(`dind-volume-prune-after worker=${worker}`, after ? ratios(after) : { sample: "unavailable" });
    return "completed";
  }

  /** The leg's exec wrapper: its own exact-match guard, then docker by absolute path with
   *  the minimal worker env plus the pinned request API version. Never spreads process.env. */
  private docker(argv: readonly string[], timeoutMs: number, signal?: AbortSignal): Promise<string> {
    assertDindVolumeArgvAllowed(argv);
    const env = { ...dockerExecEnvironment(this.dockerHost), DOCKER_API_VERSION: PINNED_API_VERSION };
    return this.exec(argv, { file: DOCKER_BIN, env, timeoutMs, ...(signal ? { signal } : {}) });
  }

  /** A valid sample taken strictly after `afterMs`, waiting up to AFTER_SAMPLE_WAIT_MS; null if none. */
  private async waitFreshSample(afterMs: number, signal?: AbortSignal): Promise<DindMeterSample | null> {
    const until = this.now() + AFTER_SAMPLE_WAIT_MS;
    for (;;) {
      const s = this.readSample();
      if (s && s.epochS * 1000 > afterMs) return s;
      const left = until - this.now();
      if (left <= 0 || signal?.aborted) return null;
      await this.sleep(Math.min(AFTER_SAMPLE_POLL_MS, left), signal);
    }
  }
}
