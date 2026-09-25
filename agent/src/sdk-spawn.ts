// Detached spawn for the SDK subprocess + process-group kill.
//
// The default SDK spawn does NOT set `detached` (verified against the bundled SDK
// spawn: no `detached:true`. The pinned version lives in agent/package.json, not
// restated here — the `0.3.201` this was first measured at is provenance, not a
// currentness pin, and re-checking on an SDK bump is a behaviour check, not a number
// bump; issue #723), so its child is not a process-group leader and `process.kill(-pid)`
// cannot reach a bash the agent backgrounded. Passing this as
// `Options.spawnClaudeCodeProcess` spawns the Claude Code CLI in its OWN group
// (`detached: true` ⇒ setsid on POSIX), so a watchdog trip can group-kill the
// whole tree. Node's ChildProcess structurally satisfies the SDK's
// SpawnedProcess contract (stdin/stdout/killed/exitCode/kill/on/once/off).
//
// PRD #51 M4: the SDK CLI is an UNTRUSTED execution surface, so it (and its whole
// tree) runs as the `runner` uid via the setpriv wrapper in runner-uid.ts — the
// wrapper is the group leader after it execs the CLI, so a detached spawn's pid is
// still the group id the kill targets. Under the split the worker cannot signal the
// runner group directly (EPERM), so the group-kill reaps via a setpriv-to-runner
// `kill` (killRunnerGroup). Single-uid (#58) falls back to a direct spawn/kill.
//
// Degrade path: `abortController.abort()` remains the PRIMARY, asserted stop —
// the SDK closes stdin, waits its grace window, then signals the child (that
// SIGTERM cross-uid-EPERMs under the split, but stdin-EOF still stops the CLI).
// The group kill here is the load-bearing B1 reap + defense for orphaned
// grandchildren; if even the single pid fails (already gone) it is a no-op.

import type { ChildProcess } from "node:child_process";
import type { SpawnOptions } from "@anthropic-ai/claude-agent-sdk";
import { killRunnerGroup, killRunnerGroupOnly, runnerSpawn } from "./runner-uid.js";

/** Spawn the SDK subprocess in its own process group, under the `runner` uid. */
export function spawnDetached(opts: SpawnOptions): ChildProcess {
  return runnerSpawn(opts.command, opts.args, {
    cwd: opts.cwd,
    env: opts.env,
    signal: opts.signal,
    detached: true,
    stdio: ["pipe", "pipe", "pipe"],
  });
}

/**
 * Best-effort SIGKILL of the process GROUP led by `pid` (the runner subprocess
 * tree), via the setpriv-to-runner reap under the split, or directly single-uid.
 * Safe with an undefined or already-dead pid. @returns true if a kill was dispatched.
 */
export function killProcessGroup(pid: number | undefined): boolean {
  return killRunnerGroup(pid);
}

/**
 * issue #1656: SIGKILL the process GROUP `pgid` only, never the bare pid (see
 * killRunnerGroupOnly). The return value cannot tell "already gone" from "failed"; confirm
 * absence with {@link processGroupPresent}.
 */
export function killProcessGroupOnly(pgid: number): boolean {
  return killRunnerGroupOnly(pgid);
}

/**
 * issue #1656: whether any process is still in process group `pgid`, asked of the kernel with
 * `kill(-pgid, 0)`: an atomic existence check on the group, unlike a snapshot listing of
 * processes that a member forking around the list/read steps can slip past. ESRCH means absent;
 * success or EPERM means present (EPERM is what the worker uid gets for a live runner group
 * under the uid split); zombies count as present. @returns undefined, fail closed, for any other
 * error or a non-positive pgid. `kill` is injectable for tests.
 */
export function processGroupPresent(
  pgid: number,
  kill: (pid: number, signal?: string | number) => true = process.kill,
): boolean | undefined {
  if (pgid <= 0) return undefined;
  try {
    kill(-pgid, 0);
    return true;
  } catch (err) {
    const code = (err as NodeJS.ErrnoException).code;
    if (code === "ESRCH") return false;
    if (code === "EPERM") return true;
    return undefined;
  }
}
