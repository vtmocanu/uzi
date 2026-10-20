import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { PassThrough } from "node:stream";
import { setImmediate } from "node:timers/promises";
import os from "node:os";
import { GitCache, assertCaptureLaunch } from "../src/git.js";
import { createCodexExecutionSafety, CodexBoundaryError } from "../src/codex/safety.js";
import { ExecutionRegistry, newLocalExecutionEpoch } from "../src/codex/registry.js";
import { boundaryProcessSpawnerForTest } from "../src/codex/codex-executor.js";
import type { CodexRootHandle } from "../src/codex/launcher.js";
import type { BoundaryRequest } from "../src/harness.js";
import { latch } from "./credential-free-git-fixture.js";
import { nullLogger } from "./helpers.js";

const budget = 30_000;
function checkpoint(): BoundaryRequest {
  return { boundary: "checkpoint", sink: "done_checkpoint", deadlineMs: budget,
    activeStep: () => "checkpoint_lock_wait",
    checkpointStop: { signal: new AbortController().signal } };
}

// Real safety, registry, GitCache producer and private production adapter.
// Only supervisor handles are fake: clean disposal models a full join, not kernel proof.
function rig() {
  const registry = new ExecutionRegistry(newLocalExecutionEpoch(2852));
  const git = new GitCache(os.tmpdir(), nullLogger());
  const childReaping = latch(), releaseChild = latch();
  const timers: { request: BoundaryRequest; ms: number; fire: () => void; cancelled: boolean }[] = [];
  let launches = 0, reconciles = 0, terminalHooks = 0, poisonWrites = 0;
  const poison = registry.poison.bind(registry);
  registry.poison = (...args) => { poisonWrites++; return poison(...args); };
  const adapter = boundaryProcessSpawnerForTest("required", async spec => {
    assertCaptureLaunch(spec);
    assert.equal(spec.identity, "command");
    assert.ok(spec.args.includes("status"), "only canonical worktreeStatus is modeled");
    const index = ++launches;
    const stdout = new PassThrough(); stdout.end(" M checkpoint-sentinel.txt\n");
    return {
      started: { event: "started", supervisorPid: 300, childPid: 301, subreaper: true,
        nondumpable: true, uid: 10003, liveCapsZero: true, capBoundingSet: "0xc0", noNewPrivs: true },
      supervisorPid: 300,
      transport: { stdin: new PassThrough(), stdout, stderr: new PassThrough().end() },
      snapshot: async () => ({ event: "snapshot", id: 1, processes: [] }),
      observe: async () => ({ event: "observe", id: 1, state: "unavailable", reason: "unreadable" }),
      waitChild: async () => ({ event: "child_exit", code: 0 }),
      dispose: async () => {
        if (index === 2) { childReaping.resolve(); await releaseChild.promise; }
        return { clean: true, event: { event: "dispose", id: 1, state: "drained", authority: "ECHILD+__WALL" } };
      },
      failed: undefined, whenFailed: new Promise<Error>(() => {}),
    } satisfies CodexRootHandle;
  });
  const safety = createCodexExecutionSafety(registry,
    async () => { throw new Error("generic spawn forbidden"); },
    async () => { reconciles++; return { kind: "ready" }; },
    () => { terminalHooks++; }, adapter,
    (request, ms, fire) => {
      const timer = { request, ms, fire, cancelled: false }; timers.push(timer);
      return () => { timer.cancelled = true; };
    });
  return { registry, git, safety, timers, childReaping, releaseChild,
    counts: () => ({ launches, reconciles, terminalHooks, poisonWrites }) };
}

