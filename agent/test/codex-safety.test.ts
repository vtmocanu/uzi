import { describe, it, type TestContext } from "node:test";
import assert from "node:assert/strict";
import { PassThrough } from "node:stream";
import * as codexLauncher from "../src/codex/launcher.js";
import { WorkerClient, CodexRequestFailure, RequestError } from "../src/client.js";
import { GitCache, CheckpointSoftDeadlineError } from "../src/git.js";
import { createRunLaneInstallerReconcile, buildRunLaneReconcile } from "../src/codex/codex-executor.js";
import { nullLogger } from "./helpers.js";
import { selectCodexBinding } from "../src/codex/select.js";

import {
  CodexBoundaryError,
  CodexExecutionSafetyImpl,
  createCodexExecutionSafety,
  type BoundaryActionIdentity,
  type BoundaryActionOutcome,
  type BoundarySeams,
  type ReconcileBeforeBoundary,
  type SpawnRootSeam,
} from "../src/codex/safety.js";
import {
  ExecutionRegistry,
  newLocalExecutionEpoch,
  type ReapOutcome,
  type RegisteredRoot,
  type RootKind,
} from "../src/codex/registry.js";
import { TrustedExecutionRefusal } from "../src/trusted-execution-refusal.js";
import { CaptureOperationRevokedError, GitBoundaryAbortError, CheckpointUnsafeCleanupError } from "../src/checkpoint-stop.js";
import { captureFailureOf, type BoundaryPermit, type BoundaryRequest, type BoundaryStep } from "../src/harness.js";

for (const origin of ["deadline", "external", "both"] as const) {
  it(`R2 capture provenance distinguishes ${origin} and retains primary/cause`, async () => {
    const registry = new ExecutionRegistry(newLocalExecutionEpoch(2852));
    let expire!: () => void;
    const external = new AbortController();
    const primary = new Error("capture primary sentinel", { cause: new Error("original cause") });
    const safety = createCodexExecutionSafety(registry, spawnCounter().seam, undefined, undefined,
      async () => assert.fail("no child requested"),
      (_request, _ms, fire) => { expire = fire; return () => {}; });
    await assert.rejects(safety.withCredentialFreeCapture(1000, async context => {
      if (origin !== "external") expire();
      if (origin !== "deadline") external.abort(primary);
      assert.equal(context.signal.aborted, true);
      if (origin === "external") assert.equal(context.signal.reason, primary);
      throw primary;
    }, external.signal), (error: unknown) => {
      assert.ok(error instanceof CodexBoundaryError);
      assert.equal(error.actionError, primary);
      assert.equal(error.cause, primary);
      assert.deepEqual(captureFailureOf(error), {
        deadlineExpired: origin !== "external", childCleanupFailed: false, externallyAborted: origin !== "deadline",
      });
      assert.equal(Object.isFrozen(captureFailureOf(error)), true);
      assert.equal(captureFailureOf(JSON.parse(JSON.stringify(error))), undefined, "JSON cannot mint capture provenance");
      assert.equal(error.errors[0]?.category, origin === "deadline" ? "timeout" : "authorization");
      assert.equal(registry.state(), "poisoned");
      assert.equal(safety.completedCaptureForTeardown(), undefined);
      assert.equal(Object.keys(error).includes("deadlineExpired"), false, "provenance is not an error wire field");
      return true;
    });
    const arbitrary = new CodexBoundaryError("action", [{ category: "timeout", message: "codex capture deadline expired" }], primary);
    assert.equal(captureFailureOf(arbitrary), undefined, "matching text cannot mint capture provenance");
  });
}

for (const kind of ["git", "process"] as const) {
  for (const proof of ["own", "wrong", "live"] as const) {
    it(`R2 ${kind} revocation requires the exact already-aborted capture signal: ${proof}`, async () => {
      const registry = new ExecutionRegistry(newLocalExecutionEpoch(2852));
      let expire!: () => void;
      const safety = createCodexExecutionSafety(registry, spawnCounter().seam, undefined, undefined,
        async () => assert.fail("no child requested"),
        (_request, _ms, fire) => { expire = fire; return () => {}; });
      let primary: Error;
      await assert.rejects(safety.withCredentialFreeCapture(1000, async context => {
        const other = new AbortController(); other.abort();
        if (proof !== "live") expire();
        const signal = proof === "wrong" ? other.signal : context.signal;
        primary = kind === "git" ? new GitBoundaryAbortError("original message", undefined, signal)
          : new CaptureOperationRevokedError("original message", signal);
        if (proof === "live") expire();
        assert.equal(primary.message, "original message");
        assert.equal(primary.name, kind === "git" ? "AbortError" : "TrustedExecutionRefusal");
        assert.equal(JSON.stringify(primary), JSON.stringify(kind === "git"
          ? new GitBoundaryAbortError("original message") : new TrustedExecutionRefusal("original message")));
        assert.equal(Object.hasOwn(primary, "captureSignal"), false);
        assert.equal(JSON.stringify(primary).includes("captureSignal"), false);
        throw primary;
      }), (error: unknown) => {
        assert.ok(error instanceof CodexBoundaryError);
        assert.equal(error.actionError, primary);
        assert.equal(captureFailureOf(error)?.actionRevoked, proof === "own" ? true : undefined);
        return true;
      });
    });
  }
}

for (const origin of ["deadline", "external", "both"] as const) {
  it("R2 raw capture abort through Git factory has exact provenance: " + origin, async t => {
    const registry = new ExecutionRegistry(newLocalExecutionEpoch(2852));
    const cache = new GitCache("/unused-r2", nullLogger());
    let expire!: () => void;
    const external = new AbortController();
    const safety = createCodexExecutionSafety(registry, spawnCounter().seam, undefined, undefined,
      async () => assert.fail("no child requested"),
      (_request, _ms, fire) => { expire = fire; return () => {}; });
    await assert.rejects(safety.withCredentialFreeCapture(1000, async context =>
      cache.withCredentialFreeCapture(context, async () => {
        t.mock.method(cache as any, "runGitAsRunner", async () => {
          if (origin !== "deadline") external.abort(new Error("external authority"));
          if (origin !== "external") expire();
          throw context.signal.reason;
        });
        return cache.branchTip("/unused", "agent/issue-2852");
      }), external.signal), (error: unknown) => {
      assert.ok(error instanceof CodexBoundaryError);
      assert.ok(error.actionError instanceof GitBoundaryAbortError);
      assert.equal(captureFailureOf(error)?.actionRevoked, true);
      assert.equal(captureFailureOf(error)?.externallyAborted, origin !== "deadline");
      assert.equal(captureFailureOf(error)?.deadlineExpired, origin !== "external");
      return true;
    });
  });
}

for (const failure of ["clean", "dispose", "reap"] as const) {
  it("R2 in-flight capture child revocation waits for verified cleanup: " + failure, async () => {
    const registry = new ExecutionRegistry(newLocalExecutionEpoch(2852));
    const cache = new GitCache("/unused-r2", nullLogger());
    const waiting = defer<void>(), disposed = defer<void>(), reap = defer<ReapOutcome>();
    let expire!: () => void;
    let settled = false, reaps = 0, disposals = 0;
    const safety = createCodexExecutionSafety(registry, spawnCounter().seam, undefined, undefined,
      async () => ({
        stdin: null, stdout: new PassThrough(), stderr: new PassThrough(),
        waitChild: async () => { waiting.resolve(); return new Promise(() => {}); },
        root: { kind: "boundary_action",
          dispose: async () => { disposals++; disposed.resolve(); if (failure === "dispose") throw new Error("dispose failed"); },
          reap: async () => { reaps++; return reap.promise; },
        },
      }), (_request, _ms, fire) => { expire = fire; return () => {}; });
    const result = safety.withCredentialFreeCapture(1000, async context =>
      cache.withCredentialFreeCapture(context, async () => {
        try { return await cache.branchTip("/unused", "agent/issue-2852"); }
        finally { settled = true; }
      }));
    const checked = assert.rejects(result, (error: unknown) => {
      assert.ok(error instanceof CodexBoundaryError);
      assert.ok(error.actionError instanceof (failure === "clean" ? CaptureOperationRevokedError : CheckpointUnsafeCleanupError));
      assert.equal(captureFailureOf(error)?.actionRevoked, failure === "clean" ? true : undefined);
      return true;
    });
    await waiting.promise;
    expire();
    await disposed.promise;
    assert.equal(settled, false, "revocation cannot escape before owned reap");
    reap.resolve(failure === "reap" ? { ok: false, error: { category: "tool", message: "reap failed" } } : { ok: true });
    await checked;
    assert.equal(disposals, 1);
    assert.equal(reaps, 1);
  });
}

for (const timing of ["reap", "microtask", "foreign-signal", "child-deadline"] as const) {
  it("R2 independent child primary survives later capture expiry: " + timing, async () => {
    const registry = new ExecutionRegistry(newLocalExecutionEpoch(2852));
    const cache = new GitCache("/unused-r2", nullLogger());
    let expire!: () => void;
    let now = Date.now();
    const foreign = new AbortController(); foreign.abort();
    const primary = timing === "foreign-signal"
      ? new CaptureOperationRevokedError("independent child verification failure", foreign.signal)
      : new TrustedExecutionRefusal("independent child verification failure");
    let sawPrimary = false, reaps = 0, disposals = 0;
    const safety = createCodexExecutionSafety(registry, spawnCounter().seam, undefined, undefined,
      async () => {
        if (timing === "child-deadline") now += 1000;
        return {
          stdin: null, stdout: new PassThrough(), stderr: new PassThrough(),
          waitChild: () => {
            sawPrimary = true;
            // The helper's rejection handler selects the independent primary first.
            // Expiry then runs before startOwnedProcess resumes its await catch.
            const terminal = Promise.reject(primary);
            if (timing === "microtask") queueMicrotask(() => queueMicrotask(expire));
            return terminal;
          },
          root: { kind: "boundary_action",
            dispose: async () => { disposals++; },
            reap: async () => {
              reaps++;
              assert.equal(sawPrimary, timing !== "child-deadline");
              if (timing !== "microtask") expire();
              return { ok: true };
            },
          },
        };
      }, (_request, _ms, fire) => { expire = fire; return () => {}; }, () => now);
    await assert.rejects(safety.withCredentialFreeCapture(1000, async context =>
      cache.withCredentialFreeCapture(context, () => cache.branchTip("/unused", "agent/issue-2852"))),
      (error: unknown) => {
        assert.ok(error instanceof CodexBoundaryError);
        if (timing === "child-deadline") {
          assert.ok(error.actionError instanceof TrustedExecutionRefusal);
          assert.equal(error.actionError.message, "boundary process child deadline exceeded during launch");
        } else assert.equal(error.actionError, primary);
        assert.equal(captureFailureOf(error)?.deadlineExpired, true);
        assert.equal(captureFailureOf(error)?.actionRevoked, undefined,
          "independent terminal failures cannot authorize refusal recovery");
        return true;
      });
    assert.equal(reaps, 1);
    assert.equal(disposals, 1);
  });
}

// PRD #1171 (M3 m1, first unit) — the CodexExecutionSafety facade and the trusted
// boundary-action lane. All process work is injected; nothing real is spawned.

const req = (boundary: BoundaryRequest["boundary"]): BoundaryRequest => ({
  boundary,
  deadlineMs: 1000,
});

const tick = (): Promise<void> => new Promise((resolve) => setTimeout(resolve, 5));

function defer<T>(): { promise: Promise<T>; resolve: (v: T) => void } {
  let resolve!: (v: T) => void;
  const promise = new Promise<T>((res) => {
    resolve = res;
  });
  return { promise, resolve };
}

class FakeRoot implements RegisteredRoot {
  reapCalls = 0;
  constructor(
    readonly kind: RootKind,
    private readonly reapImpl: (d: number) => Promise<ReapOutcome> = async () => ({ ok: true }),
  ) {}
  async reap(d: number): Promise<ReapOutcome> {
    this.reapCalls += 1;
    return this.reapImpl(d);
  }
  async dispose(_d: number): Promise<void> {}
}

const failReap: (d: number) => Promise<ReapOutcome> = async () => ({
  ok: false,
  error: { category: "timeout", message: "reap timed out" },
});

function registerRoot(reg: ExecutionRegistry, root: RegisteredRoot): void {
  const reserved = reg.reserveLaunch(root.kind);
  assert.equal(reserved.kind, "reserved");
  if (reserved.kind !== "reserved") return;
  reg.registerRoot(reserved.reservation, root);
}

/** A fake low-level spawn seam with a call counter; its produced boundary-action
 *  root reaps per `reapImpl` (defaults to observed-empty). */
function spawnCounter(
  reapImpl: (d: number) => Promise<ReapOutcome> = async () => ({ ok: true }),
): { state: { calls: number; lastIdentity: BoundaryActionIdentity | undefined }; seam: SpawnRootSeam } {
  const state = { calls: 0, lastIdentity: undefined as BoundaryActionIdentity | undefined };
  const seam: SpawnRootSeam = async (_argv, identity, _deadline) => {
    state.calls += 1;
    state.lastIdentity = identity;
    const root: RegisteredRoot = {
      kind: "boundary_action",
      reap: reapImpl,
      dispose: async () => {},
    };
    return root;
  };
  return { state, seam };
}

