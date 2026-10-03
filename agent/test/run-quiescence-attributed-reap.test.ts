import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { randomUUID } from "node:crypto";
import type { Readable } from "node:stream";
import { execFileSync } from "node:child_process";
import type { ExecutorResult, RunContext } from "../src/executor.js";
import { REASON_WORKER_RESIDUE_BLOCKED, type ExecutorFactory } from "../src/runner.js";
import { TransientRecoveryError } from "../src/sdk-executor.js";
import type { RecoveryCoordinator } from "../src/recovery.js";
import type { RunProcessReap } from "../src/run-procs.js";
import { LiveAttemptRegistry, mintAttemptId, type QuiesceRunOutcome, type QuiesceRunRequest } from "../src/run-quiescence.js";
import type { AttemptSeedOptions } from "../src/git.js";
import { nullLogger, noProofReseed } from "./helpers.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, homeDir, installHarness, runnerWith, simulateCommittedWork, worktreeDirFor } from "./runner-harness.js";

// issue #1828 — the run's HOME-attributed reap (Executor.reapAttributedProcesses) is part of the
// quiescence proof. The injected quiesceRun answers `quiescent` throughout, so the ONLY thing that
// can block a boundary here is the executor's attributed reap, folded into the verdict by
// RunRunner.quiesceRun: survivors it left, or an incomplete reap, fail every boundary closed
// exactly like a blocked process proof. The pre-clone reap on a resumed run (phaseClone) keeps a
// survivor of the earlier attempt from reading the clone fetch's forge PAT. Linux only: the fold
// is gated on process.platform like the quiescence `processes` flag.

installHarness();

const GIT_ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];
const TIMEOUT = { timeout: 60_000 };
const RUNNER_OPTS = { checkpointIntervalMs: 0, recoveryRetryMs: 1 };

const INCOMPLETE: RunProcessReap = { killed: [], left: [], complete: false };
const LEFT: RunProcessReap = { killed: [], left: [4242], complete: true };
const COMPLETE: RunProcessReap = { killed: [1], left: [], complete: true };
const BLOCKING: Array<{ name: string; reap: RunProcessReap; reason: RegExp }> = [
  { name: "incomplete", reap: INCOMPLETE, reason: /HOME-attributed reap incomplete/ },
  { name: "left a survivor", reap: LEFT, reason: /HOME-attributed reap left 1 process\(es\): pids 4242/ },
];

/** A scripted attributed reap: answers `results` in call order (the last repeats) and counts calls. */
function reapScript(results: Array<RunProcessReap | Error>) {
  let calls = 0;
  return {
    calls: () => calls,
    fn: async (): Promise<RunProcessReap> => {
      const r = results[Math.min(calls, results.length - 1)]!;
      calls++;
      if (r instanceof Error) throw r;
      return r;
    },
  };
}

/** Give every executor `base` makes the scripted attributed reap. */
function withReap(base: ExecutorFactory, reap: { fn: () => Promise<RunProcessReap> }): ExecutorFactory {
  return (runId, codex) => {
    const made = base(runId, codex);
    return { ...made, executor: Object.assign(made.executor, { reapAttributedProcesses: reap.fn }) };
  };
}

/** The injected process proof: always quiescent, so only the attributed reap can block. */
function quiescentProof() {
  const calls: QuiesceRunRequest[] = [];
  const quiesceRun = async (req: QuiesceRunRequest): Promise<QuiesceRunOutcome> => {
    calls.push(req);
    return { process: { state: "quiescent", processes: [], killed: [], detail: "" }, docker: { state: "not_wired", removed: [], detail: "" } };
  };
  return { calls, quiesceRun };
}

function commitWork(tree: string): void {
  fs.writeFileSync(path.join(tree, "WORK.txt"), "work\n");
  execFileSync("git", ["-C", tree, "add", "WORK.txt"], { env: GIT_ENV, stdio: "pipe" });
  execFileSync("git", ["-C", tree, ...IDENT, "commit", "-m", "work"], { env: GIT_ENV, stdio: "pipe" });
}

