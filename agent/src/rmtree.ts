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

/** Wraps a helper command so it runs as some uid: {@link runnerCommand},
 *  {@link commandRootCommand}, or (tests) a stand-in. */
export type CommandWrapper = (command: string, args: readonly string[]) => { command: string; args: string[] };

/**
 * The fd-pinning prelude both PRD #1809 helper scripts share. Every lookup below the HOME
 * goes through `/proc/self/fd/<dirfd>/<name>`: the kernel resolves the magic link to the
 * directory the descriptor already names (by inode, not by path), then looks `name` up
 * inside it, so renaming or re-pointing an ancestor after it was opened cannot move the
 * walk. `PIN` is `O_PATH | O_DIRECTORY | O_NOFOLLOW` (`O_PATH` is Linux-only and absent
 * from `fs.constants`; its value is `010000000` on the Linux ABIs Node ships for), which
 * refuses a symlink or non-directory with ELOOP / ENOTDIR and needs only search
 * permission on the parent. Re-opening `/proc/self/fd/<pinfd>` itself for reading
 * deliberately omits `O_NOFOLLOW`: that final component is the magic link, and it names
 * the pinned inode. Exit 5 when this is not Linux or `/proc/self/fd` is missing: without
 * it no walk here is race-free, so the helpers refuse rather than fall back to paths.
 */
const PINNED_PRELUDE = `
const fs = require("node:fs");
const C = fs.constants;
const FD = "/proc/self/fd/";
const PIN = 0o10000000 | C.O_DIRECTORY | C.O_NOFOLLOW;
const READ = C.O_RDONLY | C.O_DIRECTORY;
const at = (fd, name) => FD + fd + "/" + name;
const reopen = (pinFd) => fs.openSync(FD + pinFd, READ);
const bytesOf = (st) => (typeof st.blocks === "number" ? st.blocks * 512 : st.size);
if (process.platform !== "linux" || !fs.existsSync(FD)) process.exit(5);
`;

/**
 * PRD #1809 D3: the pinned-descriptor subtree removal, as
 * `node -e <script> <home> <rel> [<sync>]`. It pins HOME, then each component of `rel`
 * relative to the previous one's descriptor (see {@link PINNED_PRELUDE}); a symlinked or
 * non-directory component is refused, never followed. It then empties the pinned leaf
 * through its own descriptor: each child directory is pinned `O_NOFOLLOW` before it is
 * descended, a symlink or file is `unlink`ed (never followed), and a directory this uid
 * owns gets OWNER `rwx` added through its pinned descriptor (the Go module cache's `0555`
 * dirs; never group, as in {@link PURGE_CHILDREN_SCRIPT}). Only then is the leaf
 * `rmdir`ed through its pinned PARENT, and only while the leaf's name there still names
 * the inode that was emptied. Nothing above the leaf is ever chmodded or removed.
 *
 * Exit 0: the leaf's name is gone from its pinned parent. 1: something remained (another
 * uid's entries, a concurrent writer). 2: a component was already missing, nothing to
 * drop. 3: a component or the leaf's final name is a symlink or non-directory (refused).
 * 4: this uid cannot open a component. 5: no `/proc/self/fd`. 6: bad arguments.
 *
 * `<sync>` is a test seam only (the worker never passes it): once the leaf is pinned the
 * script creates `<sync>.pinned` and waits up to 10 s for `<sync>.go`, so a test can swap
 * a component for a symlink between verification and removal.
 */
