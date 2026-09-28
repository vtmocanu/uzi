// PRD #1809 D4: bounded cache growth while a Claude run is running.
//
// Two layers, because one long build inside a single turn can fill the data volume before any
// turn boundary comes:
//
//  - the SOFT layer, the per-run cache cap. At every implement turn boundary the executor asks
//    {@link DiskGovernor.boundary}; a run whose caches exceed its cap is TRIMMED (least recently
//    used entries first, run-caches.ts trimRunCaches) only at a proven quiet point: between turns
//    AND with no process of the run alive, never judged from file mtimes. "Alive" is the executor's
//    probe: its CLI process groups, plus every process attributed to the run by environment
//    (`HOME=<run HOME>`) or working directory (run-procs.ts), since the CLI runs each Bash command
//    detached, outside its groups; an unknown answer is alive. A run that stays over the cap is
//    parked preventively (see {@link SOFT_PARK_RULE}).
//  - the HARD layer, the per-tick pressure stop. {@link DiskGovernor.observe} runs on every stats
//    tick (the heartbeat cadence) independently of turn boundaries; at or over the hard threshold
//    it stops the running Claude run with the largest caches through the steering channel's
//    worker-local `disk` pause mode (the same turn drop as an owner's `pause --now`) and the
//    executor parks it with a COUNTED `data_volume_full` park. Only a run inside its implement
//    loop is stoppable: a run still cloning, planning or waiting at its plan gate, and a run that
//    has left the loop to finalize, is never a candidate (the stop is only routed to the disk park
//    at an implement boundary or turn). A stop that has not produced a park within
//    {@link STOP_TIMEOUT_MS} is given up, so one lost stop cannot wedge the hard layer. A stop
//    landing while the run sits in a long wait inside the implement loop is
//    honoured only when that wait returns, so the give-up can let the hard layer stop a SECOND run
//    for the same pressure event while the first stop is still pending.
//
// Both parks end the executor, so the runner's park cache drop (run-caches.ts dropRunCaches, D2)
// runs on the way out. Both layers can be disabled (UZI_RUN_CACHE_CAP_ENABLED,
// UZI_DISK_HARD_STOP_ENABLED).

import type { Logger } from "./log.js";
import { measureRunCaches, type RunCacheBytes } from "./rmtree.js";
import { type TrimResult, type TrimTarget, trimRunCaches } from "./run-caches.js";
import { DEFAULT_DISK_PRESSURE_THRESHOLD } from "./disk-reclaim.js";
import { errMessage } from "./util.js";

/**
 * Thrown by the executor at a turn boundary (soft layer, `preventive: true`) or when the hard
 * layer's `disk` pause drops the turn (`preventive: false`). The runner catches it and takes the
 * mid-run `data_volume_full` recovery park: it reaps the run's process tree, captures what is
 * committed, and parks the run in `recovery_wait` with `recovery_cause: data_volume_full`, sending
 * `disk_park_preventive: true` for a soft park (uncounted) and nothing for a hard stop (counted).
 */
export class DiskParkSignal extends Error {
  constructor(readonly preventive: boolean) {
    super(preventive ? "run caches stayed over the cap; parking to drop them" : "data volume at the hard threshold; parking");
    this.name = "DiskParkSignal";
  }
}

/**
 * The soft layer's "stays over" rule. A run is parked preventively at a turn boundary when its
 * caches are still over the cap after whatever that boundary could do, and either
 *  - this is the {@link SOFT_PARK_BOUNDARIES}th turn boundary in a row that ends over the cap (a
 *    background process keeps blocking the trim, or the trim cannot get under it), or
 *  - a live process blocks the trim and the caches are already at or over
 *    {@link SOFT_PARK_RUNAWAY} times the cap (growth is running away and no quiet point is coming).
 * A boundary that ends at or under the cap resets the count.
 */
const SOFT_PARK_RULE = "3 boundaries over the cap in a row, or 1.25x the cap with a live process";
const SOFT_PARK_BOUNDARIES = 3;
const SOFT_PARK_RUNAWAY = 1.25;

