// PRD #1809 D4: which live processes belong to a run, found by ENVIRONMENT, not by process group.
//
// The pinned Claude CLI spawns every Bash tool command with `detached: true` (setsid: a new session
// AND a new process group), so a backgrounded `go test ./... &` leaves the CLI's process group the
// moment it starts. A probe that only asks whether the CLI's groups still have members misses it,
// and a reap that only kills those groups leaves it running. What every process the run starts DOES
// keep, unless it deliberately changes it, is its environment: the SDK env sets HOME to the run's
// private HOME (`agent-home/<runId>`), and every child inherits it. So a process is ATTRIBUTED to the
// run when its `/proc/<pid>/environ` holds the exact entry `HOME=<runHome>`, or when its working
// directory is inside the run's worktree or HOME.
//
// The scan runs as the agent uids (the uids that own the run's processes) through the same wrappers
// as the rmtree.ts helpers: under the uid split as `runner`, then `runner-cmd`; single-uid directly
// as the worker. A process's `environ` and `cwd` are readable by its own uid, so each pass looks
// only at processes of its own uid; one of those it cannot read (non-dumpable, or a read racing its
// exit that did not end in ESRCH/ENOENT) is ATTRIBUTED (fail closed: "not quiet"), while a pid that
// is gone is skipped. Every pass is bounded: a streamed read of `/proc`, at most `maxPids` pids, and
// an in-script wall-time budget (the worker cannot kill a helper under the split, see rmtree.ts).

import { type CommandWrapper, agentWrappers, runAgentHelper } from "./rmtree.js";

/** Pids one pass may look at before it stops (and says the scan is incomplete). */
const MAX_PIDS = 200_000;
/** A scan pass's own wall-time budget, and the worker's wait for it (plus slack). */
const SCAN_BUDGET_MS = 10_000;
/** A reap pass's own wall-time budget (it scans, kills and scans again). */
const REAP_BUDGET_MS = 20_000;
const HELPER_SLACK_MS = 5_000;

/**
 * The attribution helper, as `node -e <script> <mode> <procRoot> <home> <worktree|-> <maxPids>
 * <budgetMs> <workerPid>`.
 *
 * For each numeric entry of `<procRoot>` (streamed, never listed whole) it reads `status` (the Uid
 * line and State) and `stat` (ppid and start time). A process whose real or effective uid is not
 * this helper's is not this pass's to judge and is skipped; a zombie (or dead) process holds no
 * files and is skipped; the helper itself and `<workerPid>` are skipped. Otherwise it reads
 * `environ` (at most 4 MiB, bounded reads) and `cwd`:
 *  - `home`: environ holds the NUL-separated entry `HOME=<home>` exactly;
 *  - `cwd`: the working directory is `<home>` or `<worktree>` or inside either;
 *  - `unreadable`: either read failed while the pid still exists, or environ is over 4 MiB;
 *  - otherwise not the run's; a pid that vanished mid-read is skipped.
 *
 * Mode `scan` prints `A\t<pid>\t<why>` per attributed process. Mode `kill` SIGKILLs every process
 * attributed by `home` or `cwd` (never an `unreadable` one: that is not known to be the run's, and
 * never a direct child of `<workerPid>`: the worker's own git or helper children), in rounds until a
 * round kills nothing (at most 5): immediately before each kill it re-reads `stat` and requires the
 * same start time (a recycled pid has another), and re-reads `environ`/`cwd` and requires the
 * attribution again; it prints `K\t<pid>\t<why>` per kill, then the last round's survivors as `A`
 * lines. Both modes end with ONE `H\t<json>` summary (`scanned`, `truncated`: the pid or time budget
 * ran out, `unresolved`: pids whose uid could not be read). Exit 0; 6 on bad arguments; 7 when
 * `<procRoot>` cannot be read.
 */
