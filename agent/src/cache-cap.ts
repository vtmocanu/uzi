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
//  - the HARD layer, the per-tick pressure action. {@link DiskGovernor.observe} runs on every stats
//    tick (the heartbeat cadence) independently of turn boundaries. Issue #1830: EVERY registered
//    run is a candidate from `register` to `unregister` (cloning, planning, gates, implement,
//    finalize), and at or over the hard threshold the run with the largest caches gets ONE of two
//    actions, decided AFTER it is measured:
//      1. STOP, when it is *parkable*: its executor is in flight (`enterRun` .. `leftLoop`), the
//         last status the runner SENT is `running`, and the server has ACKed `running` for a send
//         newer than any non-`running` send and any declined or unreadable ACK (a same-status
//         `running` send still in flight keeps a run parkable). The stop goes through the steering channel's worker-local `disk` pause
//         mode (the same turn drop as an owner's `pause --now`) and the executor parks the run with
//         a COUNTED `data_volume_full` park. This reaches a first planning turn and an implement
//         turn alike. The disk park only lands from the server status `running`
//         (ParkRunDataVolumeFull), so no stop is ever requested otherwise.
//      2. IN-PLACE RECLAIM, for every other run (setup or clone, revision turns, gate and question
//         waits, the approve-to-first-report window, finalize, a declined, unreadable or not yet ACKed `running`): the
//         run is not stopped, it keeps its gate, its pending approval and its flight, and its
//         rebuildable caches are dropped where it stands ({@link GovernedRun.reclaimInPlace}). No
//         process is killed. It holds its own {@link DiskGovernor.reclaiming} slot, never
//         `stopping`, so a later park's release logic is untouched. A run that still holds bytes
//         after the drop (or whose drop threw) is skipped for the rest of the over-threshold
//         stretch, so the next-heaviest run (possibly parkable) is acted on instead.
//    The runner closes the stop-then-wait race at its `reportState` choke point: a report of an
//    `awaiting_*` status while a `disk` stop is pending throws {@link DiskParkSignal} instead of
//    going out, so the park lands from `running`. A stop that has not produced a park within
//    {@link STOP_TIMEOUT_MS} is given up, so one lost stop cannot wedge the hard layer, which can
//    then stop a SECOND run for the same pressure event while the first stop is still pending.
//
// Both parks end the executor, so the runner's park cache drop (run-caches.ts dropRunCaches, D2)
// runs on the way out. Both layers can be disabled (UZI_RUN_CACHE_CAP_ENABLED,
// UZI_DISK_HARD_STOP_ENABLED).

import type { Logger } from "./log.js";
import { measureRunCaches, type RunCacheBytes } from "./rmtree.js";
import { type RunCacheDropResult, type TrimResult, type TrimTarget, trimRunCaches } from "./run-caches.js";
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
  /** Issue #1830: drop the run's rebuildable caches where it stands, for a run that cannot be
   *  parked right now. Resolves what was dropped; never needs to reject (a throw is logged). */
  reclaimInPlace: () => Promise<RunCacheDropResult | undefined>;
}

interface RunState extends GovernedRun {
  /** Latest measured cache bytes (a boundary's or the hard layer's), if any. */
  cacheBytes?: number;
  /** Consecutive turn boundaries that ended over the cap. */
  overStreak: number;
  /** Issue #1830: `executor.run` is in flight ({@link DiskGovernor.enterRun} .. `leftLoop`). */
  inExecutor: boolean;
  /** The status of the latest report the runner sent, and its send number. */
  sentStatus?: string;
  sentSeq: number;
  /** The number of the latest send whose status was not `running` (0 = none yet). */
  lastNonRunningSentSeq: number;
  /** The highest send number whose ACK carried the server status `running` (0 = none yet). */
  runningAckedSeq: number;
  /** The highest send number whose ACK was declined or unreadable (any status but `running`,
   *  undefined included; 0 = none yet). It cancels every earlier `running` ACK. */
  lastNonRunningAckedSeq: number;
  /** The server status of the newest-send ACK recorded, for the in-place drop's log line only. */
  ackedStatus?: string;
  /** The send number {@link ackedStatus} belongs to. */
  ackedStatusSeq: number;
  /** The hard layer asked this run to stop. */
  stopRequested: boolean;
  /** Issue #1830: an in-place drop left this run holding bytes (or threw); it is skipped for the
   *  rest of the over-threshold stretch so the next-heaviest run is acted on. */
  inPlaceSkipped: boolean;
}

