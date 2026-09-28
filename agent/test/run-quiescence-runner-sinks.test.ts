import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import type { Readable } from "node:stream";
import { execFileSync } from "node:child_process";
import type { ExecutorResult, RunContext } from "../src/executor.js";
import type { ExecutorFactory } from "../src/runner.js";
import type { ProcessQuiescenceState, QuiesceRunOutcome, QuiesceRunRequest } from "../src/run-quiescence.js";
import { TransientRecoveryError } from "../src/sdk-executor.js";
import { CredentialSwitchSignal } from "../src/steering.js";
import { api, client, deferred, fakeGitlab, fx, git, gitlabClaim, homeDir, installHarness, runnerWith, worktreeDirFor } from "./runner-harness.js";

// issue #1783 (auditor M2 / reviewer N2) — the four credentialed paths the first rework left
// ungated now prove the clone quiescent (the process proof, the flight threaded through) before
// their credentialed part (the overlay's PAT fetch and the publish), and on survivors/unverified
// skip that part while keeping custody (the clone, the journal, the tracking ref), mapped onto
// each path's existing failure semantics:
//   - graceful shutdown:          "nothing published" (a blocked boundary); the requeue stands;
//   - milestone checkpoint:       that checkpoint's publish is skipped; the run continues;
//   - recovery-exhausted capture: the existing capture-failure branch (retain + retry);
//   - credential-switch capture:  the existing capture-failure branch (retry, then give up).
// Each path under survivors and under unverified, plus a quiescent control. The quiescence
// primitive is injected; every other part (git, the clone, the tracking ref) is real.

installHarness();

const GIT_ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];
const NOT_QUIESCENT: ProcessQuiescenceState[] = ["survivors", "unverified"];
const RUNNER_OPTS = { checkpointIntervalMs: 0, recoveryRetryMs: 1 };

function commitWork(tree: string): void {
  fs.writeFileSync(path.join(tree, "WORK.txt"), "work\n");
  execFileSync("git", ["-C", tree, "add", "WORK.txt"], { env: GIT_ENV, stdio: "pipe" });
  execFileSync("git", ["-C", tree, ...IDENT, "commit", "-m", "work"], { env: GIT_ENV, stdio: "pipe" });
}

