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
 *  - modified within `minAgeMs` → skip;
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
 * Sized against `RUN_TIMEOUT` (2h by default, `api/internal/config/config.go:537`)
 * plus an hour of margin.
 *
 * **This filter is much weaker than it looks, and it is NOT a second line of
 * defence for a live run.** A directory's mtime tracks only its own DIRECT
 * entries, so the writes a live run actually performs — appending to
 * `.claude/projects/<x>.jsonl`, several levels down — do not refresh the HOME's
 * own mtime at all. Measured 2026-07-21: a deep write left the top-level mtime
 * byte-identical, while touching a direct child refreshed it. So any run older
 * than `minAgeMs` sails straight past this check, and with `RUN_TIMEOUT` at 2h
 * runs routinely live in that gap.
 *
 * **Past `minAgeMs`, the API's terminal-status check is the ONLY thing standing
 * between a live run and a deleted HOME.** That check is sufficient — a live run
 * reads `running`/`claimed`/`queued` and is skipped — but it is load-bearing
 * alone, so do not weaken it on the theory that the age filter backs it up.
 */
export const DEFAULT_RECLAIM_MIN_AGE_MS = 3 * 60 * 60_000;

/**
 * Consecutive status-lookup failures after which the sweep gives up for this boot.
 *
 * When the api is unreachable NOTHING can be reclaimed — every candidate resolves
 * to "unknown" and skips — so each further round-trip is pure cost paid before the
 * worker can register. Against a HANGING api (NetworkPolicy drop, dead Service
 * endpoints) each lookup costs the full `WORKER_HTTP_TIMEOUT`, so at the 500-entry
 * budget an un-bailed sweep can block startup for hours. Three failures is enough
 * to distinguish "the api is down" from one unlucky request.
 */
export const DEFAULT_RECLAIM_MAX_CONSECUTIVE_FAILURES = 3;

/**
 * Wall-clock ceiling for the whole sweep. The consecutive-failure bail handles the
 * common outage; this bounds every other slow shape (a very large volume, an api
 * that is slow but not failing) so startup can never be held hostage by cleanup.
 */
export const DEFAULT_RECLAIM_DEADLINE_MS = 60_000;

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
  maxConsecutiveFailures?: number;
  deadlineMs?: number;
  /** Injected for tests. */
  now?: () => number;
}

/**
 * Every EXAMINED directory lands in exactly one bucket
 * (`removed + skippedTooRecent + skippedStatusUnknown + skippedNotTerminal +
 * failed === examined`), and `unexamined` carries everything the sweep stopped
 * short of.
 *
 * That last field exists because the earlier version claimed full accounting and
 * did not have it: when the budget cut the loop short, the remainder fell into no
 * counter and no log field, so `examined: 500, removed: 3` could not distinguish a
 * volume holding 503 directories from one holding 50,000 — precisely the case
 * where an operator needs to know. A comment is an assertion; this one now holds.
 */
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
  /** Candidates the sweep never looked at, because it stopped early. */
  unexamined: number;
  /** Why it stopped, or undefined if it ran the whole directory. */
  stoppedEarly?: "budget" | "api_unreachable" | "deadline";
}

export async function reclaimStrandedRunHomes(
  homeRoot: string,
  statusOf: RunStatusLookup,
  log: Logger,
  opts: ReclaimOptions = {},
): Promise<ReclaimSummary> {
  const minAgeMs = opts.minAgeMs ?? DEFAULT_RECLAIM_MIN_AGE_MS;
  const maxEntries = opts.maxEntries ?? DEFAULT_RECLAIM_MAX_ENTRIES;
  const maxConsecutiveFailures = opts.maxConsecutiveFailures ?? DEFAULT_RECLAIM_MAX_CONSECUTIVE_FAILURES;
  const deadlineMs = opts.deadlineMs ?? DEFAULT_RECLAIM_DEADLINE_MS;
  const now = opts.now ?? Date.now;
  const startedAt = now();
  let consecutiveFailures = 0;
  const summary: ReclaimSummary = {
    examined: 0,
    removed: 0,
    skippedNotRunDir: 0,
    skippedTooRecent: 0,
    skippedStatusUnknown: 0,
    skippedNotTerminal: 0,
    failed: 0,
    unexamined: 0,
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

  // Candidates first, so "how many did we not get to" is a subtraction from a list
  // that exists rather than a number nobody counted.
  const candidates = entries.filter((e) => e.isDirectory() && RUN_ID_RE.test(e.name));
  summary.skippedNotRunDir = entries.length - candidates.length;

  for (const entry of candidates) {
    if (summary.examined >= maxEntries) {
      summary.stoppedEarly = "budget";
      break;
    }
    if (now() - startedAt >= deadlineMs) {
      summary.stoppedEarly = "deadline";
      break;
    }
    summary.examined += 1;
    const dir = path.join(homeRoot, entry.name);

    let st;
    try {
      st = await fs.lstat(dir);
    } catch {
      // Vanished between readdir and now — someone else cleaned it up. Counted as
      // "too recent to touch" rather than leaking out of the accounting.
      summary.skippedTooRecent += 1;
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
      consecutiveFailures += 1;
      // BAIL OUT. With the api unreachable every remaining candidate resolves to
      // "unknown" and skips, so the rest of the sweep can reclaim nothing and only
      // spends time the worker needs to register, recover its orphaned runs and
      // start heartbeating. Against a HANGING api each of those costs a full HTTP
      // timeout, which is how a cleanup pass turns into hours of blocked startup.
      if (consecutiveFailures >= maxConsecutiveFailures) {
        summary.stoppedEarly = "api_unreachable";
        break;
      }
      continue;
    }
    consecutiveFailures = 0;
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

  summary.unexamined = candidates.length - summary.examined;

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
    unexamined: summary.unexamined,
    stopped_early: summary.stoppedEarly,
    took_ms: now() - startedAt,
  });
  return summary;
}
