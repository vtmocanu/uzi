import fs from "node:fs/promises";
import { constants } from "node:fs";
import { execFile } from "node:child_process";
import path from "node:path";
import { promisify } from "node:util";
import { commandRootCommand, runnerCommand, uidSplitActive } from "./runner-uid.js";

const execFileAsync = promisify(execFile);

/**
 * Remove a tree that may contain read-only DIRECTORIES.
 *
 * `fs.rm(p, { recursive: true, force: true })` is not enough: `force` suppresses
 * ENOENT, not EACCES. Unlinking a file needs write+execute on its PARENT
 * directory, and the Go module cache deliberately writes every package directory
 * mode `0555` — so a run that so much as `go build`s leaves a tree the runner's
 * cleanup cannot touch. Measured 2026-07-21 against a `0555` fixture (macOS,
 * uid != 0):
 *
 *   EACCES: permission denied, unlink '<home>/go/pkg/mod/gopkg.in/inf.v0@v0.9.1/benchmark_test.go'
 *   (code EACCES, errno -13, syscall unlink)
 *
 * `fs.rm` rejects on the first such entry. What survives, measured rather than
 * assumed: the read-only directories and everything under them, plus every
 * ancestor up to the run HOME (a directory cannot go while a child remains).
 * Siblings it reached first ARE removed — so the leak is the module cache plus a
 * hollow skeleton, which is where the incident's 167.3 MB sat.
 *
 * Strategy: try the cheap plain `rm` first — the overwhelmingly common case is a
 * HOME with no Go cache in it, and that path must not pay for a full walk. Only
 * when it fails with a permission error do we walk the tree restoring owner
 * write+execute on directories and retry. A partially-removed tree is fine: `rm`
 * is idempotent, and the second pass finishes what the first started.
 */
export async function rmTreeForce(target: string): Promise<void> {
  try {
    await fs.rm(target, { recursive: true, force: true });
    return;
  } catch (err) {
    if (!isPermissionError(err)) throw err;
  }
  await restoreTreeWritability(target);
  // Anything still un-removable after this throws, and the caller decides. The
  // run-terminal caller logs and continues: cleanup is best-effort by design and
  // must never turn a completed run into a failed one.
  await fs.rm(target, { recursive: true, force: true });
}

/** EACCES (no write on the parent) or EPERM (the same refusal on some platforms
 *  / with immutable-ish flags). Anything else — ENOSPC, EIO, EBUSY — is NOT a
 *  permission problem and a chmod walk would not help, so it propagates. */
function isPermissionError(err: unknown): boolean {
  const code = (err as NodeJS.ErrnoException | undefined)?.code;
  return code === "EACCES" || code === "EPERM";
}

/**
 * Depth-first walk adding owner `rwx` to every directory under (and including)
 * `target`.
 *
 * Order matters: a directory must be chmod'd BEFORE it can be read (traversing
 * needs `x`, listing needs `r`), so the chmod happens on the way DOWN, never on
 * the way back up.
 *
 * **The chmod cannot be redirected onto a symlink's target.** It is an `fchmod`
 * against a handle opened `O_DIRECTORY | O_NOFOLLOW`, so a symlink is refused by
 * the kernel at open time (measured: `ENOTDIR` on macOS, `ELOOP` on Linux — the
 * code differs, which is why nothing here matches on it) and the mode change
 * applies to the inode the descriptor already names. An earlier version did
 * `lstat` then a path-based `fs.chmod`, which a same-uid writer could redirect
 * between the two calls.
 *
 * **What this does NOT guarantee**, stated because the previous comment claimed
 * more than it held: the walk still resolves each level's path from the root, and
 * `O_NOFOLLOW` only constrains the FINAL component. A same-uid attacker who can
 * swap an INTERMEDIATE directory for a symlink mid-walk can still redirect where
 * we descend. Node exposes no `openat`, so a fully race-free walk is not
 * available here. The residual severity is very low: chmod requires ownership, so
 * the target is already same-uid; the change only ADDS owner `rwx`; and anyone
 * who can win that race can delete the tree outright without it.
 *
 * Best-effort per entry: one unreadable subtree must not abort the restoration
 * of its siblings. The subsequent `rm` is the thing that reports real failure.
 */
