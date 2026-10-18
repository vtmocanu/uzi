import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import { MaintenanceBudget } from "../src/maintenance-budget.js";
import { RunRunner } from "../src/runner.js";
import type { Executor, RunContext } from "../src/executor.js";
import { nullLogger } from "./helpers.js";
import { api, fakeGitlab, git, gitlabClaim, installHarness, runnerWith, worktreeDirFor } from "./runner-harness.js";

installHarness();

for (const expired of [false, true]) {
  it(`server terminal cancellation shares the first deadline and retains fenced source (${expired ? "expired" : "active"})`, async () => {
    const iid = expired ? 30312 : 30311;
    const claim = gitlabClaim(iid, { claim_generation: 7 });
    let now = 10;
    let factories = 0;
    let budget: MaintenanceBudget | undefined;
    let atAbort: MaintenanceBudget | undefined;
    const executor: Executor = {
      async run(ctx) {
        const stopped = new Promise<void>(resolve => ctx.signal!.addEventListener("abort", () => {
          atAbort = MaintenanceBudget.current();
          now += expired ? 600_001 : 1;
          resolve();
        }, { once: true }));
        api.rawInputGets(claim.run_id, {
          inputs: [], terminal_cancelled: { run_id: claim.run_id, generation: 7 },
        });
        await stopped;
        throw new Error("server terminal cancellation");
      },
      killAgentTree() {},
    };
    const { gitlab, calls } = fakeGitlab();
    const runner = runnerWith(() => ({ executor }), gitlab, undefined, undefined, {
      cancellationBudgetFactory: () => {
        factories++;
        return budget = new MaintenanceBudget(600_000, () => now);
      },
    });
    await runner.execute(claim);
    assert.equal(factories, 1, "the authenticated control initializes exactly one budget");
    assert.equal(atAbort, budget, "model abort continuations inherit that budget");
    assert.equal(budget!.deadline, 600_010);
    assert.equal(fs.existsSync(worktreeDirFor(iid)), true, "fencing and expiry both retain the local source");
    assert.ok(!api.states.some(s => s.runId === claim.run_id &&
      ["failed", "completed", "cancelled"].includes(s.body.status)), "no competing terminal report");
    assert.equal(calls.length, 0, "a fence grants no forge publication authority");
  });
}


it("cancellation observed in an active maintenance scope reuses its original budget", async () => {
  const budget = new MaintenanceBudget();
  const cancel = new AbortController();
  let lifecycle = false;
  const observe = RunRunner.prototype["observeCancellation"];
  const flight = { cancel, steering: { markCancelled() {}, abortLifecycle: () => { lifecycle = true; } } } as unknown as Parameters<typeof observe>[0];
  const receiver = { cancellationBudgetFactory: () => { throw new Error("must reuse active budget"); } } as unknown as RunRunner;
  try {
    await MaintenanceBudget.run(budget, async () => {
      assert.equal(observe.call(receiver, flight), budget);
      assert.equal(flight.cancellationBudget, budget);
    });
    assert.equal(cancel.signal.aborted, true);
    assert.equal(lifecycle, true);
  } finally { budget.dispose(); }
});

it("the first flight observation attaches its budget before synchronous model abort listeners", async () => {
  const budget = new MaintenanceBudget();
  const cancel = new AbortController();
  let seen: MaintenanceBudget | undefined;
  cancel.signal.addEventListener("abort", () => { seen = MaintenanceBudget.current(); }, { once: true });
  const observe = RunRunner.prototype["observeCancellation"];
  const flight = {
    runId: "fixture-run", claimGeneration: 7, cancel,
    steering: { markCancelled() {}, abortLifecycle() {} },
  } as unknown as Parameters<typeof observe>[0];
  const receiver = { cancellationBudgetFactory: () => budget } as unknown as RunRunner;
  try {
    await MaintenanceBudget.runAttempt("fixture-run:7", () => observe.call(receiver, flight), async () => {
      MaintenanceBudget.observeCancellation();
      assert.equal(MaintenanceBudget.current(), budget);
    });
    assert.equal(seen, budget);
    assert.equal(flight.cancellationBudget, budget);
  } finally { budget.dispose(); }
});

