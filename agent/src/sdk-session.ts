// Resume preflight for the SDK's session transcript (issue #105).
//
// The SDK does not interpret `options.resume` at all — it passes the id through to the
// bundled Claude Code CLI as `--resume <id>`, and the CLI resolves it LOCALLY, before
// any network call, against
//
//     $HOME/.claude/projects/<encoded-cwd>/<session-id>.jsonl
//
// where <encoded-cwd> is the ABSOLUTE cwd with every non-alphanumeric character
// replaced by `-`. All of this is empirically pinned against
// @anthropic-ai/claude-agent-sdk 0.3.201 (see ASSUMPTIONS below), because it is the
// CLI's private on-disk layout, not a documented contract.
//
// Why the check exists: the run lane pins a per-run HOME (`agent-home/<runId>`,
// main.ts) that lives on the CLAIMING worker's own data volume. A requeued run whose
// affinity grace lapsed can be claimed by a DIFFERENT worker, where that HOME has never
// existed — the claim still carries the session id, but the transcript it names does
// not exist there. It is NOT only a cross-worker bug: a session is filed per cwd, so
// the same HOME on the same machine fails just as hard if the cwd differs, and a worker
// returning on a fresh volume under its own identity loses everything. That breadth is
// why this is worker-side rather than something the server could decide from worker ids.
//
// Scope matters as much as existence. The CLI's lookup is NOT a scan of every project
// dir: it looks in the encoded-cwd dir (plus, for TRUNCATED encodings, prefix-sharing
// siblings, and a git-worktree fallback). A transcript sitting in some other project
// dir is invisible to the CLI. So this check is deliberately scoped the same way — an
// earlier draft of it globbed every project dir, which could answer "present" for a
// file the CLI would never find, keep the resume, and leave the run dying exactly as it
// does today. Over-broad here is not "safely conservative"; it silently un-fixes the bug.
//
// FAIL-OPEN, with a deliberate and asymmetric cut line: "the file is not there"
// (ENOENT/ENOTDIR) is a FACT and drops the resume; anything we merely could not
// determine — an unreadable dir, a session id we cannot parse, a cwd whose encoding we
// cannot reproduce — KEEPS the resume and leaves the CLI to produce its own loud
// failure. Keeping a dead resume costs the run but is visible; dropping a live one
// discards recoverable context, which is the failure class this fix exists to prevent.
//
// ASSUMPTIONS — version-coupled to @anthropic-ai/claude-agent-sdk 0.3.201, verified by
// running the shipped CLI directly (2026-07-21). If a bump changes any of them this
// preflight degrades toward answering "resolvable" more often, i.e. back toward today's
// loud failure — never toward a silent fresh start. `sdk-session.test.ts` pins each:
//   1. The transcript path is `$HOME/.claude/projects/<encoded-cwd>/<session-id>.jsonl`.
//      Planting a real transcript there got `--resume` past resolution and on to the
//      API; an empty HOME produced `No conversation found with session ID: …` on exit 1
//      with `duration_api_ms: 0` — resolution is local and spends nothing.
//   2. The encoding replaces every non-alphanumeric character with `-`.
//   3. The lookup is SCOPED: the same transcript placed in an unrelated project dir,
//      and in a dir sharing a long prefix, both failed to resolve.
//   4. Truncation boundary: an encoding of 200 characters or fewer is used verbatim; at
//      201+ the CLI truncates to 200 and appends `-` plus a 6-character base36 hash.
//      We do not reproduce that hash — a >200 encoding fails open instead.

import { access } from "node:fs/promises";
import path from "node:path";
import type { Logger } from "./log.js";

/** The SDK session id becomes a path segment below, so it is shape-checked first —
 *  same posture as RUN_ID_RE guarding the per-run HOME (runner.ts). Sessions are
 *  CLI-minted UUIDs that reach us via the claim payload; anything else is not a
 *  session id we can look up, and is never joined onto a path. */
const SESSION_ID_RE = /^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/;

/** Longest encoding the CLI stores verbatim (assumption 4). Past this it truncates and
 *  appends a hash we deliberately do not reimplement. */
const MAX_VERBATIM_ENCODED_LEN = 200;

/** The CLI's project-dir name for a cwd: every non-alphanumeric character becomes `-`
 *  (assumption 2). Exported so the test can pin the rule against the real layout. */
export function encodeCwd(cwd: string): string {
  return cwd.replace(/[^a-zA-Z0-9]/g, "-");
}

/** ENOENT/ENOTDIR are the two ways the filesystem says "not there"; everything else is
 *  a failure to look, which must not be read as an answer. */
function isNotFound(err: unknown): boolean {
  const code = (err as { code?: unknown } | null)?.code;
  return code === "ENOENT" || code === "ENOTDIR";
}

/**
 * Can the CLI still resolve `sessionId` for a session recorded at `cwd` under `homeDir`?
 *
 * `true` means resume it — the transcript is there, or we could not tell and refuse to
 * guess. `false` means it is definitively absent and the caller must drop the resume
 * AND say so, because a silent fresh start is a worse failure than the loud one it
 * replaces.
 */
export async function sessionTranscriptResolvable(
  homeDir: string,
  cwd: string,
  sessionId: string,
  log?: Logger,
): Promise<boolean> {
  if (!SESSION_ID_RE.test(sessionId)) {
    // Content-free: never echo a rejected id into a log line.
    log?.warn("session id is not UUID-shaped; not checking the transcript, leaving the resume as-is");
    return true;
  }
  const encoded = encodeCwd(cwd);
  if (encoded.length > MAX_VERBATIM_ENCODED_LEN) {
    // Past the boundary the dir name carries a hash of the CLI's own making. Guessing
    // it wrong would mean discarding a live session; let the CLI answer instead.
    log?.warn("cwd encodes past the CLI's verbatim length; not checking the transcript, leaving the resume as-is", {
      encoded_length: encoded.length,
    });
    return true;
  }
  try {
    await access(path.join(homeDir, ".claude", "projects", encoded, `${sessionId}.jsonl`));
    return true;
  } catch (err) {
    if (isNotFound(err)) return false;
    log?.warn("could not stat the session transcript; leaving the resume as-is", {
      error: String((err as Error)?.message ?? err),
    });
    return true;
  }
}