const PINNED_SUBTREE_SCRIPT = `${PINNED_PRELUDE}
const [home, rel, sync] = process.argv.slice(1);
if (!home || !home.startsWith("/") || !rel) process.exit(6);
const verdict = (e) => (e.code === "ENOENT" ? 2 : e.code === "ELOOP" || e.code === "ENOTDIR" ? 3 : 4);
const parts = rel.split("/");
let parentFd;
let leafPin;
try {
  leafPin = fs.openSync(home, PIN);
  for (const part of parts) {
    if (parentFd !== undefined) fs.closeSync(parentFd);
    parentFd = leafPin;
    leafPin = fs.openSync(at(parentFd, part), PIN);
  }
} catch (e) {
  process.exit(verdict(e));
}
const leafName = parts[parts.length - 1];
const uid = process.getuid();
function openOwned(pinFd) {
  const st = fs.fstatSync(pinFd);
  if (st.uid === uid && (st.mode & 0o700) !== 0o700) {
    try { fs.chmodSync(FD + pinFd, (st.mode & 0o7777) | 0o700); } catch {}
  }
  return reopen(pinFd);
}
let leafFd;
try { leafFd = openOwned(leafPin); } catch { process.exit(4); }
if (sync) {
  fs.writeFileSync(sync + ".pinned", "");
  const cell = new Int32Array(new SharedArrayBuffer(4));
  const deadline = Date.now() + 10000;
  while (!fs.existsSync(sync + ".go") && Date.now() < deadline) Atomics.wait(cell, 0, 0, 10);
}
let failed = 0;
function emptyDir(dirFd) {
  let entries;
  try { entries = fs.readdirSync(FD + dirFd, { withFileTypes: true }); } catch { failed++; return; }
  for (const e of entries) {
    const p = at(dirFd, e.name);
    if (!e.isDirectory()) {
      try { fs.unlinkSync(p); } catch (err) { if (err.code !== "ENOENT") failed++; }
      continue;
    }
    let pin;
    try { pin = fs.openSync(p, PIN); } catch (err) {
      // Swapped for a symlink or file since readdir: remove that entry, never follow it.
      if (err.code === "ELOOP" || err.code === "ENOTDIR") { try { fs.unlinkSync(p); } catch { failed++; } }
      else if (err.code !== "ENOENT") failed++;
      continue;
    }
    let childFd;
    try { childFd = openOwned(pin); } catch { failed++; } finally { fs.closeSync(pin); }
    if (childFd === undefined) continue;
    try { emptyDir(childFd); } finally { fs.closeSync(childFd); }
    try { fs.rmdirSync(p); } catch (err) { if (err.code !== "ENOENT") failed++; }
  }
}
emptyDir(leafFd);
const pinned = fs.fstatSync(leafPin);
fs.closeSync(leafFd);
fs.closeSync(leafPin);
function nameState() {
  try {
    const fd = fs.openSync(at(parentFd, leafName), PIN);
    const st = fs.fstatSync(fd);
    fs.closeSync(fd);
    return st.dev === pinned.dev && st.ino === pinned.ino ? "pinned" : "other";
  } catch (e) {
    return e.code === "ENOENT" ? "gone" : e.code === "ELOOP" || e.code === "ENOTDIR" ? "refused" : "other";
  }
}
if (nameState() === "pinned") {
  try { fs.rmdirSync(at(parentFd, leafName)); } catch {}
}
const after = nameState();
process.exit(after === "gone" ? 0 : after === "refused" ? 3 : 1);
`;

/** Hard ceiling on the entries one measuring pass counts (see {@link measureRunCaches}). */
const MEASURE_MAX_ENTRIES = 500_000;

/**
 * PRD #1809 D8: the measuring walk, as `node -e <script> <home> <rels-json> <max>`. Pins
 * each cache subtree through descriptors exactly as {@link PINNED_SUBTREE_SCRIPT} does (a
 * symlinked or missing component counts zero), then sums allocated bytes (`blocks * 512`,
 * what the data volume actually loses) over the subtree with `lstat`, descending only
 * into directories it re-pins `O_NOFOLLOW`. It stops after `<max>` entries and says so
 * (`truncated`), so the reading is a lower bound; unreadable entries are skipped, which
 * also only lowers it. Prints one JSON line.
 */
