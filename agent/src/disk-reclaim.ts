import type { Stats } from "node:fs";
import fs from "node:fs/promises";
import path from "node:path";
import type { Logger } from "./log.js";
import { rmTreePinned } from "./rmtree.js";
import { dropRunCaches, isNoop, type DropRunCachesOptions, type RunCacheDropResult } from "./run-caches.js";
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
 *      proven over; see {@link modelPassMinAgeMs} for the proof.
 *
 * **Every deletion is a pinned-descriptor walk** ({@link rmTreePinned}, and `dropRunCaches`'
 * own pinned subtree walk), never a path-based `fs.rm`: this pass runs while other runs'
 * agent processes are live on the same volume as the same uids, and a path-based walk can
 * be redirected outside the tree by an intermediate directory swapped for a symlink.
 *
 * **Only the worker's own directories.** Both roots are writable by the runner uids, so a
 * run can plant names there. A listed entry is examined only if it is a real directory owned
 * by the worker (see `isWorkerOwnedDir` in {@link runDiskReclaimPass}); anything else is
 * counted and skipped before it can take a status lookup or a slot in the run budget, and
 * each root's listing rotates across passes (DEFAULT_DIR_CURSORS, which also says where that
 * rotation stops guaranteeing anything), holding a window until its run budget has rotated
 * over every worker-owned run in it, so neither a flood of names smaller than the read budget
 * nor more never-removed runs than the run budget can hide a real entry for good.
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
 * The margin {@link modelPassMinAgeMs} adds to the longest model-pass timeout: the abort
 * grace (500 ms) and the pass's own HOME cleanup (`rmHomeTree`: up to three 120 s helper
 * passes plus two worker passes), with room to spare.
 */
const MODEL_PASS_AGE_MARGIN_MS = 15 * 60_000;

/**
 * How old (by mtime) a model-pass HOME that no pass in THIS process owns must be before the
 * reclaim removes it: the longest configured model-pass wall-clock cap (judge, review,
 * summary; `timeoutsMs`) plus {@link MODEL_PASS_AGE_MARGIN_MS}. main.ts derives it from the
 * configured timeouts, so raising one (e.g. SUMMARY_MODEL_TIMEOUT_MS) raises this with it.
 *
 * The proof that the owner of an old unregistered HOME is stopped:
 *
 *  - A pass in this process registers its HOME right after `mkdtemp` and unregisters it
 *    only after its own cleanup ran (model-pass.ts `isLiveModelPassHome`), so a HOME a live
 *    pass here owns is registered and never removed, whatever its age. The age bound covers
 *    the moment between `mkdtemp` creating the dir and the registration.
 *  - An unregistered HOME therefore belongs to a pass that already finished here, or to a
 *    previous life of this worker. No other worker process shares this data volume: it is
 *    one worker process per data dir (run-disk-locks.ts relies on the same). A pass holds
 *    its HOME for at most its wall-clock cap plus the abort grace and its own cleanup,
 *    after which it never writes there again, and a HOME's mtime is never earlier than its
 *    creation, so an mtime older than this bound means its pass has ended.
 */
export function modelPassMinAgeMs(timeoutsMs: readonly number[]): number {
  return Math.max(0, ...timeoutsMs) + MODEL_PASS_AGE_MARGIN_MS;
}

/** The bound with the built-in timeouts (judge and review 5 min, summary 60 s), for tests. */
const DEFAULT_MODEL_PASS_MIN_AGE_MS = modelPassMinAgeMs([5 * 60_000, 5 * 60_000, 60_000]);

/**
 * The deadline of one reclaim-side cache drop. Well under the api's ClaimGrace (5 min): a
 * resume of the run waits on its lock for the whole drop (run-disk-locks.ts), and the
 * park's own drop keeps its five minutes.
 */
const RECLAIM_DROP_DEADLINE_MS = 60_000;

/** Wall-clock ceiling for one pass. Each cache drop and tree removal is clipped to it. */
const DEFAULT_PASS_DEADLINE_MS = 10 * 60_000;

/**
 * Cap on run directories examined per pass (each costs one status lookup). Only a run with a
 * worker-owned directory counts toward it (see {@link isWorkerOwnedDir}).
 */
const DEFAULT_PASS_MAX_ENTRIES = 500;