const RUN_PROCS_SCRIPT = `
const fs = require("node:fs");
const C = fs.constants;
const [mode, root, home, worktree, maxArg, budgetArg, workerArg] = process.argv.slice(1);
const maxPids = Number(maxArg);
const budgetMs = Number(budgetArg);
const workerPid = Number(workerArg);
const abs = (p) => typeof p === "string" && p.startsWith("/") && p.length > 1;
if ((mode !== "scan" && mode !== "kill") || !abs(root) || !abs(home) || (worktree !== "-" && !abs(worktree))) process.exit(6);
if (!(maxPids >= 0) || !(budgetMs >= 0) || !Number.isInteger(workerPid)) process.exit(6);
const uid = process.getuid();
const self = process.pid;
const deadline = Date.now() + budgetMs;
const want = Buffer.from("HOME=" + home);
const dirs = worktree === "-" ? [home] : [home, worktree];
const inside = (p) => dirs.some((d) => p === d || p.startsWith(d + "/"));
const ENV_MAX = 4 * 1024 * 1024;
const w = (line) => process.stdout.write(line + "\\n");
const gone = (e) => e && (e.code === "ENOENT" || e.code === "ESRCH");
// Read at most max + 1 bytes; null when there are more than max.
function readBounded(p, max) {
  const fd = fs.openSync(p, C.O_RDONLY | C.O_NOFOLLOW | C.O_NONBLOCK);
  try {
    const chunk = Buffer.alloc(65536);
    const parts = [];
    let n = 0;
    for (;;) {
      const r = fs.readSync(fd, chunk, 0, chunk.length, null);
      if (r === 0) break;
      n += r;
      if (n > max) return null;
      parts.push(Buffer.from(chunk.subarray(0, r)));
    }
    return Buffer.concat(parts);
  } finally {
    fs.closeSync(fd);
  }
}
const exists = (pid) => { try { fs.lstatSync(root + "/" + pid); return true; } catch { return false; } };
// undefined: vanished, not this uid's, or a zombie. "unknown": its uid could not be read.
function info(pid) {
  let status, stat;
  try {
    status = readBounded(root + "/" + pid + "/status", 65536);
    stat = readBounded(root + "/" + pid + "/stat", 65536);
  } catch (e) {
    return gone(e) || !exists(pid) ? undefined : "unknown";
  }
  if (status === null || stat === null) return "unknown";
  const s = status.toString("latin1");
  const u = /^Uid:\\s+(\\d+)\\s+(\\d+)/m.exec(s);
  if (!u) return "unknown";
  if (Number(u[1]) !== uid && Number(u[2]) !== uid) return undefined;
  const st = /^State:\\s+(\\S)/m.exec(s);
  if (st && (st[1] === "Z" || st[1] === "X" || st[1] === "x")) return undefined;
  const t = stat.toString("latin1");
  const f = t.slice(t.lastIndexOf(")") + 2).split(" ");
  if (f.length < 20) return "unknown";
  return { ppid: Number(f[1]), start: f[19] };
}
function hasHome(buf) {
  let i = 0;
  while ((i = buf.indexOf(want, i)) !== -1) {
    const end = i + want.length;
    if ((i === 0 || buf[i - 1] === 0) && (end === buf.length || buf[end] === 0)) return true;
    i += 1;
  }
  return false;
}
// "home" | "cwd" | "unreadable" | "none" | "gone"
function attribution(pid) {
  let unreadable = false;
  try {
    const env = readBounded(root + "/" + pid + "/environ", ENV_MAX);
    if (env === null) unreadable = true;
    else if (hasHome(env)) return "home";
  } catch {
    if (!exists(pid)) return "gone";
    unreadable = true;
  }
  try {
    if (inside(fs.readlinkSync(root + "/" + pid + "/cwd"))) return "cwd";
  } catch {
    if (!exists(pid)) return "gone";
    unreadable = true;
  }
  return unreadable ? "unreadable" : "none";
}
let scanned = 0, truncated = false, unresolved = 0;
const spend = () => {
  if (truncated) return false;
  if (scanned >= maxPids || Date.now() > deadline) { truncated = true; return false; }
  scanned++;
  return true;
};
// One pass over the pids: [pid, why, info] for each attributed process.
function round() {
  const found = [];
  let dir;
  try { dir = fs.opendirSync(root); } catch { process.exit(7); }
  try {
    for (;;) {
      let e;
      try { e = dir.readSync(); } catch { truncated = true; break; }
      if (e === null) break;
      if (!/^[1-9][0-9]*$/.test(e.name)) continue;
      if (!spend()) break;
      const pid = Number(e.name);
      if (pid === self || pid === workerPid) continue;
      const pi = info(pid);
      if (pi === undefined) continue;
      if (pi === "unknown") { unresolved++; continue; }
      const why = attribution(pid);
      if (why === "gone" || why === "none") continue;
      found.push([pid, why, pi]);
    }
  } finally {
    dir.closeSync();
  }
  return found;
}
if (mode === "scan") {
  for (const [pid, why] of round()) w("A\\t" + pid + "\\t" + why);
} else {
  let left = [];
  for (let r = 0; r < 5; r++) {
    const found = round();
    let killed = 0;
    left = [];
    for (const [pid, why, pi] of found) {
      if (why === "unreadable" || pi.ppid === workerPid) { left.push([pid, why]); continue; }
      // Re-verify immediately before the kill: the same process (start time), still the run's.
      const again = info(pid);
      if (again === undefined) continue;
      if (again === "unknown" || again.start !== pi.start) { left.push([pid, "unreadable"]); continue; }
      const now = attribution(pid);
      if (now === "gone" || now === "none") continue;
      if (now === "unreadable") { left.push([pid, now]); continue; }
      try {
        process.kill(pid, "SIGKILL");
        killed++;
        w("K\\t" + pid + "\\t" + now);
      } catch (err) {
        if (err.code !== "ESRCH") left.push([pid, "kill-failed"]);
      }
    }
    if (killed === 0 || truncated) break;
  }
  for (const [pid, why] of left) w("A\\t" + pid + "\\t" + why);
}
w("H\\t" + JSON.stringify({ scanned, truncated, unresolved }));
`;

