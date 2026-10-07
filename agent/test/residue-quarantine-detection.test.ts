import { describe, it, beforeEach, afterEach } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { spawn, type ChildProcess } from "node:child_process";
import type { ExecutorResult, RunContext } from "../src/executor.js";
import type { CodexExecutionSafety } from "../src/harness.js";
import type { ExecutorFactory } from "../src/runner.js";
import { RunResidueBlockedError, residueQuarantine, latchResidueQuarantine, assertResidueQuarantineOpen, ResidueQuarantinedError } from "../src/residue-quarantine.js";
import { LiveAttemptRegistry, newRunAttempt, quiesceRunAttempt, setQuiescenceViewForTests, type QuiesceRunOutcome, type QuiesceRunRequest } from "../src/run-quiescence.js";
import { registerWorkerRunnerRoot } from "../src/worker-spawn-mark.js";
import { makeFakeProcRoot, plantFakeProc, plantInScope, plantUnreadableUnattributed, withQuiescenceView } from "./fake-proc.js";
import { restoreHermeticView } from "./setup/hermetic-proc.js";
import { realProcfsSkip } from "./real-procfs.js";
import { nullLogger } from "./helpers.js";
import { api, fakeGitlab, git, gitlabClaim, homeDir, installHarness, runnerWith, simulateCommittedWork } from "./runner-harness.js";

// issue #2213 — detection of the worker-wide residue quarantine latch, driven through the runner
// (phaseClone's pre-fetch check, the quiesceRun verdict latch) over a fake procfs root, plus one
// real-procfs proof. Layer: RunRunner.phaseClone / checkWorkerResidueBeforeFetch / quiesceRun, with
// run-quiescence scanOnce behind them. Single-uid Linux only: under the uid split or off Linux the
// check does not run, and a test says so rather than faking a process table.

installHarness();

const LINUX = { skip: process.platform !== "linux" ? "the worker-wide check reads procfs (Linux only)" : false };
const ME = process.getuid?.() ?? 0;
const PID = 4242;
const TIMEOUT = { timeout: 60_000 };

let root: string;
beforeEach(() => {
  root = makeFakeProcRoot("uzi-2213-proc-");
});
afterEach(() => {
  restoreHermeticView();
  fs.rmSync(root, { recursive: true, force: true });
});

const statuses = (runId: string): string[] => api.states.filter((s) => s.runId === runId).map((s) => String(s.body.status));
const lastFailed = (runId: string) => api.states.filter((s) => s.runId === runId && s.body.status === "failed").at(-1)?.body;
const exists = (pid: number): boolean => fs.existsSync(path.join(root, String(pid)));

/** An executor that must never start (the run must stop before the clone is fetched). */
function neverStarts(counter: { started: number }, safety?: CodexExecutionSafety): ExecutorFactory {
  return (runId) => ({
    homeDir: path.join(homeDir, runId),
    executor: {
      ...(safety ? { safety } : {}),
      run: async (): Promise<ExecutorResult> => {
        counter.started++;
        throw new Error("must not start");
      },
    },
  });
}

const CODEX_SAFETY: CodexExecutionSafety = {
  kind: "codex",
  withBoundary: async () => {
    throw new Error("unexpected boundary");
  },
  spawnBoundaryProcess: async () => {
    throw new Error("unexpected spawn");
  },
  dispose: async () => ({ kind: "disposed" }),
};

/** Count (and optionally replace) git.ensureClone for the duration of `fn`. */
async function withEnsureCloneSpy<T>(fn: (calls: () => number) => Promise<T>, replace?: () => Promise<string>): Promise<T> {
  const real = git.ensureClone.bind(git);
  let n = 0;
  git.ensureClone = (async (...a: Parameters<typeof real>) => {
    n++;
    return replace ? replace() : real(...a);
  }) as typeof git.ensureClone;
  try {
    return await fn(() => n);
  } finally {
    git.ensureClone = real;
  }
}

