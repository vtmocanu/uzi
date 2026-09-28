import { after, before, describe, it, type TestContext } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { execFileSync } from "node:child_process";
import {
  LiveAttemptRegistry,
  newRunAttempt,
  procfsTable,
  quiesceRunAttempt,
  reapProcesses,
  type ProcessQuiescence,
  type QuiesceProcess,
  type QuiesceRunDeps,
  type ReapDeps,
  type ScanRequest,
} from "../src/run-quiescence.js";
import { RUN_ATTEMPT_ENV, RUN_CLONE_ENV, RUN_CLONE_KEY_ENV, statStartTime, workerSpawnNonce } from "../src/worker-spawn-mark.js";
import { restoreHermeticView } from "./setup/hermetic-proc.js";

// issue #1783 M4 — the CI incident on the REAL procfs, not a fake root. A real `ssh-agent` (on
// Linux, OpenSSH makes it non-dumpable, and a setgid build is non-dumpable anyway, so a non-root
// reader gets EACCES on its environ and cwd) runs as this uid, and the production reap (real
// procfs, 5 s deadline, 100 ms interval) must:
//   (a) block a seed when the agent is in the clone's scope (by cwd, or by another live attempt's
//       markers), naming its pid;
//   (b) on an UNRELATED agent (cwd elsewhere, no markers), fail closed when its env/cwd cannot be
//       read, naming `pid <pid> "ssh-agent" could not be attributed` for the operator; when they
//       can be read (root, or a dumpable build), leave it out of the verdict entirely.
//
// Each agent is started `ssh-agent -D` (foreground: it keeps the cwd it was started in, which the
// daemonizing path would chdir to "/") from a short-lived `sh` that backgrounds it and exits, so
// the agent is reparented AWAY from this test process, like a real stray on a worker. As this
// process's own descendant, a readable unmarked agent would be excluded as "the scanner's own"
// (R5), which no production stray is. `$!` is the agent's own pid (sh forks, the child execs
// ssh-agent), confirmed below from its world-readable stat. The launcher writes `$!` to a pidfile
// before it prints it, so an agent whose launcher fails after starting it is still stopped (only
// while that pid's stat still names ssh-agent with a start time at or after a floor taken just
// before the launch, so a reused pid is adopted only if it is itself an ssh-agent started since). Every
// agent is recorded with its start time (field 22 of its world-readable stat) at launch, and is
// signalled only by that exact pid, and only while the pid's start time still matches: a pid the
// kernel has since reused for an unrelated process is never signalled.
//
// What each branch actually exercises depends on the host. On a non-root CI runner with a
// non-dumpable ssh-agent (OpenSSH's default on Linux, and any setgid build), BOTH (a) tests take the
// unreadable branch: the agent's env and cwd cannot be read, so it is blocked as
// `unreadable_unattributed` wherever it runs, and the cwd scoping of the first (a) test and the
// marker scoping of the second are exercised only where the agent IS readable (root, or a dumpable
// build). The markers test uses ANOTHER live attempt's markers (a registered attempt that is not the
// one being seeded), so its readable branch proves `live_attempt_conflict`, not own-attempt reaping.
//
// Every reap below reads only the tracked agent's real procfs entries. Only the unreadable
// kill case can signal; its injected kill checks the recorded start time again.

const PROC = path.join("/", "proc");

function onPath(name: string): string | undefined {
  for (const dir of (process.env.PATH ?? "").split(path.delimiter)) {
    if (!dir) continue;
    const candidate = path.join(dir, name);
    try {
      fs.accessSync(candidate, fs.constants.X_OK);
      if (fs.statSync(candidate).isFile()) return candidate;
    } catch {
      // not here
    }
  }
  return undefined;
}

const SSH_AGENT = onPath("ssh-agent");
const SKIP: string | false =
  process.platform !== "linux"
    ? "the real procfs reap is Linux-only"
    : SSH_AGENT === undefined
      ? "ssh-agent is not on PATH"
      : false;

let work: string;
/** Every agent this file launched: pid -> its start time read at launch. */
const started = new Map<number, number>();
/** Socket dirs, kept short (under /tmp) so a long TMPDIR cannot overflow sun_path. */
const sockDirs: string[] = [];

/** sun_path is 108 bytes on Linux, including the terminating NUL. */
const SUN_PATH_MAX = 107;

before(() => {
  work = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-procfs-ssh-agent-"));
  // Keep the hermetic default installed; each real procfs reap receives a narrowed table.
});

