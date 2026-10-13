// PRD #2170: the repository's root CLAUDE.md as it exists at one COMMIT, read from the worker's
// bare repository. The code cross-check pins instructions to the freshly fetched default tip
// (`repo_instructions_commit`), independently of the diff base. The agent-writable worktree
// (which may already carry the change under review) is never opened.
//
// Same trust model as readRepoInstructions (repo-instructions.ts): ROOT CLAUDE.md only, a symlink
// followed only when it stays inside the repo, never into `.git/`, and ending on a regular blob.
// Here the whole chain is resolved inside the commit's tree with git plumbing, never the
// filesystem: a link is a blob holding its target text, so there is no inode to race and no
// FIFO/device to block on. A link whose target passes through a symlinked DIRECTORY is not
// followed (ls-tree of the full path finds nothing), which fails closed to "no instructions".

import { posix } from "node:path";
import {
  REPO_INSTRUCTIONS_MAX_BYTES,
  sanitizeRepoInstructionsText,
  type RepoInstructionsDrop,
} from "./repo-instructions.js";

/** The one git read this module needs (git.ts GitCache.readBare). */
interface BareInstructionsReader {
  readBare(
    barePath: string,
    args: readonly string[],
    opts: { maxBytes: number; timeoutMs?: number; signal?: AbortSignal },
  ): Promise<{ text: string; truncated: boolean }>;
}

/** At most this many symlink hops are followed before the chain is treated as looping. */
const REPO_INSTRUCTIONS_MAX_SYMLINK_HOPS = 8;
/** A symlink blob larger than this is not a plausible path. */
const SYMLINK_TARGET_MAX_BYTES = 4096;
const LS_TREE_RECORD_MAX_BYTES = 8192;
const OID_RE = /^(?:[0-9a-f]{40}|[0-9a-f]{64})$/;

interface TreeEntry { mode: string; type: string; oid: string; size: number }

/** `git ls-tree -z -l` of exactly `entryPath` at `commit`; undefined when absent or ambiguous. */
async function lsTreeEntry(
  git: BareInstructionsReader, bare: string, commit: string, entryPath: string,
  opts: { timeoutMs?: number; signal?: AbortSignal },
): Promise<TreeEntry | undefined> {
  const out = await git.readBare(bare,
    ["--no-replace-objects", "--literal-pathspecs", "ls-tree", "-z", "-l", commit, "--", entryPath],
    { maxBytes: LS_TREE_RECORD_MAX_BYTES, ...opts });
  if (out.truncated) return undefined;
  const records = out.text.split("\0").filter((r) => r !== "");
  const record = records[0];
  if (records.length !== 1 || record === undefined) return undefined;
  const tab = record.indexOf("\t");
  if (tab < 0 || record.slice(tab + 1) !== entryPath) return undefined;
  const [mode, type, oid, rawSize, ...extra] = record.slice(0, tab).split(/ +/);
  if (mode === undefined || type === undefined || oid === undefined || rawSize === undefined
    || extra.length !== 0 || !OID_RE.test(oid)) return undefined;
  const size = rawSize === "-" ? -1 : Number(rawSize);
  if (!Number.isSafeInteger(size)) return undefined;
  return { mode, type, oid, size };
}

/** A symlink's target resolved against its directory, or undefined when it leaves the repo root,
 *  enters `.git/`, or is not a plain relative path. */
function resolveLinkTarget(fromPath: string, target: string): string | undefined {
  if (target === "" || target.includes("\0") || target.startsWith("/") || target.endsWith("/")) return undefined;
  const resolved = posix.normalize(posix.join(posix.dirname(fromPath), target));
  if (resolved === "." || resolved === ".." || resolved.startsWith("../") || resolved.startsWith("/")) return undefined;
  if (resolved.split("/").some((seg) => seg.toLowerCase() === ".git")) return undefined;
  return resolved;
}

/**
 * The root CLAUDE.md at `commit` in the bare repository `barePath`, sanitized exactly like
 * {@link readRepoInstructions}. Never throws: any failure (a timeout, an unreadable object, an
 * abort) is `{ dropped: "read_error" }`.
 *
 * - Absent ⇒ `absent`. A mode 100644/100755 blob is read.
 * - A mode 120000 symlink is resolved within the same commit tree, at most
 *   {@link REPO_INSTRUCTIONS_MAX_SYMLINK_HOPS} hops. Every hop must stay inside the repo root and
 *   outside `.git/`, and the final entry must be a regular blob; an escaping, broken, looping or
 *   non-blob (directory, submodule) link is `symlinked`.
 * - Over the cap by the ls-tree size, by the bounded read's `truncated` flag, or after import
 *   substitution ⇒ `too_large`. Clipped text is never returned.
 */
export async function readRepoInstructionsAtCommit(
  git: BareInstructionsReader,
  barePath: string,
  commit: string,
  opts: { maxBytes?: number; timeoutMs?: number; signal?: AbortSignal } = {},
): Promise<{ text: string } | { dropped: RepoInstructionsDrop }> {
  const maxBytes = opts.maxBytes ?? REPO_INSTRUCTIONS_MAX_BYTES;
  const read = { ...(opts.timeoutMs !== undefined ? { timeoutMs: opts.timeoutMs } : {}), ...(opts.signal ? { signal: opts.signal } : {}) };
  try {
    if (!/^[0-9a-f]{40}$/.test(commit)) return { dropped: "read_error" };
    let current = "CLAUDE.md";
    for (let hop = 0; hop <= REPO_INSTRUCTIONS_MAX_SYMLINK_HOPS; hop++) {
      const entry = await lsTreeEntry(git, barePath, commit, current, read);
      if (!entry) return { dropped: hop === 0 ? "absent" : "symlinked" };
      if (entry.type !== "blob") return { dropped: "symlinked" };
      if (entry.mode === "120000") {
        if (entry.size < 0 || entry.size > SYMLINK_TARGET_MAX_BYTES) return { dropped: "symlinked" };
        const link = await git.readBare(barePath, ["--no-replace-objects", "cat-file", "blob", entry.oid],
          { maxBytes: SYMLINK_TARGET_MAX_BYTES, ...read });
        if (link.truncated) return { dropped: "symlinked" };
        const next = resolveLinkTarget(current, link.text);
        if (next === undefined) return { dropped: "symlinked" };
        current = next;
        continue;
      }
      if (entry.mode !== "100644" && entry.mode !== "100755") return { dropped: "symlinked" };
      if (entry.size > maxBytes) return { dropped: "too_large" };
      const body = await git.readBare(barePath, ["--no-replace-objects", "cat-file", "blob", entry.oid],
        { maxBytes, ...read });
      if (body.truncated) return { dropped: "too_large" };
      const sanitized = sanitizeRepoInstructionsText(body.text);
      // The import marker is longer than the line it replaces: re-bound what is actually injected.
      if (Buffer.byteLength(sanitized, "utf8") > maxBytes) return { dropped: "too_large" };
      return { text: sanitized };
    }
    return { dropped: "symlinked" };
  } catch {
    return { dropped: "read_error" };
  }
}
