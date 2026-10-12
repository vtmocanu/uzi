import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { execFileSync } from "node:child_process";
import type { Readable } from "node:stream";
import type { Executor, ExecutorResult, RunContext } from "../src/executor.js";
import { WorkerClient } from "../src/client.js";
import { GitCache } from "../src/git.js";
import { RunRunner, type ExecutorFactory } from "../src/runner.js";
import { ActiveRunRegistry } from "../src/active-run-registry.js";
import { TerminalRejectionCoordinator } from "../src/terminal-rejections.js";
import { FakeApi } from "./fake-api.js";
import { makeFixture } from "./fixture-repo.js";
import { fakeGitlab } from "./runner-harness.js";
import { defaultGitleaksShim } from "./gitleaks-shim.js";
import { makeClaim, nullLogger, testGitCacheOptions } from "./helpers.js";
import { mkSweepOutbox, pollUntil, startSweepWorker } from "./worker-sweep-rig.js";
import { dependencyWaitAnswer, dependencyWaitPlan, dependencyWaitMilestones, dependencyWaitProgress } from "./dependency-wait-fixture.js";

/**
 * One real executor run, capped by the caller's test timeout and the runner's
 * one-second question deadline. ACKs alone select the wall path; no wall is seeded.
 */
export async function exerciseExpiredWallRunner(
  boundary: "pause" | "fallback",
  build: (ctx: RunContext, home: string) => Executor,
): Promise<void> {
  const token = "fixture-worker-token";
  const api = new FakeApi(token);
  const fx = makeFixture();
  const home = fs.mkdtempSync(path.join(os.tmpdir(), "m1-expired-wall-"));
  try {
    const client = new WorkerClient(await api.listen(), token, "test", nullLogger(), { sleep: async () => {} });
    const git = new GitCache(fx.dataDir, nullLogger(), undefined,
      testGitCacheOptions({ gitleaksBin: defaultGitleaksShim() }));
    const claim = makeClaim({
      issue_iid: 2657, claim_generation: 1, kind: "issue", park_run_supported: true,
      auto_approve: false, plan_approved: true, plan_source: "seeded", plan_md: dependencyWaitPlan,
      milestones: dependencyWaitMilestones,
      secrets: { forge_pat: "fixture-forge-pat-000000", anthropic_oauth_token: "dummy-oauth-do-not-scan" },
      config: { max_iterations: 2, completion_contract_version: 1 },
      repo: { id: "r1", url: "https://gitlab.example.test/org/repo", clone_url: fx.originPath },
    });
    api.parkRunGeneration = 1;
    api.parkRunOwnership = { status: "running", claim_generation: 1, hold_reason: null, claim_released: false };
    api.workerPauseRefused = true;
    if (boundary === "pause") api.workerPauseReason = "wall_expired";
    else {
      api.guardedQuestionStatus = "running";
      api.guardedQuestionReason = "wall_expired";
    }
    let guardedAttempts = 0;
    let acceptedQuestions = 0;
    const report = client.reportState.bind(client);
    client.reportState = async (...args) => {
      if (args[1].status === "awaiting_input" && args[1].require_running) guardedAttempts++;
      const ack = await report(...args);
      if (args[1].status === "awaiting_input" && ack.applied && ack.status === "awaiting_input")
        acceptedQuestions++;
      return ack;
    };
    client.publishCheckpoint = async (_runId, _tip, pack) => {
      for await (const _chunk of pack) { /* Drain the real checkpoint stream. */ }
      return { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/agent/issue-2657" } };
    };
    let result: ExecutorResult | undefined;
    let wallCalls = 0;
    const factory: ExecutorFactory = runId => ({
      homeDir: path.join(home, runId),
      executor: { run: async ctx => {
        fs.mkdirSync(path.join(home, runId), { recursive: true });
        const park = ctx.parkForWall!;
        ctx.parkForWall = async (...args) => { wallCalls++; return park(...args); };
        result = await build(ctx, path.join(home, runId)).run(ctx);
        return result;
      } },
    });
    const { gitlab, calls: forgeWrites } = fakeGitlab();
    const runner = new RunRunner(client, git, factory, nullLogger(), 20, undefined,
      { pollMs: 5, questionTimeoutMs: 1000, checkpointIntervalMs: 0, gitlab });
    await runner.execute(claim);
    assert.ok(result?.walled, "real executor returns a walled result");
    assert.equal(wallCalls, 1, "executor invokes the real runner wall park");
    assert.equal(api.wallParkRequests.length, 1);
    assert.equal(api.workerPauseRequests.length, 1);
    assert.equal(guardedAttempts, boundary === "fallback" ? 1 : 0);
    assert.equal(acceptedQuestions, 0, "refused attempts are not accepted awaiting_input transitions");
    assert.equal(api.messages(claim.run_id).some(message => message.kind === "question"), false);
    assert.equal(api.states.some(state => ["failed", "completed"].includes(state.body.status)), false);
    assert.equal(forgeWrites.length, 0);
  } finally {
    await api.close();
    fx.cleanup();
    fs.rmSync(home, { recursive: true, force: true });
  }
}

const gitEnv = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };
const ident = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];
const work = "independent M1 implementation\n";

