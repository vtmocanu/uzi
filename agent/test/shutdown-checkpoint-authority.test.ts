import { describe, it } from "node:test";
import assert from "node:assert/strict";
import type { WorkerClient } from "../src/client.js";
import { createRunLaneInstallerReconcile } from "../src/codex/codex-executor.js";
import { selectCodexBinding } from "../src/codex/select.js";
import { createCodexExecutionSafety, CodexBoundaryError } from "../src/codex/safety.js";
import { ExecutionRegistry, newLocalExecutionEpoch } from "../src/codex/registry.js";
import type { BoundaryRequest } from "../src/harness.js";
import { MaintenanceBudget } from "../src/maintenance-budget.js";
import { SteeringChannel, PauseNowSignal, CredentialSwitchSignal } from "../src/steering.js";
import { nullLogger, withReceipts } from "./helpers.js";

function channel() {
  const cancel = new AbortController();
  const client = withReceipts({ getInputs: async () => ({ inputs: [] }) } as unknown as WorkerClient);
  return { ch: new SteeringChannel(client, "run-shutdown", 1, nullLogger(), cancel), cancel };
}

function binding(authMode: "api_key" | "subscription") {
  const selected = selectCodexBinding({ codex: {
    auth_mode: authMode, access_token: "fixture-access", capability: "fixture-capability",
    ...(authMode === "subscription" ? {
      generation: 3, chatgpt_account_id: "fixture-account", chatgpt_plan_type: null,
    } : {}),
  } });
  assert.equal(selected.kind, "codex");
  if (selected.kind !== "codex") throw new Error("missing binding");
  return selected.binding;
}

function request(ch: SteeringChannel, deadlineAt = Date.now() + 10_000): BoundaryRequest {
  return { boundary: "shutdown", deadlineMs: 10_000,
    gracefulShutdownCheckpoint: ch.workerShutdown(deadlineAt) };
}