export async function restoreTreeWritability(target: string): Promise<void> {
  let handle;
  try {
    handle = await fs.open(target, constants.O_RDONLY | constants.O_DIRECTORY | constants.O_NOFOLLOW);
  } catch {
    // Not a directory, a symlink, already gone, or not ours to open. Nothing to
    // widen, and nothing here should be widened.
    return;
  }
  try {
    // OR the bits in rather than assigning 0o700: the tree is about to be deleted
    // so the exact mode hardly matters, but widening-only cannot surprise anyone
    // reading a half-swept tree after a crash.
    const st = await handle.stat();
    await handle.chmod(st.mode | 0o700);
  } catch {
    // Not the owner (or a read-only mount) — the rm will report it.
  } finally {
    await handle.close().catch(() => undefined);
  }
  let entries;
  try {
    entries = await fs.readdir(target, { withFileTypes: true });
  } catch {
    return;
  }
  for (const entry of entries) {
    // `readdir(withFileTypes)` types entries from `lstat`, so a symlink to a
    // directory reports `isSymbolicLink()`, not `isDirectory()`, and is skipped
    // here as well as refused by the O_NOFOLLOW open above.
    if (!entry.isDirectory()) continue;
    await restoreTreeWritability(path.join(target, entry.name));
  }
}

/**
 * Issue #1607: remove a per-run or advice HOME (`agent-home/<runId>`, `uzi-<label>-*`)
 * under the PRD #51 worker/runner uid split.
 *
 * The HOME root is created by the worker (uid `worker`), but everything the agent
 * writes inside it is owned by `runner`: the SDK CLI runs as `runner`, so the Go
 * module cache (`0555` dirs) and the SDK's private `0700` `.claude/projects` are
 * runner-owned. The worker cannot `chmod` a directory it does not own and cannot even
 * `scandir` a runner-private one, so {@link rmTreeForce} fails EACCES and the HOME
 * leaks on the data volume (measured: 0.3-1.6 GB per run, a 20 GB volume filled).
 *
 * Strategy, in trust order:
 *  1. Try {@link rmTreeForce} as the worker. Single-uid starts and HOMEs the agent
 *     never wrote into end here, exactly as before.
 *  2. Only under the split and only on a permission error: make the worker-owned
 *     root traversable by group `runner` (an `fchmod` on an `O_NOFOLLOW` handle,
 *     refusing anything that is not a directory the worker owns), then delete the
 *     root's CHILDREN as each agent uid through the existing cap-clearing `setpriv`
 *     wrappers: `runner`, then `runner-cmd` (Codex command roots are members of group
 *     `runner` and can write into a group-writable HOME), then `runner` again for
 *     what the second pass unblocked. Each pass widens only OWNER bits, never group:
 *     a `runner-cmd` entry nested inside a `runner`-private dir stays stranded rather
 *     than exposing that dir to other runs' command roots. This is not
 *     an escalation: each helper holds exactly the privileges of the agent surface
 *     that wrote those files, and every caller runs after those surfaces are reaped.
 *  3. Finish with {@link rmTreeForce} as the worker, which removes the root (a
 *     `runner` helper cannot: the root is worker-owned under the sticky
 *     `agent-home` parent) plus any worker-owned leftovers.
 *
 * The caller still decides what a failure means; every current caller logs and
 * continues, because a cleanup must never fail a run.
 */
export async function rmHomeTree(target: string, deps: HomeRemovalDeps = defaultHomeRemovalDeps()): Promise<void> {
  // The helper passes the path as a bare `node -e` argument, so a relative or
  // dash-leading path could be parsed as a node option. Every caller builds an
  // absolute path under the data dir; anything else is a bug, not a HOME.
  if (!path.isAbsolute(target)) throw new Error(`rmHomeTree: refusing non-absolute path ${target}`);
  try {
    await deps.removeTree(target);
    return;
  } catch (err) {
    if (!deps.splitActive || !isPermissionError(err)) throw err;
  }
  await openRootToRunnerGroup(target);
  // The helper's exit status is not the verdict: it cannot remove the root and may
  // legitimately leave worker-owned entries. The final worker pass below is what
  // throws if anything is still in the way.
  await deps.purgeChildrenAsAgents(target).catch(() => undefined);
  await deps.removeTree(target);
}

/** Seams for {@link rmHomeTree}, injected by tests. */
export interface HomeRemovalDeps {
  splitActive: boolean;
  removeTree: (target: string) => Promise<void>;
  purgeChildrenAsAgents: (target: string) => Promise<void>;
}

