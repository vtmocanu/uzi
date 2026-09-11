import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { nullLogger } from "./helpers.js";
import { StubExecutor } from "../src/executor.js";
import type { StateRequest } from "../src/protocol.js";
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

// PRD #1226 M4 (D5): the completion permit + exact-head PR verification, driven end-to-end through
// the REAL RunRunner.execute() (clone seed → StubExecutor commits real work → phasePublish pushes to
// origin → interlocked permit/verify block). The StubExecutor commits and returns, so the finalize
// push really lands; `git.trackingTip` is stubbed to a fixed H so the landed head is deterministic
// (the push itself never reads trackingTip — pushBranch pushes refs/uzi-runner/<branch> directly —
// so stubbing it does not disturb the real push). The fake forge answers getMergeRequestHead off its
// GET; the FakeApi permit route answers a scripted grant/deny.

const H = "1111111111111111111111111111111111111111";
const OTHER = "2222222222222222222222222222222222222222";

/** An INTERLOCKED gitlab issue claim: the worker-only config carries the completion-contract
 *  discriminator + frozen revision, exactly as claim_assembly delivers for a rollout-on issue run. */
function interlockedClaim(iid: number) {
  return gitlabClaim(iid, {
    config: { completion_contract_version: 1, contract_revision: 1 },
  });
}

function statuses(runId: string): string[] {
  return api.states.filter((s) => s.runId === runId).map((s) => s.body.status);
}

function completedBody(runId: string): StateRequest | undefined {
  return api.states.find((s) => s.runId === runId && s.body.status === "completed")?.body;
}

function mrPostBody(calls: { method: string; body?: string }[]): Record<string, unknown> {
  const post = calls.find((c) => c.method === "POST");
  assert.ok(post, "an MR was created (a POST reached the forge)");
  return JSON.parse(post.body ?? "{}") as Record<string, unknown>;
}

