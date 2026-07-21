import fs from "node:fs/promises";
import { constants } from "node:fs";
import path from "node:path";

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