describe("M1 shutdown checkpoint authority", () => {
  it("spends execution signals once and retains the first immutable deadline", () => {
    const { ch, cancel } = channel();
    const cause = ch.workerShutdown(100);
    assert.equal(cancel.signal.aborted, true);
    assert.equal(ch.lifecycleSignal().aborted, true);
    assert.equal(ch.terminalLifecycleSignal().aborted, true);
    assert.equal(cause.stopSignal.aborted, false);
    assert.equal(ch.workerShutdown(999), cause);
    assert.equal(cause.deadlineAt, 100);
    assert.equal(Object.isFrozen(cause), true);
    ch.markCancelled();
    assert.equal(cause.stopSignal.aborted, true);
    assert.equal(ch.workerShutdown(999).stopSignal.aborted, true);
  });

  for (const stop of ["terminal", "cancel", "switch", "disk"] as const) {
    it(`a delivered ${stop} prevents shutdown eligibility before or after execution abort`, () => {
      for (const before of [true, false]) {
        const { ch } = channel();
        const deliver = () => {
          if (stop === "terminal") ch.abortLifecycle();
          if (stop === "cancel") ch.markCancelled();
          if (stop === "switch") ch.tripCredentialSwitch(0);
          if (stop === "disk") ch.requestDiskStop();
        };
        if (before) deliver();
        const cause = ch.workerShutdown(100);
        if (!before) deliver();
        assert.equal(cause.stopSignal.aborted, true);
      }
    });
  }

  it("pause/rearm never creates checkpoint authority and cannot clear a switch revocation", () => {
    const { ch } = channel();
    ch.abortLifecycle(new PauseNowSignal(), false);
    ch.rearmLifecycle();
    assert.equal(ch.lifecycleSignal().aborted, false);
    ch.abortLifecycle(new CredentialSwitchSignal(), false);
    ch.rearmLifecycle();
    assert.equal(ch.workerShutdown(100).stopSignal.aborted, true);
  });

  for (const authMode of ["api_key", "subscription"] as const) {
    it(`${authMode}: only a marked shutdown bypasses the spent execution lifecycle`, async () => {
      const { ch, cancel } = channel();
      const marked = request(ch);
      let releases = 0;
      let refreshes = 0;
      const tokens: string[] = [];
      const committed = { value: 3 };
      const installer = createRunLaneInstallerReconcile(
        AbortSignal.any([cancel.signal, ch.terminalLifecycleSignal()]),
        {
          runId: "run-shutdown", binding: binding(authMode), committed,
          registerToken: (token) => tokens.push(token),
          client: {
            releaseCodex: async () => { releases++; return { access_token: "fixture-next" }; },
            refreshCodex: async () => { refreshes++; return { access_token: "fixture-next", generation: 4 }; },
          } as never,
        },
      );
      const boundary = new AbortController();
      const unmarked = await installer.reconcile({ boundary: "shutdown", deadlineMs: 1000 }, boundary.signal);
      assert.equal(unmarked.kind, "blocked");
      assert.equal(releases + refreshes, 0);
      assert.equal((await installer.reconcile(marked, boundary.signal)).kind, "ready");
      assert.deepEqual(tokens, ["fixture-next"]);
      assert.equal(releases, authMode === "api_key" ? 1 : 0);
      assert.equal(refreshes, authMode === "subscription" ? 1 : 0);
      assert.equal(committed.value, authMode === "subscription" ? 4 : 3);
    });

    it(`${authMode}: a valid response after the original deadline grants no token or generation`, async (t) => {
      let now = 100;
      t.mock.method(Date, "now", () => now);
      const { ch, cancel } = channel();
      const marked = request(ch, 150);
      const committed = { value: 3 };
      const installer = createRunLaneInstallerReconcile(cancel.signal, {
        runId: "run-shutdown", binding: binding(authMode), committed,
        registerToken: () => assert.fail("late token registration"),
        client: {
          releaseCodex: async () => { now = 151; return { access_token: "fixture-late" }; },
          refreshCodex: async () => { now = 151; return { access_token: "fixture-late", generation: 4 }; },
        } as never,
      });
      assert.equal((await installer.reconcile(marked, new AbortController().signal)).kind, "blocked");
      assert.equal(committed.value, 3);
    });
  }

  it("a delivered transport failure revokes the permit but awaits actual action settlement", async () => {
    const { ch } = channel();
    const marked = request(ch);
    const registry = new ExecutionRegistry(newLocalExecutionEpoch(3));
    const safety = createCodexExecutionSafety(registry, async () => assert.fail("no spawn"),
      async () => ({ kind: "ready" }));
    let entered!: () => void;
    const started = new Promise<void>((resolve) => { entered = resolve; });
    let finish!: () => void;
    const pending = new Promise<void>((resolve) => { finish = resolve; });
    let settled = false;
    const boundary = safety.withBoundary(marked, async (permit) => {
      ch.abortLifecycle(new Error("local transport failure"));
      assert.equal(permit.signal.aborted, true);
      entered();
      await pending;
      throw new Error("primary action failure");
    }).finally(() => { settled = true; });
    const rejected = assert.rejects(boundary, (error: unknown) =>
      error instanceof CodexBoundaryError && error.errors[0]?.category === "authorization" &&
      error.actionError instanceof Error && error.actionError.message === "primary action failure");
    await started;
    assert.equal(settled, false);
    finish();
    await rejected;
  });

  it("installer timeout remains distinct from lifecycle refusal", async () => {
    const { ch, cancel } = channel();
    const marked = request(ch, 0);
    const installer = createRunLaneInstallerReconcile(cancel.signal, {
      runId: "run-shutdown", binding: binding("api_key"),
      registerToken: () => assert.fail("no registration"),
      client: { releaseCodex: async () => assert.fail("no release") } as never,
    });
    installer.promise = Promise.resolve({ results: [], truncated: false });
    const timeout = await installer.reconcile(marked, new AbortController().signal);
    assert.equal(timeout.kind === "blocked" && timeout.errors[0]?.category, "timeout");
    ch.markCancelled();
    const refused = await installer.reconcile(marked, new AbortController().signal);
    assert.equal(refused.kind === "blocked" && refused.errors[0]?.category, "authorization");
    await installer.settle();
  });

  it("safety consumes the cause deadline before credential acquisition", async () => {
    const { ch } = channel();
    const registry = new ExecutionRegistry(newLocalExecutionEpoch(1));
    const safety = createCodexExecutionSafety(registry, async () => assert.fail("no spawn"),
      async () => assert.fail("no reconcile"));
    await assert.rejects(safety.withBoundary(request(ch, 0), async () => assert.fail("no sink")),
      (error: unknown) => error instanceof CodexBoundaryError && error.errors[0]?.category === "timeout");
  });

  it("sticky revocation is authorization, even when the original deadline also expired", async () => {
    const { ch } = channel();
    const marked = request(ch, 0);
    ch.markCancelled();
    const safety = createCodexExecutionSafety(new ExecutionRegistry(newLocalExecutionEpoch(2)),
      async () => assert.fail("no spawn"), async () => assert.fail("no reconcile"));
    await assert.rejects(safety.withBoundary(marked, async () => assert.fail("no sink")),
      (error: unknown) => error instanceof CodexBoundaryError && error.errors[0]?.category === "authorization");
  });

  it("maintenance cancellation reaches reconcile without restoring execution authority", async () => {
    const { ch, cancel } = channel();
    const marked = request(ch);
    let now = 100;
    const budget = new MaintenanceBudget(50, () => now);
    try {
      await MaintenanceBudget.run(budget, async () => {
        const installer = createRunLaneInstallerReconcile(cancel.signal, {
          runId: "run-shutdown", binding: binding("api_key"),
          registerToken: () => assert.fail("no registration"),
          client: { releaseCodex: async (_run: string, _body: unknown, _expected: unknown, signal: AbortSignal) => {
            assert.equal(signal.aborted, false);
            now = 151;
            assert.throws(() => budget.check());
            assert.equal(signal.aborted, true);
            return { access_token: "fixture-late" };
          } } as never,
        });
        assert.equal((await installer.reconcile(marked, new AbortController().signal)).kind, "blocked");
        assert.equal(cancel.signal.aborted, true);
        assert.equal(marked.gracefulShutdownCheckpoint!.stopSignal.aborted, false);
      });
    } finally { budget.dispose(); }
  });
});
