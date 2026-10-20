import assert from "node:assert/strict";
import { it } from "node:test";
import { CodexBoundaryError } from "../src/codex/safety.js";
import type { GracefulShutdownCheckpoint } from "../src/harness.js";
import { shutdownContentProof } from "./shutdown-3101-content-fixture.js";
import type { Executor } from "../src/executor.js";
import { CheckpointSoftDeadlineError, CheckpointUnsafeCleanupError } from "../src/checkpoint-stop.js";
import { MaintenanceBudget } from "../src/maintenance-budget.js";
import { CredentialSwitchSignal } from "../src/steering.js";
import { StaleClaimError } from "../src/runner.js";
import { api, client, deferred, fakeGitHub, fakeGitlab, fx, git, gitlabClaim, installHarness, runnerWith } from "./runner-harness.js";
import { codexRig, commitInTree, drain, installRecoveryRootCleanup, virtualBoundaryClock } from "./runner-codex-sinks-fixture.js";
import { recordingLogger, waitForTestEvent } from "./helpers.js";

installHarness();
installRecoveryRootCleanup();

const scenarios = ["clean", "soft fired", "transport", "stale", "wall", "credential", "unsafe", "hard",
  "shutdown active", "shutdown queued", "shutdown barrier", "shutdown before report"] as const;