/** One boundary's measurement and the whole hard-layer selection each get this long. */
const MEASURE_DEADLINE_MS = 60_000;

/** A hard stop that has not ended its run's flight within this long is given up (logged), and
 *  the hard layer may stop another run. */
const STOP_TIMEOUT_MS = 10 * 60_000;

/** While the volume stays over the hard threshold, the candidates are measured again at most this
 *  often (every stats tick would otherwise re-walk every run's caches). */
const SELECT_INTERVAL_MS = 60_000;

/** PRD #1809 D4: the knobs, from config.ts. */
export interface CacheCapConfig {
  /** UZI_RUN_CACHE_CAP_ENABLED: the soft layer. */
  capEnabled: boolean;
  /** UZI_RUN_CACHE_CAP_FRACTION: the share of the data volume all runs' caches may use. */
  capFraction: number;
  /** UZI_RUN_CACHE_LOW_WATER: where a trim stops, as a fraction of the cap. */
  lowWater: number;
  /** WORKER_MAX_CONCURRENT_RUNS: the cap is divided among this many runs. */
  maxConcurrentRuns: number;
  /** UZI_DISK_HARD_STOP_ENABLED: the hard layer. */
  hardStopEnabled: boolean;
  /** UZI_DISK_HARD_MARGIN: the hard threshold's distance below the api threshold. */
  hardMargin: number;
}

/**
 * One run's cache cap in bytes: `fraction` of the data volume's size divided by the worker's
 * maximum concurrent runs (0.5 of 25 GiB over 2 runs = 6.25 GiB). Undefined when the volume's
 * size is unknown or zero: no cap is then enforced.
 */
export function runCacheCapBytes(volumeTotalBytes: number | undefined, fraction: number, maxConcurrentRuns: number): number | undefined {
  if (volumeTotalBytes === undefined || !(volumeTotalBytes > 0)) return undefined;
  return Math.floor((volumeTotalBytes * fraction) / Math.max(1, maxConcurrentRuns));
}

/**
 * The hard layer's threshold: the api's recycle threshold (default 0.90) minus the hard margin
 * (default 0.03), so 0.87. A margin at or above the threshold would stop runs at any usage; the
 * threshold itself is then used, as the soft threshold does (DiskPressureController).
 */
export function hardStopThreshold(apiThreshold: number | undefined, margin: number): number {
  const threshold = apiThreshold ?? DEFAULT_DISK_PRESSURE_THRESHOLD;
  const hard = threshold - margin;
  return hard > 0 ? hard : threshold;
}

/** What the governor needs from the rest of the worker; every I/O is a seam for tests. */
export interface DiskGovernorOptions {
  config: CacheCapConfig;
  log: Logger;
  /** The data volume's size in bytes (statfs), or undefined when it cannot be read. */
  volumeTotalBytes: () => number | undefined;
  /** The api's recycle threshold from the latest heartbeat, or undefined before one. */
  thresholdOf: () => number | undefined;
  /** One D7 reclaim pass (DiskPressureController.reclaimNow), run after a hard stop's park. */
  reclaim?: () => Promise<void>;
  /** Measure a run HOME's caches (default {@link measureRunCaches}). */
  measure?: (home: string, opts: { deadline: number }) => Promise<RunCacheBytes>;
  /** Trim a run HOME's caches (default {@link trimRunCaches}). */
  trim?: (home: string, target: TrimTarget, log: Logger) => Promise<TrimResult>;
  now?: () => number;
}

/** A running Claude run the governor watches. */
export interface GovernedRun {
  /** The run's private HOME, where its caches live. */
  home: string;
  /** Stop the run's current turn through the steering channel's `disk` pause mode. */
  requestStop: () => void;
}

