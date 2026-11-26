// Helpers for the runtime-modules browser harness's interrupt self-test (#4713).
//
// The self-test records process groups of the self-test child's direct children and later
// demands that each was reported gone. A non-detached helper (for example a `ps` the child
// spawns) inherits the child's own job group, so counting every child's group can record a
// group that is not the browser's. These helpers keep that bookkeeping testable on its own.
// They do not claim a proven root cause for the original flake; they make the sampling
// stricter and the failure message more informative.

import { spawnSync } from "node:child_process";

function defaultRunPs() {
  return spawnSync("ps", ["-o", "pid=,ppid=,pgid=", "-e"], { encoding: "utf8" });
}

/**
 * Direct children of `ppid` that lead their own process group (pid === pgid).
 *
 * Leader-only on purpose: a detached Chromium leads its own group, while non-detached helpers
 * and a child in the window before its setsid carry the inherited group, which is not one the
 * harness should demand gone. Malformed lines are skipped. Throws a plain Error when ps fails.
 *
 * @param {number} ppid
 * @param {() => {status: number|null, stdout: string, stderr: string}} [runPs]
 * @returns {{pid: number, ppid: number, pgid: number}[]}
 */
export function childGroupRows(ppid, runPs = defaultRunPs) {
  const r = runPs();
  if (r.status !== 0) throw new Error(`ps failed: ${r.stderr}`);
  const rows = [];
  for (const line of String(r.stdout).split("\n")) {
    const f = line.trim().split(/\s+/);
    if (f.length !== 3) continue;
    const [pid, pp, pgid] = f.map(Number);
    if (![pid, pp, pgid].every((n) => Number.isInteger(n) && n > 0)) continue;
    if (pp === ppid && pid === pgid) rows.push({ pid, ppid: pp, pgid });
  }
  return rows;
}

/**
 * The launch-phase self-test must have signalled the child while the launch was pending.
 * Reads the first `SIGNAL launch-pending=` line of the child's output.
 *
 * @param {string} out
 * @returns {string|null} a problem description, or null when the launch was pending
 */
export function launchPhaseProblem(out) {
  const m = /SIGNAL launch-pending=(true|false)/.exec(out);
  if (!m) return "the child never reported its launch state on SIGTERM (no SIGNAL line)";
  if (m[1] === "true") return null;
  return "the child was signalled after the launch had settled (launch-pending=false), so the launch phase did not interrupt a pending launch";
}

/**
 * Set the exit code, wait for stdout to flush, then exit exactly once.
 *
 * Pipes are asynchronous on POSIX (Node docs, "A note on process I/O"), so process.exit can
 * drop pending writes. Writes complete in order, so the callback of an empty write means the
 * earlier writes reached the pipe. The wait is bounded by `deadlineMs` so shutdown never hangs,
 * and a stream error (EPIPE) is swallowed so it cannot reach the crash path.
 *
 * @param {number} code
 * @param {{stream?: any, deadlineMs?: number, exit?: (code: number) => void}} [opts]
 */
export function flushThenExit(code, { stream = process.stdout, deadlineMs = 2000, exit = (c) => process.exit(c) } = {}) {
  process.exitCode = code;
  stream.once("error", () => {});
  let done = false;
  let timer = null;
  const finish = () => {
    clearTimeout(timer);
    if (done) return;
    done = true;
    exit(code);
  };
  timer = setTimeout(finish, deadlineMs);
  try {
    stream.write("", () => finish());
  } catch {
    finish();
  }
}

/**
 * The launch-phase wait: poll until a group-leading browser child is seen, the launch fails,
 * or the deadline passes. The failure is checked before the leader so a launch that already
 * failed is never announced, even when its browser is visible.
 *
 * @param {{hasLeader: () => boolean, launchFailed: () => boolean, deadlineMs?: number, intervalMs?: number, sleep?: (ms: number) => Promise<void>, now?: () => number}} opts
 * @returns {Promise<"leader"|"failed"|"timeout">}
 */
export async function waitForLeader({ hasLeader, launchFailed, deadlineMs = 10000, intervalMs = 10, sleep = (ms) => new Promise((r) => setTimeout(r, ms)), now = Date.now }) {
  const end = now() + deadlineMs;
  for (;;) {
    if (launchFailed()) return "failed";
    if (hasLeader()) return "leader";
    if (now() >= end) return "timeout";
    await sleep(intervalMs);
  }
}

/**
 * Records that a signal or crash has begun its cleanup, after which no new self-test child may
 * be spawned: that cleanup's killSelfChild has already run and could not cover it.
 * `beforeSpawn()` resolves while the gate is open and never resolves once it is closed (the
 * caller parks; the signal or crash handler ends the process).
 */
export function createTerminationGate() {
  let closed = false;
  return {
    close() {
      closed = true;
    },
    get closed() {
      return closed;
    },
    beforeSpawn() {
      return closed ? new Promise(() => {}) : Promise.resolve();
    },
  };
}