for (const [index, scenario] of scenarios.entries()) {
  it(`m4 DONE soft progress: ${scenario}`, async t => {
    const clock = virtualBoundaryClock();
    const { logger, lines } = recordingLogger();
    const predecessor = deferred(), predecessorStarted = deferred(), admission = deferred();
    const active = deferred(), aborted = deferred(), cleanup = deferred(), checkpointSettled = deferred(), finish = deferred();
    const barrier = deferred();
    let barrierSettled = false;
    void barrier.promise.then(() => { barrierSettled = true; });
    const operations: Promise<unknown>[] = [];
    const events: string[] = [];
    const reports: Array<{ body: Parameters<typeof client.reportState>[1]; signal?: AbortSignal }> = [];
    const floor: unknown[] = [];
    let rig: ReturnType<typeof codexRig> | undefined;
    let armed = false, packs = 0, uploads = 0, returned = false, executorFailure: unknown;
    let checkpointError: unknown, permitReleased = false, heldBeforeCleanup = false;
    let tracking: string | null = null, candidate = "", before: unknown, after: unknown;
    const pack = git.checkpointPack.bind(git);
    t.mock.method(git, "checkpointPack", async (...args: Parameters<typeof git.checkpointPack>) => {
      if (!armed) return pack(...args);
      // Shutdown's independent preservation may subsequently ask for another pack.
      if (packs) return null;
      packs++;
      tracking = await git.trackingTip(args[0], args[1]);
      if (scenario === "soft fired") clock.advance(60_000);
      if (scenario === "shutdown before report") worker.shutdown();
      throw new CheckpointSoftDeadlineError();
    });
    t.mock.method(client, "publishCheckpoint", async (_id: string, _tip: string, stream: Parameters<typeof client.publishCheckpoint>[2]) => {
      uploads++; await drain(stream);
      return { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/m4" } };
    });
    const report = client.reportState.bind(client);
    t.mock.method(client, "reportState", async (...args: Parameters<typeof client.reportState>) => {
      const [id, body, signal] = args;
      if (body.milestones_completed?.includes("predecessor")) {
        events.push("predecessor start"); predecessorStarted.resolve();
        await predecessor.promise;
        events.push("predecessor end");
      }
      if (body.milestones_completed?.includes("following")) events.push("following start");
      if (!body.milestones_completed?.includes("m4")) return report(...args);
      reports.push({ body: structuredClone(body), signal });
      events.push("progress start"); active.resolve();
      if (scenario === "transport") throw new Error("ordinary transport unavailable");
      if (scenario === "stale") return { staleClaim: true };
      if (scenario === "wall") return { staleClaim: true, status: "paused", holdReason: "budget_exhausted" };
      if (scenario === "credential") throw new CredentialSwitchSignal();
      if (scenario === "unsafe") throw new CheckpointUnsafeCleanupError("unsafe progress refusal");
      if (scenario === "hard" || scenario === "shutdown active") {
        await new Promise<void>(resolve => {
          const stop = () => { events.push("progress abort"); aborted.resolve(); resolve(); };
          if (signal?.aborted) stop(); else signal?.addEventListener("abort", stop, { once: true });
        });
        await cleanup.promise;
        events.push("progress cleanup");
        signal!.throwIfAborted();
      }
      return report(id, body, signal);
    });
    const wait = MaintenanceBudget.waitFor.bind(MaintenanceBudget);
    t.mock.method(MaintenanceBudget, "waitFor", <T>(pending: Promise<T>, signal?: AbortSignal) => {
      const result = wait(pending, signal);
      if (armed && signal && (scenario === "shutdown queued" || scenario === "shutdown barrier")) admission.resolve();
      return result;
    });
    type ObservedFlight = { lastPublishedTip?: string; checkpointFloor?: string; lastCheckpointRefTip?: string;
      checkedStateBarrier?: Promise<void> };
    let observed: ObservedFlight | undefined;
    const snapshot = () => ({ lastPublishedTip: observed?.lastPublishedTip, checkpointFloor: observed?.checkpointFloor,
      lastCheckpointRefTip: observed?.lastCheckpointRefTip });
    const executor: Executor = {
      get safety() { return rig?.safety; },
      run: async ctx => {
        try {
          rig = codexRig({ lifecycleSignal: AbortSignal.any([ctx.signal!, ctx.terminalLifecycleSignal!]),
            now: clock.now, armDeadline: clock.armDeadline });
          const boundary = rig.safety.withBoundary.bind(rig.safety);
          rig.safety.withBoundary = async (request, action) => {
            try { return await boundary(request, action); }
            finally { if (request.sink === "done_checkpoint") { permitReleased = true; events.push("permit released"); } }
          };
          commitInTree(ctx.worktreePath, "m4.txt", "soft progress candidate\n");
          candidate = (await git.branchTip(ctx.worktreePath, ctx.branch))!;
          await ctx.checkpoint!({ reap: false });
          before = snapshot();
          if (scenario === "shutdown queued") {
            await ctx.reportProgress!({ completed: ["predecessor"], in_progress: [] });
            await predecessorStarted.promise;
          }
          if (scenario === "shutdown barrier") observed!.checkedStateBarrier = barrier.promise;
          armed = true;
          const checkpoint = ctx.checkpoint!({ reap: true, sink: "done_checkpoint",
            progress: { completed: ["m4"], in_progress: [] } }).then(
              () => { returned = true; },
              error => { checkpointError = error; },
            ).finally(() => { after = snapshot(); checkpointSettled.resolve(); });
          operations.push(checkpoint);
          await checkpoint;
          if (scenario === "shutdown queued") await ctx.reportProgress!({ completed: ["following"], in_progress: [] });
          await finish.promise;
          throw new Error("test completed observation");
        } catch (error) { executorFailure = error; throw error; }
      },
    };
    const { github } = fakeGitHub();
    const forge = fakeGitlab();
    const claim = gitlabClaim(312400 + index, {
      repo: { id: "r1", url: "https://github.com/org/repo", clone_url: fx.originPath, forge_type: "github" },
    });
    const worker = runnerWith(() => ({ executor }), forge.gitlab, undefined, logger, {
      github, checkpointIntervalMs: 0, checkpointTickIntervalMs: 0,
      checkpointTestHooks: { absoluteNow: clock.now, armSoftDeadline: (fire, ms) => clock.arm(fire, ms),
        afterPinnedPublish: state => floor.push(state), afterUnpinnedPublish: state => floor.push(state) },
    });
    const seam = worker as unknown as { runGatedSink<T>(flight: ObservedFlight, action: () => Promise<T>): Promise<T> };
    const sink = seam.runGatedSink.bind(worker);
    t.mock.method(seam, "runGatedSink", <T>(flight: ObservedFlight, action: () => Promise<T>) => {
      observed = flight; return sink(flight, action);
    });
    const execution = worker.execute(claim);
    operations.push(execution);
    let observationFailure: unknown;
    try {
      if (scenario === "hard" || scenario === "shutdown active") {
        await waitForTestEvent(active.promise, "active DONE progress");
        if (scenario === "hard") clock.advance(90_000); else worker.shutdown();
        await waitForTestEvent(aborted.promise, "progress receives abort");
        await new Promise<void>(resolve => setImmediate(resolve));
        heldBeforeCleanup = !permitReleased;
        cleanup.resolve();
      } else if (scenario === "shutdown queued" || scenario === "shutdown barrier") {
        await waitForTestEvent(admission.promise, "inert progress admission");
        worker.shutdown();
      }
      await waitForTestEvent(checkpointSettled.promise, "DONE checkpoint settles");
      if (scenario === "shutdown queued") {
        await new Promise<void>(resolve => setImmediate(resolve));
        assert.deepEqual(events.filter(event => event.includes("predecessor") || event.includes("following")),
          ["predecessor start"], "following report stays behind cancelled middle and predecessor");
        predecessor.resolve();
      }
      if (scenario === "shutdown barrier") {
        assert.equal(barrierSettled, false, "cancelling waiter leaves barrier owner pending");
        assert.strictEqual(observed!.checkedStateBarrier, barrier.promise);
        barrier.resolve();
      }
    } catch (error) { observationFailure = error; }
    finally {
      predecessor.resolve(); barrier.resolve(); cleanup.resolve(); finish.resolve();
      await Promise.allSettled(operations);
    }
    assert.equal(observationFailure, undefined);
    assert.equal(executorFailure instanceof Error && executorFailure.message, "test completed observation");
    assert.equal(packs, 1, "real Git import reaches permit-owned pack");
    assert.equal(tracking, candidate, "local candidate imported before soft pack stop");
    assert.deepEqual(after, before, "soft stop preserves unpublished floor");
    assert.equal(uploads, 0);
    assert.deepEqual(floor, []);
    const sends = !["shutdown queued", "shutdown barrier", "shutdown before report"].includes(scenario);
    assert.equal(reports.length, sends ? 1 : 0, "cancelled progress never starts late");
    if (sends) {
      assert.ok(reports[0]!.signal, "DONE progress carries hard plus lifecycle cancellation");
      assert.deepEqual(reports[0]!.body.milestones_completed, ["m4"]);
      assert.equal(reports[0]!.body.head, undefined);
      assert.equal(reports[0]!.body.checkpoint_contains_latest, undefined);
    }
    if (scenario === "clean" || scenario === "soft fired" || scenario === "transport") {
      assert.equal(returned, true);
      assert.equal(checkpointError, undefined);
      assert.equal(reports[0]!.signal!.aborted, false, "cooperative soft stop does not cancel bookkeeping");
      if (scenario !== "transport") assert.ok(api.states.some(s => s.body.milestones_completed?.includes("m4")));
    }
    if (scenario === "stale") assert.ok(checkpointError instanceof StaleClaimError);
    if (scenario === "wall") assert.equal((checkpointError as Error)?.constructor.name, "ServerWallParkedError");
    if (scenario === "credential") assert.ok(checkpointError instanceof CredentialSwitchSignal);
    if (scenario === "unsafe") assert.ok(checkpointError instanceof CheckpointUnsafeCleanupError);
    if (scenario === "hard" || scenario === "shutdown active") {
      assert.equal(heldBeforeCleanup, true, "active transport cleanup finishes before permit release");
      assert.ok(events.indexOf("progress cleanup") < events.indexOf("permit released"));
      if (scenario === "hard") {
        assert.ok(checkpointError instanceof CodexBoundaryError, "hard deadline cannot become soft success");
        assert.equal(checkpointError.stage, "action");
        assert.equal(checkpointError.boundary, "checkpoint");
        assert.equal(checkpointError.sink, "done_checkpoint");
        assert.equal(checkpointError.step, "checkpoint_report");
        assert.ok(checkpointError.errors.some(error => error.category === "timeout"));
      }
    }
    if (["stale", "wall", "credential", "unsafe", "hard"].includes(scenario)) {
      const reportSteps = (lines as Array<{ msg?: string; step?: string; sink?: string; outcome?: string; durationMs?: number }>)
        .filter(line => line.msg === "checkpoint step" && line.step === "checkpoint_report" && line.sink === "done_checkpoint");
      assert.equal(reportSteps.length, 1, "the failed report substep is recorded exactly once");
      assert.equal(reportSteps[0]!.outcome, "failed");
      assert.ok(Number.isFinite(reportSteps[0]!.durationMs));
    }
    if (scenario.startsWith("shutdown")) {
      assert.equal(returned, true);
      assert.equal(checkpointError, undefined);
      assert.equal(rig!.registry.isPoisoned(), false);
    }
    if (scenario === "shutdown queued") assert.ok(events.indexOf("predecessor end") < events.indexOf("following start"));
  });
}

it("m4 soft DONE active progress shutdown preserves content within the original grace", async t => {
  const clock = virtualBoundaryClock();
  t.mock.method(Date, "now", clock.now);
  client.protocolFeatures = ["run_checkpoint_durability"];
  const iid = 312420;
  const proof = shutdownContentProof(fx, client, iid);
  const active = deferred(), aborted = deferred(), cleanup = deferred();
  const operations: Promise<unknown>[] = [];
  let rig: ReturnType<typeof codexRig> | undefined;
  let armed = false, softPacks = 0, shutdownPacks = 0;
  let doneReturned = false, permitReleased = false, cleanupSettled = false;
  let checkpointError: unknown, setupError: unknown;
  let shutdownAt = 0, ackAt = 0;
  let shutdownProof: GracefulShutdownCheckpoint | undefined;
  proof.onShutdownReceipt(() => { ackAt = clock.now(); });
  const pack = git.checkpointPack.bind(git);
  t.mock.method(git, "checkpointPack", (...args: Parameters<typeof git.checkpointPack>) => {
    if (armed && softPacks === 0) {
      softPacks++;
      throw new CheckpointSoftDeadlineError();
    }
    if (shutdownProof) shutdownPacks++;
    return pack(...args);
  });
  const report = client.reportState.bind(client);
  t.mock.method(client, "reportState", async (...args: Parameters<typeof client.reportState>) => {
    if (!args[1].milestones_completed?.includes("shutdown-content")) return report(...args);
    const signal = args[2];
    if (!signal) throw new Error("active progress has no combined cancellation signal");
    active.resolve();
    await new Promise<void>(resolve => {
      const stop = () => { aborted.resolve(); resolve(); };
      if (signal.aborted) stop(); else signal.addEventListener("abort", stop, { once: true });
    });
    // This transport owns cleanup even after cancellation; the permit must await it.
    await cleanup.promise;
    cleanupSettled = true;
    signal.throwIfAborted();
    return report(...args);
  });
  const executor: Executor = {
    get safety() { return rig?.safety; },
    run: async ctx => {
      rig = codexRig({ lifecycleSignal: AbortSignal.any([ctx.signal!, ctx.terminalLifecycleSignal!]),
        now: clock.now, armDeadline: clock.armDeadline });
      const boundary = rig.safety.withBoundary.bind(rig.safety);
      rig.safety.withBoundary = async (request, action) => {
        if (request.boundary === "shutdown") shutdownProof = request.gracefulShutdownCheckpoint;
        try { return await boundary(request, action); }
        finally { if (request.sink === "done_checkpoint") permitReleased = true; }
      };
      clock.advance(1);
      try { await proof.prepare(ctx); }
      catch (error) { setupError = error; throw error; }
      armed = true;
      try {
        await ctx.checkpoint!({ reap: true, sink: "done_checkpoint",
          progress: { completed: ["shutdown-content"], in_progress: [] } });
        doneReturned = true;
      } catch (error) { checkpointError = error; }
      throw new Error("provider unwound after soft progress shutdown");
    },
  };
  const worker = runnerWith(() => ({ executor }), fakeGitlab().gitlab, undefined, undefined, {
    github: fakeGitHub().github, checkpointIntervalMs: 1, checkpointTickIntervalMs: 0,
    checkpointTestHooks: { absoluteNow: clock.now, armSoftDeadline: clock.arm },
  });
  const execution = worker.execute(gitlabClaim(iid, { claim_generation: 1, repo: {
    id: "r1", url: "https://github.com/org/repo", clone_url: fx.originPath,
    forge_type: "github", default_branch: "main",
  } }));
  operations.push(execution);
  try {
    await waitForTestEvent(Promise.race([active.promise, execution.then(() => {
      throw new Error("execution ended before active progress: " + String(setupError));
    })]), "active progress after real import and soft pack", 20_000);
    shutdownAt = clock.now();
    worker.shutdown();
    await waitForTestEvent(aborted.promise, "active progress lifecycle abort", 20_000);
    clock.advance(5_000);
    await new Promise<void>(resolve => setImmediate(resolve));
    assert.equal(ackAt, 0, "no shutdown ACK while active HTTP cleanup is held");
    assert.equal(permitReleased, false, "DONE permit retains the active HTTP cleanup");
    assert.equal(doneReturned, false);
    assert.equal(cleanupSettled, false);
    cleanup.resolve();
    await execution;
    await proof.verify(git);
    assert.equal(setupError, undefined);
    assert.equal(doneReturned, true);
    assert.equal(checkpointError, undefined);
    assert.equal(rig!.registry.isPoisoned(), false);
    assert.equal(cleanupSettled, true);
    assert.equal(permitReleased, true);
    assert.equal(softPacks, 1);
    assert.ok(shutdownPacks > 0, "dedicated shutdown packs use the real implementation");
    assert.ok(shutdownProof, "genuine typed shutdown request observed");
    assert.equal(shutdownProof.deadlineAt, shutdownAt + 15_000);
    assert.ok(ackAt >= shutdownAt + 5_000 && ackAt < shutdownProof.deadlineAt);
  } finally {
    cleanup.resolve();
    worker.shutdown();
    await Promise.allSettled(operations);
    proof.cleanup();
  }
});