const MEASURE_CACHES_SCRIPT = `${PINNED_PRELUDE}
const [home, relsJson, maxArg] = process.argv.slice(1);
const max = Number(maxArg);
let cacheBytes = 0, entries = 0, truncated = false;
function walk(dirFd) {
  let names;
  try { names = fs.readdirSync(FD + dirFd); } catch { return; }
  for (const name of names) {
    if (entries >= max) { truncated = true; return; }
    entries++;
    const p = at(dirFd, name);
    let st;
    try { st = fs.lstatSync(p); } catch { continue; }
    cacheBytes += bytesOf(st);
    if (!st.isDirectory()) continue;
    let childFd;
    try { const pin = fs.openSync(p, PIN); try { childFd = reopen(pin); } finally { fs.closeSync(pin); } } catch { continue; }
    try { walk(childFd); } finally { fs.closeSync(childFd); }
  }
}
for (const rel of JSON.parse(relsJson)) {
  let fd;
  try {
    fd = fs.openSync(home, PIN);
    for (const part of rel.split("/")) { const next = fs.openSync(at(fd, part), PIN); fs.closeSync(fd); fd = next; }
    cacheBytes += bytesOf(fs.fstatSync(fd));
    const dirFd = reopen(fd);
    try { walk(dirFd); } finally { fs.closeSync(dirFd); }
  } catch {
    // Missing, symlinked or unreadable: this pass counts nothing more for it.
  } finally {
    if (fd !== undefined) fs.closeSync(fd);
  }
}
process.stdout.write(JSON.stringify({ cacheBytes, truncated }) + "\\n");
`;

/** The uids a PRD #1809 helper runs as: under the split `runner`, then `runner-cmd` (Codex
 *  command roots are members of group `runner` and can write into a group-writable HOME),
 *  then, for a removal, `runner` again for what the second pass unblocked. Single-uid the
 *  wrapper is the identity, so the one pass runs as the worker itself. */
function agentWrappers(passes: "remove" | "measure"): CommandWrapper[] {
  if (!uidSplitActive()) return [runnerCommand];
  return passes === "remove" ? [runnerCommand, commandRootCommand, runnerCommand] : [runnerCommand, commandRootCommand];
}

/** Run one helper pass and resolve its exit status. Rejects only when the helper could not
 *  run at all (spawn failure, timeout): no exit status is then a verdict. The env is
 *  minimal and explicit, as in {@link purgeChildrenAsAgents}. */
