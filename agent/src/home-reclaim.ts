import fs from "node:fs/promises";
import path from "node:path";
import type { Logger } from "./log.js";
import { rmTreeForce } from "./rmtree.js";
import { RUN_ID_RE } from "./util.js";

/**
 * One-off reclaim of run HOMEs stranded before PRD #108 M6 (`agent-home/<runId>`
 * trees the old `fs.rm` could not remove — see rmtree.ts). Runs once at worker
 * startup, so a fleet PVC that already accumulated leaked HOMEs is swept without
 * anyone shelling into a pod. It is best-effort in every direction: it never
 * throws to its caller and never blocks the worker from coming up.
 *
 * **The danger is the opposite of the leak.** A sweep that races a LIVE run
 * deletes that run's SDK session transcript out from under it — PRD #108 Risk 5,
 * and a far worse bug than the disk it reclaims. So the sweep deletes a directory
 * only when it has POSITIVELY OBSERVED the run to be terminal, and skips on every
 * other outcome, including every kind of not-knowing:
 *
 *  - not a directory, or a name that is not a run UUID → not ours, skip
 *    (symlinks included: `readdir` types entries from `lstat`, so a symlink never
 *    reports `isDirectory()` and can never be followed out of the data volume);
 *  - modified within `minAgeMs` → skip, a cheap belt beside the API check;
 *  - the status lookup THREW (api down, 5xx, timeout) → unknown, skip;
 *  - the status lookup 404'd → also unknown, skip. A 404 is *probably* a deleted
 *    run whose HOME is genuinely garbage, but "probably" is not the standard
 *    here, and the cost of being wrong is asymmetric;
 *  - any non-terminal status (`queued`, `claimed`, `running`, `awaiting_approval`)
 *    → the run may still resume into this HOME, skip.
 *
 * A requeued run reads `queued`, and a run live on ANOTHER worker sharing the
 * volume reads `running` — both non-terminal, both skipped. A terminal run never
 * resumes (`runner.ts` removes the HOME itself on terminal), so a directory that
 * survives beside a terminal status is by definition strandage.
 */

/** Run statuses a run never leaves (`runs.status` CHECK, migration `00020`:
 *  queued/claimed/running/awaiting_approval/completed/failed/cancelled). A run in
 *  one of these will never resume, so its HOME cannot be wanted again. */
export const TERMINAL_RUN_STATUSES: ReadonlySet<string> = new Set(["completed", "failed", "cancelled"]);

/**
 * How stale a directory must be before it is even a candidate.
 *
 * Sized against `RUN_TIMEOUT` (2h by default, `api/internal/config/config.go:537`) plus an
 * hour of margin: no run can legitimately outlive its own timeout, so a HOME
 * untouched for longer than that cannot belong to a run still doing work. This is
 * belt only — the API's terminal-status check is the actual oracle — but it is the
 * belt that holds if the status lookup is ever made cheaper or cached.
 */
export const DEFAULT_RECLAIM_MIN_AGE_MS = 3 * 60 * 60_000;

/** Cap on directories examined per boot, so a pathological volume cannot turn
 *  startup into an unbounded sequence of API round-trips. The next boot picks up
 *  where this one stopped (the removed ones are gone). */
export const DEFAULT_RECLAIM_MAX_ENTRIES = 500;

/**
 * Resolve a run's status. Resolving to `undefined` — or throwing — both mean
 * "unknown", and both make the sweep skip.
 */
export type RunStatusLookup = (runId: string) => Promise<string | undefined>;

export interface ReclaimOptions {
  minAgeMs?: number;
  maxEntries?: number;
  /** Injected for tests. */
  now?: () => number;
}

/** Every directory is accounted for in exactly one bucket, so the log line can be
 *  read as "why was nothing reclaimed?" rather than leaving it to inference. */
export interface ReclaimSummary {
  /** Candidate run-id directories examined (after the name filter). */
  examined: number;
  removed: number;
  skippedNotRunDir: number;
  skippedTooRecent: number;
  skippedStatusUnknown: number;
  skippedNotTerminal: number;
  /** Positively terminal, but the removal itself failed. */
  failed: number;
}

export async function reclaimStrandedRunHomes(
  homeRoot: string,
  statusOf: RunStatusLookup,
  log: Logger,
  opts: ReclaimOptions = {},
): Promise<ReclaimSummary> {
  const minAgeMs = opts.minAgeMs ?? DEFAULT_RECLAIM_MIN_AGE_MS;
  const maxEntries = opts.maxEntries ?? DEFAULT_RECLAIM_MAX_ENTRIES;
  const now = opts.now ?? Date.now;
  const summary: ReclaimSummary = {
    examined: 0,
    removed: 0,
    skippedNotRunDir: 0,
    skippedTooRecent: 0,
    skippedStatusUnknown: 0,
    skippedNotTerminal: 0,
    failed: 0,
  };

  let entries;
  try {
    entries = await fs.readdir(homeRoot, { withFileTypes: true });
  } catch (err) {
    // A worker on a fresh volume has no agent-home yet — nothing to reclaim, and
    // nothing worth a warning.
    if ((err as NodeJS.ErrnoException).code !== "ENOENT") {
      log.warn("run HOME reclaim could not read the HOME root", { home_root: homeRoot });
    }
    return summary;
  }

  for (const entry of entries) {
    // Not a directory (a stray file), a symlink, or a name that is not a run
    // UUID: not something this worker created as a run HOME.
    if (!entry.isDirectory() || !RUN_ID_RE.test(entry.name)) {
      summary.skippedNotRunDir += 1;
      continue;
    }
    if (summary.examined >= maxEntries) break;
    summary.examined += 1;
    const dir = path.join(homeRoot, entry.name);

    let st;
    try {
      st = await fs.lstat(dir);
    } catch {
      // Vanished between readdir and now — someone else cleaned it up.
      summary.skippedNotRunDir += 1;
      continue;
    }
    if (now() - st.mtimeMs < minAgeMs) {
      summary.skippedTooRecent += 1;
      continue;
    }

    let status: string | undefined;
    try {
      status = await statusOf(entry.name);
    } catch {
      status = undefined; // api unreachable, 5xx, 404, timeout — all "unknown"
    }
    if (status === undefined) {
      summary.skippedStatusUnknown += 1;
      continue;
    }
    if (!TERMINAL_RUN_STATUSES.has(status)) {
      summary.skippedNotTerminal += 1;
      continue;
    }

    try {
      await rmTreeForce(dir);
      summary.removed += 1;
    } catch {
      // Do not log the error object: it carries a filesystem path, nothing more,
      // but the count is what an operator acts on.
      summary.failed += 1;
    }
  }

  // Always log, including the all-zero case: "the sweep ran and reclaimed
  // nothing" and "the sweep never ran" must not look the same in the log.
  log.info("run HOME reclaim complete", {
    home_root: homeRoot,
    examined: summary.examined,
    removed: summary.removed,
    skipped_not_run_dir: summary.skippedNotRunDir,
    skipped_too_recent: summary.skippedTooRecent,
    skipped_status_unknown: summary.skippedStatusUnknown,
    skipped_not_terminal: summary.skippedNotTerminal,
    failed: summary.failed,
  });
  return summary;
}
