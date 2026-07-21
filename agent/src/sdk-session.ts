// Resume preflight for the SDK's session transcript (issue #105).
//
// The SDK does not interpret `options.resume` at all — it passes the id through
// to the bundled Claude Code CLI as `--resume <id>`, and the CLI resolves it
// LOCALLY, before any network call, against
//
//     $HOME/.claude/projects/<cwd-with-every-slash-turned-into-a-dash>/<session-id>.jsonl
//
// Verified empirically against @anthropic-ai/claude-agent-sdk 0.3.201 in both
// directions: an empty HOME yields `{"type":"result","subtype":
// "error_during_execution", ..., "errors":["No conversation found with session ID:
// …"]}` on exit 1 with `duration_api_ms: 0` (no request is ever made, so nothing is
// spent); planting a real transcript at exactly that path gets the same invocation
// past resolution and on to the API. So the file's presence IS the resume
// precondition, and its absence is knowable before we spend a turn discovering it.
//
// Why that matters here: the run lane pins a per-run HOME (`agent-home/<runId>`,
// main.ts) that lives on the CLAIMING worker's own data volume. A requeued run whose
// affinity grace has lapsed can be claimed by a DIFFERENT worker, where that HOME has
// never existed — the claim still carries the session id, but the transcript it names
// does not exist on this machine. Same for a worker that came back on a fresh volume
// (`docker compose down -v`, a pod restarted onto empty storage) under its OWN
// identity, which is why this check is worker-side rather than a claim-time decision
// the server could make from worker ids alone.
//
// FAIL-OPEN, and the cut line is deliberate: "the directory or file is not there"
// (ENOENT/ENOTDIR) is a FACT and drops the resume, but "I could not look" (EACCES,
// EIO, anything else) keeps it, leaving the CLI to produce today's loud failure. The
// asymmetry is the point — a spurious "absent" would silently discard a perfectly
// good session, which is exactly the class of failure this check exists to prevent.

import { access, readdir } from "node:fs/promises";
import path from "node:path";
import type { Logger } from "./log.js";

/** The SDK session id becomes a path segment below, so it is shape-checked first —
 *  same posture as RUN_ID_RE guarding the per-run HOME (runner.ts). Sessions are
 *  CLI-minted UUIDs that reach us via the claim payload; anything else is not a
 *  session id we can look up, and is never joined onto a path. */
const SESSION_ID_RE = /^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/;

/** ENOENT/ENOTDIR are the two ways the filesystem says "not there"; everything else
 *  is a failure to look, which must not be read as an answer. */
function isNotFound(err: unknown): boolean {
  const code = (err as { code?: unknown } | null)?.code;
  return code === "ENOENT" || code === "ENOTDIR";
}

/**
 * Can the CLI still resolve `sessionId` under `homeDir`?
 *
 * `true` means resume it (the transcript is there, or we could not tell and refuse to
 * guess). `false` means the transcript is definitively absent and the caller must drop
 * the resume — and SAY it did, because a silent fresh start is a worse failure than
 * the loud one it replaces.
 *
 * The lookup globs every project dir rather than recomputing the cwd encoding, so a
 * future change to how the CLI encodes a cwd cannot turn a present transcript into a
 * false "absent". The dir holds one entry per cwd the HOME has ever been used with —
 * exactly one for a per-run HOME, a handful for the shared chat HOME.
 */
export async function sessionTranscriptResolvable(
  homeDir: string,
  sessionId: string,
  log?: Logger,
): Promise<boolean> {
  if (!SESSION_ID_RE.test(sessionId)) {
    // Content-free: never echo a rejected id into a log line.
    log?.warn("session id is not UUID-shaped; not checking the transcript, leaving the resume as-is");
    return true;
  }
  const projectsDir = path.join(homeDir, ".claude", "projects");
  let entries;
  try {
    entries = await readdir(projectsDir, { withFileTypes: true });
  } catch (err) {
    if (isNotFound(err)) return false; // no projects dir at all ⇒ no transcript here
    log?.warn("could not read the SDK projects dir; leaving the resume as-is", { error: String((err as Error)?.message ?? err) });
    return true;
  }
  for (const entry of entries) {
    if (!entry.isDirectory()) continue;
    try {
      await access(path.join(projectsDir, entry.name, `${sessionId}.jsonl`));
      return true;
    } catch (err) {
      if (isNotFound(err)) continue;
      log?.warn("could not stat a session transcript; leaving the resume as-is", { error: String((err as Error)?.message ?? err) });
      return true;
    }
  }
  return false;
}
