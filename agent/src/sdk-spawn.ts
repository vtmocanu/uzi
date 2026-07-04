// Detached spawn for the SDK subprocess + process-group kill.
//
// The default SDK spawn does NOT set `detached` (verified against
// @anthropic-ai/claude-agent-sdk 0.3.201: no `detached:true` in the bundled
// spawn), so its child is not a process-group leader and `process.kill(-pid)`
// cannot reach a bash the agent backgrounded. Passing this as
// `Options.spawnClaudeCodeProcess` spawns the Claude Code CLI in its OWN group
// (`detached: true` ⇒ setsid on POSIX), so a watchdog trip can group-kill the
// whole tree. Node's ChildProcess structurally satisfies the SDK's
// SpawnedProcess contract (stdin/stdout/killed/exitCode/kill/on/once/off).
//
// Degrade path: `abortController.abort()` remains the PRIMARY, asserted stop —
// the SDK closes stdin, waits its grace window, then signals the child. The
// group kill here is defense for orphaned grandchildren; if the platform can't
// signal the group it falls back to the child pid, and if even that fails
// (already gone) it is a no-op.

import { spawn, type ChildProcess } from "node:child_process";
import type { SpawnOptions } from "@anthropic-ai/claude-agent-sdk";

/** Spawn the SDK subprocess in its own process group. */
export function spawnDetached(opts: SpawnOptions): ChildProcess {
  return spawn(opts.command, opts.args, {
    cwd: opts.cwd,
    env: opts.env,
    signal: opts.signal,
    detached: true,
    stdio: ["pipe", "pipe", "pipe"],
  });
}

/**
 * Best-effort SIGKILL of the process GROUP led by `pid`, falling back to the
 * single process. Safe with an undefined or already-dead pid.
 * @returns true if a kill signal was dispatched.
 */
export function killProcessGroup(pid: number | undefined): boolean {
  if (pid === undefined || pid <= 0) return false;
  try {
    process.kill(-pid, "SIGKILL");
    return true;
  } catch {
    try {
      process.kill(pid, "SIGKILL");
      return true;
    } catch {
      return false; // already gone or not permitted — abort() covers the SDK side
    }
  }
}
