import assert from "node:assert/strict";
import { it } from "node:test";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { createHash } from "node:crypto";
import { SinkGate } from "../src/sink-gate.js";
import type { CheckpointStop } from "../src/checkpoint-stop.js";
import type { Executor } from "../src/executor.js";
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
    const claim = gitlabClaim(312308);
    if (mode === "withdrawn-now") api.stateAckWire = (body, run) => body.status === "paused"
      ? { status: 200, body: { applied: false, run: { ...run, status: "running" } } } : undefined;
    const executor: Executor = {
      get safety() { return rig?.safety; },
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
      } else assert.equal(rig!.registry.isPoisoned(), false);
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