/** The executor's quiet-point probe: resolves true while any process of the run is alive. */
export type ProcessAliveProbe = () => Promise<boolean>;

/**
 * PRD #1809 D4: the worker's per-run cache cap (soft layer) and mid-turn pressure stop (hard
 * layer). One per worker; the run runner registers each Claude run from its claim and unregisters
 * it when its flight ends.
 */
export class DiskGovernor {
  private readonly runs = new Map<string, RunState>();
  private readonly now: () => number;
  private readonly measure: NonNullable<DiskGovernorOptions["measure"]>;
  private readonly trim: NonNullable<DiskGovernorOptions["trim"]>;
  /** The run the hard layer stopped, until its flight ends (or it leaves its executor
   *  without parking, or {@link STOP_TIMEOUT_MS} passes). At most one at a time. */
  private stopping: string | undefined;
  /** When {@link stopping} was asked to stop. */
  private stopRequestedAt = 0;
  /** Issue #1830: the run an in-place drop is working on. Its own slot, never {@link stopping}: a
   *  drop is not a park, so leftLoop, unregister's D7 reclaim and the give-up timeout skip it. */
  private reclaiming: string | undefined;
  /** When the last in-place drop ended: a sample taken before it is not fresh. */
  private lastReclaimEndedAt = Number.NEGATIVE_INFINITY;
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
    this.runs.set(runId, { ...run, overStreak: 0, inExecutor: false, sentSeq: 0, lastNonRunningSentSeq: 0, runningAckedSeq: 0, lastNonRunningAckedSeq: 0, ackedStatusSeq: 0, stopRequested: false, inPlaceSkipped: false });
  }

  /** Issue #1830: `executor.run` is about to start; the run may now take a stop (see {@link parkable}). */
  enterRun(runId: string): void {
    const run = this.runs.get(runId);
    if (!run) return;
    run.inExecutor = true;
    run.inPlaceSkipped = false;
  }

  /**
   * Issue #1830: the runner is about to send a report carrying `status`. Returns the send's number
   * for {@link statusAcked}. The ACK of an earlier send no longer counts from here on.
   */
  statusRequested(runId: string, status: string): number {
    const run = this.runs.get(runId);
    if (!run) return 0;
    if (run.sentStatus !== status) run.inPlaceSkipped = false;
    run.sentStatus = status;
    run.sentSeq += 1;
    if (status !== "running") run.lastNonRunningSentSeq = run.sentSeq;
    return run.sentSeq;
  }

  /** Issue #1830: the server's ACK of send `seq` carried `status` (undefined when unreadable). A
   *  `running` ACK is remembered by its send number; whether it still counts is decided by
   *  {@link parkable}, so a late ACK of a send older than the last non-`running` one never does. */
  statusAcked(runId: string, seq: number, status: string | undefined): void {
    const run = this.runs.get(runId);
    if (!run) return;
    // Log-only: a late ACK of an older send must not overwrite a newer send's.
    if (seq >= run.ackedStatusSeq) {
      run.ackedStatus = status;
      run.ackedStatusSeq = seq;
    }
    if (status !== "running") {
      // Declined or unreadable (a 409 carries the run's real status; an already-terminal
      // run has none): it cancels every earlier `running` ACK.
      run.lastNonRunningAckedSeq = Math.max(run.lastNonRunningAckedSeq, seq);
      return;
    }
    if (seq <= run.runningAckedSeq) return;
    run.runningAckedSeq = seq;
    // Newly parkable: an in-place skip from the un-ACKed window must not outlive it.
    if (this.parkable(run)) run.inPlaceSkipped = false;
  }

  /** A stop can become a counted park only from the server status `running`: the executor is in
   *  flight, the latest send says `running`, and the server ACKed `running` for a send newer than
   *  any non-`running` send and any declined or unreadable ACK. A same-status `running` send still
   *  in flight does not unmake that. */
  private parkable(run: RunState): boolean {
    return (
      run.inExecutor &&
      run.sentStatus === "running" &&
      run.runningAckedSeq > Math.max(run.lastNonRunningSentSeq, run.lastNonRunningAckedSeq)
    );
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
   * The run left its executor (it returned or threw): it can no longer take a stop, only an
   * in-place reclaim (finalize). `diskParked` says it left for a disk park, whose flight end ({@link unregister})
   * releases the stop as before; otherwise a stop asked of it will never become a park (it
   * finished, failed, or parked for another reason first), so it is released now.
   */
  leftLoop(runId: string, diskParked: boolean): void {
    const run = this.runs.get(runId);
    if (run) {
      run.inExecutor = false;
      run.inPlaceSkipped = false;
    }
    if (this.stopping !== runId || diskParked) return;
    this.opts.log.warn("the run the hard layer stopped left its executor without a disk park; releasing the stop", {
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
   * The SOFT layer at one implement turn boundary. Measures its caches, and when they are over the cap: at a proven quiet point
   * (`processAlive` resolves false) trims them, otherwise counts the boundary as blocked.
   * Resolves "park" when the run stays over the cap by {@link SOFT_PARK_RULE}, else "continue".
   * Never throws: a failed measurement or trim continues the run.
   */
  async boundary(runId: string, processAlive: ProcessAliveProbe): Promise<"continue" | "park"> {
    const run = this.runs.get(runId);
    if (!run) return "continue";
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
   * measures every registered run and acts on the one with the largest caches (a parkable run is
   * stopped, any other has its caches dropped in place; see the header). One stop at a time: the
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
    if (sampledAtMs < Math.max(this.lastStopEndedAt, this.lastReclaimEndedAt)) return;
    const threshold = hardStopThreshold(this.opts.thresholdOf(), this.opts.config.hardMargin);
    if (usedFraction < threshold) {
      this.warnedNothingToStop = false;
      this.lastSelectionAt = Number.NEGATIVE_INFINITY;
      for (const r of this.runs.values()) r.inPlaceSkipped = false;
      return;
    }
    if (this.stopping !== undefined || this.selecting || this.reclaiming !== undefined) return;
    if (this.now() - this.lastSelectionAt < SELECT_INTERVAL_MS) return;
    const candidates = [...this.runs.entries()].filter(([, r]) => !r.stopRequested && !r.inPlaceSkipped);
    if (candidates.length === 0) {
      // A registered run that is stopping or was already reclaimed this stretch is not "nothing".
      if (this.runs.size === 0 && !this.warnedNothingToStop) {
        this.warnedNothingToStop = true;
        this.opts.log.warn("data volume at the hard threshold, but no running Claude run is registered to stop or reclaim", {
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
   *  reading) and act on the one with the most cache bytes, when any has some: stop it when it is
   *  parkable, else drop its caches in place. */
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
      this.opts.log.warn("data volume at the hard threshold, but no running Claude run holds cache bytes to act on", {
        used_fraction: usedFraction,
        hard_threshold: threshold,
      });
      return;
    }
    const [runId, run] = pick;
    // It may have ended while it was being measured.
    if (this.runs.get(runId) !== run || run.stopRequested) {
      this.lastSelectionAt = this.now();
      return;
    }
    // Decided AFTER the measurement, from the run's state now (it may have changed phase meanwhile).
    if (!this.parkable(run)) {
      await this.reclaimInPlace(runId, run, usedFraction, threshold);
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

  /**
   * Issue #1830: drop a non-parkable run's caches where it stands, under the {@link reclaiming}
   * slot. The run keeps its gate and its flight and nothing is killed. A throw is logged. The
   * run is re-measured afterwards: one still holding bytes (kept subtrees, a failed or skipped
   * removal, an unreadable measurement) is skipped for the rest of the stretch.
   */
  private async reclaimInPlace(runId: string, run: RunState, usedFraction: number, threshold: number): Promise<void> {
    this.reclaiming = runId;
    this.opts.log.warn("data volume at the hard threshold; the run with the largest caches cannot be parked, dropping its caches in place", {
      run_id: runId,
      cache_bytes: run.cacheBytes,
      used_fraction: usedFraction,
      hard_threshold: threshold,
      status: run.sentStatus,
      acked_status: run.ackedStatus,
      in_executor: run.inExecutor,
      in_place: true,
    });
    let threw = false;
    try {
      await run.reclaimInPlace();
    } catch (e) {
      threw = true;
      this.opts.log.warn("in-place cache drop failed", { run_id: runId, error: errMessage(e) });
    }
    try {
      const left = threw ? undefined : await this.measureRun(run);
      if (left === undefined || left > 0) run.inPlaceSkipped = true;
    } finally {
      this.reclaiming = undefined;
      this.lastReclaimEndedAt = this.now();
    }
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
