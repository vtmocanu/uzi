import assert from "node:assert/strict";
import { it } from "node:test";
import { CheckpointSoftDeadlineError } from "../src/checkpoint-stop.js";
import { createRunLaneInstallerReconcile } from "../src/codex/codex-executor.js";
import { ExecutionRegistry, newLocalExecutionEpoch } from "../src/codex/registry.js";
import { CodexBoundaryError, createCodexExecutionSafety, type ArmBoundaryDeadline } from "../src/codex/safety.js";
import { selectCodexBinding } from "../src/codex/select.js";
import type { BoundaryRequest, GracefulShutdownCheckpoint } from "../src/harness.js";

function deferred() {
  let resolve!: () => void;
  const promise = new Promise<void>(done => { resolve = done; });
  return { promise, resolve };
}

function fixture() {
  let now = Date.now();
  const timers = new Set<{ at: number; fire: () => void }>();
  const arm: ArmBoundaryDeadline = (_request, ms, fire) => {
    const timer = { at: now + ms, fire };
    timers.add(timer);
    return () => { timers.delete(timer); };
  };
  const advance = (ms: number) => {
    now += ms;
    const due = [...timers].filter(timer => timer.at <= now);
    for (const timer of due) {
      if (timers.delete(timer)) timer.fire();
    }
  };
  const lifecycle = new AbortController(), soft = new AbortController(), authority = new AbortController();
  let proof: GracefulShutdownCheckpoint | undefined;
  const shutdown = (signal: "soft" | "lifecycle" = "soft") => {
    proof = { stopSignal: authority.signal, deadlineAt: now + 15_000 };
    if (signal === "soft") soft.abort();
    else lifecycle.abort();
  };
  const selected = selectCodexBinding({ codex: {
    auth_mode: "api_key", access_token: "fixture", capability: "fixture",
  } });
  if (selected.kind !== "codex") assert.fail("binding");
  let releases = 0;
  const installer = createRunLaneInstallerReconcile(lifecycle.signal, {
    runId: "deadline-test", binding: selected.binding,
    client: {
      releaseCodex: async () => { releases++; return { access_token: "fixture", auth_mode: "api_key" }; },
      refreshCodex: async () => assert.fail("no refresh"),
    },
    registerToken: () => {},
  });
  const registry = new ExecutionRegistry(newLocalExecutionEpoch(3150));
  let spawns = 0;
  const budgets: number[] = [];
  const safety = createCodexExecutionSafety(registry, async () => assert.fail("no root spawn"),
    installer.reconcile, undefined, async (_request, budget) => {
      spawns++; budgets.push(budget);
      return {
        stdin: null, stdout: null, stderr: null,
        waitChild: async () => ({ code: 0 }),
        root: { kind: "boundary_action", reap: async () => ({ ok: true }), dispose: async () => {} },
      };
    }, arm, () => now);
  const request: BoundaryRequest = {
    boundary: "checkpoint", sink: "done_checkpoint", deadlineMs: 90_000,
    activeStep: () => "checkpoint_lock_wait",
    checkpointStop: { signal: soft.signal, lifecycleSignal: lifecycle.signal,
      workerShutdown: () => proof, now: () => now },
  };
  return { safety, registry, installer, request, lifecycle, soft, authority, shutdown, advance,
    now: () => now, elapseWithoutTimers: (ms: number) => { now += ms; }, timers, budgets, spawns: () => spawns, releases: () => releases,
    proof: () => proof };
}

for (const notification of ["soft", "lifecycle"] as const) {
  it("late shutdown " + notification + " notification shrinks child budget and revokes the live permit", async () => {
    const f = fixture();
    let entered = false;
    let beforeExpiry: boolean | undefined, afterExpiry: boolean | undefined;
    await assert.rejects(f.safety.withBoundary(f.request, async permit => {
      entered = true;
      f.shutdown(notification);
      beforeExpiry = permit.signal.aborted;
      assert.equal(f.registry.isPoisoned(), false);
      const child = await f.safety.spawnBoundaryProcess(permit, {
        argv: ["/inert"], cwd: "/unused", env: {}, identity: "command",
      });
      await child.completed;
      f.advance(15_000);
      afterExpiry = permit.signal.aborted;
      await assert.rejects(f.safety.spawnBoundaryProcess(permit, {
        argv: ["/inert"], cwd: "/unused", env: {}, identity: "command",
      }));
    }), CodexBoundaryError);
    assert.equal(entered, true);
    assert.equal(beforeExpiry, false);
    assert.equal(afterExpiry, true);
    assert.deepEqual(f.budgets, [15_000]);
    assert.equal(f.spawns(), 1);
    assert.equal(f.registry.isPoisoned(), true);
    assert.equal(f.timers.size, 0);
  });
}

