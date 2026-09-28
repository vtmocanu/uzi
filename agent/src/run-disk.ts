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
 *
 * Partial samples: a sample that stopped at its measure budget or deadline, could not ask the api
 * about a run, or could not measure a HOME did not reach every run. It does not replace the last
 * reading wholesale: an entry of the previous reading whose run this sample did not reach is kept
 * (with its own `sampled_at`, so the api ages it by when it was measured), as long as the run's
 * HOME is still a worker-owned directory. A run the sample did reach is decided by this sample
 * (measured, or dropped as terminal, unknown or gone). Each entry's `sampled_at` is when the
 * sample that measured it finished.
 *
 * Rotation: the run directories are visited in name order starting just after where the previous
 * sample stopped early, so a worker with more HOMEs than one sample can measure still measures the
 * trailing ones on the next sample instead of starving them.
 */

/** Dirents the root listing may read per sample. */
const DEFAULT_MAX_DIR_READS = 5_000;
/** Runs measured per sample. More HOMEs than this on one worker is a reclaim problem, not a
 *  reporting one; the next sample starts after the last run this one visited (rotation). */
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
  /** The run id the last sample stopped after when it ended early (budget or deadline); the next
   *  sample starts just after it. Undefined after a sample that visited every listed run. */
  private resumeAfter: string | undefined;

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

  /**
   * One sample: list, filter, measure, rank, merged with the previous reading for the runs this
   * sample did not reach (see the file comment). Rejects only on an unexpected error.
   */
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
    const workerOwnedDir = (st: DirStat): boolean =>
      st.isDirectory() && (workerUid === undefined || st.uid === workerUid);

    const { names, truncated: listingTruncated } = await this.listRunDirs(maxDirReads);
    // Name order, starting just after where the previous sample stopped early (rotation).
    names.sort();
    const startAt = this.resumeAfter === undefined ? 0 : names.findIndex((n) => n > this.resumeAfter!);
    const ordered = startAt <= 0 ? names : [...names.slice(startAt), ...names.slice(0, startAt)];

    const fresh: Omit<RunDiskEntry, "sampled_at">[] = [];
    // Runs this sample decided (measured, or dropped as gone/foreign/terminal/unknown).
    const reached = new Set<string>();
    let measured = 0;
    let statusFailures = 0;
    let lookupFailures = 0;
    let measureFailures = 0;
    let stoppedEarly: "budget" | "deadline" | undefined;
    let lastVisited: string | undefined;
    for (const runId of ordered) {
      if (measured >= maxMeasured) {
        stoppedEarly = "budget";
        break;
      }
      if (this.now() >= sampleDeadline) {
        stoppedEarly = "deadline";
        break;
      }
      lastVisited = runId;
      const home = path.join(o.homeRoot, runId);
      let st: DirStat;
      try {
        st = await lstat(home);
      } catch {
        reached.add(runId); // gone since the listing
        continue;
      }
      if (!workerOwnedDir(st)) {
        reached.add(runId);
        continue;
      }
      if (!o.isRunLive(runId)) {
        // Past the consecutive-failure bail the api is treated as unreachable for this sample:
        // only live runs are measured. A run not asked about is not reached.
        if (statusFailures >= DEFAULT_RECLAIM_MAX_CONSECUTIVE_FAILURES) {
          lookupFailures += 1;
          continue;
        }
        let status: string | undefined;
        try {
          status = await o.statusOf(runId);
          statusFailures = 0;
        } catch {
          statusFailures += 1;
          lookupFailures += 1;
          continue;
        }
        if (status === undefined || TERMINAL_RUN_STATUSES.has(status)) {
          reached.add(runId);
          continue;
        }
      }
      measured += 1;
      try {
        const r = await measure(home, { deadline: Math.min(sampleDeadline, this.now() + perRunMs) });
        fresh.push({
          run_id: runId,
          home_bytes: r.homeBytes,
          cache_bytes: r.cacheBytes,
          ...(r.truncated ? { truncated: true } : {}),
        });
        reached.add(runId);
      } catch (err) {
        measureFailures += 1;
        o.log.warn("run disk sample could not measure a run HOME", { run_id: runId, error: errMessage(err) });
      }
    }
    this.resumeAfter = stoppedEarly ? lastVisited : undefined;

    // Every fresh entry carries the moment this sample finished measuring.
    const sampledAt = new Date(this.now()).toISOString();
    const entries: RunDiskEntry[] = fresh.map((e) => ({ ...e, sampled_at: sampledAt }));

    // Keep the previous reading for the runs this sample did not reach, while their HOME is still a
    // worker-owned directory. A run that is gone from the listing of a complete sample fails that
    // check and drops out.
    let carried = 0;
    for (const prev of this.latest ?? []) {
      if (reached.has(prev.run_id)) continue;
      let st: DirStat;
      try {
        st = await lstat(path.join(o.homeRoot, prev.run_id));
      } catch {
        continue;
      }
      if (!workerOwnedDir(st)) continue;
      entries.push(prev);
      carried += 1;
    }

    const partial = stoppedEarly !== undefined || listingTruncated || lookupFailures > 0 || measureFailures > 0;
    if (partial) {
      o.log.info("run disk sample partial; kept the previous reading for the runs it did not reach", {
        partial: true,
        ...(stoppedEarly ? { stopped: stoppedEarly } : {}),
        ...(listingTruncated ? { listing_truncated: true } : {}),
        status_lookup_failures: lookupFailures,
        measure_failures: measureFailures,
        measured: fresh.length,
        carried,
      });
    }

    entries.sort((a, b) => b.home_bytes - a.home_bytes || (a.run_id < b.run_id ? -1 : 1));
    return entries.slice(0, maxEntries);
  }

  /** The run-id-named directory entries of the root, reading at most `maxReads` dirents
   *  (`truncated` when the listing stopped there). The dirent type comes from the directory entry,
   *  so a symlink is never a directory here. */
  private async listRunDirs(maxReads: number): Promise<{ names: string[]; truncated: boolean }> {
    let dir;
    try {
      dir = await fs.opendir(this.opts.homeRoot);
    } catch (err) {
      if ((err as NodeJS.ErrnoException).code === "ENOENT") return { names: [], truncated: false };
      throw err;
    }
    const names: string[] = [];
    let truncated = true;
    try {
      for (let read = 0; read < maxReads; read++) {
        const e = await dir.read();
        if (e === null) {
          truncated = false;
          break;
        }
        if (e.isDirectory() && RUN_ID_RE.test(e.name)) names.push(e.name);
      }
    } finally {
      await dir.close().catch(() => undefined);
    }
    return { names, truncated };
  }
}