describe("the pre-fetch worker-wide check refuses the first fetch (issue #2213)", LINUX, () => {
  for (const shape of ["claude", "codex"] as const) {
    it(`${shape} run, claim_generation 1: a planted unlinked unreadable process stops the run before ensureClone and latches`, TIMEOUT, async () => {
      plantUnreadableUnattributed(root, PID);
      const { gitlab } = fakeGitlab();
      const counter = { started: 0 };
      const claim = gitlabClaim(22130 + (shape === "codex" ? 1 : 0), { claim_generation: 1 });
      await withEnsureCloneSpy(async (calls) => {
        await withQuiescenceView({ procRoot: root }, () =>
          runnerWith(neverStarts(counter, shape === "codex" ? CODEX_SAFETY : undefined), gitlab).execute(claim),
        );
        assert.equal(calls(), 0, "the PAT-bearing fetch never ran");
      });
      assert.equal(counter.started, 0);
      const failed = lastFailed(claim.run_id);
      assert.equal(failed?.fail_origin, "worker_residue_blocked");
      const reason = String(failed?.failure_reason);
      assert.match(reason, /no clone was fetched/);
      assert.match(reason, /worker-wide check/);
      assert.doesNotMatch(reason, /HOME reap/, "names the check that refused");
      assert.match(reason, /pid 4242 "ssh-agent" could not be attributed/);
      assert.doesNotMatch(reason, /kept for inspection/);
      assert.ok(!statuses(claim.run_id).includes("recovery_wait"), "a worker fault, never a forge park");
      assert.equal(exists(PID), true, "the process was never signalled");
      const latched = residueQuarantine();
      assert.ok(latched, "the worker is latched");
      assert.equal(latched.site, "pre_clone");
      assert.equal(latched.runId, claim.run_id);
      assert.match(latched.cause, /pid 4242 "ssh-agent"/);
    });
  }

  it("control: no such process: the first claim fetches and completes, and nothing latches", TIMEOUT, async () => {
    simulateCommittedWork();
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(22132, { claim_generation: 1 });
    const factory: ExecutorFactory = (runId) => ({
      homeDir: path.join(homeDir, runId),
      executor: { run: async (ctx: RunContext): Promise<ExecutorResult> => ({ branch: ctx.branch, summary: "done" }) },
    });
    await withQuiescenceView({ procRoot: root }, () => runnerWith(factory, gitlab).execute(claim));
    assert.ok(statuses(claim.run_id).includes("completed"), statuses(claim.run_id).join(","));
    assert.equal(residueQuarantine(), undefined);
  });

  it("a latch set while claimRun was pending refuses the returned run at phaseClone, before ensureClone", TIMEOUT, async () => {
    // No process planted: only the latch. The claim loop would not have claimed it; this is the race.
    latchResidueQuarantine({ cause: "set while the claim was in flight", site: "finalize" }, nullLogger());
    const { gitlab } = fakeGitlab();
    const counter = { started: 0 };
    const claim = gitlabClaim(22133, { claim_generation: 1 });
    await withEnsureCloneSpy(async (calls) => {
      await runnerWith(neverStarts(counter), gitlab).execute(claim);
      assert.equal(calls(), 0);
    });
    const failed = lastFailed(claim.run_id);
    assert.equal(failed?.fail_origin, "worker_residue_blocked");
    assert.match(String(failed?.failure_reason), /this worker is quarantined \(set while the claim was in flight\)/);
    assert.equal(counter.started, 0);
  });
});

describe("the latch persists (issue #2213)", LINUX, () => {
  it("after the process is gone a later claim and a later turn are still refused; only the test reset clears it", TIMEOUT, async () => {
    plantUnreadableUnattributed(root, PID);
    const { gitlab } = fakeGitlab();
    const counter = { started: 0 };
    const first = gitlabClaim(22140, { claim_generation: 1 });
    await withQuiescenceView({ procRoot: root }, () => runnerWith(neverStarts(counter), gitlab).execute(first));
    assert.ok(residueQuarantine());
    // The survivor exits; the process table is clean again.
    fs.rmSync(path.join(root, String(PID)), { recursive: true, force: true });
    const later = gitlabClaim(22141, { claim_generation: 1 });
    await withEnsureCloneSpy(async (calls) => {
      await withQuiescenceView({ procRoot: root }, () => runnerWith(neverStarts(counter), gitlab).execute(later));
      assert.equal(calls(), 0, "a clean scan does not reopen the worker");
    });
    assert.match(String(lastFailed(later.run_id)?.failure_reason), /this worker is quarantined/);
    assert.throws(() => assertResidueQuarantineOpen("provider_turn"), ResidueQuarantinedError);
    assert.ok(residueQuarantine(), "still latched");
  });
});

