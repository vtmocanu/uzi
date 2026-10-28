import assert from "node:assert/strict";
import { it, type TestContext } from "node:test";
import { once } from "node:events";
import { PassThrough } from "node:stream";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { createHash } from "node:crypto";
import { SinkGate } from "../src/sink-gate.js";
import type { CheckpointStop } from "../src/checkpoint-stop.js";
import type { Executor } from "../src/executor.js";
import type { SpawnedBoundaryProcess } from "../src/codex/safety.js";
import type { BoundaryProcessRequest } from "../src/harness.js";
import { CheckpointSoftDeadlineError, GitBoundaryAbortError, CheckpointUnsafeCleanupError } from "../src/checkpoint-stop.js";
import { checkCode } from "../src/code-cross-check-gate.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, installHarness, runnerWith } from "./runner-harness.js";
import { codexRig, commitInTree, installRecoveryRootCleanup, virtualBoundaryClock } from "./runner-codex-sinks-fixture.js";
import { recordingLogger } from "./helpers.js";
import { PauseNowSignal, type SteeringChannel } from "../src/steering.js";
import { spyPublishLands } from "./runner-codex-sinks-fixture.js";

installHarness();
installRecoveryRootCleanup();

it("m2 done shares 60/90 seconds across preliminary reads and import children", async t => {
  const clock = virtualBoundaryClock();
  const softBudgets: number[] = [], childBudgets: number[] = [];
  let inDone = false, moved = false, observed = false;
  const branchTip = git.branchTip.bind(git);
  t.mock.method(git, "branchTip", async (...args: Parameters<typeof git.branchTip>) => {
    const head = await branchTip(...args);
    if (inDone && !moved) { moved = true; clock.advance(40_000); }
    return head;
  });
  let rig: ReturnType<typeof codexRig> | undefined;
  const executor: Executor = {
    get safety() { return rig?.safety; },
    run: async ctx => {
      rig = codexRig({ lifecycleSignal: AbortSignal.any([ctx.signal!, ctx.terminalLifecycleSignal!]),
        armDeadline: clock.armDeadline, now: clock.now,
        boundaryProcess: (_request, startupMs) => { if (inDone) childBudgets.push(startupMs); return undefined; } });
      commitInTree(ctx.worktreePath, "done.txt", "done allowance\n");
      const head = await branchTip(ctx.worktreePath, ctx.branch);
      inDone = true;
      try { await ctx.checkpoint!({ reap: true, sink: "done_checkpoint" }); } finally { inDone = false; }
      assert.equal(await git.trackingTip(git.barePathFor(fx.originPath), ctx.branch), head);
      observed = true;
      return { branch: ctx.branch };
    },
  };
  const forge = fakeGitlab();
  await runnerWith(() => ({ executor }), forge.gitlab, undefined, undefined, {
    checkpointIntervalMs: 0,
    checkpointTestHooks: { absoluteNow: clock.now, armSoftDeadline: (fire, ms) => { softBudgets.push(ms); return clock.arm(fire, ms); } },
  }).execute(gitlabClaim(312301));
  assert.equal(observed, true);
  assert.deepEqual(softBudgets, [60_000]);
  assert.ok(clock.budgets.some(b => b.sink === "done_checkpoint"), "the done hard deadline was armed");
  assert.ok(clock.budgets.filter(b => b.sink === "done_checkpoint").every(b => b.ms === 90_000));
  assert.ok(childBudgets.some(ms => ms === 20_000), "preliminary read consumed the same soft allowance");
  assert.ok(childBudgets.every(ms => ms <= 60_000));
});

it("m2 started milestone import settles ownership but defers ancestry bridge after its soft stop", async () => {
  const publishedBranch = "feature/m2-milestone";
  execFileSync("git", ["-C", fx.originPath, "checkout", "-b", publishedBranch], { stdio: "pipe" });
  commitInTree(fx.originPath, "floor.txt", "published floor\n");
  execFileSync("git", ["-C", fx.originPath, "checkout", "main"], { stdio: "pipe" });
  const clock = virtualBoundaryClock();
  const { logger, lines } = recordingLogger();
  const startedAt = clock.now();
  let inCheckpoint = false, advanced = false, observed = false;
  let rig: ReturnType<typeof codexRig> | undefined;
  const executor: Executor = {
    get safety() { return rig?.safety; },
    // Issue #3217: real time passes while the plan summary settles, as on a loaded runner. The
    // milestone budget must still come from the virtual clock, so it stays exactly 30 seconds.
    closeAndSettlePlanSummary: async () => {
      const until = Date.now() + 2;
      while (Date.now() < until) { /* spend real time */ }
    },
    run: async ctx => {
      rig = codexRig({ lifecycleSignal: AbortSignal.any([ctx.signal!, ctx.terminalLifecycleSignal!]),
        armDeadline: clock.armDeadline, now: clock.now,
        boundaryProcess: request => {
          if (inCheckpoint && !advanced && request.argv.some(arg => arg.startsWith("file://"))) {
            advanced = true; clock.advance(20_000);
          }
          return undefined;
        } });
      const base = await git.branchTip(fx.originPath, "main");
      assert.ok(base);
      execFileSync("git", ["-C", ctx.worktreePath, "reset", "--hard", base], { stdio: "pipe" });
      commitInTree(ctx.worktreePath, "milestone.txt", "started import\n");
      const head = await git.branchTip(ctx.worktreePath, ctx.branch);
      assert.ok(head);
      inCheckpoint = true;
      try { await ctx.checkpoint!({ reap: true, sink: "milestone_checkpoint", progress: { completed: ["m2"], in_progress: [] } }); }
      finally { inCheckpoint = false; }
      const bare = git.barePathFor(fx.originPath);
      const bridged = await git.trackingTip(bare, ctx.branch);
      assert.ok(bridged);
      assert.equal(bridged, head, "the started import settles but later bridge is deferred");
      assert.equal(await git.ancestry(bare, head, bridged), "ancestor");
      assert.equal(await git.revParse(bare, `${bridged}^{tree}`), await git.revParse(bare, `${head}^{tree}`));
      const owned = await git.committedTrackingOwnership(bare, ctx.branch, ctx.runId, bridged);
      assert.equal(owned.kind, "owned");
      assert.ok((await git.enumerateOwedCandidates(bare, ctx.runId)).some(c => c.sha === head), "original imported head remains owed");
      await ctx.checkpoint!({ reap: false });
      const retried = await git.trackingTip(bare, ctx.branch);
      assert.ok(retried);
      assert.notEqual(retried, head, "retry completes the deferred bridge");
      assert.equal(await git.ancestry(bare, head, retried), "ancestor");
      observed = true;
      return { branch: ctx.branch };
    },
  };
  await runnerWith(() => ({ executor }), fakeGitlab().gitlab, undefined, logger, {
    checkpointTestHooks: { absoluteNow: clock.now, armSoftDeadline: clock.arm },
  }).execute(gitlabClaim(312302, { kind: "task", branch: publishedBranch, open_mr: false }));
  assert.equal(advanced, true); assert.equal(observed, true);
  assert.equal(clock.now() - startedAt, 20_000, "started milestone settles between its 10s soft and 30s hard deadlines");
  assert.ok(clock.budgets.some(b => b.sink === "milestone_checkpoint"), "the milestone hard deadline was armed");
  assert.ok(clock.budgets.filter(b => b.sink === "milestone_checkpoint").every(b => b.ms === 30_000));
  assert.ok(!lines.some(line => (line as { msg?: string }).msg === "fetch-back on interruption failed; work may not be recoverable"));
  assert.ok(api.states.some(state => state.body.milestones_completed?.includes("m2")));
});

for (const error of [new GitBoundaryAbortError("hard"), new CheckpointUnsafeCleanupError("unsafe"), Object.assign(new Error("codex"), { name: "CodexBoundaryError" })]) {
  it(`m2 checkCode preserves ${"checkpointBoundary" in error ? error.checkpointBoundary : error.name} through its outer catch`, async t => {
    t.mock.method(client, "codeCrossCheckStatus", async () => ({ stage: "code", result: "no_row", repo_instructions_enabled: false }));
    if (error instanceof GitBoundaryAbortError) assert.equal(error.name, "AbortError", "existing name classifiers remain compatible");
    await assert.rejects(checkCode({ client, runId: "m2", generation: 1, signal: new AbortController().signal,
      snapshot: async () => { throw error; } }), actual => actual === error);
  });
}