/** What the ownership check reads of a listed directory (an `lstat`, never a `stat`). */
type DirStat = Pick<Stats, "uid" | "mtimeMs" | "isDirectory">;

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
  /**
   * The runs whose caches a pass found all gone, shared with the runner (see
   * {@link CachesDroppedMemo}); absent, nothing is remembered.
   */
  cachesDropped?: CachesDroppedMemo;
  /** Seams, injected by tests. */
  removeTree?: (parent: string, name: string, deadline: number) => Promise<unknown>;
  dropCaches?: (home: string, log: Logger, opts: DropRunCachesOptions) => Promise<RunCacheDropResult>;
  isModelPassLive?: (dir: string) => boolean;
  now?: () => number;
  modelPassMinAgeMs?: number;
  maxEntries?: number;
  /** Dirents one pass may keep from each root (default {@link DEFAULT_PASS_MAX_DIR_READS}). */
  maxDirReads?: number;
  /**
   * Where each root's listing resumes next pass (see {@link DEFAULT_DIR_CURSORS}), keyed by
   * root path. Absent, the module's own map, which outlives a pass.
   */
  dirCursors?: Map<string, number>;
  /**
   * The run rotation (see "Run budget" in {@link runDiskReclaimPass}), keyed by HOME root.
   * Absent, the module's own map, which outlives a pass.
   */
  runCursors?: Map<string, RunRotation>;
  /**
   * The uid that owns every directory the reclaim may examine: the worker's own
   * (`process.getuid()`, the default). Absent and without `process.getuid`, nothing is
   * filtered by owner.
   */
  workerUid?: number;
  /** `fs.lstat`, injected by tests to present a directory as owned by another uid. */
  lstat?: (p: string) => Promise<DirStat>;
  maxConsecutiveFailures?: number;
  deadlineMs?: number;
}

/** What one pass did. Every examined run lands in exactly one of the run buckets. */
export interface DiskReclaimSummary {
  runsExamined: number;
  terminalHomesRemoved: number;
  provisionDirsRemoved: number;
  /**
   * An admitted run's provision dir the worker does not own (a legacy pre-split dir the
   * entrypoint re-owned to `runner`, or a planted one): never removed, see the pass.
   */
  provisionDirsNotOwned: number;
  /** A parked run whose drop freed at least one cache subtree. */
  cachesDropped: number;
  /** A parked run whose caches were already gone (remembered from an earlier pass, or a no-op drop). */
  cachesAlreadyClear: number;
  skippedLive: number;
  /**
   * A listed run whose directories are none of them a real directory owned by the worker:
   * skipped before its status is asked for, and not counted in {@link runsExamined}.
   */
  skippedNotOwned: number;
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
  /** A model-pass-named entry that is not a real directory owned by the worker: never touched. */
  modelPassHomesNotOwned: number;
  /** Dirents read from the two roots, kept or not (a resumed listing's skipped prefix included). */
  dirEntriesRead: number;
  /**
   * "budget": the run cap, or a root's listing hit its read cap or read budget (then only
   * what was read is examined). "deadline": the pass deadline, in a listing or after it.
   */
  stoppedEarly?: "budget" | "api_unreachable" | "deadline";
}

/**
 * The runs whose caches a reclaim pass found all gone (dropped or absent), so later passes
 * skip them instead of walking their HOME again. Owned by main.ts so it outlives one pass,
 * and shared with the runner: a run that executes again may rebuild its caches, so its
 * entry is forgotten
 *
 *  - when the runner starts executing it ({@link forget}, called while the runner holds the
 *    run's {@link RunDiskLocks} lock, so it is ordered after any drop that was in progress
 *    and whose {@link add} ran under the same lock). A run that resumes here, rebuilds its
 *    caches and parks again entirely between two passes is still dropped by the next one;
 *  - when a pass sees the run executing, or reads a status other than a process-ended park;
 *  - when a pass's complete listing no longer has the run's HOME.
 */
export class CachesDroppedMemo {
  private readonly ids = new Set<string>();

  has(runId: string): boolean {
    return this.ids.has(runId);
  }

  add(runId: string): void {
    this.ids.add(runId);
  }

  forget(runId: string): void {
    this.ids.delete(runId);
  }

  /** Forget every run `keep` rejects. */
  retainOnly(keep: (runId: string) => boolean): void {
    for (const id of this.ids) if (!keep(id)) this.ids.delete(id);
  }
}

interface RunDirs {
  home?: string;
  provision?: string;
}

/**
 * Cap on the directory entries one pass keeps from each root (`agent-home`, `provision`),
 * charged for EVERY dirent in the pass's window, kept or not. Both roots are writable by live
 * runs' agent uids, so a run can plant names there: an audit planted 300k and the whole
 * listing cost the worker ~120 MB, ~3M would OOM-kill it. Streamed reads charged here bound
 * what a pass holds to this many names per root, far above any real fleet's run and
 * model-pass HOMEs. A listing that resumes past a skipped prefix (below) reads that prefix
 * and discards it, holding none of it; the prefix is charged to {@link PASS_READ_BUDGET_FACTOR}.
 */
const DEFAULT_PASS_MAX_DIR_READS = 50_000;

/**
 * Each root's listing reads at most this many times its keep cap in one pass, the skipped
 * prefix of a resumed listing included, so no pass reads a flooded root to its end.
 */
