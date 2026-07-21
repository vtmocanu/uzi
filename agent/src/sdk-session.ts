// Resume preflight for the SDK's session transcript (issue #105).
//
// The SDK does not interpret `options.resume` at all — it passes the id through to the
// bundled Claude Code CLI as `--resume <id>`, and the CLI resolves it LOCALLY, before
// any network call, against
//
//     $HOME/.claude/projects/<encoded-cwd>/<session-id>.jsonl
//
// Verified empirically against @anthropic-ai/claude-agent-sdk 0.3.201 in both
// directions: an empty HOME yields `{"type":"result","subtype":"error_during_execution",
// ..., "errors":["No conversation found with session ID: …"]}` on exit 1 with
// `duration_api_ms: 0` (no request is made, so nothing is spent); planting a real
// transcript at exactly that path gets the same invocation past resolution and on to
// the API. So the file's presence IS the resume precondition, and its absence is
// knowable before we spend a turn discovering it.
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
// ── Why this GLOBS every project dir instead of computing the one right dir ──────────
//
// The obvious implementation is to reproduce `<encoded-cwd>` and stat one file. It is
// the wrong one, and the reason is measured, not assumed. `<encoded-cwd>` is NOT the cwd
// string: the CLI resolves the cwd through realpath FIRST, then encodes (every
// non-alphanumeric → `-`, truncating long paths with a base36 hash). Proven against the
// real CLI with a symlinked cwd — a transcript under `encoded(realpath)` resolves; the
// same transcript under `encoded(the symlink path)` gives "No conversation found". So a
// single-path check that encodes the raw cwd would look at a directory that never exists
// whenever ANY component of the worker's data path is a symlink, return "absent", and
// SILENTLY DISCARD A LIVE SESSION — the precise failure this whole check exists to
// prevent. The CLI's lookup also has fallbacks (prefix-sibling dirs, a `git worktree
// list` pass) that widen the same gap.
//
// The glob is a strict SUPERSET of the CLI's candidate dirs, so it can never produce a
// false absent. Its only possible error is a false PRESENT — a transcript for some other
// cwd under this HOME — whose consequence is merely that the resume is kept and the run
// hits today's loud failure: the bug is not fixed for that case, but nothing new breaks.
// That asymmetry (false present harmless, false absent destructive) is the whole design.
//
// And the false present is UNREACHABLE in both lanes, because each lane's cwd is
// invariant for the life of a session, so a HOME only ever accumulates the ONE project
// dir belonging to its session(s):
//   - Run lane: HOME is per-run (`agent-home/<runId>`) and the cwd is that run's own
//     clone (`runner/<repoDir>/issue-<iid>`, one dir). One session, one project dir.
//   - Chat lane: the cwd is the baked source snapshot `UZI_SRC_DIR` (`/opt/uzi-src`) —
//     `main.ts` builds ChatExecutor with no `srcDir` override, so `chat-executor.ts`
//     defaults it, and EVERY chat session on the worker runs under that identical cwd.
//     So the shared chat HOME holds exactly one project dir too, not "a handful".
// (An earlier version of this comment claimed the chat HOME holds "a handful" of project
// dirs. That was false — chat's cwd is constant — and that false line is what made a
// single-path narrowing look safe. It is not. Do not narrow this to one path.)
//
// FAIL-OPEN on top of all that, cut deliberately: "the directory or file is not there"
// (ENOENT/ENOTDIR) is a FACT and drops the resume; "I could not look" (EACCES, EIO,
// anything else) keeps it, leaving the CLI to produce today's loud failure.
//
// VERSION-COUPLED to @anthropic-ai/claude-agent-sdk 0.3.201: the transcript LAYOUT
// (`$HOME/.claude/projects/<dir>/<session-id>.jsonl`) is the CLI's private on-disk
// format, not a documented contract. The glob depends only on the `.jsonl`-named-by-
// session-id leaf and the two-level projects/<dir> nesting — NOT on the encoding rule,
// which is exactly why globbing survives an encoding change. A layout change would make
// this find nothing and fail OPEN (degrade toward today's loud failure), never toward a
// silent fresh start — but nothing in CI would notice it had stopped firing, so an SDK
// bump needs this re-verified. The layout, the local pre-network resolution, and the
// realpath-before-encode behaviour were each established independently by researcher-105
// (shipped bundle + live harness) and by this author (direct CLI runs), so the claims
// above are corroborated, not single-sourced.

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
 * The lookup globs every project dir rather than recomputing the cwd encoding — see the
 * header for why that superset is deliberate (the CLI encodes realpath(cwd), so a
 * single computed path false-absents on any symlinked data dir) and why the false
 * present it admits is unreachable (each lane's cwd is invariant, so a HOME holds one
 * project dir).
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
