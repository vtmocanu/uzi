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
  setQuiescenceViewForTests,
  type ProcessQuiescence,
  type QuiesceProcess,
} from "../src/run-quiescence.js";
import { RUN_ATTEMPT_ENV, RUN_CLONE_ENV, RUN_CLONE_KEY_ENV } from "../src/worker-spawn-mark.js";
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
// ssh-agent), confirmed below from its world-readable stat. Every agent is stopped by that exact
// pid.
//
// These tests deliberately select the REAL process table: setQuiescenceViewForTests(undefined)
// clears the hermetic default the `npm test` preload installed, which is what production runs
// with. They only scan (seed mode never signals an unmarked process, and every clone path, key and
// marker here is unique to this file), so no host process is signalled.

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
const started = new Set<number>();

before(() => {
  work = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-procfs-ssh-agent-"));
  // Real /proc and the production reap timings: no test view at all.
  setQuiescenceViewForTests(undefined);
});

after(async () => {
  for (const pid of started) await stopByPid(pid);
  restoreHermeticView();
  fs.rmSync(work, { recursive: true, force: true });
});

/** True while `pid` exists and is not a zombie. */
function alive(pid: number): boolean {
  try {
    return !/^State:\s*[ZX]/m.test(procfsTable.readStatus(pid));
  } catch {
    return false;
  }
}

/** SIGTERM `pid`, then SIGKILL it if it is still alive after 2 s. By exact pid only. */
async function stopByPid(pid: number): Promise<void> {
  if (!started.has(pid)) return;
  started.delete(pid);
  try {
    process.kill(pid, "SIGTERM");
  } catch {
    return; // already gone
  }
  const end = Date.now() + 2_000;
  while (alive(pid) && Date.now() < end) await new Promise((r) => setTimeout(r, 25));
  if (alive(pid)) {
    try {
      process.kill(pid, "SIGKILL");
    } catch {
      // gone between the check and the signal
    }
  }
}

/** Start `ssh-agent -D -a <sock>` in `cwd` with exactly `env`, reparented away from this process;
 *  resolve once its socket exists (bounded), with the agent's own pid. */
async function startAgent(cwd: string, env: NodeJS.ProcessEnv): Promise<number> {
  const sockDir = fs.mkdtempSync(path.join(work, "sock-"));
  const sock = path.join(sockDir, "agent.sock");
  const out = execFileSync("sh", ["-c", '"$0" -D -a "$1" </dev/null >/dev/null 2>&1 & echo $!', SSH_AGENT!, sock], {
    cwd,
    env,
    encoding: "utf8",
    timeout: 10_000,
  });
  const pid = Number(out.trim());
  assert.ok(Number.isSafeInteger(pid) && pid > 0, `no agent pid from the launcher: ${JSON.stringify(out)}`);
  started.add(pid);
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
  t.diagnostic(`verdict detail (the failure_reason text): ${q.detail}`);
  const rank = (p: QuiesceProcess): number => (p.reason === "unreadable_unattributed" ? 0 : 1);
  const order = q.processes
    .filter((p) => UNVERIFIED_REASONS.has(p.reason))
    .sort((a, b) => rank(a) - rank(b) || a.pid - b.pid)
    .map((p) => p.pid);
  const at = order.indexOf(pid);
  if (at === 0) {
    assert.ok(
      q.detail.startsWith(`runner-uid pid ${pid} "ssh-agent" could not be attributed (env/cwd unreadable)`),
      q.detail,
    );
    assert.ok(q.detail.includes(`pid ${pid} "ssh-agent" could not be attributed`), q.detail);
    return;
  }
  t.diagnostic(`another unattributable runner-uid process ranks first on this host: ${q.detail}`);
  if (at === 1) assert.ok(q.detail.includes(`; pid ${pid} "ssh-agent" (env/cwd unreadable)`), q.detail);
  else assert.match(q.detail, /; \+\d+ more$/);
}

const cloneKeyFor = (clone: string): string => newRunAttempt("run-key-probe", 1, clone, () => []).cloneKey;

describe("the real procfs reap against a real ssh-agent", { skip: SKIP }, () => {
  it("(a) in scope by cwd: a seed is blocked and names the agent's pid", { timeout: 60_000 }, async (t) => {
    const clone = path.join(work, "runner", "github.com+o+r", "issue-9101");
    fs.mkdirSync(clone, { recursive: true });
    const pid = await startAgent(clone, { PATH: process.env.PATH });
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
      });
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
    const pid = await startAgent(elsewhere, {
      PATH: process.env.PATH,
      [RUN_ATTEMPT_ENV]: other.marker,
      [RUN_CLONE_ENV]: other.clonePath,
      [RUN_CLONE_KEY_ENV]: other.cloneKey,
    });
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
      });
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
    const pid = await startAgent(elsewhere, { PATH: process.env.PATH });
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
      });
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