describe("CodexExecutionSafety.withBoundary: gate ordering", () => {
  // Issue #1516: the clock is mocked (Date only) and the wall-clock deadline timer is
  // disarmed, so stage time is advanced explicitly and full-suite load cannot redden it.
  const clockedSafety = (
    t: TestContext,
    epoch: number,
    quiesce: () => void,
  ): { safety: CodexExecutionSafetyImpl; seen: number[] } => {
    t.mock.timers.enable({ apis: ["Date"], now: 0 });
    const seen: number[] = [];
    const safety = new CodexExecutionSafetyImpl(new ExecutionRegistry(newLocalExecutionEpoch(epoch)), {
      quiesce: async (request) => {
        seen.push(request.deadlineMs);
        quiesce();
        return { kind: "quiescent", epoch };
      },
      reap: async (request) => {
        seen.push(request.deadlineMs);
        return { kind: "observed_empty", evidence: "supervisor_echild", epoch };
      },
      dispose: async () => ({ kind: "disposed" }),
      spawnRoot: spawnCounter().seam,
      armDeadline: () => () => {},
    });
    return { safety, seen };
  };

  it("uses one absolute deadline and passes a shrinking remaining budget across stages", async (t) => {
    const { safety, seen } = clockedSafety(t, 12, () => t.mock.timers.tick(20));
    await safety.withBoundary({ boundary: "finalize", deadlineMs: 100 }, async () => undefined);
    assert.deepEqual(seen, [100, 80], "reap gets what is left of the one absolute deadline after quiesce");
  });

  it("a stage that runs past the absolute deadline is refused before reap and the action", async (t) => {
    const { safety, seen } = clockedSafety(t, 12, () => t.mock.timers.tick(101));
    let actionCalls = 0;
    await assert.rejects(
      safety.withBoundary({ boundary: "finalize", deadlineMs: 100 }, async () => { actionCalls += 1; }),
      (error: unknown) =>
        error instanceof CodexBoundaryError &&
        error.stage === "quiesce" &&
        error.errors[0]?.message === "codex boundary deadline expired after quiesce",
    );
    assert.deepEqual(seen, [100], "reap never ran");
    assert.equal(actionCalls, 0, "the action never ran");
  });

  it("propagates the same deadline AbortSignal into reconciliation", async (t) => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(13));
    let reconcileSignal: AbortSignal | undefined;
    let entered!: () => void;
    const reconciling = new Promise<void>((resolve) => { entered = resolve; });
    const safety = createCodexExecutionSafety(
      reg,
      spawnCounter().seam,
      async (_request, signal) => {
        reconcileSignal = signal;
        entered();
        await new Promise<void>((resolve) => signal.addEventListener("abort", () => resolve(), { once: true }));
        return { kind: "blocked", errors: [{ category: "timeout", message: "reconcile cancelled at boundary deadline" }] };
      },
    );
    t.mock.timers.enable({ apis: ["Date", "setTimeout"], now: 0 });
    const rejected = assert.rejects(
      safety.withBoundary({ boundary: "shutdown", deadlineMs: 25 }, async () => undefined),
      (error: unknown) => error instanceof CodexBoundaryError && error.stage === "reconcile",
    );
    await reconciling;
    t.mock.timers.tick(24);
    assert.equal(reconcileSignal?.aborted, false, "the boundary still has one millisecond left");
    t.mock.timers.tick(1);
    await rejected;
    assert.equal(reconcileSignal?.aborted, true);
  });

  it("a clean quiesce that settles after expiry still blocks permit mint and action", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(14));
    let actionCalls = 0;
    const safety = new CodexExecutionSafetyImpl(reg, {
      quiesce: async () => {
        await new Promise<void>((resolve) => setTimeout(resolve, 30));
        return { kind: "quiescent", epoch: 14 };
      },
      reap: async () => ({ kind: "observed_empty", evidence: "supervisor_echild", epoch: 14 }),
      dispose: async () => ({ kind: "disposed" }),
      spawnRoot: spawnCounter().seam,
    });
    await assert.rejects(
      safety.withBoundary({ boundary: "shutdown", deadlineMs: 15 }, async () => { actionCalls += 1; }),
      (error: unknown) => error instanceof CodexBoundaryError && error.stage === "quiesce",
    );
    assert.equal(actionCalls, 0, "a late clean result cannot mint a permit after the deadline");
  });

  it("quiesces, reaps, then mints a matching-epoch permit and runs the action", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(11));
    const safety = createCodexExecutionSafety(reg, spawnCounter().seam);
    let seenEpoch: number | undefined;
    let seenBoundary: string | undefined;
    const result = await safety.withBoundary(req("finalize"), async (permit) => {
      seenEpoch = permit.epoch;
      seenBoundary = permit.boundary;
      return 123;
    });
    assert.equal(result, 123);
    assert.equal(seenEpoch, 11);
    assert.equal(seenBoundary, "finalize");
  });

  it("leaves the sink UNCALLED and poisons when quiesce is not quiescent", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(1));
    reg.reserveLaunch("provider"); // unsettled -> quiesce is incomplete
    const { state: spawnState, seam } = spawnCounter();
    const safety = createCodexExecutionSafety(reg, seam);
    let called = 0;
    await assert.rejects(
      safety.withBoundary(req("shutdown"), async () => {
        called += 1;
        return 1;
      }),
      (e: unknown) => e instanceof CodexBoundaryError && e.stage === "quiesce",
    );
    assert.equal(called, 0);
    assert.equal(reg.state(), "poisoned");
    assert.equal(spawnState.calls, 0);
  });

  it("leaves the sink UNCALLED and poisons when reap is not observed-empty", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(2));
    registerRoot(reg, new FakeRoot("provider", failReap));
    const safety = createCodexExecutionSafety(reg, spawnCounter().seam);
    let called = 0;
    await assert.rejects(
      safety.withBoundary(req("terminal"), async () => {
        called += 1;
        return 1;
      }),
      (e: unknown) => e instanceof CodexBoundaryError && e.stage === "reap",
    );
    assert.equal(called, 0);
    assert.equal(reg.state(), "poisoned");
  });

  it("holds the permit through the FULL async action (a slow sink body)", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(1));
    const safety = createCodexExecutionSafety(reg, spawnCounter().seam);
    const gate = defer<void>();
    let settled = false;
    const p = safety
      .withBoundary(req("checkpoint"), async () => {
        await gate.promise;
        return "slow-done";
      })
      .then((v) => {
        settled = true;
        return v;
      });
    await tick();
    assert.equal(settled, false); // still inside the action, permit held
    gate.resolve();
    assert.equal(await p, "slow-done");
    assert.equal(settled, true);
  });
});

describe("CodexExecutionSafety.withBoundary: serialized overlapping boundaries", () => {
  it("charges queue wait to the same deadline and never runs an action whose budget expired", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(20));
    const safety = createCodexExecutionSafety(reg, spawnCounter().seam);
    const releaseFirst = defer<void>();
    const first = safety.withBoundary({ boundary: "checkpoint", deadlineMs: 100 }, async () => {
      await releaseFirst.promise;
    });
    await tick();
    let secondRan = false;
    const second = safety.withBoundary({ boundary: "shutdown", deadlineMs: 10 }, async () => {
      secondRan = true;
    });
    await new Promise<void>((resolve) => setTimeout(resolve, 20));
    releaseFirst.resolve();
    await first;
    await assert.rejects(
      second,
      (error: unknown) => error instanceof CodexBoundaryError && error.stage === "quiesce",
    );
    assert.equal(secondRan, false);
  });

  it("does not begin a second boundary until the first fully releases", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(1));
    const events: string[] = [];
    const seams: BoundarySeams = {
      quiesce: async (r) => {
        events.push(`quiesce-${r.boundary}`);
        return { kind: "quiescent", epoch: 1 };
      },
      reap: async (_r, epoch) => ({ kind: "observed_empty", evidence: "supervisor_echild", epoch }),
      dispose: async () => ({ kind: "disposed" }),
      spawnRoot: async () => {
        throw new Error("unused in this test");
      },
    };
    const safety = new CodexExecutionSafetyImpl(reg, seams);
    const gate = defer<void>();

    const p1 = safety.withBoundary(req("checkpoint"), async () => {
      events.push("action1-start");
      await gate.promise;
      events.push("action1-end");
    });
    const p2 = safety.withBoundary(req("park"), async () => {
      events.push("action2");
    });

    await tick();
    // The second boundary must not even quiesce until the first releases.
    assert.deepEqual(events, ["quiesce-checkpoint", "action1-start"]);
    gate.resolve();
    await Promise.all([p1, p2]);
    assert.deepEqual(events, [
      "quiesce-checkpoint",
      "action1-start",
      "action1-end",
      "quiesce-park",
      "action2",
    ]);
  });
});

describe("CodexExecutionSafety.spawnBoundaryAction: boundary-action lane", () => {
  it("holds the permit until a late/backgrounded child settles", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(1));
    const gate = defer<ReapOutcome>();
    const { state: spawnState, seam } = spawnCounter(async () => gate.promise);
    const safety = createCodexExecutionSafety(reg, seam);

    let settled = false;
    const p = safety
      .withBoundary(req("finalize"), async (permit) => {
        // Fire-and-FORGET: the action body does not await the boundary action, so
        // only the facade's own child tracking keeps the permit held.
        void safety.spawnBoundaryAction(permit, ["late-child"], "command");
        return "body-returned";
      })
      .then((v) => {
        settled = true;
        return v;
      });

    await tick();
    assert.equal(spawnState.calls, 1); // the child was spawned
    assert.equal(settled, false); // ...but the boundary is NOT released yet
    gate.resolve({ ok: true }); // the late child finally reaps ECHILD
    assert.equal(await p, "body-returned");
    assert.equal(settled, true);
    assert.notEqual(reg.state(), "poisoned");
  });

  it("drains an action admitted while an earlier boundary-action batch is settling", async (t) => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(1));
    const first = defer<ReapOutcome>();
    const second = defer<ReapOutcome>();
    const firstBatchEntered = defer<void>();
    const secondBatchEntered = defer<void>();
    let captured: BoundaryPermit | undefined;
    let actionBatches = 0;
    let secondBatchSettled = false;
    const originalAllSettled = Promise.allSettled;
    t.mock.method(Promise, "allSettled", function (
      this: PromiseConstructor,
      ...args: Parameters<typeof Promise.allSettled>
    ) {
      // Forward before signalling: allSettled has already snapshotted the batch.
      const settlements = originalAllSettled.apply(this, args);
      // Ignore any pre-action registry work; the permit identifies the drain lane.
      if (!captured) return settlements;
      actionBatches += 1;
      if (actionBatches === 1) firstBatchEntered.resolve();
      if (actionBatches === 2) {
        secondBatchEntered.resolve();
        return settlements.then((outcomes) => {
          secondBatchSettled = true;
          return outcomes;
        });
      }
      return settlements;
    });
    let spawnCalls = 0;
    const seam: SpawnRootSeam = async () => {
      spawnCalls += 1;
      const reap = spawnCalls === 1 ? first.promise : second.promise;
      return new FakeRoot("boundary_action", async () => reap);
    };
    const safety = createCodexExecutionSafety(reg, seam);
    let boundarySettled = false;
    const boundary = safety
      .withBoundary(req("finalize"), async (permit) => {
        captured = permit;
        void safety.spawnBoundaryAction(permit, ["first"], "command");
      })
      .then(() => {
        assert.equal(secondBatchSettled, true, "the boundary awaits the second batch settlement");
        boundarySettled = true;
      });
    const boundaryCompletion = boundary.then(
      () => "boundary" as const,
      () => "boundary" as const,
    );
    t.after(async () => {
      first.resolve({ ok: true });
      second.resolve({ ok: true });
      // Observe rejection during failure cleanup without replacing the primary assertion.
      await boundary.catch(() => {});
    });

    await firstBatchEntered.promise;
    assert.ok(captured, "the boundary callback captured its held permit");
    void safety.spawnBoundaryAction(captured, ["admitted-during-drain"], "command");
    assert.equal(spawnCalls, 2, "the second action was admitted while the first batch drained");
    first.resolve({ ok: true });
    assert.equal(
      await Promise.race([
        secondBatchEntered.promise.then(() => "second-batch" as const),
        boundaryCompletion,
      ]),
      "second-batch",
      "the drain enters the second batch before boundary completion",
    );
    assert.equal(boundarySettled, false, "the second batch still holds the boundary permit");
    second.resolve({ ok: true });
    await boundary;
    assert.equal(boundarySettled, true);
  });

  it("a timed-out boundary-action child poisons the epoch (never 'clean')", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(1));
    const { seam } = spawnCounter(failReap);
    const safety = createCodexExecutionSafety(reg, seam);
    let bodyCompleted = false;
    await assert.rejects(
      safety.withBoundary(req("credentialed_git"), async (permit) => {
        const outcome = await safety.spawnBoundaryAction(permit, ["git", "push"], "command");
        assert.equal(outcome.kind, "poisoned");
        bodyCompleted = true;
        return "ignored";
      }),
      (e: unknown) => e instanceof CodexBoundaryError && e.stage === "action",
    );
    assert.equal(bodyCompleted, true); // the action body ran to completion...
    assert.equal(reg.state(), "poisoned"); // ...but the boundary is poisoned, not clean
  });

  it("poisons when a fire-and-forget boundary root rejects during reap", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(1));
    const seam: SpawnRootSeam = async () => new FakeRoot(
      "boundary_action",
      async () => {
        throw new Error("reap seam rejected");
      },
    );
    const safety = createCodexExecutionSafety(reg, seam);

    await assert.rejects(
      safety.withBoundary(req("finalize"), async (permit) => {
        void safety.spawnBoundaryAction(permit, ["background"], "command");
        return "body-finished";
      }),
      (error: unknown) => error instanceof CodexBoundaryError && error.stage === "action",
    );
    assert.equal(reg.state(), "poisoned");
    assert.ok(
      reg.poisonErrors().some((error) => /root reap rejected/.test(error.message)),
      "the rejected reap leaves bounded poison evidence",
    );
  });

  it("disposes the spawned root and returns poisoned when registration fails (finding 6)", async () => {
    // The injected spawn seam returns a root whose kind is NOT "boundary_action", so
    // the registry's registerRoot rejects it (kind mismatch) and poisons. The facade
    // must NOT go on to reap an unadmitted root: it must tear the just-spawned root
    // down and surface the poison.
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(7));
    let disposeCalls = 0;
    let reapCalls = 0;
    const seam: SpawnRootSeam = async () => ({
      kind: "command", // mismatch: the reservation is boundary_action
      reap: async () => {
        reapCalls += 1;
        return { ok: true };
      },
      dispose: async () => {
        disposeCalls += 1;
      },
    });
    const safety = createCodexExecutionSafety(reg, seam);
    let outcome: BoundaryActionOutcome | undefined;
    await assert.rejects(
      safety.withBoundary(req("finalize"), async (permit) => {
        outcome = await safety.spawnBoundaryAction(permit, ["x"], "command");
        return "body";
      }),
      (e: unknown) => e instanceof CodexBoundaryError && e.stage === "action",
    );
    assert.equal(outcome?.kind, "poisoned");
    if (outcome?.kind === "poisoned") assert.equal(outcome.error.category, "protocol");
    assert.equal(disposeCalls, 1); // the unadmitted root was torn down...
    assert.equal(reapCalls, 0); // ...and never reaped
    assert.equal(reg.state(), "poisoned");
  });

  it("refuses a stale permit once its boundary has ended", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(9));
    const { state: spawnState, seam } = spawnCounter();
    const safety = createCodexExecutionSafety(reg, seam);
    let captured: BoundaryPermit | undefined;
    await safety.withBoundary(req("checkpoint"), async (permit) => {
      captured = permit;
    });
    if (!captured) throw new Error("permit was not captured");
    const outcome = await safety.spawnBoundaryAction(captured, ["x"], "command");
    assert.equal(outcome.kind, "refused");
    if (outcome.kind === "refused") assert.equal(outcome.reason, "stale_permit");
    assert.equal(spawnState.calls, 0);
  });

  it("refuses a permit captured from an earlier boundary inside a later boundary", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(9));
    const { state: spawnState, seam } = spawnCounter();
    const safety = createCodexExecutionSafety(reg, seam);
    let captured: BoundaryPermit | undefined;
    await safety.withBoundary(req("checkpoint"), async (permit) => {
      captured = permit;
    });
    if (!captured) throw new Error("permit was not captured");

    let outcome: BoundaryActionOutcome | undefined;
    await safety.withBoundary(req("credentialed_git"), async () => {
      outcome = await safety.spawnBoundaryAction(captured!, ["git", "push"], "worker_pat");
    });
    assert.equal(outcome?.kind, "refused");
    if (outcome?.kind === "refused") assert.equal(outcome.reason, "stale_permit");
    assert.equal(spawnState.calls, 0);
  });
});