function headSubject(tree: string): string {
  return execFileSync("git", ["-C", tree, "log", "-1", "--format=%s"], { env: GIT_ENV, encoding: "utf8", stdio: "pipe" }).trim();
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

/**
 * A quiescer that answers `blocked` for its first `blockedCalls` proofs (Infinity = always) and
 * `quiescent` after. `inBlockedPhase()` is true until a quiescent answer has been given, so a spy
 * can tell whether a credentialed step ran before the clone was proven quiescent.
 */
function phasedQuiescer(blocked: ProcessQuiescenceState, blockedCalls: number) {
  const calls: QuiesceRunRequest[] = [];
  let cleared = blocked === "quiescent";
  const quiesceRun = async (req: QuiesceRunRequest): Promise<QuiesceRunOutcome> => {
    calls.push(req);
    const state: ProcessQuiescenceState = calls.length <= blockedCalls ? blocked : "quiescent";
    if (state === "quiescent") cleared = true;
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
  return { calls, quiesceRun, inBlockedPhase: () => !cleared };
}

function drain(stream: Readable): Promise<void> {
  return new Promise((resolve, reject) => {
    stream.on("data", () => undefined);
    stream.on("end", () => resolve());
    stream.on("error", reject);
  });
}

/** Spy the checkpoint publish; record whether each call ran before quiescence was proven. */
function spyPublish(inBlockedPhase: () => boolean): { calls: () => number; blockedCalls: () => number } {
  let n = 0;
  let early = 0;
  (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = async (_runId: string, _tip: string, pack: Readable) => {
    if (inBlockedPhase()) early += 1;
    await drain(pack);
    n += 1;
    return { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/agent/issue-x" } };
  };
  return { calls: () => n, blockedCalls: () => early };
}

const statuses = (runId: string): string[] => api.states.filter((s) => s.runId === runId).map((s) => String(s.body.status));
const statusTexts = (runId: string): string[] =>
  api
    .messages(runId)
    .filter((m) => m.kind === "status")
    .map((m) => String(m.payload.text));

// ─── graceful shutdown ─────────────────────────────────────────────────────────────────────

function waitAbort(signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    if (signal.aborted) return resolve();
    signal.addEventListener("abort", () => resolve(), { once: true });
  });
}

function shutdownFactory(started: { resolve: () => void }): ExecutorFactory {
  return (runId) => ({
    homeDir: path.join(homeDir, runId),
    executor: {
      run: async (ctx: RunContext): Promise<ExecutorResult> => {
        commitWork(ctx.worktreePath);
        // Uncommitted work too: a sink body that ran would wip-commit it (a runner-clone git).
        fs.writeFileSync(path.join(ctx.worktreePath, "UNCOMMITTED.txt"), "in progress\n");
        started.resolve();
        await waitAbort(ctx.signal!);
        throw new Error("aborted mid-run");
      },
    },
  });
}

describe("issue #1783 M2: the graceful-shutdown sink is gated (nothing published when not quiescent)", () => {
  for (const state of NOT_QUIESCENT) {
    it(`${state}: nothing published, the feed names boundary_blocked, no terminal report, the clone and tracking ref kept`, async () => {
      const { gitlab } = fakeGitlab();
      const iid = 1860 + (state === "survivors" ? 0 : 1);
      const q = phasedQuiescer(state, Infinity);
      const pub = spyPublish(q.inBlockedPhase);
      const started = deferred();
      const runner = runnerWith(shutdownFactory(started), gitlab, undefined, undefined, { ...RUNNER_OPTS, quiesceRun: q.quiesceRun });
      const claim = gitlabClaim(iid);
      const p = runner.execute(claim);
      await started.promise;
      runner.shutdown();
      await p;
      assert.ok(q.calls.some((c) => c.mode === "own"), "the shutdown sink ran the proof");
      assert.equal(pub.calls(), 0, "no checkpoint published");
      assert.ok(
        statusTexts(claim.run_id).some((t) => t.startsWith("shutdown checkpoint NOT published (reason: boundary_blocked)")),
        JSON.stringify(statusTexts(claim.run_id)),
      );
      assert.ok(!statuses(claim.run_id).some((s) => s === "failed" || s === "completed"), "the run stays non-terminal for requeue");
      assert.equal(trackingSha(iid), null, "no fetch-back: the tracking ref is untouched");
      assert.equal(fs.existsSync(path.join(worktreeDirFor(iid), "WORK.txt")), true, "the clone is kept");
      assert.equal(headSubject(worktreeDirFor(iid)), "work", "no runner-clone git ran before the proof (no wip marker)");
    });
  }

  it("quiescent: the shutdown checkpoint is published", async () => {
    const { gitlab } = fakeGitlab();
    const q = phasedQuiescer("quiescent", 0);
    const pub = spyPublish(q.inBlockedPhase);
    const started = deferred();
    const runner = runnerWith(shutdownFactory(started), gitlab, undefined, undefined, { ...RUNNER_OPTS, quiesceRun: q.quiesceRun });
    const claim = gitlabClaim(1862);
    const p = runner.execute(claim);
    await started.promise;
    runner.shutdown();
    await p;
    assert.equal(pub.calls(), 1);
    assert.ok(statusTexts(claim.run_id).includes("shutdown checkpoint published to origin"));
    assert.ok(q.calls.length >= 2, "the proof, then the re-proof after the wip marker");
  });
});

// ─── milestone checkpoint ──────────────────────────────────────────────────────────────────

interface MilestoneSnapshot {
  published: number;
  tracking: string | null;
}

function milestoneFactory(iid: number, pub: { calls: () => number }, seen: MilestoneSnapshot[]): ExecutorFactory {
  return (runId) => ({
    homeDir: path.join(homeDir, runId),
    executor: {
      run: async (ctx: RunContext): Promise<ExecutorResult> => {
        commitWork(ctx.worktreePath);
        await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] } });
        seen.push({ published: pub.calls(), tracking: trackingSha(iid) });
        return { branch: ctx.branch, summary: "done" };
      },
    },
  });
}

