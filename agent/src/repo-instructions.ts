// PRD #246 M2: repo-borne instructions, opt-in (repo.claudemd_enabled) and default off.
//
// Trust model (PRD #246 §Solution): the clone's CLAUDE.md is part of the config
// class the `settingSources: []` isolation exists to block. When — and only when —
// the repo owner has vouched for the repo's review discipline, uzi reads the ROOT
// CLAUDE.md ONLY, through its own controlled channel (never the SDK's project
// loader), so `settingSources: []` never loosens. Nothing else is read: no nested
// `**/CLAUDE.md`, no `CLAUDE.local.md`, no `.claude/` config. A symlinked CLAUDE.md
// IS followed, but ONLY when its fully-resolved real path is a regular file that
// stays INSIDE the clone tree AND is not under the clone's `.git/` dir (the common
// `CLAUDE.md -> AGENTS.md` convention, this repo's own layout); an escaping, broken,
// looping, non-file, or `.git/`-internal symlink is never read. The containment check
// enforces exactly one invariant: "a hostile repo cannot redirect the read outside
// its own tree." The file is size-capped, and line-leading `@`-import lines are
// stripped so the model is not induced to Read arbitrary files.
//
// PROVENANCE — a followed target is ANY in-tree regular file, NOT necessarily a
// human-authored one: the symlink can point at an untracked, recovered, or build-
// generated file, so this is a genuine widening of the automatically-injected source
// versus the prior "one root regular file only" (the runtime frame in prompt.ts states
// the content's origin is UNVERIFIED, not that it was human-authored). What bounds it is
// that the target stays inside the vouched-for clone (a hostile repo could put the same
// bytes in CLAUDE.md directly), the content is framed DOWNSTREAM
// (buildRepoInstructionsContext) as a nonce-fenced UNTRUSTED/ADVISORY block, lead-only,
// and — see readRepoInstructions and the sdk-executor call site — the read happens BEFORE
// the same-run dependency install writes into the clone and BEFORE any agent turn runs
// repo-authored code. That ordering removes the same-run package-manager writer from the
// resolve→open window; the file descriptor then pins the bytes after open, and O_NOFOLLOW
// rejects a FINAL-component symlink swap. It is NOT total: a PARENT-component swap before
// open, and documented cross-run races (with WORKER_MAX_CONCURRENT_RUNS > 1 a sibling
// run's Bash can write another run's worktree — docs/worker-setup.md), are residual.
//
// The safety of this feature is guardrails + human review + these STRUCTURAL
// transforms + the advisory framing — NOT content/prose sanitization, which is
// trivially bypassed and manufactures false confidence (PRD open question 3).

import fs from "node:fs/promises";
import type { FileHandle } from "node:fs/promises";
import { constants as fsConstants } from "node:fs";
import path from "node:path";

/** Body cap for the repo's root CLAUDE.md, matching REPO_AGENT_MAX_BYTES
 *  (repoagents.ts) and the skill body cap. Larger ⇒ dropped as `too_large`. */
export const REPO_INSTRUCTIONS_MAX_BYTES = 64 * 1024;

/** The repo's root instructions file inside the clone. ROOT ONLY — never a nested
 *  CLAUDE.md below root, never `CLAUDE.local.md`. */
export function repoInstructionsPath(clonePath: string): string {
  return path.join(clonePath, "CLAUDE.md");
}

/** Why a repo's CLAUDE.md was not injected: no file, over the size cap (raw OR
 *  post-sanitization), an UNSAFE symlink (escapes the clone tree, is broken, loops,
 *  or resolves to a non-file) or a non-regular file — never read (`symlinked`), or a
 *  read failure (e.g. EACCES, transient FS error). Trace-logged by the caller. */
type RepoInstructionsDrop = "absent" | "too_large" | "symlinked" | "read_error";

/** Visible marker left in place of a stripped line-leading `@`-import, so the
 *  structural transform is auditable in the injected text. */
const IMPORT_STRIPPED_MARKER = "<!-- uzi: @-import stripped -->";