describe("CodexExecutionSafety.spawnBoundaryProcess: permit-owned subprocesses", () => {
  /** Access the new typed launcher error at test time so the pre-fix module still loads and
   *  the regression reports a named assertion instead of a module-import failure. */
  function childExitTimeout(): Error {
    const ctor = (codexLauncher as unknown as {
      SupervisedChildExitTimeoutError?: new () => Error;
    }).SupervisedChildExitTimeoutError;
    assert.ok(ctor, "the launcher exports a typed child-exit timeout");
    return new ctor();
  }

  it("cancel waits for delayed reap, is idempotent, and leaves its permit usable", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(1913));
    const reaped = defer<ReapOutcome>();
    let disposals = 0;
    let reaps = 0;
    let launches = 0;
    const safety = createCodexExecutionSafety(reg, spawnCounter().seam, undefined, undefined, async () => {
      launches += 1;
      return {
        root: {
          kind: "boundary_action",
          dispose: async () => { disposals += 1; },
          reap: async () => { reaps += 1; return reaped.promise; },
        },
        stdin: null, stdout: null, stderr: null,
        waitChild: async () => launches === 1 ? new Promise(() => {}) : { code: 0 },
      };
    });
    await safety.withBoundary(req("finalize"), async (permit) => {
      const request = { argv: ["/bin/true"], cwd: "/tmp", env: {}, identity: "worker_pat" as const };
      const child = await safety.spawnBoundaryProcess(permit, request);
      const first = child.cancel();
      assert.strictEqual(child.cancel(), first);
      let done = false;
      void first.then(() => { done = true; });
      await tick();
      assert.equal(done, false);
      assert.equal(disposals, 1);
      assert.equal(reaps, 1);
      reaped.resolve({ ok: true });
      await first;
      assert.deepEqual(await child.completed, { code: -1 });
      await child.cancel();
      assert.equal(permit.signal.aborted, false);
      const sibling = await safety.spawnBoundaryProcess(permit, request);
      assert.deepEqual(await sibling.completed, { code: 0 });
      await sibling.cancel();
      assert.equal(disposals, 1);
    });
    assert.equal(reg.isPoisoned(), false);
  });

  it("a permit abort during launch still disposes and reaps the returned root once", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(1910));
    const launched = defer<void>();
    const entered = defer<void>();
    let fire!: () => void;
    let disposals = 0;
    let reaps = 0;
    const safety = createCodexExecutionSafety(reg, spawnCounter().seam, undefined, undefined, async () => {
      entered.resolve();
      await launched.promise;
      return {
        root: {
          kind: "boundary_action",
          dispose: async () => { disposals += 1; },
          reap: async () => { reaps += 1; return { ok: true }; },
        },
        stdin: null, stdout: null, stderr: null,
        waitChild: async () => { throw new Error("an aborted launch must not wait for its child"); },
      };
    }, (_request, _ms, trigger) => { fire = trigger; return () => {}; });
    const boundary = safety.withBoundary(req("finalize"), async (permit) => {
      const child = await safety.spawnBoundaryProcess(permit, {
        argv: ["/bin/true"], cwd: "/tmp", env: {}, identity: "worker_pat",
      });
      await assert.rejects(child.cancel(), /deadline exceeded/);
    });
    const rejected = assert.rejects(boundary, CodexBoundaryError);
    await entered.promise;
    fire();
    launched.resolve();
    await rejected;
    assert.equal(disposals, 1);
    assert.equal(reaps, 1);
    assert.equal(reg.isPoisoned(), true);
  });

  it("cancel during natural-exit reap shares the completion owner", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(1911));
    const reaped = defer<ReapOutcome>();
    let reaps = 0;
    let disposals = 0;
    const safety = createCodexExecutionSafety(reg, spawnCounter().seam, undefined, undefined, async () => ({
      root: {
        kind: "boundary_action",
        dispose: async () => { disposals += 1; },
        reap: async () => { reaps += 1; return reaped.promise; },
      },
      stdin: null, stdout: null, stderr: null,
      waitChild: async () => ({ code: 0 }),
    }));
    await safety.withBoundary(req("finalize"), async (permit) => {
      const child = await safety.spawnBoundaryProcess(permit, {
        argv: ["/bin/true"], cwd: "/tmp", env: {}, identity: "worker_pat",
      });
      await tick();
      assert.equal(reaps, 1);
      const cancelled = child.cancel();
      await tick();
      assert.equal(disposals, 1);
      reaped.resolve({ ok: true });
      await cancelled;
      assert.deepEqual(await child.completed, { code: -1 });
      assert.equal(reaps, 1);
      assert.equal(disposals, 1);
    });
  });

  for (const failure of ["dispose", "reap"] as const) {
    it(`cancel fails closed on ${failure} failure`, async () => {
      const reg = new ExecutionRegistry(newLocalExecutionEpoch(1912));
      let reaps = 0;
      const safety = createCodexExecutionSafety(reg, spawnCounter().seam, undefined, undefined, async () => ({
        root: {
          kind: "boundary_action",
          dispose: async () => { if (failure === "dispose") throw new Error("disposal failed"); },
          reap: async () => {
            reaps += 1;
            return failure === "reap" ? failReap(1_000) : { ok: true };
          },
        },
        stdin: null, stdout: null, stderr: null,
        waitChild: async () => new Promise(() => {}),
      }));
      await assert.rejects(safety.withBoundary(req("finalize"), async (permit) => {
        const child = await safety.spawnBoundaryProcess(permit, {
          argv: ["/bin/true"], cwd: "/tmp", env: {}, identity: "worker_pat",
        });
        await assert.rejects(child.cancel(), /disposal failed|reap cleanly/);
        await assert.rejects(child.completed, /disposal failed|reap cleanly/);
        assert.equal(reaps, 1);
      }), CodexBoundaryError);
      assert.equal(reg.isPoisoned(), true);
    });
  }

  it("a checkpoint opt-in returns a soft timeout only after disposal and full root reap", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(1914));
    const events: string[] = [];
    const stdout = new PassThrough();
    const stderr = new PassThrough();
    const safety = createCodexExecutionSafety(reg, spawnCounter().seam, undefined, undefined, async () => ({
      root: {
        kind: "boundary_action",
        dispose: async () => { events.push("dispose"); },
        reap: async () => {
          events.push("reap");
          return events.includes("dispose")
            ? { ok: true as const }
            : { ok: false as const, error: { category: "timeout" as const, message: "root was not disposed" } };
        },
      },
      stdin: new PassThrough(), stdout, stderr,
      waitChild: async () => { throw childExitTimeout(); },
    }));

    const result = await safety.withBoundary({ boundary: "checkpoint", deadlineMs: 5_000 }, async (permit) => {
      const child = await safety.spawnBoundaryProcess(permit, {
        argv: ["/usr/bin/git", "fetch"], cwd: "/tmp", env: {}, identity: "worker_pat",
        timeoutMs: 1_000, recoverableTimeout: true,
      } as Parameters<typeof safety.spawnBoundaryProcess>[1]);
      const completed = await child.completed;
      assert.equal(permit.signal.aborted, false, "the outer boundary deadline did not fire");
      return completed;
    });
    assert.deepEqual(result, { code: -1, softTimedOut: true });
    assert.deepEqual(events, ["dispose", "reap"], "a clean full-root reap precedes the recoverable result");
    assert.equal(reg.isPoisoned(), false, "a settled best-effort child leaves the boundary usable");
    assert.equal(stdout.destroyed, true, "the abandoned output collector is closed");
    assert.equal(stderr.destroyed, true, "the abandoned error collector is closed");
  });

  it("a child returned after its soft launch budget is disposed and reaped before recovery", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(1918));
    const events: string[] = [];
    const safety = createCodexExecutionSafety(reg, spawnCounter().seam, undefined, undefined, async () => {
      await new Promise<void>((resolve) => setTimeout(resolve, 25));
      return {
        root: {
          kind: "boundary_action",
          dispose: async () => { events.push("dispose"); },
          reap: async () => {
            events.push("reap");
            return events.includes("dispose")
              ? { ok: true as const }
              : { ok: false as const, error: { category: "timeout" as const, message: "root not disposed" } };
          },
        },
        stdin: new PassThrough(), stdout: new PassThrough(), stderr: new PassThrough(),
        waitChild: async () => { events.push("wait_child"); return { code: 0 }; },
      };
    });

    const completed = await safety.withBoundary({ boundary: "checkpoint", deadlineMs: 1_000 }, async (permit) => {
      const child = await safety.spawnBoundaryProcess(permit, {
        argv: ["/usr/bin/git", "fetch"], cwd: "/tmp", env: {}, identity: "worker_pat",
        timeoutMs: 5, recoverableTimeout: true,
      });
      return child.completed;
    });
    assert.deepEqual(completed, { code: -1, softTimedOut: true });
    assert.deepEqual(events, ["dispose", "reap"], "late launch never enters an unbudgeted child wait");
    assert.equal(reg.isPoisoned(), false);
  });

  it("a checkpoint soft timeout remains fatal when the owned root cannot be reaped", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(1915));
    const events: string[] = [];
    const safety = createCodexExecutionSafety(reg, spawnCounter().seam, undefined, undefined, async () => ({
      root: {
        kind: "boundary_action",
        dispose: async () => { events.push("dispose"); },
        reap: async () => {
          events.push("reap_failed");
          return { ok: false, error: { category: "timeout", message: "owned child still live" } };
        },
      },
      stdin: new PassThrough(), stdout: new PassThrough(), stderr: new PassThrough(),
      waitChild: async () => { throw childExitTimeout(); },
    }));

    await assert.rejects(safety.withBoundary({ boundary: "checkpoint", deadlineMs: 5_000 }, async (permit) => {
      const child = await safety.spawnBoundaryProcess(permit, {
        argv: ["/usr/bin/git", "fetch"], cwd: "/tmp", env: {}, identity: "worker_pat",
        timeoutMs: 1_000, recoverableTimeout: true,
      } as Parameters<typeof safety.spawnBoundaryProcess>[1]);
      await child.completed;
    }), CodexBoundaryError);
    assert.deepEqual(events, ["dispose", "reap_failed"]);
    assert.equal(reg.isPoisoned(), true, "an unverified child never becomes a soft skip");
  });

  it("the same typed child timeout is fatal for finalize without an opt-in", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(1916));
    const safety = createCodexExecutionSafety(reg, spawnCounter().seam, undefined, undefined, async () => ({
      root: new FakeRoot("boundary_action"),
      stdin: new PassThrough(), stdout: new PassThrough(), stderr: new PassThrough(),
      waitChild: async () => { throw childExitTimeout(); },
    }));
    await assert.rejects(safety.withBoundary({ boundary: "finalize", deadlineMs: 1_000 }, async (permit) => {
      const child = await safety.spawnBoundaryProcess(permit, {
        argv: ["/usr/bin/git", "fetch"], cwd: "/tmp", env: {}, identity: "worker_pat", timeoutMs: 20,
      });
      await child.completed;
    }), CodexBoundaryError);
    assert.equal(reg.isPoisoned(), true, "ordinary boundary child timeouts still poison");
  });

  it("rejects a recoverable timeout request outside a checkpoint permit before spawning", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(1917));
    let spawns = 0;
    const safety = createCodexExecutionSafety(reg, spawnCounter().seam, undefined, undefined, async () => {
      spawns += 1;
      throw new Error("must not spawn");
    });
    await assert.rejects(safety.withBoundary({ boundary: "finalize", deadlineMs: 1_000 }, async (permit) => {
      await safety.spawnBoundaryProcess(permit, {
        argv: ["/usr/bin/git", "fetch"], cwd: "/tmp", env: {}, identity: "worker_pat",
        recoverableTimeout: true,
      } as Parameters<typeof safety.spawnBoundaryProcess>[1]);
    }));
    assert.equal(spawns, 0, "the invalid opt-in is refused before launch");
  });

  it("caps a boundary child to its requested deadline", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(19));
    let launchBudget = -1;
    let waitBudget = -1;
    const safety = createCodexExecutionSafety(
      reg,
      spawnCounter().seam,
      undefined,
      undefined,
      async (_request, deadlineMs) => {
        launchBudget = deadlineMs;
        await new Promise<void>((resolve) => setTimeout(resolve, 50));
        return {
          root: new FakeRoot("boundary_action"),
          stdin: new PassThrough(), stdout: new PassThrough(), stderr: new PassThrough(),
          waitChild: async (deadline) => { waitBudget = deadline; return { code: 0 }; },
        };
      },
    );
    await safety.withBoundary({ boundary: "finalize", deadlineMs: 1_000 }, async (permit) => {
      const child = await safety.spawnBoundaryProcess(permit, {
        argv: ["/usr/bin/git", "status"], cwd: "/tmp", env: {}, identity: "worker_pat", timeoutMs: 250,
      });
      await child.completed;
    });
    assert.ok(launchBudget > 0 && launchBudget <= 250);
    assert.ok(waitBudget > 0 && waitBudget <= launchBudget - 30,
      "launch time must consume the same child deadline used for waiting");
  });

  it("reserves before spawn, registers, and holds the permit until the whole root reaps", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(9));
    const exited = defer<{ code: number }>();
    let reservationSeen = false;
    let reaps = 0;
    const safety = createCodexExecutionSafety(
      reg,
      spawnCounter().seam,
      undefined,
      undefined,
      async () => {
        reservationSeen = reg.pendingLaunchCount() === 1;
        return {
          root: new FakeRoot("boundary_action", async () => { reaps += 1; return { ok: true }; }),
          stdin: new PassThrough(), stdout: new PassThrough(), stderr: new PassThrough(),
          waitChild: async () => exited.promise,
        };
      },
    );
    let boundarySettled = false;
    const p = safety.withBoundary(req("finalize"), async (permit) => {
      const process = await safety.spawnBoundaryProcess(permit, {
        argv: ["/usr/bin/git", "status"], cwd: "/tmp", env: {}, identity: "worker_pat",
      });
      void process.completed;
    }).then(() => { boundarySettled = true; });
    await tick();
    assert.equal(reservationSeen, true, "the launch reservation exists before the spawn seam runs");
    assert.equal(boundarySettled, false, "a fire-and-forget child still holds the permit");
    exited.resolve({ code: 0 });
    await p;
    assert.equal(reaps, 1, "the registered boundary root reaped before permit release");
  });

  it("deadline abort still awaits root reap and poisons instead of abandoning the action", async (t) => {
    t.mock.timers.enable({ apis: ["Date", "setTimeout"], now: 0 });
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(10));
    let reaped = false;
    let reapBudget = -1;
    let entered!: () => void;
    const registered = new Promise<void>((resolve) => { entered = resolve; });
    const safety = createCodexExecutionSafety(
      reg,
      spawnCounter().seam,
      undefined,
      undefined,
      async () => ({
        root: new FakeRoot("boundary_action", async (deadlineMs) => {
          reaped = true;
          reapBudget = deadlineMs;
          return { ok: true };
        }),
        stdin: new PassThrough(), stdout: new PassThrough(), stderr: new PassThrough(),
        waitChild: async () => new Promise<{ code: number }>(() => undefined),
      }),
    );
    const rejected = assert.rejects(
      safety.withBoundary({ boundary: "shutdown", deadlineMs: 20 }, async (permit) => {
        const process = await safety.spawnBoundaryProcess(permit, {
          argv: ["/bin/sleep", "forever"], cwd: "/tmp", env: {}, identity: "worker_pat",
        });
        entered(); // spawnBoundaryProcess has registered the permit-owned root.
        await process.completed;
      }),
      CodexBoundaryError,
    );
    await registered;
    t.mock.timers.tick(20);
    await rejected;
    assert.equal(reaped, true, "deadline cancellation reaped before withBoundary rejected");
    assert.ok(reapBudget <= 2, `action-root reap received only the remaining budget (${reapBudget}ms)`);
    assert.equal(reg.isPoisoned(), true, "a timed-out permit-held process poisons publication");
  });
});

