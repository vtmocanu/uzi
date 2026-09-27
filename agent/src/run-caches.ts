import type { Logger } from "./log.js";
import { measureRunCaches, rmHomeSubtree, type RunCacheBytes, RUN_CACHE_SUBTREES } from "./rmtree.js";
import { errMessage } from "./util.js";

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
 * a failed measurement drops only the numbers.
 */
export async function dropRunCaches(home: string, log: Logger): Promise<void> {
  const before = await measureQuietly(home);
  const dropped: string[] = [];
  const absent: string[] = [];
  const failed: string[] = [];
  for (const rel of RUN_CACHE_SUBTREES) {
    try {
      if ((await rmHomeSubtree(home, rel)) === "removed") dropped.push(rel);
      else absent.push(rel);
    } catch (e) {
      failed.push(rel);
      log.warn("run cache drop failed; the parked HOME keeps it", { run_home: home, subtree: rel, error: errMessage(e) });
    }
  }
  // Nothing to measure again when nothing was measured, or nothing was there.
  const after = before && before.cacheBytes > 0 ? await measureQuietly(home) : before;
  log.info("run caches dropped on park", {
    run_home: home,
    dropped,
    ...(absent.length > 0 ? { absent } : {}),
    ...(failed.length > 0 ? { failed } : {}),
    ...(before && after
      ? {
          bytes_freed: Math.max(0, before.cacheBytes - after.cacheBytes),
          cache_bytes_left: after.cacheBytes,
          ...(before.truncated || after.truncated ? { measure_truncated: true } : {}),
        }
      : {}),
  });
}

async function measureQuietly(home: string): Promise<RunCacheBytes | undefined> {
  try {
    return await measureRunCaches(home);
  } catch {
    return undefined;
  }
}