describe("issue #1783 M2: the milestone checkpoint is gated (its publish skipped, the run continues)", () => {
  for (const state of NOT_QUIESCENT) {
    it(`${state}: the checkpoint publishes nothing and fetches nothing back, and the run continues to completion`, async () => {
      const { gitlab } = fakeGitlab();
      const iid = 1865 + (state === "survivors" ? 0 : 1);
      // Blocked at the milestone (the first proof) only, so the continuing run finalizes cleanly.
      const q = phasedQuiescer(state, 1);
      const pub = spyPublish(q.inBlockedPhase);
      const seen: MilestoneSnapshot[] = [];
      const claim = gitlabClaim(iid);
      await runnerWith(milestoneFactory(iid, pub, seen), gitlab, undefined, undefined, { ...RUNNER_OPTS, quiesceRun: q.quiesceRun }).execute(claim);
      assert.equal(seen.length, 1, "ctx.checkpoint returned and the executor continued");
      assert.deepEqual(seen[0], { published: 0, tracking: null }, "no publish and no fetch-back at the blocked milestone");
      assert.equal(pub.blockedCalls(), 0, "no publish ran before the clone was proven quiescent");
      assert.ok(statuses(claim.run_id).includes("completed"), `the run continued: ${statuses(claim.run_id).join(",")}`);
      assert.ok(!statuses(claim.run_id).includes("failed"));
    });
  }

  it("quiescent: the milestone checkpoint publishes", async () => {
    const { gitlab } = fakeGitlab();
    const iid = 1867;
    const q = phasedQuiescer("quiescent", 0);
    const pub = spyPublish(q.inBlockedPhase);
    const seen: MilestoneSnapshot[] = [];
    await runnerWith(milestoneFactory(iid, pub, seen), gitlab, undefined, undefined, { ...RUNNER_OPTS, quiesceRun: q.quiesceRun }).execute(gitlabClaim(iid));
    assert.equal(seen.length, 1);
    assert.ok(seen[0]!.published >= 1, "the milestone published");
    assert.notEqual(seen[0]!.tracking, null, "the milestone fetched back");
  });
});

// ─── recovery-exhausted capture ────────────────────────────────────────────────────────────

function recoveryFactory(): ExecutorFactory {
  return (runId) => ({
    homeDir: path.join(homeDir, runId),
    executor: {
      run: async (ctx: RunContext): Promise<ExecutorResult> => {
        commitWork(ctx.worktreePath);
        throw new TransientRecoveryError();
      },
    },
  });
}

/** Record whether the capture's fetch-back ran before the clone was proven quiescent. */
function spyFetchBack(inBlockedPhase: () => boolean): { blockedCalls: () => number } {
  let early = 0;
  const orig = git.fetchAgentBranch.bind(git);
  git.fetchAgentBranch = (async (...a: Parameters<typeof git.fetchAgentBranch>) => {
    if (inBlockedPhase()) early += 1;
    return orig(...a);
  }) as typeof git.fetchAgentBranch;
  return { blockedCalls: () => early };
}

