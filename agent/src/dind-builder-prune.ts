// Pressure-only BuildKit regular-cache prune leg for a docker-tier worker's DinD daemon
// (issue #4291).
//
// The cache prune (dind-prune.ts) only runs on a provably idle, custody-clear worker, and the
// volume leg (dind-volume-prune.ts) reclaims anonymous volumes only. A worker that is always
// busy or custody-held therefore never trims its BuildKit cache, which can fill the data root.
// This leg is a THIRD, separate controller with its own loop, streak, backoff and single-flight
// state. Under sustained data-root pressure it runs exactly one frozen command even while runs
// are active or custody is held:
//
//   docker builder prune -f --filter type=regular --filter until=24h --max-used-space 4GiB
//
// What it deliberately does NOT do: close or open the claim gate, drain claims, check idleness
// or custody, or touch images, volumes or containers. `type=regular` leaves BuildKit cache
// mounts (type=exec.cachemount) alone, and the absence of `--all` leaves internal, frontend and
// shared records alone.
//
// Owner boundary: ordinary regular build-cache misses on a busy worker are accepted ("no worse
// than a fresh worker"). `--max-used-space 4GiB` is a total-cache performance target, not a
// bound on what the daemon may keep or on how much of the volume this reclaims. BuildKit
// serializes concurrent prunes (muPrune), so two prunes never run their deletions at once.
//
// Maintenance coexistence: server-fenced maintenance (dind-maintenance.ts) closes the gate with
// owner "maintenance" and may stop the daemon. This leg only READS the gate and defers
// (`maintenance-fenced`, streak kept, no backoff) while that owner holds it, with a synchronous
// check immediately before the spawn. If maintenance stops the daemon mid-prune, the client
// fails and the result is `unknown`.
//
// Settlement. The child is spawned detached in its own process group and settlement is the
// child's `close` event only (exit AND every stdio pipe holder closed). A kill (prune timeout,
// abort, stdout overflow) is SIGKILL to the whole group. That is LOCAL child settlement and is
// distinct from the daemon's in-flight-batch uncertainty: killing the client cancels the
// daemon-side prune only after its in-flight batch, and records already deleted stay deleted.
// So every kill is `unknown`, which claims neither reclamation nor preservation. If `close`
// is not seen within `settleTimeoutMs` after a kill (a descendant escaped the group and holds
// a pipe), the attempt is `unknown` with reason settlement-unconfirmed and the leg LATCHES:
// no further spawn happens, and no backoff is consumed or reset, until that `close` arrives.
// The latch is in memory only, so a worker restart clears it, and it never touches the gate or
// any run. The pipes are unref'd (never destroyed) and drained into a discard sink so a late
// `close` is still observed and a worker that finishes its work can still exit.
//
// Outcomes (every attempt outcome consumes the backoff, re-armed by a <75% sample or after 1h,
// except `unknown`, which always waits the full hour):
//   refused   nothing was submitted (buildx plugin missing, spawn failure, buildx CLI error).
//   unknown   submitted but the result is unknown (timeout/abort/overflow kill, unconfirmed
//             settlement, a non-zero exit with no daemon error, a gRPC transport loss such as
//             `rpc error: code = Unavailable|Canceled|DeadlineExceeded`, which is not a daemon
//             response).
//   conflict  the daemon says a prune is already running (defensive).
//   rejected  any other daemon error response (`Error response from daemon:` or a non-transport
//             `rpc error: code =`). Prune may have removed some records first.
//   completed exit 0.

import { spawn, type ChildProcess } from "node:child_process";
import { lstatSync } from "node:fs";
import type { Readable } from "node:stream";
import { readDindMeterSample, type DindMeterSample } from "./dind-meter.js";
import {
  BACKOFF_MS,
  DOCKER_BIN,
  MAX_CONSECUTIVE_GAP_S,
  TICK_MS,
  TRIGGER_STREAK,
  dockerExecEnvironment,
  isBelowLow,
  isOver,
  ratios,
  type DindPruneGate,
} from "./dind-prune.js";
import type { Config } from "./config.js";
import type { Logger } from "./log.js";
import { errMessage, sleep as abortableSleep } from "./util.js";

