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
// its own tree." A followed target is always a regular file the lead can already read
// through its own tools, and it is framed DOWNSTREAM (buildRepoInstructionsContext) as
// a nonce-fenced UNTRUSTED/ADVISORY block, lead-only — so following an in-tree symlink
// grants no capability the opt-in did not already imply. The file is size-capped, and
// line-leading `@`-import lines are stripped so the model is not induced to Read
// arbitrary files.
//
// The safety of this feature is guardrails + human review + these STRUCTURAL
// transforms + the advisory framing — NOT content/prose sanitization, which is
// trivially bypassed and manufactures false confidence (PRD open question 3).

import fs from "node:fs/promises";
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
 * Resolve the root CLAUDE.md to the real, in-tree, regular file to read.
 *
 * Returns the canonical `readPath` (the path the body is read FROM — reading the
 * resolved path, not the raw link, is what closes the symlink-swap TOCTOU) plus its
 * `size` for the pre-read cap, or a drop reason:
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
 *   4. `stat` the resolved target; a throw ⇒ `{ dropped: "read_error" }`; a non-file
 *      (a symlink to a directory/device) ⇒ `{ dropped: "symlinked" }`.
 *   The check enforces the "cannot redirect the read outside its own tree" invariant
 *   (plus the `.git/` carve-out); a followed target is a regular file the lead can
 *   already read via its own tools, injected only as nonce-fenced advisory context.
 * - A non-symlink that is not a regular file (a directory/socket named CLAUDE.md) ⇒
 *   `{ dropped: "symlinked" }`.
 * - A plain regular file ⇒ read it directly.
 */
async function resolveReadPath(
  clonePath: string,
  filePath: string,
): Promise<{ readPath: string; size: number } | { dropped: RepoInstructionsDrop }> {
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
    let targetStat;
    try {
      targetStat = await fs.stat(realTarget);
    } catch {
      return { dropped: "read_error" };
    }
    // A symlink to a directory/device is still refused.
    if (!targetStat.isFile()) return { dropped: "symlinked" };
    return { readPath: realTarget, size: targetStat.size };
  }

  // A directory/socket named CLAUDE.md is never read.
  if (!stat.isFile()) return { dropped: "symlinked" };
  return { readPath: filePath, size: stat.size };
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
 *   repo cannot redirect the read outside its own tree; a followed target is a regular
 *   file the lead can already read via its own tools. See `resolveReadPath` for the
 *   exact rules.
 * - Over `maxBytes` (the RESOLVED target's size) ⇒ `{ dropped: "too_large" }`.
 * - A `readFile` failure after the lstat/stat passed (e.g. EACCES on a mode-000 file, a
 *   transient FS error, a TOCTOU delete) ⇒ `{ dropped: "read_error" }`. The read is
 *   guarded IN the reader so BOTH callers (the SDK production path and the stub) treat
 *   it as non-fatal — a throw here must never abort run setup.
 * - Otherwise read UTF-8 from the resolved canonical path, normalize CRLF→LF, and
 *   strip line-leading `@`-import lines (replaced with a visible marker). An inline
 *   `@ref` mid-line may pass through — that is acceptable and documented: the SDK
 *   loader never resolves it because WE read the file, so a surviving inline ref is
 *   inert; the strip is defense-in-depth against a model-induced `Read`, not a
 *   load-bearing control.
 * - The `@`-import marker is longer than the `@…` line it replaces, so a crafted
 *   file UNDER the raw cap can amplify OVER it after substitution. The sanitized
 *   size is re-checked against `maxBytes` ⇒ `{ dropped: "too_large" }`, so the
 *   INJECTED text can never exceed the cap regardless of marker amplification.
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
  const { readPath, size } = resolved;

  // Bound the RESOLVED target's size before reading it.
  if (size > maxBytes) return { dropped: "too_large" };

  let sanitized: string;
  try {
    // Read the resolved canonical path (not the raw link) — this closes the
    // symlink-swap TOCTOU. The read stays guarded: an lstat/stat can pass while the
    // read still fails (e.g. EACCES, a transient FS error, a TOCTOU delete).
    const raw = await fs.readFile(readPath, "utf8");
    const normalized = raw.replace(/\r\n/g, "\n").replace(/\r/g, "\n");
    // Strip LINE-LEADING `@<path>` import lines (e.g. `@./foo.md`, `@docs/x.md`,
    // `@~/y`). Scoped to the leading token only; an inline `@ref` mid-line is left.
    sanitized = normalized
      .split("\n")
      .map((line) => (/^\s*@\S+/.test(line) ? IMPORT_STRIPPED_MARKER : line))
      .join("\n");
  } catch {
    return { dropped: "read_error" };
  }

  // Re-bound the SANITIZED size: the marker is longer than the `@…` line it
  // replaces, so a file under the raw cap can amplify over it. Enforce the cap on
  // what actually gets injected so marker amplification cannot defeat it.
  if (Buffer.byteLength(sanitized, "utf8") > maxBytes) return { dropped: "too_large" };

  return { text: sanitized };
}