function defaultHomeRemovalDeps(env: NodeJS.ProcessEnv = process.env): HomeRemovalDeps {
  return {
    splitActive: uidSplitActive(env),
    removeTree: rmTreeForce,
    purgeChildrenAsAgents: purgeChildrenAsAgents,
  };
}

/**
 * Add group `rwx` to the worker-owned HOME root so the `runner` helper can traverse
 * it. Refuses a symlink (the `O_NOFOLLOW` open fails), a non-directory, and a root the
 * worker does not own: those are not a HOME this worker created, and nothing here may
 * widen or delete through them. The root's group is `runner` via the setgid
 * `agent-home` parent; widening a HOME that is about to be deleted exposes nothing the
 * runner does not already own.
 */
async function openRootToRunnerGroup(target: string): Promise<void> {
  const handle = await fs.open(target, constants.O_RDONLY | constants.O_DIRECTORY | constants.O_NOFOLLOW);
  try {
    const st = await handle.stat();
    const uid = process.getuid?.();
    if (uid !== undefined && st.uid !== uid) {
      throw Object.assign(new Error(`HOME root ${target} is not owned by this worker (uid ${st.uid})`), {
        code: "EPERM",
      });
    }
    await handle.chmod((st.mode & 0o7777) | 0o770);
  } finally {
    await handle.close().catch(() => undefined);
  }
}

/**
 * The script each agent-uid helper runs, as `node -e <script> <target>`. It walks the
 * root's children adding OWNER `rwx` to the directories it owns (the `0555` Go module
 * cache), then removes each child it can. Owner-only on purpose: widening a private
 * `0700` dir to group `runner` would expose provider state to every `runner-cmd`
 * process, including another run's still-live command root. Both steps refuse to follow symlinks: the walk opens with
 * `O_NOFOLLOW`, readdir types entries from `lstat`, and `rmSync` unlinks a symlink
 * rather than its target. It never touches the root itself. Errors are swallowed per
 * entry; the worker's final pass reports what remains.
 */
export const PURGE_CHILDREN_SCRIPT = `
const fs = require("node:fs");
const path = require("node:path");
const root = process.argv[1];
const flags = fs.constants.O_RDONLY | fs.constants.O_DIRECTORY | fs.constants.O_NOFOLLOW;
function widen(dir) {
  let fd;
  try { fd = fs.openSync(dir, flags); } catch { return; }
  try { fs.fchmodSync(fd, (fs.fstatSync(fd).mode & 0o7777) | 0o700); } catch {} finally { fs.closeSync(fd); }
  let entries;
  try { entries = fs.readdirSync(dir, { withFileTypes: true }); } catch { return; }
  for (const e of entries) if (e.isDirectory()) widen(path.join(dir, e.name));
}
let children;
try { children = fs.readdirSync(root, { withFileTypes: true }); } catch { process.exit(1); }
for (const e of children) if (e.isDirectory()) widen(path.join(root, e.name));
let failed = 0;
for (const e of children) {
  try { fs.rmSync(path.join(root, e.name), { recursive: true, force: true }); } catch { failed++; }
}
process.exit(failed === 0 ? 0 : 1);
`;

/** Hard ceiling per helper pass, so a pathological tree cannot hang a run's teardown. */
const PURGE_TIMEOUT_MS = 120_000;

/**
 * Run {@link PURGE_CHILDREN_SCRIPT} as `runner`, then `runner-cmd`, then `runner` again
 * (or directly single-uid, where it is the same uid and only reached from tests). Each
 * pass's failure is expected (it cannot remove what the other uid owns), so only a
 * spawn that could not run at all is surfaced, and the caller ignores even that: the
 * worker's final pass is the verdict. The env is minimal and explicit: the helpers need
 * no credential, and the worker's own env (PAT, join token) must not reach them.
 */
async function purgeChildrenAsAgents(target: string): Promise<void> {
  for (const wrap of [runnerCommand, commandRootCommand, runnerCommand]) {
    const wrapped = wrap(process.execPath, ["-e", PURGE_CHILDREN_SCRIPT, target]);
    await execFileAsync(wrapped.command, wrapped.args, {
      env: { PATH: "/usr/local/bin:/usr/bin:/bin" },
      timeout: PURGE_TIMEOUT_MS,
      maxBuffer: 64 * 1024,
    }).catch((e: NodeJS.ErrnoException & { code?: unknown }) => {
      // A non-zero exit is the expected partial pass; only a helper that could not run is news.
      if (typeof e.code !== "number") throw e;
    });
  }
}