for (const failure of ["soft import", "ordinary import", "capture", "unread head", "owed context"] as const) {
  it(`m2 fresh interlocked snapshot recovers ${failure} once with the retained candidate`, async t => {
    const clock = virtualBoundaryClock();
    const softBudgets: number[] = [];
    let active = false, imports = 0, original = "", verified = false, unread = false;
    const fetch = git.fetchAgentBranch.bind(git), pin = git.pinCodeSnapshot.bind(git), tip = git.branchTip.bind(git);
    const submitted: unknown[] = [];
    const candidates: Array<string | undefined> = [];
    let failCapture = failure === "capture";
    let contextStopped = false;
    const originTip = git.originBranchTip.bind(git);
    t.mock.method(git, "originBranchTip", async (...args: Parameters<typeof git.originBranchTip>) => {
      if (active && failure === "owed context" && !contextStopped) {
        assert.equal(imports, 0, "the context initializer stops before import");
        contextStopped = true; throw new CheckpointSoftDeadlineError();
      }
      return originTip(...args);
    });
    t.mock.method(git, "fetchAgentBranch", async (...args: Parameters<typeof git.fetchAgentBranch>) => {
      if (active) {
        imports++;
        assert.equal(args[4].checkpointImportMode, "done");
        if (imports === 1 && failure === "soft import") throw new CheckpointSoftDeadlineError();
        if (imports === 1 && failure === "ordinary import") throw new Error("local import unavailable");
        candidates.push(args[4].expectedCandidateSha);
      }
      return fetch(...args);
    });
    t.mock.method(git, "branchTip", async (...args: Parameters<typeof git.branchTip>) => {
      if (active && failure === "unread head" && !unread) { unread = true; return null; }
      return tip(...args);
    });
    t.mock.method(git, "pinCodeSnapshot", async (...args: Parameters<typeof git.pinCodeSnapshot>) => {
      if (active && failCapture) { failCapture = false; throw new Error("capture unavailable"); }
      return pin(...args);
    });
    t.mock.method(client, "recordCompletionAttempt", async () => ({ unmet: [], attemptCount: 1 }));
    t.mock.method(client, "codeCrossCheckStatus", async () => ({ stage: "code", result: "no_row", repo_instructions_enabled: false }));
    const submit = t.mock.method(client, "submitCodeCrossCheck", async (_id: string, _generation: number, snapshot: Parameters<typeof client.submitCodeCrossCheck>[2]) => {
      submitted.push(snapshot);
      assert.ok("head_commit" in snapshot);
      assert.equal(snapshot.head_commit, original);
      return { stage: "code" as const, round: 1 as const, candidate_generation: 1,
        head_commit: snapshot.head_commit, base_commit: snapshot.base_commit, candidate_digest: "c".repeat(64),
        checker_run_id: null, checker_harness: "codex", checker_model: "fixture", checker_effort: null,
        outcome: "completed" as const, reason_class: null, findings: [], interrupted_at: null,
        finalized_at: new Date().toISOString(), deadline_at: new Date().toISOString() };
    });
    let rig: ReturnType<typeof codexRig> | undefined;
    const executor: Executor = {
      get safety() { return rig?.safety; },
      run: async ctx => {
        rig = codexRig({ lifecycleSignal: AbortSignal.any([ctx.signal!, ctx.terminalLifecycleSignal!]),
          armDeadline: clock.armDeadline, now: clock.now });
        commitInTree(ctx.worktreePath, "fresh.txt", "fresh candidate\n");
        original = (await tip(ctx.worktreePath, ctx.branch))!;
        active = true;
        await ctx.checkpoint!({ reap: true, sink: "done_checkpoint" });
        await ctx.recordCompletionAttempt!({ declared: [], head: original, worktreeFingerprint: null });
        await ctx.codeCrossCheckGate!({ interlocked: true });
        const expectedImports = failure === "owed context" ? 1 : 2;
        assert.equal(imports, expectedImports, "one fresh import after the original attempt");
        if (failure === "owed context") assert.equal(contextStopped, true, "matching stopped initializer is retried");
        await ctx.codeCrossCheckGate!({ interlocked: true });
        assert.equal(imports, expectedImports, "later done reads never acquire another snapshot");
        verified = true; active = false;
        return { branch: ctx.branch };
      },
    };
    await runnerWith(() => ({ executor }), fakeGitlab().gitlab, undefined, undefined, {
      checkpointIntervalMs: 0,
      checkpointTestHooks: { absoluteNow: clock.now, armSoftDeadline: (fire, ms) => { softBudgets.push(ms); return clock.arm(fire, ms); } },
    }).execute(gitlabClaim(312303, { code_cross_check_required: true, claim_generation: 1 }));
    assert.equal(verified, true); assert.equal(submit.mock.callCount(), 1);
    assert.ok(candidates.length > 0);
    assert.equal(candidates.at(-1), original, "fresh import must retain original candidate");
    assert.deepEqual((submitted[0] as { head_commit?: string }).head_commit, original, "submitted snapshot must carry original head");
    assert.deepEqual(softBudgets, [60_000, 60_000]);
    assert.ok(clock.budgets.some(b => b.sink === "done_checkpoint"), "the fresh hard deadline was armed");
    assert.ok(clock.budgets.filter(b => b.sink === "done_checkpoint").every(b => b.ms === 90_000));
  });
}

it("m2 synchronous shutdown during genuine installer join is clean and grants no ordinary credential authority", async () => {
  let rig: ReturnType<typeof codexRig> | undefined;
  let checked = false;
  const executor: Executor = {
    get safety() { return rig?.safety; },
    run: async ctx => {
      rig = codexRig({ lifecycleSignal: AbortSignal.any([ctx.signal!, ctx.terminalLifecycleSignal!]) });
      commitInTree(ctx.worktreePath, "shutdown.txt", "shutdown candidate\n");
      let finish!: () => void;
      rig.installer.promise = new Promise(resolve => { finish = () => resolve({ results: [], truncated: false }); });
      rig.installer.abort.signal.addEventListener("abort", () => queueMicrotask(() => {
        worker.shutdown(); finish();
      }), { once: true });
      await ctx.checkpoint!({ reap: true, sink: "done_checkpoint" });
      assert.equal(rig.registry.isPoisoned(), false);
      assert.equal(rig.refreshCalls(), 0, "ordinary done cannot reconcile credentials with shutdown authority");
      assert.equal(rig.releaseCalls(), 0);
      checked = true;
      throw new Error("shutdown interrupted provider");
    },
  };
  const worker = runnerWith(() => ({ executor }), fakeGitlab().gitlab);
  await worker.execute(gitlabClaim(312304));
  assert.equal(checked, true);
});

