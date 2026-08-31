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
// The DURABLE argument, first, because it survives any single fact below being wrong: a
// scoped check must REPRODUCE the CLI's private, undocumented, version-coupled path
// algorithm — realpath the cwd, encode it (non-alphanumeric → `-`), truncate at 200
// chars, append a base36 hash, and consult whatever sibling/worktree fallbacks the CLI
// consults — and EVERY reproduction error in that chain fails in the DESTRUCTIVE
// direction: we compute a dir the CLI did not, find nothing, answer "absent", and
// silently discard a live session. The glob reproduces none of it: it is a strict
// SUPERSET of the CLI's candidate dirs, so its only possible error is the SURVIVABLE one
// (a false present → keep a dead resume → today's loud failure, nothing new breaks).
// One-directional error, and it points the safe way. That is the whole design, and it
// holds no matter which link in the chain a future SDK changes.
//
// This is the same instinct as the fail-open-past-a-hash rule: we already refuse to
// reimplement the CLI's base36 truncation hash because guessing it wrong yields a false
// absent and the CLI is a better authority than our reconstruction. That reasoning does
// not stop at the hash — it applies to realpath, the encoding, and the fallbacks equally.
//
// Realpath is the EVIDENCE that made this concrete, not the reason on its own. Measured
// against the real CLI (@anthropic-ai/claude-agent-sdk 0.3.201) with a symlinked cwd: a
// transcript under `encoded(realpath(cwd))` RESOLVES; the identical transcript under
// `encoded(the symlink path)` gives "No conversation found". So the CLI realpaths BEFORE
// encoding, and a single-path check that encodes the raw cwd looks at a directory that
// cannot exist whenever ANY component of the worker's data path is a symlink — and
// symlinked components demonstrably exist in this environment (that measurement is the
// proof). The CLI also has prefix-sibling and `git worktree list --porcelain` fallbacks
// in its lookup, read from the shipped bundle by researcher-105; this author did NOT
// independently exercise them (an earlier "prefix sibling not found" measurement used a
// sub-200-char cwd, so the encoding was verbatim and the truncation-prefix machinery was
// never engaged — it did not test that path, and does not contradict the source-read).
// Each is one more link a scoped check would have to reproduce; the glob subsumes all.
//
// The false present is additionally UNREACHABLE in both lanes today, because each lane's
// cwd is invariant for a session's life, so a HOME only ever holds the ONE project dir
// of its own session(s):
//   - Run lane: HOME is per-run (`agent-home/<runId>`) and the cwd is that run's clone,
//     `runner/<repoDir>/<key>` where `key` is `issue-<iid>` (git.ts, runnerCloneForBranch
//     / createOrAttachRunnerClone) — no runId, no attempt counter, no nonce, so a re-
//     claimed run reuses the same clone dir. One session, one project dir.
//   - Chat lane: the cwd is the baked source snapshot `UZI_SRC_DIR` (`/opt/uzi-src`) —
//     `main.ts` builds ChatExecutor with no `srcDir` override, so `chat-executor.ts`
//     defaults it (`srcDir ?? UZI_SRC_DIR`, then `cwd: this.srcDir`), and EVERY chat
//     session on the worker runs under that identical cwd. So the shared chat HOME holds
//     exactly one project dir too — NOT "a handful", as an earlier version of this
//     comment wrongly claimed. That one false line is what made a single-path narrowing
//     look safe; it is falsifiable in two greps. Do not narrow this to one path.
// So with one project dir per HOME in both lanes, the glob and a CORRECT scoped check
// are behaviorally identical TODAY; the glob simply costs nothing and fails safe if that
// wiring ever changes.
//
// WHAT WOULD FALSIFY THE SAFETY ARGUMENT: if any lane ever ran two DIFFERENT cwds under
// one HOME, the glob could answer "present" for a transcript belonging to the other cwd
// that the CLI will not resolve — a false present. Still the survivable direction (a
// kept dead resume, not a discarded live one), but it would mean the glob is no longer
// behaviorally identical to a correct scoped check. If you add a second cwd per HOME,
// revisit this.
//
// FAIL-OPEN on top of all that, cut deliberately: "the directory or file is not there"
// (ENOENT/ENOTDIR) is a FACT and drops the resume; "I could not look" (EACCES, EIO,
// anything else) keeps it, leaving the CLI to produce today's loud failure. The loop
// below carries NO `isDirectory()` filter, on purpose — see its comment; that filter was
// the one residual false-absent path in the glob, and dropping it makes "the glob's only
// error is a false present" exactly true rather than nearly true.
//
// VERSION-COUPLED to the bundled Claude Code CLI's transcript LAYOUT
// (`$HOME/.claude/projects/<dir>/<session-id>.jsonl`), the CLI's private on-disk
// format, not a documented contract. The glob depends only on the `.jsonl`-named-by-
// session-id leaf and the two-level projects/<dir> nesting — NOT on the encoding rule,
// which is exactly why globbing survives an encoding change. A layout change would make
// this find nothing and fail OPEN (degrade toward today's loud failure), never toward a
// silent fresh start — but nothing in CI would notice it had stopped firing, so an SDK
// bump needs this re-verified. The pinned SDK version lives in agent/package.json and is
// deliberately NOT restated as a number here — a hardcoded version is exactly what went
// stale (issue #723). The `0.3.201` figures elsewhere in this header are the ORIGINAL
// measurement provenance (issue #105), not a currentness claim; the layout was re-checked
// unchanged against the then-pinned SDK for #723. The layout, the local pre-network
// resolution, and the realpath-before-encode behaviour were each established independently by researcher-105
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
    // Deliberately NO `entry.isDirectory()` guard. A Dirent from readdir reports a
    // symlink-to-directory as isSymbolicLink() && !isDirectory(), so filtering on
    // isDirectory() would SKIP a symlinked project dir and answer absent — a false
    // absent, the one destructive error mode, reachable because this environment has
    // symlinked path components (the realpath finding proves it). Instead we `access()`
    // every entry name and let the filesystem classify it: a plain file entry raises
    // ENOTDIR on the join, which isNotFound routes to `continue` exactly like a missing
    // transcript. So the loop admits real dirs AND symlinked dirs, and rejects only
    // what genuinely has no `<sid>.jsonl` under it. This is what makes the "the glob's
    // only error is a false PRESENT" claim in the header exactly true, not nearly.
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