describe("CodexExecutionSafety [R3-2]: PAT-bearing ordering guard", () => {
  it("refuses a worker_pat action while any command root is still live", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(3));
    registerRoot(reg, new FakeRoot("command")); // registered, never reaped
    await reg.quiesceChildren(1000); // -> closed; the command root stays unreaped
    assert.equal(reg.hasLiveCommandRoot(), true);

    const { state: spawnState, seam } = spawnCounter();
    // A reap seam that CLAIMS observed-empty without actually reaping the registry's
    // roots — the guard must not trust it, it re-checks the registry directly.
    const seams: BoundarySeams = {
      quiesce: (r) => reg.quiesceChildren(r.deadlineMs),
      reap: async (_r, epoch) => ({ kind: "observed_empty", evidence: "supervisor_echild", epoch }),
      dispose: (r) => reg.disposeTools(r.deadlineMs),
      spawnRoot: seam,
    };
    const safety = new CodexExecutionSafetyImpl(reg, seams);

    let refusal: BoundaryActionOutcome | undefined;
    await safety.withBoundary(req("credentialed_git"), async (permit) => {
      refusal = await safety.spawnBoundaryAction(permit, ["git", "push"], "worker_pat");
    });
    assert.equal(refusal?.kind, "refused");
    if (refusal?.kind === "refused") assert.equal(refusal.reason, "command_roots_live");
    assert.equal(spawnState.calls, 0); // the spawn seam was NEVER invoked
  });

  it("allows a worker_pat action once every command root has reaped", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(4));
    registerRoot(reg, new FakeRoot("command")); // will be reaped by the real reap seam
    const { state: spawnState, seam } = spawnCounter();
    const safety = createCodexExecutionSafety(reg, seam); // real reap marks the command root
    let outcome: BoundaryActionOutcome | undefined;
    await safety.withBoundary(req("credentialed_git"), async (permit) => {
      outcome = await safety.spawnBoundaryAction(permit, ["git", "push"], "worker_pat");
    });
    assert.equal(outcome?.kind, "settled");
    assert.equal(spawnState.calls, 1);
    assert.equal(spawnState.lastIdentity, "worker_pat");
    assert.equal(reg.hasLiveCommandRoot(), false);
  });
});

describe("CodexExecutionSafety.withBoundary: per-sink reconcile (m4)", () => {
  it("reconcile blocked → throws CodexBoundaryError('reconcile') BEFORE quiesce/reap/mint/action, poisons, and blocks later publication", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(5));
    const events: string[] = [];
    const seams: BoundarySeams = {
      quiesce: async () => {
        events.push("quiesce");
        return { kind: "quiescent", epoch: 5 };
      },
      reap: async (_r, epoch) => {
        events.push("reap");
        return { kind: "observed_empty", evidence: "supervisor_echild", epoch };
      },
      dispose: async () => ({ kind: "disposed" }),
      spawnRoot: async () => {
        throw new Error("unused");
      },
    };
    const reconcile: ReconcileBeforeBoundary = async () => ({
      kind: "blocked",
      errors: [{ category: "authorization", message: "refresh contended" }],
    });
    const safety = new CodexExecutionSafetyImpl(reg, seams, reconcile);
    let actionRan = 0;
    await assert.rejects(
      safety.withBoundary(req("finalize"), async () => {
        actionRan += 1;
      }),
      (e: unknown) => e instanceof CodexBoundaryError && e.stage === "reconcile",
    );
    assert.equal(actionRan, 0, "the trusted action never ran");
    assert.deepEqual(events, [], "neither quiesce nor reap ran (blocked before the reap)");
    assert.equal(reg.state(), "poisoned");
    assert.equal(reg.isPoisoned(), true);

    // Later publication is blocked: even a now-READY reconcile fails at quiesce on the poisoned
    // registry (the real registry seams surface the poison).
    const safety2 = createCodexExecutionSafety(reg, spawnCounter().seam, async () => ({ kind: "ready" }));
    await assert.rejects(
      safety2.withBoundary(req("finalize"), async () => {
        actionRan += 1;
      }),
      (e: unknown) => e instanceof CodexBoundaryError && e.stage === "quiesce",
    );
    assert.equal(actionRan, 0, "the trusted action still never ran — publication stays blocked");
  });

  it("reconcile ready → proceeds through quiesce/reap/mint and runs the action", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(6));
    const safety = createCodexExecutionSafety(reg, spawnCounter().seam, async () => ({ kind: "ready" }));
    let ran = false;
    const result = await safety.withBoundary(req("checkpoint"), async (permit) => {
      ran = true;
      return permit.epoch;
    });
    assert.equal(ran, true);
    assert.equal(result, 6);
  });

  it("#2852 terminal hook refusal remains incomplete after a later successful hook", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(3));
    let calls = 0;
    const safety = createCodexExecutionSafety(reg, spawnCounter().seam, undefined, () => {
      if (++calls === 1) throw new Error("private cleanup detail");
    });
    const first = await safety.dispose(req("terminal"));
    assert.equal(first.kind, "incomplete");
    assert.equal(JSON.stringify(first).includes("private cleanup detail"), false);
    const second = await safety.dispose(req("terminal"));
    assert.equal(second.kind, "incomplete", "cleared tables cannot erase hook failure");
    assert.equal(calls, 2, "independent cleanup is still attempted");
    assert.equal(reg.isPoisoned(), true);
  });

  for (const hookFails of [false, true]) {
    it(`#2852 registry disposal preserves primary failure with hook failure ${hookFails}`, async () => {
      const reg = new ExecutionRegistry(newLocalExecutionEpoch(3));
      const primary = new Error("primary disposal failure");
      const secondary = new Error("secondary cleanup failure");
      reg.disposeTools = async () => { throw primary; };
      let hooks = 0;
      const safety = createCodexExecutionSafety(reg, spawnCounter().seam, undefined, () => {
        hooks++;
        if (hookFails) throw secondary;
      });
      await assert.rejects(safety.dispose(req("terminal")), error => {
        if (hookFails) {
          assert.ok(error instanceof AggregateError);
          assert.equal(error.cause, primary);
          assert.deepEqual(error.errors, [primary, secondary]);
          assert.equal(error.message.includes(primary.message), false);
          assert.equal(error.message.includes(secondary.message), false);
        } else assert.equal(error, primary);
        return true;
      });
      assert.equal(hooks, 1);
    });
  }

  it("dispose runs the onDispose terminal hook AFTER a post-run sink registered a token (F1: sink tokens are evicted)", async () => {
    // Models the executor's F1 wiring: a post-run sink reconcile registers a fresh token
    // into a set (via addSecret), and the terminal dispose the runner calls evicts it via
    // onDispose. FAIL-OLD/PASS-FIXED: before the onDispose hook, dispose did NOT evict, so
    // every Codex sink leaked a token registration into the worker-lifetime redactor.
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(3));
    const registered = new Set<string>();
    const removed: string[] = [];
    const reconcile: ReconcileBeforeBoundary = async () => {
      registered.add("sink-token"); // stands in for log.addSecret + releasedTokens.add
      return { kind: "ready" };
    };
    const onDispose = (): void => {
      for (const t of registered) removed.push(t);
      registered.clear();
    };
    const safety = createCodexExecutionSafety(reg, spawnCounter().seam, reconcile, onDispose);
    // A post-run sink: withBoundary runs the reconcile (registers the token), no eviction yet.
    await safety.withBoundary(req("finalize"), async () => {});
    assert.deepEqual([...registered], ["sink-token"], "the sink reconcile registered a token");
    assert.deepEqual(removed, [], "not evicted until the terminal dispose");
    // The terminal dispose (the runner, after the last sink) evicts it via onDispose.
    await safety.dispose(req("terminal"));
    assert.deepEqual(removed, ["sink-token"], "the terminal dispose evicted the post-run sink token");
    assert.equal(registered.size, 0, "cleared so a second dispose cannot double-remove");
    await safety.dispose(req("terminal"));
    assert.deepEqual(removed, ["sink-token"], "a second dispose is idempotent (no re-remove)");
  });
});

describe("CodexExecutionSafety.withBoundary: primary-failure evidence preservation", () => {
  it("preserves the action's OWN thrown error alongside the poison evidence", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(1));
    const safety = createCodexExecutionSafety(reg, spawnCounter().seam);
    const actionError = new Error("action body blew up");
    await assert.rejects(
      // The action poisons the epoch (a cleanup fault) AND then throws its own error;
      // both the primary failure and the cleanup evidence must survive, not just poison.
      safety.withBoundary(req("checkpoint"), async () => {
        reg.poison({ category: "unknown", message: "cleanup evidence" });
        throw actionError;
      }),
      (e: unknown) => {
        assert.ok(e instanceof CodexBoundaryError);
        assert.equal(e.stage, "action");
        // The action's own error is retrievable, not swallowed by the poison evidence.
        assert.equal(e.actionError, actionError);
        assert.equal(e.cause, actionError);
        // ...and the cleanup/poison evidence is still carried alongside it.
        assert.ok(e.errors.some((x) => x.message === "cleanup evidence"));
        return true;
      },
    );
    assert.equal(reg.state(), "poisoned");
  });

  it("carries no actionError when the action did not itself throw (poison-only)", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(1));
    const { seam } = spawnCounter(failReap); // the boundary child reap times out -> poison
    const safety = createCodexExecutionSafety(reg, seam);
    await assert.rejects(
      safety.withBoundary(req("credentialed_git"), async (permit) => {
        await safety.spawnBoundaryAction(permit, ["git", "push"], "command");
        return "body-ok"; // the body returns normally; only cleanup failed
      }),
      (e: unknown) => {
        assert.ok(e instanceof CodexBoundaryError);
        assert.equal(e.stage, "action");
        assert.equal(e.actionError, undefined);
        assert.equal(e.cause, undefined);
        return true;
      },
    );
  });
});