describe("issue #1783 M2: the recovery-exhausted capture is gated (the capture-failure branch)", () => {
  for (const state of NOT_QUIESCENT) {
    it(`${state}: nothing captured or published while blocked; retained and retried, then parked once quiescent`, async () => {
      const { gitlab } = fakeGitlab();
      const iid = 1870 + (state === "survivors" ? 0 : 1);
      const q = phasedQuiescer(state, 3);
      const pub = spyPublish(q.inBlockedPhase);
      const fetch = spyFetchBack(q.inBlockedPhase);
      const claim = gitlabClaim(iid);
      await runnerWith(recoveryFactory(), gitlab, undefined, undefined, { ...RUNNER_OPTS, quiesceRun: q.quiesceRun }).execute(claim);
      assert.equal(fetch.blockedCalls(), 0, "no fetch-back while the clone was not provably quiescent");
      assert.equal(pub.blockedCalls(), 0, "no publish while the clone was not provably quiescent");
      assert.ok(
        statusTexts(claim.run_id).some((t) => t.startsWith("Recovery checkpoint could not be verified")),
        "the existing capture-failure notice (retain the work, retry capture)",
      );
      assert.ok(q.calls.length > 3, "the capture was retried past the blocked proofs");
      assert.ok(statuses(claim.run_id).includes("recovery_wait"), "parked once the clone was proven quiescent");
      assert.ok(!statuses(claim.run_id).includes("failed"));
    });
  }

  it("quiescent: the first capture verifies and publishes, then parks", async () => {
    const { gitlab } = fakeGitlab();
    const q = phasedQuiescer("quiescent", 0);
    const pub = spyPublish(q.inBlockedPhase);
    const claim = gitlabClaim(1872);
    await runnerWith(recoveryFactory(), gitlab, undefined, undefined, { ...RUNNER_OPTS, quiesceRun: q.quiesceRun }).execute(claim);
    assert.ok(statuses(claim.run_id).includes("recovery_wait"));
    assert.ok(!statusTexts(claim.run_id).some((t) => t.startsWith("Recovery checkpoint could not be verified")));
    assert.ok(pub.calls() >= 1);
  });
});

// ─── credential-switch capture ─────────────────────────────────────────────────────────────

function switchFactory(): ExecutorFactory {
  return (runId) => ({
    homeDir: path.join(homeDir, runId),
    executor: {
      run: async (ctx: RunContext): Promise<ExecutorResult> => {
        commitWork(ctx.worktreePath);
        throw new CredentialSwitchSignal();
      },
    },
  });
}

describe("issue #1783 M2: the credential-switch capture is gated (the capture-failure branch)", () => {
  for (const state of NOT_QUIESCENT) {
    it(`${state}: nothing captured or published; gives up (credential_switch_failed), never released, custody kept`, async () => {
      const { gitlab } = fakeGitlab();
      const iid = 1875 + (state === "survivors" ? 0 : 1);
      const q = phasedQuiescer(state, Infinity);
      const pub = spyPublish(q.inBlockedPhase);
      const fetch = spyFetchBack(q.inBlockedPhase);
      const claim = gitlabClaim(iid);
      api.overrideStateStatus(claim.run_id, "queued");
      await runnerWith(switchFactory(), gitlab, undefined, undefined, { ...RUNNER_OPTS, quiesceRun: q.quiesceRun }).execute(claim);
      const st = statuses(claim.run_id);
      assert.ok(st.includes("credential_switch_failed"), `the capture-failure give-up: ${st.join(",")}`);
      assert.ok(!st.includes("credential_switch"), "never released without a verified capture");
      assert.ok(!st.some((s) => s === "failed" || s === "completed"), "no terminal report");
      assert.equal(fetch.blockedCalls(), 0, "no fetch-back");
      assert.equal(pub.calls(), 0, "no publish");
      assert.equal(trackingSha(iid), null, "the tracking ref is untouched");
      assert.equal(fs.existsSync(path.join(worktreeDirFor(iid), "WORK.txt")), true, "the clone is kept");
      assert.ok(q.calls.filter((c) => c.mode === "own").length >= 2, "every capture attempt ran the proof");
    });
  }

  it("quiescent: the capture verifies and the switch is released", async () => {
    const { gitlab } = fakeGitlab();
    const q = phasedQuiescer("quiescent", 0);
    spyPublish(q.inBlockedPhase);
    const claim = gitlabClaim(1877);
    api.overrideStateStatus(claim.run_id, "queued");
    await runnerWith(switchFactory(), gitlab, undefined, undefined, { ...RUNNER_OPTS, quiesceRun: q.quiesceRun }).execute(claim);
    const st = statuses(claim.run_id);
    assert.ok(st.includes("credential_switch"), `released: ${st.join(",")}`);
    assert.ok(!st.includes("credential_switch_failed"));
  });
});