/**
 * Resolve the root CLAUDE.md to the canonical, in-tree path to read.
 *
 * Returns the `readPath` (the fully-resolved real path, or the plain root path for a
 * non-symlink), or a drop reason. The regular-file check and the size cap are NOT done
 * here: they are taken from an `fstat` on the OPEN handle in `readRepoInstructions`, so
 * the same inode is validated and read with no path-stat→read re-resolution gap.
 *
 * - `lstat` failure (ENOENT) ⇒ `{ dropped: "absent" }`.
 * - A symlink is FOLLOWED, but only when it is safe:
 *   1. `realpath` BOTH the clone and the link — this walks the whole chain and throws
 *      on `ELOOP`/a broken target. Any throw ⇒ `{ dropped: "symlinked" }`. Both sides
 *      are resolved (a raw `clonePath` may itself be a symlink), then compared.
 *   2. Containment via `path.relative(realClone, realTarget)` — an empty/`..`/`../…`
 *      or absolute relative path means the target escapes the tree ⇒ `symlinked`.
 *      `path.relative` is used, not a `startsWith` string prefix, which a sibling like
 *      `/clone-evil` would defeat.
 *   3. Refuse a target under the clone's `.git/` dir (first path segment `.git`) —
 *      it is in-tree but never legitimate instruction content ⇒ `symlinked`.
 *   The check enforces the "cannot redirect the read outside its own tree" invariant
 *   (plus the `.git/` carve-out). Whether the resolved target is a regular file, and
 *   its size, are decided by the reader's fstat, not a stat here.
 * - A non-symlink that is not a regular file (a directory/socket named CLAUDE.md) ⇒
 *   `{ dropped: "symlinked" }` (the cheap lstat classification; the reader's fstat is
 *   the authoritative check for the symlink branch).
 * - A plain regular file ⇒ read it directly.
 */
async function resolveReadPath(
  clonePath: string,
  filePath: string,
): Promise<{ readPath: string } | { dropped: RepoInstructionsDrop }> {
  let stat;
  try {
    stat = await fs.lstat(filePath);
  } catch {
    return { dropped: "absent" };
  }

  if (stat.isSymbolicLink()) {
    let realClone: string;
    let realTarget: string;
    try {
      // realpath follows the WHOLE chain and throws on ELOOP / a broken link. Resolve
      // BOTH sides — the clone root itself may be reached via a symlink.
      realClone = await fs.realpath(clonePath);
      realTarget = await fs.realpath(filePath);
    } catch {
      return { dropped: "symlinked" };
    }
    // Containment: use path.relative, NOT realTarget.startsWith(realClone) — the
    // string-prefix form is fooled by a sibling like `/clone-evil`.
    const rel = path.relative(realClone, realTarget);
    if (rel === "" || rel === ".." || rel.startsWith(".." + path.sep) || path.isAbsolute(rel)) {
      return { dropped: "symlinked" };
    }
    // In-tree but never instruction content: refuse a target under the clone's `.git/`
    // dir (git internals/hooks/config), so a symlink cannot inject repo metadata.
    if (rel.split(path.sep)[0] === ".git") {
      return { dropped: "symlinked" };
    }
    // The regular-file check happens on the OPEN handle in the reader (a symlink to a
    // directory/device is refused there), so the same inode is validated and read.
    return { readPath: realTarget };
  }

  // A directory/socket named CLAUDE.md is never read.
  if (!stat.isFile()) return { dropped: "symlinked" };
  return { readPath: filePath };
}

/**
 * Read + structurally sanitize the clone's ROOT CLAUDE.md.
 *
 * - `lstat` the root path; a read error (ENOENT) ⇒ `{ dropped: "absent" }`.
 * - A symlinked CLAUDE.md is FOLLOWED only when its fully-resolved real path is a
 *   regular file that stays INSIDE the clone tree and is not under `.git/` (the
 *   `CLAUDE.md -> AGENTS.md` convention). An escaping/broken/looping/non-file/`.git/`
 *   symlink ⇒ `{ dropped: "symlinked" }`, and so is a non-symlink that is not a regular
 *   file (a directory). The containment check preserves the invariant that a hostile
 *   repo cannot redirect the read outside its own tree. See `resolveReadPath` for the
 *   exact rules.
 * - Over `maxBytes` (the RESOLVED target's fstat size) ⇒ `{ dropped: "too_large" }`.
 * - An `open`/`fstat`/`read` failure (e.g. EACCES on a mode-000 file, a transient FS
 *   error, a TOCTOU delete) ⇒ `{ dropped: "read_error" }`. The read is guarded IN the
 *   reader so BOTH callers (the SDK production path and the stub) treat it as
 *   non-fatal — a throw here must never abort run setup.
 * - Otherwise read UTF-8 from the OPEN handle, normalize CRLF→LF, and strip
 *   line-leading `@`-import lines (replaced with a visible marker). An inline `@ref`
 *   mid-line may pass through — that is acceptable and documented: the SDK loader
 *   never resolves it because WE read the file, so a surviving inline ref is inert;
 *   the strip is defense-in-depth against a model-induced `Read`, not a load-bearing
 *   control.
 * - The `@`-import marker is longer than the `@…` line it replaces, so a crafted
 *   file UNDER the raw cap can amplify OVER it after substitution. The sanitized
 *   size is re-checked against `maxBytes` ⇒ `{ dropped: "too_large" }`, so the
 *   INJECTED text can never exceed the cap regardless of marker amplification.
 *
 * TOCTOU (mitigated, not eliminated): the resolved path is `open`ed ONCE with O_NOFOLLOW
 * and the fstat (regular-file check + size) and the read both go through that one handle,
 * so the bytes read are pinned to the opened inode and a final-component symlink swap
 * before/at open is rejected. The residual window is resolve→open. The CALL SITE narrows
 * it by reading BEFORE startDepsInstall, which removes the same-run package-manager writer
 * (the install runs --ignore-scripts, so it is the package manager's own writes, not repo
 * postinstall code). Two residuals remain and are NOT closed here: a PARENT-component swap
 * before open (O_NOFOLLOW guards only the final component), and documented cross-run races
 * (a sibling run's Bash can write another run's worktree — docs/worker-setup.md).
 *
 * An empty/whitespace-only result is NOT a drop reason — it is returned as-is and
 * the caller (buildRepoInstructionsContext) decides to inject nothing.
 */