describe("CodexExecutionSafety.withBoundary: boundary deadline trigger (issue #1513)", () => {
  // Timer-free quiesce/reap so the only timer withBoundary arms is the boundary deadline.
  const timerFreeSeams = (epoch: number): BoundarySeams => ({
    quiesce: async () => ({ kind: "quiescent", epoch }),
    reap: async () => ({ kind: "observed_empty", evidence: "supervisor_echild", epoch }),
    dispose: async () => ({ kind: "disposed" }),
    spawnRoot: spawnCounter().seam,
  });
  const awaitAbort = (signal: AbortSignal): Promise<void> =>
    signal.aborted
      ? Promise.resolve()
      : new Promise<void>((resolve) => signal.addEventListener("abort", () => resolve(), { once: true }));

  it("with no armDeadline seam, a short wall-clock deadline still aborts the permit signal", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(40));
    const safety = createCodexExecutionSafety(reg, spawnCounter().seam);
    let signal: AbortSignal | undefined;
    // Real default timer, but a budget wide enough that quiesce/reap/permit acquisition
    // cannot plausibly outrun it under a loaded event loop: the action is always reached
    // and then waits only on the deadline's abort, so the rejection stage is "action".
    await assert.rejects(
      safety.withBoundary({ boundary: "shutdown", deadlineMs: 250 }, async (permit) => {
        signal = permit.signal;
        await awaitAbort(permit.signal);
      }),
      (e: unknown) => e instanceof CodexBoundaryError && e.stage === "action",
    );
    assert.equal(signal?.aborted, true, "the default deadline aborted the permit signal");
  });

  it("the default deadline timer is unref'd and cleared at teardown", async () => {
    const safety = new CodexExecutionSafetyImpl(new ExecutionRegistry(newLocalExecutionEpoch(41)), timerFreeSeams(41));
    const realSetTimeout = globalThis.setTimeout;
    const realClearTimeout = globalThis.clearTimeout;
    const armed: { ms: number | undefined; handle: NodeJS.Timeout }[] = [];
    const cleared: unknown[] = [];
    try {
      globalThis.setTimeout = ((fn: () => void, ms?: number) => {
        const handle = realSetTimeout(fn, ms);
        armed.push({ ms, handle });
        return handle;
      }) as typeof setTimeout;
      globalThis.clearTimeout = ((handle?: NodeJS.Timeout) => {
        cleared.push(handle);
        realClearTimeout(handle);
      }) as typeof clearTimeout;
      const result = await safety.withBoundary({ boundary: "checkpoint", deadlineMs: 60_000 }, async () => "ok");
      assert.equal(result, "ok");
    } finally {
      globalThis.setTimeout = realSetTimeout;
      globalThis.clearTimeout = realClearTimeout;
    }
    // The spy is process-global, so under node's shared-process test mode it can also see
    // timers scheduled by concurrent work during the await window. Only the boundary's own
    // deadline is armed for the remaining ~60s budget, so filter on that window.
    const deadlines = armed.filter(({ ms }) => ms !== undefined && ms > 59_000 && ms <= 60_000);
    assert.equal(deadlines.length, 1, `exactly one deadline timer armed for the remaining budget: ${armed.map((a) => a.ms).join(",")}`);
    const [deadline] = deadlines;
    assert.equal(deadline?.handle.hasRef(), false, "the deadline timer never holds the process open");
    assert.ok(cleared.includes(deadline?.handle), "teardown cleared the deadline timer");
  });

  it("an injected armDeadline receives the request and budget; fire aborts the permit signal; cancel runs at teardown", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(42));
    const calls: { request: BoundaryRequest; ms: number }[] = [];
    let fire: (() => void) | undefined;
    let cancels = 0;
    const safety = createCodexExecutionSafety(reg, spawnCounter().seam, undefined, undefined, undefined, (request, ms, f) => {
      calls.push({ request, ms });
      fire = f;
      return () => {
        cancels += 1;
      };
    });
    let signal: AbortSignal | undefined;
    await assert.rejects(
      safety.withBoundary({ boundary: "finalize", deadlineMs: 60_000 }, async (permit) => {
        signal = permit.signal;
        assert.equal(permit.signal.aborted, false, "no wall-clock deadline fired on its own");
        assert.equal(cancels, 0, "not cancelled while the action runs");
        fire?.();
        await awaitAbort(permit.signal);
      }),
      (e: unknown) => e instanceof CodexBoundaryError && e.stage === "action",
    );
    assert.equal(calls.length, 1);
    assert.equal(calls[0]?.request.boundary, "finalize");
    assert.ok((calls[0]?.ms ?? 0) > 59_000 && (calls[0]?.ms ?? 0) <= 60_000, `budget forwarded: ${calls[0]?.ms}`);
    assert.equal(signal?.aborted, true, "fire aborted the permit signal");
    assert.equal(cancels, 1, "the boundary cancelled its deadline at teardown");
  });

  it("an injected armDeadline that never fires lets the boundary settle cleanly and is cancelled once", async () => {
    let cancels = 0;
    const safety = new CodexExecutionSafetyImpl(new ExecutionRegistry(newLocalExecutionEpoch(43)), {
      ...timerFreeSeams(43),
      armDeadline: () => () => {
        cancels += 1;
      },
    });
    assert.equal(await safety.withBoundary({ boundary: "checkpoint", deadlineMs: 60_000 }, async () => 7), 7);
    assert.equal(cancels, 1);
  });

  // Issue #1900: the deadline records the caller's active step when it fires, not later.
  const firedBoundaryError = async (
    epoch: number,
    activeStep: BoundaryRequest["activeStep"],
  ): Promise<CodexBoundaryError> => {
    let fire: (() => void) | undefined;
    const safety = new CodexExecutionSafetyImpl(new ExecutionRegistry(newLocalExecutionEpoch(epoch)), {
      ...timerFreeSeams(epoch),
      armDeadline: (_request, _ms, f) => {
        fire = f;
        return () => {};
      },
    });
    let caught: unknown;
    try {
      await safety.withBoundary(
        { boundary: "finalize", deadlineMs: 60_000, ...(activeStep !== undefined ? { activeStep } : {}) },
        async (permit) => {
          fire?.();
          await awaitAbort(permit.signal);
          // A second fire after the abort must not re-read the probe.
          fire?.();
        },
      );
    } catch (e) {
      caught = e;
    }
    assert.ok(caught instanceof CodexBoundaryError, `expected CodexBoundaryError, got ${String(caught)}`);
    return caught;
  };
  const NO_STEP_DIAGNOSTIC = "codex boundary failed at action (finalize): codex boundary action deadline exceeded";

  it("names the step active when the deadline fired, not the step the action moved on to", async () => {
    let current: BoundaryStep = "base_align";
    let fire: (() => void) | undefined;
    const safety = new CodexExecutionSafetyImpl(new ExecutionRegistry(newLocalExecutionEpoch(44)), {
      ...timerFreeSeams(44),
      armDeadline: (_request, _ms, f) => {
        fire = f;
        return () => {};
      },
    });
    let caught: unknown;
    try {
      await safety.withBoundary({ boundary: "finalize", deadlineMs: 60_000, activeStep: () => current }, async (permit) => {
        // Pins read-before-abort: a probe read after the abort would already see "push".
        permit.signal.addEventListener("abort", () => {
          current = "push";
        });
        fire?.();
        current = "push";
        await awaitAbort(permit.signal);
        fire?.(); // a later fire is ignored: only the first one counts
      });
    } catch (e) {
      caught = e;
    }
    assert.ok(caught instanceof CodexBoundaryError);
    assert.equal(caught.stage, "action");
    assert.equal(caught.step, "base_align");
    assert.equal(
      caught.diagnostic,
      "codex boundary failed at action (finalize) during base-align: codex boundary action deadline exceeded during base-align",
    );
    assert.ok(!caught.diagnostic.includes("during push"), caught.diagnostic);
    assert.deepEqual(
      caught.errors.map((e) => e.message),
      ["codex boundary action deadline exceeded during base-align"],
    );
  });

  it("names the fired step and the action error's name, never its message, when the action throws", async () => {
    let current: BoundaryStep = "base_align";
    let fire: (() => void) | undefined;
    const safety = new CodexExecutionSafetyImpl(new ExecutionRegistry(newLocalExecutionEpoch(50)), {
      ...timerFreeSeams(50),
      armDeadline: (_request, _ms, f) => {
        fire = f;
        return () => {};
      },
    });
    let caught: unknown;
    try {
      await safety.withBoundary({ boundary: "finalize", deadlineMs: 60_000, activeStep: () => current }, async () => {
        fire?.();
        current = "push";
        throw new Error("secret-ish detail");
      });
    } catch (e) {
      caught = e;
    }
    assert.ok(caught instanceof CodexBoundaryError);
    assert.equal(caught.step, "base_align");
    assert.ok(caught.diagnostic.includes("during base-align"), caught.diagnostic);
    assert.ok(caught.diagnostic.includes("action error: Error"), caught.diagnostic);
    assert.ok(!caught.diagnostic.includes("secret-ish"), caught.diagnostic);
    assert.ok(!caught.diagnostic.includes("during push"), caught.diagnostic);
  });

  it("with no probe the deadline diagnostic is unchanged", async () => {
    const e = await firedBoundaryError(45, undefined);
    assert.equal(e.step, undefined);
    assert.equal(e.diagnostic, NO_STEP_DIAGNOSTIC);
    assert.equal(e.message, "codex boundary failed at action");
    assert.deepEqual(
      e.errors.map((x) => x.message),
      ["codex boundary action deadline exceeded"],
    );
  });

  it("a probe that throws or returns an unknown step leaves the diagnostic unchanged", async () => {
    const baseline = await firedBoundaryError(46, undefined);
    const throwing = await firedBoundaryError(47, () => {
      throw new Error("probe exploded");
    });
    const unknown = await firedBoundaryError(48, () => "__proto__" as unknown as "push");
    const bogus = await firedBoundaryError(49, () => "not_a_step" as unknown as "push");
    for (const e of [throwing, unknown, bogus]) {
      assert.equal(e.step, undefined);
      assert.equal(e.diagnostic, baseline.diagnostic);
      assert.equal(e.diagnostic, NO_STEP_DIAGNOSTIC);
      assert.equal(e.message, baseline.message);
      assert.deepEqual(e.errors, baseline.errors);
    }
  });
});

// Issue #1766 (M3a): the vault-locked reconcile deferral and the credential-free capture settle.
describe("W8 account boundary safety", () => {
  it("carries account deferral through the poisoned boundary with secret-free diagnostic", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(9));
    const reconcile: ReconcileBeforeBoundary = async () => ({
      kind: "blocked", deferral: "account_unavailable",
      errors: [{ category: "authorization", message: "codex subscription boundary reconcile deferred: account unavailable" }],
    });
    const safety = createCodexExecutionSafety(reg, spawnCounter().seam, reconcile);
    await assert.rejects(safety.withBoundary(req("finalize"), async () => assert.fail("no action")), (error: unknown) => {
      assert.ok(error instanceof CodexBoundaryError);
      assert.equal(error.deferral, "account_unavailable");
      assert.equal(error.stage, "reconcile");
      assert.equal(error.diagnostic, "codex boundary failed at reconcile (finalize): codex subscription boundary reconcile deferred: account unavailable");
      return true;
    });
    assert.equal(reg.state(), "poisoned");
  });
});

describe("CodexExecutionSafety: vault_locked deferral (issue #1766)", () => {
  function inertSeams(events: string[]): BoundarySeams {
    return {
      quiesce: async () => {
        events.push("quiesce");
        return { kind: "quiescent", epoch: 9 };
      },
      reap: async (_r, epoch) => {
        events.push("reap");
        return { kind: "observed_empty", evidence: "supervisor_echild", epoch };
      },
      dispose: async () => ({ kind: "disposed" }),
      spawnRoot: async () => {
        throw new Error("unused");
      },
    };
  }

  it("a blocked reconcile carrying a deferral throws CodexBoundaryError('reconcile') with .deferral, poisons, and never runs the action", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(9));
    const events: string[] = [];
    const reconcile: ReconcileBeforeBoundary = async () => ({
      kind: "blocked",
      errors: [{ category: "authorization", message: "codex subscription boundary reconcile deferred: vault locked" }],
      deferral: "vault_locked",
    });
    const safety = new CodexExecutionSafetyImpl(reg, inertSeams(events), reconcile);
    let actionRan = 0;
    let caught: unknown;
    await safety
      .withBoundary(req("park"), async () => {
        actionRan += 1;
      })
      .catch((e: unknown) => {
        caught = e;
      });
    assert.ok(caught instanceof CodexBoundaryError);
    assert.equal(caught.stage, "reconcile");
    assert.equal(caught.deferral, "vault_locked");
    assert.equal(actionRan, 0);
    assert.deepEqual(events, []);
    assert.equal(reg.state(), "poisoned");
  });

  it("a generic blocked reconcile carries no deferral", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(9));
    const reconcile: ReconcileBeforeBoundary = async () => ({
      kind: "blocked",
      errors: [{ category: "authorization", message: "refresh contended" }],
    });
    const safety = new CodexExecutionSafetyImpl(reg, inertSeams([]), reconcile);
    let caught: unknown;
    await safety.withBoundary(req("park"), async () => undefined).catch((e: unknown) => {
      caught = e;
    });
    assert.ok(caught instanceof CodexBoundaryError);
    assert.equal(caught.deferral, undefined);
  });

  it("settleForCredentialFreeCapture waits behind an in-flight boundary, then poisons and drains without reconciling", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(4));
    let reconciles = 0;
    const safety = createCodexExecutionSafety(reg, spawnCounter().seam, async () => {
      reconciles += 1;
      return { kind: "ready" };
    });
    const gate = defer<void>();
    const order: string[] = [];
    const boundary = safety.withBoundary(req("checkpoint"), async () => {
      order.push("action-start");
      await gate.promise;
      order.push("action-end");
    });
    await tick();
    const settle = safety.settleForCredentialFreeCapture(2000).then((r) => {
      order.push("settled");
      return r;
    });
    await tick();
    assert.deepEqual(order, ["action-start"], "settle did not overtake the running boundary");
    assert.equal(reg.state(), "closed", "the registry is untouched while settle is queued");
    gate.resolve();
    await boundary;
    const result = await settle;
    assert.deepEqual(result, { kind: "observed_empty" });
    assert.deepEqual(order, ["action-start", "action-end", "settled"]);
    assert.equal(reconciles, 1, "only the boundary reconciled; settle never does");
    assert.equal(reg.state(), "poisoned");
    assert.equal(reg.reserveLaunch("provider").kind, "denied");
  });

  it("settleForCredentialFreeCapture returns incomplete when its deadline passes in the queue, and keeps the queue ordered", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(4));
    const safety = createCodexExecutionSafety(reg, spawnCounter().seam);
    const gate = defer<void>();
    const boundary = safety.withBoundary(req("checkpoint"), async () => {
      await gate.promise;
    });
    await tick();
    const result = await safety.settleForCredentialFreeCapture(20);
    assert.equal(result.kind, "incomplete");
    assert.equal(reg.state(), "closed", "a queued-out settle never touched the registry");
    let laterRan = false;
    const later = safety.withBoundary(req("checkpoint"), async () => {
      laterRan = true;
    });
    await tick();
    assert.equal(laterRan, false, "a later boundary still waits for the in-flight one");
    gate.resolve();
    await boundary;
    await later;
    assert.equal(laterRan, true);
  });
});