interface RunState extends GovernedRun {
  /** Latest measured cache bytes (a boundary's or the hard layer's), if any. */
  cacheBytes?: number;
  /** Consecutive turn boundaries that ended over the cap. */
  overStreak: number;
  /** The run reached its implement loop, where a `disk` stop is routed to the disk park. */
  stoppable: boolean;
  /** The hard layer asked this run to stop. */
  stopRequested: boolean;
}

/** The executor's quiet-point probe: resolves true while any process of the run is alive. */
export type ProcessAliveProbe = () => Promise<boolean>;

/**
 * PRD #1809 D4: the worker's per-run cache cap (soft layer) and mid-turn pressure stop (hard
 * layer). One per worker; the run runner registers each running Claude run that can take the
 * fenced disk park and unregisters it when its flight ends.
 */
export class DiskGovernor {
  private readonly runs = new Map<string, RunState>();
  private readonly now: () => number;
  private readonly measure: NonNullable<DiskGovernorOptions["measure"]>;
  private readonly trim: NonNullable<DiskGovernorOptions["trim"]>;
  /** The run the hard layer stopped, until its flight ends (or it leaves its implement loop
   *  without parking, or {@link STOP_TIMEOUT_MS} passes). At most one at a time. */
  private stopping: string | undefined;
  /** When {@link stopping} was asked to stop. */
  private stopRequestedAt = 0;
  /** When the last hard-layer selection that stopped nothing ended ({@link SELECT_INTERVAL_MS}). */
  private lastSelectionAt = Number.NEGATIVE_INFINITY;
  /** A hard-layer selection (measuring the candidates) is in flight. */
  private selecting = false;
  /** When the last stopped run's flight ended: a sample taken before it is not fresh. */
  private lastStopEndedAt = Number.NEGATIVE_INFINITY;
  /** One "over the hard threshold, nothing to stop" warning per stretch over it. */
  private warnedNothingToStop = false;

  constructor(private readonly opts: DiskGovernorOptions) {
    this.now = opts.now ?? Date.now;
    this.measure = opts.measure ?? ((home, o) => measureRunCaches(home, o));
    this.trim = opts.trim ?? ((home, target, log) => trimRunCaches(home, target, log));
  }

  /** Whether either layer is on; the runner registers nothing otherwise. */
  get enabled(): boolean {
    return this.opts.config.capEnabled || this.opts.config.hardStopEnabled;
  }

  /** Watch a running Claude run. Idempotent per run id (a later call replaces the entry). */
  register(runId: string, run: GovernedRun): void {
    this.runs.set(runId, { ...run, overStreak: 0, stoppable: false, stopRequested: false });
  }

  /**
   * Stop watching a run (its flight ended: parked, finished or failed). When it was the run the
   * hard layer stopped, the next stop waits for a sample taken after this, and one D7 reclaim runs
   * now (the park's cache drop has already run in the flight's finally).
   */
  unregister(runId: string): void {
    if (!this.runs.delete(runId)) return;
    if (this.stopping !== runId) return;
    this.releaseStop();
    if (this.opts.reclaim) {
      void this.opts.reclaim().catch((e: unknown) =>
        this.opts.log.warn("disk reclaim after a pressure stop failed", { run_id: runId, error: errMessage(e) }),
      );
    }
  }

  /**
   * The run left its implement loop (its executor returned or threw): it is no longer a hard-stop
   * candidate. `diskParked` says it left for a disk park, whose flight end ({@link unregister})
   * releases the stop as before; otherwise a stop asked of it will never become a park (it
   * finished, failed, or parked for another reason first), so it is released now.
   */
  leftLoop(runId: string, diskParked: boolean): void {
    const run = this.runs.get(runId);
    if (run) run.stoppable = false;
    if (this.stopping !== runId || diskParked) return;
    this.opts.log.warn("the run the hard layer stopped left its implement loop without a disk park; releasing the stop", {
      run_id: runId,
    });
    this.releaseStop();
  }

  private releaseStop(): void {
    this.stopping = undefined;
    this.lastStopEndedAt = this.now();
  }