export function commitDependencySlice(tree: string): void {
  fs.writeFileSync(path.join(tree, "M1.txt"), work);
  execFileSync("git", ["-C", tree, "add", "M1.txt"], { env: gitEnv });
  execFileSync("git", ["-C", tree, ...ident, "commit", "-m", "implement M1"], { env: gitEnv });
}

function gitRead(tree: string, ...args: string[]): string {
  return execFileSync("git", ["-C", tree, ...args], { env: gitEnv }).toString().trim();
}

/**
 * Both real executors run through the same physical runner/worker seams as owner pauses.
 * Two executions and two ten-second observation deadlines bound this fixture; failures
 * abort and await the worker before cleanup, so no sibling fixture is left running.
 */
export async function exerciseDependencyWaitRunner(
  build: (ctx: RunContext, home: string) => Executor,
  ownerMode?: "now" | "after_milestone",
) {
  const token = "fixture-worker-token";
  const api = new FakeApi(token);
  const fx = makeFixture();
  const home = fs.mkdtempSync(path.join(os.tmpdir(), "m4-wait-home-"));
  const sweep = await mkSweepOutbox();
  let worker: ReturnType<typeof startSweepWorker> | undefined;
  try {
    const client = new WorkerClient(await api.listen(), token, "test", nullLogger(), { sleep: async () => {} });
    const git = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({ gitleaksBin: defaultGitleaksShim() }));
    const active = new ActiveRunRegistry();
    const { gitlab, calls: forgeWrites } = fakeGitlab();
    const claim = makeClaim({
      issue_iid: 2657, claim_generation: 1, kind: "issue", park_run_supported: true,
      plan_approved: true, plan_source: "seeded", plan_md: dependencyWaitPlan,
      milestones: dependencyWaitMilestones,
      secrets: { forge_pat: "fixture-forge-pat-000000", anthropic_oauth_token: "dummy-oauth-do-not-scan" },
      config: { max_iterations: 8, completion_contract_version: 1 },
      repo: { id: "r1", url: "https://gitlab.example.test/org/repo", clone_url: fx.originPath },
      ...(ownerMode ? { pause_pending: true, pause_mode: ownerMode } : {}),
    });
    api.parkRunGeneration = 1;
    api.parkRunOwnership = { status: "running", claim_generation: 1, hold_reason: null, claim_released: false };
    let questions = 0;
    api.onState(claim.run_id, state => {
      if (state.status === "awaiting_input") {
        questions++;
        api.setInputs(claim.run_id, [{ id: questions, kind: "answer",
          body: JSON.stringify({ question_id: state.open_question_id, answers: [dependencyWaitAnswer] }) }]);
      }
    });
    const published: string[] = [];
    const checkpointRef = "refs/uzi-checkpoints/agent/issue-2657";
    client.publishCheckpoint = async (_runId, tip, pack: Readable) => {
      const chunks: Buffer[] = [];
      for await (const chunk of pack) chunks.push(Buffer.from(chunk));
      // Simulate forge receipt: materialize the pack and update its checkpoint ref before ACK.
      execFileSync("git", ["-C", fx.originPath, "index-pack", "--stdin"], { env: gitEnv, input: Buffer.concat(chunks) });
      execFileSync("git", ["-C", fx.originPath, "update-ref", checkpointRef, tip], { env: gitEnv, stdio: "pipe" });
      assert.equal(gitRead(fx.originPath, "show", `${checkpointRef}:M1.txt`), work.trim());
      assert.equal(api.states.filter(s => s.runId === claim.run_id && s.body.status === "paused").length, published.length);
      published.push(tip);
      return { ok: true, body: { published: true, ref: checkpointRef } };
    };
    let executions = 0;
    let tree = "";
    let resumed = false;
    const factory: ExecutorFactory = runId => ({
      homeDir: path.join(home, runId),
      executor: { run: async ctx => {
        executions++;
        tree = ctx.worktreePath;
        fs.mkdirSync(path.join(home, runId), { recursive: true });
        const sessionMarker = path.join(home, runId, "session.marker");
        if (executions === 1) fs.writeFileSync(sessionMarker, "resumable M1 session\n");
        else assert.equal(fs.readFileSync(sessionMarker, "utf8"), "resumable M1 session\n");
        assert.equal(active.has(runId), true);
        assert.equal(protectedExecutions, 1, "Worker protects dispatch before executor entry");
        if (executions === 1 && !ownerMode) return build(ctx, path.join(home, runId)).run(ctx);
        if (executions === 1) {
          commitDependencySlice(tree);
          await ctx.reportIteration?.(1, dependencyWaitProgress);
          await ctx.checkpoint?.({ reap: true, progress: dependencyWaitProgress });
        } else {
          assert.equal(gitRead(tree, "rev-parse", "HEAD"), published[0], "owner resume adopts published checkpoint");
          assert.equal(fs.readFileSync(path.join(tree, "M1.txt"), "utf8"), work);
          assert.equal(ctx.planApproved, true);
          assert.equal(ctx.approvedPlan, dependencyWaitPlan);
          assert.deepEqual(ctx.frozenMilestones, dependencyWaitMilestones);
          resumed = true;
        }
        assert.equal(await ctx.parkForPause!({ completedCount: 1, total: 2 }), true);
        return { branch: ctx.branch, pausedAt: { completedCount: 1, total: 2 } };
      } },
    });
    const options = { pollMs: 5, questionTimeoutMs: 1000, checkpointIntervalMs: 0, gitlab, activeRuns: active };
    let runner = new RunRunner(client, git, factory, nullLogger(), 20, undefined, options);
    const coordinator = new TerminalRejectionCoordinator(sweep.outbox, client, nullLogger());
    let protectedExecutions = 0;
    let protections = 0;
    let releases = 0;
    const protect = coordinator.protectExecution.bind(coordinator);
    coordinator.protectExecution = runId => {
      protections++;
      protectedExecutions++;
      const release = protect(runId);
      return () => { release(); releases++; protectedExecutions--; };
    };
    let queued: typeof claim | undefined = claim;
    let idleClaims = 0;
    client.claimRun = async () => {
      if (!queued) { idleClaims++; return null; }
      const next = queued;
      queued = undefined;
      return next;
    };
    // RunRunner + real HTTP remain intact; only the worker's claim source is scripted.
    worker = startSweepWorker({ outbox: sweep.outbox, client, runner: { execute: claim => runner.execute(claim), isExecuting: runId => runner.isExecuting(runId), resumePendingRecoveries: () => runner.resumePendingRecoveries() }, activeRuns: active, terminalRejections: coordinator });
    await pollUntil(() => executions === 1 && releases === 1 && idleClaims > 0, 10_000, "first park and worker slot release");
    const firstStates = api.states.filter(s => s.runId === claim.run_id).map(s => s.body);
    const paused = firstStates.find(s => s.status === "paused");
    assert.ok(paused, JSON.stringify(firstStates));
    assert.equal(firstStates.some(s => ["failed", "completed", "pause_failed"].includes(s.status)), false);
    assert.equal(forgeWrites.length, 0);
    assert.equal(published.length, 1);
    assert.equal(active.has(claim.run_id), false);
    assert.equal(runner.isExecuting(claim.run_id), false);
    assert.equal(protectedExecutions, 0);
    const bare = git.barePathFor(fx.originPath);
    assert.equal(await git.trackingTip(bare, "agent/issue-2657"), published[0]);
    assert.equal(gitRead(bare, "show", `${published[0]}:M1.txt`), work.trim());
    assert.equal(fs.existsSync(tree), false, "verified park retires the local runner clone");
    const owed = await git.enumerateOwedCandidates(bare, claim.run_id);
    assert.deepEqual(owed, [], "confirmed publication clears producing owed pins");
    const inventory = await git.readInventoryCloneHeads(bare, claim.run_id);
    assert.equal(inventory.kind, "verified");
    assert.ok(inventory.kind === "verified");
    assert.deepEqual(inventory.heads, []);
    assert.deepEqual(inventory.foreignOwners, []);
    assert.deepEqual(inventory.clones, []);
    const retained = await git.discoverRetainedRecovery(fx.originPath, "agent/issue-2657",
      "issue-2657", claim.run_id);
    assert.equal(retained, undefined, "ordinary parked clone has no retained recovery episode");
    const localPins = gitRead(bare, "for-each-ref", "--format=%(refname)",
      "refs/uzi-owed/", "refs/uzi-recovery-pin/", "refs/uzi-recovery-episode/",
      "refs/uzi-settle/", "refs/uzi-coverage/").split("\n").filter(Boolean).map(ref => ({
        ref: ref.replace(claim.run_id, "<run>"), contents: gitRead(bare, "show", `${ref}:M1.txt`),
      }));
    const custody = {
      localPins, owed, retained,
      inventory: inventory.clones,
      sessionContents: fs.readFileSync(path.join(home, claim.run_id, "session.marker"), "utf8"),
      status: paused.status, holdReason: (await client.getRunOwnership(claim.run_id)).hold_reason ?? null,
      checkpointContainsLatest: paused.checkpoint_contains_latest,
      cloneRetained: fs.existsSync(tree), active: active.has(claim.run_id),
      executing: runner.isExecuting(claim.run_id), protectedExecutions,
      terminalPending: sweep.outbox.listPendingTerminals().length,
    };
    assert.equal(custody.holdReason, null);
    assert.equal(custody.terminalPending, 0);
    if (!ownerMode) {
      assert.equal(questions, 1);
      assert.ok(firstStates.some(s => JSON.stringify(s.milestones_completed) === '["m1"]'));
    }
    api.setInputs(claim.run_id, []);
    api.parkRunGeneration = 2;
    api.parkRunOwnership = { status: "running", claim_generation: 2, hold_reason: null, claim_released: false };
    // A fresh cache has no local tracking ref: resume must recover the pushed checkpoint.
    const resumeGit = new GitCache(path.join(fx.dataDir, "owner-resume"), nullLogger(), undefined,
      testGitCacheOptions({ gitleaksBin: defaultGitleaksShim() }));
    runner = new RunRunner(client, resumeGit, factory, nullLogger(), 20, undefined, options);
    queued = { ...claim, claim_generation: 2, checkpoint_tip: published[0], branch: "agent/issue-2657", pause_pending: true, pause_mode: "now" };
    await pollUntil(() => resumed && releases === 2, 10_000, "owner resume and second park");
    assert.equal(executions, 2, "released admission allows the same run to be claimed again");
    assert.equal(protections, 2);
    assert.equal(protectedExecutions, 0);
    assert.equal(active.has(claim.run_id), false);
    assert.equal(runner.isExecuting(claim.run_id), false);
    assert.equal(api.states.some(s => ["failed", "completed"].includes(s.body.status)), false);
    assert.equal(forgeWrites.length, 0);
    return custody;
  } finally {
    try { await worker?.stop(); } finally {
      try { await api.close(); } finally {
        fx.cleanup();
        fs.rmSync(home, { recursive: true, force: true });
        fs.rmSync(sweep.root, { recursive: true, force: true });
      }
    }
  }
}