describe("CodexBoundaryError.diagnostic (issue #1864)", () => {
  const unsettled = { category: "protocol" as const, message: "quiesceChildren: 1 callback/child-turn reservation(s) unsettled" };

  it("names the stage, the milestone checkpoint and the unsettled work; message stays bare", () => {
    const err = new CodexBoundaryError("quiesce", [unsettled], undefined, undefined, {
      boundary: "checkpoint",
      sink: "milestone_checkpoint",
    });
    assert.equal(
      err.diagnostic,
      "codex boundary failed at quiesce (milestone checkpoint): quiesceChildren: 1 callback/child-turn reservation(s) unsettled",
    );
    assert.equal(err.message, "codex boundary failed at quiesce");
    assert.equal(err.boundary, "checkpoint");
    assert.equal(err.sink, "milestone_checkpoint");
  });

  it("labels the done checkpoint, falls back to the boundary name, and omits the label when neither is known", () => {
    const done = new CodexBoundaryError("reap", [{ category: "timeout", message: "reap timed out" }], undefined, undefined, {
      boundary: "checkpoint",
      sink: "done_checkpoint",
    });
    assert.equal(done.diagnostic, "codex boundary failed at reap (done checkpoint): reap timed out");
    const park = new CodexBoundaryError("reap", [{ category: "timeout", message: "reap timed out" }], undefined, undefined, {
      boundary: "park",
    });
    assert.equal(park.diagnostic, "codex boundary failed at reap (park): reap timed out");
    assert.equal(park.sink, undefined);
    const bare = new CodexBoundaryError("action", [{ category: "timeout", message: "late" }]);
    assert.equal(bare.diagnostic, "codex boundary failed at action: late");
    assert.equal(bare.boundary, undefined);
  });

  it("says 'no detail' when there are no errors and no action error", () => {
    assert.equal(new CodexBoundaryError("quiesce", []).diagnostic, "codex boundary failed at quiesce: no detail");
  });

  it("keeps at most three errors and counts the rest", () => {
    const errors = ["a", "b", "c", "d", "e"].map((m) => ({ category: "protocol" as const, message: m }));
    assert.equal(new CodexBoundaryError("reap", errors).diagnostic, "codex boundary failed at reap: a; b; c; +2 more");
  });

  it("replaces control characters (C0 incl. CR/LF/TAB, DEL, C1) with a space", () => {
    const err = new CodexBoundaryError("quiesce", [
      { category: "protocol", message: "line1\r\nline2\tx\u0007y\u007fz\u0085w\u009b" },
    ]);
    assert.equal(err.diagnostic, "codex boundary failed at quiesce: line1 line2 x y z w");
    for (let i = 0; i < err.diagnostic.length; i++) {
      const code = err.diagnostic.charCodeAt(i);
      assert.ok(code > 0x1f && !(code >= 0x7f && code <= 0x9f), `control char at ${i}`);
    }
  });

  it("folds zero-width and bidi formatting characters (U+202E, U+200B, U+2066, U+FEFF) to a space", () => {
    const rlo = String.fromCharCode(0x202e);
    const zwsp = String.fromCharCode(0x200b);
    const err = new CodexBoundaryError("quiesce", [
      { category: "protocol", message: `safe${rlo}txt.exe${zwsp}tail\u2066iso\u2069\ufeffend` },
    ]);
    assert.equal(err.diagnostic, "codex boundary failed at quiesce: safe txt.exe tail iso end");
    assert.ok(!err.diagnostic.includes(rlo), "no U+202E");
    assert.ok(!err.diagnostic.includes(zwsp), "no U+200B");
  });

  // One code point per Unicode category (Cc, Cf, Zl, Zp) and per range the old hand-listed
  // predicate missed; runner-codex-sinks.test.ts rejects the same list on the read side.
  for (const cp of [
    0x0007, 0x001b, 0x007f, 0x0085, 0x00ad, 0x061c, 0x180e, 0x200b, 0x200f, 0x2028,
    0x2029, 0x202a, 0x202e, 0x2060, 0x2064, 0x2066, 0x2069, 0x206a, 0x206f, 0xfeff,
  ]) {
    const hex = `U+${cp.toString(16).toUpperCase().padStart(4, "0")}`;
    it(`folds ${hex} to a space`, () => {
      const ch = String.fromCodePoint(cp);
      const err = new CodexBoundaryError("quiesce", [{ category: "protocol", message: `a${ch}b` }]);
      assert.equal(err.diagnostic, "codex boundary failed at quiesce: a b");
      assert.ok(!err.diagnostic.includes(ch), `no ${hex}`);
    });
  }

  it("caps each error at 160 characters and the whole diagnostic at 500", () => {
    const one = new CodexBoundaryError("reap", [{ category: "protocol", message: "x".repeat(400) }]);
    const detail = one.diagnostic.slice("codex boundary failed at reap: ".length);
    assert.equal(detail.length, 160);
    assert.ok(detail.endsWith("…"));
    const many = new CodexBoundaryError(
      "reap",
      Array.from({ length: 10 }, () => ({ category: "protocol" as const, message: "y".repeat(400) })),
      new TypeError("boom"),
      undefined,
      { boundary: "checkpoint", sink: "milestone_checkpoint" },
    );
    assert.equal(many.diagnostic.length, 500);
    assert.ok(many.diagnostic.endsWith("…"));
  });

  it("names only the action error's class, never its message or object", () => {
    // Secret-shaped fixture assembled at runtime, never a full token literal in source.
    const secret = "gh" + "p_" + "A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8";
    const typed = new CodexBoundaryError(
      "action",
      [{ category: "tool", message: "spawnBoundaryAction: child action failed before reap completed" }],
      new TypeError(`push failed with ${secret}`),
    );
    assert.equal(
      typed.diagnostic,
      "codex boundary failed at action: spawnBoundaryAction: child action failed before reap completed; action error: TypeError",
    );
    assert.ok(!typed.diagnostic.includes(secret));
    const weird = new Error(`x ${secret}`);
    weird.name = `Bad name ${secret}`;
    const odd = new CodexBoundaryError("action", [], weird);
    assert.equal(odd.diagnostic, "codex boundary failed at action: action error: Error");
    const plain = new CodexBoundaryError("action", [], { token: secret });
    assert.equal(plain.diagnostic, "codex boundary failed at action: action error: Error");
    assert.ok(!plain.diagnostic.includes(secret));
    assert.equal(typed.message, "codex boundary failed at action");
  });

  it("keeps a token-shaped error name out of harness messages and the diagnostic", async () => {
    // An identifier-shaped secret: a name-syntax check alone would let it through.
    const secret = "gh" + "p_" + "A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8";
    const named = new Error("x");
    named.name = secret;
    const direct = new CodexBoundaryError("action", [], named);
    assert.equal(direct.diagnostic, "codex boundary failed at action: action error: Error");

    const reg = new ExecutionRegistry(newLocalExecutionEpoch(1));
    const seam: SpawnRootSeam = async () => {
      const e = new Error("spawn");
      e.name = secret;
      throw e;
    };
    const safety = createCodexExecutionSafety(reg, seam);
    let outcome: BoundaryActionOutcome | undefined;
    const thrown = await safety
      .withBoundary(req("finalize"), async (permit) => {
        outcome = await safety.spawnBoundaryAction(permit, ["git", "push"], "command");
      })
      .then(
        () => undefined,
        (e: unknown) => e,
      );
    assert.equal(outcome?.kind, "poisoned");
    assert.equal(
      outcome?.kind === "poisoned" ? outcome.error.message : undefined,
      "spawnBoundaryAction: spawn failed (Error)",
    );
    assert.ok(thrown instanceof CodexBoundaryError);
    assert.ok(!thrown.diagnostic.includes(secret));
    assert.ok(thrown.diagnostic.includes("spawnBoundaryAction: spawn failed (Error)"));
  });

  it("a quiesce failure through withBoundary carries the request's boundary, sink and diagnostic", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(3));
    const safety = new CodexExecutionSafetyImpl(reg, {
      quiesce: async () => ({ kind: "incomplete", errors: [unsettled] }),
      reap: async () => ({ kind: "observed_empty", evidence: "supervisor_echild", epoch: 3 }),
      dispose: async () => ({ kind: "disposed" }),
      spawnRoot: spawnCounter().seam,
    });
    let called = 0;
    await assert.rejects(
      safety.withBoundary({ boundary: "checkpoint", deadlineMs: 1000, sink: "milestone_checkpoint" }, async () => {
        called += 1;
      }),
      (e: unknown) => {
        assert.ok(e instanceof CodexBoundaryError);
        assert.equal(e.stage, "quiesce");
        assert.equal(e.boundary, "checkpoint");
        assert.equal(e.sink, "milestone_checkpoint");
        assert.equal(
          e.diagnostic,
          "codex boundary failed at quiesce (milestone checkpoint): quiesceChildren: 1 callback/child-turn reservation(s) unsettled",
        );
        return true;
      },
    );
    assert.equal(called, 0);
  });
});

describe("M2 first-send deadline safety propagation", () => {
  it("keeps refresh_unknown on the poisoned reconcile error and never admits the action", async (t) => {
    let fire!: () => void;
    let sends = 0;
    const events: string[] = [];
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(9));
    t.mock.method(globalThis, "fetch", async (_url: unknown, init: RequestInit) => {
      sends++;
      const waiting = new Promise<Response>((_resolve, reject) => {
        init.signal!.addEventListener("abort", () => reject(new Error("private-canary")), { once: true });
      });
      fire();
      return waiting;
    });
    const client = new WorkerClient("http://fixture.invalid", "fixture-token", "test", nullLogger());
    const selected = selectCodexBinding({ codex: {
      auth_mode: "subscription", access_token: "fixture-token", capability: "fixture-cap", generation: 3,
      chatgpt_account_id: "fixture-account", chatgpt_plan_type: null,
    } });
    assert.equal(selected.kind, "codex");
    if (selected.kind !== "codex") assert.fail("fixture binding");
    const reconcile = buildRunLaneReconcile("run-1", client, selected.binding, () => assert.fail("no token"));
    const safety = new CodexExecutionSafetyImpl(reg, {
      armDeadline: (_request, _ms, callback) => { fire = callback; return () => {}; },
      quiesce: async () => { events.push("quiesce"); return { kind: "quiescent", epoch: 9 }; },
      reap: async () => { events.push("reap"); return { kind: "observed_empty", evidence: "supervisor_echild", epoch: 9 }; },
      dispose: async () => ({ kind: "disposed" }),
      spawnRoot: spawnCounter().seam,
    }, reconcile);
    await assert.rejects(safety.withBoundary(req("finalize"), async () => { events.push("action"); }), (err: unknown) => {
      assert.ok(err instanceof CodexBoundaryError);
      assert.equal(err.stage, "reconcile");
      assert.equal(err.deferral, "refresh_unknown");
      assert.doesNotMatch(err.message, /private-canary|fixture.invalid|fixture-cap/);
      return true;
    });
    assert.equal(sends, 1);
    assert.deepEqual(events, []);
    assert.equal(reg.state(), "poisoned");
  });
});

describe("A normal boundaries refuse unusable registries before credentials", () => {
  for (const state of ["poisoned", "disposed"] as const) {
    it(`refuses ${state} before reconcile or action`, async () => {
      const registry = new ExecutionRegistry(newLocalExecutionEpoch(1));
      let reconciles = 0, actions = 0, quiesces = 0, reaps = 0;
      const quiesce = registry.quiesceChildren.bind(registry);
      const reap = registry.reapProcesses.bind(registry);
      registry.quiesceChildren = (...args) => { quiesces++; return quiesce(...args); };
      registry.reapProcesses = (...args) => { reaps++; return reap(...args); };
      const spawn = spawnCounter();
      const safety = createCodexExecutionSafety(registry, spawn.seam, async () => {
        reconciles++;
        return { kind: "ready" };
      });
      if (state === "poisoned") registry.poison({ category: "protocol", message: "prior proof refused" });
      else assert.equal((await safety.dispose(req("terminal"))).kind, "disposed");
      const priorErrors = registry.poisonErrors();
      const priorQuiesces = quiesces, priorReaps = reaps;
      await assert.rejects(safety.withBoundary(req("finalize"), async () => { actions++; }), CodexBoundaryError);
      assert.equal(reconciles, 0, "an unusable epoch cannot release or refresh credentials");
      assert.equal(actions, 0);
      assert.equal(spawn.state.calls, 0);
      assert.equal(quiesces, priorQuiesces);
      assert.equal(reaps, priorReaps);
      assert.equal(registry.state(), state);
      assert.deepEqual(registry.poisonErrors(), priorErrors, "refusal does not mint fresh cleanup evidence");
    });
  }

  it("rechecks registry state after waiting for the safety queue", async () => {
    const registry = new ExecutionRegistry(newLocalExecutionEpoch(1));
    let reconciles = 0, secondActions = 0;
    const entered = defer<void>(), release = defer<void>();
    const safety = createCodexExecutionSafety(registry, spawnCounter().seam, async () => {
      reconciles++;
      return { kind: "ready" };
    });
    const first = safety.withBoundary(req("checkpoint"), async () => {
      entered.resolve(undefined);
      await release.promise;
    });
    await entered.promise;
    const second = safety.withBoundary(req("finalize"), async () => { secondActions++; });
    const outcomes = Promise.allSettled([first, second]);
    registry.poison({ category: "protocol", message: "proof changed while queued" });
    release.resolve(undefined);
    const results = await outcomes;
    assert.equal(results[1]!.status, "rejected");
    assert.equal(reconciles, 1, "only the first usable acquisition reconciled");
    assert.equal(secondActions, 0);
  });
});