it("cancellation budget factory failure cannot prevent model and lifecycle stop", () => {
  const cancel = new AbortController();
  let lifecycle = false;
  const observe = RunRunner.prototype["observeCancellation"];
  const flight = { cancel, steering: { markCancelled() {}, abortLifecycle: () => { lifecycle = true; } } } as unknown as Parameters<typeof observe>[0];
  const receiver = { cancellationBudgetFactory: () => { throw new Error("factory fixture"); } } as unknown as RunRunner;
  assert.throws(() => observe.call(receiver, flight), /factory fixture/);
  assert.equal(cancel.signal.aborted, true);
  assert.equal(lifecycle, true);
});

for (const boundary of ["lock admission", "journal read"] as const) {
it(`cancellation expiry during retirement ${boundary} retains the clone`, async () => {
  const claim = gitlabClaim(30303);
  let now = 10;
  let retirement = false;
  let ambient = false;
  const executor: Executor = {
    async run(ctx) {
      const stopped = new Promise<void>(resolve => ctx.signal!.addEventListener("abort", () => resolve(), { once: true }));
      api.setInputs(claim.run_id, [{ id: 1, kind: "cancel" }]);
      await stopped;
      throw new Error("run cancelled");
    },
    killAgentTree() {},
  };
  const retire = git.retireRunnerClone.bind(git);
  git.retireRunnerClone = async (...args) => {
    retirement = true;
    const budget = MaintenanceBudget.current();
    ambient = !!budget;
    if (boundary === "lock admission") {
      // The actual Git lock checks the same clock before admitting its callback.
      now += 600_001;
    } else {
      const read = git["readRecoveryCapture"].bind(git);
      git["readRecoveryCapture"] = async (...readArgs) => {
        const journal = await read(...readArgs);
        now += 600_001;
        return journal;
      };
    }
    return retire(...args);
  };
  const runner = runnerWith(() => ({ executor }), fakeGitlab().gitlab, undefined, undefined, {
    cancellationBudgetFactory: () => new MaintenanceBudget(600_000, () => now),
  });
  await runner.execute(claim);
  assert.equal(retirement, true);
  assert.equal(ambient, true, "retirement carries the cancellation budget across its lock wait");
  assert.equal(fs.existsSync(worktreeDirFor(30303)), true, "expiry inside retirement cannot delete the source");
});
}

for (const expired of [false, true]) {
  it(`D2b first cancellation budget includes delayed maintenance and finally cleanup (${expired ? "expired retains" : "V1 short cancel cleans"})`, async () => {
    const iid = expired ? 30302 : 30301;
    const claim = gitlabClaim(iid);
    let now = 10;
    let factories = 0;
    let budget: MaintenanceBudget | undefined;
    const executor: Executor = {
      async run(ctx: RunContext) {
        assert.ok(ctx.signal);
        const stopped = new Promise<void>(resolve => {
          ctx.signal!.addEventListener("abort", () => {
            assert.equal(factories, 1, "budget exists before model abort listener");
            now += expired ? 600_001 : 1;
            resolve();
          }, { once: true });
        });
        api.setInputs(claim.run_id, [{ id: 1, kind: "cancel" }, { id: 2, kind: "cancel" }]);
        await stopped;
        assert.equal(budget?.deadline, 600_010, "repeat cancellation cannot renew the first deadline");
        throw new Error("run cancelled");
      },
      killAgentTree() {},
    };
    const { gitlab } = fakeGitlab();
    const runner = runnerWith(() => ({ executor }), gitlab, undefined, undefined, {
      cancellationBudgetFactory: () => {
        factories++;
        return budget = new MaintenanceBudget(600_000, () => now);
      },
    });
    await runner.execute(claim);
    assert.equal(factories, 1);
    assert.equal(fs.existsSync(worktreeDirFor(iid)), expired);
    assert.ok(api.states.some(s => s.runId === claim.run_id && s.body.status === "failed"));
  });
}