describe("checkpointStop admission preserves credential-free capture custody", { concurrency: false }, () => {
  for (const queuedBeforeCapture of [false, true]) {
    it(queuedBeforeCapture
      ? "queued healthy checkpoint expiry preserves active capture revision and last-child custody"
      : "checkpoint during active capture refuses before timers and preserves callback/child custody", async () => {
      const r = rig();
      const firstEntered = latch(), releaseFirst = latch();
      const captureEntered = latch(), continueCapture = latch(), callbackReturning = latch();
      let first: Promise<unknown> = Promise.resolve();
      let queued: Promise<void> | undefined;
      let captureFinished = false, disposalFinished = false;
      const pending: Promise<unknown>[] = [];
      if (queuedBeforeCapture) {
        first = r.safety.withBoundary({ boundary: "checkpoint", deadlineMs: budget }, async () => {
          firstEntered.resolve(); await releaseFirst.promise;
        });
        pending.push(first);
        await firstEntered.promise;
        assert.equal(r.registry.state(), "closed", "first boundary is healthy while holding its slot");
      }
      const capture = r.safety.withCredentialFreeCapture(budget, context =>
        r.git.withCredentialFreeCapture(context, async () => {
          captureEntered.resolve();
          await continueCapture.promise;
          assert.deepEqual(await r.git.worktreeStatus("/capture/runner"), [" M checkpoint-sentinel.txt"],
            "rejected checkpoint must leave the live capture launch authority usable");
          // Deliberately leave the final child's full join to the safety owner.
          const last = r.git.worktreeStatus("/capture/runner");
          pending.push(last);
          await r.childReaping.promise;
          callbackReturning.resolve();
          return "captured";
        })).then(result => { captureFinished = true; return result; });
      pending.push(capture);
      if (queuedBeforeCapture) {
        const request = checkpoint();
        queued = assert.rejects(r.safety.withBoundary(request, async () => assert.fail("rejected action ran")),
          (error: unknown) => {
            assert.ok(error instanceof CodexBoundaryError);
            assert.equal(error.stage, "reconcile");
            assert.equal(error.boundary, "checkpoint");
            assert.equal(error.sink, "done_checkpoint");
            assert.equal(error.step, "checkpoint_lock_wait");
            return true;
          });
        pending.push(queued);
        assert.ok(r.timers.some(timer => timer.request === request), "checkpoint queued before poisoning");
        releaseFirst.resolve();
        await first;
      }
      try {
        await captureEntered.promise;
        assert.equal(r.registry.state(), "poisoned");
        const counts = r.counts(), errors = [...r.registry.poisonErrors()];
        assert.equal(r.safety.completedCaptureForTeardown(), undefined, "live callback has no completed proof");
        if (queuedBeforeCapture) {
          const queueTimer = r.timers.find(timer => timer.request.checkpointStop);
          assert.ok(queueTimer);
          assert.equal(queueTimer.cancelled, false);
          assert.ok(queueTimer.ms > 0 && queueTimer.ms <= budget, "original admission allowance only");
          queueTimer.fire(); // Both refusal writer and its catch run against the existing capture poison.
          await queued;
        } else {
          const timerCount = r.timers.length;
          await assert.rejects(r.safety.withBoundary(checkpoint(), async () => assert.fail("rejected action ran")),
            CodexBoundaryError);
          assert.equal(r.timers.length, timerCount, "entry refusal has no timer side effect");
        }
        assert.deepEqual(r.counts(), counts, "no credentials, launches, disposal or fresh poison on refusal");
        assert.deepEqual(r.registry.poisonErrors(), errors);
        const disposal = r.safety.dispose({ boundary: "terminal", deadlineMs: budget })
          .then(result => { disposalFinished = true; return result; });
        pending.push(disposal);
        await setImmediate();
        assert.equal(disposalFinished, false, "rejected queue slot cannot bypass the held callback");
        assert.equal(r.counts().terminalHooks, 0);
        continueCapture.resolve();
        await callbackReturning.promise;
        await setImmediate();
        assert.equal(captureFinished, false, "callback return is insufficient before final child join");
        assert.equal(disposalFinished, false, "disposal must retain custody through the last child");
        assert.equal(r.counts().terminalHooks, 0);
        assert.equal(r.counts().launches, 2, "capture remains usable after checkpoint refusal");
        r.releaseChild.resolve();
        const result = await capture;
        assert.equal(result.kind, "observed_empty");
        if (result.kind !== "observed_empty") assert.fail("capture did not complete");
        assert.equal(result.value, "captured");
        assert.equal((await disposal).kind, "disposed");
        assert.equal(r.safety.acceptsCompletedCapture(result.proof), true);
        assert.equal(r.registry.acceptsCompletedCapture(result.proof), true);
        assert.equal(r.safety.completedCaptureForTeardown(), result.proof, "retain exact completion object");
        assert.equal(r.safety.acceptsCompletedCapture({} as typeof result.proof), false, "lookalikes are no evidence");
        assert.equal(r.counts().poisonWrites, counts.poisonWrites, "capture revision survives both queue error writers");
      } finally {
        releaseFirst.resolve(); continueCapture.resolve(); r.releaseChild.resolve();
        await Promise.allSettled(pending);
      }
    });
  }

  it("checkpoint after completed capture and after disposal preserves exact evidence without credentials or timers", async () => {
    const r = rig();
    const result = await r.safety.withCredentialFreeCapture(budget, context =>
      r.git.withCredentialFreeCapture(context, () => r.git.worktreeStatus("/capture/runner")));
    assert.equal(result.kind, "observed_empty");
    if (result.kind !== "observed_empty") assert.fail("capture did not complete");
    assert.deepEqual(result.value, [" M checkpoint-sentinel.txt"]);
    for (const state of ["poisoned", "disposed"] as const) {
      if (state === "disposed") assert.equal((await r.safety.dispose({ boundary: "terminal", deadlineMs: budget })).kind, "disposed");
      assert.equal(r.registry.state(), state);
      const counts = r.counts(), timerCount = r.timers.length;
      const errors = [...r.registry.poisonErrors()];
      await assert.rejects(r.safety.withBoundary(checkpoint(), async () => assert.fail("refused action ran")), CodexBoundaryError);
      assert.deepEqual(r.counts(), counts);
      assert.equal(r.timers.length, timerCount);
      assert.deepEqual(r.registry.poisonErrors(), errors);
      assert.equal(r.safety.acceptsCompletedCapture(result.proof), true);
      assert.equal(r.safety.completedCaptureForTeardown(), result.proof);
    }
  });

  it("healthy checkpoint queue expiry still poisons and cannot issue a clean certificate", async () => {
    const r = rig();
    const entered = latch(), release = latch();
    const first = r.safety.withBoundary({ boundary: "checkpoint", deadlineMs: budget }, async () => {
      entered.resolve(); await release.promise;
    });
    const checkedFirst = assert.rejects(first, CodexBoundaryError);
    await entered.promise;
    const request = checkpoint();
    const queued = assert.rejects(r.safety.withBoundary(request, async () => assert.fail("expired action ran")),
      (error: unknown) => {
        assert.ok(error instanceof CodexBoundaryError, "hard expiry is not a clean soft-stop certificate");
        assert.equal(error.stage, "reconcile");
        assert.equal(error.errors[0]?.category, "timeout");
        assert.equal(error.actionError instanceof Error && error.actionError.message, "codex source admission deadline expired");
        return true;
      });
    try {
      assert.equal(r.registry.isPoisoned(), false);
      const timer = r.timers.find(timer => timer.request === request);
      assert.ok(timer);
      timer.fire();
      await queued;
      assert.equal(r.registry.isPoisoned(), true);
      assert.ok(r.counts().poisonWrites > 0, "healthy registry must still fail closed");
      assert.equal(r.counts().reconciles, 1, "only the predecessor reconciles");
      assert.equal(r.counts().launches, 0);
      assert.equal(r.safety.completedCaptureForTeardown(), undefined);
    } finally {
      release.resolve();
      await Promise.allSettled([checkedFirst, queued]);
    }
  });
});
