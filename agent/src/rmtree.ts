import fs from "node:fs/promises";
import { constants } from "node:fs";
import { type ChildProcess, execFile, spawn } from "node:child_process";
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
 * The streamed directory readers the PRD #1809 helper scripts share. Neither loads a whole
 * directory: `fs.opendirSync` + `Dir.readSync()` pulls entries from the kernel in small buffered
 * batches, so a run's (prompt-injectable) agent that fills a cache dir with a million names cannot
 * make a helper allocate them all at once. Every dirent read is charged to `spend()`, and reading
 * stops when that says the budget is gone.
 *
 * `readSome(dirFd, limit, skip, spend)` reopens the directory through its pinned descriptor (a
 * fresh stream positioned at the start), reads at most `limit` names not in `skip`, and closes it.
 *
 * `drain(dirFd, limit, spend, skip, each, onFirst)` reads the directory through ONE open stream
 * in batches of at most `limit` names not in `skip`, and hands each batch to `each` (which returns
 * how many of its names it removed, and adds the ones it could not to `skip`) before reading the
 * next. The cursor is kept across batches, so a name that could not be removed is read once per
 * round, not once per batch: an earlier version reopened the stream for every batch, so a
 * directory of N stuck names cost about N^2/batch reads and could spend a pass's whole entry
 * budget before the next uid's pass ran (#1809 M1 review). Whether a stream returns an entry added
 * or removed after it was opened is unspecified, so a round that removed something is followed by
 * one more round from a fresh stream, which picks up anything the first missed; a round that
 * removed nothing ends the drain. `onFirst` runs once, after the first batch is read and before it
 * is handed on. Exported only so a test can drive `drain` over a real directory.
 */
export const STREAM_PRELUDE = `
function readSome(dirFd, limit, skip, spend) {
  const dir = fs.opendirSync(FD + dirFd);
  const out = [];
  try {
    let e;
    while (out.length < limit && (e = dir.readSync()) !== null) {
      if (!spend()) break;
      if (!skip.has(e.name)) out.push({ name: e.name, isDir: e.isDirectory() });
    }
  } finally {
    dir.closeSync();
  }
  return out;
}
function drain(dirFd, limit, spend, skip, each, onFirst) {
  let first = true;
  for (;;) {
    let dir;
    try { dir = fs.opendirSync(FD + dirFd); } catch { return; }
    let removed = 0;
    let stop = false;
    try {
      for (;;) {
        const batch = [];
        let ended = false;
        while (batch.length < limit) {
          let e;
          try { e = dir.readSync(); } catch { stop = true; break; }
          if (e === null) { ended = true; break; }
          if (!spend()) { stop = true; break; }
          if (!skip.has(e.name)) batch.push({ name: e.name, isDir: e.isDirectory() });
        }
        if (first) { first = false; if (onFirst) onFirst(); }
        // A batch already read is processed even when reading it spent the budget: its
        // dirents are paid for, and the handler descends no directory past the budget.
        if (batch.length > 0) removed += each(batch);
        if (stop || ended) break;
      }
    } finally {
      dir.closeSync();
    }
    if (stop || removed === 0) return;
  }
}
`;

/**
 * PRD #1809 D3: the pinned-descriptor subtree removal, as
 * `node -e <script> <home> <rel> <maxEntries> <budgetMs> <mode> <expect> [<sync> [<phase>]]`.
 * It pins HOME,
 * then each component of `rel` relative to the previous one's descriptor (see
 * {@link PINNED_PRELUDE}); a symlinked or non-directory component is refused, never
 * followed. It then empties the pinned leaf through its own descriptor: each child
 * directory is pinned `O_NOFOLLOW` before it is descended, a symlink or file is `unlink`ed
 * (never followed), and a directory this uid owns gets OWNER `rwx` added through its pinned
 * descriptor (the Go module cache's `0555` dirs; never group, as in
 * {@link PURGE_CHILDREN_SCRIPT}). Finally the leaf is `rmdir`ed through its pinned PARENT,
 * after checking immediately before that the name there still resolves to the emptied
 * inode; a race between that check and the `rmdir` can at worst remove an empty directory
 * planted there. Nothing above the leaf is ever chmodded or removed.
 *
 * Bounded work: each directory is read through {@link STREAM_PRELUDE}'s `drain`, in streamed
 * batches processed before the next is read, on one cursor per round (an entry that could not
 * be removed is skipped, and read again only by the one extra round a round with removals
 * earns). Every dirent read counts against `<maxEntries>`, and the pass also stops at
 * `<budgetMs>` of wall time; hitting either exits 8 with the rest left in place. Recursion
 * holds one descriptor per level (two while a level's stream is open).
 *
 * `<mode>` `remove` (what {@link rmHomeSubtree} passes) does all of the above. `empty` (what
 * {@link rmTreePinned} passes) stops once the leaf is emptied and never `rmdir`s it: the
 * caller removes the leaf itself, as the uid that owns it. `<expect>` is `-` or the
 * `<dev>:<ino>` the leaf must pin to; a leaf that pins to anything else (the name was
 * swapped since the caller pinned it) is refused before anything is touched (exit 3).
 *
 * Exit 0: the leaf's name is gone from its pinned parent (`empty`: the leaf is empty). 2: a component was already
 * missing, nothing to drop. 3: a component or the leaf's final name is a symlink or
 * non-directory (refused). 4: something remained (another uid's entries, a concurrent
 * writer). 5: no `/proc/self/fd`. 6: bad arguments. 7: this uid cannot open a component.
 * 8: the entry or time budget ran out. Any other status (Node's 1 on an uncaught throw, a
 * `setpriv` failure) means the helper itself failed, which is not a verdict on the tree.
 *
 * `<sync>` is a test seam only (the worker never passes it): the script creates
 * `<sync>.<phase>` and waits up to 10 s for `<sync>.go`, so a test can swap a path
 * between verification and removal. Phase `pinned` (the default) pauses once the leaf is
 * pinned; phase `listed` pauses after the leaf's first batch of names was read and before
 * any child in it is pinned; phase `emptied` pauses after a child directory was emptied and
 * before it is `rmdir`ed through its parent.
 */