for (const generation of [undefined, 8]) {
  it(`unproven cancellation generation ${generation} latches refusal without changing legacy deferral policy`, async () => {
    const claim = gitlabClaim(30304, { claim_generation: 7 });
    const budget = new MaintenanceBudget();
    let factories = 0, drains = 0;
    const cancel = new AbortController();
    const observe = RunRunner.prototype["observeCancellation"];
    const read = RunRunner.prototype["getObservedRunOwnership"];
    const dispose = RunRunner.prototype["disposeCredentialFreeOwnerCancel"];
    const flight = {
      runId: claim.run_id, claimGeneration: 7, cancel,
      steering: { markCancelled() {}, abortLifecycle() {} },
      executor: { settleForCredentialFreeCapture: async () => { drains++; return { kind: "observed_empty" }; } },
    } as unknown as Parameters<typeof observe>[0];
    const receiver = {
      cancellationBudgetFactory: () => { factories++; return budget; },
      client: { getRunOwnership: async () => ({ status: "cancelled", claim_generation: generation }) },
      git: { withRecoveryOperation: async (_signal: AbortSignal, _deadline: number, action: () => Promise<unknown>) => MaintenanceBudget.run(budget, action) },
      observeCancellation: observe,
      runCancellationMaintenance: RunRunner.prototype["runCancellationMaintenance"],
      cancellationExpired: RunRunner.prototype["cancellationExpired"],
    } as unknown as RunRunner;
    try {
      await read.call(receiver, flight);
      await read.call(receiver, flight);
      assert.equal(factories, 1);
      assert.equal(flight.cancellationCustodyUnknown, true);
      assert.equal(cancel.signal.aborted, true);
      assert.equal(await dispose.call(receiver, claim, flight), "retained");
      assert.equal(drains, 0, "an unproven generation never reaches a weaker preservation producer");
    } finally { budget.dispose(); }
  });
}

for (const testCase of [
  { current: 7, status: "failed", state: "open", settle: true },
  { current: 8, status: "failed", state: "open", settle: true },
  { current: 7, status: "running", state: "open", settle: false },
  // Earlier live generations keep the pre-existing archive path; held eligibility remains terminal-only.
  { current: 8, status: "running", state: "open", settle: true },
  { current: 8, status: "failed", state: "discarded", settle: false },
]) {
  it(`foreground exact generation 7 custody with current ${testCase.current}/${testCase.status}/${testCase.state}`, async () => {
    const claim = gitlabClaim(30305, { claim_generation: 7, inventory_guarded: true });
    claim.repo.id = "00000000-0000-4000-8000-000000000072";
    const holdId = "00000000-0000-4000-8000-000000000071";
    const settle = RunRunner.prototype["settleRecoveryGeneration"];
    const flight = { runId: claim.run_id, claimGeneration: 7, barePath: "/trusted/fixture.git" } as unknown as Parameters<typeof settle>[1];
    const calls: string[] = [];
    const receiver = {
      completionReceipts: new Map(), completionKey: () => "fixture:7",
      recovery: { enabled: true },
      client: {
        hasHeldPublicationV2: () => true,
        getRunOwnership: () => { throw new Error("current run belongs to a different worker"); },
        listRecoveryHolds: async () => ({ run_id: claim.run_id, holds: [{ hold_id: holdId, generation: 7, inventory_guarded: true }] }),
        getExactRecoveryHold: async (run: string, generation: number, binding: unknown) => {
          calls.push("exact");
          assert.equal(run, claim.run_id); assert.equal(generation, 7);
          assert.deepEqual(binding, { holdId, repoId: claim.repo.id });
          return { state: testCase.state, current_claim_generation: testCase.current, run_status: testCase.status };
        },
      },
      settleGuardedInventory: async (original: unknown, sourceFlight: unknown, _source: unknown, _body: unknown, _signal: unknown, quiescent: boolean) => {
        calls.push("settle"); assert.equal(original, claim); assert.equal(sourceFlight, flight); assert.equal(quiescent, true);
      },
    } as unknown as RunRunner;
    await settle.call(receiver, claim, flight, nullLogger());
    assert.deepEqual(calls, testCase.settle ? ["exact", "settle"] : ["exact"],
      "foreground settlement uses exact custody despite current-owner drift and keeps live current inventory protected");
  });
}

it("sticky cancellation metadata failure still stops model and lifecycle", () => {
  const cancel = new AbortController();
  let lifecycle = false;
  const observe = RunRunner.prototype["observeCancellation"];
  const budget = new MaintenanceBudget();
  const flight = { cancel, steering: {
    markCancelled() { throw new Error("metadata fixture"); },
    abortLifecycle() { lifecycle = true; },
  } } as unknown as Parameters<typeof observe>[0];
  const receiver = { cancellationBudgetFactory: () => budget } as unknown as RunRunner;
  try {
    assert.throws(() => observe.call(receiver, flight), /metadata fixture/);
    assert.equal(cancel.signal.aborted, true);
    assert.equal(lifecycle, true);
    assert.equal(flight.cancellationBudget, budget);
  } finally { budget.dispose(); }
});
