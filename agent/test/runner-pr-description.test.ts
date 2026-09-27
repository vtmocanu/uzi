import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import { nullLogger, makeClaim } from "./helpers.js";
import { StubExecutor } from "../src/executor.js";
import {
  COMPLETION_END,
  COMPLETION_START,
  REGION_END,
  REGION_START,
  closingDirectiveFor,
  parseOwnedBlocks,
} from "../src/pr-description.js";
import type { ClaimResponse } from "../src/protocol.js";
import { FakePrDescApi } from "./fake-pr-desc-api.js";
import { api, fakeGitlab, fx, git, gitlabClaim, installHarness, runner, runnerWith } from "./runner-harness.js";

installHarness();

// PRD #1798 M6 (M6b): the description publisher and the completion interlock, end to end through the
// REAL RunRunner.execute() (StubExecutor commits real work, phasePublish pushes it), against a fake
// forge whose single PR keeps its description across reads and writes.

const H = "1111111111111111111111111111111111111111";
const OTHER = "2222222222222222222222222222222222222222";

function interlockedClaim(iid: number, scope?: { deferred: { milestone_id: string; title: string; reason: string }[] }) {
  return gitlabClaim(iid, {
    config: { completion_contract_version: 1, contract_revision: 1, ...(scope ? { completion_scope: scope } : {}) },
  });
}

function statuses(runId: string): string[] {
  return api.states.filter((s) => s.runId === runId).map((s) => s.body.status);
}

const SIZE_REGION = [REGION_START, "**Size:** code +1 −0 · 1 file", REGION_END].join("\n");

/** An adopted PR's body: uzi's (older) blocks with human text around them. */
function adoptedBody(iid: number, humanText: string): string {
  const completion = [COMPLETION_START, `Related to #${iid}.`, "", "---", "Opened by uzi.", COMPLETION_END].join("\n");
  return `${humanText}\n\n${SIZE_REGION}\n\n${completion}\n\nA review bot's summary.`;
}