/** Test seams for {@link scanRunProcesses} / {@link reapRunProcesses}. */
export interface RunProcsOptions {
  /** The uids to run the passes as, in order (default: the measuring wrappers of rmtree.ts). */
  wrappers?: readonly CommandWrapper[];
  /** The proc root to read (default `/proc`); tests point it at a fake tree. */
  procRoot?: string;
  /** Pids one pass may look at (default {@link MAX_PIDS}). */
  maxPids?: number;
  /** One pass's own wall-time budget in ms (default {@link SCAN_BUDGET_MS} / {@link REAP_BUDGET_MS}). */
  budgetMs?: number;
  /** The worker's pid, never attributed and whose direct children are never killed (default
   *  `process.pid`). */
  workerPid?: number;
}

/** What one scan found. */
export interface RunProcessScan {
  /** Pids attributed to the run (by HOME, working directory, or an unreadable environ/cwd). */
  pids: number[];
  /** Every pass ran to the end: no budget ran out, no uid was unreadable, no helper failed. A scan
   *  that is not complete is "unknown", and unknown is not quiet. */
  complete: boolean;
}

/** What one reap did. */
export interface RunProcessReap {
  killed: number[];
  /** Attributed processes still there after the reap (unreadable ones are never killed). */
  left: number[];
  complete: boolean;
}

/** The `/proc` of this host; a constant so no caller spells the path. */
const PROC_ROOT = "/proc";

async function passes(
  mode: "scan" | "kill",
  home: string,
  worktree: string | undefined,
  opts: RunProcsOptions,
): Promise<{ attributed: number[]; killed: number[]; complete: boolean }> {
  const wrappers = opts.wrappers ?? agentWrappers("measure");
  const budgetMs = opts.budgetMs ?? (mode === "scan" ? SCAN_BUDGET_MS : REAP_BUDGET_MS);
  const attributed = new Set<number>();
  const killed = new Set<number>();
  let complete = true;
  for (const wrap of wrappers) {
    const args = [
      mode,
      opts.procRoot ?? PROC_ROOT,
      home,
      worktree ?? "-",
      String(opts.maxPids ?? MAX_PIDS),
      String(budgetMs),
      String(opts.workerPid ?? process.pid),
    ];
    let result;
    try {
      result = await runAgentHelper(wrap, RUN_PROCS_SCRIPT, args, budgetMs + HELPER_SLACK_MS);
    } catch {
      complete = false;
      continue;
    }
    const lines = result.stdout.split("\n").filter((l) => l !== "");
    const last = lines.pop();
    if (result.code !== 0 || !last?.startsWith("H\t")) {
      complete = false;
      continue;
    }
    let summary: { truncated?: unknown; unresolved?: unknown };
    try {
      summary = JSON.parse(last.slice(2)) as typeof summary;
    } catch {
      complete = false;
      continue;
    }
    if (summary.truncated !== false || summary.unresolved !== 0) complete = false;
    for (const line of lines) {
      const f = line.split("\t");
      const pid = Number(f[1]);
      if (f.length !== 3 || !Number.isSafeInteger(pid) || pid <= 0) {
        complete = false;
        continue;
      }
      if (f[0] === "A") attributed.add(pid);
      else if (f[0] === "K") killed.add(pid);
      else complete = false;
    }
  }
  return { attributed: [...attributed], killed: [...killed], complete };
}

/**
 * PRD #1809 D4: the live processes attributed to a run (see the file comment): the cache cap's
 * quiet point needs none of them alive. Never throws: a helper that could not run, or a pass that
 * ran out of its budget, makes the scan incomplete, and the caller treats that as "not quiet".
 */
export async function scanRunProcesses(home: string, worktree: string | undefined, opts: RunProcsOptions = {}): Promise<RunProcessScan> {
  try {
    const r = await passes("scan", home, worktree, opts);
    return { pids: r.attributed, complete: r.complete };
  } catch {
    return { pids: [], complete: false };
  }
}

/**
 * PRD #1809 D4: SIGKILL the processes attributed to a run by HOME or working directory, each by its
 * exact pid, as the uid that owns it, after re-verifying it immediately before the kill (see
 * {@link RUN_PROCS_SCRIPT}). The disk parks call this after the executor's process-group reap, which
 * misses a detached Bash command. Never throws.
 */
export async function reapRunProcesses(home: string, worktree: string | undefined, opts: RunProcsOptions = {}): Promise<RunProcessReap> {
  try {
    const r = await passes("kill", home, worktree, opts);
    return { killed: r.killed, left: r.attributed, complete: r.complete };
  } catch {
    return { killed: [], left: [], complete: false };
  }
}
