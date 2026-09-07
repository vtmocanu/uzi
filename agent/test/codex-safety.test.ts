import { describe, it } from "node:test";
import assert from "node:assert/strict";

import {
  CodexBoundaryError,
  CodexExecutionSafetyImpl,
  createCodexExecutionSafety,
  type BoundaryActionIdentity,
  type BoundaryActionOutcome,
  type BoundarySeams,
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