  /** The cap in bytes, from the volume's current size. */
  capBytes(): number | undefined {
    const c = this.opts.config;
    return runCacheCapBytes(this.opts.volumeTotalBytes(), c.capFraction, c.maxConcurrentRuns);
  }

  /**
   * The SOFT layer at one implement turn boundary. Marks the run stoppable (it is in its
   * implement loop), measures its caches, and when they are over the cap: at a proven quiet point
   * (`processAlive` resolves false) trims them, otherwise counts the boundary as blocked.
   * Resolves "park" when the run stays over the cap by {@link SOFT_PARK_RULE}, else "continue".
   * Never throws: a failed measurement or trim continues the run.
   */
  async boundary(runId: string, processAlive: ProcessAliveProbe): Promise<"continue" | "park"> {
    const run = this.runs.get(runId);
    if (!run) return "continue";
    run.stoppable = true;
    // A hard stop is already on its way to this run: its pending turn drop parks it.
    if (!this.opts.config.capEnabled || run.stopRequested) return "continue";
    const cap = this.capBytes();
    if (cap === undefined) return "continue";
    const measured = await this.measureRun(run);
    if (measured === undefined) return "continue";
    if (measured <= cap) {
      run.overStreak = 0;
      return "continue";
    }
    const fields = { run_id: runId, cache_bytes: measured, cap_bytes: cap };
    let alive: boolean;
    try {
      alive = await processAlive();
    } catch {
      alive = true; // unknown is not quiet
    }
    let bytes = measured;
    if (alive) {
      this.opts.log.warn("run caches over the cap, but a process of the run is still alive; not trimming", fields);
    } else {
      const lowWater = Math.floor(cap * this.opts.config.lowWater);
      let result: TrimResult | undefined;
      try {
        result = await this.trim(run.home, { totalBytes: measured, capBytes: cap, lowWaterBytes: lowWater }, this.opts.log);
      } catch (e) {
        this.opts.log.warn("run cache trim failed", { ...fields, error: errMessage(e) });
      }
      bytes = (await this.measureRun(run)) ?? result?.estimatedBytesAfter ?? measured;
      this.opts.log.info("run caches trimmed between turns", {
        ...fields,
        low_water_bytes: lowWater,
        cache_bytes_after: bytes,
        ...(result
          ? {
              go_build_units: result.goBuildUnits,
              go_build_bytes: result.goBuildBytes,
              npm_buckets: result.npmBuckets,
              npm_contents: result.npmContents,
              npm_bytes: result.npmBytes,
              module_cache_removed: result.moduleCacheRemoved,
              kept_changed: result.kept,
              ...(result.npmSkipped ? { npm_skipped: result.npmSkipped } : {}),
              ...(result.outOfTime ? { out_of_time: true } : {}),
            }
          : {}),
      });
      if (bytes <= cap) {
        run.overStreak = 0;
        return "continue";
      }
    }
    run.overStreak += 1;
    // A hard stop asked while this boundary measured or trimmed parks the run COUNTED at the
    // executor's next check; the soft layer does not also resolve a (preventive) park.
    if (run.stopRequested) return "continue";
    const runaway = alive && bytes >= cap * SOFT_PARK_RUNAWAY;
    if (run.overStreak >= SOFT_PARK_BOUNDARIES || runaway) {
      this.opts.log.warn("run caches stayed over the cap; parking the run so its caches are dropped", {
        ...fields,
        cache_bytes: bytes,
        boundaries_over: run.overStreak,
        process_alive: alive,
        rule: SOFT_PARK_RULE,
      });
      return "park";
    }
    return "continue";
  }

