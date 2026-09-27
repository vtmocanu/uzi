import fs from "node:fs/promises";
import path from "node:path";
import type { Logger } from "./log.js";
import { rmHomeTree } from "./rmtree.js";
import { dropRunCaches } from "./run-caches.js";
import { isLiveModelPassHome } from "./model-pass.js";
import { DEFAULT_RECLAIM_MAX_CONSECUTIVE_FAILURES, TERMINAL_RUN_STATUSES, type RunStatusLookup } from "./home-reclaim.js";
import type { RunDiskLocks } from "./run-disk-locks.js";
import { errMessage, RUN_ID_RE, sleep } from "./util.js";

/**
 * PRD #1809 D7: the running, non-destructive disk reclaim. The startup sweep
 * (home-reclaim.ts) runs once per boot; this pass runs periodically and whenever the data
 * volume reaches the D5 soft threshold, and removes only what it can prove safe:
 *
 *  (a) a TERMINAL run's HOME (`agent-home/<runId>`) and provision dir
 *      (`provision/<runId>`), with the status confirmed by the api exactly as the startup
 *      sweep confirms it (home-reclaim.ts: every kind of not-knowing skips);
 *  (b) the rebuildable caches (D2, `dropRunCaches`) of a run PARKED WITH ITS PROCESS ENDED:
 *      api status `limit_wait`, `recovery_wait` or `paused`. Everything else in that HOME
 *      (the transcript, `.claude.json`, unknown files) stays for the resume. A gate status
 *      (`awaiting_approval`, `awaiting_input`, `awaiting_followup`) keeps a live executor
 *      and is never eligible; nor is `queued`, `claimed` or `running`;
 *  (c) a model-pass HOME (`uzi-judge-*`, `uzi-review-*`, `uzi-summary-*`) whose pass is
 *      proven over; see {@link DEFAULT_MODEL_PASS_MIN_AGE_MS} for the proof.
 *
 * **Never a live run.** A run this worker is executing is skipped before its status is even
 * asked for, and the check is repeated under the run's {@link RunDiskLocks} lock right
 * before anything is deleted. The runner marks a run executing synchronously when it
 * starts, then waits for the same lock, so a resume that arrives mid-deletion waits for the
 * deletion to finish (run-disk-locks.ts).
 *
 * **Why the status can be read outside the lock.** The status decides only WHICH removal
 * applies, and each removal stays safe if the status moves on after it was read:
 *  - a terminal status is final, so a run read as terminal is still terminal at deletion;
 *  - a run read as parked may since have been resumed. If that resume is on this worker
 *    it is executing, which the under-lock check sees. If it is elsewhere, its HOME here is
 *    no longer the one it uses (a HOME is per worker volume, see main.ts), and dropping
 *    rebuildable caches from it removes nothing the run needs.
 */

/** Api statuses of a run that parked with its process ended (D2): eligible for a cache drop. */
const PROCESS_ENDED_PARK_STATUSES: ReadonlySet<string> = new Set(["limit_wait", "recovery_wait", "paused"]);

/** The ephemeral HOMEs `runReadOnlyModelPass` creates under the HOME root (its `homePrefix`es). */
const MODEL_PASS_HOME_RE = /^uzi-(?:judge|review|summary)-/;

/**
 * How old (by mtime) a model-pass HOME that no pass in THIS process owns must be before the
 * reclaim removes it. The proof that its owner is stopped:
 *
 *  - A pass in this process registers its HOME right after `mkdtemp` and unregisters it
 *    only after its own cleanup ran (model-pass.ts `isLiveModelPassHome`), so a HOME a live
 *    pass here owns is always registered and is never removed, whatever its age.
 *  - An unregistered HOME therefore belongs to a pass that already finished here, or to
 *    another process: a previous life of this worker, or a second worker process sharing
 *    the volume. A pass holds its HOME for at most its wall-clock cap (5 minutes for the
 *    judge and review passes, 60 s by default for the summary pass) plus the abort grace
 *    and its own cleanup, after which it never writes there again. A HOME's mtime is never
 *    earlier than its creation, so an mtime older than this bound means the pass that
 *    created it started more than an hour ago and has ended.
 *
 * The one knob that can break the bound is SUMMARY_MODEL_TIMEOUT_MS: an operator who sets it
 * above an hour must also raise this (the `modelPassMinAgeMs` option).
 */
const DEFAULT_MODEL_PASS_MIN_AGE_MS = 60 * 60_000;

/** Wall-clock ceiling for one pass. Each cache drop carries its own five-minute deadline. */
const DEFAULT_PASS_DEADLINE_MS = 10 * 60_000;