describe("m1 cooperative safety admission", () => {
  it("attributes an already hard-expired admission instead of reporting a soft stop", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(3122));
    const safety = createCodexExecutionSafety(reg, spawnCounter().seam);
    const stop = new AbortController();
    stop.abort();
    await assert.rejects(safety.withBoundary({
      boundary: "checkpoint", sink: "done_checkpoint", deadlineMs: 0,
      activeStep: () => "checkpoint_lock_wait", checkpointStop: { signal: stop.signal },
    }, async () => assert.fail("expired action ran")), (error: unknown) => {
      assert.ok(error instanceof CodexBoundaryError);
      assert.equal(error.errors[0]?.category, "timeout");
      assert.equal(error.boundary, "checkpoint");
      assert.equal(error.sink, "done_checkpoint");
      assert.equal(error.step, "checkpoint_lock_wait");
      assert.equal(reg.isPoisoned(), true);
      return true;
    });
  });
  it("uses the injected hard clock while source certification waits behind its predecessor", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(3140));
    const entered = defer<void>(), release = defer<void>();
    let fireQueue: (() => void) | undefined;
    let reconciles = 0;
    const safety = createCodexExecutionSafety(reg, spawnCounter().seam,
      async () => { reconciles++; return { kind: "ready" }; },
      undefined, undefined,
      (request, _ms, fire) => {
        if (request.checkpointStop) fireQueue = fire;
        return () => {};
      });
    const active = safety.withBoundary(req("checkpoint"), async () => {
      entered.resolve(); await release.promise;
    });
    const activeChecked = assert.rejects(active, CodexBoundaryError);
    await entered.promise;
    const stop = new AbortController(); stop.abort();
    const queued = safety.withBoundary({ boundary: "checkpoint", sink: "done_checkpoint",
      deadlineMs: 30_000, activeStep: () => "checkpoint_lock_wait",
      checkpointStop: { signal: stop.signal } }, async () => assert.fail("queued action ran"));
    const checked = assert.rejects(queued, (error: unknown) => {
      assert.ok(error instanceof CodexBoundaryError);
      assert.equal(error.stage, "reconcile");
      assert.equal(error.errors[0]?.category, "timeout");
      assert.equal(error.sink, "done_checkpoint");
      assert.equal(error.step, "checkpoint_lock_wait");
      return true;
    });
    try {
      assert.ok(fireQueue, "queue acquisition must use the injected hard clock");
      fireQueue();
      await checked;
      assert.equal(reconciles, 1, "queued source checks cannot overtake the active predecessor");
      assert.equal(reg.isPoisoned(), true);
    } finally {
      // Also settle the fixture when this assertion is used as a regression probe.
      reg.poison({ category: "protocol", message: "queue fixture teardown" });
      release.resolve();
      await Promise.allSettled([activeChecked, checked]);
    }
  });

  it("forfeits a queued boundary while retaining the active predecessor in the tail", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(3123));
    const safety = createCodexExecutionSafety(reg, spawnCounter().seam);
    const entered = defer<void>(), release = defer<void>();
    const events: string[] = [];
    const active = safety.withBoundary(req("checkpoint"), async () => { entered.resolve(); await release.promise; events.push("active"); });
    await entered.promise;
    const stop = new AbortController();
    const request = { ...req("checkpoint"), checkpointStop: { signal: stop.signal } };
    const cancelled = safety.withBoundary(request, async () => assert.fail("cancelled action ran"));
    const rejected = assert.rejects(cancelled, CheckpointSoftDeadlineError);
    const later = safety.withBoundary(req("checkpoint"), async () => { events.push("later"); });
    stop.abort(); await rejected;
    await new Promise<void>(resolve => setImmediate(resolve));
    const beforePredecessorSettled = [...events];
    release.resolve(); await Promise.all([active, later]);
    assert.deepEqual(beforePredecessorSettled, [], "successor must remain queued until predecessor settles");
    assert.deepEqual(events, ["active", "later"]);
    assert.equal(reg.isPoisoned(), false);
  });

  for (const path of ["buffered", "streaming"] as const) {
    for (const failure of ["clean", "dispose", "reap"] as const) {
      it("m1 " + path + " cancellation awaits " + failure + " cleanup and closes owned collectors", async () => {
        const reg = new ExecutionRegistry(newLocalExecutionEpoch(3124));
        const launched = defer<void>(), launch = defer<void>(), disposed = defer<void>(), reap = defer<void>();
        const stdout = new PassThrough(), stderr = new PassThrough();
        let reaps = 0, disposals = 0;
        const safety = createCodexExecutionSafety(reg, spawnCounter().seam, undefined, undefined, async () => {
          launched.resolve(); await launch.promise;
          return { stdin: null, stdout, stderr, waitChild: async () => new Promise(() => {}), root: {
            kind: "boundary_action",
            dispose: async () => { disposals++; disposed.resolve(); if (failure === "dispose") throw new Error("unsafe dispose"); },
            reap: async () => { reaps++; await reap.promise; return failure === "reap" ? failReap(1000) : { ok: true }; },
          } };
        });
        const cache = new GitCache("/unused-m1", nullLogger());
        const stop = new AbortController();
        const boundary = safety.withBoundary(req("checkpoint"), async permit => {
          const operation = cache.withBoundaryProcessSpawner(request => safety.spawnBoundaryProcess(permit, request), permit.signal,
            async () => path === "buffered" ? cache.branchTip("/unused", "agent/issue-3123")
              : cache.readBare("/unused", ["show", "HEAD"], { maxBytes: 1024 }), { softSignal: stop.signal });
          const rejection = assert.rejects(operation, error => {
            if (failure === "clean") assert.ok(error instanceof CheckpointSoftDeadlineError);
            else assert.ok(!(error instanceof CheckpointSoftDeadlineError));
            return true;
          });
          await launched.promise;
          stop.abort(); // deliberately during async launch
          launch.resolve();
          await disposed.promise;
          assert.equal(stdout.destroyed, false, "no stream closure before verified reap");
          reap.resolve();
          await rejection;
          if (failure === "clean") {
            assert.equal(stdout.destroyed, true);
            assert.equal(stderr.destroyed, true);
          }
        });
        if (failure === "clean") await boundary;
        else await assert.rejects(boundary, CodexBoundaryError);
        assert.equal(reaps, 1); assert.equal(disposals, 1);
        assert.equal(reg.isPoisoned(), failure !== "clean");
        stdout.destroy(); stderr.destroy();
      });
    }
  }

  for (const outcome of ["settled", "unsafe", "hard"] as const) {
    it("m1 done stop joins the real installer before classification: " + outcome, async () => {
      const lifecycle = new AbortController(), stop = new AbortController(), hard = new AbortController();
      const selected = selectCodexBinding({ codex: { auth_mode: "api_key", access_token: "fixture-token", capability: "fixture-cap" } });
      if (selected.kind !== "codex") assert.fail("binding");
      let calls = 0;
      const installer = createRunLaneInstallerReconcile(lifecycle.signal, {
        runId: "run-1", binding: selected.binding,
        client: { releaseCodex: async () => { calls++; assert.fail("no credential work"); },
          refreshCodex: async () => { calls++; assert.fail("no refresh"); } },
        registerToken: () => assert.fail("no token"),
      });
      const settlement = defer<{ results: []; truncated: boolean }>();
      let rejectInstall!: (error: Error) => void;
      installer.promise = Promise.race([settlement.promise, new Promise<never>((_resolve, reject) => { rejectInstall = reject; })]);
      lifecycle.abort(); stop.abort();
      const authority = new AbortController();
      const request = { ...req("checkpoint"), checkpointStop: {
        signal: stop.signal, workerShutdown: () => ({ stopSignal: authority.signal, deadlineAt: Date.now() + 1000 }),
      } };
      let returned = false;
      const result = installer.reconcile(request, hard.signal).finally(() => { returned = true; });
      // Attach immediately: unsafe cleanup must become blocked, not an unhandled rejection.
      const checked = outcome === "settled" ? assert.rejects(result, CheckpointSoftDeadlineError)
        : result.then(value => assert.equal(value.kind, "blocked"));
      await new Promise<void>(resolve => setImmediate(resolve));
      assert.equal(installer.abort.signal.aborted, true);
      assert.equal(returned, false, "no clean stop while installer bookkeeping is live");
      assert.equal(calls, 0);
      if (outcome === "unsafe") rejectInstall(new Error("unverified installer cleanup"));
      else if (outcome === "hard") hard.abort();
      else settlement.resolve({ results: [], truncated: false });
      await checked;
      assert.equal(calls, 0);
      // The real owner retains and eventually joins the promise after hard expiry.
      if (outcome === "hard") {
        settlement.resolve({ results: [], truncated: false });
        await installer.promise;
      }
    });
  }

  for (const fenced of [false, true]) {
    it("m1 installer source distinguishes worker shutdown from real lifecycle fence: " + fenced, async () => {
      const lifecycle = new AbortController(), stop = new AbortController(), authority = new AbortController();
      const selected = selectCodexBinding({ codex: { auth_mode: "api_key", access_token: "fixture-token", capability: "fixture-cap" } });
      if (selected.kind !== "codex") assert.fail("binding");
      const installer = createRunLaneInstallerReconcile(lifecycle.signal, {
        runId: "run-1", binding: selected.binding,
        client: { releaseCodex: async () => assert.fail("no release after lifecycle abort"),
          refreshCodex: async () => assert.fail("no refresh") }, registerToken: () => assert.fail("no token"),
      });
      lifecycle.abort(); stop.abort(); if (fenced) authority.abort();
      const proof = { stopSignal: authority.signal, deadlineAt: Date.now() + 1000 };
      const request = { ...req("checkpoint"), checkpointStop: { signal: stop.signal, workerShutdown: () => proof } };
      if (fenced) assert.equal((await installer.reconcile(request, new AbortController().signal)).kind, "blocked");
      else await assert.rejects(installer.reconcile(request, new AbortController().signal), CheckpointSoftDeadlineError);
    });
  }

  it("m1 a soft stop never downgrades an ambiguous subscription refresh", async () => {
    const selected = selectCodexBinding({ codex: { auth_mode: "subscription", access_token: "fixture-token", capability: "fixture-cap", generation: 3,
      chatgpt_account_id: "fixture-account", chatgpt_plan_type: null } });
    if (selected.kind !== "codex") assert.fail("binding");
    const stop = new AbortController();
    const client = new WorkerClient("http://fixture.invalid", "fixture-token", "test", nullLogger());
    const reconcile = buildRunLaneReconcile("run-1", client, selected.binding, () => assert.fail("no token"));
    const original = client.refreshCodex.bind(client);
    client.refreshCodex = async () => { stop.abort(); throw new CodexRequestFailure("parent_abort"); };
    const request = { ...req("checkpoint"), checkpointStop: { signal: stop.signal } };
    const first = await reconcile(request, new AbortController().signal);
    assert.equal(first.kind, "blocked");
    const again = await reconcile(request, new AbortController().signal);
    assert.equal(again.kind, "blocked");
    client.refreshCodex = original;
  });
});


it("m1 cancellation during natural-exit reap closes streams only after the single reap settles", async () => {
  const reg = new ExecutionRegistry(newLocalExecutionEpoch(3126));
  const inReap = defer<void>(), reaped = defer<ReapOutcome>(), disposed = defer<void>();
  const stdout = new PassThrough(), stderr = new PassThrough();
  let reaps = 0, disposals = 0;
  const safety = createCodexExecutionSafety(reg, spawnCounter().seam, undefined, undefined, async () => ({
    stdin: null, stdout, stderr, waitChild: async () => ({ code: 0 }),
    root: { kind: "boundary_action", reap: async () => { reaps++; inReap.resolve(); return reaped.promise; },
      dispose: async () => { disposals++; disposed.resolve(); } },
  }));
  await safety.withBoundary(req("checkpoint"), async permit => {
    const process = await safety.spawnBoundaryProcess(permit, { argv: ["/bin/true"], cwd: "/unused", env: {}, identity: "worker_pat" });
    await inReap.promise;
    const cancelled = process.cancel();
    await disposed.promise;
    assert.equal(stdout.destroyed, false); assert.equal(stderr.destroyed, false);
    reaped.resolve({ ok: true }); await cancelled;
    assert.equal(stdout.destroyed, true); assert.equal(stderr.destroyed, true);
  });
  assert.equal(reaps, 1); assert.equal(disposals, 1); assert.equal(reg.isPoisoned(), false);
});

for (const outcome of ["settled", "unsafe", "vault", "hard"] as const) {
  it("m1 actual boundary certifies stopped installer admission: " + outcome, async () => {
    const lifecycle = new AbortController(), stop = new AbortController();
    const selected = selectCodexBinding({ codex: { auth_mode: "api_key", access_token: "fixture-token", capability: "fixture-cap" } });
    if (selected.kind !== "codex") assert.fail("binding");
    const installer = createRunLaneInstallerReconcile(lifecycle.signal, {
      runId: "run-1", binding: selected.binding,
      client: { releaseCodex: async () => assert.fail("no release"), refreshCodex: async () => assert.fail("no refresh") },
      registerToken: () => assert.fail("no token"), isVaultLocked: () => outcome === "vault",
    });
    const settlement = defer<{ results: []; truncated: boolean }>();
    let rejectInstall!: (error: Error) => void;
    installer.promise = Promise.race([settlement.promise, new Promise<never>((_resolve, reject) => { rejectInstall = reject; })]);
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(3127));
    let fire!: () => void;
    const safety = new CodexExecutionSafetyImpl(reg, {
      quiesce: async () => assert.fail("no quiesce"), reap: async () => assert.fail("no reap"),
      dispose: async () => ({ kind: "disposed" }), spawnRoot: spawnCounter().seam,
      armDeadline: (_request, _ms, callback) => { fire = callback; return () => {}; },
    }, installer.reconcile);
    stop.abort();
    let returned = false;
    const result = safety.withBoundary({ ...req("checkpoint"), sink: "done_checkpoint",
      checkpointStop: { signal: stop.signal } }, async () => assert.fail("no action")).finally(() => { returned = true; });
    const checked = assert.rejects(result, (error: unknown) => {
      if (outcome === "settled") assert.ok(error instanceof CheckpointSoftDeadlineError);
      else {
        assert.ok(error instanceof CodexBoundaryError);
        assert.equal(error.stage, "reconcile");
        assert.equal(error.sink, "done_checkpoint");
        if (outcome === "vault") assert.equal(error.deferral, "vault_locked");
      }
      return true;
    });
    await new Promise<void>(resolve => setImmediate(resolve));
    assert.equal(installer.abort.signal.aborted, true);
    assert.equal(returned, false);
    if (outcome === "unsafe") rejectInstall(new Error("installer cleanup unverified"));
    else if (outcome === "hard") fire();
    else settlement.resolve({ results: [], truncated: false });
    await checked;
    if (outcome === "hard") { settlement.resolve({ results: [], truncated: false }); await installer.promise; }
    assert.equal(reg.isPoisoned(), outcome !== "settled");
  });
}

it("m1 genuine stopped source stays behind its credential predecessor and ahead of its successor", async () => {
  const lifecycle = new AbortController(), stop = new AbortController();
  const selected = selectCodexBinding({ codex: { auth_mode: "api_key", access_token: "fixture-token", capability: "fixture-cap" } });
  if (selected.kind !== "codex") assert.fail("binding");
  const entered = defer<void>(), release = defer<void>();
  let calls = 0, live = 0, reconciles = 0;
  const installer = createRunLaneInstallerReconcile(lifecycle.signal, {
    runId: "run-1", binding: selected.binding,
    client: { releaseCodex: async () => {
      calls++; live++;
      assert.equal(live, 1);
      if (calls === 1) { entered.resolve(); await release.promise; }
      live--;
      return { auth_mode: "api_key", access_token: "fixture-token" };
    }, refreshCodex: async () => assert.fail("no refresh") },
    registerToken: () => {},
  });
  const reg = new ExecutionRegistry(newLocalExecutionEpoch(3129));
  const safety = createCodexExecutionSafety(reg, spawnCounter().seam, async (...args) => {
    reconciles++; return installer.reconcile(...args);
  });
  const first = safety.withBoundary(req("checkpoint"), async () => {});
  await entered.promise;
  const stopped = safety.withBoundary({ ...req("checkpoint"), checkpointStop: { signal: stop.signal } },
    async () => assert.fail("no stopped action"));
  const checked = assert.rejects(stopped, CheckpointSoftDeadlineError);
  const last = safety.withBoundary(req("checkpoint"), async () => {});
  stop.abort();
  await new Promise<void>(resolve => setImmediate(resolve));
  assert.equal(reconciles, 1, "source checks cannot overlap credential work");
  assert.equal(calls, 1);
  release.resolve();
  await Promise.all([first, checked, last]);
  assert.equal(reconciles, 3);
  assert.equal(calls, 2, "stopped source never sends credentials");
  assert.equal(reg.isPoisoned(), false);
});