function trackingSha(iid: number): string | null {
  try {
    return execFileSync("git", ["-C", git.barePathFor(fx.originPath), "rev-parse", "--verify", `refs/uzi-runner/agent/issue-${iid}`], {
      env: GIT_ENV,
      encoding: "utf8",
      stdio: "pipe",
    }).trim();
  } catch {
    return null;
  }
}

const statuses = (runId: string): string[] => api.states.filter((s) => s.runId === runId).map((s) => String(s.body.status));
const lastFailed = (runId: string) => api.states.filter((s) => s.runId === runId && s.body.status === "failed").at(-1)?.body;

function drain(stream: Readable): Promise<void> {
  return new Promise((resolve, reject) => {
    stream.on("data", () => undefined);
    stream.on("end", () => resolve());
    stream.on("error", reject);
  });
}

function spyPublish(): { calls: () => number } {
  let n = 0;
  (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = async (_runId: string, _tip: string, pack: Readable) => {
    await drain(pack);
    n += 1;
    return { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/agent/issue-x" } };
  };
  return { calls: () => n };
}

const finalizeFactory: ExecutorFactory = (runId) => ({
  homeDir: path.join(homeDir, runId),
  executor: { run: async (ctx: RunContext): Promise<ExecutorResult> => ({ branch: ctx.branch, summary: "done" }) },
});

// ─── finalize gate ─────────────────────────────────────────────────────────────────────────

describe("issue #1828: the finalize gate", { skip: process.platform !== "linux" }, () => {
  for (const c of BLOCKING) {
    it(`${c.name}: fails worker_residue_blocked, no push, no MR, the clone kept`, TIMEOUT, async () => {
      const { gitlab, calls: mrCalls } = fakeGitlab();
      const iid = 18280 + (c.reap === LEFT ? 1 : 0);
      simulateCommittedWork();
      const { quiesceRun } = quiescentProof();
      const reap = reapScript([c.reap]);
      const claim = gitlabClaim(iid);
      await runnerWith(withReap(finalizeFactory, reap), gitlab, undefined, undefined, { quiesceRun }).execute(claim);
      const failed = lastFailed(claim.run_id);
      assert.ok(failed, "the run fails");
      assert.equal(failed.fail_origin, "worker_residue_blocked");
      const reason = String(failed.failure_reason);
      assert.ok(reason.startsWith(`${REASON_WORKER_RESIDUE_BLOCKED}: `), reason);
      assert.match(reason, c.reason);
      assert.equal(mrCalls.length, 0, "nothing was published");
      assert.equal(trackingSha(iid), null, "nothing was pushed or fetched back");
      assert.equal(fs.existsSync(worktreeDirFor(iid)), true, "the clone is kept");
      assert.ok(reap.calls() >= 1, "the attributed reap ran");
    });
  }

  it("a rejecting reap reads as blocked, never as a throw out of quiesceRun", TIMEOUT, async () => {
    const { gitlab, calls: mrCalls } = fakeGitlab();
    const iid = 18282;
    simulateCommittedWork();
    const { quiesceRun } = quiescentProof();
    const claim = gitlabClaim(iid);
    await runnerWith(withReap(finalizeFactory, reapScript([new Error("proc exploded")])), gitlab, undefined, undefined, { quiesceRun }).execute(claim);
    const failed = lastFailed(claim.run_id);
    assert.equal(failed?.fail_origin, "worker_residue_blocked");
    assert.match(String(failed?.failure_reason), /HOME-attributed reap failed: proc exploded/);
    assert.equal(mrCalls.length, 0);
    assert.equal(fs.existsSync(worktreeDirFor(iid)), true, "the clone is kept");
  });

  it("control: a complete reap finalizes and retires the clone as before", TIMEOUT, async () => {
    const { gitlab, calls: mrCalls } = fakeGitlab();
    const iid = 18283;
    simulateCommittedWork();
    const { quiesceRun } = quiescentProof();
    const reap = reapScript([COMPLETE]);
    const claim = gitlabClaim(iid);
    await runnerWith(withReap(finalizeFactory, reap), gitlab, undefined, undefined, { quiesceRun }).execute(claim);
    assert.ok(statuses(claim.run_id).includes("completed"), statuses(claim.run_id).join(","));
    assert.equal(mrCalls.length, 1);
    assert.equal(fs.existsSync(worktreeDirFor(iid)), false);
    assert.ok(reap.calls() >= 1);
  });
});

// ─── generic failure settlement ────────────────────────────────────────────────────────────

/** An enabled recovery coordinator stand-in that counts the credentialed capture (the custody settle). */
function fakeRecovery(): { recovery: RecoveryCoordinator; captures: () => number } {
  let captures = 0;
  const recovery = {
    enabled: true,
    pin: async (input: { runId: string; sourceSha: string; branch: string; generation?: number }) => ({
      version: 1,
      runId: input.runId,
      captureId: "cap-1",
      sourceSha: input.sourceSha,
      kind: "issue",
      branch: input.branch,
      generation: input.generation,
    }),
    captureAndUpload: async () => {
      captures += 1;
      return { state: "needs_action", captureId: "cap-1", reason: "test" };
    },
    release: async () => undefined,
    inventoryHolds: async () => [],
    inspect: async () => [],
    resumePending: async () => undefined,
  };
  return { recovery: recovery as unknown as RecoveryCoordinator, captures: () => captures };
}

const crashFactory: ExecutorFactory = (runId) => ({
  homeDir: path.join(homeDir, runId),
  executor: {
    run: async (ctx: RunContext): Promise<ExecutorResult> => {
      commitWork(ctx.worktreePath);
      throw new Error("agent crashed");
    },
  },
});

describe("issue #1828: generic failure settlement", { skip: process.platform !== "linux" }, () => {
  for (const c of BLOCKING) {
    it(`${c.name}: the custody settle is not called, the clone is kept`, TIMEOUT, async () => {
      const { gitlab } = fakeGitlab();
      const iid = 18290 + (c.reap === LEFT ? 1 : 0);
      const { recovery, captures } = fakeRecovery();
      const { quiesceRun } = quiescentProof();
      const claim = gitlabClaim(iid);
      await runnerWith(withReap(crashFactory, reapScript([c.reap])), gitlab, undefined, undefined, { ...RUNNER_OPTS, quiesceRun, recovery }).execute(claim);
      assert.ok(statuses(claim.run_id).includes("failed"));
      assert.equal(captures(), 0, "no credentialed capture/settle ran");
      assert.equal(fs.existsSync(path.join(worktreeDirFor(iid), "WORK.txt")), true, "the clone is kept");
    });
  }

  it("control: a complete reap lets the settle run", TIMEOUT, async () => {
    const { gitlab } = fakeGitlab();
    const iid = 18292;
    const { recovery, captures } = fakeRecovery();
    const { quiesceRun } = quiescentProof();
    const claim = gitlabClaim(iid);
    await runnerWith(withReap(crashFactory, reapScript([COMPLETE])), gitlab, undefined, undefined, { ...RUNNER_OPTS, quiesceRun, recovery }).execute(claim);
    assert.ok(statuses(claim.run_id).includes("failed"));
    assert.equal(captures(), 1, "the credentialed settle ran once");
  });
});

// ─── owner pause ───────────────────────────────────────────────────────────────────────────

interface PauseSnapshot {
  parked: boolean | undefined;
  published: number;
  tracking: string | null;
}

function pauseFactory(iid: number, pub: { calls: () => number }, seen: PauseSnapshot[]): ExecutorFactory {
  return (runId) => ({
    homeDir: path.join(homeDir, runId),
    executor: {
      run: async (ctx: RunContext): Promise<ExecutorResult> => {
        commitWork(ctx.worktreePath);
        const at = { completedCount: 1, total: 2 };
        const p = await ctx.parkForPause?.(at);
        seen.push({ parked: p, published: pub.calls(), tracking: trackingSha(iid) });
        return p ? { branch: ctx.branch, pausedAt: at } : { branch: ctx.branch };
      },
    },
  });
}

describe("issue #1828: the owner pause", { skip: process.platform !== "linux" }, () => {
  for (const c of BLOCKING) {
    it(`${c.name}: pause_failed (never paused), nothing published, the run continues`, TIMEOUT, async () => {
      const { gitlab } = fakeGitlab();
      const iid = 18300 + (c.reap === LEFT ? 1 : 0);
      const pub = spyPublish();
      const { quiesceRun } = quiescentProof();
      // Blocked at the pause sink only; the finalize gate then passes like any pause_failed run.
      const reap = reapScript([c.reap, COMPLETE]);
      const seen: PauseSnapshot[] = [];
      const claim = gitlabClaim(iid);
      await runnerWith(withReap(pauseFactory(iid, pub, seen), reap), gitlab, undefined, undefined, { ...RUNNER_OPTS, quiesceRun }).execute(claim);
      assert.deepEqual(seen, [{ parked: false, published: 0, tracking: null }]);
      const st = statuses(claim.run_id);
      assert.ok(st.includes("pause_failed"), st.join(","));
      assert.ok(!st.includes("paused"), st.join(","));
    });
  }

  it("control: a complete reap publishes the checkpoint and parks", TIMEOUT, async () => {
    const { gitlab } = fakeGitlab();
    const iid = 18302;
    const pub = spyPublish();
    const { quiesceRun } = quiescentProof();
    const seen: PauseSnapshot[] = [];
    const claim = gitlabClaim(iid);
    await runnerWith(withReap(pauseFactory(iid, pub, seen), reapScript([COMPLETE])), gitlab, undefined, undefined, { ...RUNNER_OPTS, quiesceRun }).execute(claim);
    assert.deepEqual(seen.map((s) => s.parked), [true]);
    assert.ok(statuses(claim.run_id).includes("paused"));
    assert.ok(pub.calls() >= 1);
  });
});

// ─── wall park and completion hold ─────────────────────────────────────────────────────────

function wallFactory(outcomes: unknown[]): ExecutorFactory {
  return (runId) => ({
    homeDir: path.join(homeDir, runId),
    executor: {
      run: async (ctx: RunContext): Promise<ExecutorResult> => {
        commitWork(ctx.worktreePath);
        const o = await ctx.parkForWall?.({ completedCount: 1, total: 2 });
        outcomes.push(o);
        return o === "parked" || o === "undeliverable"
          ? { branch: ctx.branch, walled: { reason: "run exceeded its wall-clock timeout" } }
          : { branch: ctx.branch };
      },
    },
  });
}

function holdFactory(held: Array<boolean | undefined>): ExecutorFactory {
  return (runId) => ({
    homeDir: path.join(homeDir, runId),
    executor: {
      run: async (ctx: RunContext): Promise<ExecutorResult> => {
        commitWork(ctx.worktreePath);
        const h = await ctx.enterCompletionHold!("test hold");
        held.push(h);
        if (h) return { branch: ctx.branch, completionHeld: { reason: "test hold" } };
        throw new Error("legacy terminal (could not hold)");
      },
    },
  });
}

describe("issue #1828: the wall park and the completion hold", { skip: process.platform !== "linux" }, () => {
  for (const c of BLOCKING) {
    it(`wall park, ${c.name}: parked with an empty head, nothing published, never failed`, TIMEOUT, async () => {
      const { gitlab } = fakeGitlab();
      const iid = 18310 + (c.reap === LEFT ? 1 : 0);
      const pub = spyPublish();
      const { quiesceRun } = quiescentProof();
      const outcomes: unknown[] = [];
      const claim = gitlabClaim(iid);
      await runnerWith(withReap(wallFactory(outcomes), reapScript([c.reap])), gitlab, undefined, undefined, { ...RUNNER_OPTS, quiesceRun }).execute(claim);
      assert.deepEqual(outcomes, ["parked"], "the wall park stands");
      assert.deepEqual(api.wallParkRequests[0]!.body, { ...api.wallParkRequests[0]!.body, head: "", published: false });
      assert.ok(!statuses(claim.run_id).includes("failed"));
      assert.equal(pub.calls(), 0);
      assert.equal(trackingSha(iid), null);
    });

    it(`completion hold, ${c.name}: held with an empty head, nothing published, never failed`, TIMEOUT, async () => {
      const { gitlab } = fakeGitlab();
      const iid = 18320 + (c.reap === LEFT ? 1 : 0);
      api.setCompletionHoldResponse("paused", 200);
      const pub = spyPublish();
      const { quiesceRun } = quiescentProof();
      const held: Array<boolean | undefined> = [];
      const claim = gitlabClaim(iid);
      await runnerWith(withReap(holdFactory(held), reapScript([c.reap])), gitlab, undefined, undefined, { ...RUNNER_OPTS, quiesceRun }).execute(claim);
      assert.deepEqual(held, [true], "the hold stands");
      assert.equal((api.completionHoldRequests[0]!.body as { head: string }).head, "");
      assert.ok(!statuses(claim.run_id).includes("failed"));
      assert.equal(pub.calls(), 0);
      assert.equal(trackingSha(iid), null);
    });
  }

  it("control: a complete reap lets the wall park capture and publish a head", TIMEOUT, async () => {
    const { gitlab } = fakeGitlab();
    const iid = 18312;
    const pub = spyPublish();
    const { quiesceRun } = quiescentProof();
    const outcomes: unknown[] = [];
    await runnerWith(withReap(wallFactory(outcomes), reapScript([COMPLETE])), gitlab, undefined, undefined, { ...RUNNER_OPTS, quiesceRun }).execute(gitlabClaim(iid));
    assert.deepEqual(outcomes, ["parked"]);
    const body = api.wallParkRequests[0]!.body as { head: string; published: boolean };
    assert.equal(body.head.length, 40);
    assert.equal(body.published, true);
    assert.ok(pub.calls() >= 1);
  });

  it("control: a complete reap lets the completion hold capture and publish a head", TIMEOUT, async () => {
    const { gitlab } = fakeGitlab();
    const iid = 18322;
    api.setCompletionHoldResponse("paused", 200);
    const pub = spyPublish();
    const { quiesceRun } = quiescentProof();
    const held: Array<boolean | undefined> = [];
    await runnerWith(withReap(holdFactory(held), reapScript([COMPLETE])), gitlab, undefined, undefined, { ...RUNNER_OPTS, quiesceRun }).execute(gitlabClaim(iid));
    assert.deepEqual(held, [true]);
    assert.equal((api.completionHoldRequests[0]!.body as { head: string }).head.length, 40);
    assert.ok(pub.calls() >= 1);
  });
});

// ─── recovery capture (captureRecoveryRestorePoint) ────────────────────────────────────────

const recoveryFactory: ExecutorFactory = (runId) => ({
  homeDir: path.join(homeDir, runId),
  executor: {
    run: async (ctx: RunContext): Promise<ExecutorResult> => {
      fs.writeFileSync(path.join(ctx.worktreePath, "ONLY_COPY.txt"), "uncommitted work\n");
      throw new TransientRecoveryError();
    },
  },
});

describe("issue #1828: the recovery capture", { skip: process.platform !== "linux" }, () => {
  for (const c of BLOCKING) {
    it(`${c.name}: the capture is not verified (residueBlocked): the run fails worker_residue_blocked, nothing parked`, TIMEOUT, async () => {
      const { gitlab, calls: mrCalls } = fakeGitlab();
      const iid = 18330 + (c.reap === LEFT ? 1 : 0);
      const { calls, quiesceRun } = quiescentProof();
      const claim = gitlabClaim(iid, { claim_generation: 3 });
      await runnerWith(withReap(recoveryFactory, reapScript([c.reap])), gitlab, undefined, nullLogger(), { ...RUNNER_OPTS, quiesceRun }).execute(claim);
      const st = statuses(claim.run_id);
      assert.ok(!st.includes("recovery_wait"), `never parked over an uncaptured clone: ${st.join(",")}`);
      const failed = lastFailed(claim.run_id);
      assert.equal(failed?.fail_origin, "worker_residue_blocked");
      assert.match(String(failed?.failure_reason), c.reason);
      assert.ok(calls.some((q) => q.site === "recovery_capture"), "the capture proof ran");
      assert.equal(trackingSha(iid), null, "nothing was fetched back");
      assert.equal(mrCalls.length, 0);
      assert.equal(fs.existsSync(path.join(worktreeDirFor(iid), "ONLY_COPY.txt")), true, "the uncommitted work is kept");
    });
  }

  it("control: a complete reap verifies the capture and parks", TIMEOUT, async () => {
    const { gitlab } = fakeGitlab();
    const iid = 18332;
    const { quiesceRun } = quiescentProof();
    const claim = gitlabClaim(iid, { claim_generation: 3 });
    await runnerWith(withReap(recoveryFactory, reapScript([COMPLETE])), gitlab, undefined, nullLogger(), { ...RUNNER_OPTS, quiesceRun }).execute(claim);
    const st = statuses(claim.run_id);
    assert.ok(st.includes("recovery_wait"), st.join(","));
    assert.ok(!st.includes("failed"), st.join(","));
  });
});

// ─── predecessor capture ───────────────────────────────────────────────────────────────────

function fixtureSeed(attemptId: string): AttemptSeedOptions {
  return { attemptId, isLive: () => false, beforeSeed: async () => {}, quiescent: async () => true };
}

/** A predecessor attempt as a worker that died mid-run leaves it: an attempt clone with
 *  uncommitted work, journaled for capture. */
async function seedPredecessor(iid: number, runId: string): Promise<string> {
  const b = await git.ensureClone(fx.originPath);
  const attemptId = mintAttemptId(1);
  const clone = await git.createOrAttachRunnerClone(b, iid, noProofReseed, runId, false, undefined, fixtureSeed(attemptId));
  fs.writeFileSync(path.join(clone.path, "ONLY_COPY.txt"), "must survive recovery\n");
  await git.markRecoveryCapture(b, clone.path, `agent/issue-${iid}`, runId, clone.attemptId);
  return clone.path;
}

describe("issue #1828: the predecessor capture", { skip: process.platform !== "linux" }, () => {
  for (const c of BLOCKING) {
    it(`${c.name}: the capture proof is blocked, no model starts, the predecessor is untouched`, TIMEOUT, async () => {
      const iid = 18340 + (c.reap === LEFT ? 1 : 0);
      const runId = randomUUID();
      const clonePath = await seedPredecessor(iid, runId);
      let started = 0;
      const factory: ExecutorFactory = (id) => ({
        homeDir: path.join(homeDir, id),
        executor: {
          run: async (): Promise<ExecutorResult> => {
            started++;
            throw new Error("must not start");
          },
        },
      });
      const { gitlab } = fakeGitlab();
      const { calls, quiesceRun } = quiescentProof();
      await runnerWith(withReap(factory, reapScript([c.reap])), gitlab, undefined, undefined, {
        ...RUNNER_OPTS,
        quiesceRun,
        dockerHost: "unix:///nonexistent-docker.sock",
        liveAttempts: new LiveAttemptRegistry(),
      }).execute(gitlabClaim(iid, { run_id: runId }));
      assert.equal(calls[0]?.site, "predecessor_capture");
      assert.equal(calls[0]?.mode, "capture");
      assert.equal(lastFailed(runId)?.fail_origin, "worker_residue_blocked");
      assert.match(String(lastFailed(runId)?.failure_reason), c.reason);
      assert.equal(started, 0);
      assert.equal(fs.existsSync(path.join(clonePath, "ONLY_COPY.txt")), true);
      assert.equal(trackingSha(iid), null, "no fetch-back ran");
      assert.ok(!statuses(runId).includes("recovery_wait"));
    });
  }

  it("control: a complete reap lets the predecessor capture verify and park", TIMEOUT, async () => {
    const iid = 18342;
    const runId = randomUUID();
    await seedPredecessor(iid, runId);
    const factory: ExecutorFactory = (id) => ({
      homeDir: path.join(homeDir, id),
      executor: { run: async (): Promise<ExecutorResult> => { throw new Error("must not start"); } },
    });
    const { gitlab } = fakeGitlab();
    const { quiesceRun } = quiescentProof();
    await runnerWith(withReap(factory, reapScript([COMPLETE])), gitlab, undefined, undefined, {
      ...RUNNER_OPTS,
      quiesceRun,
      dockerHost: "unix:///nonexistent-docker.sock",
      liveAttempts: new LiveAttemptRegistry(),
    }).execute(gitlabClaim(iid, { run_id: runId }));
    assert.ok(statuses(runId).includes("recovery_wait"), statuses(runId).join(","));
    assert.notEqual(trackingSha(iid), null, "the predecessor's work was fetched back");
  });
});

// ─── pre-clone resume reap ─────────────────────────────────────────────────────────────────

/** Count git.ensureClone calls and snapshot the attributed-reap call count at each one. */
function spyEnsureClone(reap: { calls: () => number }) {
  const realEnsure = git.ensureClone.bind(git);
  const reapCallsAtClone: number[] = [];
  git.ensureClone = (async (...a: Parameters<typeof realEnsure>) => {
    reapCallsAtClone.push(reap.calls());
    return realEnsure(...a);
  }) as typeof git.ensureClone;
  return { reapCallsAtClone, restore: () => { git.ensureClone = realEnsure; } };
}

describe("issue #1828: the pre-clone reap of a resumed run", { skip: process.platform !== "linux" }, () => {
  for (const c of BLOCKING) {
    it(`${c.name}: HOME exists: ensureClone is not called, the run fails worker_residue_blocked (not a forge park)`, TIMEOUT, async () => {
      const { gitlab, calls: mrCalls } = fakeGitlab();
      const iid = 18350 + (c.reap === LEFT ? 1 : 0);
      const claim = gitlabClaim(iid);
      fs.mkdirSync(path.join(homeDir, claim.run_id), { recursive: true });
      const { quiesceRun } = quiescentProof();
      const reap = reapScript([c.reap]);
      const spy = spyEnsureClone(reap);
      let started = 0;
      const factory: ExecutorFactory = (runId) => ({
        homeDir: path.join(homeDir, runId),
        executor: { run: async (): Promise<ExecutorResult> => { started++; throw new Error("must not start"); } },
      });
      try {
        await runnerWith(withReap(factory, reap), gitlab, undefined, undefined, { ...RUNNER_OPTS, quiesceRun }).execute(claim);
      } finally {
        spy.restore();
      }
      assert.deepEqual(spy.reapCallsAtClone, [], "ensureClone (the PAT-bearing fetch) never ran");
      assert.equal(started, 0);
      const failed = lastFailed(claim.run_id);
      assert.equal(failed?.fail_origin, "worker_residue_blocked");
      assert.match(String(failed?.failure_reason), c.reason);
      assert.ok(!statuses(claim.run_id).includes("recovery_wait"), "not classified as a forge-unreachable park");
      assert.equal(mrCalls.length, 0);
    });
  }

  it("control: HOME exists and the reap is complete: it ran once before the clone, and the run proceeds", TIMEOUT, async () => {
    const { gitlab } = fakeGitlab();
    const iid = 18352;
    simulateCommittedWork();
    const claim = gitlabClaim(iid);
    fs.mkdirSync(path.join(homeDir, claim.run_id), { recursive: true });
    const { quiesceRun } = quiescentProof();
    const reap = reapScript([COMPLETE]);
    const spy = spyEnsureClone(reap);
    try {
      await runnerWith(withReap(finalizeFactory, reap), gitlab, undefined, undefined, { quiesceRun }).execute(claim);
    } finally {
      spy.restore();
    }
    assert.deepEqual(spy.reapCallsAtClone, [1], "one reap preceded the clone fetch");
    assert.ok(statuses(claim.run_id).includes("completed"), statuses(claim.run_id).join(","));
  });

  it("control: no HOME on disk (a fresh run): the reap is not called before the clone", TIMEOUT, async () => {
    const { gitlab } = fakeGitlab();
    const iid = 18353;
    simulateCommittedWork();
    const claim = gitlabClaim(iid);
    assert.equal(fs.existsSync(path.join(homeDir, claim.run_id)), false);
    const { quiesceRun } = quiescentProof();
    const reap = reapScript([LEFT]);
    const spy = spyEnsureClone(reap);
    try {
      await runnerWith(withReap(finalizeFactory, reap), gitlab, undefined, undefined, { quiesceRun }).execute(claim);
    } finally {
      spy.restore();
    }
    assert.deepEqual(spy.reapCallsAtClone, [0], "ensureClone ran with no reap before it, though a reap would have blocked");
  });
});