/** Cap on run directories examined per pass (each costs one status lookup). */
const DEFAULT_PASS_MAX_ENTRIES = 500;

export interface DiskReclaimDeps {
  /** The run HOME root (`<dataDir>/agent-home`); model-pass HOMEs live here too. */
  homeRoot: string;
  /** The provision root (`<dataDir>/provision`). */
  provisionRoot: string;
  /** The api status lookup, with the startup sweep's contract (404 → undefined, else throw). */
  statusOf: RunStatusLookup;
  /** Whether the runner is executing the run in this process (RunRunner.isExecuting). */
  isRunLive: (runId: string) => boolean;
  locks: RunDiskLocks;
  log: Logger;
  /** Seams, injected by tests. */
  removeTree?: (dir: string) => Promise<void>;
  dropCaches?: (home: string, log: Logger) => Promise<void>;
  isModelPassLive?: (dir: string) => boolean;
  now?: () => number;
  modelPassMinAgeMs?: number;
  maxEntries?: number;
  maxConsecutiveFailures?: number;
  deadlineMs?: number;
}

/** What one pass did. Every examined run lands in exactly one of the run buckets. */
export interface DiskReclaimSummary {
  runsExamined: number;
  terminalHomesRemoved: number;
  provisionDirsRemoved: number;
  cachesDropped: number;
  skippedLive: number;
  skippedStatusUnknown: number;
  /** Neither terminal nor parked with its process ended (gates, queued, running, ...). */
  skippedNotEligible: number;
  /** A run for which at least one removal failed. */
  failed: number;
  unexamined: number;
  modelPassHomesRemoved: number;
  modelPassHomesLive: number;
  modelPassHomesTooRecent: number;
  modelPassHomesFailed: number;
  stoppedEarly?: "budget" | "api_unreachable" | "deadline";
}

interface RunDirs {
  home?: string;
  provision?: string;
}

/** Directory entries of `root` whose names pass `keep`, as absolute paths; [] when absent. */
async function listDirs(root: string, keep: (name: string) => boolean, log: Logger): Promise<Map<string, string>> {
  const out = new Map<string, string>();
  let entries;
  try {
    entries = await fs.readdir(root, { withFileTypes: true });
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code !== "ENOENT") {
      log.warn("disk reclaim could not read a directory", { dir: root, error: errMessage(err) });
    }
    return out;
  }
  // readdir types entries from lstat, so a symlink is never a directory here and is never
  // followed out of the data volume.
  for (const e of entries) if (e.isDirectory() && keep(e.name)) out.set(e.name, path.join(root, e.name));
  return out;
}

/**
 * One reclaim pass. Best-effort in every direction: it never throws, and every failure is
 * counted and logged in the summary line.
 */