it("m2 fresh snapshot clock includes production sink-gate wait and never calls an expired action", async t => {
  const clock = virtualBoundaryClock();
  const { logger, lines } = recordingLogger();
  let active = false, fresh = false, imports = 0, verified = false, preempted = false;
  const fetch = git.fetchAgentBranch.bind(git);
  t.mock.method(git, "fetchAgentBranch", async (...args: Parameters<typeof git.fetchAgentBranch>) => {
    if (active) { imports++; throw new Error("original import unavailable"); }
    return fetch(...args);
  });
  const run = SinkGate.prototype.run;
  t.mock.method(SinkGate.prototype, "run", async function<T>(this: SinkGate, fn: () => Promise<T>, stop?: CheckpointStop): Promise<T> {
    if (!fresh || !stop) return run.call(this, fn, stop) as Promise<T>;
    const release = this.tryAcquire(() => { preempted = true; clock.advance(60_000); });
    assert.ok(release, "hold the actual gate before fresh snapshot admission");
    try { return await run.call(this, fn, stop) as T; }
    finally { release(); }
  });
  t.mock.method(client, "codeCrossCheckStatus", async () => ({ stage: "code", result: "no_row", repo_instructions_enabled: false }));
  const submit = t.mock.method(client, "submitCodeCrossCheck", async (_id: string, _generation: number, snapshot: Parameters<typeof client.submitCodeCrossCheck>[2]) => {
    assert.deepEqual(snapshot, { reason_class: "snapshot_failed" });
    throw new Error("fixture response unavailable");
  });
  let rig: ReturnType<typeof codexRig> | undefined;
  const executor: Executor = {
    get safety() { return rig?.safety; },
    run: async ctx => {
      rig = codexRig({ lifecycleSignal: AbortSignal.any([ctx.signal!, ctx.terminalLifecycleSignal!]),
        armDeadline: clock.armDeadline, now: clock.now });
      commitInTree(ctx.worktreePath, "queue.txt", "fresh queue\n");
      active = true;
      await ctx.checkpoint!({ reap: true, sink: "done_checkpoint" });
      fresh = true;
      await ctx.codeCrossCheckGate!({ interlocked: true });
      fresh = false;
      assert.equal(imports, 1, "expired fresh action never imports");
      assert.equal(rig.registry.isPoisoned(), false, "real source reconcile certifies clean stopped admission");
      assert.ok(clock.budgets.some(b => b.sink === "done_checkpoint" && b.ms === 30_000), "queue time consumes the original hard allowance");
      verified = true; active = false;
      return { branch: ctx.branch };
    },
  };
  await runnerWith(() => ({ executor }), fakeGitlab().gitlab, undefined, logger, {
    checkpointIntervalMs: 0, checkpointTestHooks: { absoluteNow: clock.now, armSoftDeadline: clock.arm },
  }).execute(gitlabClaim(312305, { code_cross_check_required: true, claim_generation: 1 }));
  assert.equal(verified, true); assert.equal(preempted, true); assert.equal(submit.mock.callCount(), 1);
  const steps = (lines as Array<{ msg: string; sink?: string; durationMs?: number }>).filter(line => line.msg === "checkpoint step");
  assert.ok(steps.length > 0);
  for (const step of steps) {
    assert.equal(step.sink, "done_checkpoint");
    assert.ok(Number.isFinite(step.durationMs));
    assert.ok(Number(step.durationMs) >= 0);
  }
});

for (const phase of ["initial", "fresh"] as const) {
  for (const error of [new GitBoundaryAbortError("hard"), new CheckpointUnsafeCleanupError("unsafe")]) {
    it(`m2 ${phase} runner import preserves ${error.checkpointBoundary} through advisory catches`, async t => {
      let active = false, calls = 0, verified = false;
      const fetch = git.fetchAgentBranch.bind(git);
      t.mock.method(git, "fetchAgentBranch", async (...args: Parameters<typeof git.fetchAgentBranch>) => {
        if (active) {
          calls++;
          if (phase === "fresh" && calls === 1) throw new Error("original import unavailable");
          throw error;
        }
        return fetch(...args);
      });
      t.mock.method(client, "codeCrossCheckStatus", async () => ({ stage: "code", result: "no_row", repo_instructions_enabled: false }));
      const submit = t.mock.method(client, "submitCodeCrossCheck", async () => assert.fail("hard/unsafe cannot become snapshot_failed"));
      let rig: ReturnType<typeof codexRig> | undefined;
      const executor: Executor = {
        get safety() { return rig?.safety; },
        run: async ctx => {
          rig = codexRig({ lifecycleSignal: AbortSignal.any([ctx.signal!, ctx.terminalLifecycleSignal!]) });
          commitInTree(ctx.worktreePath, "escape.txt", "hard boundary\n");
          active = true;
          if (phase === "initial") await assert.rejects(ctx.checkpoint!({ reap: true, sink: "done_checkpoint" }), e => e === error);
          else {
            await ctx.checkpoint!({ reap: true, sink: "done_checkpoint" });
            await assert.rejects(ctx.codeCrossCheckGate!({ interlocked: true }), e => e === error);
          }
          active = false; verified = true;
          return { branch: ctx.branch };
        },
      };
      await runnerWith(() => ({ executor }), fakeGitlab().gitlab, undefined, undefined, { checkpointIntervalMs: 0 })
        .execute(gitlabClaim(312306, { code_cross_check_required: true, claim_generation: 1 }));
      assert.equal(verified, true); assert.equal(submit.mock.callCount(), 0);
    });
  }
}

it("m2 known bridge preservation refusal invalidates the first snapshot before fresh recovery", async t => {
  const publishedBranch = "feature/m2-refused-bridge";
  execFileSync("git", ["-C", fx.originPath, "checkout", "-b", publishedBranch], { stdio: "pipe" });
  commitInTree(fx.originPath, "floor.txt", "published floor\n");
  execFileSync("git", ["-C", fx.originPath, "checkout", "main"], { stdio: "pipe" });
  let active = false, imports = 0, refused = false, verified = false, original = "";
  const fetch = git.fetchAgentBranch.bind(git), update = git.updateTrackingRef.bind(git);
  t.mock.method(git, "fetchAgentBranch", async (...args: Parameters<typeof git.fetchAgentBranch>) => {
    if (active) { imports++; assert.equal(args[4].expectedCandidateSha, original); }
    return fetch(...args);
  });
  t.mock.method(git, "updateTrackingRef", async (...args: Parameters<typeof git.updateTrackingRef>) => {
    if (active) { refused = true; return { kind: "not_updated", reason: "ownership_unknown" }; }
    return update(...args);
  });
  t.mock.method(client, "codeCrossCheckStatus", async () => ({ stage: "code", result: "no_row", repo_instructions_enabled: false }));
  const submit = t.mock.method(client, "submitCodeCrossCheck", async (_id: string, _generation: number, snapshot: Parameters<typeof client.submitCodeCrossCheck>[2]) => {
    assert.ok("head_commit" in snapshot); assert.equal(snapshot.head_commit, original);
    throw new Error("fixture response unavailable");
  });
  let rig: ReturnType<typeof codexRig> | undefined;
  const executor: Executor = {
    get safety() { return rig?.safety; },
    run: async ctx => {
      rig = codexRig({ lifecycleSignal: AbortSignal.any([ctx.signal!, ctx.terminalLifecycleSignal!]) });
      const base = await git.branchTip(fx.originPath, "main"); assert.ok(base);
      execFileSync("git", ["-C", ctx.worktreePath, "reset", "--hard", base], { stdio: "pipe" });
      commitInTree(ctx.worktreePath, "rewritten.txt", "rewritten head\n");
      original = (await git.branchTip(ctx.worktreePath, ctx.branch))!;
      active = true;
      await ctx.checkpoint!({ reap: true, sink: "done_checkpoint" });
      assert.equal(refused, true);
      await ctx.codeCrossCheckGate!({ interlocked: true });
      assert.equal(imports, 2, "known bridge refusal cannot retain a clean snapshot certificate");
      active = false; verified = true;
      return { branch: ctx.branch };
    },
  };
  await runnerWith(() => ({ executor }), fakeGitlab().gitlab, undefined, undefined, { checkpointIntervalMs: 0 })
    .execute(gitlabClaim(312307, { kind: "task", branch: publishedBranch, open_mr: false, code_cross_check_required: true, claim_generation: 1 }));
  assert.equal(verified, true); assert.equal(submit.mock.callCount(), 1);
});

