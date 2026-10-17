import { it } from "node:test";
import assert from "node:assert/strict";
import { MaintenanceBudget } from "../src/maintenance-budget.js";
import { RunRunner } from "../src/runner.js";

for (const generation of [7, undefined, 8]) {
  it(`memory preservation observes cancellation before further work (generation ${generation})`, async () => {
    const preserve = RunRunner.prototype["preserveWorkerMemory"];
    const budget = new MaintenanceBudget();
    const lifecycle = new AbortController();
    const cancel = new AbortController();
    let cancelled = false, factories = 0, warnings = 0, laterWork = 0;
    const incarnation = {};
    const flight = {
      runId: "memory-cancel-fixture", claimGeneration: 7, cancel,
      retainedLocalCustody: true, preserveRecoveryClone: true, preserveSession: true,
      steering: {
        terminalLifecycleSignal: () => lifecycle.signal,
        claimFence: () => undefined, isCancelled: () => cancelled,
        pendingCredentialSwitch: () => undefined, getPauseMode: () => null,
        markCancelled: () => { cancelled = true; }, abortLifecycle: () => lifecycle.abort(),
      },
      workerMemory: { incarnation, ended: false },
      runLog: { warn: () => {
        warnings++;
        assert.equal(cancelled, true, "the ownership response itself latches cancellation");
        assert.equal(flight.cancellationBudget, budget, "the first observation starts its shared budget");
      } },
      reportState: async () => { laterWork++; },
      batcher: { flushOwned: async () => { laterWork++; } },
    } as unknown as Parameters<typeof preserve>[0];
    const hold = { context: { flight, claim_generation: 7 }, deadline: Date.now() + 5000 } as Parameters<typeof preserve>[1];
    const receiver = {
      shutdownSignal: new AbortController(),
      cancellationBudgetFactory: () => { factories++; return budget; },
      client: { memoryIncarnation: incarnation, getRunOwnership: async () => ({ status: "cancelled", claim_generation: generation }) },
      git: { withRecoveryOperation: async (signal: AbortSignal, _deadline: number, action: (signal: AbortSignal) => Promise<void>) => action(signal) },
      retainedTerminalGuard: RunRunner.prototype["retainedTerminalGuard"],
      getObservedRunOwnership: RunRunner.prototype["getObservedRunOwnership"],
      observeCancellation: RunRunner.prototype["observeCancellation"],
      settleForCredentialCapture: async () => { laterWork++; return { kind: "observed_empty" }; },
      captureRecoveryRestorePoint: async () => { laterWork++; },
    } as unknown as RunRunner;
    try {
      await preserve.call(receiver, flight, hold);
      assert.equal(warnings, 1);
      assert.equal(factories, 1);
      assert.equal(cancel.signal.aborted, true);
      assert.equal(lifecycle.signal.aborted, true);
      assert.equal(laterWork, 0, "no capture, publication, flush or park follows cancellation");
      assert.equal(flight.retainedLocalCustody, true);
      assert.equal(flight.preserveRecoveryClone, true);
      assert.equal(flight.preserveSession, true);
      assert.notEqual(flight.parked, true);
      assert.equal(flight.cancellationCustodyUnknown, generation === 7 ? undefined : true);
      assert.equal(flight.keepGuardedInventoryOpen, generation === 7 ? undefined : true);
    } finally { budget.dispose(); }
  });
}