export async function runDiskReclaimPass(deps: DiskReclaimDeps): Promise<DiskReclaimSummary> {
  const removeTree = deps.removeTree ?? ((dir: string) => rmHomeTree(dir));
  const dropCaches = deps.dropCaches ?? dropRunCaches;
  const isModelPassLive = deps.isModelPassLive ?? isLiveModelPassHome;
  const now = deps.now ?? Date.now;
  const modelPassMinAgeMs = deps.modelPassMinAgeMs ?? DEFAULT_MODEL_PASS_MIN_AGE_MS;
  const maxEntries = deps.maxEntries ?? DEFAULT_PASS_MAX_ENTRIES;
  const maxConsecutiveFailures = deps.maxConsecutiveFailures ?? DEFAULT_RECLAIM_MAX_CONSECUTIVE_FAILURES;
  const deadlineMs = deps.deadlineMs ?? DEFAULT_PASS_DEADLINE_MS;
  const log = deps.log;
  const startedAt = now();
  const summary: DiskReclaimSummary = {
    runsExamined: 0,
    terminalHomesRemoved: 0,
    provisionDirsRemoved: 0,
    cachesDropped: 0,
    skippedLive: 0,
    skippedStatusUnknown: 0,
    skippedNotEligible: 0,
    failed: 0,
    unexamined: 0,
    modelPassHomesRemoved: 0,
    modelPassHomesLive: 0,
    modelPassHomesTooRecent: 0,
    modelPassHomesFailed: 0,
  };

  const isRunId = (name: string) => RUN_ID_RE.test(name);
  const homes = await listDirs(deps.homeRoot, isRunId, log);
  const provisions = await listDirs(deps.provisionRoot, isRunId, log);
  const runs = new Map<string, RunDirs>();
  for (const [id, dir] of homes) runs.set(id, { home: dir });
  for (const [id, dir] of provisions) runs.set(id, { ...runs.get(id), provision: dir });

  let consecutiveFailures = 0;
  for (const [runId, dirs] of runs) {
    if (summary.runsExamined >= maxEntries) {
      summary.stoppedEarly = "budget";
      break;
    }
    if (now() - startedAt >= deadlineMs) {
      summary.stoppedEarly = "deadline";
      break;
    }
    summary.runsExamined += 1;
    if (deps.isRunLive(runId)) {
      summary.skippedLive += 1;
      continue;
    }
    // The startup sweep's status contract (home-reclaim.ts RunStatusLookup): a throw is a
    // could-not-ask that counts toward the bail, undefined is a 404 answer that skips.
    let status: string | undefined;
    try {
      status = await deps.statusOf(runId);
      consecutiveFailures = 0;
    } catch {
      summary.skippedStatusUnknown += 1;
      consecutiveFailures += 1;
      if (consecutiveFailures >= maxConsecutiveFailures) {
        summary.stoppedEarly = "api_unreachable";
        break;
      }
      continue;
    }
    if (status === undefined) {
      summary.skippedStatusUnknown += 1;
      continue;
    }
    const terminal = TERMINAL_RUN_STATUSES.has(status);
    if (!terminal && !(PROCESS_ENDED_PARK_STATUSES.has(status) && dirs.home)) {
      summary.skippedNotEligible += 1;
      continue;
    }
    await deps.locks.withLock(runId, async () => {
      // Re-checked under the lock: a claim of this run that started after the check above
      // is executing now, and the runner is waiting on this lock, so leave everything.
      if (deps.isRunLive(runId)) {
        summary.skippedLive += 1;
        return;
      }
      if (!terminal) {
        // dropRunCaches never throws; it logs what it dropped and what it could not.
        await dropCaches(dirs.home as string, log);
        summary.cachesDropped += 1;
        return;
      }
      let failed = false;
      for (const [dir, key] of [
        [dirs.home, "terminalHomesRemoved"],
        [dirs.provision, "provisionDirsRemoved"],
      ] as const) {
        if (!dir) continue;
        try {
          await removeTree(dir);
          summary[key] += 1;
        } catch (err) {
          failed = true;
          log.warn("disk reclaim could not remove a terminal run's directory", { run_id: runId, dir, error: errMessage(err) });
        }
      }
      if (failed) summary.failed += 1;
    });
  }
  summary.unexamined = runs.size - summary.runsExamined;

  // (c) model-pass HOMEs. No lock: a pass always creates a fresh mkdtemp name, so nothing
  // ever resumes into an old one; the registry and the age bound are the whole guard.
  const passHomes = await listDirs(deps.homeRoot, (name) => MODEL_PASS_HOME_RE.test(name), log);
  for (const dir of passHomes.values()) {
    if (now() - startedAt >= deadlineMs) {
      summary.stoppedEarly ??= "deadline";
      break;
    }
    if (isModelPassLive(dir)) {
      summary.modelPassHomesLive += 1;
      continue;
    }
    let mtimeMs: number;
    try {
      mtimeMs = (await fs.lstat(dir)).mtimeMs;
    } catch {
      continue; // gone already
    }
    if (now() - mtimeMs < modelPassMinAgeMs) {
      summary.modelPassHomesTooRecent += 1;
      continue;
    }
    try {
      await removeTree(dir);
      summary.modelPassHomesRemoved += 1;
    } catch (err) {
      summary.modelPassHomesFailed += 1;
      log.warn("disk reclaim could not remove a stranded model-pass HOME", { dir, error: errMessage(err) });
    }
  }

  log.info("disk reclaim pass complete", {
    runs_examined: summary.runsExamined,
    terminal_homes_removed: summary.terminalHomesRemoved,
    provision_dirs_removed: summary.provisionDirsRemoved,
    caches_dropped: summary.cachesDropped,
    skipped_live: summary.skippedLive,
    skipped_status_unknown: summary.skippedStatusUnknown,
    skipped_not_eligible: summary.skippedNotEligible,
    failed: summary.failed,
    unexamined: summary.unexamined,
    model_pass_homes_removed: summary.modelPassHomesRemoved,
    model_pass_homes_live: summary.modelPassHomesLive,
    model_pass_homes_too_recent: summary.modelPassHomesTooRecent,
    model_pass_homes_failed: summary.modelPassHomesFailed,
    stopped_early: summary.stoppedEarly,
    took_ms: now() - startedAt,
  });
  return summary;
}

