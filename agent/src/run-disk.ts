import type { Stats } from "node:fs";
import fs from "node:fs/promises";
import path from "node:path";
import type { Logger } from "./log.js";
import { RUN_DISK_MAX_ENTRIES, type RunDiskEntry } from "./protocol.js";
import { measureRunHome, type MeasureOptions, type RunHomeBytes } from "./rmtree.js";
import { DEFAULT_RECLAIM_MAX_CONSECUTIVE_FAILURES, TERMINAL_RUN_STATUSES, type RunStatusLookup } from "./home-reclaim.js";
import { errMessage, RUN_ID_RE } from "./util.js";

/**
 * PRD #1809 D8: the per-run HOME size the heartbeat reports (`stats.run_disk`).
 *
 * Measuring a HOME walks it, which can take seconds to minutes for a run with tens of GB of
 * build cache, so it never runs on the heartbeat path. The heartbeat calls {@link
 * RunDiskSampler.current}, which returns the latest FINISHED sample and, when the interval has
 * passed and no sample is running, starts the next one in the background (not awaited). A sample
 * that fails or is slow therefore costs the heartbeat nothing: it keeps sending the previous
 * reading.
 *
 * Which runs: the entries under the run HOME root (`<dataDir>/agent-home`) that are named like a
 * run id, are a real directory (an `lstat`, never followed) and are owned by the worker's uid, the
 * same worker-owned filter as the D7 reclaim (disk-reclaim.ts `isWorkerOwnedDir`): the root is
 * writable by the agent uids, which cannot create an entry owned by the worker, so a planted name
 * is never measured. Of those, a run the runner executes now is measured; any other is measured
 * only when the api reports it non-terminal (parked, or waiting on a gate). A terminal run's HOME
 * is left to the reclaims, and a run the api does not know (404) or could not be asked about is
 * skipped.
 *
 * Bounds: the root listing reads at most `maxDirReads` dirents, at most `maxMeasured` runs are
 * measured per sample, each measure gets its own deadline and entry budget ({@link
 * measureRunHome}), and the whole sample stops at `sampleDeadlineMs`. The result keeps the
 * `maxEntries` largest HOMEs, largest first.
 */

/** Dirents the root listing may read per sample. */
const DEFAULT_MAX_DIR_READS = 5_000;
/** Runs measured per sample. More HOMEs than this on one worker is a reclaim problem, not a
 *  reporting one; the listing order decides which are measured. */
const DEFAULT_MAX_MEASURED = 200;
/** Wall time one HOME's measure may take. */
const DEFAULT_PER_RUN_DEADLINE_MS = 60_000;
/** Wall time one whole sample may take. */
const DEFAULT_SAMPLE_DEADLINE_MS = 4 * 60_000;

type DirStat = Pick<Stats, "uid" | "isDirectory">;

export interface RunDiskSamplerOptions {
  /** The run HOME root (`<dataDir>/agent-home`). */
  homeRoot: string;
  /** Minimum time between sample starts. Zero or less turns the sampler off. */
  intervalMs: number;
  /** Whether the runner is executing the run in this process (RunRunner.isExecuting). */
  isRunLive: (runId: string) => boolean;
  /** The api status lookup, with the startup sweep's contract (404 → undefined, else throw). */
  statusOf: RunStatusLookup;
  log: Logger;
  /** Seams, injected by tests. */
  measure?: (home: string, opts: MeasureOptions) => Promise<RunHomeBytes>;
  lstat?: (p: string) => Promise<DirStat>;
  /** The uid that owns every directory the sampler may measure (default `process.getuid()`). */
  workerUid?: number;
  now?: () => number;
  maxDirReads?: number;
  maxMeasured?: number;
  maxEntries?: number;
  perRunDeadlineMs?: number;
  sampleDeadlineMs?: number;
}

export class RunDiskSampler {
  private readonly opts: RunDiskSamplerOptions;
  private readonly now: () => number;
  private latest: RunDiskEntry[] | undefined;
  private inFlight: Promise<void> | undefined;
  private lastStartedAt: number | undefined;

  constructor(opts: RunDiskSamplerOptions) {
    this.opts = opts;
    this.now = opts.now ?? Date.now;
  }