for (const deadline of ["shutdown", "original"] as const) {
  for (const identity of ["command", "worker_pat"] as const) {
    it("absolute " + deadline + " expiry refuses " + identity + " admission before delayed timers fire", async () => {
      const f = fixture();
      let entered = false;
      let beforeAdmission: boolean | undefined, afterAdmission: boolean | undefined;
      await assert.rejects(f.safety.withBoundary(f.request, async permit => {
        entered = true;
        if (deadline === "shutdown") f.shutdown();
        f.elapseWithoutTimers(deadline === "shutdown" ? 15_001 : 90_001);
        beforeAdmission = permit.signal.aborted;
        await assert.rejects(async () => {
          const child = await f.safety.spawnBoundaryProcess(permit, {
            argv: ["/inert"], cwd: "/unused", env: {}, identity,
          });
          await child.completed;
        });
        afterAdmission = permit.signal.aborted;
      }), CodexBoundaryError);
      assert.equal(entered, true);
      assert.equal(beforeAdmission, false, "the timer callback has deliberately not run");
      assert.equal(afterAdmission, true, "admission applies the hard fence synchronously");
      assert.equal(f.spawns(), 0, "no owned work is admitted after the absolute deadline");
      assert.equal(f.registry.rootCount(), 0);
      assert.equal(f.registry.isPoisoned(), true);
      assert.equal(f.timers.size, 0);
    });
  }
}

it("shutdown expiry wakes production quiescence while retaining the unsettled reservation", async () => {
  const f = fixture();
  const reserved = f.registry.reserveLaunch("command");
  if (reserved.kind !== "reserved") assert.fail("reservation");
  let settled = false;
  const pending = f.safety.withBoundary(f.request, async () => assert.fail("no action"));
  const checked = assert.rejects(pending, CodexBoundaryError).then(() => { settled = true; });
  await new Promise<void>(resolve => setImmediate(resolve));
  assert.equal(f.releases(), 1, "genuine source reconciled before shutdown");
  assert.equal(f.registry.state(), "closing");
  f.shutdown();
  f.advance(15_000);
  await new Promise<void>(resolve => setImmediate(resolve));
  const woke = settled;
  const custody = await f.registry.settleForCapture(0);
  f.registry.cancelReservation(reserved.reservation);
  await checked;
  assert.equal(woke, true, "quiescence must wake at shutdown expiry");
  assert.equal(custody.kind, "incomplete");
  assert.equal(f.spawns(), 0);
  assert.equal(f.registry.isPoisoned(), true);
  assert.equal(f.timers.size, 0);
});

it("queued source expiry forfeits only its slot and keeps successors behind the live predecessor", async () => {
  const f = fixture(), entered = deferred(), release = deferred();
  const first = f.safety.withBoundary({ boundary: "checkpoint", deadlineMs: 90_000 }, async () => {
    entered.resolve(); await release.promise;
  });
  const checkedFirst = assert.rejects(first, CodexBoundaryError);
  await entered.promise;
  let secondSettled = false, thirdSettled = false;
  const second = f.safety.withBoundary(f.request, async () => assert.fail("no expired action"));
  const checkedSecond = assert.rejects(second, (error: unknown) => {
    assert.ok(error instanceof CodexBoundaryError);
    assert.equal(error.sink, "done_checkpoint");
    assert.equal(error.step, "checkpoint_lock_wait");
    return true;
  }).then(() => { secondSettled = true; });
  const third = f.safety.withBoundary({ boundary: "checkpoint", deadlineMs: 90_000 },
    async () => assert.fail("no successor action"));
  const checkedThird = assert.rejects(third, CodexBoundaryError).then(() => { thirdSettled = true; });
  f.shutdown();
  f.advance(15_000);
  await new Promise<void>(resolve => setImmediate(resolve));
  const expiredWhileHeld = secondSettled;
  assert.equal(thirdSettled, false);
  assert.equal(f.releases(), 1, "no source work overtakes the predecessor");
  release.resolve();
  await Promise.all([checkedFirst, checkedSecond, checkedThird]);
  assert.equal(expiredWhileHeld, true);
  assert.equal(f.registry.isPoisoned(), true);
  assert.equal(f.timers.size, 0);
});

it("the earlier done hard ceiling retains timeout precedence over a later shutdown deadline", async () => {
  const f = fixture();
  let beforeExpiry: boolean | undefined, afterExpiry: boolean | undefined;
  await assert.rejects(f.safety.withBoundary(f.request, async permit => {
    f.advance(85_000);
    f.shutdown();
    beforeExpiry = permit.signal.aborted;
    f.advance(5_000);
    afterExpiry = permit.signal.aborted;
  }), (error: unknown) => {
    assert.ok(error instanceof CodexBoundaryError);
    assert.ok(error.errors.length > 0);
    assert.ok(error.errors.every(item => item.category === "timeout"));
    return true;
  });
  assert.equal(beforeExpiry, false);
  assert.equal(afterExpiry, true);
  assert.equal(f.registry.isPoisoned(), true);
  assert.equal(f.timers.size, 0);
});

it("timely cooperative shutdown preserves the registry for the typed shutdown boundary", async () => {
  const f = fixture();
  await assert.rejects(f.safety.withBoundary(f.request, async permit => {
    f.shutdown();
    assert.equal(permit.signal.aborted, false);
  }), CheckpointSoftDeadlineError);
  assert.equal(f.registry.isPoisoned(), false);
  f.lifecycle.abort();
  let actions = 0;
  await f.safety.withBoundary({
    boundary: "shutdown", deadlineMs: 90_000, gracefulShutdownCheckpoint: f.proof(),
  }, async permit => { assert.equal(permit.signal.aborted, false); actions++; });
  assert.equal(actions, 1);
  assert.equal(f.registry.isPoisoned(), false);
  assert.equal(f.timers.size, 0);
});