const PASS_READ_BUDGET_FACTOR = 4;

/**
 * Where each root's next listing resumes: the number of leading dirents to read past and
 * discard before the pass's window starts. One worker process owns a data dir
 * (run-disk-locks.ts), so a module-level map outlives each pass exactly as long as it must.
 *
 * **Why it rotates.** Without it a capped listing reads the same leading window every pass,
 * so more than a cap's worth of planted names ahead of a real HOME (in readdir order) hide
 * it from every pass. With it, a pass that stops at the cap resumes the next pass where it
 * stopped, and a listing that reaches the directory's end, or whose window ends at the
 * root's read budget (cap x {@link PASS_READ_BUDGET_FACTOR}), starts the next pass from 0.
 * A pass that stops at its run budget before examining every worker-owned run of its window
 * holds the window instead (the run rotation, see the end of {@link runDiskReclaimPass}), so
 * a window of R worker-owned runs is held for at most ceil(R / maxEntries) passes.
 *
 * **What it guarantees, and what it does not.** A root whose readdir order does not change
 * between passes except by this reclaim's own removals (which the cursor subtracts), and
 * whose size N is within the read budget, is read whole across at most ceil(N / cap) + 1
 * windows, so at most (ceil(N / cap) + 1) x ceil(R / maxEntries) consecutive passes, R being
 * the most worker-owned runs one window holds (one pass per window when R <= maxEntries).
 * Beyond that it guarantees nothing:
 *  - entries past the read budget in readdir order are not reached while the root stays
 *    that large (a flood that big hides them; the budget is what keeps a pass bounded);
 *  - a live writer that inserts OR deletes names adaptively around the cursor between two
 *    passes shifts the positions the cursor counts, and can keep an entry out of every
 *    window;
 *  - a worker restart starts every cursor at 0 again.
 */
const DEFAULT_DIR_CURSORS = new Map<string, number>();

/**
 * A run-budget rotation over one held listing window: the first run id it examined and the
 * last one a pass examined before stopping at the run budget. The runs it has covered are the
 * ids from `start` to `last` in cyclic id order.
 */
interface RunRotation {
  start: string;
  last: string;
  /**
   * How many dirents each root's held window keeps next pass, when its listing stopped at a
   * cap: this pass's window less the entries it removed from it, so the window still ends at
   * the same entry and nothing slides into it from past its end. Undefined for a listing that
   * reached the directory's end, which has nothing past it to slide in.
   */
  keep: { home?: number; provision?: number };
}

/**
 * The run rotation in progress, keyed by HOME root (see {@link DiskReclaimDeps.runCursors}).
 * Module-level for the reason DEFAULT_DIR_CURSORS is.
 */
const DEFAULT_RUN_CURSORS = new Map<string, RunRotation>();

interface DirListing {
  lists: Array<Map<string, string>>;
  /** Dirents read, the skipped prefix included. */
  read: number;
  /** Dirents read past the skipped prefix: the pass's window. */
  kept: number;
  /** The listing stopped at its keep cap or its read budget. */
  capped: boolean;
  /** The listing stopped at the pass deadline. */
  deadline: boolean;
  /** Where the next pass's listing of this root starts, before this pass's own removals. */
  next: number;
  /** The whole directory was read from its first entry (absent counts; a cap, a skipped prefix or an error does not). */
  whole: boolean;
}

/**
 * Stream `root`'s entries (never a whole listing): read past the first `skip` of them, then
 * keep at most `maxReads` further dirents, sorting the directories among them into one map
 * per filter (first match wins). Every dirent, skipped or kept, is charged to `budget`, and
 * the `deadline` (on `now`) is checked before each read. Empty when `root` is absent.
 *
 * A skip that exhausts the budget stops the listing as capped with the cursor back at 0; a
 * window that ends at the budget also wraps the next pass to 0. A deadline stop keeps the
 * position reached (the skip itself when it was still skipping).
 */