/** Per-command timeout for the prune (the client process group is killed when it elapses). */
const PRUNE_TIMEOUT_MS = 5 * 60_000;
/** After a kill, how long to wait for the child's `close` before latching the leg. */
const SETTLE_TIMEOUT_MS = 10_000;
/** How long to wait for a sample newer than the prune to log the after reading. */
const AFTER_SAMPLE_WAIT_MS = 60_000;
const AFTER_SAMPLE_POLL_MS = 5_000;
/** A sample older than this is stale (matches the meter reader's window). */
const MAX_SAMPLE_AGE_MS = 90_000;
/** Bound on error text copied into a log line. */
const MAX_LOGGED_ERROR_CHARS = 300;
/** Largest stdout accepted from the prune client (it lists every deleted record). */
const MAX_STDOUT_BYTES = 16 * 1024 * 1024;
/** Stderr is only classified and logged in bounded form, so a modest cap suffices. */
const MAX_STDERR_BYTES = 1024 * 1024;
/** The buildx CLI plugin the docker client needs for `builder prune` with BuildKit forced on. */
const DEFAULT_BUILDX_PLUGIN_PATH = "/usr/libexec/docker/cli-plugins/docker-buildx";
/**
 * The docker/cli message when DOCKER_BUILDKIT=1 is set and buildx is absent or unusable
 * ("BuildKit is enabled but the buildx component is missing or broken"). The match is
 * deliberately this one narrow substring; the exact text is to be confirmed against the real
 * image.
 */
const BUILDX_MISSING_TEXT = "buildx component is missing or broken";

/** gRPC codes that mean the transport was lost, not that the daemon answered. */
const TRANSPORT_RPC_CODES: ReadonlySet<string> = new Set(["Unavailable", "Canceled", "DeadlineExceeded"]);

const NO_CLAIM_NOTE = "outcome not proven; no reclamation or preservation claimed";

/** THE ONLY docker argv this leg ever runs, frozen. */
export const DIND_BUILDER_PRUNE_ARGV: readonly string[] = Object.freeze([
  "builder",
  "prune",
  "-f",
  "--filter",
  "type=regular",
  "--filter",
  "until=24h",
  "--max-used-space",
  "4GiB",
]);

/**
 * An INDEPENDENT literal of the one allowed command (NUL-joined), deliberately not derived from
 * {@link DIND_BUILDER_PRUNE_ARGV}, so widening that constant without also editing this literal
 * fails at import.
 */
const EXPECTED_ARGV_JOINED =
  "builder\u0000prune\u0000-f\u0000--filter\u0000type=regular\u0000--filter\u0000until=24h\u0000--max-used-space\u00004GiB";
const EXPECTED_ARGV_LENGTH = 9;

/** The leg's exact-match guard: throws unless `argv` equals the independent literal above. */
export function assertDindBuilderArgvAllowed(argv: readonly string[]): void {
  if (argv.length !== EXPECTED_ARGV_LENGTH || argv.join("\u0000") !== EXPECTED_ARGV_JOINED) {
    throw new Error("dind-builder-prune: refusing a docker argv outside the frozen command");
  }
}

// Module-load self-assertion: the exported constant must pass the guard (which compares against
// the independent literal) and nearby variants must fail, so a widened constant throws at import
// (every worker boot and every test).
assertDindBuilderArgvAllowed(DIND_BUILDER_PRUNE_ARGV);
{
  const f = DIND_BUILDER_PRUNE_ARGV;
  const bads: string[][] = [
    ["builder", "prune", "-f"],
    [...f, "-a"],
    [...f, "--all"],
    f.filter((v, i) => !(v === "type=regular" || (v === "--filter" && f[i + 1] === "type=regular"))),
    f.map((v) => (v === "until=24h" ? "until=12h" : v)),
    f.map((v) => (v === "4GiB" ? "8GiB" : v)),
    ["volume", "prune", "-f"],
    ["system", "prune", "-f"],
  ];
  for (const bad of bads) {
    let refused = false;
    try {
      assertDindBuilderArgvAllowed(bad);
    } catch {
      refused = true;
    }
    if (!refused) throw new Error("dind-builder-prune: guard accepted a disallowed argv");
  }
}

export type DindBuilderPruneAttemptOutcome = "completed" | "refused" | "unknown" | "conflict" | "rejected";

/** What one evaluation did. Only {@link DindBuilderPruneAttemptOutcome} values consume the backoff. */
export type DindBuilderPruneOutcome =
  | "no-sample"
  | "backoff"
  | "below"
  | "pending"
  | "in-flight"
  | "maintenance-fenced"
  | "aborted"
  | "unsettled"
  | DindBuilderPruneAttemptOutcome;