describe("what does not latch (issue #2213)", LINUX, () => {
  /** Run one first-claim with the sentinel clone: returns whether the fetch was reached. */
  async function reachesFetch(claimIid: number, factory: ExecutorFactory = neverStarts({ started: 0 }), extra = {}, generation = 1): Promise<{ reached: boolean; reason: string }> {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(claimIid, { claim_generation: generation });
    let reached = false;
    await withEnsureCloneSpy(
      async (calls) => {
        await withQuiescenceView({ procRoot: root }, () => runnerWith(factory, gitlab, undefined, undefined, extra).execute(claim));
        reached = calls() > 0;
      },
      async () => {
        throw new Error("sentinel: the fetch was reached");
      },
    );
    return { reached, reason: String(lastFailed(claim.run_id)?.failure_reason) };
  }

  it("another live run's recorded root and its non-dumpable descendants are attributed, not reported", TIMEOUT, async () => {
    const liveAttempts = new LiveAttemptRegistry();
    liveAttempts.add(newRunAttempt("other-run", 1, "/data/runner/x/issue-9", () => [{ pid: 50, startTime: 123456 }]));
    plantFakeProc(root, 50, { uid: ME, startTime: 123456, env: {}, cwd: "/" });
    plantFakeProc(root, 51, { uid: ME, ppid: 50, sid: 50, env: "unreadable" });
    plantFakeProc(root, 52, { uid: ME, ppid: 51, env: "unreadable" });
    const r = await reachesFetch(22150, undefined, { liveAttempts });
    assert.equal(r.reached, true, r.reason);
    assert.equal(residueQuarantine(), undefined);
  });

  it("a worker-launched runner root (registerWorkerRunnerRoot) and its descendants are attributed", TIMEOUT, async () => {
    const unregister = registerWorkerRunnerRoot(60, () => 777);
    try {
      plantFakeProc(root, 60, { uid: ME, startTime: 777, env: "unreadable" });
      plantFakeProc(root, 61, { uid: ME, ppid: 60, sid: 60, env: "unreadable" });
      const r = await reachesFetch(22151);
      assert.equal(r.reached, true, r.reason);
      assert.equal(residueQuarantine(), undefined);
    } finally {
      unregister();
    }
  });

  it("the attributedToOtherLive residual (approved, #1659): an unreadable process linked to another live attempt's root is not reported while that attempt lives", TIMEOUT, async () => {
    const liveAttempts = new LiveAttemptRegistry();
    liveAttempts.add(newRunAttempt("other-run", 1, "/data/runner/x/issue-9", () => [{ pid: 70, startTime: 5 }]));
    // An agent-controlled unreadable process that made itself a child of the root: indistinguishable
    // from the root's own descendants by ancestry, so the worker-wide scan cannot see it.
    plantFakeProc(root, 70, { uid: ME, startTime: 5, env: {}, cwd: "/" });
    plantFakeProc(root, 71, { uid: ME, ppid: 70, comm: "evil", env: "unreadable" });
    const r = await reachesFetch(22152, undefined, { liveAttempts });
    assert.equal(r.reached, true, "the residual: it is not reported");
    assert.equal(residueQuarantine(), undefined);
  });

  it("a readable runner-uid process, whatever its markers or cwd, is not in scope and is never signalled", TIMEOUT, async () => {
    plantInScope(root, 80, "/data/runner/github.com+o+r/issue-3", { marker: "run:20260101T000000Z-g1-0123456789abcdef", cloneKey: "k", uid: ME });
    plantFakeProc(root, 81, { uid: ME, env: {}, cwd: "/tmp" });
    const r = await reachesFetch(22153);
    assert.equal(r.reached, true, r.reason);
    assert.equal(exists(80) && exists(81), true, "no readable process was signalled");
    assert.equal(residueQuarantine(), undefined);
  });

  it("a process of another uid is ignored", TIMEOUT, async () => {
    plantFakeProc(root, 82, { uid: ME + 7, env: "unreadable" });
    const r = await reachesFetch(22154);
    assert.equal(r.reached, true, r.reason);
  });

  it("status_unreadable refuses the fetch without latching", TIMEOUT, async () => {
    // A pid directory whose status is a directory: unreadable with EISDIR, not ENOENT/ESRCH.
    fs.mkdirSync(path.join(root, "90", "status"), { recursive: true });
    const r = await reachesFetch(22155);
    assert.equal(r.reached, false);
    assert.match(r.reason, /worker_residue_blocked/);
    assert.match(r.reason, /no clone was fetched/);
    assert.equal(residueQuarantine(), undefined, "an unreadable status is not the latch cause");
  });

  it("HOME-attributed reap `left` (a later claim) refuses as today without latching", TIMEOUT, async () => {
    const factory: ExecutorFactory = (runId) => ({
      homeDir: path.join(homeDir, runId),
      executor: {
        run: async (): Promise<ExecutorResult> => {
          throw new Error("must not start");
        },
        reapAttributedProcesses: async () => ({ killed: [], left: [4242], complete: true }),
      },
    });
    const r = await reachesFetch(22157, factory, {}, 2);
    assert.equal(r.reached, false);
    assert.match(r.reason, /HOME-attributed reap left 1 process\(es\)/);
    assert.match(r.reason, /by the HOME reap before the clone fetch/);
    assert.match(r.reason, /no clone was fetched/);
    assert.doesNotMatch(r.reason, /worker-wide check/, "the worker-wide check never ran at this site");
    assert.equal(residueQuarantine(), undefined);
  });

  it("under an active uid split the check does not run: no scan, no latch, the fetch proceeds", TIMEOUT, async () => {
    plantUnreadableUnattributed(root, PID);
    const before = process.env.UZI_UID_SPLIT;
    process.env.UZI_UID_SPLIT = "1";
    try {
      const r = await reachesFetch(22156);
      assert.equal(r.reached, true, r.reason);
      assert.equal(residueQuarantine(), undefined);
      assert.equal(exists(PID), true);
    } finally {
      if (before === undefined) delete process.env.UZI_UID_SPLIT;
      else process.env.UZI_UID_SPLIT = before;
    }
  });
});

