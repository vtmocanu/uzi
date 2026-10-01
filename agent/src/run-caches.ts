import type { Logger } from "./log.js";
import {
  type CacheUnit,
  evictCacheUnits,
  listGoBuildUnits,
  listNpmCache,
  measureRunCaches,
  type NpmCacheListing,
  rmHomeSubtree,
  type RunCacheBytes,
  RUN_CACHE_SUBTREES,
  type TrimHelperOptions,
} from "./rmtree.js";
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
 * Claude run that parked, i.e. whose process has ended, and (issue #1830) in place by the
 * hard disk layer for a live run that cannot be parked (a gate wait, finalize), where the
 * run keeps going and `keep` names any subtree a still-running step needs; a Codex run's
 * caches live on its own per-run volume.
 *
 * Best-effort and NEVER throws: it runs in a `finally`, and a failed drop only means the
 * HOME keeps its caches exactly as before this change. Only the cache subtrees are
 * measured (see {@link measureRunCaches}), before the drop and again after it, so the log
 * line carries the bytes actually freed without walking the rest of a possibly huge HOME;
 * a failed measurement drops only the numbers. Every step runs against one deadline
 * (`deadlineMs` from now, {@link DROP_DEADLINE_MS} by default); a subtree the deadline
 * leaves no time for is skipped with a warning, and the second measurement is skipped too.
 *
 * Resolves what happened to each subtree, so a caller can tell a drop that freed something
 * from a no-op. `message` names the log line (the park's by default; the disk reclaim logs
 * its own), and `quietNoop` suppresses that line when every subtree was already absent.
 */
export async function dropRunCaches(home: string, log: Logger, opts: DropRunCachesOptions = {}): Promise<RunCacheDropResult> {
  const deadlineMs = opts.deadlineMs ?? DROP_DEADLINE_MS;
  const deadline = Date.now() + deadlineMs;
  const before = await measureQuietly(home, deadline);
  const dropped: string[] = [];
  const absent: string[] = [];
  const failed: string[] = [];
  const skipped: string[] = [];
  for (const rel of RUN_CACHE_SUBTREES) {
    if (opts.keep?.includes(rel)) continue;
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
  const result: RunCacheDropResult = { dropped, absent, failed, skipped };
  if (opts.quietNoop && isNoop(result)) return result;
  log.info(opts.message ?? "run caches dropped on park", {
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
  return result;
}

/** What {@link dropRunCaches} did with each {@link RUN_CACHE_SUBTREES} entry. */
export interface RunCacheDropResult {
  dropped: string[];
  absent: string[];
  failed: string[];
  /** Not attempted: the deadline had passed. */
  skipped: string[];
}

export interface DropRunCachesOptions {
  /** The whole drop's wall-time ceiling from now (default {@link DROP_DEADLINE_MS}). */
  deadlineMs?: number;
  /** The summary log line's message (default "run caches dropped on park"). */
  message?: string;
  /** Log nothing when every subtree was already absent. */
  quietNoop?: boolean;
  /** Subtrees ({@link RUN_CACHE_SUBTREES} entries) to leave in place; issue #1830: the in-place
   *  drop keeps `.npm/_cacache` while a live `npm ci` is still reading it. A kept subtree is
   *  neither dropped nor reported absent. */
  keep?: readonly string[];
}

/** Every subtree was already absent: the drop found nothing to do. */
export function isNoop(r: RunCacheDropResult): boolean {
  return r.dropped.length === 0 && r.failed.length === 0 && r.skipped.length === 0;
}

async function measureQuietly(home: string, deadline: number): Promise<RunCacheBytes | undefined> {
  try {
    return await measureRunCaches(home, { deadline });
  } catch {
    return undefined;
  }
}

/** The whole between-turns trim's wall-time ceiling: it holds the run at a turn boundary. */
const TRIM_DEADLINE_MS = 3 * 60_000;

/** What a trim is asked to do, all in bytes. */
export interface TrimTarget {
  /** The run's measured cache bytes (all {@link RUN_CACHE_SUBTREES}) before the trim. */
  totalBytes: number;
  /** The run's cap: the module cache goes whole only while the rest leaves the run over it. */
  capBytes: number;
  /** Where eviction stops: a trim evicts until the caches are at or under this. */
  lowWaterBytes: number;
}

/** What {@link trimRunCaches} did. Byte counts are the evicted units' allocated bytes as listed. */
export interface TrimResult {
  goBuildBytes: number;
  goBuildUnits: number;
  npmBytes: number;
  npmBuckets: number;
  npmContents: number;
  /** Why the npm stage evicted nothing although it ran (an incomplete listing). */
  npmSkipped?: string;
  /** Units kept because their mtime or type changed between the listing and the eviction. */
  kept: number;
  /** `go/pkg/mod` was removed whole (stage c). */
  moduleCacheRemoved: boolean;
  /** The trim ran out of its deadline before finishing its stages. */
  outOfTime: boolean;
  /** `totalBytes` minus the listed bytes of what was evicted (the module cache's are not listed). */
  estimatedBytesAfter: number;
}

/** Seams for {@link trimRunCaches}; production uses the rmtree.ts helpers. */
export interface TrimDeps {
  listGoBuild: (home: string, needBytes: number, opts: TrimHelperOptions) => Promise<{ units: CacheUnit[]; truncated: boolean }>;
  listNpm: (home: string, opts: TrimHelperOptions) => Promise<NpmCacheListing>;
  evict: (
    home: string,
    rel: string,
    units: readonly CacheUnit[],
    opts: TrimHelperOptions,
  ) => Promise<{ removed: Set<string>; kept: number; failed: number }>;
  removeSubtree: (home: string, rel: string, opts: { deadline?: number }) => Promise<"removed" | "absent">;
  deadlineMs?: number;
  now?: () => number;
}

const defaultTrimDeps: TrimDeps = {
  listGoBuild: listGoBuildUnits,
  listNpm: listNpmCache,
  evict: evictCacheUnits,
  removeSubtree: rmHomeSubtree,
};

/**
 * `content-v2/<algo>/<xx>/<yy>/<rest>` → `<algo>:<hex digest>`, the key an integrity token maps
 * to (cacache stores content at its algorithm and hex digest split 2/2/rest), or undefined for
 * any other path.
 */
function contentKey(rel: string): string | undefined {
  const p = rel.split("/");
  if (p.length !== 5 || p[0] !== "content-v2") return undefined;
  return `${p[1]}:${p[2]}${p[3]}${p[4]}`;
}

/** `sha512-<base64>` → `sha512:<hex digest>`, or undefined when it does not parse. */
function integrityKey(token: string): string | undefined {
  const dash = token.indexOf("-");
  if (dash <= 0) return undefined;
  const hex = Buffer.from(token.slice(dash + 1), "base64").toString("hex");
  return hex ? `${token.slice(0, dash)}:${hex}` : undefined;
}

/** How many of `buckets`' lines reference each content key. */
function referenceCounts(buckets: readonly { integrities: string[] }[]): Map<string, number> {
  const refs = new Map<string, number>();
  for (const b of buckets) {
    for (const t of b.integrities) {
      const k = integrityKey(t);
      if (k) refs.set(k, (refs.get(k) ?? 0) + 1);
    }
  }
  return refs;
}

/** The listed content files no bucket in `refs` references. */
function unreferenced(contents: readonly CacheUnit[], refs: Map<string, number>): CacheUnit[] {
  return contents.filter((c) => {
    const k = contentKey(c.rel);
    return k !== undefined && (refs.get(k) ?? 0) === 0;
  });
}

/**
 * PRD #1809 D4: TRIM (never wipe) a running Claude run's caches, called only at a proven quiet
 * point (between turns, no process of the run alive). Evicts least-recently-used entries until
 * the caches are at or under `lowWaterBytes`, in this order:
 *
 *  a. `.cache/go-build`, oldest mtime first, one unit per `*-a`/`*-d` file or executable-cache
 *     directory; `README`, `trim.txt` and `testexpire.txt` are never touched. Go refreshes an
 *     entry's mtime when it uses it (at most hourly), so the oldest mtimes are the least recently
 *     used entries.
 *  b. only while still over the low-water mark: `.npm/_cacache` by index entry. The oldest
 *     `index-v5` bucket files go first; then only `content-v2` files that no REMAINING bucket's
 *     integrity references are deleted (npm's cache errors a request whose index entry names
 *     content that is gone, so content under a live entry is never deleted). Skipped when the
 *     index could not be read completely.
 *  c. only while still over the CAP after (a) and (b): `go/pkg/mod` whole, through
 *     {@link rmHomeSubtree} (Go does not support evicting single modules from its read-only
 *     tree; it re-downloads). `go/bin` is outside it and kept.
 *
 * Every listing and eviction runs through the pinned, agent-uid helpers in rmtree.ts: symlinks are
 * never followed, and an entry whose mtime changed between the listing and the eviction (it was
 * used meanwhile) is kept. The whole trim shares one deadline. Never throws: a failed step leaves
 * what it did not remove, and the caller measures again.
 */
export async function trimRunCaches(
  home: string,
  target: TrimTarget,
  log: Logger,
  deps: TrimDeps = defaultTrimDeps,
): Promise<TrimResult> {
  const now = deps.now ?? Date.now;
  const deadline = now() + (deps.deadlineMs ?? TRIM_DEADLINE_MS);
  const opts: TrimHelperOptions = { deadline };
  const result: TrimResult = {
    goBuildBytes: 0,
    goBuildUnits: 0,
    npmBytes: 0,
    npmBuckets: 0,
    npmContents: 0,
    kept: 0,
    moduleCacheRemoved: false,
    outOfTime: false,
    estimatedBytesAfter: target.totalBytes,
  };
  const over = (limit: number): boolean => result.estimatedBytesAfter > limit;
  const timeLeft = (): boolean => {
    if (now() < deadline) return true;
    result.outOfTime = true;
    return false;
  };

  // (a) go-build, oldest first, until the low-water mark.
  if (over(target.lowWaterBytes) && timeLeft()) {
    try {
      const need = result.estimatedBytesAfter - target.lowWaterBytes;
      const { units } = await deps.listGoBuild(home, need, opts);
      const chosen: CacheUnit[] = [];
      let sum = 0;
      for (const u of units) {
        if (sum >= need) break;
        chosen.push(u);
        sum += u.bytes;
      }
      if (chosen.length > 0) {
        const ev = await deps.evict(home, ".cache/go-build", chosen, opts);
        for (const u of chosen) {
          if (!ev.removed.has(u.rel)) continue;
          result.goBuildBytes += u.bytes;
          result.goBuildUnits++;
        }
        result.kept += ev.kept;
        result.estimatedBytesAfter -= result.goBuildBytes;
      }
    } catch (e) {
      log.warn("run cache trim: go build cache step failed", { run_home: home, error: errMessage(e) });
    }
  }

  // (b) npm, by index entry, only while still over the low-water mark.
  if (over(target.lowWaterBytes) && timeLeft()) {
    try {
      await trimNpm(home, target, result, opts, deps);
    } catch (e) {
      log.warn("run cache trim: npm cache step failed", { run_home: home, error: errMessage(e) });
    }
  }

  // (c) the module cache, whole, only while still over the CAP.
  if (over(target.capBytes) && timeLeft()) {
    try {
      result.moduleCacheRemoved = (await deps.removeSubtree(home, "go/pkg/mod", { deadline })) === "removed";
    } catch (e) {
      log.warn("run cache trim: module cache removal failed", { run_home: home, error: errMessage(e) });
    }
  }
  return result;
}

/** Stage (b) of {@link trimRunCaches}. */
async function trimNpm(
  home: string,
  target: TrimTarget,
  result: TrimResult,
  opts: TrimHelperOptions,
  deps: TrimDeps,
): Promise<void> {
  const listing = await deps.listNpm(home, opts);
  if (!listing.complete) {
    result.npmSkipped = "the npm index could not be listed completely";
    return;
  }
  if (listing.buckets.length === 0 && listing.contents.length === 0) return;
  const need = result.estimatedBytesAfter - target.lowWaterBytes;
  // Plan: orphaned content first (nothing references it), then the oldest buckets, each freeing
  // itself plus the content only it referenced, until the need is met.
  const refs = referenceCounts(listing.buckets);
  const contentByKey = new Map<string, CacheUnit>();
  for (const c of listing.contents) {
    const k = contentKey(c.rel);
    if (k) contentByKey.set(k, c);
  }
  let planned = unreferenced(listing.contents, refs).reduce((n, c) => n + c.bytes, 0);
  const chosen: typeof listing.buckets = [];
  for (const b of listing.buckets) {
    if (planned >= need) break;
    chosen.push(b);
    planned += b.bytes;
    for (const t of b.integrities) {
      const k = integrityKey(t);
      if (!k) continue;
      const n = (refs.get(k) ?? 0) - 1;
      refs.set(k, n);
      if (n === 0) planned += contentByKey.get(k)?.bytes ?? 0;
    }
  }
  let removedBuckets = new Set<string>();
  if (chosen.length > 0) {
    const ev = await deps.evict(home, ".npm/_cacache", chosen, opts);
    removedBuckets = ev.removed;
    result.kept += ev.kept;
  }
  for (const b of chosen) {
    if (!removedBuckets.has(b.rel)) continue;
    result.npmBuckets++;
    result.npmBytes += b.bytes;
  }
  // Content is judged against the buckets that REMAIN (every listed bucket not actually removed,
  // including a chosen one kept because it changed), never against the plan.
  const remaining = listing.buckets.filter((b) => !removedBuckets.has(b.rel));
  const orphans = unreferenced(listing.contents, referenceCounts(remaining));
  if (orphans.length > 0) {
    const ev = await deps.evict(home, ".npm/_cacache", orphans, opts);
    result.kept += ev.kept;
    for (const c of orphans) {
      if (!ev.removed.has(c.rel)) continue;
      result.npmContents++;
      result.npmBytes += c.bytes;
    }
  }
  result.estimatedBytesAfter -= result.npmBytes;
}