const PINNED_SUBTREE_SCRIPT = `${PINNED_PRELUDE}${STREAM_PRELUDE}
const [home, rel, maxArg, budgetArg, mode = "remove", expect = "-", sync, phase = "pinned"] = process.argv.slice(1);
const maxEntries = Number(maxArg);
const budgetMs = Number(budgetArg);
if (!home || !home.startsWith("/") || !rel || !(maxEntries >= 0) || !(budgetMs >= 0)) process.exit(6);
if (mode !== "remove" && mode !== "empty") process.exit(6);
const deadline = Date.now() + budgetMs;
let readCount = 0;
let exhausted = false;
const spend = () => {
  if (exhausted) return false;
  if (readCount >= maxEntries || Date.now() > deadline) { exhausted = true; return false; }
  readCount++;
  return true;
};
function pause(point) {
  if (!sync || phase !== point) return;
  fs.writeFileSync(sync + "." + phase, "");
  const cell = new Int32Array(new SharedArrayBuffer(4));
  const until = Date.now() + 10000;
  while (!fs.existsSync(sync + ".go") && Date.now() < until) Atomics.wait(cell, 0, 0, 10);
}
const verdict = (e) => (e.code === "ENOENT" ? 2 : e.code === "ELOOP" || e.code === "ENOTDIR" ? 3 : 7);
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
if (expect !== "-") {
  const st = fs.fstatSync(leafPin, { bigint: true });
  if (st.dev + ":" + st.ino !== expect) process.exit(3);
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
try { leafFd = openOwned(leafPin); } catch { process.exit(7); }
pause("pinned");
const BATCH = 256;
const gone = (err) => err.code === "ENOENT";
// Remove one entry of the directory pinned at dirFd; true when its name is gone.
function removeEntry(dirFd, e) {
  const p = at(dirFd, e.name);
  if (!e.isDir) {
    try { fs.unlinkSync(p); return true; } catch (err) { return gone(err); }
  }
  let pin;
  try { pin = fs.openSync(p, PIN); } catch (err) {
    // Swapped for a symlink or file since it was listed: remove that entry, never follow it.
    if (err.code === "ELOOP" || err.code === "ENOTDIR") {
      try { fs.unlinkSync(p); return true; } catch (err2) { return gone(err2); }
    }
    return gone(err);
  }
  let childFd;
  try { childFd = openOwned(pin); } catch { return false; } finally { fs.closeSync(pin); }
  try { emptyDir(childFd, false); } finally { fs.closeSync(childFd); }
  if (exhausted) return false;
  pause("emptied");
  try { fs.rmdirSync(p); return true; } catch (err) {
    // Swapped for a symlink or file while it was being emptied: unlink that entry itself.
    if (err.code === "ENOTDIR") {
      try { fs.unlinkSync(p); return true; } catch (err2) { return gone(err2); }
    }
    return gone(err);
  }
}
function emptyDir(dirFd, isLeaf) {
  const stuck = new Set();
  drain(dirFd, BATCH, spend, stuck, (batch) => {
    let removed = 0;
    for (const e of batch) {
      if (removeEntry(dirFd, e)) removed++;
      else stuck.add(e.name);
    }
    return removed;
  }, isLeaf ? () => pause("listed") : undefined);
}
emptyDir(leafFd, true);
if (mode === "empty") {
  let left;
  try { left = readSome(leafFd, 1, new Set(), () => true).length; } catch { left = 1; }
  process.exit(exhausted ? 8 : left === 0 ? 0 : 4);
}
const pinned = fs.fstatSync(leafPin);
fs.closeSync(leafFd);
fs.closeSync(leafPin);
if (exhausted) process.exit(8);
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
process.exit(after === "gone" ? 0 : after === "refused" ? 3 : 4);
`;

/** Hard ceiling on the entries one measuring pass counts (see {@link measureRunCaches}). */
const MEASURE_MAX_ENTRIES = 500_000;

/**
 * Hard ceiling on the dirents one removal pass reads (see {@link rmHomeSubtree}). A planted
 * flood of names ends the pass here instead of holding the teardown; a real cache bigger
 * than this is left partly dropped, and the drop says so.
 */
const REMOVE_MAX_ENTRIES = 2_000_000;

/** Wall time a helper's own budget stops short of its kill timeout, so a pass that runs out
 *  of time exits with its budget verdict instead of being killed by `execFile`. */
const HELPER_SLACK_MS = 5_000;

/**
 * PRD #1809 D8: the measuring walk, as `node -e <script> <home> <rels-json> <max> <budgetMs>`.
 * Pins each cache subtree through descriptors exactly as {@link PINNED_SUBTREE_SCRIPT} does (a
 * symlinked or missing component counts zero), then sums allocated bytes (`blocks * 512`,
 * what the data volume actually loses) over the subtree with `lstat`, descending only
 * into directories it re-pins `O_NOFOLLOW`. Directories are streamed (one dirent at a time
 * from a buffered `Dir`, never a whole listing), every dirent read is counted, and the walk
 * stops at `<max>` dirents or `<budgetMs>` of wall time and says so (`truncated`), so the
 * reading is a lower bound; unreadable entries are skipped, which also only lowers it. Prints
 * one JSON line, including `entries`, the dirents it counted.
 *
 * The time budget is the helper's own because nothing else can stop it: under the uid split
 * the helper runs as `runner`/`runner-cmd` and the worker holds no CAP_KILL, so when the
 * worker's `execFile` timeout fires, its kill fails, the call rejects, and the helper walks on.
 * The caller sets `<budgetMs>` short of that timeout ({@link HELPER_SLACK_MS}).
 */
const MEASURE_CACHES_SCRIPT = `${PINNED_PRELUDE}
const [home, relsJson, maxArg, budgetArg] = process.argv.slice(1);
const max = Number(maxArg);
const budgetMs = budgetArg === undefined ? Infinity : Number(budgetArg);
if (!(budgetMs >= 0)) process.exit(6);
const deadline = Date.now() + budgetMs;
let cacheBytes = 0, entries = 0, truncated = false;
function walk(dirFd) {
  let dir;
  try { dir = fs.opendirSync(FD + dirFd); } catch { return; }
  try {
    let e;
    while (!truncated) {
      try { e = dir.readSync(); } catch { return; }
      if (e === null) return;
      if (entries >= max || Date.now() > deadline) { truncated = true; return; }
      entries++;
      const p = at(dirFd, e.name);
      let st;
      try { st = fs.lstatSync(p); } catch { continue; }
      cacheBytes += bytesOf(st);
      if (!st.isDirectory()) continue;
      let childFd;
      try { const pin = fs.openSync(p, PIN); try { childFd = reopen(pin); } finally { fs.closeSync(pin); } } catch { continue; }
      try { walk(childFd); } finally { fs.closeSync(childFd); }
    }
  } finally {
    dir.closeSync();
  }
}
for (const rel of JSON.parse(relsJson)) {
  if (truncated) break;
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
process.stdout.write(JSON.stringify({ cacheBytes, entries, truncated }) + "\\n");
`;

/** The uids a PRD #1809 helper runs as: under the split `runner`, then `runner-cmd` (Codex
 *  command roots are members of group `runner` and can write into a group-writable HOME),
 *  then, for a removal, `runner` again for what the second pass unblocked. Single-uid the
 *  wrapper is the identity, so the one pass runs as the worker itself. */
function agentWrappers(passes: "remove" | "measure"): CommandWrapper[] {
  if (!uidSplitActive()) return [runnerCommand];
  return passes === "remove" ? [runnerCommand, commandRootCommand, runnerCommand] : [runnerCommand, commandRootCommand];
}

/** Milliseconds a helper pass may run: {@link PURGE_TIMEOUT_MS}, clipped to what is left
 *  before `deadline` (epoch ms). Zero or less means the caller's deadline has passed. */
function passTimeout(deadline: number | undefined): number {
  return deadline === undefined ? PURGE_TIMEOUT_MS : Math.min(PURGE_TIMEOUT_MS, deadline - Date.now());
}

/** What `execFile` rejects with, for {@link helperFailure}. */
type HelperExecError = Error & { code?: unknown; killed?: boolean; signal?: NodeJS.Signals | null };

/**
 * A helper pass that produced no verdict, as a SHORT error. `execFile`'s own message is
 * "Command failed: <command line>", which quotes the whole `node -e <script>` (several KB of
 * script) into every log line that carries it (#1809 M1 review), so it is never passed on.
 */
