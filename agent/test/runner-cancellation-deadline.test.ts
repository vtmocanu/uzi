import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import { MaintenanceBudget } from "../src/maintenance-budget.js";
import { RunRunner } from "../src/runner.js";
import type { Executor, RunContext } from "../src/executor.js";
import { api, fakeGitlab, git, gitlabClaim, installHarness, runnerWith, worktreeDirFor } from "./runner-harness.js";

installHarness();


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
