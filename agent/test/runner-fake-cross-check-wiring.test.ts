// Issue #4158: with FakeApi.modelCrossCheckRunningRule on, the fake api's cross-check running rule
// engages for every claim a runner executes. A claim handed straight
// to RunRunner.execute (no enqueueClaim, no claim route) must still be modelled, so the runner's
// own `running` report is refused for a human-gated run holding an unapproved stored plan and
// admitted once an applied approval bound to the current gate revision exists. Removing the
// RunRunner.prototype.execute wrapper in fake-api.ts turns the first test red.
import { describe, it } from "node:test";
import assert from "node:assert/strict";
import type { Executor } from "../src/executor.js";
import type { ClaimResponse } from "../src/protocol.js";
import { api, baseUrl, fakeGitlab, gitlabClaim, installHarness, runner, TOKEN } from "./runner-harness.js";

installHarness();

const PLAN = "# PLAN\n- stored human plan";
let nextIid = 41_580;

const exec: Executor = { run: async (ctx) => ({ branch: ctx.branch }) };

function gatedStoredPlanClaim(): ClaimResponse {
  return gitlabClaim(nextIid++, {
    claim_generation: 1, auto_approve: false, plan_cross_check_gated: true, plan_md: PLAN, resume_gate_revision: 1,
  });
}

async function receipt(runId: string, kind: "ack" | "applied", id: number): Promise<void> {
  const res = await fetch(`${baseUrl}/api/worker/runs/${runId}/inputs/${kind}`, {
    method: "POST",
    headers: { "Content-Type": "application/json", Authorization: `Bearer ${TOKEN}` },
    body: JSON.stringify({ ids: [id], claim_generation: 1 }),
  });
  assert.equal(res.status, 200, `${kind} receipt`);
}

describe("fake api cross-check wiring through RunRunner.execute", () => {
  it("refuses the running report of an unapproved stored plan", async () => {
    api.modelCrossCheckRunningRule = true;
    const c = gatedStoredPlanClaim();
    api.setInputClaimGeneration(c.run_id, 1);
    await runner(exec, fakeGitlab().gitlab).execute(c);
    const refusals = api.crossCheckRunningRefusals.filter((r) => r.runId === c.run_id);
    assert.ok(refusals.length > 0, "the fake refused at least one running report");
    assert.ok(!api.states.some((s) => s.runId === c.run_id && s.body.status === "running"), "no running report was recorded");
  });

  it("admits the running report after an applied bound approval for the current revision", async () => {
    api.modelCrossCheckRunningRule = true;
    const c = gatedStoredPlanClaim();
    api.setInputClaimGeneration(c.run_id, 1);
    const id = 41_999;
    api.appendInputs(c.run_id, [{ id, kind: "approve_plan", body: "{}", gate_binding: "bound", gate_revision: 1 }]);
    await receipt(c.run_id, "ack", id);
    await receipt(c.run_id, "applied", id);
    await runner(exec, fakeGitlab().gitlab).execute(c);
    assert.deepEqual(api.crossCheckRunningRefusals.filter((r) => r.runId === c.run_id), []);
    assert.ok(api.states.some((s) => s.runId === c.run_id && s.body.status === "running"), "a running report was recorded");
  });
});