for (const mode of ["now", "wall", "withdrawn-now"] as const) {
  it(`m2 genuine ${mode} during done leaves a clean registry for the production park route`, async () => {
    const restore = spyPublishLands();
    let rig: ReturnType<typeof codexRig> | undefined;
    let injected = false, checkpointReturned = false, cleanAfterDone: boolean | undefined;
    let parkResult: unknown;
    let capturedRig: ReturnType<typeof codexRig> | undefined;
    let captureProof: object | undefined;
    let captureDeadlineAt = 0, captureCallbacks = 0, capturesSettled = 0;
    const claim = gitlabClaim(312308);
    if (mode === "withdrawn-now") api.stateAckWire = (body, run) => body.status === "paused"
      ? { status: 200, body: { applied: false, run: { ...run, status: "running" } } } : undefined;
    const executor: Executor = {
      get safety() { return rig?.safety; },
      withCredentialFreeCapture: async (ms, action, purpose, signal, attempt) => {
        const current = rig!;
        capturedRig = current;
        captureDeadlineAt = Date.now() + ms;
        const credentials = current.refreshCalls() + current.releaseCalls();
        const result = await current.capture(ms, context => {
          captureCallbacks++;
          return action(context);
        }, purpose, signal, attempt);
        assert.equal(result.kind, "observed_empty");
        if (result.kind === "observed_empty") {
          assert.ok("proof" in result && result.proof !== null && typeof result.proof === "object");
          captureProof = result.proof;
          assert.equal(current.registry.acceptsCompletedCapture(captureProof), true,
            "capture callback and supervised children settled at the same poison revision");
        }
        assert.equal(current.refreshCalls() + current.releaseCalls(), credentials,
          "credential-free capture performs no refresh or release");
        capturesSettled++;
        return result;
      },
      run: async ctx => {
        const execution = new AbortController();
        const forward = () => { if (!(ctx.signal?.reason instanceof PauseNowSignal)) execution.abort(ctx.signal?.reason); };
        if (ctx.signal?.aborted) forward();
        else ctx.signal?.addEventListener("abort", forward, { once: true });
        rig = codexRig({ lifecycleSignal: AbortSignal.any([execution.signal, ctx.terminalLifecycleSignal!]) });
        const withBoundary = rig.safety.withBoundary.bind(rig.safety);
        rig.safety.withBoundary = async (request, action) => {
          if (!injected && request.sink === "done_checkpoint") {
            injected = true;
            const active = (worker as unknown as { activeRuns: Map<string, { steering: SteeringChannel }> }).activeRuns.get(claim.run_id)!;
            const route = active.steering as unknown as { route(kind: string, body?: string, id?: number): void };
            route.route("pause", mode === "wall" ? "wall" : "now", 1);
            if (mode === "withdrawn-now") route.route("pause_cancel", undefined, 2);
          }
          return withBoundary(request, action);
        };
        commitInTree(ctx.worktreePath, "pause-done.txt", "pause admission\n");
        await ctx.checkpoint!({ reap: true, sink: "done_checkpoint" });
        checkpointReturned = true;
        cleanAfterDone = !rig.registry.isPoisoned();
        const at = { completedCount: 1, total: 1 };
        if (mode === "wall") {
          parkResult = await ctx.parkForWall!(at);
          return parkResult === "parked" || parkResult === "undeliverable"
            ? { branch: ctx.branch, walled: { reason: "run exceeded its wall-clock timeout" } }
            : { branch: ctx.branch };
        }
        // A withdrawn turn remains dropped. The owner ACK decides the pending park.
        parkResult = await ctx.parkForPause!(at);
        if (mode === "withdrawn-now") {
          const oldRig = rig;
          assert.equal(parkResult, false);
          assert.ok(captureProof);
          assert.equal(oldRig.registry.acceptsCompletedCapture(captureProof), true);
          const disposal = await oldRig.safety.dispose({ boundary: "terminal",
            deadlineMs: Math.max(0, captureDeadlineAt - Date.now()) });
          assert.equal(disposal.kind, "disposed", "fresh epoch requires completed old-rig cleanup");
          assert.equal(oldRig.registry.acceptsCompletedCapture(captureProof), true);
          await oldRig.installer.settle();
          // Model recreateEpochAfterReap in CodexExecutor.run before normal activity resumes.
          rig = codexRig({ lifecycleSignal: AbortSignal.any([execution.signal, ctx.terminalLifecycleSignal!]) });
          assert.notEqual(rig.registry, oldRig.registry, "continuation never reuses the captured epoch");
        }
        return parkResult ? { branch: ctx.branch, pausedAt: at } : { branch: ctx.branch };
      },
    };
    const worker = runnerWith(() => ({ executor }), fakeGitlab().gitlab, undefined, undefined, { checkpointIntervalMs: 0 });
    try {
      await worker.execute(claim);
      assert.equal(injected, true);
      assert.equal(checkpointReturned, true);
      assert.equal(cleanAfterDone, true);
      assert.equal(parkResult, mode === "wall" ? "parked" : mode !== "withdrawn-now");
      if (mode === "wall") {
        const park = api.wallParkRequests.find(s => s.runId === claim.run_id);
        assert.match(String(park?.body.head), /^[0-9a-f]{40}$/, "the clean source permits a verified local wall capture");
      } else {
        assert.equal(captureCallbacks, 1, "pause enters the real credential-free capture callback");
        assert.equal(capturesSettled, 1, "pause awaits supervised capture settlement");
        assert.equal(capturedRig!.registry.isPoisoned(), true, "capture permanently closes old epoch admission");
        assert.deepEqual(capturedRig!.registry.reserveLaunch("provider"), { kind: "denied", reason: "admission_closed" });
        assert.equal(capturedRig!.registry.state(), "disposed");
        if (mode === "withdrawn-now") {
          assert.equal(capturedRig!.boundaries.includes("finalize"), false, "old epoch is never finalized");
          assert.ok(rig!.boundaries.includes("finalize"), "continuation finalizes on the fresh epoch");
          assert.equal(rig!.registry.isPoisoned(), false);
          assert.equal(rig!.registry.state(), "disposed");
        }
      }
      assert.ok(api.states.some(s => s.runId === claim.run_id && s.body.status === "paused") ||
        api.wallParkRequests.some(s => s.runId === claim.run_id));
      assert.equal(api.states.some(s => s.runId === claim.run_id && s.body.status === "completed"), mode === "withdrawn-now");
      assert.ok(!api.states.some(s => s.runId === claim.run_id && s.body.status === "failed"));
    } finally {
      restore();
      await Promise.allSettled([rig?.installer.settle() ?? Promise.resolve()]);
    }
  });
}

it("m2 real candidate acquisition mismatch is advisory without retention and guarded finalize continues", async t => {
  const { logger, lines } = recordingLogger();
  const snapshots: unknown[] = [];
  t.mock.method(client, "codeCrossCheckStatus", async () => ({ stage: "code", result: "no_row", repo_instructions_enabled: false }));
  t.mock.method(client, "submitCodeCrossCheck", async (_id: string, _gen: number, snapshot: Parameters<typeof client.submitCodeCrossCheck>[2]) => {
    snapshots.push(snapshot);
    throw new Error("fixture response unavailable");
  });
  let rig: ReturnType<typeof codexRig> | undefined;
  let mismatch = false, returned = false;
  let beforeReceipt: string | undefined, beforeHead: string | undefined, movedHead: string | undefined;
  let acquisitionState: { receipt: string; head: string; stage: string } | undefined;
  const receiptPath = (bare: string, branch: string) => path.join(bare, "uzi-owed",
    "receipt-" + createHash("sha256").update(branch).digest("hex") + ".json");
  const flags: Array<{ preserveRecoveryClone: boolean; preserveSession: boolean }> = [];
  const fetch = git.fetchAgentBranch.bind(git);
  t.mock.method(git, "fetchAgentBranch", async (...args: Parameters<typeof git.fetchAgentBranch>) => {
    if (mismatch && args[4].expectedCandidateSha) {
      commitInTree(args[1], "moved.txt", "candidate moved during acquisition\n");
      movedHead = execFileSync("git", ["-C", args[1], "rev-parse", "HEAD"], { encoding: "utf8" }).trim();
      try { return await fetch(...args); }
      finally {
        acquisitionState = {
          receipt: fs.readFileSync(receiptPath(args[0], args[2]), "utf8"),
          head: execFileSync("git", ["-C", args[0], "rev-parse", "refs/uzi-runner/" + args[2]], { encoding: "utf8" }).trim(),
          stage: execFileSync("git", ["-C", args[0], "for-each-ref", "--format=%(objectname)", "refs/uzi-incoming/"], { encoding: "utf8" }).trim(),
        };
      }
    }
    return fetch(...args);
  });
  const executor: Executor = {
    get safety() { return rig?.safety; },
    run: async ctx => {
      rig = codexRig({ lifecycleSignal: AbortSignal.any([ctx.signal!, ctx.terminalLifecycleSignal!]) });
      commitInTree(ctx.worktreePath, "prior.txt", "prior owned work\n");
      await ctx.checkpoint!({ reap: false });
      const bare = git.barePathFor(fx.originPath);
      beforeReceipt = fs.readFileSync(receiptPath(bare, ctx.branch), "utf8");
      beforeHead = await git.trackingTip(bare, ctx.branch) ?? undefined;
      commitInTree(ctx.worktreePath, "candidate.txt", "original\n");
      mismatch = true;
      await ctx.checkpoint!({ reap: true, sink: "done_checkpoint" });
      await ctx.codeCrossCheckGate!({ interlocked: true });
      mismatch = false; returned = true;
      return { branch: ctx.branch };
    },
  };
  const worker = runnerWith(() => ({ executor }), fakeGitlab().gitlab, undefined, logger, { checkpointIntervalMs: 0 });
  type Flight = { preserveRecoveryClone: boolean; preserveSession: boolean };
  const seam = worker as unknown as { fetchBackBestEffort(...args: [string, string, string, Flight, ...unknown[]]): Promise<unknown> };
  const original = seam.fetchBackBestEffort.bind(worker);
  t.mock.method(seam, "fetchBackBestEffort", async (...args: Parameters<typeof seam.fetchBackBestEffort>) => {
    try { return await original(...args); }
    finally { if (mismatch) flags.push({ preserveRecoveryClone: args[3].preserveRecoveryClone, preserveSession: args[3].preserveSession }); }
  });
  try {
    await worker.execute(gitlabClaim(312309, { code_cross_check_required: true, claim_generation: 1 }));
    assert.equal(returned, true);
    assert.ok(beforeReceipt && beforeHead && movedHead && acquisitionState);
    assert.deepEqual(acquisitionState, { receipt: beforeReceipt, head: beforeHead, stage: movedHead });
    assert.deepEqual(snapshots, [{ reason_class: "snapshot_failed" }]);
    assert.equal(flags.length, 1, "fresh snapshot sees the changed head before a second acquisition");
    assert.ok(flags.every(f => !f.preserveRecoveryClone && !f.preserveSession));
    assert.ok(!JSON.stringify(lines).includes("tracking preservation refused"), JSON.stringify(lines));
    assert.ok(rig!.boundaries.includes("finalize"), "guarded finalize still acquires its permit");
    assert.ok(api.states.some(s => s.body.status === "completed"));
  } finally { await Promise.allSettled([rig?.installer.settle() ?? Promise.resolve()]); }
});