after(async () => {
  for (const pid of started.keys()) await stopByPid(pid);
  restoreHermeticView();
  fs.rmSync(work, { recursive: true, force: true });
  for (const dir of sockDirs) fs.rmSync(dir, { recursive: true, force: true });
});

/** True while `pid` exists and is not a zombie. */
function alive(pid: number): boolean {
  try {
    return !/^State:\s*[ZX]/m.test(procfsTable.readStatus(pid));
  } catch {
    return false;
  }
}

/** `pid`'s start time read from procfs now, or undefined when it is gone or unparsable. */
function startTimeOf(pid: number): number | undefined {
  try {
    return statStartTime(procfsTable.readStat(pid));
  } catch {
    return undefined;
  }
}

/** True while `pid` is alive and is still the process recorded at launch (same start time). */
function stillOurs(pid: number, startTime: number): boolean {
  return startTimeOf(pid) === startTime && alive(pid);
}

/** Record a launched agent by pid and start time; a pid already gone is not recorded. */
function track(pid: number): void {
  const startTime = startTimeOf(pid);
  if (startTime !== undefined) started.set(pid, startTime);
}

/** A boot-relative start-time floor taken just before a launch: the start time (same clock-tick
 *  units as field 22) of a throwaway `sh` that reads its own stat and exits. Any process started
 *  after this call has a start time at or above it. Undefined when it cannot be read. */
function startTimeFloor(): number | undefined {
  try {
    return statStartTime(execFileSync("sh", ["-c", 'cat "$0/$$/stat"', PROC], { encoding: "utf8", timeout: 5_000 }));
  } catch {
    return undefined;
  }
}

/** Recovery-path tracking (the launcher failed, so its pid came from the pidfile): record `pid` only
 *  when its stat still names `ssh-agent` and its start time is at or after `floor`, taken before the
 *  launch. If the agent died and the kernel reused its pid, the reused process is adopted only when it
 *  is itself an ssh-agent started after this launch began; any other process is left alone. */
function trackRecovered(pid: number, floor: number | undefined): void {
  if (floor === undefined) return;
  let stat: string;
  try {
    stat = procfsTable.readStat(pid);
  } catch {
    return; // gone
  }
  const startTime = statStartTime(stat);
  if (!/^\d+ \(ssh-agent\) /.test(stat) || startTime === undefined || startTime < floor) return;
  started.set(pid, startTime);
}

/** SIGTERM `pid`, then SIGKILL it if it is still alive after 2 s. By exact pid only, and only while
 *  its start time still matches the one recorded at launch, re-checked before each signal. */
async function stopByPid(pid: number): Promise<void> {
  const startTime = started.get(pid);
  if (startTime === undefined) return;
  started.delete(pid);
  if (!stillOurs(pid, startTime)) return; // gone, or the pid was reused
  try {
    process.kill(pid, "SIGTERM");
  } catch {
    return; // already gone
  }
  const end = Date.now() + 2_000;
  while (stillOurs(pid, startTime) && Date.now() < end) await new Promise((r) => setTimeout(r, 25));
  if (stillOurs(pid, startTime)) {
    try {
      process.kill(pid, "SIGKILL");
    } catch {
      // gone between the check and the signal
    }
  }
}

/** A fresh, short socket dir: under /tmp when it is usable, else under os.tmpdir(). */
function shortSockDir(): string {
  for (const base of ["/tmp", os.tmpdir()]) {
    try {
      const dir = fs.mkdtempSync(path.join(base, "uzi-sa-"));
      sockDirs.push(dir);
      return dir;
    } catch {
      // not writable here; try the next base
    }
  }
  throw new Error("no writable directory for the ssh-agent socket");
}

/** Start `ssh-agent -D -a <sock>` in `cwd` with exactly `env`, reparented away from this process;
 *  resolve once its socket exists (bounded), with the agent's own pid. Skips the test (undefined)
 *  when even the short socket path would not fit sun_path. */