describe("every quiesceRun verdict latches on unreadable_unattributed, and only on it (issue #2213)", LINUX, () => {
  function quiescer(processes: Array<{ reason: string }>, state: "unverified" | "survivors") {
    const calls: QuiesceRunRequest[] = [];
    const quiesceRun = async (req: QuiesceRunRequest): Promise<QuiesceRunOutcome> => {
      calls.push(req);
      return {
        process: {
          state,
          processes: processes.map((p, i) => ({ pid: 900 + i, uid: ME, comm: "x", cwd: "/", reason: p.reason })),
          killed: [],
          detail: `runner-uid pid 900 "x" could not be attributed (env/cwd unreadable)`,
        },
        docker: { state: "not_wired", removed: [], detail: "" },
      };
    };
    return { calls, quiesceRun };
  }
  async function finalizeWith(iid: number, processes: Array<{ reason: string }>, state: "unverified" | "survivors") {
    simulateCommittedWork();
    const { gitlab } = fakeGitlab();
    const { calls, quiesceRun } = quiescer(processes, state);
    const factory: ExecutorFactory = (runId) => ({
      homeDir: path.join(homeDir, runId),
      executor: { run: async (ctx: RunContext): Promise<ExecutorResult> => ({ branch: ctx.branch, summary: "done" }) },
    });
    const claim = gitlabClaim(iid, { claim_generation: 1 });
    await withQuiescenceView({ procRoot: root }, () => runnerWith(factory, gitlab, undefined, undefined, { quiesceRun }).execute(claim));
    return { calls, failed: lastFailed(claim.run_id) };
  }

  it("the finalize verdict with an unreadable_unattributed entry latches; the blocking is unchanged (the run still fails closed, typed)", TIMEOUT, async () => {
    const { calls, failed } = await finalizeWith(22160, [{ reason: "unreadable_unattributed" }], "unverified");
    assert.ok(calls.length >= 1);
    assert.equal(failed?.fail_origin, "worker_residue_blocked");
    const latched = residueQuarantine();
    assert.ok(latched);
    assert.equal(latched.site, "finalize");
  });

  it("kill_unconfirmed refuses as today without latching", TIMEOUT, async () => {
    const { failed } = await finalizeWith(22161, [{ reason: "own_attempt:kill_unconfirmed" }], "survivors");
    assert.equal(failed?.fail_origin, "worker_residue_blocked");
    assert.equal(residueQuarantine(), undefined);
  });

  it("an unattributed in-scope survivor and a live-attempt conflict refuse without latching", TIMEOUT, async () => {
    const { failed } = await finalizeWith(22162, [{ reason: "unattributed_in_scope" }, { reason: "live_attempt_conflict" }], "survivors");
    assert.equal(failed?.fail_origin, "worker_residue_blocked");
    assert.equal(residueQuarantine(), undefined);
  });
});