it("a shutdown sink keeps its wall-clock budget when the test clock is far from wall time", async () => {
  // Issue #3217 (CodeRabbit on PR #3219): shutdown() stamps a Date.now deadline, so reapForSink must
  // keep Date.now for a shutdown request even when the test clock (absoluteNow) is set and far ahead.
  const farAhead = 1_000_000_000;
  const clock = virtualBoundaryClock();
  let rig: ReturnType<typeof codexRig> | undefined;
  let ready!: () => void;
  const started = new Promise<void>(resolve => { ready = resolve; });
  const shutdowns: Array<{ deadlineMs: number; deadlineAt: number }> = [];
  const executor: Executor = {
    get safety() { return rig?.safety; },
    run: async ctx => {
      rig = codexRig({ lifecycleSignal: AbortSignal.any([ctx.signal!, ctx.terminalLifecycleSignal!]),
        armDeadline: clock.armDeadline, now: clock.now });
      const boundary = rig.safety.withBoundary.bind(rig.safety);
      rig.safety.withBoundary = async (request, action) => {
        if (request.boundary === "shutdown" && request.gracefulShutdownCheckpoint) {
          shutdowns.push({ deadlineMs: request.deadlineMs, deadlineAt: request.gracefulShutdownCheckpoint.deadlineAt });
        }
        return boundary(request, action);
      };
      commitInTree(ctx.worktreePath, "shutdown.txt", "work to publish\n");
      ready();
      await new Promise<void>(resolve => {
        if (ctx.signal!.aborted) resolve();
        else ctx.signal!.addEventListener("abort", () => resolve(), { once: true });
      });
      throw new Error("provider unwound after shutdown");
    },
  };
  const worker = runnerWith(() => ({ executor }), fakeGitlab().gitlab, undefined, undefined, {
    checkpointTestHooks: { absoluteNow: () => clock.now() + farAhead, armSoftDeadline: clock.arm },
  });
  const execution = worker.execute(gitlabClaim(312311));
  try {
    await started;
    const shutdownAt = Date.now();
    worker.shutdown();
    await execution;
    assert.ok(shutdowns.length > 0, "the shutdown durability publish reached the safety");
    for (const s of shutdowns) {
      assert.ok(s.deadlineMs > 0, `shutdown budget ${s.deadlineMs} must stay positive`);
      assert.ok(s.deadlineMs <= s.deadlineAt - shutdownAt, "bounded by the wall-clock shutdown deadline");
    }
  } finally {
    worker.shutdown();
    await Promise.allSettled([execution, rig?.installer.settle() ?? Promise.resolve()]);
  }
});

// ---------------------------------------------------------------------------------------------
// Issue #3304 (code-snapshot half): the done-gate code-head read (`readBare` of the `--name-only`
// diff, a Codex prompt claim) must settle at its boundary. Only that one child is faked; the safety
// facade, registry, `spawnCheckpointProcess`, `exitGatedStream`, `spawnGit` and `readBare` are real.
// ---------------------------------------------------------------------------------------------

type DiffStdout = "never_ends" | "ended_unread" | "read_then_ended" | "destroyed" | "errored";
interface DiffChild {
  stdout: DiffStdout;
  /** `clean_exit`: waitChild resolves 0 at once. `until_cancelled`: it never resolves by itself. */
  exit: "clean_exit" | "until_cancelled";
  /** `unclean`: the whole-root reap fails. `dispose_throws`: the root's dispose rejects. */
  cleanup?: "unclean_reap" | "dispose_throws";
}
/** `soft_then_hard` records whether the gate was still pending after the soft deadline alone. The `during`
 *  actions replace the clock: an owner cancel, or a `now` pause, lands while the child is stalled. */
type Stop = "none" | "soft" | "hard" | "soft_then_hard" | "cancel" | "pause_now";

/** Observe the controlled child's consequences without advancing any wall-clock timer. */
async function observeTurns(done: () => boolean = () => false): Promise<boolean> {
  for (let turn = 0; !done() && turn < 200; turn++) {
    await new Promise<void>(resolve => setImmediate(resolve));
  }
  return done();
}

function eventSignal(): { ready: Promise<void>; mark: () => void } {
  let mark!: () => void;
  const ready = new Promise<void>(resolve => { mark = resolve; });
  return { ready, mark };
}

/** Track (and unref) every pending 600 s timer, so a red run can neither hang the process nor hide one. */
function trackReadTimers(t: TestContext): { pending: () => number; flush: () => number } {
  const real = globalThis.setTimeout, realClear = globalThis.clearTimeout;
  const live = new Map<unknown, () => void>();
  t.mock.method(globalThis, "setTimeout", ((fn: (...args: unknown[]) => void, ms?: number, ...rest: unknown[]) => {
    if (ms !== 600_000) return real(fn, ms, ...rest);
    const run = (): void => { live.delete(handle); fn(...rest); };
    const handle: NodeJS.Timeout = real(run, ms);
    handle.unref();
    live.set(handle, run);
    return handle;
  }) as unknown as typeof setTimeout);
  t.mock.method(globalThis, "clearTimeout", ((handle?: NodeJS.Timeout) => {
    live.delete(handle);
    return realClear(handle);
  }) as typeof clearTimeout);
  // Only a red run uses this: it plays the 600 s timers out so the run still ends by assertion.
  return { pending: () => live.size, flush: () => { const runs = [...live.values()]; runs.forEach(run => run()); return runs.length; } };
}

const isNameOnlyDiff = (request: BoundaryProcessRequest): boolean =>
  request.argv.includes("--name-only") && request.argv.includes("--no-textconv");

interface DiffRun {
  settledAtStop: boolean;
  settledAfterSoft: boolean | undefined;
  outcome: { kind: "resolved"; value: unknown } | { kind: "rejected"; error: unknown } | undefined;
  submitted: unknown[];
  lines: Array<{ msg?: string; outcome?: string; step?: string }>;
  events: string[];
  poisoned: boolean;
  timersAtStop: number;
  cancels: number;
  failedStates: number;
  spawnedNameOnly: number;
}