  /**
   * The HARD layer, on every stats tick: the data volume's used fraction (undefined = unknown,
   * never acts). At or over {@link hardStopThreshold}, and with no earlier stop still in flight,
   * measures the stoppable runs and stops the one with the largest caches. One stop at a time: the
   * next waits until the stopped run's flight has ended (parked) AND a sample taken after that is
   * still over: `sampledAtMs` is when the sample was taken (the heartbeat observes it only after
   * its round-trip), and a sample taken before the last stopped run ended is ignored. While the
   * volume stays over, a selection that stopped nothing (which measured every candidate) is not
   * repeated for {@link SELECT_INTERVAL_MS}. A stop still pending after {@link STOP_TIMEOUT_MS} is given up
   * here. Never throws and never waits.
   */
  observe(usedFraction: number | undefined, sampledAtMs: number = this.now()): void {
    if (!this.opts.config.hardStopEnabled) return;
    if (this.stopping !== undefined && this.now() - this.stopRequestedAt >= STOP_TIMEOUT_MS) {
      this.opts.log.warn("a pressure stop did not produce a park in time; giving it up", {
        run_id: this.stopping,
        timeout_ms: STOP_TIMEOUT_MS,
      });
      this.releaseStop();
    }
    if (usedFraction === undefined) return;
    if (sampledAtMs < this.lastStopEndedAt) return;
    const threshold = hardStopThreshold(this.opts.thresholdOf(), this.opts.config.hardMargin);
    if (usedFraction < threshold) {
      this.warnedNothingToStop = false;
      this.lastSelectionAt = Number.NEGATIVE_INFINITY;
      return;
    }
    if (this.stopping !== undefined || this.selecting) return;
    if (this.now() - this.lastSelectionAt < SELECT_INTERVAL_MS) return;
    const candidates = [...this.runs.entries()].filter(([, r]) => r.stoppable && !r.stopRequested);
    if (candidates.length === 0) {
      if (!this.warnedNothingToStop) {
        this.warnedNothingToStop = true;
        this.opts.log.warn("data volume at the hard threshold, but no running Claude run can be stopped", {
          used_fraction: usedFraction,
          hard_threshold: threshold,
        });
      }
      return;
    }
    this.selecting = true;
    void this.selectAndStop(candidates, usedFraction, threshold).finally(() => {
      this.selecting = false;
    });
  }

  /** Measure the candidates (fresh, one shared deadline; a failed one falls back to its last
   *  reading) and stop the one with the most cache bytes, when any has some. */
  private async selectAndStop(candidates: [string, RunState][], usedFraction: number, threshold: number): Promise<void> {
    const deadline = this.now() + MEASURE_DEADLINE_MS;
    let pick: [string, RunState] | undefined;
    for (const entry of candidates) {
      const [, run] = entry;
      if (this.now() < deadline) await this.measureRun(run, deadline);
      if ((run.cacheBytes ?? 0) > (pick?.[1].cacheBytes ?? 0)) pick = entry;
    }
    if (!pick) {
      this.lastSelectionAt = this.now();
      this.opts.log.warn("data volume at the hard threshold, but no running Claude run holds cache bytes to stop for", {
        used_fraction: usedFraction,
        hard_threshold: threshold,
      });
      return;
    }
    const [runId, run] = pick;
    // It may have ended (or left its implement loop) while it was being measured.
    if (this.runs.get(runId) !== run || !run.stoppable) {
      this.lastSelectionAt = this.now();
      return;
    }
    run.stopRequested = true;
    this.stopping = runId;
    this.stopRequestedAt = this.now();
    this.opts.log.warn("data volume at the hard threshold; stopping the run with the largest caches and parking it", {
      run_id: runId,
      cache_bytes: run.cacheBytes,
      used_fraction: usedFraction,
      hard_threshold: threshold,
      cause: "data_volume_full",
    });
    run.requestStop();
  }

  /** Measure one run's caches, recording the reading; undefined when it failed. */
  private async measureRun(run: RunState, deadline = this.now() + MEASURE_DEADLINE_MS): Promise<number | undefined> {
    try {
      const m = await this.measure(run.home, { deadline });
      run.cacheBytes = m.cacheBytes;
      return m.cacheBytes;
    } catch (e) {
      this.opts.log.warn("run cache measurement failed", { run_home: run.home, error: errMessage(e) });
      return undefined;
    }
  }
}
