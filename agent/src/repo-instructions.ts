// PRD #246 M2: repo-borne instructions, opt-in (repo.claudemd_enabled) and default off.
//
// Trust model (PRD #246 §Solution): the clone's CLAUDE.md is part of the config
// class the `settingSources: []` isolation exists to block. When — and only when —
// the repo owner has vouched for the repo's review discipline, uzi reads the ROOT
// CLAUDE.md ONLY, through its own controlled channel (never the SDK's project
// loader), so `settingSources: []` never loosens. Nothing else is read: no nested
// `**/CLAUDE.md`, no `CLAUDE.local.md`, no `.claude/` config. Symlinks are never
// followed (a symlinked CLAUDE.md is never read), the file is size-capped, and
// line-leading `@`-import lines are stripped so the model is not induced to Read
// arbitrary files. The result is framed DOWNSTREAM (buildRepoInstructionsContext)
// as a nonce-fenced UNTRUSTED/ADVISORY block, lead-only.
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
 *  post-sanitization), a symlink/non-regular-file (never read), or a read failure
 *  (e.g. EACCES, transient FS error). Trace-logged by the caller. */
type RepoInstructionsDrop = "absent" | "too_large" | "symlinked" | "read_error";

/** Visible marker left in place of a stripped line-leading `@`-import, so the
 *  structural transform is auditable in the injected text. */
const IMPORT_STRIPPED_MARKER = "<!-- uzi: @-import stripped -->";

/**
 * Read + structurally sanitize the clone's ROOT CLAUDE.md.
 *
 * - `lstat` the root path; a read error (ENOENT) ⇒ `{ dropped: "absent" }`.
 * - Not a regular file (symlink or directory) ⇒ `{ dropped: "symlinked" }`. A
 *   symlinked CLAUDE.md is NEVER read — the lstat `isFile()` guard mirrors how
 *   repo-skills.ts guards a symlinked SKILL.md, so a hostile repo cannot redirect
 *   the read outside its own tree.
 * - Over `maxBytes` ⇒ `{ dropped: "too_large" }`.
 * - A `readFile` failure after the lstat passed (e.g. EACCES on a mode-000 file, a
 *   transient FS error, a TOCTOU delete) ⇒ `{ dropped: "read_error" }`. The read is
 *   guarded IN the reader so BOTH callers (the SDK production path and the stub) treat
 *   it as non-fatal — a throw here must never abort run setup.
 * - Otherwise read UTF-8, normalize CRLF→LF, and strip line-leading `@`-import
 *   lines (replaced with a visible marker). An inline `@ref` mid-line may pass
 *   through — that is acceptable and documented: the SDK loader never resolves it
 *   because WE read the file, so a surviving inline ref is inert; the strip is
 *   defense-in-depth against a model-induced `Read`, not a load-bearing control.
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

  let stat;
  try {
    stat = await fs.lstat(filePath);
  } catch {
    return { dropped: "absent" };
  }
  // A symlink or directory is never read (lstat does not follow the link), exactly
  // as repo-skills.ts requires a real file for a SKILL.md.
  if (!stat.isFile()) return { dropped: "symlinked" };
  if (stat.size > maxBytes) return { dropped: "too_large" };

  let sanitized: string;
  try {
    const raw = await fs.readFile(filePath, "utf8");
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