/** A killed child whose `close` was not observed within the settle bound. */
export interface UnsettledChild {
  pid: number;
  pgid: number;
  /** Resolves when the child's `close` is finally observed. Never rejects. */
  settled: Promise<void>;
}

export type BuilderPruneExecResult =
  | { kind: "spawn-error"; error: string }
  | { kind: "exit"; code: number | null; signal: string | null; stdout: string; stderr: string }
  | { kind: "killed"; reason: "timeout" | "abort" | "overflow"; unsettled?: UnsettledChild };

export interface BuilderPruneExecOptions {
  file: string;
  /** Test seam: arguments placed before the guarded argv (empty in production). */
  argvPrefix: readonly string[];
  env: NodeJS.ProcessEnv;
  timeoutMs: number;
  settleTimeoutMs: number;
  /** Stdout cap in bytes before the group is killed as overflow (default 16 MiB). */
  maxStdoutBytes?: number;
  signal?: AbortSignal;
}

/** Spawns `<file> <argvPrefix> <argv>` and reports how the child ended. Never rejects. */
export type BuilderPruneExec = (argv: readonly string[], opts: BuilderPruneExecOptions) => Promise<BuilderPruneExecResult>;

function killGroup(pid: number): void {
  try {
    process.kill(-pid, "SIGKILL");
  } catch {
    // ESRCH: the group is already gone. Any other failure cannot be acted on here; the
    // settle bound below reports a child that never closes.
  }
}

function unrefStream(s: Readable | null): void {
  (s as (Readable & { unref?: () => void }) | null)?.unref?.();
}

/**
 * The leg's own exec wrapper (not execDocker): the child is detached into its own process
 * group so a kill reaches every in-group descendant, and settlement is its `close` event only.
 */
const execBuilderPrune: BuilderPruneExec = (argv, opts) =>
  new Promise<BuilderPruneExecResult>((resolve) => {
    let child: ChildProcess;
    try {
      child = spawn(opts.file, [...opts.argvPrefix, ...argv], {
        env: opts.env,
        detached: true,
        stdio: ["ignore", "pipe", "pipe"],
      });
    } catch (err) {
      resolve({ kind: "spawn-error", error: errMessage(err) });
      return;
    }
    let stdout = "";
    let stderr = "";
    let stdoutBytes = 0;
    let killReason: "timeout" | "abort" | "overflow" | undefined;
    let resolved = false;
    let discard = false;
    let unsettledResolve: (() => void) | undefined;
    let pruneTimer: NodeJS.Timeout | undefined;
    let settleTimer: NodeJS.Timeout | undefined;

    const finish = (result: BuilderPruneExecResult): void => {
      if (pruneTimer) clearTimeout(pruneTimer);
      if (settleTimer) clearTimeout(settleTimer);
      opts.signal?.removeEventListener("abort", onAbort);
      if (!resolved) {
        resolved = true;
        resolve(result);
      }
    };

    const startKill = (reason: "timeout" | "abort" | "overflow"): void => {
      // Signal the group at most once: a later call (more stdout after overflow, an abort after a
      // timeout) must not re-send SIGKILL to a pgid that may have been recycled.
      if (killReason) return;
      killReason = reason;
      if (pruneTimer) clearTimeout(pruneTimer);
      if (child.pid !== undefined) killGroup(child.pid);
      if (settleTimer) return;
      settleTimer = setTimeout(() => {
        const pid = child.pid;
        if (resolved || pid === undefined) return;
        // `close` never came: latch via the result, stop draining into memory, and let the
        // worker exit without waiting on this child. Pipes are unref'd, never destroyed.
        discard = true;
        stdout = "";
        stderr = "";
        const settled = new Promise<void>((r) => (unsettledResolve = r));
        child.unref();
        unrefStream(child.stdout);
        unrefStream(child.stderr);
        finish({ kind: "killed", reason: killReason ?? reason, unsettled: { pid, pgid: pid, settled } });
      }, opts.settleTimeoutMs);
    };

    function onAbort(): void {
      startKill("abort");
    }

    child.stdout?.on("data", (chunk: Buffer) => {
      if (discard) return;
      stdoutBytes += chunk.length;
      if (stdoutBytes > (opts.maxStdoutBytes ?? MAX_STDOUT_BYTES)) {
        stdout = "";
        startKill("overflow");
        return;
      }
      if (!killReason) stdout += chunk.toString("utf8");
    });
    child.stderr?.on("data", (chunk: Buffer) => {
      if (discard) return;
      if (stderr.length < MAX_STDERR_BYTES) stderr += chunk.toString("utf8");
    });
    child.once("error", (err) => {
      // Only a failure to start (no pid) is a spawn error; a later error (a failed kill) is
      // left to the settle bound.
      if (child.pid === undefined) finish({ kind: "spawn-error", error: errMessage(err) });
    });
    child.once("close", (code, signal) => {
      if (resolved) {
        unsettledResolve?.();
        return;
      }
      if (killReason) finish({ kind: "killed", reason: killReason });
      else finish({ kind: "exit", code, signal, stdout, stderr });
    });

    pruneTimer = setTimeout(() => startKill("timeout"), opts.timeoutMs);
    if (opts.signal?.aborted) onAbort();
    else opts.signal?.addEventListener("abort", onAbort, { once: true });
  });

