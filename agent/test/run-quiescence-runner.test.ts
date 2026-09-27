import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import type { ExecutorResult, RunContext } from "../src/executor.js";
import { failOriginForReason, REASON_WORKER_RESIDUE_BLOCKED, type ExecutorFactory } from "../src/runner.js";
import { LimitReachedError } from "../src/limit.js";
import {
  LiveAttemptRegistry,
  type ProcessQuiescenceState,
  type QuiesceRunOutcome,
  type QuiesceRunRequest,
} from "../src/run-quiescence.js";
import { api, fakeGitlab, fx, git, gitlabClaim, homeDir, installHarness, runnerWith, simulateCommittedWork, worktreeDirFor } from "./runner-harness.js";

// issue #1783 — the runner fails CLOSED on a clone it cannot prove quiescent, identically for
// `survivors` and `unverified`: at a park (the credentialed sink body is skipped, the park stands),
// at the terminal retire (the clone and its recovery journal are kept) and at the finalize boundary
// (the run fails with fail_origin worker_residue_blocked, the clone kept). The quiescence primitive
// itself is exercised against real processes in run-quiescence.test.ts; here it is injected.

installHarness();

const GIT_ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];

function journal(iid: number): string | undefined {
  try {
    const raw = execFileSync("git", ["-C", git.barePathFor(fx.originPath), "config", "--local", "--get", `uzi-recovery.agent/issue-${iid}.clone`], {
      env: GIT_ENV,
      encoding: "utf8",
      stdio: "pipe",
    }).trim();
    return raw || undefined;
  } catch {
    return undefined;
  }
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

/** A quiescer that answers `states` in call order (the last one repeats) and records requests. */
function scriptedQuiescer(states: Array<ProcessQuiescenceState>) {
  const calls: QuiesceRunRequest[] = [];
  const quiesceRun = async (req: QuiesceRunRequest): Promise<QuiesceRunOutcome> => {
    const state = states[Math.min(calls.length, states.length - 1)]!;
    calls.push(req);
    return {
      process: {
        state,
        processes: state === "quiescent" ? [] : [{ pid: 4242, uid: 10002, comm: "sleep", cwd: req.targetPaths[0]!, reason: "own_attempt:kill_unconfirmed" }],
        killed: [],
        detail: `scripted ${state}`,
      },
      docker: { state: "not_wired", removed: [], detail: "" },
    };
  };
  return { calls, quiesceRun };
}

const NOT_QUIESCENT: ProcessQuiescenceState[] = ["survivors", "unverified"];

describe("issue #1783: fail_origin mapping", () => {
  it("maps the residue reason prefix to worker_residue_blocked", () => {
    assert.equal(failOriginForReason(`${REASON_WORKER_RESIDUE_BLOCKED}: the run's clone could not be proven quiescent`), "worker_residue_blocked");
    assert.equal(failOriginForReason("some agent failure"), undefined);
  });
});

describe("issue #1783: park fails closed (A-park-fail-closed)", () => {
  for (const state of NOT_QUIESCENT) {
    it(`${state}: the sink body is skipped, the tracking ref and journal are untouched, the clone is kept, the park stands`, async () => {
      const { gitlab } = fakeGitlab();
      const iid = 1783 + (state === "survivors" ? 0 : 1);
      const { calls, quiesceRun } = scriptedQuiescer([state]);
      let wipCalls = 0;
      const realWip = git.commitWipMarker.bind(git);
      git.commitWipMarker = (async (...a: Parameters<typeof realWip>) => {
        wipCalls++;
        return realWip(...a);
      }) as typeof git.commitWipMarker;
      let marker: string | undefined;
      const factory: ExecutorFactory = (runId) => ({
        homeDir: path.join(homeDir, runId),
        executor: {
          run: async (ctx: RunContext): Promise<ExecutorResult> => {
            marker = ctx.runAttempt?.marker;
            fs.writeFileSync(path.join(ctx.worktreePath, "WORK.txt"), "work\n");
            execFileSync("git", ["-C", ctx.worktreePath, "add", "WORK.txt"], { env: GIT_ENV, stdio: "pipe" });
            execFileSync("git", ["-C", ctx.worktreePath, ...IDENT, "commit", "-m", "work"], { env: GIT_ENV, stdio: "pipe" });
            throw new LimitReachedError({ resetsAtMs: Date.now() + 5 * 3600_000, rateLimitType: "five_hour" });
          },
        },
      });
      await runnerWith(factory, gitlab, undefined, undefined, { quiesceRun }).execute(gitlabClaim(iid, { wait_on_limit: true }));

      assert.ok(api.states.some((s) => s.body.status === "limit_wait"), "the park still stands");
      assert.equal(calls[0]?.processes, process.platform === "linux");
      assert.equal(calls[0]?.attempt?.marker, marker, "the flight's own attempt marker is the one quiesced");
      assert.ok(marker && /^.+:[0-9]{8}T[0-9]{6}Z-(g[0-9]+|gx)-[0-9a-f]{16}$/.test(marker), "ctx carries the attempt marker");
      assert.equal(wipCalls, 0, "commitWipMarker (the sink body) never ran");
      assert.equal(trackingSha(iid), null, "no fetch-back: the bare tracking ref is untouched");
      assert.ok(journal(iid), "the recovery journal still points at the kept clone");
      assert.equal(fs.existsSync(worktreeDirFor(iid)), true, "the clone is kept");
      assert.equal(fs.existsSync(path.join(worktreeDirFor(iid), "WORK.txt")), true);
    });
  }
});

describe("issue #1783: terminal retire fails closed", () => {
  for (const state of NOT_QUIESCENT) {
    it(`${state}: a failed run keeps its clone and journal`, async () => {
      const { gitlab } = fakeGitlab();
      const iid = 1790 + (state === "survivors" ? 0 : 1);
      const { calls, quiesceRun } = scriptedQuiescer([state]);
      const liveAttempts = new LiveAttemptRegistry();
      const factory: ExecutorFactory = (runId) => ({
        homeDir: path.join(homeDir, runId),
        executor: {
          run: async (): Promise<ExecutorResult> => {
            assert.equal(liveAttempts.others(undefined).length, 1, "the attempt is live while the agent runs");
            throw new Error("agent crashed");
          },
        },
      });
      await runnerWith(factory, gitlab, undefined, undefined, { quiesceRun, liveAttempts }).execute(gitlabClaim(iid));
      assert.ok(api.states.some((s) => s.body.status === "failed"));
      assert.equal(calls.length, 1, "one quiescence proof, before the retire");
      assert.equal(fs.existsSync(worktreeDirFor(iid)), true, "the clone is kept");
      assert.ok(journal(iid), "the journal is kept");
      assert.equal(liveAttempts.others(undefined).length, 0, "the attempt left the live set after its final reap");
    });
  }

  it("control: a quiescent clone is retired as before", async () => {
    const { gitlab } = fakeGitlab();
    const iid = 1795;
    const { calls, quiesceRun } = scriptedQuiescer(["quiescent"]);
    const factory: ExecutorFactory = (runId) => ({
      homeDir: path.join(homeDir, runId),
      executor: { run: async (): Promise<ExecutorResult> => { throw new Error("agent crashed"); } },
    });
    await runnerWith(factory, gitlab, undefined, undefined, { quiesceRun }).execute(gitlabClaim(iid));
    assert.equal(calls.length, 1);
    assert.equal(fs.existsSync(worktreeDirFor(iid)), false, "the clone is retired");
    assert.equal(journal(iid), undefined);
  });
});

describe("issue #1783: finalize boundary fails closed", () => {
  for (const state of NOT_QUIESCENT) {
    it(`${state}: the run fails with fail_origin worker_residue_blocked, no MR, the clone kept`, async () => {
      const { gitlab, calls: mrCalls } = fakeGitlab();
      const iid = 1800 + (state === "survivors" ? 0 : 1);
      simulateCommittedWork();
      const { calls, quiesceRun } = scriptedQuiescer([state]);
      const factory: ExecutorFactory = (runId) => ({
        homeDir: path.join(homeDir, runId),
        executor: { run: async (ctx: RunContext): Promise<ExecutorResult> => ({ branch: ctx.branch, summary: "done" }) },
      });
      await runnerWith(factory, gitlab, undefined, undefined, { quiesceRun }).execute(gitlabClaim(iid));
      const failed = api.states.filter((s) => s.body.status === "failed").at(-1)?.body;
      assert.ok(failed, "the run fails");
      assert.equal(failed.fail_origin, "worker_residue_blocked");
      assert.ok(String(failed.failure_reason).startsWith(`${REASON_WORKER_RESIDUE_BLOCKED}: `));
      assert.equal(mrCalls.length, 0, "nothing was published");
      assert.equal(calls.length, 1, "the terminal finally does not re-quiesce a clone it keeps");
      assert.equal(fs.existsSync(worktreeDirFor(iid)), true, "the clone is kept");
    });
  }

  it("control: a quiescent clone finalizes and is retired as before", async () => {
    const { gitlab, calls: mrCalls } = fakeGitlab();
    const iid = 1805;
    simulateCommittedWork();
    const { calls, quiesceRun } = scriptedQuiescer(["quiescent"]);
    const factory: ExecutorFactory = (runId) => ({
      homeDir: path.join(homeDir, runId),
      executor: { run: async (ctx: RunContext): Promise<ExecutorResult> => ({ branch: ctx.branch, summary: "done" }) },
    });
    await runnerWith(factory, gitlab, undefined, undefined, { quiesceRun }).execute(gitlabClaim(iid));
    assert.ok(api.states.some((s) => s.body.status === "completed"), "the run completes");
    assert.equal(mrCalls.length, 1);
    assert.equal(calls.length, 2, "finalize, then terminal retire");
    assert.equal(fs.existsSync(worktreeDirFor(iid)), false);
  });
});