describe("RunRunner — completion interlock over a preserved body (PRD #1798 M6, amended D10)", () => {
  it("hold path: a human-typed `Closes #N` outside uzi's blocks forces a whole-body non-closing rewrite, then holds", async () => {
    const { gitlab, pr } = fakeGitlab({ head: OTHER, existing: adoptedBody(1820, "Closes #1820 when merged, please.") });
    const claim = interlockedClaim(1820);
    api.setCompletionPermitResponse(true);
    git.trackingTip = (async () => H) as typeof git.trackingTip;

    await runnerWith(() => ({ executor: new StubExecutor(nullLogger()) }), gitlab, undefined, undefined, {
      recoveryRetryMs: 1,
    }).execute(claim);

    assert.equal(api.completionHoldRequests.length, 1, "the run holds");
    assert.ok(!statuses(claim.run_id).includes("failed"), "the rewrite landed, so it holds rather than fails");
    assert.equal(closingDirectiveFor(pr.description, 1820, "org/repo"), false, "no closing directive anywhere in the body");
    assert.match(pr.description, /Completion unverified/);
    assert.ok(!pr.description.includes("A review bot's summary."), "preservation yielded to the interlock");
  });

  it("hold path: when that non-closing rewrite cannot be written, the run fails CLOSED", async () => {
    const { gitlab } = fakeGitlab({ head: OTHER, putStatus: 403, existing: adoptedBody(1821, "Fixes #1821") });
    const claim = interlockedClaim(1821);
    api.setCompletionPermitResponse(true);
    git.trackingTip = (async () => H) as typeof git.trackingTip;

    await runnerWith(() => ({ executor: new StubExecutor(nullLogger()) }), gitlab, undefined, undefined, {
      recoveryRetryMs: 1,
    }).execute(claim);

    assert.equal(api.completionHoldRequests.length, 0, "never holds a possibly-closing MR");
    assert.ok(statuses(claim.run_id).includes("failed"), "fails closed");
    assert.ok(!statuses(claim.run_id).includes("completed"));
  });

  it("hold path: human text without a directive is preserved while the completion block turns non-closing", async () => {
    const existing = adoptedBody(1822, "Notes from the maintainer.");
    const { gitlab, pr } = fakeGitlab({ head: OTHER, existing });
    const claim = interlockedClaim(1822);
    api.setCompletionPermitResponse(true);
    git.trackingTip = (async () => H) as typeof git.trackingTip;

    await runnerWith(() => ({ executor: new StubExecutor(nullLogger()) }), gitlab, undefined, undefined, {
      recoveryRetryMs: 1,
    }).execute(claim);

    assert.equal(api.completionHoldRequests.length, 1);
    assert.ok(pr.description.startsWith("Notes from the maintainer.\n\n"), "outside text is kept byte for byte");
    assert.ok(pr.description.endsWith("\n\nA review bot's summary."));
    assert.match(pr.description, /Completion unverified/);
  });

  it("owner partial on a verified head: a human `Closes #N` forces a non-closing rewrite, and the run completes non-closing", async () => {
    const { gitlab, pr } = fakeGitlab({ head: H, existing: adoptedBody(1823, "Resolves #1823") });
    const claim = interlockedClaim(1823, { deferred: [{ milestone_id: "m2", title: "Second", reason: "later" }] });
    api.setCompletionPermitResponse(true);
    git.trackingTip = (async () => H) as typeof git.trackingTip;

    await runner(new StubExecutor(nullLogger()), gitlab).execute(claim);

    assert.ok(statuses(claim.run_id).includes("completed"), "a verified partial completes");
    assert.equal(closingDirectiveFor(pr.description, 1823, "org/repo"), false, "the partial PR never closes");
    assert.match(pr.description, /owner scope decision/);
  });

  it("verified head: malformed completion markers are rewritten whole, and the completion carries Closes", async () => {
    const broken = `${SIZE_REGION}\n\n${COMPLETION_START}\nRelated to #1824.\n${COMPLETION_START}\n${COMPLETION_END}`;
    const { gitlab, pr } = fakeGitlab({ head: H, existing: broken });
    const claim = interlockedClaim(1824);
    api.setCompletionPermitResponse(true);
    git.trackingTip = (async () => H) as typeof git.trackingTip;

    await runner(new StubExecutor(nullLogger()), gitlab).execute(claim);

    assert.ok(statuses(claim.run_id).includes("completed"));
    const parsed = parseOwnedBlocks(pr.description);
    assert.equal(parsed.kind, "ok");
    assert.match(parsed.kind === "ok" ? (parsed.completion ?? "") : "", /Closes #1824/, "a completed run's PR carries Closes");
  });

  it("verified head: when the Closes write cannot be confirmed, the run never reports completed", async () => {
    const { gitlab } = fakeGitlab({ head: H, putStatus: 403, existing: "Legacy body with no markers." });
    const claim = interlockedClaim(1825);
    api.setCompletionPermitResponse(true);
    git.trackingTip = (async () => H) as typeof git.trackingTip;

    await runnerWith(() => ({ executor: new StubExecutor(nullLogger()) }), gitlab, undefined, undefined, {
      recoveryRetryMs: 1,
    }).execute(claim);

    assert.ok(!statuses(claim.run_id).includes("completed"));
    assert.equal(api.completionHoldRequests.length, 1, "it holds");
  });
});

describe("RunRunner — the description publisher (PRD #1798 M6)", () => {
  it("stages, binds and acks the new PR's description through the api, and the body carries uzi's region", async () => {
    const prApi = new FakePrDescApi();
    api.prDescription = prApi;
    const { gitlab, pr } = fakeGitlab();
    const claim = gitlabClaim(1830);
    // The fake answers the landed head, so read 1 matches the staged snapshot.
    const tip = git.trackingTip.bind(git);
    git.trackingTip = (async (bare: string, branch: string) => {
      const h = await tip(bare, branch);
      if (h) pr.head = h;
      return h;
    }) as typeof git.trackingTip;

    await runner(new StubExecutor(nullLogger()), gitlab).execute(claim);

    assert.ok(statuses(claim.run_id).includes("completed"));
    const ops = prApi.calls.map((c) => c.op);
    assert.deepEqual(ops.filter((o) => o === "stage").length, 1);
    assert.equal(prApi.calls.find((c) => c.op === "stage")!.body.source, "deterministic_only", "no editor pass, no lead claims");
    assert.deepEqual(prApi.acks(), ["published"]);
    const parsed = parseOwnedBlocks(pr.description);
    assert.ok(parsed.kind === "ok" && parsed.region?.startsWith(REGION_START));
    assert.match(pr.description, /Closes #1830/, "a legacy issue run keeps Closes at creation");
  });

  it("a publisher failure is advisory: an api without the routes still completes the run", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(1831);
    await runner(new StubExecutor(nullLogger()), gitlab).execute(claim);
    assert.ok(statuses(claim.run_id).includes("completed"));
  });
});

describe("RunRunner — refresh runs (PRD #1798 D17)", () => {
  function mrReworkClaim(): ClaimResponse {
    return makeClaim({
      run_id: randomUUID(),
      kind: "mr_rework",
      issue_iid: null,
      issue_title: "Rework: address MR review",
      issue_description: "Apply the reviewer's feedback.",
      branch: "agent/issue-42",
      base_branch: "main",
      repo: { id: "r1", url: "https://gitlab.example.test/org/repo", clone_url: fx.originPath },
      last_seq: 0,
      secrets: { forge_pat: "fixture-forge-pat-000000", anthropic_oauth_token: "dummy-oauth-do-not-scan" },
    });
  }

  it("mr_rework leaves a legacy PR without uzi markers untouched", async () => {
    const legacy = "A human-written PR body.\n\nCloses #42";
    const { gitlab, calls, pr } = fakeGitlab({ head: H, existing: legacy });
    git.trackingTip = (async () => H) as typeof git.trackingTip;
    await runner(new StubExecutor(nullLogger()), gitlab).execute(mrReworkClaim());
    assert.ok(!calls.some((c) => c.method === "PUT"), "no body write");
    assert.equal(pr.description, legacy);
  });

  it("mr_rework on a marked PR refreshes the region and keeps the completion block's Closes", async () => {
    const completion = [COMPLETION_START, "Related to #42.", "", "Closes #42", "", "---", "Opened by uzi.", COMPLETION_END].join("\n");
    const existing = `Intro.\n\n${SIZE_REGION}\n\n${completion}\n\nBot text.`;
    const { gitlab, pr } = fakeGitlab({ head: H, existing });
    git.trackingTip = (async () => H) as typeof git.trackingTip;
    await runner(new StubExecutor(nullLogger()), gitlab).execute(mrReworkClaim());
    const parsed = parseOwnedBlocks(pr.description);
    assert.ok(parsed.kind === "ok");
    assert.equal(parsed.kind === "ok" && parsed.completion, completion, "the completion block is untouched");
    assert.notEqual(parsed.kind === "ok" && parsed.region, SIZE_REGION, "the region was refreshed");
    assert.match(parsed.kind === "ok" ? (parsed.region ?? "") : "", /Describes `1111111` against `main`\./);
    assert.ok(pr.description.startsWith("Intro.\n\n") && pr.description.endsWith("\n\nBot text."));
  });
});
