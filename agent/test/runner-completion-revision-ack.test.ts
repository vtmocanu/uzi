import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { nullLogger } from "./helpers.js";
import { StubExecutor } from "../src/executor.js";
import {
  api,
  fakeGitlab,
  git,
  gitlabClaim,
  installHarness,
  runner,
  runnerWith,
} from "./runner-harness.js";

installHarness();

// Issue #1626: a FRESH interlocked run's claim is assembled BEFORE its completion contract freezes
// (plan approval / the autopilot plan report), so `config.contract_revision` is absent, and the
// worker stays in-process across the plan gate with no re-claim. The /state ACK's
// RunDTO.completion_revision is the channel that brings the frozen revision in: the runner keeps
// the latest one any ACK carried and binds the finalize permit to it, ahead of the claim's value.
// Driven end-to-end through the REAL RunRunner.execute(), like runner-completion-permit.test.ts.

const H = "1111111111111111111111111111111111111111";

function statuses(runId: string): string[] {
  return api.states.filter((s) => s.runId === runId).map((s) => s.body.status);
}

describe("RunRunner — completion revision off the /state ACK (issue #1626)", () => {
  it("claim WITHOUT contract_revision + ACK carries completion_revision 1 → permit requested at revision 1, run completes", async () => {
    const { gitlab } = fakeGitlab({ head: H });
    const claim = gitlabClaim(1626, { config: { completion_contract_version: 1 } });
    // The contract freezes mid-run: the revision appears on the ACKs only from the first report on
    // (armed inside the hook, which runs before that report's ACK is sent).
    api.onState(claim.run_id, () => api.setStateAckCompletionRevision(claim.run_id, 1));
    api.setCompletionPermitResponse(true);
    git.trackingTip = (async () => H) as typeof git.trackingTip;

    await runner(new StubExecutor(nullLogger()), gitlab).execute(claim);

    assert.strictEqual(api.completionPermitRequests.length, 1, "the permit was requested (no fail-closed hold)");
    assert.strictEqual(api.completionPermitRequests[0]!.body.contract_revision, 1, "the ACK's revision rode the permit");
    assert.ok(statuses(claim.run_id).includes("completed"), "the run completed");
    assert.strictEqual(api.completionHoldRequests.length, 0, "no completion hold");
  });

  it("the LATEST ACK revision wins over the claim's (a revised contract)", async () => {
    const { gitlab } = fakeGitlab({ head: H });
    const claim = gitlabClaim(1627, { config: { completion_contract_version: 1, contract_revision: 1 } });
    api.setStateAckCompletionRevision(claim.run_id, 2);
    api.setCompletionPermitResponse(true);
    git.trackingTip = (async () => H) as typeof git.trackingTip;

    await runner(new StubExecutor(nullLogger()), gitlab).execute(claim);

    assert.strictEqual(api.completionPermitRequests.length, 1);
    assert.strictEqual(api.completionPermitRequests[0]!.body.contract_revision, 2, "the ACK's newer revision is echoed");
  });

  it("no revision on the claim NOR any ACK → still holds fail-closed (completion identity unresolvable)", async () => {
    const { gitlab, calls } = fakeGitlab({ head: H });
    const claim = gitlabClaim(1628, { config: { completion_contract_version: 1 } });
    api.setCompletionPermitResponse(true); // armed, but must never be reached
    git.trackingTip = (async () => H) as typeof git.trackingTip;

    await runnerWith(() => ({ executor: new StubExecutor(nullLogger()) }), gitlab, undefined, undefined, {
      recoveryRetryMs: 1,
    }).execute(claim);

    assert.strictEqual(api.completionPermitRequests.length, 0, "no permit without a resolvable revision");
    assert.strictEqual(calls.length, 0, "no MR is opened");
    assert.ok(!statuses(claim.run_id).includes("completed"), "never completes");
    assert.strictEqual(api.completionHoldRequests.length, 1, "holds instead");
  });
});