async function startAgent(t: TestContext, cwd: string, env: NodeJS.ProcessEnv): Promise<number | undefined> {
  const sockDir = shortSockDir();
  const sock = path.join(sockDir, "agent.sock");
  if (Buffer.byteLength(sock) > SUN_PATH_MAX) {
    t.skip(`the socket path ${sock} exceeds sun_path (${SUN_PATH_MAX} bytes)`);
    return undefined;
  }
  const pidFile = path.join(sockDir, "agent.pid");
  const floor = startTimeFloor();
  let out: string;
  try {
    out = execFileSync(
      "sh",
      ["-c", '"$0" -D -a "$1" </dev/null >/dev/null 2>&1 & echo $! >"$2"; echo $!', SSH_AGENT!, sock, pidFile],
      { cwd, env, encoding: "utf8", timeout: 10_000 },
    );
  } catch (err) {
    // The launcher failed or timed out: recover the agent's pid from the pidfile, so the cleanup
    // still stops it, but only while that pid is still an ssh-agent started after `floor`.
    const recovered = Number(fs.existsSync(pidFile) ? fs.readFileSync(pidFile, "utf8").trim() : NaN);
    if (Number.isSafeInteger(recovered) && recovered > 0) trackRecovered(recovered, floor);
    throw err;
  }
  const pid = Number(out.trim());
  assert.ok(Number.isSafeInteger(pid) && pid > 0, `no agent pid from the launcher: ${JSON.stringify(out)}`);
  track(pid);
  const end = Date.now() + 10_000;
  while (!fs.existsSync(sock) && alive(pid) && Date.now() < end) await new Promise((r) => setTimeout(r, 25));
  assert.ok(fs.existsSync(sock), `ssh-agent (pid ${pid}) did not create its socket within 10 s`);
  // The pid is the agent itself (world-readable stat, even for a non-dumpable process), and it is
  // not this test's child.
  const stat = procfsTable.readStat(pid);
  assert.match(stat, /^\d+ \(ssh-agent\) /, `pid ${pid} is not the ssh-agent: ${stat}`);
  const ppid = Number(stat.slice(stat.lastIndexOf(")") + 2).split(/\s+/)[1]);
  assert.notEqual(ppid, process.pid, "the agent was reparented away from the test");
  return pid;
}

/** "unreadable" when this uid cannot read the agent's environ (non-dumpable, e.g. a setgid
 *  ssh-agent as a non-root user), "readable" otherwise. */
function dumpability(pid: number): "unreadable" | "readable" {
  try {
    fs.readFileSync(path.join(PROC, String(pid), "environ"));
    return "readable";
  } catch (err) {
    const code = (err as NodeJS.ErrnoException).code;
    if (code === "EACCES" || code === "EPERM") return "unreadable";
    throw err;
  }
}

const UNVERIFIED_REASONS = new Set(["unreadable_unattributed", "status_unreadable", "status_unparsable"]);

/**
 * Assert the unverified verdict's detail names `pid` as unattributed. The detail names at most two
 * pids, the unreadable unattributed ones first and then by pid; on a host where another runner-uid
 * process is also unattributable and ranks before this agent, the agent may be the second one named
 * or only counted in "+N more", which the assertion follows (and says so in a diagnostic).
 */
function assertDetailNames(t: TestContext, q: ProcessQuiescence, pid: number): void {
  t.diagnostic(`verdict detail (what RunResidueBlockedError wraps): ${q.detail}`);
  const rank = (p: QuiesceProcess): number => (p.reason === "unreadable_unattributed" ? 0 : 1);
  const order = q.processes
    .filter((p) => UNVERIFIED_REASONS.has(p.reason))
    .sort((a, b) => rank(a) - rank(b) || a.pid - b.pid)
    .map((p) => p.pid);
  const at = order.indexOf(pid);
  assert.ok(at >= 0, `pid ${pid} is among the unverified processes: ${JSON.stringify(q.processes)}`);
  if (at === 0) {
    assert.ok(
      q.detail.startsWith(`runner-uid pid ${pid} "ssh-agent" could not be attributed (env/cwd unreadable)`),
      q.detail,
    );
    return;
  }
  t.diagnostic(`another unattributable runner-uid process ranks first on this host: ${q.detail}`);
  if (at === 1) {
    assert.ok(q.detail.includes(`; pid ${pid} "ssh-agent" (env/cwd unreadable)`), q.detail);
    return;
  }
  // Ranked third or later: only counted, in "+N more", with N every unverified pid past the two named.
  t.diagnostic(`pid ${pid} ranks ${at + 1}th, so it is only counted in "+N more", not named`);
  assert.ok(q.detail.endsWith(`; +${order.length - 2} more`), q.detail);
}

const cloneKeyFor = (clone: string): string => newRunAttempt("run-key-probe", 1, clone, () => []).cloneKey;