  /**
   * The latest finished sample (undefined before the first one finishes, or when the sampler is
   * off), for the heartbeat. Starts the next sample in the background when one is due. Never
   * awaits and never throws.
   */
  current(): RunDiskEntry[] | undefined {
    this.maybeStart();
    return this.latest;
  }

  /** The sample in flight, if any (tests await it; production never does). */
  settled(): Promise<void> {
    return this.inFlight ?? Promise.resolve();
  }

  private maybeStart(): void {
    if (this.opts.intervalMs <= 0 || this.inFlight) return;
    const now = this.now();
    if (this.lastStartedAt !== undefined && now - this.lastStartedAt < this.opts.intervalMs) return;
    this.lastStartedAt = now;
    this.inFlight = this.sample()
      .then((entries) => {
        this.latest = entries;
      })
      .catch((err: unknown) => {
        // The previous reading stays: a failed sample must not blank the report.
        this.opts.log.warn("run disk sample failed", { error: errMessage(err) });
      })
      .finally(() => {
        this.inFlight = undefined;
      });
  }

  /** One sample: list, filter, measure, rank. Rejects only on an unexpected error. */
  async sample(): Promise<RunDiskEntry[]> {
    const o = this.opts;
    const measure = o.measure ?? measureRunHome;
    const lstat = o.lstat ?? ((p: string) => fs.lstat(p));
    const workerUid = "workerUid" in o ? o.workerUid : process.getuid?.();
    const maxDirReads = o.maxDirReads ?? DEFAULT_MAX_DIR_READS;
    const maxMeasured = o.maxMeasured ?? DEFAULT_MAX_MEASURED;
    const maxEntries = o.maxEntries ?? RUN_DISK_MAX_ENTRIES;
    const perRunMs = o.perRunDeadlineMs ?? DEFAULT_PER_RUN_DEADLINE_MS;
    const sampleDeadline = this.now() + (o.sampleDeadlineMs ?? DEFAULT_SAMPLE_DEADLINE_MS);

    const names = await this.listRunDirs(maxDirReads);
    const entries: RunDiskEntry[] = [];
    let measured = 0;
    let statusFailures = 0;
    for (const runId of names) {
      if (measured >= maxMeasured || this.now() >= sampleDeadline) break;
      const home = path.join(o.homeRoot, runId);
      let st: DirStat;
      try {
        st = await lstat(home);
      } catch {
        continue; // gone since the listing
      }
      if (!st.isDirectory() || (workerUid !== undefined && st.uid !== workerUid)) continue;
      if (!o.isRunLive(runId)) {
        // Past the consecutive-failure bail the api is treated as unreachable for this sample:
        // only live runs are measured.
        if (statusFailures >= DEFAULT_RECLAIM_MAX_CONSECUTIVE_FAILURES) continue;
        let status: string | undefined;
        try {
          status = await o.statusOf(runId);
          statusFailures = 0;
        } catch {
          statusFailures += 1;
          continue;
        }
        if (status === undefined || TERMINAL_RUN_STATUSES.has(status)) continue;
      }
      measured += 1;
      try {
        const r = await measure(home, { deadline: Math.min(sampleDeadline, this.now() + perRunMs) });
        entries.push({
          run_id: runId,
          home_bytes: r.homeBytes,
          cache_bytes: r.cacheBytes,
          ...(r.truncated ? { truncated: true } : {}),
        });
      } catch (err) {
        o.log.warn("run disk sample could not measure a run HOME", { run_id: runId, error: errMessage(err) });
      }
    }
    entries.sort((a, b) => b.home_bytes - a.home_bytes || (a.run_id < b.run_id ? -1 : 1));
    return entries.slice(0, maxEntries);
  }

  /** The run-id-named directory entries of the root, reading at most `maxReads` dirents. The
   *  dirent type comes from the directory entry, so a symlink is never a directory here. */
  private async listRunDirs(maxReads: number): Promise<string[]> {
    let dir;
    try {
      dir = await fs.opendir(this.opts.homeRoot);
    } catch (err) {
      if ((err as NodeJS.ErrnoException).code === "ENOENT") return [];
      throw err;
    }
    const out: string[] = [];
    try {
      for (let read = 0; read < maxReads; read++) {
        const e = await dir.read();
        if (e === null) break;
        if (e.isDirectory() && RUN_ID_RE.test(e.name)) out.push(e.name);
      }
    } finally {
      await dir.close().catch(() => undefined);
    }
    return out;
  }
}
