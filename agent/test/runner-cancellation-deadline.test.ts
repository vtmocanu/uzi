import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import { MaintenanceBudget } from "../src/maintenance-budget.js";
import { RunRunner } from "../src/runner.js";
import type { Executor, RunContext } from "../src/executor.js";
import { api, fakeGitlab, gitlabClaim, installHarness, runnerWith, worktreeDirFor } from "./runner-harness.js";

installHarness();


it("cancellation observed in an active maintenance scope reuses its original budget", async () => {
  const budget = new MaintenanceBudget();
  const cancel = new AbortController();
  let lifecycle = false;
  const observe = RunRunner.prototype["observeCancellation"];
  const flight = { cancel, steering: { abortLifecycle: () => { lifecycle = true; } } } as Parameters<typeof observe>[0];
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
  const flight = { cancel, steering: { abortLifecycle: () => { lifecycle = true; } } } as Parameters<typeof observe>[0];
  const receiver = { cancellationBudgetFactory: () => { throw new Error("factory fixture"); } } as unknown as RunRunner;
  assert.throws(() => observe.call(receiver, flight), /factory fixture/);
  assert.equal(cancel.signal.aborted, true);
  assert.equal(lifecycle, true);
});

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