function helperFailure(e: HelperExecError): Error {
  const how =
    typeof e.code === "number"
      ? `exited ${e.code}`
      : e.killed
        ? `timed out (killed by ${e.signal ?? "a signal"})`
        : e.signal
          ? `ended by ${e.signal}`
          : typeof e.code === "string"
            ? `could not run or was not waited for (${e.code})`
            : "timed out or could not run";
  return Object.assign(new Error(`agent-uid helper ${how}`), typeof e.code === "string" ? { code: e.code } : {});
}

/** Run one helper pass and resolve its exit status. Rejects, with a short {@link helperFailure},
 *  only when the helper could not be spawned or the worker stopped waiting for it (its
 *  timeout): no exit status is then a verdict. The timeout does NOT stop a helper under the uid
 *  split: the worker has no CAP_KILL over the agent uids, so its kill fails and the helper runs
 *  on until its own in-script budget (`budgetMs`, set {@link HELPER_SLACK_MS} short of the
 *  timeout) ends it. Single-uid the kill lands. The env is minimal and explicit, as in
 *  {@link purgeChildrenAsAgents}. */
async function runHelper(wrap: CommandWrapper, script: string, args: readonly string[], timeout: number): Promise<number> {
  const wrapped = wrap(process.execPath, ["-e", script, ...args]);
  return execFileAsync(wrapped.command, wrapped.args, {
    env: { PATH: "/usr/local/bin:/usr/bin:/bin" },
    timeout,
    maxBuffer: 64 * 1024,
  }).then(
    () => 0,
    (e: HelperExecError) => {
      if (typeof e.code !== "number") throw helperFailure(e);
      return e.code;
    },
  );
}

/** Test seams and the caller's deadline for {@link rmHomeSubtree}. */
export interface SubtreeRemovalOptions {
  /** The uids to run the passes as, in order (default: {@link agentWrappers}). */
  wrappers?: readonly CommandWrapper[];
  /** Dirents one pass may read before it stops (default {@link REMOVE_MAX_ENTRIES}). */
  maxEntries?: number;
  /** Epoch ms after which no pass starts and a running one is cut short. */
  deadline?: number;
}

/**
 * PRD #1809 D3: remove ONE named cache subtree (a {@link RUN_CACHE_SUBTREES} entry)
 * inside a run HOME, leaving the HOME root and every sibling intact. Resolves "removed"
 * or "absent" (nothing to drop); throws on a refusal, on an exhausted budget or deadline,
 * or when the subtree is still there.
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
 * Each pass is bounded by `maxEntries` dirents read and by its timeout (clipped to
 * `deadline`); a pass that runs out stops the removal (another uid would meet the same
 * tree), so the drop is best-effort and reports what it could not finish.
 */
export async function rmHomeSubtree(
  home: string,
  rel: string,
  opts: SubtreeRemovalOptions = {},
): Promise<"removed" | "absent"> {
  // The helper takes both as bare `node -e` arguments, so a relative or dash-leading
  // HOME, or an unlisted `rel`, is refused before anything runs.
  if (!path.isAbsolute(home)) throw new Error(`rmHomeSubtree: refusing non-absolute HOME ${home}`);
  if (!isRunCacheSubtree(rel)) {
    throw new Error(`rmHomeSubtree: refusing ${JSON.stringify(rel)}, not a listed cache subtree`);
  }
  const wrappers = opts.wrappers ?? agentWrappers("remove");
  const maxEntries = opts.maxEntries ?? REMOVE_MAX_ENTRIES;
  let last = "no pass ran";
  for (const wrap of wrappers) {
    const timeout = passTimeout(opts.deadline);
    if (timeout <= 0) {
      last = "the deadline passed before this pass";
      break;
    }
    const budgetMs = Math.max(0, timeout - HELPER_SLACK_MS);
    const code = await runHelper(
      wrap,
      PINNED_SUBTREE_SCRIPT,
      [home, rel, String(maxEntries), String(budgetMs), "remove", "-"],
      timeout,
    );
    if (code === 0) return "removed";
    if (code === 2) return "absent";
    // A symlink is a symlink whoever looks: a refusal is final, no later pass runs.
    if (code === 3) {
      throw Object.assign(new Error(`rmHomeSubtree: refusing ${rel} under ${home} (symlink or non-directory)`), {
        code: "ELOOP",
      });
    }
    if (code === 5) throw new Error(`rmHomeSubtree: refusing ${rel} under ${home} (no descriptor-pinned walk here)`);
    if (code === 8) {
      last = `the pass ran out of its budget (${maxEntries} entries or ${budgetMs} ms)`;
      break;
    }
    last =
      code === 4
        ? "entries remained"
        : code === 7
          ? "a component this uid cannot open"
          : `the helper itself failed (exit ${code})`;
  }
  throw Object.assign(new Error(`rmHomeSubtree: ${rel} is still present under ${home} (last pass: ${last})`), {
    code: "ENOTEMPTY",
  });
}

/** `O_PATH` (Linux; absent from `fs.constants`), as in {@link PINNED_PRELUDE}. */
const O_PATH = 0o10000000;
const PIN_FLAGS = O_PATH | constants.O_DIRECTORY | constants.O_NOFOLLOW;
const SELF_FD = "/proc/self/fd/";

/** One path component: no separator, not `.`/`..`, not dash-leading (it becomes a bare
 *  `node -e` argument). Run ids and `mkdtemp` names (`uzi-judge-XXXXXX`) all fit. */
const TREE_NAME_RE = /^[A-Za-z0-9_][A-Za-z0-9._-]*$/;

/** The worker itself as a helper's uid: the command unchanged. */
const asWorker: CommandWrapper = (command, args) => ({ command, args: [...args] });

/** Test seams and the caller's deadline for {@link rmTreePinned}. */
export interface PinnedTreeRemovalOptions {
  /** The uids to run the emptying passes as, in order (default: see {@link rmTreePinned}). */
  wrappers?: readonly CommandWrapper[];
  /** Dirents one pass may read before it stops (default {@link REMOVE_MAX_ENTRIES}). */
  maxEntries?: number;
  /** Epoch ms after which no pass starts and a running one is cut short. */
  deadline?: number;
  /** Whether the PRD #51 uid split is active (default: {@link uidSplitActive}). */
  splitActive?: boolean;
  /** The worker's uid for the root's owner check (default: `process.getuid`); a test seam. */
  getuid?: () => number | undefined;
}

/** `<dev>:<ino>` of a pinned descriptor: the identity the helper checks its own pin against. */
async function identityOf(pinFd: number): Promise<string> {
  const st = await fs.stat(SELF_FD + pinFd, { bigint: true });
  return `${st.dev}:${st.ino}`;
}