/** PRD #1809 D5: the api's default recycle threshold, assumed until a heartbeat says otherwise. */
export const DEFAULT_DISK_PRESSURE_THRESHOLD = 0.9;

/** Minimum spacing between two reclaim passes triggered by pressure (the periodic pass is not spaced). */
const DEFAULT_PRESSURE_SPACING_MS = 60_000;

export interface DiskPressureOptions {
  /** Distance of the soft threshold below the api threshold (config.diskSoftMargin). */
  softMargin: number;
  /** The api's recycle threshold, or undefined when no heartbeat has carried one. */
  thresholdOf: () => number | undefined;
  /** One reclaim pass (production: {@link runDiskReclaimPass} bound to its deps). */
  reclaim: () => Promise<unknown>;
  /** Periodic pass cadence (config.diskReclaimIntervalMs). */
  intervalMs: number;
  log: Logger;
  pressureSpacingMs?: number;
  now?: () => number;
}

/**
 * PRD #1809 D5/D7: the worker's disk-pressure state. The heartbeat loop feeds it the data
 * volume's used fraction on every stats tick ({@link observe}); the run lane's claim loop
 * asks {@link claimsBlocked} before each claim; {@link loop} runs the periodic reclaim.
 *
 * Admission fails OPEN: an unknown sample (a statfs failure, no sample yet) never blocks a
 * claim. The stop is an optimisation that keeps a filling volume from taking on new work;
 * the guarantee for a volume that does fill is D6's typed handling, not this.
 */
export class DiskPressureController {
  private readonly now: () => number;
  private readonly pressureSpacingMs: number;
  private blocked = false;
  private inFlight: Promise<void> | undefined;
  private lastPressurePassAt = Number.NEGATIVE_INFINITY;

  constructor(private readonly opts: DiskPressureOptions) {
    this.now = opts.now ?? Date.now;
    this.pressureSpacingMs = opts.pressureSpacingMs ?? DEFAULT_PRESSURE_SPACING_MS;
  }

  /** The soft threshold: the api threshold (default 0.90) minus the soft margin. */
  softThreshold(): number {
    return Math.max(0, (this.opts.thresholdOf() ?? DEFAULT_DISK_PRESSURE_THRESHOLD) - this.opts.softMargin);
  }

  /** Whether the run lane must take no new claim: the latest sample is at or over the soft threshold. */
  claimsBlocked(): boolean {
    return this.blocked;
  }

  /**
   * Record the latest data-volume used fraction. Logs once per transition across the soft
   * threshold, and at or over it requests a (rate-limited) reclaim pass. Never throws and
   * never waits for the pass.
   */
  observe(usedFraction: number | undefined): void {
    const soft = this.softThreshold();
    const over = usedFraction !== undefined && usedFraction >= soft;
    if (over !== this.blocked) {
      this.blocked = over;
      const fields = { used_fraction: usedFraction ?? null, soft_threshold: soft };
      if (over) this.opts.log.warn("data volume at or over the soft threshold; claiming no new run until it drops", fields);
      else this.opts.log.info("data volume back under the soft threshold; claiming resumes", fields);
    }
    if (over) void this.requestReclaim("pressure");
  }

  /**
   * Start a reclaim pass unless one is running (then its promise is returned) or, for a
   * pressure trigger, the last pressure-triggered pass started less than the spacing ago
   * (then undefined). The returned promise never rejects.
   */
  requestReclaim(reason: "periodic" | "pressure"): Promise<void> | undefined {
    if (this.inFlight) return this.inFlight;
    if (reason === "pressure") {
      if (this.now() - this.lastPressurePassAt < this.pressureSpacingMs) return undefined;
      this.lastPressurePassAt = this.now();
    }
    const pass = (async () => {
      try {
        await this.opts.reclaim();
      } catch (err) {
        this.opts.log.warn("disk reclaim pass failed", { reason, error: errMessage(err) });
      } finally {
        this.inFlight = undefined;
      }
    })();
    this.inFlight = pass;
    return pass;
  }

  /** The periodic pass, every `intervalMs` until `signal` aborts. Never throws. */
  async loop(signal: AbortSignal): Promise<void> {
    while (!signal.aborted) {
      await sleep(this.opts.intervalMs, signal);
      if (signal.aborted) break;
      await this.requestReclaim("periodic");
    }
    // A pass still in flight is not awaited: shutdown must fit in the termination grace, and a
    // pass cut short leaves nothing inconsistent (every removal is idempotent; the next pass,
    // or the startup sweep, finishes it).
  }
}
