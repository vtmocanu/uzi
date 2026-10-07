import { followSuccessfulPush } from "./publication-fixture.js";
import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { nullLogger } from "./helpers.js";
import { COMPLETION_START, COMPLETION_END, REGION_START, REGION_END, parseOwnedBlocks } from "../src/pr-description.js";
import { FakePrDescApi } from "./fake-pr-desc-api.js";
import { StubExecutor } from "../src/executor.js";
import type { StateRequest } from "../src/protocol.js";
import {
  api,
  fakeGitlab as rawFakeGitlab,
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
// push really lands; the fake forge follows the exact successful push candidate.
// The fake forge answers getMergeRequestHead off its
// GET; the FakeApi permit route answers a scripted grant/deny.

let pushedSha = "";
function fakeGitlab(opts: Parameters<typeof rawFakeGitlab>[0] = {}) {
  const forge = rawFakeGitlab(opts);
  const source = followSuccessfulPush(git, forge.pr, opts.head === undefined || opts.head === H);
  const push = git.pushBranch.bind(git);
  git.pushBranch = async (...args) => {
    await push(...args);
    pushedSha = source();
  };
  return forge;
}

const H = "1111111111111111111111111111111111111111";
const OTHER = "2222222222222222222222222222222222222222";

/** An INTERLOCKED gitlab issue claim: the worker-only config carries the completion-contract
 *  discriminator + frozen revision, exactly as claim_assembly delivers for a rollout-on issue run. */
function interlockedClaim(iid: number) {
  return gitlabClaim(iid, {
    config: { completion_contract_version: 1, contract_revision: 1 },
  });
}

/** PRD #1227 M2: an INTERLOCKED gitlab issue claim whose FROZEN contract carries an owner PARTIAL
 *  decision (≥1 deferred milestone), exactly as claim_assembly delivers for a scope_reduced run. The
 *  worker must open + verify the MR as a partial that NEVER closes — Closes is not added even after
 *  head verification (reconcileMrDescription(!isOwnerPartial) with isOwnerPartial=true). */
function ownerPartialClaim(iid: number) {
  return gitlabClaim(iid, {
    config: {
      completion_contract_version: 1,
      contract_revision: 1,
      completion_scope: {
        deferred: [{ milestone_id: "m3", title: "Third milestone", reason: "deprioritized" }],
      },
    },
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
    const { gitlab, calls, all } = fakeGitlab({ head: H });
    const claim = interlockedClaim(1300);
    api.setCompletionPermitResponse(true);


    await runner(new StubExecutor(nullLogger()), gitlab).execute(claim);

    // The permit was requested with the exact (contract_revision, branch, head=H) identity.
    assert.strictEqual(api.completionPermitRequests.length, 1, "the permit was requested exactly once");
    assert.strictEqual(api.completionPermitRequests[0]!.body.head, pushedSha, "the landed head H rode the permit request");
    assert.strictEqual(api.completionPermitRequests[0]!.body.contract_revision, 1);
    assert.strictEqual(api.completionPermitRequests[0]!.body.branch, "agent/issue-1300");
    // The MR was created and the PR head was read back (a GET) to verify it.
    const body = mrPostBody(calls);
    assert.doesNotMatch(String(body.description), /Closes #/, "an interlocked MR is created WITHOUT Closes; the closing line is added only after head verification");
    assert.ok(all.some((c) => c.method === "GET"), "the PR head was read to verify it");
    // Completed WITH head H (the permit-bound head rides the terminal report).
    const done = completedBody(claim.run_id);
    assert.ok(done, "the run reported completed");
    assert.strictEqual(done!.head, pushedSha, "the completed report carries the permitted+verified head H");
    assert.strictEqual(done!.mr_iid, 42);
    // No hold was needed.
    assert.strictEqual(api.completionHoldRequests.length, 0, "a clean interlocked completion never holds");
    // PRD #1225 (CodeRabbit !1254): the verified completion is the ONLY place Closes is written — the
    // MR was created WITHOUT it, and the verified-head reconcile ADDS the canonical Closes body.
    const reassert = mrPutBody(calls);
    assert.match(String(reassert.description), /Closes #1300/, "the verified completion is the ONLY place Closes is written");
    // CodeRabbit !1254: assert the COMPLETE forge-call sequence, not just that each kind occurred —
    // create (no Closes) → verify head → add Closes → RE-VERIFY head. A wrong ordering (e.g. adding
    // Closes before the verify, or skipping the post-add re-verify) would leave the per-kind checks
    // above green while breaking the head bind.
    // PRD #1798 M6: the description publisher reads the new PR twice (read, revalidate) and writes
    // nothing when the body it created is current; the reconcile then reads, writes and re-reads.
    assert.deepStrictEqual(
      all.map((c) => c.method),
      ["POST", "GET", "GET", "GET", "GET", "PUT", "GET", "GET"],
      "forge calls run in the order: create MR (POST) → publisher read + revalidate (GET, GET) → verify head (GET) → reconcile read (GET) → add Closes (PUT) → reconcile confirm (GET) → re-verify head (GET)",
    );
  });

  it("OWNER PARTIAL (PRD #1227 M2): creates a [partial] MR with NO Closes, verifies head H, and the post-verify PUT STILL has no Closes", async () => {
    const { gitlab, calls, all, pr } = fakeGitlab({ head: H });
    const claim = ownerPartialClaim(1310);
    api.setCompletionPermitResponse(true);


    await runner(new StubExecutor(nullLogger()), gitlab).execute(claim);

    // The permit is requested and the head is verified exactly like a full delivery.
    assert.strictEqual(api.completionPermitRequests.length, 1, "a partial run still requests the permit");
    // The MR is created as a [partial] with NO Closes and the deferred milestone listed.
    const post = mrPostBody(calls);
    assert.match(String(post.title), /^\[partial\] /, "an owner partial MR title is prefixed [partial]");
    assert.doesNotMatch(String(post.description), /Closes #/, "an owner partial MR is created WITHOUT Closes");
    assert.match(String(post.description), /owner scope decision/, "the created body states the partial delivery");
    assert.match(String(post.description), /m3.*Third milestone.*deprioritized/, "the deferred milestone is named with its owner reason");
    // The head is verified (permit binding) — the partial does not skip verification.
    assert.ok(all.some((c) => c.method === "GET"), "the PR head is still verified for a partial");
    // CRITICAL (PRD #1227 M2): the post-verify reconcile PUT re-renders the NON-closing partial body —
    // reconcileMrDescription(!isOwnerPartial) with isOwnerPartial=true → withCloses=false. The verified
    // head does NOT add Closes for a partial, contrasting the non-partial test above which DOES.
    // PRD #1798 M6: the reconcile writes only when the completion block differs, and the partial's
    // block is already the non-closing one it was created with, so the check is on the PR as it
    // stands after the verified-head reconcile (the reconcile re-read it and found its block).
    assert.ok(!calls.some((c) => c.method === "PUT"), "the non-closing partial block was already current: no rewrite");
    assert.doesNotMatch(pr.description, /Closes #/, "a partial NEVER adds Closes even after head verification");
    assert.match(pr.description, /owner scope decision/, "the reconciled body is still the partial-delivery body");
    // The run completes (the head verified) and carries head H, like any granted interlocked run.
    const done = completedBody(claim.run_id);
    assert.ok(done, "an owner partial run still completes on a verified head");
    assert.strictEqual(done!.head, pushedSha, "the completed report carries the permitted+verified head H");
    assert.strictEqual(api.completionHoldRequests.length, 0, "a verified owner partial never holds");
  });

  it("permit DENIED → never creates the MR, never completes, holds the run", async () => {
    const { gitlab, all } = fakeGitlab({ head: H });
    const claim = interlockedClaim(1301);
    api.setCompletionPermitResponse(false, { denyReason: "missing_milestones" });
    // The hold ACK defaults to paused/200, so enterCompletionHold parks the run.


    await runnerWith(() => ({ executor: new StubExecutor(nullLogger()) }), gitlab, undefined, undefined, {
      recoveryRetryMs: 1,
    }).execute(claim);

    assert.strictEqual(api.completionPermitRequests.length, 1, "the permit was requested");
    assert.strictEqual(all.length, 0, "a denied permit opens NO MR and reads NO PR head");
    assert.ok(!statuses(claim.run_id).includes("completed"), "a denied permit never completes");
    assert.ok(!statuses(claim.run_id).includes("failed"), "a denied permit holds, not fails");
    assert.strictEqual(api.completionHoldRequests.length, 1, "the run entered the completion hold");
  });

  it("granted but PR head != H → MR is created but the run does NOT complete; it holds", async () => {
    const { gitlab, calls, all } = fakeGitlab({ head: OTHER });
    const claim = interlockedClaim(1302);
    api.setCompletionPermitResponse(true);


    await runnerWith(() => ({ executor: new StubExecutor(nullLogger()) }), gitlab, undefined, undefined, {
      recoveryRetryMs: 1,
    }).execute(claim);

    // The MR exists (create-then-verify), and the head was read.
    assert.ok(calls.some((c) => c.method === "POST"), "the MR was created before the head verify");
    assert.ok(all.some((c) => c.method === "GET"), "the PR head was read");
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
    const { gitlab, calls, all } = fakeGitlab({ head: H, headStatus: 404 });
    const claim = interlockedClaim(1305);
    api.setCompletionPermitResponse(true);


    await runnerWith(() => ({ executor: new StubExecutor(nullLogger()) }), gitlab, undefined, undefined, {
      recoveryRetryMs: 1,
    }).execute(claim);

    // The MR exists (create-then-verify) and the head read was attempted (a GET) before it threw.
    assert.ok(calls.some((c) => c.method === "POST"), "the MR was created before the head verify");
    assert.ok(all.some((c) => c.method === "GET"), "the PR head read was attempted");
    // An unreadable head must not report completed; the run holds.
    assert.ok(!statuses(claim.run_id).includes("completed"), "an unreadable PR head never completes");
    assert.strictEqual(api.completionHoldRequests.length, 1, "an unreadable PR head holds the run");
    assert.ok(!statuses(claim.run_id).includes("failed"), "an unreadable PR head holds rather than fails");
    // PRD #1225 (CodeRabbit !1254): before holding, the MR body is reconciled off Closes with the banner.
    const reconciled = mrPutBody(calls);
    assert.doesNotMatch(String(reconciled.description), /Closes #/, "the held MR no longer carries Closes");
    assert.match(String(reconciled.description), /Completion unverified/, "the held MR body warns the completion is unverified");
  });

  it("permit request THROWS (permanent HTTP error) → the run does not falsely complete", async () => {
    const { gitlab, calls } = fakeGitlab({ head: H });
    const claim = interlockedClaim(1303);
    // A PERMANENT error (409) is thrown at once; a transient one is retried (next test).
    api.setCompletionPermitResponse(false, { httpStatus: 409 });


    await runner(new StubExecutor(nullLogger()), gitlab).execute(claim);

    assert.strictEqual(api.completionPermitRequests.length, 1, "the permit was attempted");
    assert.strictEqual(calls.length, 0, "no MR is opened when the permit request errors");
    assert.ok(!statuses(claim.run_id).includes("completed"), "a permit-request error never completes");
    // A permanent error propagates to the generic terminal path; the run fails rather than completing.
    assert.ok(statuses(claim.run_id).includes("failed"), "the run fails on a permanent permit error");
    assert.strictEqual(api.completionHoldRequests.length, 0, "a thrown permit request does not enter the hold");
  });

  it("permit request hits an api OUTAGE (transient 5xx) → the run waits it out, then completes (e2e phase 52 regression)", async () => {
    const { gitlab, calls } = fakeGitlab({ head: H });
    const claim = interlockedClaim(1307);
    // Three 503s (the api down at finalize), then the api answers and grants.
    api.setCompletionPermitResponse(true, { httpStatus: 503, failTimes: 3 });


    await runner(new StubExecutor(nullLogger()), gitlab).execute(claim);

    assert.strictEqual(api.completionPermitRequests.length, 4, "three failed attempts, then the granted one");
    assert.ok(calls.some((c) => c.method === "POST"), "the MR is opened once the permit is granted");
    assert.ok(statuses(claim.run_id).includes("completed"), "the run completes after the outage");
    assert.ok(!statuses(claim.run_id).includes("failed"), "an api outage at finalize never fails the run");
    assert.strictEqual(api.completionHoldRequests.length, 0, "a transient outage is not a hold");
  });

  it("granted + head matches H but the add-Closes reconcile FAILS → strips Closes first; the strip fails too, so it fails CLOSED, never completes (ADR 1225)", async () => {
    const { gitlab, calls, all } = fakeGitlab({ head: H, putStatus: 404 });
    const claim = interlockedClaim(1306);
    api.setCompletionPermitResponse(true);


    await runnerWith(() => ({ executor: new StubExecutor(nullLogger()) }), gitlab, undefined, undefined, {
      recoveryRetryMs: 1,
    }).execute(claim);

    const body = mrPostBody(calls);
    assert.doesNotMatch(String(body.description), /Closes #/, "the interlocked MR is created WITHOUT Closes");
    assert.ok(all.some((c) => c.method === "GET"), "the PR head was read to verify it");
    assert.ok(calls.some((c) => c.method === "PUT"), "the add-Closes reconcile was attempted");
    assert.ok(!statuses(claim.run_id).includes("completed"), "a failed add-Closes never completes");
    // PRD #1798 M6 (ADR 1225): an add that may have landed is never held as-is: the hold strips
    // Closes first, and a strip that cannot be written (the same 404) cannot prove the MR is
    // non-closing, so the run fails closed instead of holding.
    assert.ok(calls.filter((c) => c.method === "PUT").length >= 2, "the strip was attempted after the failed add");
    assert.strictEqual(api.completionHoldRequests.length, 0, "never holds a possibly-closing MR");
    assert.ok(statuses(claim.run_id).includes("failed"), "fails closed");
  });

  it("head matches at verify but CHANGES before the post-add re-verify → strips Closes and holds (bind to verified head, CodeRabbit !1254)", async () => {
    // The verify read answers H so the add proceeds; the add-Closes write moves the head to OTHER, so the
    // post-add re-verify reads OTHER, modelling a head change in the read→add window. Closes must not be
    // left on the changed head.
    // PRD #1798 M6: run WITH the description api (the production configuration), on the REAL landed
    // head, so the publisher stages, binds and publishes a version and the completion block carries a
    // described SHA (the staleness line a moved head changes).
    const prApi = new FakePrDescApi();
    api.prDescription = prApi;
    const { gitlab, calls, all } = fakeGitlab({
      onWrite: (p, description) => {
        if (/Closes #1307/.test(description)) p.head = OTHER;
      },
    });

    const claim = interlockedClaim(1307);
    api.setCompletionPermitResponse(true);

    await runnerWith(() => ({ executor: new StubExecutor(nullLogger()) }), gitlab, undefined, undefined, {
      recoveryRetryMs: 1,
    }).execute(claim);

    // The PR is read by the publisher (read, revalidate), the head verify, the add-Closes reconcile
    // (read, confirm), the post-add re-verify, and the strip reconcile (read, confirm).
    assert.strictEqual(
      all.filter((c) => c.method === "GET").length,
      8,
      "the head is read at verify and again after the add (the re-verify)",
    );
    assert.ok(!statuses(claim.run_id).includes("completed"), "a head change after adding Closes never completes");
    assert.strictEqual(api.completionHoldRequests.length, 1, "a post-add head change holds the run");
    assert.ok(!statuses(claim.run_id).includes("failed"), "a post-add head change holds rather than fails");
    // The LAST reconcile strips Closes back off and adds the unverified banner.
    const reconciled = mrPutBody(calls);
    assert.doesNotMatch(String(reconciled.description), /Closes #/, "the held MR no longer carries Closes after the head change");
    assert.match(String(reconciled.description), /Completion unverified/, "the held MR body warns the completion is unverified");
    assert.deepStrictEqual(prApi.acks(), ["published"], "the publisher really published a version");
  });

  it("PR head != H AND the non-closing strip write FAILS → fails CLOSED (never holds a possibly-closing MR, CodeRabbit !1254)", async () => {
    // An adopted MR may already carry Closes; if the strip-to-non-closing write fails on the hold path
    // we cannot prove the MR is non-closing, so the run must FAIL rather than park a possibly-closing MR.
    // A permanent write refusal (403): a 5xx would be retried by the forge retry schedule first.
    const { gitlab, calls } = fakeGitlab({ head: OTHER, putStatus: 403 });
    const claim = interlockedClaim(1308);
    api.setCompletionPermitResponse(true);


    await runnerWith(() => ({ executor: new StubExecutor(nullLogger()) }), gitlab, undefined, undefined, {
      recoveryRetryMs: 1,
    }).execute(claim);

    assert.ok(calls.some((c) => c.method === "PUT"), "the non-closing strip write was attempted");
    assert.ok(!statuses(claim.run_id).includes("completed"), "it never completes");
    assert.strictEqual(api.completionHoldRequests.length, 0, "it does NOT enter the hold when the strip failed");
    assert.ok(statuses(claim.run_id).includes("failed"), "it fails closed instead of holding");
  });

  it("LEGACY (non-interlocked) run → no permit, no PR-head read, Closes as before, completes with NO head", async () => {
    const { gitlab, calls, pr } = fakeGitlab({ head: H });
    const claim = gitlabClaim(1304); // no config ⇒ legacy
    api.setCompletionPermitResponse(true); // armed, but must never be reached

    await runner(new StubExecutor(nullLogger()), gitlab).execute(claim);

    // The interlock is entirely inert for a legacy run.
    assert.strictEqual(api.completionPermitRequests.length, 0, "a legacy run never requests a permit");
    // PRD #1798 M6: the description publisher reads the PR (D11) and may refresh its region (here the
    // fake's head differs from the landed one, so it regenerates for it), but nothing verifies the head
    // and the completion block is the one the MR was created with.
    const created = parseOwnedBlocks(String(mrPostBody(calls).description));
    const now = parseOwnedBlocks(pr.description);
    assert.ok(created.kind === "ok" && now.kind === "ok");
    assert.equal(now.kind === "ok" && now.completion, created.kind === "ok" && created.completion, "a legacy run never reconciles the completion block");
    // Closes renders exactly as before, and the completed report carries NO head.
    const body = mrPostBody(calls);
    assert.match(String(body.description), /Closes #1304/, "the legacy Closes renders unchanged");
    const done = completedBody(claim.run_id);
    assert.ok(done, "the legacy run completed");
    assert.strictEqual(done!.head, undefined, "a legacy completed report carries NO head (byte-for-byte legacy)");
    assert.strictEqual(done!.mr_iid, 42);
  });
});


function cappedExecutor(): StubExecutor {
  const executor = new StubExecutor(nullLogger());
  const run = executor.run.bind(executor);
  executor.run = async (ctx) => ({ ...await run(ctx), scopeCapped: { completedCount: 1, total: 3 } });
  return executor;
}

describe("interlocked capped MR delivery", () => {
  for (const adopted of [false, true]) {
    it(`opts into a capped permit and preserves a non-closing partial MR (adopted ${adopted})`, async () => {
      const claim = interlockedClaim(1320);
      const forge = fakeGitlab({ head: H, ...(adopted ? { existing: ["Human notes", REGION_START, "Old size", REGION_END, COMPLETION_START, "Closes #1320", COMPLETION_END].join("\n\n") } : {}) });
      api.setCompletionPermitResponse(true);

      await runner(cappedExecutor(), forge.gitlab).execute(claim);
      assert.equal(api.completionPermitRequests.length, 1);
      assert.equal(api.completionPermitRequests[0]!.body.scope_capped, true);
      const created = mrPostBody(forge.calls);
      assert.match(String(created.title), /^\[partial\] /);
      assert.doesNotMatch(String(created.description), /Closes #/);
      assert.doesNotMatch(forge.pr.description, /Closes #1320/);
      assert.match(forge.pr.description, /Implements part of #1320/);
      if (adopted) {
        assert.match(forge.pr.description, /Human notes/);
        assert.ok(forge.calls.some((c) => c.method === "PUT"), "the adopted closing line was stripped");
      }
      assert.ok(forge.reads.length > 0, "head verification still runs");
      assert.equal(completedBody(claim.run_id)?.head, pushedSha);
      assert.equal(completedBody(claim.run_id)?.scope_capped, true);
      assert.equal(api.completionHoldRequests.length, 0);
    });
  }

  for (const denied of [false, true]) {
    it(`holds a capped MR delivery on ${denied ? "permit denial" : "head mismatch"}`, async () => {
      const claim = interlockedClaim(1321);
      const forge = fakeGitlab({ head: OTHER });
      api.setCompletionPermitResponse(!denied, { denyReason: "scope_cap_not_reached" });

      await runnerWith(() => ({ executor: cappedExecutor() }), forge.gitlab, undefined, undefined, { recoveryRetryMs: 1 }).execute(claim);
      assert.equal(api.completionPermitRequests[0]!.body.scope_capped, true);
      assert.equal(completedBody(claim.run_id), undefined);
      assert.equal(api.completionHoldRequests.length, 1);
      if (denied) assert.equal(forge.all.length, 0);
      else assert.doesNotMatch(forge.pr.description, /Closes #/);
    });
  }
});