/**
 * PRD #1809 M3: remove the whole directory `<parent>/<name>` (a terminal run's HOME or
 * provision dir, a stranded model-pass HOME) without ever resolving a path inside it.
 * Resolves "removed", or "absent" when there was nothing there; throws on a refusal, an
 * exhausted budget or deadline, or anything left behind.
 *
 * {@link rmHomeTree} walks by path (`fs.rm`), so a same-uid process that swaps an
 * INTERMEDIATE directory for a symlink mid-walk redirects the deletion outside the tree
 * (an audit's racer deleted 82 files outside `agent-home` that way). The boot sweep and a
 * run's own teardown keep using it; the running disk reclaim cannot, because it deletes
 * while other runs' `runner`/`runner-cmd` processes are live on the same volume. So this
 * is {@link PINNED_SUBTREE_SCRIPT}'s walk:
 *
 *  1. The worker pins `parent`, then `name` inside it through its descriptor with
 *     `O_PATH | O_DIRECTORY | O_NOFOLLOW`, refusing a symlink, a non-directory, and a
 *     directory the worker does not own (as `openRootToRunnerGroup` does: none of those
 *     is a tree this worker created). Under the uid split it adds group `rwx` to the
 *     pinned root through its descriptor, so the `runner` helpers can traverse it.
 *  2. The helper empties the tree in `empty` mode, and refuses to start unless its own
 *     pin of `<parent>/<name>` is the inode the worker pinned: under the split as
 *     `runner`, `runner-cmd`, `runner`, then the worker (worker-owned leftovers), stopping
 *     at the first pass that leaves it empty; single-uid once, as the worker.
 *  3. The worker `rmdir`s the emptied root through its pinned parent, after checking that
 *     the name there still pins to the same inode.
 *
 * Linux-only, like every pinned walk here: without `/proc/self/fd` it refuses outright.
 */
export async function rmTreePinned(
  parent: string,
  name: string,
  opts: PinnedTreeRemovalOptions = {},
): Promise<"removed" | "absent"> {
  if (!path.isAbsolute(parent)) throw new Error(`rmTreePinned: refusing non-absolute parent ${parent}`);
  if (!TREE_NAME_RE.test(name)) throw new Error(`rmTreePinned: refusing ${JSON.stringify(name)}, not one path component`);
  if (process.platform !== "linux") throw new Error("rmTreePinned: refusing, no descriptor-pinned walk here");
  const split = opts.splitActive ?? uidSplitActive();
  const target = path.join(parent, name);
  let parentPin;
  try {
    parentPin = await fs.open(parent, PIN_FLAGS);
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code === "ENOENT") return "absent";
    throw err;
  }
  try {
    const at = `${SELF_FD}${parentPin.fd}/${name}`;
    let leafPin;
    try {
      leafPin = await fs.open(at, PIN_FLAGS);
    } catch (err) {
      const code = (err as NodeJS.ErrnoException).code;
      if (code === "ENOENT") return "absent";
      if (code === "ELOOP" || code === "ENOTDIR") {
        throw Object.assign(new Error(`rmTreePinned: refusing ${target} (symlink or non-directory)`), { code: "ELOOP" });
      }
      throw err;
    }
    try {
      const st = await fs.stat(SELF_FD + leafPin.fd);
      const uid = opts.getuid ? opts.getuid() : process.getuid?.();
      if (uid !== undefined && st.uid !== uid) {
        throw Object.assign(new Error(`rmTreePinned: ${target} is not owned by this worker (uid ${st.uid})`), {
          code: "EPERM",
        });
      }
      // chmod follows the magic link to the pinned inode, never a path.
      if (split) await fs.chmod(SELF_FD + leafPin.fd, (st.mode & 0o7777) | 0o770);
      const expect = await identityOf(leafPin.fd);
      const wrappers =
        opts.wrappers ?? (split ? [runnerCommand, commandRootCommand, runnerCommand, asWorker] : [asWorker]);
      const maxEntries = opts.maxEntries ?? REMOVE_MAX_ENTRIES;
      let emptied = false;
      let last = "no pass ran";
      for (const wrap of wrappers) {
        const timeout = passTimeout(opts.deadline);
        if (timeout <= 0) {
          last = "the deadline passed before this pass";
          break;
        }
        const budgetMs = Math.max(0, timeout - HELPER_SLACK_MS);
        const code = await runHelper(
          wrap,
          PINNED_SUBTREE_SCRIPT,
          [parent, name, String(maxEntries), String(budgetMs), "empty", expect],
          timeout,
        );
        if (code === 0) {
          emptied = true;
          break;
        }
        // The name no longer pins to our inode (swapped or gone): nothing more to do by name.
        if (code === 2 || code === 3) {
          last = code === 2 ? "the name vanished" : "the name no longer names the pinned tree";
          break;
        }
        if (code === 5) throw new Error(`rmTreePinned: refusing ${target} (no descriptor-pinned walk here)`);
        if (code === 8) {
          last = `the pass ran out of its budget (${maxEntries} entries or ${budgetMs} ms)`;
          break;
        }
        last =
          code === 4
            ? "entries remained"
            : code === 7
              ? "a component this uid cannot open"
              : `the helper itself failed (exit ${code})`;
      }
      if (emptied) {
        // Remove the root by name only while the name still pins to the emptied inode. A
        // swap between this check and the rmdir can at worst remove an empty directory.
        let current: string | undefined;
        try {
          const check = await fs.open(at, PIN_FLAGS);
          try {
            current = await identityOf(check.fd);
          } finally {
            await check.close().catch(() => undefined);
          }
        } catch {
          current = undefined;
        }
        if (current === expect) {
          try {
            await fs.rmdir(at);
          } catch (err) {
            if ((err as NodeJS.ErrnoException).code !== "ENOENT") throw err;
          }
          return "removed";
        }
        last = current === undefined ? "the name vanished" : "the name no longer names the pinned tree";
      }
      throw Object.assign(new Error(`rmTreePinned: ${target} was not removed (last pass: ${last})`), {
        code: "ENOTEMPTY",
      });
    } finally {
      await leafPin.close().catch(() => undefined);
    }
  } finally {
    await parentPin.close().catch(() => undefined);
  }
}

function isRunCacheSubtree(rel: string): rel is RunCacheSubtree {
  return (RUN_CACHE_SUBTREES as readonly string[]).includes(rel);
}

/** PRD #1809 D8: allocated bytes under a run HOME's {@link RUN_CACHE_SUBTREES}. */
export interface RunCacheBytes {
  cacheBytes: number;
  /** Dirents the kept pass counted (at most its `maxEntries`). */
  entries: number;
  /** A pass hit its entry ceiling, so `cacheBytes` is a lower bound. */
  truncated: boolean;
}

/** Test seams and the caller's deadline for {@link measureRunCaches}. */
export interface MeasureOptions {
  /** The uids to run the passes as, in order (default: {@link agentWrappers}). */
  wrappers?: readonly CommandWrapper[];
  /** Dirents one pass may count (default {@link MEASURE_MAX_ENTRIES}). */
  maxEntries?: number;
  /**
   * Epoch ms after which no pass starts. A running pass is given its own in-script budget
   * ending {@link HELPER_SLACK_MS} before the deadline, and the worker stops waiting for it at
   * the deadline; under the uid split it cannot kill it (see {@link runHelper}).
   */
  deadline?: number;
}

/**
 * PRD #1809 D8: measure the cache subtrees of a run HOME (only those, never the whole
 * HOME). Under the uid split the worker cannot list the agent's `0700` dirs, so the walk
 * runs as `runner` and as `runner-cmd` through the same wrappers the removal uses, and
 * the larger reading is kept: each uid sees a subset, so the result is a lower bound,
 * never an over-count from summing passes. That means a cache both uids can read is
 * walked twice; the cost is bounded by `maxEntries` per pass and counted against the
 * caller's `deadline`. Single-uid it runs once as the worker. Rejects only when no pass
 * produced a reading.
 */
