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
import fs from "node:fs";
import path from "node:path";
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
 * issue #1656: whether any process is still in process group `pgid`, by scanning
 * `<procRoot>/<pid>/stat` (field 5, pgrp; zombies count as present). Independent of any kill's
 * exit status. @returns undefined when absence cannot be established: no readable procfs, a
 * procfs mounted with any `hidepid` but 0/off, or any `subset=` (other uids' processes, e.g. the runner's, would be
 * invisible and read as absent), a stat unreadable for any reason but the process exiting
 * mid-scan, or a scan that saw no process at all. `procRoot` is injectable for tests.
 */
export function processGroupPresent(pgid: number, procRoot = "/proc"): boolean | undefined {
  if (!procfsShowsAllProcesses(procRoot)) return undefined;
  let entries: string[];
  try {
    entries = fs.readdirSync(procRoot);
  } catch {
    return undefined;
  }
  let scanned = 0;
  for (const name of entries) {
    if (!/^\d+$/.test(name)) continue;
    let stat: string;
    try {
      stat = fs.readFileSync(path.join(procRoot, name, "stat"), "utf8");
    } catch (err) {
      // Exited between the listing and the read: not a member. Anything else is unknowable.
      if ((err as NodeJS.ErrnoException).code === "ENOENT" || (err as NodeJS.ErrnoException).code === "ESRCH") continue;
      return undefined;
    }
    // `pid (comm) state ppid pgrp ...`; comm may hold spaces and parens, so split after the LAST ')'.
    const close = stat.lastIndexOf(")");
    if (close < 0) return undefined;
    const pgrp = Number(stat.slice(close + 2).split(" ")[2]);
    if (!Number.isInteger(pgrp)) return undefined;
    scanned++;
    if (pgrp === pgid) return true;
  }
  return scanned > 0 ? false : undefined;
}

/** Whether the procfs at `procRoot` provably lists every uid's processes. Fail closed: only a
 *  proc mount with no `hidepid` option (or `hidepid=0`/`hidepid=off`) and no `subset=` option
 *  counts; any other value, including one a future kernel adds, is not complete. */
function procfsShowsAllProcesses(procRoot: string): boolean {
  let mountinfo: string;
  try {
    mountinfo = fs.readFileSync(path.join(procRoot, "self", "mountinfo"), "utf8");
  } catch {
    return false;
  }
  for (const line of mountinfo.split("\n")) {
    const fields = line.split(" ");
    const sep = fields.indexOf("-");
    if (sep < 0 || fields[4] !== procRoot || fields[sep + 1] !== "proc") continue;
    const superOpts = (fields[sep + 3] ?? "").split(",");
    return superOpts.every((o) =>
      o.startsWith("hidepid=") ? o === "hidepid=0" || o === "hidepid=off" : !o.startsWith("subset="),
    );
  }
  return false;
}