async function runDiffScenario(t: TestContext, issue: number, spec: {
  child: DiffChild; stop: Stop; claim?: Record<string, unknown>;
  /** Which boundary child is faked; the first matching spawn only. Default: the `--name-only` diff. */
  match?: (request: BoundaryProcessRequest) => boolean;
}): Promise<DiffRun> {
  const clock = virtualBoundaryClock();
  const started = clock.now();
  let deferredFenceCloses = 0;
  let maskedOperations = 0, maskedOperationSettled = false;
  if (spec.stop === "soft_then_hard") {
    const scoped = git as unknown as { runGit(cwd: string | undefined, args: string[], ...rest: unknown[]): Promise<string> };
    const runGit = scoped.runGit.bind(scoped);
    t.mock.method(scoped, "runGit", (...args: Parameters<typeof scoped.runGit>) => {
      const running = runGit(...args);
      if (args[1][0] === "update-ref" && args[1].some(arg => arg.startsWith("refs/uzi-cross-check/"))) {
        maskedOperations++;
        void running.then(() => { maskedOperationSettled = true; }, () => { maskedOperationSettled = true; });
      }
      return running;
    });
    const open = fs.promises.open.bind(fs.promises);
    t.mock.method(fs.promises, "open", async (...args: Parameters<typeof fs.promises.open>) => {
      const file = await open(...args);
      if (String(args[0]).includes("code-snapshot-") && String(args[0]).endsWith(".fence")) {
        const close = file.close.bind(file);
        t.mock.method(file, "close", async () => {
          await close();
          deferredFenceCloses++;
          // Inject delayed I/O delivery beyond the old observation cap. These turns
          // are fixture load, not a pass criterion or an extension of virtual time.
          for (let turn = 0; turn < 400; turn++) await new Promise<void>(resolve => setImmediate(resolve));
        });
      }
      return file;
    });
  }
  const { logger, lines } = recordingLogger();
  const timers = trackReadTimers(t);
  const events: string[] = [];
  const submitted: unknown[] = [];
  t.mock.method(client, "codeCrossCheckStatus", async () => ({ stage: "code", result: "no_row", repo_instructions_enabled: false }));
  t.mock.method(client, "recordCompletionAttempt", async () => ({ unmet: [], attemptCount: 1 }));
  t.mock.method(client, "submitCodeCrossCheck", async (_id: string, _generation: number, snapshot: unknown) => {
    submitted.push(snapshot);
    throw new Error("fixture response unavailable");
  });
  let nameOnly = 0, cancels = 0;
  const launched = eventSignal(), reaped = eventSignal();
  let stdout!: PassThrough;
  const spawnDiff = async (): Promise<SpawnedBoundaryProcess> => {
    stdout = new PassThrough();
    const stderr = new PassThrough(), stdin = new PassThrough();
    for (const stream of [stdout, stderr, stdin]) stream.on("error", () => undefined);
    const closed = new Promise<void>(resolve => stdout.once("close", () => resolve()));
    nameOnly++; launched.mark();
    events.push("spawned");
    if (spec.child.stdout === "ended_unread") { stdout.resume(); stdout.end(); await once(stdout, "end"); }
    else if (spec.child.stdout === "read_then_ended") { stdout.write("lost\n"); stdout.resume(); stdout.end(); await once(stdout, "end"); }
    else if (spec.child.stdout === "destroyed") { stdout.destroy(); await closed; }
    else if (spec.child.stdout === "errored") { stdout.destroy(new Error("fixture stdout failure")); await closed; }
    return {
      root: {
        kind: "boundary_action",
        reap: async () => {
          await new Promise<void>(resolve => setImmediate(resolve));
          events.push("reap");
          reaped.mark();
          return spec.child.cleanup === "unclean_reap"
            ? { ok: false as const, error: { category: "tool" as const, message: "fixture root did not reap" } }
            : { ok: true as const };
        },
        dispose: async () => {
          cancels++; events.push("dispose");
          if (spec.child.cleanup === "dispose_throws") throw new Error("fixture dispose failed");
        },
      },
      stdin, stdout, stderr,
      waitChild: () => spec.child.exit === "clean_exit" ? Promise.resolve({ code: 0 }) : new Promise(() => undefined),
    };
  };
  let rig: ReturnType<typeof codexRig> | undefined;
  let hardSignal: AbortSignal | undefined;
  let childStopSignals: AbortSignal[] | undefined;
  let scenarioError: unknown;
  let record: Omit<DiffRun, "lines" | "failedStates" | "submitted" | "spawnedNameOnly" | "cancels"> | undefined;
  const match = spec.match ?? isNameOnlyDiff;
  let matched = false;
  const claim = gitlabClaim(issue, { kind: "prompt", issue_iid: null, issue_description: "Do the prompt task.",
    code_cross_check_required: true, claim_generation: 1, ...spec.claim });
  const route = (kind: string, body?: string): void => {
    const active = (worker as unknown as { activeRuns: Map<string, { steering: { route(kind: string, body?: string, id?: number): void } }> })
      .activeRuns.get(claim.run_id)!;
    active.steering.route(kind, body, 1);
  };
  const executor: Executor = {
    get safety() { return rig?.safety; },
    run: async ctx => {
      // As the production run context does for a `now` pause: that abort never reaches the execution.
      const execution = new AbortController();
      const forward = (): void => { if (!(ctx.signal?.reason instanceof PauseNowSignal)) execution.abort(ctx.signal?.reason); };
      if (ctx.signal?.aborted) forward(); else ctx.signal?.addEventListener("abort", forward, { once: true });
      rig = codexRig({ lifecycleSignal: AbortSignal.any([execution.signal, ctx.terminalLifecycleSignal!]),
        armDeadline: clock.armDeadline, now: clock.now,
        boundaryProcess: request => {
          if (matched || !match(request)) return undefined;
          matched = true;
          if (spec.stop === "soft_then_hard") {
            childStopSignals = (git as unknown as { boundaryStopSignals(): AbortSignal[] }).boundaryStopSignals();
          }
          return spawnDiff();
        } });
      const withBoundary = rig.safety.withBoundary.bind(rig.safety);
      rig.safety.withBoundary = async (request, action) => {
        try {
          return await withBoundary(request, permit => {
            if (request.sink === "done_checkpoint") hardSignal = permit.signal;
            return action(permit);
          });
        }
        finally { if (request.sink === "done_checkpoint") events.push("boundary-returned"); }
      };
      commitInTree(ctx.worktreePath, "prompt.txt", "prompt candidate\n");
      let settled = false;
      const gate = ctx.codeCrossCheckGate!({ interlocked: false }).then(
        value => ({ kind: "resolved" as const, value }), error => ({ kind: "rejected" as const, error }));
      void gate.then(() => { settled = true; });
      try {
        if (spec.stop !== "none") {
          await launched.ready;
          // A clean exit must have fully settled (exit, reap, completion) BEFORE the stop arrives.
          if (spec.child.exit === "clean_exit") {
            await reaped.ready;
            await observeTurns();
          }
        }
        else {
          // The step-label control uses only real children. For fake early-EOF cases, first
          // reach the controlled completion, then observe whether the bounded read settled.
          if (spec.match) await gate;
          else { await launched.ready; await reaped.ready; await observeTurns(() => settled); }
        }
        let settledAfterSoft: boolean | undefined;
        if (spec.stop === "cancel") { route("cancel"); await observeTurns(() => settled); }
        else if (spec.stop === "pause_now") { route("pause", "now"); await observeTurns(() => settled); }
        else if (spec.stop === "soft_then_hard") {
          assert.equal(childStopSignals?.length, 1, "the masked mutation receives no soft stop signal");
          assert.equal(childStopSignals[0], hardSignal, "the masked mutation retains the held hard signal");
          clock.advance(60_000);
          settledAfterSoft = await observeTurns(() => settled);
          // This child exited cleanly before soft expiry, but its output never
          // ended. Observe runGit before fence cleanup can delay the outer gate.
          assert.equal(maskedOperations, 1, "the controlled masked operation was observed");
          assert.equal(cancels, 0, "soft expiry must not cancel the masked child");
          assert.equal(maskedOperationSettled, false, "soft expiry must leave the masked operation pending");
          assert.equal(hardSignal?.aborted, false, "the soft deadline must leave the masked hard signal live");
          clock.advance(30_000);
          assert.equal(hardSignal?.aborted, true, "hard stop did not settle the gate: hard deadline did not fire");
          // The hard stop owns settlement, including real fence-close I/O. A fixed
          // number of immediate turns cannot establish that this unwind is done.
          await gate;
          assert.equal(deferredFenceCloses, 1, "the delayed fence-close delivery was exercised");
          assert.equal(clock.now() - started, 90_000, "settlement used only the original hard deadline");
        } else if (spec.stop !== "none") {
          clock.advance(spec.stop === "soft" ? 60_000 : 90_000);
          await observeTurns(() => settled);
        }
        record = { settledAtStop: settled, settledAfterSoft, outcome: undefined, events, timersAtStop: timers.pending(),
          poisoned: rig.registry.isPoisoned() };
      } catch (error) {
        scenarioError = error;
        throw error;
      } finally {
        // Unblock pending work after a refusal without waiting for the 600 s timer.
        // A fired hard stop ignored by production is bounded by the existing test timeout.
        stdout?.destroy();
        if (!settled) timers.flush();
        if (!settled && !(await observeTurns(() => settled))) clock.advance(120_000);
        const outcome = await gate;
        if (record) record.outcome = outcome;
      }
      return { branch: ctx.branch };
    },
  };
  const worker = runnerWith(() => ({ executor }), fakeGitlab().gitlab, undefined, logger, {
    checkpointIntervalMs: 0,
    checkpointTestHooks: { absoluteNow: clock.now, armSoftDeadline: clock.arm },
  });
  await worker.execute(claim);
  if (scenarioError) throw scenarioError;
  assert.ok(record, "the executor reached the scenario record");
  return { ...record, submitted, lines: lines as DiffRun["lines"], cancels, spawnedNameOnly: nameOnly,
    failedStates: api.states.filter(s => s.body.status === "failed").length };
}

