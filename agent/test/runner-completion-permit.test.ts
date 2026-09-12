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

/** PRD #1225 (CodeRabbit !1254): the reconcile rewrite is a PUT on GitLab (updateMergeRequestDescription).
 *  Return the LAST PUT's parsed body, so a success path that reconciles once (re-assert) and a hold path
 *  that reconciles once (unverified) both resolve to the write that actually landed on the MR. */
function mrPutBody(calls: { method: string; body?: string }[]): Record<string, unknown> {
  const puts = calls.filter((c) => c.method === "PUT");
  assert.ok(puts.length > 0, "the MR description was reconciled (a PUT reached the forge)");
  return JSON.parse(puts[puts.length - 1]!.body ?? "{}") as Record<string, unknown>;
}

describe("RunRunner — completion permit + PR-head verification (PRD #1226 M4 D5)", () => {
  it("granted + PR head matches H → opens the MR WITHOUT Closes, adds Closes only after verifying head H, and completes WITH head H", async () => {
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
    assert.doesNotMatch(String(body.description), /Closes #/, "an interlocked MR is created WITHOUT Closes; the closing line is added only after head verification");
    assert.ok(calls.some((c) => c.method === "GET"), "the PR head was read to verify it");
    // Completed WITH head H (the permit-bound head rides the terminal report).
    const done = completedBody(claim.run_id);
    assert.ok(done, "the run reported completed");
    assert.strictEqual(done!.head, H, "the completed report carries the permitted+verified head H");
    assert.strictEqual(done!.mr_iid, 42);
    // No hold was needed.
    assert.strictEqual(api.completionHoldRequests.length, 0, "a clean interlocked completion never holds");
    // PRD #1225 (CodeRabbit !1254): the verified completion is the ONLY place Closes is written — the
    // MR was created WITHOUT it, and the verified-head reconcile ADDS the canonical Closes body.
    const reassert = mrPutBody(calls);
    assert.match(String(reassert.description), /Closes #1300/, "the verified completion is the ONLY place Closes is written");
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
    assert.ok(!statuses(claim.run_id).includes("failed"), "a PR-head mismatch holds rather than fails");
    // PRD #1225 (CodeRabbit !1254): before holding, the MR body is reconciled to the unverified
    // variant so a human merge cannot close the issue for an unverified head — NO Closes, WITH banner.
    const reconciled = mrPutBody(calls);
    assert.doesNotMatch(String(reconciled.description), /Closes #/, "the held MR no longer carries Closes");
    assert.match(String(reconciled.description), /Completion unverified/, "the held MR body warns the completion is unverified");
  });

  it("granted but the PR head read THROWS → MR is created, run holds, and the body is reconciled off Closes", async () => {
    // A non-200 single-item GET makes getMergeRequestHead throw a ForgeError (the "cannot verify"
    // branch), distinct from the value-mismatch branch above.
    const { gitlab, calls } = fakeGitlab({ head: H, headStatus: 404 });
    const claim = interlockedClaim(1305);
    api.setCompletionPermitResponse(true);
    git.trackingTip = (async () => H) as typeof git.trackingTip;

    await runnerWith(() => ({ executor: new StubExecutor(nullLogger()) }), gitlab, undefined, undefined, {
      recoveryRetryMs: 1,
    }).execute(claim);

    // The MR exists (create-then-verify) and the head read was attempted (a GET) before it threw.
    assert.ok(calls.some((c) => c.method === "POST"), "the MR was created before the head verify");
    assert.ok(calls.some((c) => c.method === "GET"), "the PR head read was attempted");
    // An unreadable head must not report completed; the run holds.
    assert.ok(!statuses(claim.run_id).includes("completed"), "an unreadable PR head never completes");
    assert.strictEqual(api.completionHoldRequests.length, 1, "an unreadable PR head holds the run");
    assert.ok(!statuses(claim.run_id).includes("failed"), "an unreadable PR head holds rather than fails");
    // PRD #1225 (CodeRabbit !1254): before holding, the MR body is reconciled off Closes with the banner.
    const reconciled = mrPutBody(calls);
    assert.doesNotMatch(String(reconciled.description), /Closes #/, "the held MR no longer carries Closes");
    assert.match(String(reconciled.description), /Completion unverified/, "the held MR body warns the completion is unverified");
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

  it("granted + head matches H but the add-Closes reconcile FAILS → MR holds (no Closes), never completes", async () => {
    const { gitlab, calls } = fakeGitlab({ head: H, putStatus: 404 });
    const claim = interlockedClaim(1306);
    api.setCompletionPermitResponse(true);
    git.trackingTip = (async () => H) as typeof git.trackingTip;

    await runnerWith(() => ({ executor: new StubExecutor(nullLogger()) }), gitlab, undefined, undefined, {
      recoveryRetryMs: 1,
    }).execute(claim);

    const body = mrPostBody(calls);
    assert.doesNotMatch(String(body.description), /Closes #/, "the interlocked MR is created WITHOUT Closes");
    assert.ok(calls.some((c) => c.method === "GET"), "the PR head was read to verify it");
    assert.ok(calls.some((c) => c.method === "PUT"), "the add-Closes reconcile was attempted");
    assert.ok(!statuses(claim.run_id).includes("completed"), "a failed add-Closes never completes");
    assert.ok(!statuses(claim.run_id).includes("failed"), "it holds rather than fails");
    assert.strictEqual(api.completionHoldRequests.length, 1, "it enters the completion hold");
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