/**
 * PRD #1809 D2: the rebuildable caches a process-ending park drops from a run HOME,
 * relative to that HOME. Each is refilled on demand by its tool (`go build` refills the
 * build cache, `go` re-downloads modules, `npm` refills its content cache), so dropping
 * one costs a resumed run time, never state. Nothing outside this list is ever deleted
 * by {@link rmHomeSubtree}: the session transcript, `.claude.json`, `go/bin` and every
 * unknown file stay. Adding an entry needs code evidence that it is rebuildable.
 */
export const RUN_CACHE_SUBTREES = [".cache/go-build", "go/pkg/mod", ".npm/_cacache"] as const;

type RunCacheSubtree = (typeof RUN_CACHE_SUBTREES)[number];

/** What an agent-uid chain helper concluded (see {@link SUBTREE_CHAIN_SCRIPT}). */
type ChainVerdict = "ok" | "absent" | "refused" | "failed";

/** Seams for {@link rmHomeSubtree}, injected by tests to simulate the uid split. */
export interface SubtreeRemovalDeps {
  splitActive: boolean;
  removeTree: (target: string) => Promise<void>;
  purgeChildrenAsAgents: (target: string) => Promise<void>;
  /** Run {@link SUBTREE_CHAIN_SCRIPT} as the agent uids: `check` verifies the chain,
   *  `rmdir` also removes the (by then empty) leaf. */
  chainAsAgents: (home: string, rel: RunCacheSubtree, mode: "check" | "rmdir") => Promise<ChainVerdict>;
}

function defaultSubtreeRemovalDeps(): SubtreeRemovalDeps {
  return {
    splitActive: uidSplitActive(),
    removeTree: rmTreeForce,
    purgeChildrenAsAgents: purgeChildrenAsAgents,
    chainAsAgents: chainAsAgents,
  };
}

/**
 * PRD #1809 D3: remove ONE named cache subtree (a {@link RUN_CACHE_SUBTREES} entry)
 * inside a run HOME, leaving the HOME root and every sibling intact.
 *
 * {@link rmHomeTree} cannot be pointed at a subtree: it widens the root it is given to
 * group `runner` and expects a worker-owned root, while a cache dir is written by the
 * agent. This helper never chmods the HOME root or any sibling.
 *
 *  1. Refuse anything but a listed `rel` under an absolute `home`.
 *  2. Walk HOME and each component of `rel` with `O_DIRECTORY | O_NOFOLLOW` opens: a
 *     symlinked or non-directory `.cache`, `go`, `go/pkg`, `.npm` or leaf is refused
 *     (a throw), never followed. A missing component means there is nothing to drop,
 *     which is success. Under the split, a component the worker cannot open (a
 *     runner-private dir) is checked by the same walk run as the agent uids.
 *  3. Try the worker's plain {@link rmTreeForce}. Single-uid and worker-writable trees
 *     end here.
 *  4. Under the split, on a permission error only: run {@link PURGE_CHILDREN_SCRIPT} on
 *     the subtree as the agent uids (it removes the subtree's children, not the subtree),
 *     then remove the now-empty leaf, first as the worker and, when its parent is
 *     agent-owned (`go/pkg`), by an agent-uid `rmdir` behind a re-run of the chain walk.
 *  5. Walk the chain again and throw unless the subtree is gone. Whether that failure
 *     matters is the caller's call; the park caller logs it and keeps the run parked.
 *
 * The walk resolves each level by path (Node has no `openat`), so the residual
 * {@link restoreTreeWritability} documents applies here too: a writer racing an
 * intermediate component into a symlink between the walk and the removal. The park
 * caller runs after the run's agent processes are reaped, which removes that writer.
 */
