import fs from "node:fs/promises";
import { constants } from "node:fs";
import { execFile } from "node:child_process";
import path from "node:path";
import { promisify } from "node:util";
import { runnerCommand, uidSplitActive } from "./runner-uid.js";

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
 *     root's CHILDREN as `runner` through the existing cap-clearing `setpriv`
 *     wrapper ({@link runnerCommand}). This is not an escalation: the helper holds
 *     exactly the privileges the agent that wrote those files already had.
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
  await deps.purgeChildrenAsRunner(target).catch(() => undefined);
  await deps.removeTree(target);
}

/** Seams for {@link rmHomeTree}, injected by tests. */
export interface HomeRemovalDeps {
  splitActive: boolean;
  removeTree: (target: string) => Promise<void>;
  purgeChildrenAsRunner: (target: string) => Promise<void>;
}

function defaultHomeRemovalDeps(env: NodeJS.ProcessEnv = process.env): HomeRemovalDeps {
  return {
    splitActive: uidSplitActive(env),
    removeTree: rmTreeForce,
    purgeChildrenAsRunner: purgeChildrenAsRunner,
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
 * The script the `runner` helper runs, as `node -e <script> <target>`. It walks the
 * root's children adding owner `rwx` to directories (the `0555` Go module cache),
 * then removes each child. Both steps refuse to follow symlinks: the walk opens with
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

/** Hard ceiling on the helper, so a pathological tree cannot hang a run's teardown. */
const PURGE_TIMEOUT_MS = 120_000;

/**
 * Run {@link PURGE_CHILDREN_SCRIPT} as `runner` (or directly single-uid, where it is
 * the same uid and only reached from tests). The env is minimal and explicit: the
 * helper needs no credential, and the worker's own env (PAT, join token) must not
 * reach a runner process.
 */
async function purgeChildrenAsRunner(target: string): Promise<void> {
  const wrapped = runnerCommand(process.execPath, ["-e", PURGE_CHILDREN_SCRIPT, target]);
  await execFileAsync(wrapped.command, wrapped.args, {
    env: { PATH: "/usr/local/bin:/usr/bin:/bin" },
    timeout: PURGE_TIMEOUT_MS,
    maxBuffer: 64 * 1024,
  });
}