/** Read only this file's recorded agent. Signal only when explicitly allowed. */
function trackedAgentDeps(pid: number, maySignal = false): ReapDeps {
  const start = started.get(pid);
  assert.ok(start !== undefined, `agent pid ${pid} was not tracked`);
  return {
    table: {
      ...procfsTable,
      listPids: () => stillOurs(pid, start) ? [pid] : [],
    },
    kill: (candidate) => {
      assert.equal(candidate, pid, "only the tracked agent may be signalled");
      assert.equal(maySignal, true, "observation cases must never signal");
      if (stillOurs(pid, start)) process.kill(pid, "SIGKILL");
    },
  };
}

function trackedAgentReap(pid: number): QuiesceRunDeps {
  return { reap: { ...trackedAgentDeps(pid), viaHelper: false } };
}

describe("the real procfs reap against a real ssh-agent", { skip: SKIP }, () => {
  it("signals only a tracked unreadable unattributed agent through the narrowed real procfs table", { timeout: 60_000 }, async (t) => {
    const clone = path.join(work, "runner", "github.com+o+r", "issue-9100");
    const elsewhere = path.join(work, "elsewhere-9100");
    fs.mkdirSync(clone, { recursive: true });
    fs.mkdirSync(elsewhere, { recursive: true });
    const pid = await startAgent(t, elsewhere, { PATH: process.env.PATH });
    if (pid === undefined) return;
    try {
      if (dumpability(pid) === "readable") {
        t.skip("this ssh-agent exposes its environment; the unreadable branch is unavailable");
        return;
      }
      assert.equal(stillOurs(pid, started.get(pid)!), true);
      const req = {
        mode: "own" as const,
        targetUid: process.getuid!(),
        targetKey: cloneKeyFor(clone),
        targetPaths: [clone],
        ownMarker: undefined,
        liveMarkers: [],
        liveRoots: [],
        workerNonce: workerSpawnNonce(),
        mayKillUnreadableUnattributed: true,
      } as ScanRequest & { mayKillUnreadableUnattributed: boolean };
      const out = await reapProcesses(req, trackedAgentDeps(pid, true));
      assert.equal(out.state, "quiescent", out.detail);
      assert.deepEqual(out.killed, [pid]);
      assert.equal(alive(pid), false);
    } finally {
      await stopByPid(pid);
    }
  });
  it("(a) in scope by cwd: a seed is blocked and names the agent's pid", { timeout: 60_000 }, async (t) => {
    const clone = path.join(work, "runner", "github.com+o+r", "issue-9101");
    fs.mkdirSync(clone, { recursive: true });
    const pid = await startAgent(t, clone, { PATH: process.env.PATH });
    if (pid === undefined) return;
    try {
      const how = dumpability(pid);
      t.diagnostic(`ssh-agent pid ${pid}: env/cwd ${how} to uid ${process.getuid?.()}`);
      const out = await quiesceRunAttempt({
        mode: "seed",
        attempt: undefined,
        cloneKey: cloneKeyFor(clone),
        targetPaths: [clone],
        processes: true,
        dockerHost: undefined,
        registry: new LiveAttemptRegistry(),
      }, trackedAgentReap(pid));
      const q = out.process!;
      assert.notEqual(q.state, "quiescent", q.detail);
      const entry = q.processes.find((p) => p.pid === pid);
      assert.ok(entry, `the agent's pid ${pid} is listed: ${JSON.stringify(q.processes)}`);
      assert.equal(entry.comm, "ssh-agent");
      assert.ok(!q.killed.includes(pid), "a seed never signals an unmarked process");
      assert.equal(alive(pid), true);
      if (how === "unreadable") {
        assert.equal(q.state, "unverified", q.detail);
        assert.equal(entry.reason, "unreadable_unattributed");
        assertDetailNames(t, q, pid);
      } else {
        assert.equal(entry.reason, "unattributed_in_scope");
        if (q.state === "survivors") {
          // The survivors detail counts the in-scope processes; the pid is in `processes` (and so
          // in the "not quiescent" log line), not in the detail itself.
          assert.match(q.detail, /^\d+ in-scope process\(es\) survive the reap$/);
          t.diagnostic(`readable in-scope survivor: the detail "${q.detail}" does not name pid ${pid}`);
        } else {
          assert.equal(q.state, "unverified", q.detail);
          t.diagnostic(`another runner-uid process is unattributable on this host: ${q.detail}`);
        }
      }
    } finally {
      await stopByPid(pid);
    }
  });

  it("(a) in scope by another live attempt's markers (cwd elsewhere): a seed is blocked and names the pid", { timeout: 60_000 }, async (t) => {
    const clone = path.join(work, "runner", "github.com+o+r", "issue-9102");
    const elsewhere = path.join(work, "elsewhere-9102");
    fs.mkdirSync(clone, { recursive: true });
    fs.mkdirSync(elsewhere, { recursive: true });
    const other = newRunAttempt("run-9102", 2, clone, () => []);
    const registry = new LiveAttemptRegistry();
    registry.add(other);
    const pid = await startAgent(t, elsewhere, {
      PATH: process.env.PATH,
      [RUN_ATTEMPT_ENV]: other.marker,
      [RUN_CLONE_ENV]: other.clonePath,
      [RUN_CLONE_KEY_ENV]: other.cloneKey,
    });
    if (pid === undefined) return;
    try {
      const how = dumpability(pid);
      t.diagnostic(`ssh-agent pid ${pid}: env/cwd ${how} to uid ${process.getuid?.()}`);
      const out = await quiesceRunAttempt({
        mode: "seed",
        attempt: undefined,
        cloneKey: other.cloneKey,
        targetPaths: [clone],
        processes: true,
        dockerHost: undefined,
        registry,
      }, trackedAgentReap(pid));
      const q = out.process!;
      assert.notEqual(q.state, "quiescent", q.detail);
      const entry = q.processes.find((p) => p.pid === pid);
      assert.ok(entry, `the agent's pid ${pid} is listed: ${JSON.stringify(q.processes)}`);
      assert.ok(!q.killed.includes(pid), "a live attempt's process is never signalled");
      assert.equal(alive(pid), true);
      if (how === "unreadable") {
        assert.equal(q.state, "unverified", q.detail);
        assert.equal(entry.reason, "unreadable_unattributed");
        assertDetailNames(t, q, pid);
      } else {
        assert.equal(entry.reason, "live_attempt_conflict");
      }
    } finally {
      await stopByPid(pid);
    }
  });

  it("(b) unrelated (cwd outside the clone, no markers): an own reap fails closed on it only when it cannot be read", { timeout: 60_000 }, async (t) => {
    const clone = path.join(work, "runner", "github.com+o+r", "issue-9103");
    const elsewhere = path.join(work, "elsewhere-9103");
    fs.mkdirSync(clone, { recursive: true });
    fs.mkdirSync(elsewhere, { recursive: true });
    const own = newRunAttempt("run-9103", 1, clone, () => []);
    const registry = new LiveAttemptRegistry();
    registry.add(own);
    const pid = await startAgent(t, elsewhere, { PATH: process.env.PATH });
    if (pid === undefined) return;
    try {
      const how = dumpability(pid);
      t.diagnostic(`ssh-agent pid ${pid}: env/cwd ${how} to uid ${process.getuid?.()}`);
      const out = await quiesceRunAttempt({
        mode: "own",
        attempt: own,
        cloneKey: own.cloneKey,
        targetPaths: [clone],
        processes: true,
        dockerHost: undefined,
        registry,
      }, trackedAgentReap(pid));
      const q = out.process!;
      const entry = q.processes.find((p) => p.pid === pid);
      assert.ok(!q.killed.includes(pid), "an unrelated process is never signalled");
      assert.equal(alive(pid), true);
      if (how === "unreadable") {
        // The accepted fail-closed behaviour: nothing proves the agent is not the run's, so the
        // proof blocks, and the operator is told which process to identify.
        assert.equal(q.state, "unverified", q.detail);
        assert.ok(entry, `the agent's pid ${pid} is listed: ${JSON.stringify(q.processes)}`);
        assert.equal(entry.reason, "unreadable_unattributed");
        assert.equal(entry.comm, "ssh-agent");
        assertDetailNames(t, q, pid);
      } else {
        assert.equal(entry, undefined, `a readable out-of-scope agent is not listed: ${JSON.stringify(entry)}`);
        t.diagnostic(
          `ssh-agent pid ${pid} is readable to uid ${process.getuid?.()} (root, or a dumpable build), so its ` +
            `cwd and env prove it out of scope and the blocked branch does not apply; verdict: ${q.state} (${q.detail})`,
        );
      }
    } finally {
      await stopByPid(pid);
    }
  });
});