export interface DindBuilderPruneOptions {
  dockerHost: string;
  /** Used in the log tokens until the api-assigned worker id is known. */
  workerName: string;
  /** Only READ: the leg never closes or opens it. */
  gate: DindPruneGate;
  log: Logger;
  readSample?: () => DindMeterSample | null;
  /** Wall clock, epoch ms. */
  now?: () => number;
  sleep?: (ms: number, signal?: AbortSignal) => Promise<void>;
  /** Prune child timeout (default 5 minutes). */
  pruneTimeoutMs?: number;
  /** How long to wait for the child's `close` after a kill (default 10 seconds). */
  settleTimeoutMs?: number;
  /** Test-only: stdout cap before an overflow kill (default 16 MiB). */
  maxStdoutBytes?: number;
  /** Test seam: the buildx plugin path the preflight checks. */
  pluginPath?: string;
  /** Test seam: the executable to spawn (default the docker client). */
  spawnFile?: string;
  /** Test seam: arguments placed before the guarded argv (e.g. a node fixture script). */
  spawnArgvPrefix?: readonly string[];
  /** Test seam: replaces the process-spawning wrapper. */
  exec?: BuilderPruneExec;
  /** Called after a latched, unsettled child's `close` is finally observed. */
  onSettled?: () => void;
}

/**
 * Build the builder leg, or undefined when this worker must never run it: the master switch
 * (the cache prune's predicate), the leg's own switch, and a wired docker daemon.
 */
export function createDindBuilderPrune(
  config: Pick<Config, "dindPruneEnabled" | "dindBuilderPruneEnabled" | "dockerWiring" | "workerName">,
  deps: Omit<DindBuilderPruneOptions, "dockerHost" | "workerName">,
): DindBuilderPruneController | undefined {
  const dockerHost = config.dockerWiring?.dockerHost;
  if (config.dindPruneEnabled !== true || config.dindBuilderPruneEnabled !== true || !dockerHost) return undefined;
  return new DindBuilderPruneController({ ...deps, dockerHost, workerName: config.workerName });
}

function boundedText(text: string): string {
  // eslint-disable-next-line no-control-regex
  const flat = text.replace(/[\u0000-\u001f\u007f]+/g, " ").trim();
  return flat.length > MAX_LOGGED_ERROR_CHARS ? `${flat.slice(0, MAX_LOGGED_ERROR_CHARS)}…` : flat;
}

/** Classifies a failed prune, in order: spawn failure, local kill, CLI text, daemon response. */
function classifyPruneFailure(res: BuilderPruneExecResult): "refused" | "unknown" | "conflict" | "rejected" {
  if (res.kind === "spawn-error") return "refused";
  if (res.kind === "killed") return "unknown";
  if (res.signal) return "unknown";
  const text = `${res.stdout}\n${res.stderr}`;
  if (text.includes(BUILDX_MISSING_TEXT)) return "refused";
  // A gRPC transport loss is not a daemon response: the request may have been submitted.
  const rpc = /rpc error: code = (\w+)/.exec(text);
  const daemon = text.includes("Error response from daemon:") || rpc !== null;
  if (!text.includes("Error response from daemon:") && rpc && TRANSPORT_RPC_CODES.has(rpc[1]!)) return "unknown";
  if (daemon) return text.includes("already running") ? "conflict" : "rejected";
  // A numeric non-zero exit with no daemon response: the request may have been submitted.
  return "unknown";
}