export async function rmHomeSubtree(
  home: string,
  rel: string,
  deps: SubtreeRemovalDeps = defaultSubtreeRemovalDeps(),
): Promise<void> {
  if (!path.isAbsolute(home)) throw new Error(`rmHomeSubtree: refusing non-absolute HOME ${home}`);
  if (!isRunCacheSubtree(rel)) {
    throw new Error(`rmHomeSubtree: refusing ${JSON.stringify(rel)}, not a listed cache subtree`);
  }
  const target = path.join(home, rel);
  if ((await verifyChain(home, rel, deps)) === "absent") return;
  try {
    await deps.removeTree(target);
    return;
  } catch (err) {
    if (!deps.splitActive || !isPermissionError(err)) throw err;
  }
  // As in rmHomeTree, the helper's exit status is not the verdict; the final walk is.
  await deps.purgeChildrenAsAgents(target).catch(() => undefined);
  try {
    await deps.removeTree(target);
  } catch (err) {
    if (!isPermissionError(err)) throw err;
    await deps.chainAsAgents(home, rel, "rmdir");
  }
  if ((await verifyChain(home, rel, deps)) !== "absent") {
    throw Object.assign(new Error(`rmHomeSubtree: ${rel} is still present under ${home}`), { code: "ENOTEMPTY" });
  }
}

function isRunCacheSubtree(rel: string): rel is RunCacheSubtree {
  return (RUN_CACHE_SUBTREES as readonly string[]).includes(rel);
}

/**
 * Steps 2 and 5 of {@link rmHomeSubtree}: "present" (every component a real directory)
 * or "absent" (some component missing); a symlink / non-directory component, or
 * anything else the walk could not settle, throws.
 */
async function verifyChain(
  home: string,
  rel: RunCacheSubtree,
  deps: SubtreeRemovalDeps,
): Promise<"present" | "absent"> {
  let dir = home;
  for (const part of ["", ...rel.split("/")]) {
    if (part !== "") dir = path.join(dir, part);
    try {
      const handle = await fs.open(dir, constants.O_RDONLY | constants.O_DIRECTORY | constants.O_NOFOLLOW);
      await handle.close().catch(() => undefined);
    } catch (err) {
      const code = (err as NodeJS.ErrnoException | undefined)?.code;
      if (code === "ENOENT") return "absent";
      if (deps.splitActive && isPermissionError(err)) {
        // A runner-private component the worker cannot open: let the agent uids walk it.
        const verdict = await deps.chainAsAgents(home, rel, "check");
        if (verdict === "ok") return "present";
        if (verdict === "absent") return "absent";
        throw new Error(`rmHomeSubtree: refusing ${rel} under ${home} (agent-uid walk: ${verdict})`);
      }
      // ELOOP (Linux) / ENOTDIR (macOS, or a plain file): a symlink or non-directory is
      // not a path this code deletes through.
      throw Object.assign(new Error(`rmHomeSubtree: refusing ${dir} (${code ?? "error"})`), { code });
    }
  }
  return "present";
}

/**
 * The agent-uid half of {@link verifyChain}, as `node -e <script> <home> <rel> <mode>`.
 * Walks HOME then each component of `rel` with `O_DIRECTORY | O_NOFOLLOW` opens. Exit
 * 0: every component is a real directory (and, in `rmdir` mode, the leaf was removed or
 * was already gone); 2: a component is missing; 3: a symlink or non-directory component
 * (refused); anything else: this uid could not decide (e.g. it cannot traverse a
 * component). `rmdirSync` removes only an EMPTY directory and never follows a symlink.
 */
export const SUBTREE_CHAIN_SCRIPT = `
const fs = require("node:fs");
const path = require("node:path");
const [home, rel, mode] = process.argv.slice(1);
const flags = fs.constants.O_RDONLY | fs.constants.O_DIRECTORY | fs.constants.O_NOFOLLOW;
let dir = home;
for (const part of ["", ...rel.split("/")]) {
  if (part !== "") dir = path.join(dir, part);
  let fd;
  try { fd = fs.openSync(dir, flags); } catch (e) {
    if (e.code === "ENOENT") process.exit(2);
    if (e.code === "ELOOP" || e.code === "ENOTDIR") process.exit(3);
    process.exit(4);
  }
  fs.closeSync(fd);
}
if (mode === "rmdir") {
  try { fs.rmdirSync(dir); } catch (e) { if (e.code !== "ENOENT") process.exit(4); }
}
process.exit(0);
`;

/**
 * Run {@link SUBTREE_CHAIN_SCRIPT} as `runner`, then `runner-cmd`, stopping at the first
 * definite answer. A refusal from either uid is final: a symlink is a symlink whoever
 * looks. Same minimal env and ceiling as {@link purgeChildrenAsAgents}.
 */
