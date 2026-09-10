import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { PassThrough } from "node:stream";

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
import type { BoundaryPermit, BoundaryRequest } from "../src/harness.js";

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
  it("uses one absolute deadline and passes a shrinking remaining budget across stages", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(12));
    const seen: number[] = [];
    const safety = new CodexExecutionSafetyImpl(reg, {
      quiesce: async (request) => {
        seen.push(request.deadlineMs);
        await new Promise<void>((resolve) => setTimeout(resolve, 20));
        return { kind: "quiescent", epoch: 12 };
      },
      reap: async (request) => {
        seen.push(request.deadlineMs);
        return { kind: "observed_empty", evidence: "supervisor_echild", epoch: 12 };
      },
      dispose: async () => ({ kind: "disposed" }),
      spawnRoot: spawnCounter().seam,
    });
    await safety.withBoundary({ boundary: "finalize", deadlineMs: 100 }, async () => undefined);
    assert.equal(seen.length, 2);
    assert.ok((seen[0] ?? 0) <= 100 && (seen[0] ?? 0) > 0);
    assert.ok((seen[1] ?? 0) < (seen[0] ?? 0) - 10, `remaining budgets shrink: ${seen.join(" -> ")}`);
  });

  it("propagates the same deadline AbortSignal into reconciliation", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(13));
    let reconcileSignal: AbortSignal | undefined;
    const safety = createCodexExecutionSafety(
      reg,
      spawnCounter().seam,
      async (_request, signal) => {
        reconcileSignal = signal;
        await new Promise<void>((resolve) => signal.addEventListener("abort", () => resolve(), { once: true }));
        return { kind: "blocked", errors: [{ category: "timeout", message: "reconcile cancelled at boundary deadline" }] };
      },
    );
    const started = Date.now();
    await assert.rejects(
      safety.withBoundary({ boundary: "shutdown", deadlineMs: 25 }, async () => undefined),
      (error: unknown) => error instanceof CodexBoundaryError && error.stage === "reconcile",
    );
    assert.equal(reconcileSignal?.aborted, true);
    assert.ok(Date.now() - started < 100, "reconciliation used the boundary deadline, not its own full timeout");
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

  it("drains an action admitted while an earlier boundary-action batch is settling", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(1));
    const first = defer<ReapOutcome>();
    const second = defer<ReapOutcome>();
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
        void safety.spawnBoundaryAction(permit, ["first"], "command");
        setTimeout(() => {
          void safety.spawnBoundaryAction(permit, ["admitted-during-drain"], "command");
        }, 0);
      })
      .then(() => {
        boundarySettled = true;
      });

    await tick();
    assert.equal(spawnCalls, 2, "the second action was admitted while the first batch drained");
    first.resolve({ ok: true });
    await tick();
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

  it("deadline abort still awaits root reap and poisons instead of abandoning the action", async () => {
    const reg = new ExecutionRegistry(newLocalExecutionEpoch(10));
    let reaped = false;
    let reapBudget = -1;
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
    await assert.rejects(
      safety.withBoundary({ boundary: "shutdown", deadlineMs: 20 }, async (permit) => {
        const process = await safety.spawnBoundaryProcess(permit, {
          argv: ["/bin/sleep", "forever"], cwd: "/tmp", env: {}, identity: "worker_pat",
        });
        await process.completed;
      }),
      CodexBoundaryError,
    );
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
