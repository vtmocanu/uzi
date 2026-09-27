import type { Logger } from "./log.js";
import { type HomeSubtreeBytes, measureHomeSubtrees, rmHomeSubtree, RUN_CACHE_SUBTREES } from "./rmtree.js";
import { errMessage } from "./util.js";

/**
 * PRD #1809 D2: drop a parked run's rebuildable caches ({@link RUN_CACHE_SUBTREES}) from
 * its preserved HOME, keeping everything else a resume needs (the session transcript,
 * `.claude.json`, `go/bin`, unknown files). Called from `executeClaim`'s finally for a
 * Claude run that parked, i.e. whose process has ended; a live gate park never reaches
 * that finally, and a Codex run's caches live on its own per-run volume.
 *
 * Best-effort and NEVER throws: it runs in a `finally`, and a failed drop only means the
 * HOME keeps its caches exactly as before this change. The HOME is measured before and
 * after (through the agent uids under the split, see {@link measureHomeSubtrees}) so the
 * log line carries the bytes actually freed; a failed measurement drops only the numbers.
 */
export async function dropRunCaches(home: string, log: Logger): Promise<void> {
  const before = await measureQuietly(home);
  const dropped: string[] = [];
  const failed: string[] = [];
  for (const rel of RUN_CACHE_SUBTREES) {
    try {
      await rmHomeSubtree(home, rel);
      dropped.push(rel);
    } catch (e) {
      failed.push(rel);
      log.warn("run cache drop failed; the parked HOME keeps it", { run_home: home, subtree: rel, error: errMessage(e) });
    }
  }
  const after = await measureQuietly(home);
  log.info("run caches dropped on park", {
    run_home: home,
    dropped,
    ...(failed.length > 0 ? { failed } : {}),
    ...(before && after
      ? {
          bytes_freed: Math.max(0, before.homeBytes - after.homeBytes),
          home_bytes: after.homeBytes,
          cache_bytes_left: after.cacheBytes,
        }
      : {}),
  });
}

async function measureQuietly(home: string): Promise<HomeSubtreeBytes | undefined> {
  try {
    return await measureHomeSubtrees(home);
  } catch {
    return undefined;
  }
}
