// Issue #1673: the runner holds a resume report behind the applied receipt of the input that
// caused it. When that receipt keeps failing on the active claim, the run must fail cleanly
// rather than wait forever or send the resume as if the input were applied.
import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { nullLogger } from "./helpers.js";
import { StubExecutor } from "../src/executor.js";
import { api, fakeGitlab, gitlabClaim, input, installHarness, runner } from "./runner-harness.js";

installHarness();

describe("RunRunner — input receipts (issue #1673)", () => {
  it("fails the run, without a resume report, when the approval's APPLIED keeps failing", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(1673);
    // The owner approves once the gate is reported; that approval's applied receipt then fails.
    api.onState(claim.run_id, (body) => {
      if (body.status === "awaiting_approval") api.setInputs(claim.run_id, [input("approve_plan")]);
    });
    api.failInputReceipts("applied", 503);
    await runner(new StubExecutor(nullLogger(), { planGate: true }), gitlab).execute(claim);

    const states = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body);
    const statuses = states.map((s) => s.status);
    const gate = statuses.indexOf("awaiting_approval");
    assert.ok(gate >= 0, `the run reached the plan gate: ${statuses.join(",")}`);
    assert.deepStrictEqual(statuses.slice(gate + 1), ["failed"], "no resume report after the gate, then failed");
    assert.match(states.at(-1)?.failure_reason ?? "", /could not confirm 1 applied operator input/);
    assert.ok(
      api.inputReceiptCalls.filter((c) => c.runId === claim.run_id && c.kind === "applied").length >= 30,
      "the applied receipt was retried up to the active-claim bound",
    );
  });
});