export async function readRepoInstructions(
  clonePath: string,
  maxBytes = REPO_INSTRUCTIONS_MAX_BYTES,
): Promise<{ text: string } | { dropped: RepoInstructionsDrop }> {
  const filePath = repoInstructionsPath(clonePath);

  const resolved = await resolveReadPath(clonePath, filePath);
  if ("dropped" in resolved) return resolved;
  const { readPath } = resolved;

  let sanitized: string;
  let fh: FileHandle | undefined = undefined;
  try {
    // Open ONCE and do the fstat (regular-file check + size) AND the read through the
    // same handle, so both operate on the same opened fd — a swap of readPath AFTER this
    // open cannot redirect the read. Reading BEFORE the same-run dependency install (the
    // sdk-executor call site) only NARROWS the resolve→open window by removing that
    // writer; it does not fully close it (a parent-component swap before open, and
    // cross-run writers, remain — see this function's doc comment).
    //
    // O_NONBLOCK: a symlink resolved to a FIFO/device would otherwise BLOCK in open()
    // waiting for a writer, hanging run setup; with it, open returns and the fstat below
    // rejects the non-regular file. O_NOFOLLOW: readPath is already realpath-canonical
    // (no symlink components), so this only bites if the final component was swapped to a
    // symlink after realpath — then open fails ELOOP and we drop `read_error` rather than
    // follow it. Both are inert for a plain regular file.
    fh = await fs.open(
      readPath,
      fsConstants.O_RDONLY | fsConstants.O_NONBLOCK | fsConstants.O_NOFOLLOW,
    );
    const st = await fh.stat();
    // A resolved target that is not a regular file (a symlink to a directory/device/FIFO,
    // or a directory named CLAUDE.md) is never read.
    if (!st.isFile()) return { dropped: "symlinked" };
    // Bound the RESOLVED target's size before reading it.
    if (st.size > maxBytes) return { dropped: "too_large" };

    const raw = await fh.readFile("utf8");
    const normalized = raw.replace(/\r\n/g, "\n").replace(/\r/g, "\n");
    // Strip LINE-LEADING `@<path>` import lines (e.g. `@./foo.md`, `@docs/x.md`,
    // `@~/y`). Scoped to the leading token only; an inline `@ref` mid-line is left.
    sanitized = normalized
      .split("\n")
      .map((line) => (/^\s*@\S+/.test(line) ? IMPORT_STRIPPED_MARKER : line))
      .join("\n");
  } catch {
    return { dropped: "read_error" };
  } finally {
    // A close() failure is NON-FATAL and must never override the return: an unguarded
    // `await fh.close()` that rejects in a finally replaces the try's return value with a
    // throw, breaking the reader's "always returns a drop or text, never throws" contract
    // the callers rely on to keep run setup alive. Swallow it (the bytes are already read,
    // or a drop is already decided).
    try {
      await fh?.close();
    } catch {
      /* non-fatal */
    }
  }

  // Re-bound the SANITIZED size: the marker is longer than the `@…` line it
  // replaces, so a file under the raw cap can amplify over it. Enforce the cap on
  // what actually gets injected so marker amplification cannot defeat it.
  if (Buffer.byteLength(sanitized, "utf8") > maxBytes) return { dropped: "too_large" };

  return { text: sanitized };
}