describe("a hostile comm is sanitized wherever the cause surfaces (issue #2213)", LINUX, () => {
  it("ESC, U+202E and a newline in the comm never reach the latch cause, the failure_reason or the heartbeat member", TIMEOUT, async () => {
    plantFakeProc(root, PID, { uid: ME, comm: "ev\u001b[31mil\u202ex\nboom", env: "unreadable", cwd: "/" });
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(22170, { claim_generation: 1 });
    await withQuiescenceView({ procRoot: root }, () => runnerWith(neverStarts({ started: 0 }), gitlab).execute(claim));
    const latched = residueQuarantine();
    assert.ok(latched);
    // eslint-disable-next-line no-control-regex
    const unsafe = /[\u0000-\u001f\u007f-\u009f\u202a-\u202e\u2066-\u2069\u2028\u2029]/;
    assert.doesNotMatch(latched.cause, unsafe);
    assert.ok(latched.cause.length <= 163, "capped at 160 plus the ellipsis");
    const reason = String(lastFailed(claim.run_id)?.failure_reason);
    assert.doesNotMatch(reason, unsafe);
    assert.match(reason, /ev\?\[31mil\?x\?boom/);
  });
});

// ─── the real procfs ───────────────────────────────────────────────────────────────────────

const PY_NON_DUMPABLE =
  "import ctypes, sys, time\n" +
  "libc = ctypes.CDLL(None)\n" +
  "libc.prctl(4, 0, 0, 0, 0)\n" + // PR_SET_DUMPABLE = 4, value 0
  'print("ready", flush=True)\n' +
  "time.sleep(300)\n";

describe("the real procfs (issue #2213)", { skip: process.platform !== "linux" ? "Linux only" : realProcfsSkip("residue quarantine real procfs (issue #2213)") }, () => {
  it("a real non-dumpable python3 child is named unreadable_unattributed, latches, and is not killed", TIMEOUT, async (t) => {
    let child: ChildProcess | undefined;
    try {
      child = spawn("python3", ["-c", PY_NON_DUMPABLE], { stdio: ["ignore", "pipe", "ignore"] });
    } catch (err) {
      t.skip(`python3 could not start: ${String(err)}`);
      return;
    }
    const pid = child.pid;
    if (pid === undefined) {
      t.skip("python3 is not available");
      return;
    }
    try {
      const ready = await new Promise<boolean>((resolve) => {
        const timer = setTimeout(() => resolve(false), 10_000);
        child!.stdout!.once("data", () => {
          clearTimeout(timer);
          resolve(true);
        });
        child!.once("error", () => resolve(false));
        child!.once("exit", () => resolve(false));
      });
      if (!ready) {
        t.skip("python3 did not start (or lacks ctypes)");
        return;
      }
      try {
        fs.readFileSync(`/proc/${pid}/environ`);
        t.skip("the non-dumpable child stays readable here (a privileged reader): the unreadable branch is not exercised");
        return;
      } catch {
        // EACCES: the intended, unreadable shape.
      }
      setQuiescenceViewForTests(undefined); // the real procfs, the production timing
      const out = await quiesceRunAttempt({
        mode: "capture",
        attempt: undefined,
        cloneKey: "",
        targetPaths: [],
        processes: true,
        workerWide: true,
        dockerHost: undefined,
        registry: new LiveAttemptRegistry(),
      });
      const named = out.process?.processes.find((p) => p.pid === pid);
      assert.equal(named?.reason, "unreadable_unattributed", out.process?.detail);
      assert.deepEqual(out.process?.killed, [], "nothing was signalled");
      const rr = runnerWith(neverStarts({ started: 0 }), fakeGitlab().gitlab);
      await assert.rejects(rr.checkWorkerResidueBeforeFetch("22180000-0000-4000-8000-000000000000", "pre_clone"), RunResidueBlockedError);
      assert.ok(residueQuarantine(), "latched");
      assert.doesNotThrow(() => process.kill(pid, 0), "the process is still alive: it was never killed");
    } finally {
      restoreHermeticView();
      try {
        child.kill("SIGKILL"); // exact pid of the child this test started
      } catch {
        // already gone
      }
    }
  });
});