export async function measureRunCaches(home: string, opts: MeasureOptions = {}): Promise<RunCacheBytes> {
  if (!path.isAbsolute(home)) throw new Error(`measureRunCaches: refusing non-absolute HOME ${home}`);
  const wrappers = opts.wrappers ?? agentWrappers("measure");
  const maxEntries = opts.maxEntries ?? MEASURE_MAX_ENTRIES;
  let best: RunCacheBytes | undefined;
  let lastErr: unknown;
  for (const wrap of wrappers) {
    const timeout = passTimeout(opts.deadline);
    if (timeout <= 0) {
      lastErr ??= new Error("measureRunCaches: the deadline passed before a pass could run");
      break;
    }
    const budgetMs = Math.max(0, timeout - HELPER_SLACK_MS);
    const wrapped = wrap(process.execPath, [
      "-e",
      MEASURE_CACHES_SCRIPT,
      home,
      JSON.stringify(RUN_CACHE_SUBTREES),
      String(maxEntries),
      String(budgetMs),
    ]);
    let stdout: string;
    try {
      ({ stdout } = await execFileAsync(wrapped.command, wrapped.args, {
        env: { PATH: "/usr/local/bin:/usr/bin:/bin" },
        timeout,
        maxBuffer: 64 * 1024,
      }));
    } catch (e) {
      lastErr = helperFailure(e as HelperExecError);
      continue;
    }
    try {
      const r = JSON.parse(stdout) as RunCacheBytes;
      best =
        best === undefined || r.cacheBytes > best.cacheBytes
          ? { ...r, truncated: (best?.truncated ?? false) || r.truncated }
          : { ...best, truncated: best.truncated || r.truncated };
    } catch (e) {
      lastErr = e;
    }
  }
  if (!best) throw lastErr ?? new Error("measureRunCaches: no measuring pass ran");
  return best;
}

/** PRD #1809 D4: the cache subtrees a between-turns trim evicts entries from (the module cache
 *  is never trimmed per entry: it goes whole, through {@link rmHomeSubtree}). */
const TRIMMABLE_CACHES = [".cache/go-build", ".npm/_cacache"] as const;
type TrimmableCache = (typeof TRIMMABLE_CACHES)[number];

/** Dirents one listing pass may read (bounds the helper's walk and its memory). */
const TRIM_LIST_MAX_ENTRIES = 400_000;
/** Units one listing pass may print, and so one trim may evict. A cache bigger than this is
 *  trimmed over several quiet points. */
const TRIM_LIST_MAX_UNITS = 200_000;
/** Cap on what the worker reads back from one helper pass. */
const TRIM_MAX_STDOUT = 48 * 1024 * 1024;

/**
 * The pieces the two PRD #1809 D4 trim scripts share, on top of {@link PINNED_PRELUDE} and
 * {@link STREAM_PRELUDE}: argument checks, the entry and wall-time budget (`spend`), buffered
 * output (`w`, printed once at exit), `lst` (an `lstat` through a pinned parent, never
 * following), `openChild` (a directory inside a pinned parent, pinned `O_NOFOLLOW` then reopened
 * for reading) and `pinRoot` (the cache root, pinned component by component from HOME, as
 * {@link PINNED_SUBTREE_SCRIPT} does: a symlinked component exits 3, a missing one 2).
 */
const TRIM_PRELUDE = `
const out = [];
const w = (line) => out.push(line);
const flush = () => { if (out.length) process.stdout.write(out.join("\\n") + "\\n"); };
let seen = 0, exhausted = false, deadline = 0, maxEntries = 0;
function budget(max, ms) {
  if (!(max >= 0) || !(ms >= 0)) process.exit(6);
  maxEntries = max;
  deadline = Date.now() + ms;
}
const spend = () => {
  if (exhausted) return false;
  if (seen >= maxEntries || Date.now() > deadline) { exhausted = true; return false; }
  seen++;
  return true;
};
const lst = (dirFd, name) => { try { return fs.lstatSync(at(dirFd, name), { bigint: true }); } catch { return undefined; } };
const bytesB = (st) => Number(st.blocks) * 512;
function openChild(dirFd, name) {
  const pin = fs.openSync(at(dirFd, name), PIN);
  try { return reopen(pin); } finally { fs.closeSync(pin); }
}
const badPart = (x) => x === "" || x === "." || x === "..";
function pinRoot(home, rel) {
  if (!home || !home.startsWith("/") || !rel || rel.split("/").some(badPart)) process.exit(6);
  let fd;
  try {
    fd = fs.openSync(home, PIN);
    for (const part of rel.split("/")) { const next = fs.openSync(at(fd, part), PIN); fs.closeSync(fd); fd = next; }
    return fd;
  } catch (e) {
    process.exit(e.code === "ENOENT" ? 2 : e.code === "ELOOP" || e.code === "ENOTDIR" ? 3 : 7);
  }
}
// Stream the directory read through dirFd, calling fn(name) for each entry, charged to spend().
function each(dirFd, fn) {
  let dir;
  try { dir = fs.opendirSync(FD + dirFd); } catch { return; }
  try {
    for (;;) {
      let e;
      try { e = dir.readSync(); } catch { return; }
      if (e === null || !spend()) return;
      fn(e.name);
    }
  } finally {
    dir.closeSync();
  }
}
`;

/**
 * PRD #1809 D4: the trim's LIST pass, as
 * `node -e <script> <home> <rel> <kind> <needBytes> <maxEntries> <maxUnits> <budgetMs>`. Read-only:
 * a no-follow `lstat` walk over pinned descriptors ({@link TRIM_PRELUDE}); a symlink is never
 * followed and never a unit. Prints tab-separated lines, then one `H\t<json>` summary line
 * (`truncated`: the entry or time budget ran out, so the listing is partial).
 *
 * `<kind>` `go` (`<rel>` is `.cache/go-build`): one unit is a regular file named `*-a` or `*-d`
 * in a top-level directory (Go's own trim selects entries by that suffix,
 * `cmd/go/internal/cache/cache.go`), or a directory there (Go's executable cache entries are
 * directories; its bytes are its whole tree's). `README`, `trim.txt` and `testexpire.txt`, and
 * every other top-level file, are never units. The units are sorted oldest-mtime first and only
 * the oldest prefix whose bytes reach `<needBytes>` is printed (at most `<maxUnits>`), as
 * `U\t<mtimeNs>\t<f|d>\t<bytes>\t<xx/name>`: under the uid split each uid prints its own oldest
 * prefix, and the oldest units covering the need across all uids are always inside the union.
 *
 * `<kind>` `npm` (`<rel>` is `.npm/_cacache`): every `index-v5` bucket file as
 * `B\t<mtimeNs>\t<bytes>\t<index-v5/...>\t<integrity tokens, space-separated>` (every
 * `sha<N>-<base64>` token anywhere in the bucket, so a torn or unparsed line still counts as a
 * reference); a bucket this uid could not read as `X\t<rel>`; every index directory it opened
 * as `D\t<rel>`, and one it could not, or a symlink or other non-file in the index, as
 * `E\t<rel>` (the worker trusts a listing only when every index directory and bucket was read by
 * some uid); and every `content-v2/<algo>/<xx>/<yy>/<rest>`
 * file as `C\t<mtimeNs>\t<bytes>\t<rel>`.
 *
 * Exit 0 (listed), 2 (the cache is absent), 3 (a symlinked component), 5, 6, 7 as the removal
 * script; a truncated listing still exits 0 and says so in its summary.
 */