for (const stage of ["reconcile", "quiesce", "reap", "action"] as const) {
  for (const refusal of ["poison", "hard", "lifecycle", "authority"] as const) {
    it("m1 soft escape is guarded at " + stage + " against " + refusal, async () => {
      const reg = new ExecutionRegistry(newLocalExecutionEpoch(3130));
      const stop = new AbortController(), lifecycle = new AbortController(), authority = new AbortController();
      let fire!: () => void;
      const primary = new CheckpointSoftDeadlineError();
      const fail = () => {
        if (refusal === "poison") reg.poison({ category: "protocol", message: "source cleanup failed" });
        if (refusal === "hard") fire();
        if (refusal === "lifecycle") lifecycle.abort();
        if (refusal === "authority") authority.abort();
        stop.abort();
        throw primary;
      };
      const safety = new CodexExecutionSafetyImpl(reg, {
        armDeadline: (_request, _ms, callback) => { fire = callback; return () => {}; },
        quiesce: async () => { if (stage === "quiesce") fail(); return { kind: "quiescent", epoch: 3130 }; },
        reap: async () => { if (stage === "reap") fail(); return { kind: "observed_empty", evidence: "supervisor_echild", epoch: 3130 }; },
        dispose: async () => ({ kind: "disposed" }), spawnRoot: spawnCounter().seam,
      }, async () => { if (stage === "reconcile") fail(); return { kind: "ready" }; });
      const common = { deadlineMs: 1000, sink: "done_checkpoint" as const,
        activeStep: () => "checkpoint_lock_wait" as const, checkpointStop: { signal: stop.signal, lifecycleSignal: lifecycle.signal } };
      const request: BoundaryRequest = refusal === "authority"
        ? { ...common, boundary: "shutdown", gracefulShutdownCheckpoint: { stopSignal: authority.signal, deadlineAt: Date.now() + 1000 } }
        : { ...common, boundary: "checkpoint" };
      await assert.rejects(safety.withBoundary(request, async () => { assert.equal(stage, "action"); fail(); }), (error: unknown) => {
        assert.ok(error instanceof CodexBoundaryError);
        assert.equal(error.stage, stage);
        assert.equal(error.actionError, primary);
        assert.equal(error.sink, "done_checkpoint");
        assert.equal(error.step, "checkpoint_lock_wait");
        assert.equal(reg.isPoisoned(), true);
        return true;
      });
    });
  }
}

it("m1 recovered startup timeout cannot hide lifecycle refusal before the soft signal fires", async () => {
  const reg = new ExecutionRegistry(newLocalExecutionEpoch(3141));
  const lifecycle = new AbortController(), stop = new AbortController();
  const primary = new CheckpointSoftDeadlineError();
  const safety = createCodexExecutionSafety(reg, spawnCounter().seam);
  await assert.rejects(safety.withBoundary({
    boundary: "checkpoint", sink: "done_checkpoint", deadlineMs: 1000,
    activeStep: () => "checkpoint_lock_wait",
    checkpointStop: { signal: stop.signal, lifecycleSignal: lifecycle.signal },
  }, async () => { lifecycle.abort(); throw primary; }), (error: unknown) => {
    assert.ok(error instanceof CodexBoundaryError);
    assert.equal(error.actionError, primary);
    assert.equal(error.stage, "action");
    assert.equal(error.errors[0]?.category, "authorization");
    assert.equal(error.sink, "done_checkpoint");
    assert.equal(error.step, "checkpoint_lock_wait");
    assert.equal(reg.isPoisoned(), true);
    assert.equal(stop.signal.aborted, false);
    return true;
  });
});

it("m1 reconcile soft escape preserves poison evidence and primary error", async () => {
  const reg = new ExecutionRegistry(newLocalExecutionEpoch(3128));
  const stop = new AbortController();
  const primary = new CheckpointSoftDeadlineError();
  const evidence = { category: "protocol" as const, message: "source refused" };
  const safety = createCodexExecutionSafety(reg, spawnCounter().seam, async () => {
    reg.poison(evidence); stop.abort(); throw primary;
  });
  await assert.rejects(safety.withBoundary({ ...req("checkpoint"), sink: "done_checkpoint",
    activeStep: () => "checkpoint_lock_wait", checkpointStop: { signal: stop.signal } },
  async () => assert.fail("no action")), (error: unknown) => {
    assert.ok(error instanceof CodexBoundaryError);
    assert.equal(error.stage, "reconcile");
    assert.equal(error.sink, "done_checkpoint");
    assert.equal(error.step, "checkpoint_lock_wait");
    assert.equal(error.actionError, primary);
    assert.deepEqual(error.errors, [evidence]);
    return true;
  });
});

for (const refusal of ["vault", "account", "response", "lifecycle", "stale"] as const) {
  it("m1 soft admission preserves " + refusal + " refusal", async () => {
    const subscription = refusal === "stale" || refusal === "account";
    const selected = selectCodexBinding({ codex: subscription
      ? { auth_mode: "subscription", access_token: "fixture-token", capability: "fixture-cap", generation: 3,
        chatgpt_account_id: "fixture-account", chatgpt_plan_type: null }
      : { auth_mode: "api_key", access_token: "fixture-token", capability: "fixture-cap" } });
    if (selected.kind !== "codex") assert.fail("binding");
    const stop = new AbortController(), lifecycle = new AbortController();
    if (["vault", "lifecycle", "stale"].includes(refusal)) stop.abort();
    if (refusal === "lifecycle") lifecycle.abort();
    const client = new WorkerClient("http://fixture.invalid", "fixture-token", "test", nullLogger());
    client.releaseCodex = async () => {
      stop.abort();
      throw new CodexRequestFailure("response");
    };
    client.refreshCodex = async () => {
      stop.abort();
      throw new RequestError("POST", "/fixture", 409, JSON.stringify({ reason: "codex_account_unavailable" }));
    };
    const reconcile = buildRunLaneReconcile("run-1", client, selected.binding, () => assert.fail("no token"),
      { value: refusal === "stale" ? 2 : subscription ? 3 : undefined }, () => refusal === "vault", lifecycle.signal);
    // A shutdown label alone never exempts the ordinary lifecycle fence.
    const request = { ...req(refusal === "lifecycle" ? "shutdown" : "checkpoint"), checkpointStop: { signal: stop.signal } };
    const outcome = await reconcile(request, new AbortController().signal);
    assert.equal(outcome.kind, "blocked");
    if (refusal === "account" && outcome.kind === "blocked") assert.equal(outcome.deferral, "account_unavailable");
  });
}

it("m2 acquisition preserves authority attribution after source reconciliation", async () => {
  const reg = new ExecutionRegistry(newLocalExecutionEpoch(3143));
  const lifecycle = new AbortController(), soft = new AbortController();
  const safety = createCodexExecutionSafety(reg, spawnCounter().seam, async () => {
    lifecycle.abort();
    return { kind: "ready" };
  });
  await assert.rejects(safety.withBoundary({
    boundary: "checkpoint", sink: "done_checkpoint", deadlineMs: 10_000,
    checkpointStop: { signal: soft.signal, lifecycleSignal: lifecycle.signal },
  }, async () => assert.fail("revoked authority cannot reach the action")), (error: unknown) => {
    assert.ok(error instanceof CodexBoundaryError);
    assert.equal(error.stage, "reconcile");
    assert.equal(error.errors[0]?.category, "authorization");
    assert.equal(reg.isPoisoned(), true);
    return true;
  });
});

for (const reason of ["owner_cancel", "claim_loss", "untyped_lifecycle", "shutdown_revoked", "worker_shutdown"] as const) {
  it("m2 mutation permit retains hard authority for " + reason, async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(3142));
    const lifecycle = new AbortController(), soft = new AbortController(), authority = new AbortController();
    let shutdown: { stopSignal: AbortSignal; deadlineAt: number } | undefined;
    let observedAbort: boolean | undefined;
    const safety = createCodexExecutionSafety(reg, spawnCounter().seam);
    const boundary = safety.withBoundary({ boundary: "checkpoint", sink: "done_checkpoint", deadlineMs: 10_000,
      checkpointStop: { signal: soft.signal, lifecycleSignal: lifecycle.signal, workerShutdown: () => shutdown },
    }, async permit => {
      // Import mutation masks only cooperative cancellation. The permit is its hard signal.
      soft.abort();
      assert.equal(permit.signal.aborted, false);
      if (reason === "worker_shutdown" || reason === "shutdown_revoked") {
        shutdown = { stopSignal: authority.signal, deadlineAt: Date.now() + 10_000 };
      }
      lifecycle.abort(new Error(reason));
      if (reason === "shutdown_revoked") authority.abort();
      observedAbort = permit.signal.aborted;
    });
    await assert.rejects(boundary, reason === "worker_shutdown" ? CheckpointSoftDeadlineError : CodexBoundaryError);
    assert.equal(observedAbort, reason !== "worker_shutdown");
    assert.equal(reg.isPoisoned(), reason !== "worker_shutdown");
  });
}

describe("R2 terminal disposal acquisition", () => {
  function fixture(dispose: BoundarySeams["dispose"]) {
    const registry = new ExecutionRegistry(newLocalExecutionEpoch(3200));
    let now = 100;
    let expire: (() => void) | undefined;
    let cancels = 0;
    const safety = new CodexExecutionSafetyImpl(registry, {
      quiesce: r => registry.quiesceChildren(r.deadlineMs),
      reap: (r, epoch) => registry.reapProcesses(r.deadlineMs, epoch),
      dispose, spawnRoot: spawnCounter().seam,
      now: () => now,
      armDeadline: (request, _ms, fire) => {
        if (request.boundary !== "terminal") return () => {};
        expire = fire;
        return () => { cancels++; expire = undefined; };
      },
    });
    return { registry, safety, advance: (ms: number) => { now += ms; },
      expire: () => { assert.ok(expire); expire(); }, cancels: () => cancels };
  }

  for (const failure of [false, true]) {
    it(`enters idle zero-budget disposal and joins cleanup (throws=${failure})`, async () => {
      const disposing = defer<void>(), cleanup = defer<void>();
      const primary = new Error("idle cleanup failed");
      const budgets: number[] = [];
      const f = fixture(async request => {
        budgets.push(request.deadlineMs);
        disposing.resolve();
        await cleanup.promise;
        if (failure) throw primary;
        return { kind: "disposed" };
      });
      const disposal = f.safety.dispose({ boundary: "terminal", deadlineMs: 0 });
      let settled = false;
      const checked = disposal.then(
        result => { settled = true; return { result }; },
        error => { settled = true; return { error }; },
      );
      // A turn of the event loop also lets a refused disposal report its result.
      await new Promise<void>(resolve => setImmediate(resolve));
      try {
        assert.deepEqual(budgets, [0], "idle disposal must enter the cleanup owner without inflating its budget");
        await disposing.promise;
        let laterRan = false;
        const later = f.safety.withBoundary(req("checkpoint"), async () => { laterRan = true; });
        f.advance(100);
        await new Promise<void>(resolve => setImmediate(resolve));
        assert.equal(settled, false);
        assert.equal(laterRan, false, "later acquisition must join entered zero-budget cleanup");
        cleanup.resolve();
        assert.deepEqual(await checked, failure ? { error: primary } : { result: { kind: "disposed" } });
        await later;
        assert.equal(laterRan, true);
      } finally {
        cleanup.resolve();
        await checked;
      }
    });
  }

  for (const order of ["predecessor-first", "deadline-first", "ready-after-deadline"] as const) {
    it(`refuses queued disposal in the same-tick ${order} race`, async () => {
      let disposals = 0;
      const f = fixture(async () => { disposals++; return { kind: "disposed" }; });
      const entered = defer<void>(), release = defer<void>();
      const active = f.safety.withBoundary(req("checkpoint"), async () => {
        entered.resolve(); await release.promise;
      });
      await entered.promise;
      const disposal = f.safety.dispose({ boundary: "terminal", deadlineMs: 50 });
      try {
        if (order === "predecessor-first") release.resolve();
        f.advance(50);
        if (order !== "ready-after-deadline") f.expire();
        if (order !== "predecessor-first") release.resolve();
        await active;
        assert.deepEqual(await disposal, { kind: "incomplete", errors: [{
          category: "timeout", message: "codex disposal expired in the boundary queue",
        }] });
        assert.equal(disposals, 0);
      } finally { release.resolve(); await active; }
    });
  }

  for (const mode of ["hook", "zero", "expired"] as const) {
    it(`returns incomplete on ${mode} acquisition expiry without releasing the real predecessor`, async () => {
      let disposals = 0;
      const f = fixture(async () => { disposals++; return { kind: "disposed" }; });
      const entered = defer<void>(), release = defer<void>();
      const active = f.safety.withBoundary(req("checkpoint"), async () => {
        entered.resolve(); await release.promise;
      });
      await entered.promise;
      const disposal = f.safety.dispose({ boundary: "terminal", deadlineMs: mode === "zero" ? 0 : 50 });
      if (mode === "hook") { f.advance(50); f.expire(); }
      if (mode === "expired") { f.advance(50); release.resolve(); }
      try {
        assert.deepEqual(await disposal, { kind: "incomplete", errors: [{
          category: "timeout", message: "codex disposal expired in the boundary queue",
        }] });
        const events: string[] = [];
        const successor = f.safety.withBoundary(req("checkpoint"), async () => { events.push("boundary"); });
        const capture = f.safety.settleForCredentialFreeCapture(1000).then(result => {
          events.push("capture"); return result;
        });
        if (mode !== "expired") {
          await new Promise<void>(resolve => setImmediate(resolve));
          assert.deepEqual(events, [], "neither successor can overtake the real predecessor");
          assert.equal(f.registry.state(), "closed", "expiry leaves the registry untouched");
        }
        release.resolve();
        await active;
        await successor;
        assert.equal((await capture).kind, "observed_empty");
        assert.deepEqual(events, ["boundary", "capture"], "retained slot eventually drains in order");
        assert.equal(disposals, 0, "late predecessor settlement never starts terminal disposal");
      } finally { release.resolve(); await active; }
    });
  }

  for (const failure of [false, true]) {
    it(`passes the original remaining budget and joins owned cleanup (throws=${failure})`, async () => {
      const entered = defer<void>(), release = defer<void>(), disposing = defer<void>(), cleanup = defer<void>();
      const primary = new Error("disposal failed");
      const budgets: number[] = [];
      const f = fixture(async request => {
        budgets.push(request.deadlineMs); disposing.resolve(); await cleanup.promise;
        if (failure) throw primary;
        return { kind: "disposed" };
      });
      const active = f.safety.withBoundary(req("checkpoint"), async () => {
        entered.resolve(); await release.promise;
      });
      await entered.promise;
      const disposal = f.safety.dispose({ boundary: "terminal", deadlineMs: 50 });
      const checked = failure ? assert.rejects(disposal, error => error === primary) : disposal;
      f.advance(20); release.resolve(); await active; await disposing.promise;
      assert.deepEqual(budgets, [30]);
      assert.equal(f.cancels(), 1, "acquisition timer is cancelled once before owned cleanup");
      let laterRan = false;
      const later = f.safety.withBoundary(req("checkpoint"), async () => { laterRan = true; });
      f.advance(50);
      await new Promise<void>(resolve => setImmediate(resolve));
      assert.equal(laterRan, false, "owned cleanup is joined even past the acquisition deadline");
      cleanup.resolve(); await checked; await later;
      assert.equal(laterRan, true, "success or throw releases the queue");
      assert.equal(budgets.length, 1);
    });
  }
});