async function chainAsAgents(home: string, rel: RunCacheSubtree, mode: "check" | "rmdir"): Promise<ChainVerdict> {
  for (const wrap of [runnerCommand, commandRootCommand]) {
    const wrapped = wrap(process.execPath, ["-e", SUBTREE_CHAIN_SCRIPT, home, rel, mode]);
    const code = await execFileAsync(wrapped.command, wrapped.args, {
      env: { PATH: "/usr/local/bin:/usr/bin:/bin" },
      timeout: PURGE_TIMEOUT_MS,
      maxBuffer: 64 * 1024,
    }).then(
      () => 0,
      (e: NodeJS.ErrnoException & { code?: unknown }) => {
        if (typeof e.code !== "number") throw e;
        return e.code;
      },
    );
    if (code === 0) return "ok";
    if (code === 2) return "absent";
    if (code === 3) return "refused";
  }
  return "failed";
}

/** PRD #1809 D6: allocated bytes of a run HOME and of its {@link RUN_CACHE_SUBTREES}. */
export interface HomeSubtreeBytes {
  homeBytes: number;
  cacheBytes: number;
}

/**
 * The walk each measuring uid runs, as `node -e <script> <home> <subtrees-json>`. An
 * `lstat` walk that never follows a symlink (only `lstat` directories are descended),
 * summing allocated bytes (`blocks * 512`, what the data volume actually loses; `size`
 * where a platform reports no blocks). Unreadable entries are skipped, so a pass is a
 * lower bound of what it could not see. Prints one JSON line.
 */
const MEASURE_HOME_SCRIPT = `
const fs = require("node:fs");
const path = require("node:path");
const home = process.argv[1];
const caches = JSON.parse(process.argv[2]).map((r) => path.join(home, r));
let homeBytes = 0, cacheBytes = 0;
function walk(p, inCache) {
  let st;
  try { st = fs.lstatSync(p); } catch { return; }
  const n = typeof st.blocks === "number" ? st.blocks * 512 : st.size;
  homeBytes += n;
  if (inCache) cacheBytes += n;
  if (!st.isDirectory()) return;
  let entries;
  try { entries = fs.readdirSync(p); } catch { return; }
  for (const e of entries) { const c = path.join(p, e); walk(c, inCache || caches.includes(c)); }
}
walk(home, false);
process.stdout.write(JSON.stringify({ homeBytes, cacheBytes }) + "\\n");
`;

type CommandWrapper = (command: string, args: readonly string[]) => { command: string; args: string[] };

/**
 * PRD #1809 D3/D6: measure a run HOME and the cache bytes under it. Under the uid split
 * the worker cannot list the agent's `0700` dirs, so the walk runs as `runner` and as
 * `runner-cmd` through the same cap-clearing wrappers the purge uses, and the larger
 * reading of each field is kept: each uid sees a subset, so the result is a lower bound,
 * never an over-count from summing passes. Single-uid it runs once, unwrapped (the
 * wrapper is the identity there). Rejects only when no pass produced a reading.
 */
export async function measureHomeSubtrees(
  home: string,
  wrappers: readonly CommandWrapper[] = uidSplitActive() ? [runnerCommand, commandRootCommand] : [runnerCommand],
): Promise<HomeSubtreeBytes> {
  if (!path.isAbsolute(home)) throw new Error(`measureHomeSubtrees: refusing non-absolute HOME ${home}`);
  let best: HomeSubtreeBytes | undefined;
  let lastErr: unknown;
  for (const wrap of wrappers) {
    const wrapped = wrap(process.execPath, ["-e", MEASURE_HOME_SCRIPT, home, JSON.stringify(RUN_CACHE_SUBTREES)]);
    try {
      const { stdout } = await execFileAsync(wrapped.command, wrapped.args, {
        env: { PATH: "/usr/local/bin:/usr/bin:/bin" },
        timeout: PURGE_TIMEOUT_MS,
        maxBuffer: 64 * 1024,
      });
      const r = JSON.parse(stdout) as HomeSubtreeBytes;
      best = {
        homeBytes: Math.max(best?.homeBytes ?? 0, r.homeBytes),
        cacheBytes: Math.max(best?.cacheBytes ?? 0, r.cacheBytes),
      };
    } catch (e) {
      lastErr = e;
    }
  }
  if (!best) throw lastErr ?? new Error("measureHomeSubtrees: no measuring pass ran");
  return best;
}