async function listDirs(
  root: string,
  filters: ReadonlyArray<(name: string) => boolean>,
  skip: number,
  maxReads: number,
  budget: number,
  deadline: number,
  now: () => number,
  log: Logger,
): Promise<DirListing> {
  const lists = filters.map(() => new Map<string, string>());
  let skipped = 0;
  let read = 0;
  let capped = false;
  let pastDeadline = false;
  let atEnd = false;
  let next = skip;
  let dir;
  try {
    dir = await fs.opendir(root);
  } catch (err) {
    const absent = (err as NodeJS.ErrnoException).code === "ENOENT";
    if (!absent) log.warn("disk reclaim could not read a directory", { dir: root, error: errMessage(err) });
    // An unreadable root keeps its cursor: the next pass retries the same window.
    return { lists, read: 0, kept: 0, capped, deadline: false, next: absent ? 0 : skip, whole: absent };
  }
  try {
    while (skipped < skip) {
      if (skipped >= budget) {
        capped = true;
        next = 0; // the resume point is past what one pass may read: start over
        break;
      }
      if (now() >= deadline) {
        pastDeadline = true;
        break; // next stays at skip: retry the same window
      }
      if ((await dir.read()) === null) {
        atEnd = true;
        break;
      }
      skipped += 1;
    }
    const keep = Math.min(maxReads, budget - skipped);
    while (!atEnd && !capped && !pastDeadline) {
      if (read >= keep) {
        capped = true;
        break;
      }
      if (now() >= deadline) {
        pastDeadline = true;
        break;
      }
      const e = await dir.read();
      if (e === null) {
        atEnd = true;
        break;
      }
      read += 1;
      // Dirent types come from the directory entry, never a stat that follows it: a symlink
      // is never a directory here and is never followed out of the data volume.
      if (!e.isDirectory()) continue;
      const i = filters.findIndex((keepName) => keepName(e.name));
      if (i >= 0) lists[i]?.set(e.name, path.join(root, e.name));
    }
    if (atEnd) next = 0;
    else if (capped || read > 0) next = skipped + read >= budget ? 0 : skipped + read;
  } catch (err) {
    log.warn("disk reclaim could not read a directory", { dir: root, error: errMessage(err) });
    await dir.close().catch(() => undefined);
    return { lists, read: skipped + read, kept: read, capped: false, deadline: false, next: skip, whole: false };
  }
  await dir.close().catch(() => undefined);
  if (capped) log.warn("disk reclaim stopped listing a directory at its read cap", { dir: root, max_reads: maxReads, budget, skipped });
  return { lists, read: skipped + read, kept: read, capped, deadline: pastDeadline, next, whole: atEnd && skip === 0 };
}

/**
 * One reclaim pass. Best-effort in every direction: it never throws, and every failure is
 * counted and logged in the summary line.
 */
