import type { Logger } from "./log.js";
import { measureRunCaches, rmHomeSubtree, type RunCacheBytes, RUN_CACHE_SUBTREES } from "./rmtree.js";
import { errMessage } from "./util.js";

/**
 * The whole drop's wall-time ceiling: both measurements and every removal pass share it.
 * It runs in `executeClaim`'s finally, so without one a pathological HOME could hold the
 * worker slot for every helper's own timeout in turn (3 subtrees x 3 uid passes x 120 s,
 * plus the measurements: about 26 minutes). Five minutes caps that worst case; whatever the
 * drop did not reach stays in the HOME, exactly as it did before this drop existed.
 */
const DROP_DEADLINE_MS = 5 * 60_000;

/**
 * PRD #1809 D2: drop a parked run's rebuildable caches ({@link RUN_CACHE_SUBTREES}) from
 * its preserved HOME, keeping everything else a resume needs (the session transcript,
 * `.claude.json`, `go/bin`, unknown files). Called from `executeClaim`'s finally for a
 * Claude run that parked, i.e. whose process has ended; a live gate park never reaches
 * that finally, and a Codex run's caches live on its own per-run volume.
 *
 * Best-effort and NEVER throws: it runs in a `finally`, and a failed drop only means the
 * HOME keeps its caches exactly as before this change. Only the cache subtrees are
 * measured (see {@link measureRunCaches}), before the drop and again after it, so the log
 * line carries the bytes actually freed without walking the rest of a possibly huge HOME;
 * a failed measurement drops only the numbers. Every step runs against one deadline
 * (`deadlineMs` from now, {@link DROP_DEADLINE_MS} by default); a subtree the deadline
 * leaves no time for is skipped with a warning, and the second measurement is skipped too.
 */
export async function dropRunCaches(home: string, log: Logger, deadlineMs: number = DROP_DEADLINE_MS): Promise<void> {
  const deadline = Date.now() + deadlineMs;
  const before = await measureQuietly(home, deadline);
  const dropped: string[] = [];
  const absent: string[] = [];
  const failed: string[] = [];
  const skipped: string[] = [];
  for (const rel of RUN_CACHE_SUBTREES) {
    if (Date.now() >= deadline) {
      skipped.push(rel);
      continue;
    }
    try {
      if ((await rmHomeSubtree(home, rel, { deadline })) === "removed") dropped.push(rel);
      else absent.push(rel);
    } catch (e) {
      failed.push(rel);
      log.warn("run cache drop failed; the parked HOME keeps it", { run_home: home, subtree: rel, error: errMessage(e) });
    }
  }
  if (skipped.length > 0) {
    log.warn("run cache drop ran out of time; the parked HOME keeps the rest", {
      run_home: home,
      skipped,
      deadline_ms: deadlineMs,
    });
  }
  // Nothing to measure again when nothing was measured, nothing was there, or no time is left.
  const after =
    before && before.cacheBytes > 0 ? (Date.now() < deadline ? await measureQuietly(home, deadline) : undefined) : before;
  log.info("run caches dropped on park", {
    run_home: home,
    dropped,
    ...(absent.length > 0 ? { absent } : {}),
    ...(failed.length > 0 ? { failed } : {}),
    ...(skipped.length > 0 ? { skipped } : {}),
    ...(before && after
      ? {
          bytes_freed: Math.max(0, before.cacheBytes - after.cacheBytes),
          cache_bytes_left: after.cacheBytes,
          ...(before.truncated || after.truncated ? { measure_truncated: true } : {}),
        }
      : {}),
  });
}

async function measureQuietly(home: string, deadline: number): Promise<RunCacheBytes | undefined> {
  try {
    return await measureRunCaches(home, { deadline });
  } catch {
    return undefined;
  }
}