const attemptOutcomes = (r: DiffRun): Array<string | undefined> =>
  r.lines.filter(l => l.msg === "checkpoint attempt").map(l => l.outcome);

it("m3304 v1: a clean exit with a stdout that never ends settles at the soft deadline as snapshot_failed", async t => {
  const r = await runDiffScenario(t, 330401, { child: { stdout: "never_ends", exit: "clean_exit" }, stop: "soft" });
  assert.equal(r.spawnedNameOnly, 1);
  assert.equal(r.settledAtStop, true, "the gate settles at the soft deadline, not after the 600 s read timer");
  assert.deepEqual(r.submitted, [{ reason_class: "snapshot_failed" }]);
  assert.ok(r.lines.some(l => l.msg === "code snapshot unavailable"));
  assert.deepEqual(attemptOutcomes(r), ["soft_stop"]);
  assert.equal(r.failedStates, 0, "the run does not fail");
  assert.equal(r.poisoned, false);
  assert.equal(r.timersAtStop, 0, "no 600 s read timer is left pending");
  assert.ok(r.events.indexOf("reap") >= 0 && r.events.indexOf("reap") < r.events.indexOf("boundary-returned"),
    "the child's completion settled before the boundary returned");
});

it("m3304 v1: a hard expiry with a stdout that never ends is terminal and leaves no read timer", async t => {
  const r = await runDiffScenario(t, 330402, { child: { stdout: "never_ends", exit: "clean_exit" }, stop: "hard" });
  assert.equal(r.settledAtStop, true);
  assert.equal(r.outcome?.kind, "rejected");
  assert.equal((r.outcome as { error: Error }).error.name, "CodexBoundaryError");
  assert.deepEqual(r.submitted, [], "a hard expiry is never reported as snapshot_failed");
  assert.equal(r.timersAtStop, 0, "the hard expiry settles the read; its 600 s timer is cleared");
});

it("m3304 v2 control: a child running until cancelled settles at the soft deadline as snapshot_failed", async t => {
  const r = await runDiffScenario(t, 330403, { child: { stdout: "never_ends", exit: "until_cancelled" }, stop: "soft" });
  assert.equal(r.settledAtStop, true);
  assert.deepEqual(r.submitted, [{ reason_class: "snapshot_failed" }]);
  assert.deepEqual(attemptOutcomes(r), ["soft_stop"]);
  assert.equal(r.failedStates, 0);
  assert.equal(r.poisoned, false);
  assert.ok(r.events.indexOf("dispose") >= 0 && r.events.indexOf("reap") >= 0, "cancellation disposed and reaped the child");
});

for (const [name, child, stop] of [
  ["unclean reap after a clean exit", { stdout: "never_ends", exit: "clean_exit", cleanup: "unclean_reap" }, "none"],
  ["a throwing dispose while cancelled", { stdout: "never_ends", exit: "until_cancelled", cleanup: "dispose_throws" }, "soft"],
] as const) {
  it(`m3304 v3: ${name} stays terminal, never snapshot_failed`, async t => {
    const r = await runDiffScenario(t, name.startsWith("unclean") ? 330404 : 330405, { child, stop });
    assert.equal(r.settledAtStop, true);
    assert.equal(r.outcome?.kind, "rejected");
    assert.equal((r.outcome as { error: Error }).error.name, "CodexBoundaryError");
    assert.deepEqual(r.submitted, []);
  });
}

it("m3304 v4(i): a source that already ended with nothing read is an empty diff, so the gate proceeds without a stall", async t => {
  const r = await runDiffScenario(t, 330406, { child: { stdout: "ended_unread", exit: "clean_exit" }, stop: "none" });
  assert.equal(r.settledAtStop, true, "no stall: the empty ended source settles at the child's clean exit");
  assert.deepEqual(r.outcome, { kind: "resolved", value: { action: "proceed" } });
  assert.ok(!r.lines.some(l => l.msg === "code snapshot unavailable"));
  assert.deepEqual(r.submitted, []);
  assert.equal(r.timersAtStop, 0);
});

for (const [stdout, label] of [
  ["read_then_ended", "data already read"], ["destroyed", "destroyed without end"], ["errored", "errored"],
] as const) {
  it(`m3304 v4: a source with ${label} at the handle is refused as snapshot_failed, never an empty diff`, async t => {
    const r = await runDiffScenario(t, 330407 + ["read_then_ended", "destroyed", "errored"].indexOf(stdout),
      { child: { stdout, exit: "clean_exit" }, stop: "none" });
    assert.equal(r.settledAtStop, true, "settles at the child's clean exit");
    assert.deepEqual(r.submitted, [{ reason_class: "snapshot_failed" }]);
    assert.ok(r.lines.some(l => l.msg === "code snapshot unavailable"));
    assert.equal(r.failedStates, 0);
    assert.equal(r.timersAtStop, 0);
  });
}

it("m3304 v1 on an unmasked shared read (merge-base in the capture) settles at the soft deadline as snapshot_failed", async t => {
  const r = await runDiffScenario(t, 330411, { child: { stdout: "never_ends", exit: "clean_exit" }, stop: "soft",
    match: request => request.argv.includes("merge-base") && !request.argv.includes("--is-ancestor") });
  assert.equal(r.spawnedNameOnly, 1, "the unmasked merge-base child was the stalled one");
  assert.equal(r.settledAtStop, true, "the collector settles at the soft deadline after the child's completion");
  assert.deepEqual(r.submitted, [{ reason_class: "snapshot_failed" }]);
  assert.deepEqual(attemptOutcomes(r), ["soft_stop"]);
  assert.equal(r.failedStates, 0);
  assert.equal(r.poisoned, false);
});

it("m3304 negative: a masked child (the pin's update-ref) stays hard-only, so soft leaves it pending and hard is terminal", async t => {
  const r = await runDiffScenario(t, 330412, { child: { stdout: "never_ends", exit: "clean_exit" }, stop: "soft_then_hard",
    match: request => request.argv.includes("update-ref") && request.argv.some(arg => arg.startsWith("refs/uzi-cross-check/")) });
  assert.equal(r.spawnedNameOnly, 1, "the masked update-ref child was the stalled one");
  assert.equal(r.settledAfterSoft, false, "a started mutation is never reported half-done at the soft deadline");
  assert.equal(r.settledAtStop, true);
  assert.equal(r.outcome?.kind, "rejected");
  assert.equal((r.outcome as { error: Error }).error.name, "CodexBoundaryError");
  assert.deepEqual(r.submitted, []);
});