describe("RunRunner — completion permit + PR-head verification (PRD #1226 M4 D5)", () => {
  it("granted + PR head matches H → opens the MR with Closes, and completes WITH head H", async () => {
    const { gitlab, calls } = fakeGitlab({ head: H });
    const claim = interlockedClaim(1300);
    api.setCompletionPermitResponse(true);
    git.trackingTip = (async () => H) as typeof git.trackingTip;

    await runner(new StubExecutor(nullLogger()), gitlab).execute(claim);

    // The permit was requested with the exact (contract_revision, branch, head=H) identity.
    assert.strictEqual(api.completionPermitRequests.length, 1, "the permit was requested exactly once");
    assert.strictEqual(api.completionPermitRequests[0]!.body.head, H, "the landed head H rode the permit request");
    assert.strictEqual(api.completionPermitRequests[0]!.body.contract_revision, 1);
    assert.strictEqual(api.completionPermitRequests[0]!.body.branch, "agent/issue-1300");
    // The MR was created and the PR head was read back (a GET) to verify it.
    const body = mrPostBody(calls);
    assert.match(String(body.description), /Closes #1300/, "a granted permit renders Closes");
    assert.ok(calls.some((c) => c.method === "GET"), "the PR head was read to verify it");
    // Completed WITH head H (the permit-bound head rides the terminal report).
    const done = completedBody(claim.run_id);
    assert.ok(done, "the run reported completed");
    assert.strictEqual(done!.head, H, "the completed report carries the permitted+verified head H");
    assert.strictEqual(done!.mr_iid, 42);
    // No hold was needed.
    assert.strictEqual(api.completionHoldRequests.length, 0, "a clean interlocked completion never holds");
  });

  it("permit DENIED → never creates the MR, never completes, holds the run", async () => {
    const { gitlab, calls } = fakeGitlab({ head: H });
    const claim = interlockedClaim(1301);
    api.setCompletionPermitResponse(false, { denyReason: "missing_milestones" });
    // The hold ACK defaults to paused/200, so enterCompletionHold parks the run.
    git.trackingTip = (async () => H) as typeof git.trackingTip;

    await runnerWith(() => ({ executor: new StubExecutor(nullLogger()) }), gitlab, undefined, undefined, {
      recoveryRetryMs: 1,
    }).execute(claim);

    assert.strictEqual(api.completionPermitRequests.length, 1, "the permit was requested");
    assert.strictEqual(calls.length, 0, "a denied permit opens NO MR and reads NO PR head");
    assert.ok(!statuses(claim.run_id).includes("completed"), "a denied permit never completes");
    assert.ok(!statuses(claim.run_id).includes("failed"), "a denied permit holds, not fails");
    assert.strictEqual(api.completionHoldRequests.length, 1, "the run entered the completion hold");
  });

  it("granted but PR head != H → MR is created but the run does NOT complete; it holds", async () => {
    const { gitlab, calls } = fakeGitlab({ head: OTHER });
    const claim = interlockedClaim(1302);
    api.setCompletionPermitResponse(true);
    git.trackingTip = (async () => H) as typeof git.trackingTip;

    await runnerWith(() => ({ executor: new StubExecutor(nullLogger()) }), gitlab, undefined, undefined, {
      recoveryRetryMs: 1,
    }).execute(claim);

    // The MR exists (create-then-verify), and the head was read.
    assert.ok(calls.some((c) => c.method === "POST"), "the MR was created before the head verify");
    assert.ok(calls.some((c) => c.method === "GET"), "the PR head was read");
    // A head mismatch must not report completed; the run holds.
    assert.ok(!statuses(claim.run_id).includes("completed"), "a PR-head mismatch never completes");
    assert.strictEqual(api.completionHoldRequests.length, 1, "a PR-head mismatch holds the run");
  });

  it("permit request THROWS (transport/HTTP error) → the run does not falsely complete", async () => {
    const { gitlab, calls } = fakeGitlab({ head: H });
    const claim = interlockedClaim(1303);
    api.setCompletionPermitResponse(false, { httpStatus: 500 }); // the client throws on a non-200
    git.trackingTip = (async () => H) as typeof git.trackingTip;

    await runner(new StubExecutor(nullLogger()), gitlab).execute(claim);

    assert.strictEqual(api.completionPermitRequests.length, 1, "the permit was attempted");
    assert.strictEqual(calls.length, 0, "no MR is opened when the permit request errors");
    assert.ok(!statuses(claim.run_id).includes("completed"), "a permit-request error never completes");
    // A transport error propagates to the generic terminal path; the run fails rather than completing.
    assert.ok(statuses(claim.run_id).includes("failed"), "the run fails on a permit transport error");
    assert.strictEqual(api.completionHoldRequests.length, 0, "a thrown permit request does not enter the hold");
  });

  it("LEGACY (non-interlocked) run → no permit, no PR-head read, Closes as before, completes with NO head", async () => {
    const { gitlab, calls } = fakeGitlab({ head: H });
    const claim = gitlabClaim(1304); // no config ⇒ legacy
    api.setCompletionPermitResponse(true); // armed, but must never be reached

    await runner(new StubExecutor(nullLogger()), gitlab).execute(claim);

    // The interlock is entirely inert for a legacy run.
    assert.strictEqual(api.completionPermitRequests.length, 0, "a legacy run never requests a permit");
    assert.ok(!calls.some((c) => c.method === "GET"), "a legacy run never reads the PR head");
    // Closes renders exactly as before, and the completed report carries NO head.
    const body = mrPostBody(calls);
    assert.match(String(body.description), /Closes #1304/, "the legacy Closes renders unchanged");
    const done = completedBody(claim.run_id);
    assert.ok(done, "the legacy run completed");
    assert.strictEqual(done!.head, undefined, "a legacy completed report carries NO head (byte-for-byte legacy)");
    assert.strictEqual(done!.mr_iid, 42);
  });
});