const TRIM_LIST_SCRIPT = `${PINNED_PRELUDE}${TRIM_PRELUDE}
const [home, rel, kind, needArg, maxArg, maxUnitsArg, budgetArg] = process.argv.slice(1);
const need = Number(needArg);
const maxUnits = Number(maxUnitsArg);
if ((kind !== "go" && kind !== "npm") || !(need >= 0) || !(maxUnits >= 0)) process.exit(6);
budget(Number(maxArg), Number(budgetArg));
const rootPin = pinRoot(home, rel);
let root;
try { root = reopen(rootPin); } catch { process.exit(7); }
function treeBytes(dirFd) {
  let n = 0;
  each(dirFd, (name) => {
    const st = lst(dirFd, name);
    if (!st) return;
    n += bytesB(st);
    if (!st.isDirectory()) return;
    let fd;
    try { fd = openChild(dirFd, name); } catch { return; }
    try { n += treeBytes(fd); } finally { fs.closeSync(fd); }
  });
  return n;
}
if (kind === "go") {
  const KEEP = new Set(["README", "trim.txt", "testexpire.txt"]);
  const units = [];
  each(root, (top) => {
    if (KEEP.has(top)) return;
    const st = lst(root, top);
    if (!st || !st.isDirectory()) return;
    let sub;
    try { sub = openChild(root, top); } catch { return; }
    try {
      each(sub, (name) => {
        const cst = lst(sub, name);
        if (!cst) return;
        if (cst.isFile() && /-[ad]$/.test(name)) {
          units.push([cst.mtimeNs, "f", bytesB(cst), top + "/" + name]);
        } else if (cst.isDirectory()) {
          let fd;
          try { fd = openChild(sub, name); } catch { return; }
          try { units.push([cst.mtimeNs, "d", bytesB(cst) + treeBytes(fd), top + "/" + name]); } finally { fs.closeSync(fd); }
        }
      });
    } finally {
      fs.closeSync(sub);
    }
  });
  units.sort((a, b) => (a[0] < b[0] ? -1 : a[0] > b[0] ? 1 : 0));
  let sum = 0, printed = 0, total = 0;
  for (const u of units) total += u[2];
  for (const u of units) {
    if (sum >= need || printed >= maxUnits) break;
    w("U\\t" + u[0] + "\\t" + u[1] + "\\t" + u[2] + "\\t" + u[3]);
    sum += u[2];
    printed++;
  }
  w("H\\t" + JSON.stringify({ units: units.length, bytes: total, truncated: exhausted }));
} else {
  const INTEGRITY = /sha[0-9]+-[A-Za-z0-9+/=]+/g;
  const MAX_BUCKET = 8 * 1024 * 1024;
  let printed = 0;
  const unit = (line) => { if (printed >= maxUnits) { exhausted = true; return; } w(line); printed++; };
  function walkIndex(dirFd, relp, depth) {
    each(dirFd, (name) => {
      const st = lst(dirFd, name);
      if (!st) return;
      const r = relp + "/" + name;
      if (st.isDirectory()) {
        // Deeper than cacache writes (index-v5/<xx>/<yy>/<bucket>): not a layout this trusts.
        if (depth >= 3) { w("E\\t" + r); return; }
        let fd;
        try { fd = openChild(dirFd, name); } catch { w("E\\t" + r); return; }
        w("D\\t" + r);
        try { walkIndex(fd, r, depth + 1); } finally { fs.closeSync(fd); }
      } else if (st.isFile()) {
        let text;
        if (st.size <= MAX_BUCKET) {
          try {
            const fd = fs.openSync(at(dirFd, name), C.O_RDONLY | C.O_NOFOLLOW);
            try { text = fs.readFileSync(fd, "latin1"); } finally { fs.closeSync(fd); }
          } catch {}
        }
        if (text === undefined) { w("X\\t" + r); return; }
        const ints = [...new Set(text.match(INTEGRITY) || [])];
        unit("B\\t" + st.mtimeNs + "\\t" + bytesB(st) + "\\t" + r + "\\t" + ints.join(" "));
      } else {
        // A symlink (or anything else) in the index: never followed here, but npm would follow
        // it and read entries this listing cannot see, so the index is not complete.
        w("E\\t" + r);
      }
    });
  }
  function walkContent(dirFd, relp, depth) {
    each(dirFd, (name) => {
      const st = lst(dirFd, name);
      if (!st) return;
      const r = relp + "/" + name;
      if (st.isDirectory() && depth < 4) {
        let fd;
        try { fd = openChild(dirFd, name); } catch { return; }
        try { walkContent(fd, r, depth + 1); } finally { fs.closeSync(fd); }
      } else if (st.isFile() && depth === 4) {
        unit("C\\t" + st.mtimeNs + "\\t" + bytesB(st) + "\\t" + r);
      }
    });
  }
  const idxSt = lst(rootPin, "index-v5");
  if (idxSt && idxSt.isDirectory()) {
    let fd;
    try { fd = openChild(rootPin, "index-v5"); } catch { w("E\\tindex-v5"); }
    if (fd !== undefined) {
      w("D\\tindex-v5");
      try { walkIndex(fd, "index-v5", 1); } finally { fs.closeSync(fd); }
    }
  } else if (idxSt) {
    w("E\\tindex-v5");
  }
  const conSt = lst(rootPin, "content-v2");
  if (conSt && conSt.isDirectory()) {
    let fd;
    try { fd = openChild(rootPin, "content-v2"); } catch {}
    if (fd !== undefined) {
      try { walkContent(fd, "content-v2", 1); } finally { fs.closeSync(fd); }
    }
  }
  w("H\\t" + JSON.stringify({ truncated: exhausted }));
}
flush();
`;

/**
 * PRD #1809 D4: the trim's EVICT pass, as `node -e <script> <home> <rel> <maxEntries> <budgetMs>`
 * with the units to evict on stdin, one `<f|d>\t<mtimeNs>\t<relpath>` per line (relative to the
 * cache root). For each unit it pins the cache root and every directory component of the unit
 * `O_NOFOLLOW` through descriptors ({@link TRIM_PRELUDE}), then `lstat`s the unit through its
 * pinned parent and evicts it only when it is still the listed type (a regular file for `f`, a
 * directory for `d`, never a symlink) with the listed mtime, to the nanosecond: an entry written
 * or used since the listing is kept. A file is `unlink`ed; a directory is emptied through pinned
 * descriptors as in {@link PINNED_SUBTREE_SCRIPT} (a symlink inside it is unlinked, never
 * followed; a directory this uid owns gets owner `rwx`) and then `rmdir`ed. Prints `R\t<relpath>`
 * per evicted unit and one `H\t<json>` summary (`removed`, `kept`, `missing`, `failed`,
 * `exhausted`). Every unit and every dirent inside an evicted directory counts against
 * `<maxEntries>`; the pass stops there or at `<budgetMs>`. Exits as {@link TRIM_LIST_SCRIPT}.
 */