export async function runDiskReclaimPass(deps: DiskReclaimDeps): Promise<DiskReclaimSummary> {
  const removeTree = deps.removeTree ?? ((parent: string, name: string, deadline: number) => rmTreePinned(parent, name, { deadline }));
  const dropCaches = deps.dropCaches ?? dropRunCaches;
  const memo = deps.cachesDropped;
  const isModelPassLive = deps.isModelPassLive ?? isLiveModelPassHome;
  const now = deps.now ?? Date.now;
  const modelPassMinAgeMs = deps.modelPassMinAgeMs ?? DEFAULT_MODEL_PASS_MIN_AGE_MS;
  const maxEntries = deps.maxEntries ?? DEFAULT_PASS_MAX_ENTRIES;
  const maxDirReads = deps.maxDirReads ?? DEFAULT_PASS_MAX_DIR_READS;
  const cursors = deps.dirCursors ?? DEFAULT_DIR_CURSORS;
  const runCursors = deps.runCursors ?? DEFAULT_RUN_CURSORS;
  const workerUid = "workerUid" in deps ? deps.workerUid : process.getuid?.();
  const lstat = deps.lstat ?? ((p: string) => fs.lstat(p));
  const maxConsecutiveFailures = deps.maxConsecutiveFailures ?? DEFAULT_RECLAIM_MAX_CONSECUTIVE_FAILURES;
  const deadlineMs = deps.deadlineMs ?? DEFAULT_PASS_DEADLINE_MS;
  const log = deps.log;
  const startedAt = now();
  const summary: DiskReclaimSummary = {
    runsExamined: 0,
    terminalHomesRemoved: 0,
    provisionDirsRemoved: 0,
    provisionDirsNotOwned: 0,
    cachesDropped: 0,
    cachesAlreadyClear: 0,
    skippedLive: 0,
    skippedNotOwned: 0,
    skippedStatusUnknown: 0,
    skippedNotEligible: 0,
    failed: 0,
    unexamined: 0,
    modelPassHomesRemoved: 0,
    modelPassHomesLive: 0,
    modelPassHomesTooRecent: 0,
    modelPassHomesFailed: 0,
    modelPassHomesNotOwned: 0,
    dirEntriesRead: 0,
  };

  /**
   * Whether `dir` is a real directory (never a symlink: `lstat`) owned by the worker. Every
   * directory this pass may act on is one the worker created: a run HOME (sdk-executor.ts
   * `fs.mkdir`, codex-executor.ts ensureCodexSharedDirectory, which also asserts the worker
   * owner), a provision dir (provision.ts `fs.mkdir`) and a model-pass HOME (model-pass.ts
   * `fs.mkdtemp`), all in this process; the entrypoint's legacy migration keeps every run
   * HOME root worker-owned. Both roots are writable by the runner uids (3775, sticky), which
   * cannot create an entry owned by the worker, so a planted name costs this one `lstat`,
   * never a status lookup or a slot in the run budget.
   */
  const isWorkerOwnedDir = async (dir: string): Promise<boolean> => {
    let st: DirStat;
    try {
      st = await lstat(dir);
    } catch {
      return false; // gone already
    }
    return st.isDirectory() && (workerUid === undefined || st.uid === workerUid);
  };
  const removedFrom = new Map<string, number>();
  const noteRemoved = (root: string) => removedFrom.set(root, (removedFrom.get(root) ?? 0) + 1);

  const isRunId = (name: string) => RUN_ID_RE.test(name);
  const passDeadline = startedAt + deadlineMs;
  const readBudget = maxDirReads * PASS_READ_BUDGET_FACTOR;
  const homeSkip = cursors.get(deps.homeRoot) ?? 0;
  const provisionSkip = cursors.get(deps.provisionRoot) ?? 0;
  // A run rotation in progress holds each root's window, to the same end entry (see RunRotation.keep).
  const rotation = runCursors.get(deps.homeRoot);
  // agent-home is listed once: its run HOMEs and its model-pass HOMEs come from one stream.
  const homeList = await listDirs(
    deps.homeRoot,
    [isRunId, (name) => MODEL_PASS_HOME_RE.test(name)],
    homeSkip,
    Math.min(maxDirReads, rotation?.keep.home ?? maxDirReads),
    readBudget,
    passDeadline,
    now,
    log,
  );
  const provisionList = await listDirs(
    deps.provisionRoot,
    [isRunId],
    provisionSkip,
    Math.min(maxDirReads, rotation?.keep.provision ?? maxDirReads),
    readBudget,
    passDeadline,
    now,
    log,
  );
  const [homes = new Map<string, string>(), passHomes = new Map<string, string>()] = homeList.lists;
  const [provisions = new Map<string, string>()] = provisionList.lists;
  summary.dirEntriesRead = homeList.read + provisionList.read;
  // A capped root was listed only in part: the pass still examines what it read (bounded),
  // and reports the stop. A deadline stop outranks a cap.
  if (homeList.capped || provisionList.capped) summary.stoppedEarly = "budget";
  if (homeList.deadline || provisionList.deadline) summary.stoppedEarly = "deadline";
  const listed = new Map<string, RunDirs>();
  for (const [id, dir] of homes) listed.set(id, { home: dir });
  for (const [id, dir] of provisions) listed.set(id, { ...listed.get(id), provision: dir });
  // Run budget: the runs are examined in id order starting after the rotation's last run
  // (the last run a pass examined before it stopped at maxEntries), wrapping round. Without
  // it every pass over the same window would examine the same first maxEntries worker-owned
  // runs, so enough runs that are never removed (e.g. HOMEs of runs the api no longer knows,
  // 404) would starve every terminal HOME behind them. Id order, not listing order, so the
  // rotation means the same thing whatever order the next listing returns. While a rotation
  // is in progress the listing window is held (below); once a pass reaches a run the
  // rotation already covered, the whole window has been examined and the listing moves on.
  const covered = (id: string): boolean => {
    if (rotation === undefined) return false;
    const { start, last } = rotation;
    return start <= last ? id >= start && id <= last : id >= start || id <= last;
  };
  const ids = [...listed.keys()].sort();
  const from = rotation === undefined ? 0 : ids.findIndex((id) => id > rotation.last);
  const order = from <= 0 ? ids : [...ids.slice(from), ...ids.slice(0, from)];
  const runs = new Map<string, RunDirs>(order.map((id) => [id, listed.get(id) as RunDirs]));
  // Forget runs whose HOME is gone, so the memo stays bounded by what is on the volume. Only
  // from a whole listing: a HOME outside a capped, resumed (or failed) listing's window is not
  // known to be gone.
  if (homeList.whole) memo?.retainOnly((id) => homes.has(id));

  let consecutiveFailures = 0;
  let firstExamined: string | undefined;
  let lastExamined: string | undefined;
  let runBudgetStop = false;
  // This pass reached a run the rotation already covered: every run of the held window has
  // now been visited. The covered runs sit at the end of `order`, so all the rest came first.
  let wrapped = false;
  for (const [runId, listed] of runs) {
    if (now() - startedAt >= deadlineMs) {
      summary.stoppedEarly = "deadline";
      break;
    }
    if (!wrapped && covered(runId)) wrapped = true;
    // Ownership first, before the run budget and any status lookup: the pass acts only on
    // directories the worker owns, the same check rmTreePinned makes on the leaf (it refuses
    // any other owner with EPERM). A run is admitted by either of its dirs being the
    // worker's; a dir of an admitted run that is not the worker's is left alone and counted.
    // That includes a legacy pre-split provision dir the entrypoint's one-time migration
    // re-owned to `runner`: it is out of this reclaim's reach, and needs the agent-uid purge
    // (#1607), which in turn requires a worker-owned root.
    const homeOwned = listed.home !== undefined && (await isWorkerOwnedDir(listed.home));
    const provisionOwned = listed.provision !== undefined && (await isWorkerOwnedDir(listed.provision));
    if (!homeOwned && !provisionOwned) {
      summary.skippedNotOwned += 1;
      continue;
    }
    const dirs: RunDirs = {};
    if (homeOwned) dirs.home = listed.home;
    if (provisionOwned) dirs.provision = listed.provision;
    else if (listed.provision) summary.provisionDirsNotOwned += 1;
    if (summary.runsExamined >= maxEntries) {
      summary.stoppedEarly = "budget";
      runBudgetStop = true;
      break;
    }
    summary.runsExamined += 1;
    firstExamined ??= runId;
    lastExamined = runId;
    if (deps.isRunLive(runId)) {
      memo?.forget(runId); // executing again: its caches may be refilled by the next park
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
    const parked = PROCESS_ENDED_PARK_STATUSES.has(status);
    // Any other status means the run moved on (resumed, or ended): forget its drop.
    if (!parked) memo?.forget(runId);
    if (!terminal && !(parked && dirs.home)) {
      summary.skippedNotEligible += 1;
      continue;
    }
    if (parked && memo?.has(runId)) {
      summary.cachesAlreadyClear += 1;
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
        // dropRunCaches never throws; it logs what it dropped and what it could not, under
        // its own message, and nothing for a no-op.
        const r = await dropCaches(dirs.home as string, log, {
          deadlineMs: Math.max(0, Math.min(RECLAIM_DROP_DEADLINE_MS, passDeadline - now())),
          message: "disk reclaim dropped a parked run's caches",
          quietNoop: true,
        });
        if (r.failed.length > 0 || r.skipped.length > 0) summary.failed += 1;
        else memo?.add(runId);
        if (r.dropped.length > 0) summary.cachesDropped += 1;
        else if (isNoop(r)) summary.cachesAlreadyClear += 1;
        return;
      }
      memo?.forget(runId);
      let failed = false;
      for (const [root, dir, key] of [
        [deps.homeRoot, dirs.home, "terminalHomesRemoved"],
        [deps.provisionRoot, dirs.provision, "provisionDirsRemoved"],
      ] as const) {
        if (!dir) continue;
        try {
          await removeTree(root, runId, passDeadline);
          summary[key] += 1;
          noteRemoved(root);
        } catch (err) {
          failed = true;
          log.warn("disk reclaim could not remove a terminal run's directory", { run_id: runId, dir, error: errMessage(err) });
        }
      }
      if (failed) summary.failed += 1;
    });
  }
  summary.unexamined = runs.size - summary.runsExamined - summary.skippedNotOwned;

  // (c) model-pass HOMEs. No lock: a pass always creates a fresh mkdtemp name, so nothing
  // ever resumes into an old one; the registry and the age bound are the whole guard.
  for (const [name, dir] of passHomes) {
    if (now() - startedAt >= deadlineMs) {
      summary.stoppedEarly ??= "deadline";
      break;
    }
    if (isModelPassLive(dir)) {
      summary.modelPassHomesLive += 1;
      continue;
    }
    let st: DirStat;
    try {
      st = await lstat(dir);
    } catch {
      continue; // gone already
    }
    // model-pass.ts creates every pass HOME as the worker (mkdtemp); a planted name is not one.
    if (!st.isDirectory() || (workerUid !== undefined && st.uid !== workerUid)) {
      summary.modelPassHomesNotOwned += 1;
      continue;
    }
    if (now() - st.mtimeMs < modelPassMinAgeMs) {
      summary.modelPassHomesTooRecent += 1;
      continue;
    }
    try {
      await removeTree(deps.homeRoot, name, passDeadline);
      summary.modelPassHomesRemoved += 1;
      noteRemoved(deps.homeRoot);
    } catch (err) {
      summary.modelPassHomesFailed += 1;
      log.warn("disk reclaim could not remove a stranded model-pass HOME", { dir, error: errMessage(err) });
    }
  }

  // A pass that stopped at its run budget before its rotation wrapped holds both roots on the
  // same window next pass (same skip, and the same end entry: RunRotation.keep) and resumes
  // the runs after the last one it examined, so every worker-owned run in the window is
  // examined before the listing moves on: a window with R worker-owned runs is held for at
  // most ceil(R / maxEntries) passes. Its removals were all inside the window, at or past the
  // skip, so the skip stays as it was. A pass that wrapped (or that did not stop at the run
  // budget) ends the rotation and moves the listing on like any other pass.
  if (runBudgetStop && !wrapped && firstExamined !== undefined && lastExamined !== undefined) {
    const keep = (root: string, listing: DirListing) =>
      listing.capped ? Math.max(0, listing.kept - (removedFrom.get(root) ?? 0)) : undefined;
    runCursors.set(deps.homeRoot, {
      start: rotation?.start ?? firstExamined,
      last: lastExamined,
      keep: { home: keep(deps.homeRoot, homeList), provision: keep(deps.provisionRoot, provisionList) },
    });
  } else {
    runCursors.delete(deps.homeRoot);
    // Advance each root's cursor past this pass's window, less the entries this pass removed
    // from inside it (each was ahead of the cursor, so the next listing is that much shorter).
    // A listing that reached the end (or its read budget) restarts from 0.
    for (const [root, listing] of [
      [deps.homeRoot, homeList],
      [deps.provisionRoot, provisionList],
    ] as const) {
      const next = listing.next > 0 ? Math.max(0, listing.next - (removedFrom.get(root) ?? 0)) : 0;
      if (next > 0) cursors.set(root, next);
      else cursors.delete(root);
    }
  }

  log.info("disk reclaim pass complete", {
    runs_examined: summary.runsExamined,
    terminal_homes_removed: summary.terminalHomesRemoved,
    provision_dirs_removed: summary.provisionDirsRemoved,
    provision_dirs_not_owned: summary.provisionDirsNotOwned,
    caches_dropped: summary.cachesDropped,
    caches_already_clear: summary.cachesAlreadyClear,
    skipped_live: summary.skippedLive,
    skipped_not_owned: summary.skippedNotOwned,
    skipped_status_unknown: summary.skippedStatusUnknown,
    skipped_not_eligible: summary.skippedNotEligible,
    failed: summary.failed,
    unexamined: summary.unexamined,
    model_pass_homes_removed: summary.modelPassHomesRemoved,
    model_pass_homes_live: summary.modelPassHomesLive,
    model_pass_homes_too_recent: summary.modelPassHomesTooRecent,
    model_pass_homes_failed: summary.modelPassHomesFailed,
    model_pass_homes_not_owned: summary.modelPassHomesNotOwned,
    dir_entries_read: summary.dirEntriesRead,
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
  /**
   * One reclaim pass (production: {@link runDiskReclaimPass} bound to its deps), or
   * undefined when the reclaim is off (UZI_DISK_RECLAIM=0): then no pass ever runs.
   */
  reclaim?: () => Promise<unknown>;
  /** Whether the admission stop is on (config.diskAdmissionEnabled, UZI_DISK_ADMISSION). */
  admission: boolean;
  /**
   * How long the admission stop may hold once a reclaim has run since the volume crossed the
   * soft threshold (config.diskAdmissionMaxWaitMs). Past it, claims reopen until the volume
   * drops under the soft threshold and crosses it again.
   */
  admissionMaxWaitMs: number;
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
 *
 * **The stop is bounded.** The run lane's claim cannot be narrowed to this worker's own
 * parked runs (the api's run-lane claim takes no such filter), so a blocked lane refuses a
 * resume of its own parked run too, and the api's disk recycle (#837) only fires at the
 * full threshold. Below it, on a single-worker install, an unbounded stop could idle the
 * worker forever with nothing reclaimable. So once a reclaim pass that started after the
 * crossing has finished and the volume is still over the soft threshold
 * `admissionMaxWaitMs` after the crossing, claims reopen with a warning; they close again
 * only on the next fresh crossing (the volume first drops under the soft threshold).
 */
export class DiskPressureController {
  private readonly now: () => number;
  private readonly pressureSpacingMs: number;
  /** The latest KNOWN sample is at or over the soft threshold. */
  private over = false;
  /** The latest sample is unknown (a statfs failure): claims are not blocked meanwhile. */
  private unknown = false;
  /** When the current stretch over the soft threshold began. */
  private overSince = 0;
  /** A reclaim pass that started during the current stretch has finished (or none can run). */
  private reclaimedSinceCrossing = false;
  /** The current stretch outlived its bounded wait: claims are open until the next crossing. */
  private reopened = false;
  private inFlight: Promise<void> | undefined;
  private lastPressurePassAt = Number.NEGATIVE_INFINITY;

  constructor(private readonly opts: DiskPressureOptions) {
    this.now = opts.now ?? Date.now;
    this.pressureSpacingMs = opts.pressureSpacingMs ?? DEFAULT_PRESSURE_SPACING_MS;
  }

  /**
   * The soft threshold: the api threshold (default 0.90) minus the soft margin. When the
   * margin is at least the threshold (an api threshold of 0.05 against the default 0.10
   * margin), that would be zero or less and block every claim at any usage; the soft
   * threshold is then the api threshold itself.
   */
  softThreshold(): number {
    const threshold = this.opts.thresholdOf() ?? DEFAULT_DISK_PRESSURE_THRESHOLD;
    const soft = threshold - this.opts.softMargin;
    return soft > 0 ? soft : threshold;
  }

  /**
   * Whether the run lane must take no new claim: the admission stop is on, the latest sample
   * is known and at or over the soft threshold, and this stretch has not outlived its bounded
   * wait.
   */
  claimsBlocked(): boolean {
    return this.opts.admission && !this.unknown && this.over && !this.reopened;
  }

  /**
   * Record the latest data-volume used fraction. Logs once per transition across the soft
   * threshold, and at or over it requests a (rate-limited) reclaim pass. Never throws and
   * never waits for the pass.
   *
   * An unknown sample (`undefined`) is not a reading: it leaves the stretch state (over,
   * since when, reopened) as it was, so a statfs blip neither ends a stretch (which would
   * restart its bounded wait at the next known sample) nor starts one. Only claimsBlocked
   * fails open while the latest sample is unknown, with one warning when that lifts an active
   * stop (a later unknown sample finds the stop already lifted and says nothing).
   */
  observe(usedFraction: number | undefined): void {
    if (usedFraction === undefined) {
      // Say once, on the known-to-unknown transition, that an active stop just lifted.
      if (this.claimsBlocked()) {
        this.opts.log.warn("disk sample unknown; claims fail open", { soft_threshold: this.softThreshold() });
      }
      this.unknown = true;
      return;
    }
    this.unknown = false;
    const soft = this.softThreshold();
    const over = usedFraction >= soft;
    const fields = { used_fraction: usedFraction, soft_threshold: soft };
    if (over !== this.over) {
      this.over = over;
      this.reopened = false;
      if (over) {
        this.overSince = this.now();
        // With the reclaim off there is nothing to wait for before the bound applies.
        this.reclaimedSinceCrossing = this.opts.reclaim === undefined;
      }
      if (this.opts.admission) {
        if (over) this.opts.log.warn("data volume at or over the soft threshold; claiming no new run until it drops", fields);
        else this.opts.log.info("data volume back under the soft threshold; claiming resumes", fields);
      }
    }
    if (!over) return;
    void this.requestReclaim("pressure");
    if (
      this.opts.admission &&
      !this.reopened &&
      this.reclaimedSinceCrossing &&
      this.now() - this.overSince >= this.opts.admissionMaxWaitMs
    ) {
      this.reopened = true;
      this.opts.log.warn("data volume still over the soft threshold after a reclaim and the admission wait; claiming resumes", {
        ...fields,
        waited_ms: this.now() - this.overSince,
      });
    }
  }

  /**
   * Start a reclaim pass unless one is running (then its promise is returned), the reclaim
   * is off, or, for a pressure trigger, the last pressure-triggered pass started less than
   * the spacing ago (then undefined). The returned promise never rejects.
   */
  requestReclaim(reason: "periodic" | "pressure" | "disk_full"): Promise<void> | undefined {
    const reclaim = this.opts.reclaim;
    if (!reclaim) return undefined;
    if (this.inFlight) return this.inFlight;
    if (reason === "pressure") {
      if (this.now() - this.lastPressurePassAt < this.pressureSpacingMs) return undefined;
      this.lastPressurePassAt = this.now();
    }
    const startedAt = this.now();
    // inFlight is assigned before the pass can run at all: the pass starts on a later
    // microtask, so even a reclaim that throws synchronously settles through the catch below
    // and clears inFlight after it was set, never before.
    const pass: Promise<void> = Promise.resolve()
      .then(reclaim)
      .then(
        () => undefined,
        (err: unknown) => this.opts.log.warn("disk reclaim pass failed", { reason, error: errMessage(err) }),
      )
      .finally(() => {
        if (this.inFlight === pass) this.inFlight = undefined;
        if (this.over && startedAt >= this.overSince) this.reclaimedSinceCrossing = true;
      });
    this.inFlight = pass;
    return pass;
  }

  /**
   * PRD #1809 D6: a write to the data volume failed disk-full, so run one reclaim pass now and
   * wait for it, before the caller retries its operation once. A pass already in flight may have
   * listed the volume before the failure, so it is awaited first and then a fresh pass runs (or
   * joins one another caller started meanwhile). Not spaced like a pressure pass: every caller is
   * a failed operation about to retry. Resolves at once when the reclaim is off. Never rejects.
   */
  async reclaimNow(): Promise<void> {
    if (!this.opts.reclaim) return;
    if (this.inFlight) await this.inFlight;
    await this.requestReclaim("disk_full");
  }

  /** The periodic pass, every `intervalMs` until `signal` aborts. Never throws. */
  async loop(signal: AbortSignal): Promise<void> {
    if (!this.opts.reclaim) return;
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