async function runHelper(wrap: CommandWrapper, script: string, args: readonly string[]): Promise<number> {
  const wrapped = wrap(process.execPath, ["-e", script, ...args]);
  return execFileAsync(wrapped.command, wrapped.args, {
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
}

/**
 * PRD #1809 D3: remove ONE named cache subtree (a {@link RUN_CACHE_SUBTREES} entry)
 * inside a run HOME, leaving the HOME root and every sibling intact. Resolves "removed"
 * or "absent" (nothing to drop); throws on a refusal or when the subtree is still there.
 *
 * {@link rmHomeTree} cannot be pointed at a subtree: it widens the root it is given to
 * group `runner` and expects a worker-owned root, while a cache dir is written by the
 * agent. This helper never chmods the HOME root or any sibling, and never resolves the
 * subtree by path after checking it: a run HOME is `worker:runner 0775` (worker umask
 * `002`) and not sticky, so any LIVE `runner`/`runner-cmd` process can rename `.cache`,
 * `go` or `go/pkg` and plant a symlink at any moment. Reaping this run's processes before
 * the park does not remove every such writer: another run's processes on the same worker
 * run as the same uids and are not reaped. So the check and the deletion are one walk
 * over pinned directory descriptors ({@link PINNED_SUBTREE_SCRIPT}); a swapped component
 * can at worst make it delete the tree it pinned (now under another name) or refuse.
 *
 * Under the uid split the walk runs as `runner`, then `runner-cmd`, then `runner` (the
 * owners of what the agent wrote; the worker cannot even list a runner-private dir), with
 * the same cap-clearing wrappers and minimal env as {@link purgeChildrenAsAgents}, and
 * stops at the first pass that settles it. No worker path-based removal runs at all.
 * Single-uid the same script runs once as the worker (a child process of the same uid, so
 * there is one implementation of the walk). Without `/proc/self/fd` (not Linux) it
 * refuses in both modes: every worker is a Linux container, and the path-based fallback
 * is exactly what this replaced.
 *
 * `wrappers` is a test seam: the uids to run the passes as, in order.
 */
export async function rmHomeSubtree(
  home: string,
  rel: string,
  wrappers: readonly CommandWrapper[] = agentWrappers("remove"),
): Promise<"removed" | "absent"> {
  // The helper takes both as bare `node -e` arguments, so a relative or dash-leading
  // HOME, or an unlisted `rel`, is refused before anything runs.
  if (!path.isAbsolute(home)) throw new Error(`rmHomeSubtree: refusing non-absolute HOME ${home}`);
  if (!isRunCacheSubtree(rel)) {
    throw new Error(`rmHomeSubtree: refusing ${JSON.stringify(rel)}, not a listed cache subtree`);
  }
  let last = "no pass ran";
  for (const wrap of wrappers) {
    const code = await runHelper(wrap, PINNED_SUBTREE_SCRIPT, [home, rel]);
    if (code === 0) return "removed";
    if (code === 2) return "absent";
    // A symlink is a symlink whoever looks: a refusal is final, no later pass runs.
    if (code === 3) {
      throw Object.assign(new Error(`rmHomeSubtree: refusing ${rel} under ${home} (symlink or non-directory)`), {
        code: "ELOOP",
      });
    }
    if (code === 5) throw new Error(`rmHomeSubtree: refusing ${rel} under ${home} (no descriptor-pinned walk here)`);
    last = code === 4 ? "a component this uid cannot open" : `exit ${code}`;
  }
  throw Object.assign(new Error(`rmHomeSubtree: ${rel} is still present under ${home} (last pass: ${last})`), {
    code: "ENOTEMPTY",
  });
}

function isRunCacheSubtree(rel: string): rel is RunCacheSubtree {
  return (RUN_CACHE_SUBTREES as readonly string[]).includes(rel);
}

/** PRD #1809 D8: allocated bytes under a run HOME's {@link RUN_CACHE_SUBTREES}. */
export interface RunCacheBytes {
  cacheBytes: number;
  /** A pass hit its entry ceiling, so `cacheBytes` is a lower bound. */
  truncated: boolean;
}

/**
 * PRD #1809 D8: measure the cache subtrees of a run HOME (only those, never the whole
 * HOME). Under the uid split the worker cannot list the agent's `0700` dirs, so the walk
 * runs as `runner` and as `runner-cmd` through the same wrappers the removal uses, and
 * the larger reading is kept: each uid sees a subset, so the result is a lower bound,
 * never an over-count from summing passes. Single-uid it runs once as the worker. Each
 * pass stops after `maxEntries` entries and flags the reading truncated. Rejects only
 * when no pass produced a reading. `wrappers` and `maxEntries` are test seams.
 */
export async function measureRunCaches(
  home: string,
  wrappers: readonly CommandWrapper[] = agentWrappers("measure"),
  maxEntries: number = MEASURE_MAX_ENTRIES,
): Promise<RunCacheBytes> {
  if (!path.isAbsolute(home)) throw new Error(`measureRunCaches: refusing non-absolute HOME ${home}`);
  let best: RunCacheBytes | undefined;
  let lastErr: unknown;
  for (const wrap of wrappers) {
    const wrapped = wrap(process.execPath, [
      "-e",
      MEASURE_CACHES_SCRIPT,
      home,
      JSON.stringify(RUN_CACHE_SUBTREES),
      String(maxEntries),
    ]);
    try {
      const { stdout } = await execFileAsync(wrapped.command, wrapped.args, {
        env: { PATH: "/usr/local/bin:/usr/bin:/bin" },
        timeout: PURGE_TIMEOUT_MS,
        maxBuffer: 64 * 1024,
      });
      const r = JSON.parse(stdout) as RunCacheBytes;
      best = {
        cacheBytes: Math.max(best?.cacheBytes ?? 0, r.cacheBytes),
        truncated: (best?.truncated ?? false) || r.truncated,
      };
    } catch (e) {
      lastErr = e;
    }
  }
  if (!best) throw lastErr ?? new Error("measureRunCaches: no measuring pass ran");
  return best;
}