it("m3304 negative: an owner cancel during the stall is terminal, never snapshot_failed", async t => {
  const r = await runDiffScenario(t, 330413, { child: { stdout: "never_ends", exit: "clean_exit" }, stop: "cancel" });
  assert.equal(r.settledAtStop, true);
  assert.equal(r.outcome?.kind, "rejected");
  assert.equal((r.outcome as { error: Error }).error.name, "CodexBoundaryError");
  assert.deepEqual(r.submitted, []);
});

it("m3304 negative: a now-pause during the stall settles without failing the run", async t => {
  const r = await runDiffScenario(t, 330414, { child: { stdout: "never_ends", exit: "clean_exit" }, stop: "pause_now" });
  assert.equal(r.settledAtStop, true, "the pause stops the stalled read");
  assert.equal(r.failedStates, 0);
  assert.equal(r.poisoned, false);
});

it("m3304 negative: a claim without code_cross_check_required has no gate and never reads the code head", async () => {
  let rig: ReturnType<typeof codexRig> | undefined;
  let gate: unknown = "unset";
  const executor: Executor = {
    get safety() { return rig?.safety; },
    run: async ctx => {
      rig = codexRig({ lifecycleSignal: AbortSignal.any([ctx.signal!, ctx.terminalLifecycleSignal!]) });
      commitInTree(ctx.worktreePath, "plain.txt", "no cross-check\n");
      await ctx.checkpoint!({ reap: true, sink: "done_checkpoint" });
      gate = ctx.codeCrossCheckGate;
      return { branch: ctx.branch };
    },
  };
  await runnerWith(() => ({ executor }), fakeGitlab().gitlab, undefined, undefined, { checkpointIntervalMs: 0 })
    .execute(gitlabClaim(330415, { kind: "prompt", issue_iid: null, issue_description: "Do the prompt task." }));
  assert.equal(gate, undefined);
  assert.ok(!rig!.processArgv.some(argv => argv.includes("--name-only") && argv.includes("--no-textconv")));
});

// ---------------------------------------------------------------------------------------------
// Issue #3304 (done-body half): the reaped done checkpoint's own code-head capture takes the same
// snapshot, so an unmasked shared read that stalls there must settle at the soft deadline too.
// ---------------------------------------------------------------------------------------------

async function runDoneBodyScenario(t: TestContext, issue: number, spec: {
  exit: "clean_exit" | "until_cancelled"; stop: "soft" | "none"; match?: (request: BoundaryProcessRequest) => boolean;
}): Promise<{ settled: boolean; error: unknown; poisoned: boolean; lines: DiffRun["lines"]; failedStates: number; spawnedNameOnly: number; spawnedStalled: number }> {
  const clock = virtualBoundaryClock();
  const { logger, lines } = recordingLogger();
  const timers = trackReadTimers(t);
  let stalled = 0, nameOnly = 0;
  const launched = eventSignal(), reaped = eventSignal();
  let stdout: PassThrough | undefined;
  const match = spec.match ?? (request => request.argv.includes("merge-base") && !request.argv.includes("--is-ancestor"));
  let matched = false;
  let rig: ReturnType<typeof codexRig> | undefined;
  let result: { settled: boolean; error: unknown; poisoned: boolean } | undefined;
  const spawnStalled = async (): Promise<SpawnedBoundaryProcess> => {
    stalled++;
    launched.mark();
    const out = stdout = new PassThrough();
    const stderr = new PassThrough(), stdin = new PassThrough();
    for (const stream of [out, stderr, stdin]) stream.on("error", () => undefined);
    return {
      root: {
        kind: "boundary_action",
        reap: async () => { await new Promise<void>(resolve => setImmediate(resolve)); reaped.mark(); return { ok: true as const }; },
        dispose: async () => undefined,
      },
      stdin, stdout: out, stderr,
      waitChild: () => spec.exit === "clean_exit" ? Promise.resolve({ code: 0 }) : new Promise(() => undefined),
    };
  };
  const executor: Executor = {
    get safety() { return rig?.safety; },
    run: async ctx => {
      rig = codexRig({ lifecycleSignal: AbortSignal.any([ctx.signal!, ctx.terminalLifecycleSignal!]),
        armDeadline: clock.armDeadline, now: clock.now,
        boundaryProcess: request => {
          if (isNameOnlyDiff(request)) nameOnly++;
          return spec.stop !== "none" && !matched && match(request) ? (matched = true, spawnStalled()) : undefined;
        } });
      commitInTree(ctx.worktreePath, "done-body.txt", "done body candidate\n");
      let settled = false, error: unknown;
      const done = ctx.checkpoint!({ reap: true, sink: "done_checkpoint" }).catch(err => { error = err; }).then(() => { settled = true; });
      try {
        if (spec.stop !== "none") {
          await launched.ready;
          if (spec.exit === "clean_exit") {
            await reaped.ready;
            await observeTurns();
          }
          clock.advance(60_000);
        }
        await observeTurns(() => settled);
        result = { settled, error, poisoned: rig.registry.isPoisoned() };
      } finally {
        stdout?.destroy();
        if (!settled) timers.flush();
        if (!settled && !(await observeTurns(() => settled))) clock.advance(120_000);
        await done;
        if (result) result.error = error;
      }
      return { branch: ctx.branch };
    },
  };
  const worker = runnerWith(() => ({ executor }), fakeGitlab().gitlab, undefined, logger, {
    checkpointIntervalMs: 0,
    checkpointTestHooks: { absoluteNow: clock.now, armSoftDeadline: clock.arm },
  });
  await worker.execute(gitlabClaim(issue, { code_cross_check_required: true, claim_generation: 1 }));
  assert.ok(result, "the executor reached the scenario record");
  return { ...result, lines: lines as DiffRun["lines"], spawnedNameOnly: nameOnly, spawnedStalled: stalled,
    failedStates: api.states.filter(s => s.body.status === "failed").length };
}

for (const exit of ["clean_exit", "until_cancelled"] as const) {
  it(`m3304 done body ${exit === "clean_exit" ? "v1" : "v2 control"}: an unmasked shared read stalling in the code-head capture settles at the soft deadline`, async t => {
    const r = await runDoneBodyScenario(t, exit === "clean_exit" ? 330421 : 330422, { exit, stop: "soft" });
    assert.equal(r.spawnedStalled, 1, "the unmasked merge-base child was the stalled one");
    assert.equal(r.settled, true, "the done checkpoint settles at the soft deadline, not after the 600 s read timer");
    assert.deepEqual(attemptOutcomes(r as unknown as DiffRun), ["soft_stop"]);
    assert.equal(r.failedStates, 0, "the run does not fail");
    assert.equal(r.poisoned, false);
    // An issue claim never runs the prompt-only changed-files diff; this pins that the done body did not either.
    assert.equal(r.spawnedNameOnly, 0, "an issue claim's done body never runs the prompt-only changed-files diff");
    // The done-body caller passes its step callback: the stall is reported under the pin sub-step.
    const steps = r.lines.filter(l => l.msg === "checkpoint step").map(l => l.step);
    assert.equal(steps.at(-1), "checkpoint_code_head_pin", `the stalled read is labelled by the done-body pin step, saw ${JSON.stringify(steps)}`);
  });
}

it("m3304 steps: the code-head capture reports each split step in checkpoint step records", async t => {
  const r = await runDiffScenario(t, 330423, { child: { stdout: "ended_unread", exit: "clean_exit" }, stop: "none", match: () => false });
  const steps = r.lines.filter(l => l.msg === "checkpoint step").map(l => l.step);
  const split = ["runner_tip", "tracking", "default_tip", "pin", "changed_files"].map(s => `checkpoint_code_head_${s}`);
  let at = 0;
  for (const step of steps) if (step === split[at]) at++;
  assert.equal(at, split.length, `the split steps appear in capture order, saw ${JSON.stringify(steps)}`);
});