const TRIM_EVICT_SCRIPT = `${PINNED_PRELUDE}${STREAM_PRELUDE}${TRIM_PRELUDE}
const [home, rel, maxArg, budgetArg] = process.argv.slice(1);
budget(Number(maxArg), Number(budgetArg));
const rootPin = pinRoot(home, rel);
const uid = process.getuid();
function openOwned(pinFd) {
  const st = fs.fstatSync(pinFd);
  if (st.uid === uid && (st.mode & 0o700) !== 0o700) {
    try { fs.chmodSync(FD + pinFd, (st.mode & 0o7777) | 0o700); } catch {}
  }
  return reopen(pinFd);
}
const gone = (err) => err.code === "ENOENT";
function removeEntry(dirFd, e) {
  const p = at(dirFd, e.name);
  if (!e.isDir) {
    try { fs.unlinkSync(p); return true; } catch (err) { return gone(err); }
  }
  let pin;
  try { pin = fs.openSync(p, PIN); } catch (err) {
    if (err.code === "ELOOP" || err.code === "ENOTDIR") {
      try { fs.unlinkSync(p); return true; } catch (err2) { return gone(err2); }
    }
    return gone(err);
  }
  let fd;
  try { fd = openOwned(pin); } catch { return false; } finally { fs.closeSync(pin); }
  try { emptyDir(fd); } finally { fs.closeSync(fd); }
  if (exhausted) return false;
  try { fs.rmdirSync(p); return true; } catch (err) { return gone(err); }
}
function emptyDir(dirFd) {
  const stuck = new Set();
  drain(dirFd, 256, spend, stuck, (batch) => {
    let removed = 0;
    for (const e of batch) {
      if (removeEntry(dirFd, e)) removed++;
      else stuck.add(e.name);
    }
    return removed;
  });
}
const res = { removed: 0, kept: 0, missing: 0, failed: 0 };
const input = fs.readFileSync(0, "utf8");
for (const line of input.split("\\n")) {
  if (!line) continue;
  if (!spend()) break;
  const [t, mtime, r] = line.split("\\t");
  const parts = r ? r.split("/") : [];
  if ((t !== "f" && t !== "d") || !mtime || parts.length === 0 || parts.some(badPart)) { res.failed++; continue; }
  const held = [];
  let parent = rootPin;
  try {
    for (const part of parts.slice(0, -1)) { parent = fs.openSync(at(parent, part), PIN); held.push(parent); }
  } catch (e) {
    for (const fd of held) fs.closeSync(fd);
    if (gone(e)) res.missing++; else res.kept++;
    continue;
  }
  try {
    const name = parts[parts.length - 1];
    let st;
    try { st = fs.lstatSync(at(parent, name), { bigint: true }); } catch (e) {
      if (gone(e)) res.missing++; else res.failed++;
      continue;
    }
    if (!(t === "f" ? st.isFile() : st.isDirectory()) || String(st.mtimeNs) !== mtime) { res.kept++; continue; }
    if (removeEntry(parent, { name, isDir: t === "d" })) { res.removed++; w("R\\t" + r); }
    else res.failed++;
  } finally {
    for (const fd of held) fs.closeSync(fd);
  }
}
w("H\\t" + JSON.stringify({ ...res, exhausted }));
flush();
`;

/** One trim unit: a go-build entry, an npm index bucket, or an npm content file. */
export interface CacheUnit {
  /** Relative to the cache root (`.cache/go-build` or `.npm/_cacache`). */
  rel: string;
  /** The listed mtime in nanoseconds, as the helper printed it: eviction re-checks it exactly. */
  mtimeNs: string;
  bytes: number;
  type: "f" | "d";
  /** Indices of the wrappers (uids) whose listing saw it; eviction runs as those. */
  seenBy: number[];
}

/** An npm index bucket: a unit plus the integrity tokens its lines carry. */
interface NpmBucket extends CacheUnit {
  integrities: string[];
}

/** One `.npm/_cacache` listing, merged across uids. */
export interface NpmCacheListing {
  buckets: NpmBucket[];
  contents: CacheUnit[];
  /** Every index directory and bucket was read by some uid and no pass was truncated: only then
   *  may content be judged unreferenced. */
  complete: boolean;
}

/** Test seams and the caller's deadline for the trim helpers. */
export interface TrimHelperOptions {
  /** The uids to run the passes as, in order (default: {@link agentWrappers}). */
  wrappers?: readonly CommandWrapper[];
  /** Dirents one pass may read (default {@link TRIM_LIST_MAX_ENTRIES}). */
  maxEntries?: number;
  /** Units one listing pass may print (default {@link TRIM_LIST_MAX_UNITS}). */
  maxUnits?: number;
  /** Epoch ms after which no pass starts; a running one stops at its own budget. */
  deadline?: number;
}

/** Spawn one trim helper pass with `input` on stdin; resolve its exit status and stdout. The
 *  deadline and the no-kill caveat are {@link runHelper}'s; the stdout read back is capped. */
async function runTrimHelper(
  wrap: CommandWrapper,
  script: string,
  args: readonly string[],
  timeout: number,
  input = "",
): Promise<{ code: number; stdout: string }> {
  const wrapped = wrap(process.execPath, ["-e", script, ...args]);
  return await new Promise((resolve, reject) => {
    let child: ChildProcess;
    try {
      child = spawn(wrapped.command, wrapped.args, {
        env: { PATH: "/usr/local/bin:/usr/bin:/bin" },
        stdio: ["pipe", "pipe", "ignore"],
      });
    } catch (e) {
      reject(helperFailure(e as HelperExecError));
      return;
    }
    const chunks: Buffer[] = [];
    let size = 0;
    let overflow = false;
    let settled = false;
    const settle = (fn: () => void): void => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      fn();
    };
    const timer = setTimeout(() => {
      child.kill("SIGKILL");
      settle(() => reject(helperFailure({ name: "Error", message: "", killed: true, signal: "SIGKILL" })));
    }, timeout);
    child.stdout?.on("data", (b: Buffer) => {
      size += b.length;
      if (size > TRIM_MAX_STDOUT) overflow = true;
      if (!overflow) chunks.push(b);
    });
    child.on("error", (e) => settle(() => reject(helperFailure(e as HelperExecError))));
    child.on("close", (code, signal) =>
      settle(() => {
        if (overflow) reject(new Error(`agent-uid helper printed more than ${TRIM_MAX_STDOUT} bytes`));
        else if (code === null) reject(helperFailure({ name: "Error", message: "", signal }));
        else resolve({ code, stdout: Buffer.concat(chunks).toString("utf8") });
      }),
    );
    // The helper may exit before reading its input (a refused root): never an unhandled EPIPE.
    child.stdin?.on("error", () => undefined);
    child.stdin?.end(input);
  });
}

/** The helper's `H` summary line, or undefined when it printed none. */
function summaryOf(stdout: string): Record<string, unknown> | undefined {
  for (const line of stdout.split("\n")) {
    if (line.startsWith("H\t")) return JSON.parse(line.slice(2)) as Record<string, unknown>;
  }
  return undefined;
}

function assertTrimmable(home: string, rel: string, caller: string): asserts rel is TrimmableCache {
  if (!path.isAbsolute(home)) throw new Error(`${caller}: refusing non-absolute HOME ${home}`);
  if (!(TRIMMABLE_CACHES as readonly string[]).includes(rel)) {
    throw new Error(`${caller}: refusing ${JSON.stringify(rel)}, not a trimmable cache`);
  }
}

/**
 * Run the LIST pass over `rel` as each wrapper and hand each pass's stdout to `onPass`. A pass
 * that finds the cache absent (2) or refuses a symlinked component (3) contributes nothing; any
 * other non-zero status, a failed helper or a missing summary makes the result partial.
 */