/** The `Total:` value only (never raw output), or undefined. */
function parseReclaimed(stdout: string): string | undefined {
  const m = /^Total:\s+(\S+)\s*$/m.exec(stdout);
  return m ? boundedText(m[1]!) : undefined;
}

export class DindBuilderPruneController {
  private readonly dockerHost: string;
  private readonly workerName: string;
  private readonly gate: DindPruneGate;
  private readonly log: Logger;
  private readonly readSampleRaw: () => DindMeterSample | null;
  private readonly now: () => number;
  private readonly sleep: (ms: number, signal?: AbortSignal) => Promise<void>;
  private readonly pruneTimeoutMs: number;
  private readonly settleTimeoutMs: number;
  private readonly maxStdoutBytes?: number;
  private readonly pluginPath: string;
  private readonly spawnFile: string;
  private readonly spawnArgvPrefix: readonly string[];
  private readonly exec: BuilderPruneExec;
  private readonly onSettled?: () => void;

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
  /** Set while a killed child's `close` has not been observed; in memory only. */
  private unsettled?: { pid: number; pgid: number; sinceMs: number };

  constructor(opts: DindBuilderPruneOptions) {
    this.dockerHost = opts.dockerHost;
    this.workerName = opts.workerName;
    this.gate = opts.gate;
    this.log = opts.log;
    this.readSampleRaw = opts.readSample ?? (() => readDindMeterSample());
    this.now = opts.now ?? Date.now;
    this.sleep = opts.sleep ?? abortableSleep;
    this.pruneTimeoutMs = opts.pruneTimeoutMs ?? PRUNE_TIMEOUT_MS;
    this.settleTimeoutMs = opts.settleTimeoutMs ?? SETTLE_TIMEOUT_MS;
    this.maxStdoutBytes = opts.maxStdoutBytes;
    this.pluginPath = opts.pluginPath ?? DEFAULT_BUILDX_PLUGIN_PATH;
    this.spawnFile = opts.spawnFile ?? DOCKER_BIN;
    this.spawnArgvPrefix = opts.spawnArgvPrefix ?? [];
    this.exec = opts.exec ?? execBuilderPrune;
    this.onSettled = opts.onSettled;
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
        await this.tick(signal);
      } catch (err) {
        try {
          this.log.warn("dind-builder-prune: evaluation failed", { error: boundedText(errMessage(err)) });
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
  async tick(signal?: AbortSignal): Promise<DindBuilderPruneOutcome> {
    const sample = this.readSample();
    if (!sample) {
      this.streak = 0;
      this.lastEpoch = undefined;
      return "no-sample";
    }
    if (this.unsettled) {
      // A killed child may still be running: spawn nothing and leave the backoff state alone.
      this.streak = 0;
      this.lastEpoch = sample.epochS;
      return "unsettled";
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
      this.log.info("dind-builder-prune: re-armed", { reason: below ? "below-low-watermark" : "backoff-elapsed" });
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
        this.log.info(`dind-builder-prune-deferred worker=${this.label()} reason=maintenance-fenced`, {});
      }
      return "maintenance-fenced";
    }
    return undefined;
  }

  private async attempt(before: DindMeterSample, signal?: AbortSignal): Promise<DindBuilderPruneOutcome> {
    if (this.inFlight) return "in-flight";
    this.inFlight = true;
    try {
      return await this.run(before, signal);
    } finally {
      this.inFlight = false;
    }
  }

  /** The buildx plugin must be a regular file (lstat, so a symlink does not count). */
  private pluginPresent(): boolean {
    try {
      return lstatSync(this.pluginPath).isFile();
    } catch {
      return false;
    }
  }

  private async run(before: DindMeterSample, signal?: AbortSignal): Promise<DindBuilderPruneOutcome> {
    const worker = this.label();
    const stop1 = this.blocked(signal);
    if (stop1) return stop1;
    if (!this.pluginPresent()) {
      this.log.warn(`dind-builder-prune-refused worker=${worker}`, { reason: "buildx-plugin-missing" });
      return "refused";
    }
    // Guard the production argv before any spawn, whatever test seam is in use.
    assertDindBuilderArgvAllowed(DIND_BUILDER_PRUNE_ARGV);
    // Synchronously immediately before the spawn: maintenance may have fenced meanwhile.
    const stop2 = this.blocked(signal);
    if (stop2) return stop2;
    // DOCKER_BUILDKIT=1 makes docker/cli fail hard instead of silently falling back to the
    // legacy builder when buildx is missing or broken. Never spreads process.env.
    const env = { ...dockerExecEnvironment(this.dockerHost), DOCKER_BUILDKIT: "1" };
    const res = await this.exec(DIND_BUILDER_PRUNE_ARGV, {
      file: this.spawnFile,
      argvPrefix: this.spawnArgvPrefix,
      env,
      timeoutMs: this.pruneTimeoutMs,
      settleTimeoutMs: this.settleTimeoutMs,
      ...(this.maxStdoutBytes !== undefined ? { maxStdoutBytes: this.maxStdoutBytes } : {}),
      ...(signal ? { signal } : {}),
    });
    if (res.kind === "killed" && res.unsettled) return this.latch(res.unsettled, res.reason, before);
    if (res.kind === "exit" && res.code === 0 && !res.signal) {
      const finishedAtMs = this.now();
      const reclaimed = parseReclaimed(res.stdout);
      this.log.info(`dind-builder-prune-completed worker=${worker}`, {
        before_bytes_pct: ratios(before).bytes_pct,
        before_inodes_pct: ratios(before).inodes_pct,
        ...(reclaimed !== undefined ? { reclaimed } : {}),
      });
      const after = await this.waitFreshSample(finishedAtMs, signal);
      this.log.info(`dind-builder-prune-after worker=${worker}`, after ? ratios(after) : { sample: "unavailable" });
      return "completed";
    }
    const kind = classifyPruneFailure(res);
    if (kind === "refused") {
      const text = res.kind === "exit" ? boundedText(`${res.stderr} ${res.stdout}`) : res.kind === "spawn-error" ? boundedText(res.error) : "";
      this.log.warn(`dind-builder-prune-refused worker=${worker}`, {
        reason: res.kind === "spawn-error" ? "spawn-failed" : "buildx-unavailable",
        error: text,
      });
    } else if (kind === "conflict") {
      const error = res.kind === "exit" ? boundedText(`${res.stderr} ${res.stdout}`) : "";
      this.log.warn(`dind-builder-prune-conflict worker=${worker}`, { reason: "prune-already-running", error });
    } else {
      const error = res.kind === "exit" ? boundedText(`${res.stderr} ${res.stdout}`) : "";
      this.log.warn(`dind-builder-prune-${kind} worker=${worker}`, {
        note: NO_CLAIM_NOTE,
        ...(res.kind === "killed" ? { kill_reason: res.reason } : {}),
        ...(res.kind === "exit" && res.code !== null ? { exit_code: res.code } : {}),
        error,
        ...ratios(before),
      });
    }
    return kind;
  }

  /** The killed child never closed within the bound: latch the leg until its `close` arrives. */
  private latch(child: UnsettledChild, reason: string, before: DindMeterSample): "unknown" {
    const worker = this.label();
    const sinceMs = this.now();
    this.unsettled = { pid: child.pid, pgid: child.pgid, sinceMs };
    this.log.warn(`dind-builder-prune-unknown worker=${worker}`, {
      note: NO_CLAIM_NOTE,
      reason: "settlement-unconfirmed",
      kill_reason: reason,
      ...ratios(before),
    });
    this.log.warn(`dind-builder-prune-unsettled worker=${worker}`, { pid: child.pid, pgid: child.pgid });
    void child.settled.then(() => {
      this.unsettled = undefined;
      try {
        this.log.info(`dind-builder-prune-settled worker=${this.label()}`, { after_ms: this.now() - sinceMs });
      } catch {
        // A broken logger must not leave the latch set.
      }
      this.onSettled?.();
    });
    return "unknown";
  }

  /** A valid sample taken strictly after `afterMs`, waiting up to AFTER_SAMPLE_WAIT_MS; null if none. */
  private async waitFreshSample(afterMs: number, signal?: AbortSignal): Promise<DindMeterSample | null> {
    const until = this.now() + AFTER_SAMPLE_WAIT_MS;
    for (;;) {
      const s = this.readSample();
      if (s && s.epochS * 1000 > afterMs) return s;
      const left = until - this.now();
      if (left <= 0 || signal?.aborted) return null;
      try {
        await this.sleep(Math.min(AFTER_SAMPLE_POLL_MS, left), signal);
      } catch {
        return null;
      }
    }
  }
}