async function listPasses(
  home: string,
  rel: TrimmableCache,
  kind: "go" | "npm",
  needBytes: number,
  opts: TrimHelperOptions,
  onPass: (stdout: string, index: number) => void,
): Promise<{ partial: boolean }> {
  const wrappers = opts.wrappers ?? agentWrappers("measure");
  let partial = false;
  for (const [index, wrap] of wrappers.entries()) {
    const timeout = passTimeout(opts.deadline);
    if (timeout <= 0) {
      partial = true;
      break;
    }
    const budgetMs = Math.max(0, timeout - HELPER_SLACK_MS);
    const args = [
      home,
      rel,
      kind,
      String(Math.max(0, Math.ceil(needBytes))),
      String(opts.maxEntries ?? TRIM_LIST_MAX_ENTRIES),
      String(opts.maxUnits ?? TRIM_LIST_MAX_UNITS),
      String(budgetMs),
    ];
    let result;
    try {
      result = await runTrimHelper(wrap, TRIM_LIST_SCRIPT, args, timeout);
    } catch {
      partial = true;
      continue;
    }
    if (result.code === 2 || result.code === 3) continue;
    const summary = result.code === 0 ? summaryOf(result.stdout) : undefined;
    if (!summary) {
      partial = true;
      continue;
    }
    if (summary.truncated === true) partial = true;
    onPass(result.stdout, index);
  }
  return { partial };
}

/** Merge one unit into `byRel`, recording which uid saw it. */
function mergeUnit<T extends CacheUnit>(byRel: Map<string, T>, unit: T, index: number): void {
  const prior = byRel.get(unit.rel);
  if (prior) {
    if (!prior.seenBy.includes(index)) prior.seenBy.push(index);
  } else {
    byRel.set(unit.rel, unit);
  }
}

const byMtime = (a: CacheUnit, b: CacheUnit): number => {
  const x = BigInt(a.mtimeNs);
  const y = BigInt(b.mtimeNs);
  return x < y ? -1 : x > y ? 1 : 0;
};

/**
 * PRD #1809 D4: list `.cache/go-build`'s oldest units (see {@link TRIM_LIST_SCRIPT}, kind `go`)
 * whose bytes reach `needBytes`, merged across the agent uids and sorted oldest-mtime first.
 * `truncated` when some pass ran out of budget or failed: the units are then the oldest of what
 * was seen, which is still safe to evict, just not globally oldest.
 */
export async function listGoBuildUnits(
  home: string,
  needBytes: number,
  opts: TrimHelperOptions = {},
): Promise<{ units: CacheUnit[]; truncated: boolean }> {
  const rel = ".cache/go-build";
  assertTrimmable(home, rel, "listGoBuildUnits");
  const byRel = new Map<string, CacheUnit>();
  const { partial } = await listPasses(home, rel, "go", needBytes, opts, (stdout, index) => {
    for (const line of stdout.split("\n")) {
      const f = line.split("\t");
      if (f[0] !== "U" || f.length !== 5) continue;
      mergeUnit(byRel, { mtimeNs: f[1]!, type: f[2] === "d" ? "d" : "f", bytes: Number(f[3]), rel: f[4]!, seenBy: [index] }, index);
    }
  });
  return { units: [...byRel.values()].sort(byMtime), truncated: partial };
}

/**
 * PRD #1809 D4: list `.npm/_cacache`'s index buckets (with the integrity tokens each references)
 * and content files (see {@link TRIM_LIST_SCRIPT}, kind `npm`), merged across the agent uids.
 * `complete` only when no pass was partial and every index directory and bucket some uid could
 * not read was read by another: content may be judged unreferenced only against a complete index.
 */
export async function listNpmCache(home: string, opts: TrimHelperOptions = {}): Promise<NpmCacheListing> {
  const rel = ".npm/_cacache";
  assertTrimmable(home, rel, "listNpmCache");
  const buckets = new Map<string, NpmBucket>();
  const contents = new Map<string, CacheUnit>();
  const opened = new Set<string>();
  const unreadable = new Set<string>();
  const { partial } = await listPasses(home, rel, "npm", 0, opts, (stdout, index) => {
    for (const line of stdout.split("\n")) {
      const f = line.split("\t");
      if (f[0] === "B" && f.length === 5) {
        const integrities = f[4] ? f[4].split(" ").filter(Boolean) : [];
        mergeUnit(buckets, { mtimeNs: f[1]!, bytes: Number(f[2]), rel: f[3]!, type: "f", seenBy: [index], integrities }, index);
      } else if (f[0] === "C" && f.length === 4) {
        mergeUnit(contents, { mtimeNs: f[1]!, bytes: Number(f[2]), rel: f[3]!, type: "f", seenBy: [index] }, index);
      } else if (f[0] === "D" && f.length === 2) {
        opened.add(f[1]!);
      } else if ((f[0] === "E" || f[0] === "X") && f.length === 2) {
        unreadable.add(f[1]!);
      }
    }
  });
  const unresolved = [...unreadable].some((r) => !opened.has(r) && !buckets.has(r));
  return {
    buckets: [...buckets.values()].sort(byMtime),
    contents: [...contents.values()],
    complete: !partial && !unresolved,
  };
}

/**
 * PRD #1809 D4: evict `units` (relative to the cache root `rel`) through the EVICT pass (see
 * {@link TRIM_EVICT_SCRIPT}): each uid gets only the units its own listing saw and that no
 * earlier uid removed. Resolves the set of units actually removed; a unit whose mtime or type
 * changed since the listing, or that is gone, is simply not in it. Never throws for a pass that
 * failed: what it did not remove stays.
 */
export async function evictCacheUnits(
  home: string,
  rel: string,
  units: readonly CacheUnit[],
  opts: TrimHelperOptions = {},
): Promise<{ removed: Set<string>; kept: number; failed: number }> {
  assertTrimmable(home, rel, "evictCacheUnits");
  const wrappers = opts.wrappers ?? agentWrappers("measure");
  const removed = new Set<string>();
  let kept = 0;
  let failed = 0;
  for (const [index, wrap] of wrappers.entries()) {
    const mine = units.filter((u) => u.seenBy.includes(index) && !removed.has(u.rel));
    if (mine.length === 0) continue;
    const timeout = passTimeout(opts.deadline);
    if (timeout <= 0) break;
    const budgetMs = Math.max(0, timeout - HELPER_SLACK_MS);
    // Each unit is one entry, plus what an evicted directory holds.
    const maxEntries = opts.maxEntries ?? REMOVE_MAX_ENTRIES;
    const input = mine.map((u) => `${u.type}\t${u.mtimeNs}\t${u.rel}`).join("\n") + "\n";
    let result;
    try {
      result = await runTrimHelper(wrap, TRIM_EVICT_SCRIPT, [home, rel, String(maxEntries), String(budgetMs)], timeout, input);
    } catch {
      continue;
    }
    for (const line of result.stdout.split("\n")) {
      if (line.startsWith("R\t")) removed.add(line.slice(2));
    }
    const summary = summaryOf(result.stdout);
    kept = typeof summary?.kept === "number" ? summary.kept : kept;
    failed = typeof summary?.failed === "number" ? summary.failed : failed;
  }
  return { removed, kept, failed };
}
