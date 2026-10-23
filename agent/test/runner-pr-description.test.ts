import { enableRefreshPublication, followSuccessfulPush } from "./publication-fixture.js";
import fs from "node:fs";
import path from "node:path";
import { SdkExecutor, type SdkQueryFn } from "../src/sdk-executor.js";
import { assistant, resultOk, homeDir, input } from "./runner-harness.js";
import { beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { randomUUID } from "node:crypto";
import { nullLogger, makeClaim } from "./helpers.js";
import { StubExecutor, type ExecutorResult, type RunContext } from "../src/executor.js";
import { computeSize } from "../src/pr-size.js";
import {
  COMPLETION_END,
  COMPLETION_START,
  REGION_END,
  REGION_START,
  closingDirectiveFor,
  parseOwnedBlocks,
  renderCompletionBlock,
} from "../src/pr-description.js";
import type { ClaimResponse } from "../src/protocol.js";
import { FakePrDescApi } from "./fake-pr-desc-api.js";
import { api, client, fakeGitlab as rawFakeGitlab, fx, git, gitlabClaim, installHarness, runner, runnerWith, type FakeForgeOpts } from "./runner-harness.js";

installHarness();
beforeEach(async () => {
  api.setRegisterProtocolFeatures(["claim_generation_fence", "completion_non_closing_v1"]);
  await client.register("pr-description");
});

// PRD #1798 M6 (M6b): the description publisher and the completion interlock, end to end through the
// REAL RunRunner.execute() (StubExecutor commits real work, phasePublish pushes it), against a fake
// forge whose single PR keeps its description across reads and writes.

function fakeGitlab(opts: Parameters<typeof rawFakeGitlab>[0] = {}) {
  const forge = rawFakeGitlab(opts);
  followSuccessfulPush(git, forge.pr, opts.head === undefined || opts.head === H);
  return forge;
}

// Carried-over reconcile test (PRD #2170): reconcileCompletion rewrites the completion block (in place, or
// the whole body when the markers are malformed) and must keep the code cross-check section through both
// rewrites. "issue" renders the issue arm; "prompt" has a kindLine (RUN_KIND_PROFILES completionLine).
for (const arm of ["issue", "prompt"] as const) {
for (const body of ["in-place", "whole-body"] as const) {
for (const interrupted of [false, true]) {
  it(`code summary survives verified completion reconciliation; arm=${arm} rewrite=${body} interrupted=${interrupted}`, async t => {
    const summary: import("../src/code-cross-check-contract.js").CodeCrossCheckRecord = {
      stage: "code", round: 1, candidate_generation: 1, head_commit: "b".repeat(40), base_commit: "a".repeat(40),
      candidate_digest: "c".repeat(64), checker_run_id: "child", checker_harness: "codex", checker_model: "test-checker",
      checker_effort: "high", outcome: "completed", reason_class: null,
      findings: [{ id: "F1", severity: "major", path: "a.ts", line: 1, title: "Check guard", detail: "advice" }],
      dispositions: [{ finding_id: "F1", disposition: "declined", reason: "verified existing guard" }],
      finalized_at: new Date().toISOString(), interrupted_at: null, deadline_at: new Date().toISOString(),
    };
    t.mock.method(client, "codeCrossCheckStatus", async () => interrupted
      ? { ...summary, interrupted_at: new Date().toISOString(), candidate_generation: 2, findings: [], dispositions: null } : summary);
    class SummaryExecutor extends StubExecutor {
      override async run(ctx: RunContext): Promise<ExecutorResult> {
        const result = await super.run(ctx);
        return { ...result, codeCrossCheckSummary: summary };
      }
    }
    const iid = 2190;
    const existing = body === "in-place" ? adoptedBody(iid, "Maintainer notes.")
      : `Maintainer notes.\n\n${SIZE_REGION}\n\n${COMPLETION_START}\nRelated to #${iid}.\n${COMPLETION_START}\n${COMPLETION_END}`;
    const { gitlab, pr } = fakeGitlab({ head: H, existing });
    const claim = { ...interlockedClaim(iid), ...(arm === "prompt" ? { kind: "prompt" as const } : {}),
      code_cross_check_required: true, claim_generation: 1 };
    api.setCompletionPermitResponse(true);
    await runner(new SummaryExecutor(nullLogger()), gitlab).execute(claim);
    assert.ok(statuses(claim.run_id).includes("completed"));
    if (body === "in-place") assert.ok(pr.description.startsWith("Maintainer notes."));
    if (arm === "issue") assert.match(pr.description, /Closes #2190/);
    else {
      assert.match(pr.description, /Ad-hoc scheduled prompt run/, "the kind's own completion line is rendered");
      assert.doesNotMatch(pr.description, /Closes #2190/);
    }
    assert.ok(parseOwnedBlocks(pr.description).kind === "ok");
    if (interrupted) {
      assert.match(pr.description, /Code cross-check incomplete: interrupted/);
      assert.doesNotMatch(pr.description, /checked `bbbb|findings:|F1/);
    } else {
      assert.match(pr.description, /checked `bbbbbbb`, Codex test-checker · high/);
      assert.match(pr.description, /1 finding: 0 addressed, 1 declined/);
      assert.ok(pr.description.includes("**Changes after the check were not cross-checked.**"));
    }
  });
}
}
}

const H = "1111111111111111111111111111111111111111";
const OTHER = "2222222222222222222222222222222222222222";

function interlockedClaim(iid: number, scope?: { deferred: { milestone_id: string; title: string; reason: string }[] }) {
  api.completionPermitScope = scope ?? {};
  const claim = gitlabClaim(iid, {
    claim_generation: 7,
    config: { completion_contract_version: 1, contract_revision: 1, ...(scope ? { completion_scope: scope } : {}) },
  });
  api.setPublicationState(claim.run_id, scope?.deferred ?? [], 1);
  return claim;
}

function statuses(runId: string): string[] {
  return api.states.filter((s) => s.runId === runId).map((s) => s.body.status);
}

function partialDeliveryFeed(runId: string): string[] {
  return api.messages(runId)
    .filter(m => m.kind === "status" && typeof m.payload?.text === "string")
    .map(m => m.payload!.text as string)
    .filter(text => text.startsWith("Retained scope delivered;"));
}

describe("M4 owner-deferred work", () => {
  it("refresh and verified reconcile reset checked work and preserve ordinary lead guidance", async () => {
    const iid = 3090;
    const secret = "glpat-" + "abcdefghijklmnopqrst";
    const scope = { deferred: [
      { milestone_id: "m2", title: `Build **export** ${COMPLETION_END} ${secret}`, reason: "coding remains" },
      { milestone_id: "m3", title: "Live acceptance", reason: `Closes #3090\n${COMPLETION_END}\n/close ${secret}` },
    ] };
    const guidance = "Lead guidance: deploy the retained change and inspect the export in the staging UI.";
    const checked = renderCompletionBlock({ issueIid: iid, branch: "old", closes: false, completionScope: scope })
      .replaceAll("> - [ ]", "> - [x]");
    const existing = `${guidance}\n\n${SIZE_REGION}\n\n${checked}\n\nHuman review notes.`;
    const { gitlab, pr } = fakeGitlab({ head: H, existing });
    const claim = interlockedClaim(iid, scope);
    claim.secrets.forge_pat = secret;
    api.setCompletionPermitResponse(true);
    await runner(new StubExecutor(nullLogger()), gitlab).execute(claim);
    assert.ok(statuses(claim.run_id).includes("completed"));
    const parsed = parseOwnedBlocks(pr.description);
    assert.ok(parsed.kind === "ok" && parsed.completion);
    assert.equal((parsed.completion.match(/^> - \[ \]/gm) ?? []).length, 2);
    const explanation = "Not delivered or verified by this run; must be completed or verified before closing the source issue.";
    for (const id of ["m2", "m3"]) {
      const item = parsed.completion.split("\n").find(line => line.startsWith(`> - [ ] \`${id}\` — `));
      assert.ok(item, `${id} is an unchecked item`);
      assert.ok(item.endsWith(`. ${explanation}`), `${id} carries its own explanation`);
      if (id === "m2") {
        assert.ok(item.includes("Build \\*\\*export\\*\\*"));
        assert.ok(item.includes("\\*\\*\\*REDACTED\\*\\*\\*"), "registered title secret was redacted before escaping");
      } else {
        assert.ok(item.includes("Live acceptance"));
        assert.ok(item.includes("\\*\\*\\*REDACTED\\*\\*\\*"), "registered reason secret was redacted before escaping");
      }
    }
    assert.ok(!parsed.completion.includes("> - [x]"));
    assert.ok(!pr.description.includes(secret), "registered provider token is redacted before escaping");
    assert.equal(pr.description.split(COMPLETION_END).length, 2, "metadata cannot forge markers");
    assert.equal(closingDirectiveFor(pr.description, iid, "org/repo"), false);
    assert.ok(pr.description.startsWith(guidance + "\n\n"));
    assert.ok(pr.description.endsWith("\n\nHuman review notes."));
    assert.ok(!parsed.completion.includes("staging UI"), "no inferred verification steps in authoritative block");
    assert.deepEqual(partialDeliveryFeed(claim.run_id), [
      "Retained scope delivered; 2 deferred milestone(s) still not delivered or verified by this run. The owner must complete or verify them before closing the source issue.",
    ]);
  });

  it("successful coding-only partial publishes generic unchecked work and the partial success summary", async () => {
    const scope = { deferred: [{ milestone_id: "m2", title: "Implement export", reason: "coding remains" }] };
    const claim = interlockedClaim(3092, scope);
    const { gitlab, pr } = fakeGitlab({ head: H });
    api.setCompletionPermitResponse(true);
    await runner(new StubExecutor(nullLogger()), gitlab).execute(claim);
    assert.ok(statuses(claim.run_id).includes("completed"));
    assert.deepEqual(partialDeliveryFeed(claim.run_id), [
      "Retained scope delivered; 1 deferred milestone(s) still not delivered or verified by this run. The owner must complete or verify them before closing the source issue.",
    ]);
    assert.ok(pr.description.split("\n").includes("> - [ ] `m2` — I\u200bmplement export: coding remains. Not delivered or verified by this run; must be completed or verified before closing the source issue."));
    assert.doesNotMatch(pr.description, /acceptance.*pending|pending.*acceptance/i);
  });

  it("M4 maintainer-pending: checked boxes are reset on refresh and the verified head never adds Closes", async () => {
    const iid = 3094;
    const A = "00000000-0000-4000-8000-0000000000a1";
    const pending = { version: 1, contract_revision: 1, criteria: [{
      criterion_id: A, milestone_id: "m2", kind: "maintainer", text: "Observe the hosted rollout",
      reason: "Only the owner can reach the cluster", conditions: ["Rolled out"], verification_steps: ["Open the dashboard"],
      authorization_revision: 1, status: "unverified" }] } as unknown as import("../src/completion-pending.js").CompletionPending;
    api.setRegisterProtocolFeatures(["claim_generation_fence", "completion_non_closing_v1", "completion_semantic_publication_v1"]);
    await client.register("m4-render");
    const checked = renderCompletionBlock({ issueIid: iid, branch: "old", closes: false, completionPending: pending })
      .replaceAll("> - [ ]", "> - [x]");
    assert.match(checked, /> - \[x\]/);
    const existing = `Lead guidance.\n\n${SIZE_REGION}\n\n${checked}\n\nHuman review notes.`;
    const { gitlab, pr } = fakeGitlab({ head: H, existing });
    const claim = interlockedClaim(iid);
    api.setCompletionPermitResponse(true);
    api.stateAckWire = (body, run) => body.status === "running" && Object.keys(body).every(k => ["status", "claim_generation", "messages_through_seq"].includes(k))
      ? { status: 200, body: { run: { ...run, completion_pending: pending } } } : undefined;
    api.completionPermitWire = json => ({ granted: true, permit: {
      id: "p", contract_revision: json.contract_revision, branch: json.branch, head: json.head,
      issued_at: "2026-10-20T00:00:00Z", finding_ids: [], completion_scope: {}, completion_pending: pending } });
    await runner(new StubExecutor(nullLogger()), gitlab).execute(claim);
    assert.ok(statuses(claim.run_id).includes("completed"));
    const parsed = parseOwnedBlocks(pr.description);
    assert.ok(parsed.kind === "ok" && parsed.completion);
    assert.equal((parsed.completion.match(/^> - \[ \] `0000/gm) ?? []).length, 1);
    assert.ok(!parsed.completion.includes("> - [x]"), "a checked box is never authority");
    assert.doesNotMatch(pr.description, new RegExp(`Closes #${iid}`));
    assert.equal(closingDirectiveFor(pr.description, iid, "org/repo"), false);
    assert.ok(pr.description.startsWith("Lead guidance.\n\n"));
    assert.ok(pr.description.endsWith("\n\nHuman review notes."));
  });

  it("prompt partial completes without an owner source-issue handoff", async () => {
    const scope = { deferred: [{ milestone_id: "m2", title: "Implement export", reason: "coding remains" }] };
    const claim = { ...interlockedClaim(3093, scope), kind: "prompt" as const, issue_iid: null };
    const { gitlab, pr } = fakeGitlab({ head: H });
    api.setCompletionPermitResponse(true);
    await runner(new StubExecutor(nullLogger()), gitlab).execute(claim);
    assert.ok(statuses(claim.run_id).includes("completed"));
    assert.deepEqual(partialDeliveryFeed(claim.run_id), []);
    assert.doesNotMatch(JSON.stringify(api.messages(claim.run_id)), /The owner must complete or verify them before closing the source issue/);
    assert.match(pr.description, /Ad-hoc scheduled prompt run/);
    assert.doesNotMatch(pr.description, /before closing the source issue/);
  });

  for (const outcome of ["hold", "fail", "full", "accept-only"] as const) {
    it(`emits no partial success summary for ${outcome}`, async () => {
      const scope = outcome === "hold" || outcome === "fail"
        ? { deferred: [{ milestone_id: "m2", title: "Implement export", reason: "later" }] } : undefined;
      const claim = interlockedClaim(3091, scope);
      if (outcome === "accept-only") api.completionPermitScope = {
        accepted: [{ id: "c1", milestone_id: "m1", text: "criterion", reason: "owner accepted" }],
      };
      const { gitlab } = fakeGitlab({ head: outcome === "hold" ? OTHER : H,
        ...(outcome === "fail" ? { putStatus: 403, existing: adoptedBody(3091, "Notes.") } : {}) });
      api.setCompletionPermitResponse(true);
      await runnerWith(() => ({ executor: new StubExecutor(nullLogger()) }), gitlab, undefined, undefined,
        { recoveryRetryMs: 1 }).execute(claim);
      assert.deepEqual(partialDeliveryFeed(claim.run_id), []);
      if (outcome === "hold") assert.equal(api.completionHoldRequests.length, 1);
      else assert.ok(statuses(claim.run_id).includes(outcome === "fail" ? "failed" : "completed"));
    });
  }
});

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


    await runner(new StubExecutor(nullLogger()), gitlab).execute(claim);

    assert.ok(statuses(claim.run_id).includes("completed"));
    const parsed = parseOwnedBlocks(pr.description);
    assert.equal(parsed.kind, "ok");
    assert.match(parsed.kind === "ok" ? (parsed.completion ?? "") : "", /Closes #1824/, "a completed run's PR carries Closes");
  });

  it("verified head: the publisher's first read fails, and no blind rewrite destroys the human text (the interlock owns it)", async () => {
    const existing = adoptedBody(1826, "Notes from the maintainer.");
    let gets = 0;
    const { gitlab, pr, calls } = fakeGitlab({
      head: H,
      existing,
      // ONLY the publisher's read 1 is refused; every later read (the interlock's) succeeds.
      intercept: (req) => (req.method === "GET" && ++gets === 1 ? { status: 403, text: async () => "{}" } : undefined),
    });
    const claim = interlockedClaim(1826);
    api.setCompletionPermitResponse(true);


    await runner(new StubExecutor(nullLogger()), gitlab).execute(claim);

    assert.ok(statuses(claim.run_id).includes("completed"));
    assert.ok(pr.description.startsWith("Notes from the maintainer.\n\n"), "the human text survives");
    assert.ok(pr.description.endsWith("\n\nA review bot's summary."));
    assert.equal(calls.filter((c) => c.method === "PUT").length, 1, "one write: the interlock's Closes add, no blind rewrite");
    assert.match(pr.description, /Closes #1826/);
  });

  it("verified head: when the Closes write cannot be confirmed (nor the strip), the run fails closed and never reports completed", async () => {
    const { gitlab } = fakeGitlab({ head: H, putStatus: 403, existing: "Legacy body with no markers." });
    const claim = interlockedClaim(1825);
    api.setCompletionPermitResponse(true);


    await runnerWith(() => ({ executor: new StubExecutor(nullLogger()) }), gitlab, undefined, undefined, {
      recoveryRetryMs: 1,
    }).execute(claim);

    assert.ok(!statuses(claim.run_id).includes("completed"));
    // ADR 1225: a Closes add that may have landed is stripped before any hold; the strip is refused
    // too (403), so the MR cannot be proven non-closing and the run fails closed.
    assert.equal(api.completionHoldRequests.length, 0, "never holds a possibly-closing MR");
    assert.ok(statuses(claim.run_id).includes("failed"));
  });
});

describe("RunRunner — the description publisher (PRD #1798 M6)", () => {
  it("stages, binds and acks the new PR's description through the api, and the body carries uzi's region", async () => {
    const prApi = new FakePrDescApi();
    api.prDescription = prApi;
    const { gitlab, pr } = fakeGitlab();
    const claim = gitlabClaim(1830);
    // The fake answers the landed head, so read 1 matches the staged snapshot.


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

    const claim = mrReworkClaim();
    claim.claim_generation ??= 7;
    await enableRefreshPublication(api, client, claim, fx.originPath, 42, "closing");
    await runner(new StubExecutor(nullLogger()), gitlab).execute(claim);
    assert.ok(!calls.some((c) => c.method === "PUT"), "no body write");
    assert.equal(pr.description, legacy);
  });

  it("mr_rework on a marked PR refreshes the region and keeps the completion block's Closes", async () => {
    const completion = [COMPLETION_START, "Related to #42.", "", "Closes #42", "", "---", "Opened by uzi.", COMPLETION_END].join("\n");
    const existing = `Intro.\n\n${SIZE_REGION}\n\n${completion}\n\nBot text.`;
    const { gitlab, pr } = fakeGitlab({ head: H, existing });

    const claim = mrReworkClaim();
    claim.claim_generation ??= 7;
    await enableRefreshPublication(api, client, claim, fx.originPath, 42, "closing");
    await runner(new StubExecutor(nullLogger()), gitlab).execute(claim);
    const parsed = parseOwnedBlocks(pr.description);
    assert.ok(parsed.kind === "ok");
    assert.equal(parsed.kind === "ok" && parsed.completion, completion, "the completion block is untouched");
    assert.notEqual(parsed.kind === "ok" && parsed.region, SIZE_REGION, "the region was refreshed");
    assert.ok(parsed.kind === "ok" && parsed.region?.includes(`Describes \`${pr.head.slice(0, 7)}\` against \`main\`.`));
    assert.ok(pr.description.startsWith("Intro.\n\n") && pr.description.endsWith("\n\nBot text."));
  });
});

describe("RunRunner — the verified-head Closes add is bound to what was written (ADR 1225, PRD #1798 M6)", () => {
  /** The fake PR answers the REAL landed head, so the publisher stages, binds and acks a version and
   *  the completion block carries a described SHA: the production configuration. */
  function followLandedHead(pr: { head: string }): void {
    followSuccessfulPush(git, pr);
  }

  it("PROBE-A: the head moves when `Closes #1901` is written → Closes is stripped and the run holds", async () => {
    api.prDescription = new FakePrDescApi();
    const { gitlab, pr } = fakeGitlab({
      onWrite: (p, description) => {
        if (/Closes #1901/.test(description)) p.head = OTHER;
      },
    });
    followLandedHead(pr);
    const claim = interlockedClaim(1901);
    api.setCompletionPermitResponse(true);

    await runnerWith(() => ({ executor: new StubExecutor(nullLogger()) }), gitlab, undefined, undefined, {
      recoveryRetryMs: 1,
    }).execute(claim);

    assert.ok(!statuses(claim.run_id).includes("completed"), "never completes on a moved head");
    assert.doesNotMatch(pr.description, /Closes #1901/, "no Closes is left on the MR");
    assert.equal(closingDirectiveFor(pr.description, 1901, "org/repo"), false);
    // The strip is readable and confirmed, so the run holds (exactly once) and does not fail.
    assert.equal(api.completionHoldRequests.length, 1, "exactly one hold");
    assert.ok(!statuses(claim.run_id).includes("failed"), "the strip landed, so it holds rather than fails");
    assert.match(pr.description, /Completion unverified/);
  });

  it("PROBE-B: the MR becomes unreadable (404) after the Closes write → Closes is stripped and the run holds", async () => {
    api.prDescription = new FakePrDescApi();
    const opts: FakeForgeOpts = {};
    opts.onWrite = (_p, description) => {
      if (/Closes #1902/.test(description)) opts.headStatus = 404;
    };
    const { gitlab, pr } = fakeGitlab(opts);
    followLandedHead(pr);
    const claim = interlockedClaim(1902);
    api.setCompletionPermitResponse(true);

    await runnerWith(() => ({ executor: new StubExecutor(nullLogger()) }), gitlab, undefined, undefined, {
      recoveryRetryMs: 1,
    }).execute(claim);

    assert.ok(!statuses(claim.run_id).includes("completed"), "never completes on an unreadable MR");
    assert.doesNotMatch(pr.description, /Closes #1902/, "no Closes is left on the MR");
    assert.equal(closingDirectiveFor(pr.description, 1902, "org/repo"), false);
    // The strip's read fails, so it falls back to the blind non-closing rewrite (which lands): the
    // run holds exactly once and does not fail.
    assert.equal(api.completionHoldRequests.length, 1, "exactly one hold");
    assert.ok(!statuses(claim.run_id).includes("failed"));
    assert.match(pr.description, /Completion unverified/);
  });
});

/** The fake PR follows the REAL landed head (the publisher then stages real snapshots). Returns the
 *  landed head once the run read it. */
function followHead(pr: { head: string }): () => string {
  return followSuccessfulPush(git, pr);
}

/** A StubExecutor run stopped by an operator scope directive after committing work (PRD #634). */
class ScopeCappedStub extends StubExecutor {
  override async run(ctx: RunContext): Promise<ExecutorResult> {
    return { ...(await super.run(ctx)), scopeCapped: { completedCount: 1, total: 2 } };
  }
}

describe("RunRunner — a legacy non-closing run whose PR still closes fails closed (PRD #1798 M6, amended D10, M1)", () => {
  const closingCompletion = (iid: number) =>
    [COMPLETION_START, `Related to #${iid}.`, "", `Closes #${iid}`, "", "---", "Opened by uzi.", COMPLETION_END].join("\n");

  it("an adopted body with `Closes #N` and a failing forge write: the scope-capped legacy run fails, never completes", async () => {
    const { gitlab, pr } = fakeGitlab({ putStatus: 403, existing: `${SIZE_REGION}\n\n${closingCompletion(1840)}` });
    followHead(pr);
    const claim = gitlabClaim(1840); // no config: a legacy (non-interlocked) run

    await runner(new ScopeCappedStub(nullLogger()), gitlab).execute(claim);

    assert.ok(!statuses(claim.run_id).includes("completed"), "a partial never completes with a closing PR");
    assert.ok(statuses(claim.run_id).includes("failed"), "it fails closed");
    const failed = api.states.find((st) => st.runId === claim.run_id && st.body.status === "failed")!.body;
    assert.equal(failed.failure_reason, "completion interlock: a closing directive could not be removed from a non-closing merge request");
    assert.match(pr.description, /Closes #1840/, "the write really failed (the directive is still there)");
  });

  it("control: the same adopted body with a working forge is rewritten non-closing and the run completes", async () => {
    const { gitlab, pr } = fakeGitlab({ existing: `Closes #1841 too\n\n${SIZE_REGION}\n\n${closingCompletion(1841)}` });
    followHead(pr);
    const claim = gitlabClaim(1841);

    await runner(new ScopeCappedStub(nullLogger()), gitlab).execute(claim);

    assert.ok(statuses(claim.run_id).includes("completed"));
    assert.equal(closingDirectiveFor(pr.description, 1841, "org/repo"), false);
  });
});

describe("RunRunner — the publication and the interlock are independent (PRD #1798 M6, L1)", () => {
  it("a stale_claim at staging stops the publisher's writes; the interlock still adds Closes on the verified head", async () => {
    const prApi = new FakePrDescApi();
    prApi.failNext("stage", 409, "stale_claim");
    api.prDescription = prApi;
    const { gitlab, pr, all } = fakeGitlab();
    followHead(pr);
    const claim = interlockedClaim(1850);
    api.setCompletionPermitResponse(true);

    await runner(new StubExecutor(nullLogger()), gitlab).execute(claim);

    assert.deepEqual(prApi.calls.map((c) => c.op), ["stage"], "no api call after the 409");
    const puts = all.filter((c) => c.method === "PUT");
    assert.equal(puts.length, 1, "the only body write is the interlock's");
    assert.match(String(JSON.parse(puts[0]!.body ?? "{}").description), /Closes #1850/);
    assert.ok(statuses(claim.run_id).includes("completed"));
  });
});

describe("RunRunner — an over-cap MR read (PRD #1798 M6, H1)", () => {
  it("the publisher skips the region, the read is not retried, and a legacy run still completes", async () => {
    const { gitlab, pr, reads, calls } = fakeGitlab({ existing: "x".repeat(7 * 1_048_576) });
    followHead(pr);
    const claim = gitlabClaim(1860);

    await runner(new StubExecutor(nullLogger()), gitlab).execute(claim);

    assert.ok(statuses(claim.run_id).includes("completed"));
    assert.equal(reads.length, 1, "a response-too-large read is permanent: one GET, no retry");
    assert.ok(!calls.some((c) => c.method === "PUT"), "nothing is written over an unreadable PR");
  });
});

describe("RunRunner — the size line counts the PR's actual target, not the repo default (Greptile #1813)", () => {
  it("an adopted PR targeting `release` gets the size of the branch against `release`", async () => {
    // origin: `release` at the first commit; main gains a commit the agent branch inherits. Against
    // main the branch changes the stub's file only; against release it also carries main's commit.
    const env = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
    const og = (args: string[]) => execFileSync("git", ["-C", fx.originPath, ...args], { env, stdio: "pipe" });
    og(["branch", "release"]);
    execFileSync("sh", ["-c", 'printf "one\\ntwo\\nthree\\n" > docs-extra.md'], { cwd: fx.originPath });
    og(["add", "docs-extra.md"]);
    og(["commit", "-m", "main moves on"]);

    const prApi = new FakePrDescApi();
    api.prDescription = prApi;
    const { gitlab, pr } = fakeGitlab({ target: "release" });
    const landed = followHead(pr);
    const claim = gitlabClaim(1870, {
      repo: { id: "r1", url: "https://gitlab.example.test/org/repo", clone_url: fx.originPath, default_branch: "main" },
    });

    await runner(new StubExecutor(nullLogger()), gitlab).execute(claim);

    assert.ok(statuses(claim.run_id).includes("completed"));
    const bare = git.barePathFor(fx.originPath);
    const vsRelease = (await computeSize(git, bare, "release", landed())).line;
    const vsMain = (await computeSize(git, bare, "main", landed())).line;
    assert.ok(vsRelease && vsMain && vsRelease !== vsMain, `${vsRelease} / ${vsMain}`);
    const parsed = parseOwnedBlocks(pr.description);
    const region = parsed.kind === "ok" ? (parsed.region ?? "") : "";
    assert.ok(region.includes(vsRelease!), `the size line is against the PR's target:\n${region}`);
    assert.ok(!region.includes(vsMain!), region);
    assert.match(region, /against `release`\./);
    const stages = prApi.calls.filter((c) => c.op === "stage").map((c) => c.body.target_branch);
    assert.equal(stages.at(-1), "release", "the version the PR carries was staged for its real target");
    assert.deepEqual(prApi.acks().at(-1), "published");
  });
});

// PRD #1798 M6: the verified-head path of an INTERLOCKED non-closing delivery (an owner partial, a
// scope cap). The head is verified, so the reconcile writes the partial's non-closing block; its two
// failure shapes take different arms. A failed MR read falls back to the blind non-closing rewrite
// (`blind`): nothing confirmed the completion, so the run holds and never completes. A refused write
// leaves the MR unconfirmed (it may still carry an adopted closing block), so the strip runs first,
// and when the strip is refused too the run fails closed with no hold.
describe("RunRunner — an interlocked non-closing delivery on a verified head (PRD #1798 M6)", () => {
  const OWNER_PARTIAL = { deferred: [{ milestone_id: "m2", title: "Second", reason: "later" }] };
  const arms: Array<[string, (iid: number) => ClaimResponse, () => StubExecutor]> = [
    ["owner partial", (iid) => interlockedClaim(iid, OWNER_PARTIAL), () => new StubExecutor(nullLogger())],
    ["scope cap", (iid) => interlockedClaim(iid), () => new ScopeCappedStub(nullLogger())],
  ];
  let iid = 1910;
  for (const [name, makeClaim_, makeExecutor] of arms) {
    it(`${name}: the reconcile's MR read fails (403) → the run HOLDS and never reports completed`, async () => {
      const n = iid++;
      const opts: FakeForgeOpts = { head: H, existing: adoptedBody(n, "Notes from the maintainer.") };
      const { gitlab, pr, all } = fakeGitlab(opts);
      // The head reads keep verifying H; every MR DETAIL read after the first head read (the
      // reconcile's) is answered 403 by the transport. The head reads are left working so a run that
      // skipped the `blind` arm would re-verify the head and complete (the mutation this gates).
      let verified = false;
      let refused = 0;
      const readHead = gitlab.getMergeRequestHead.bind(gitlab);
      gitlab.getMergeRequestHead = async (...args: Parameters<typeof readHead>) => {
        const head = await readHead(...args);
        verified = true;
        return head;
      };
      const readDetail = gitlab.getMergeRequest.bind(gitlab);
      gitlab.getMergeRequest = async (...args: Parameters<typeof readDetail>) => {
        if (!verified) return readDetail(...args);
        refused++;
        opts.headStatus = 403;
        try {
          return await readDetail(...args);
        } finally {
          opts.headStatus = undefined;
        }
      };
      const claim = makeClaim_(n);
      api.setCompletionPermitResponse(true);


      await runnerWith(() => ({ executor: makeExecutor() }), gitlab, undefined, undefined, { recoveryRetryMs: 1 }).execute(claim);

      assert.ok(refused >= 1, "the reconcile's read was refused after the head was verified");
      assert.ok(!statuses(claim.run_id).includes("completed"), "an unconfirmed partial never completes");
      assert.equal(api.completionHoldRequests.length, 1, "exactly one hold");
      assert.ok(!statuses(claim.run_id).includes("failed"), "the blind rewrite landed, so it holds rather than fails");
      assert.equal(closingDirectiveFor(pr.description, n, "org/repo"), false);
      assert.ok(!pr.description.includes("Notes from the maintainer."), "the blind rewrite replaced the body whole");
      assert.equal(all.at(-1)?.method, "PUT", "the last forge call is the blind rewrite");
    });

    it(`${name}: body writes are refused (403) → the run FAILS closed with no hold`, async () => {
      const n = iid++;
      const { gitlab, pr } = fakeGitlab({ head: H, putStatus: 403, existing: adoptedBody(n, "Notes from the maintainer.") });
      const claim = makeClaim_(n);
      api.setCompletionPermitResponse(true);


      await runnerWith(() => ({ executor: makeExecutor() }), gitlab, undefined, undefined, { recoveryRetryMs: 1 }).execute(claim);

      assert.ok(!statuses(claim.run_id).includes("completed"));
      assert.equal(api.completionHoldRequests.length, 0, "never holds an MR the strip could not confirm");
      assert.ok(statuses(claim.run_id).includes("failed"), "fails closed");
      assert.equal(pr.description, adoptedBody(n, "Notes from the maintainer."), "every write really was refused");
    });
  }
});

/** A real streamed Response of `total` filler bytes (a body over a byte cap). */
function streamedResponse(status: number, total: number): Response {
  const MiB = 1_048_576;
  let sent = 0;
  const chunk = new Uint8Array(MiB).fill(0x61);
  return new Response(
    new ReadableStream<Uint8Array>({
      pull(ctl) {
        if (sent >= total) return ctl.close();
        sent += chunk.byteLength;
        ctl.enqueue(chunk);
      },
    }),
    { status },
  );
}

describe("RunRunner — over-cap forge answers are permanent and treated as unreadable (PRD #1798 M6, H1)", () => {
  it("an over-cap create (201) answer fails the run after ONE POST: never retried", async () => {
    const { gitlab, calls } = fakeGitlab({
      intercept: (req) => (req.method === "POST" ? streamedResponse(201, 16 * 1_048_576) : undefined),
    });
    const claim = gitlabClaim(1920);

    await runner(new StubExecutor(nullLogger()), gitlab).execute(claim);

    assert.ok(!statuses(claim.run_id).includes("completed"));
    assert.ok(statuses(claim.run_id).includes("failed"));
    assert.equal(calls.filter((c) => c.method === "POST").length, 1, "a response-too-large create is not retried");
  });

  it("an interlocked run whose head read is over the cap takes the unreadable-head path: strip, then hold", async () => {
    const { gitlab, pr } = fakeGitlab({
      head: H,
      existing: adoptedBody(1921, "Closes #1921 please"),
      intercept: (req) => (req.method === "GET" ? streamedResponse(200, 16 * 1_048_576) : undefined),
    });
    const claim = interlockedClaim(1921);
    api.setCompletionPermitResponse(true);


    await runnerWith(() => ({ executor: new StubExecutor(nullLogger()) }), gitlab, undefined, undefined, {
      recoveryRetryMs: 1,
    }).execute(claim);

    assert.ok(!statuses(claim.run_id).includes("completed"), "an unreadable head never completes");
    assert.equal(api.completionHoldRequests.length, 1, "exactly one hold");
    assert.ok(!statuses(claim.run_id).includes("failed"));
    assert.equal(closingDirectiveFor(pr.description, 1921, "org/repo"), false, "the adopted directive was stripped (blind)");
    assert.match(pr.description, /Completion unverified/);
  });
});

// PRD #1798 M6 (amended D10, M1): a legacy non-closing run whose publisher NEVER read the PR must not
// complete on the strength of having observed nothing. It falls back to the blind whole-body
// non-closing rewrite; only when that is refused too does the run fail closed. Since #2815 a
// scope-capped or owner-partial run also reads its PR title back, so a PR that is never readable at
// all fails closed on the title confirm even when the blind description rewrite lands.
describe("RunRunner — a legacy non-closing run whose PR is never readable (PRD #1798 M6, amended D10)", () => {
  const closingCompletion = (iid: number) =>
    [COMPLETION_START, `Related to #${iid}.`, "", `Closes #${iid}`, "", "---", "Opened by uzi.", COMPLETION_END].join("\n");

  it("description reads refused (403) before the blind rewrite: the rewrite lands and the scope-capped run completes non-closing", async () => {
    const existing = `${SIZE_REGION}\n\n${closingCompletion(1930)}`;
    // Issue #2815: the title confirm also reads the PR now, so only the reads BEFORE the blind
    // description write are refused; the title read after it succeeds.
    let written = false;
    const { gitlab, pr, calls } = fakeGitlab({
      existing,
      onWrite: () => { written = true; },
      intercept: req => req.method === "GET" && !written ? { status: 403, text: async () => "{}" } : undefined,
    });
    const claim = gitlabClaim(1930);

    await runner(new ScopeCappedStub(nullLogger()), gitlab).execute(claim);

    assert.ok(statuses(claim.run_id).includes("completed"), "the blind rewrite removed the directive");
    assert.equal(closingDirectiveFor(pr.description, 1930, "org/repo"), false);
    assert.equal(calls.filter((c) => c.method === "PUT").length, 1, "one blind write");
  });

  it("every GET is refused (403): the blind rewrite lands but the unreadable title fails the scope-capped run closed (#2815)", async () => {
    const existing = `${SIZE_REGION}\n\n${closingCompletion(1933)}`;
    const { gitlab, pr, calls } = fakeGitlab({ headStatus: 403, existing });
    const claim = gitlabClaim(1933);

    await runner(new ScopeCappedStub(nullLogger()), gitlab).execute(claim);

    assert.ok(!statuses(claim.run_id).includes("completed"));
    const failed = api.states.find((st) => st.runId === claim.run_id && st.body.status === "failed")?.body;
    assert.equal(failed?.failure_reason,
      "completion interlock: a closing directive could not be removed from a non-closing merge request's title; the merge request remains open and must not be merged as-is");
    assert.equal(api.completionHoldRequests.length, 0);
    assert.equal(closingDirectiveFor(pr.description, 1933, "org/repo"), false, "the blind description rewrite still landed");
    assert.equal(calls.filter((c) => c.method === "PUT").length, 1, "one blind write");
  });

  it("every GET and every PUT is refused (403): the run fails closed and never completes", async () => {
    const existing = `${SIZE_REGION}\n\n${closingCompletion(1931)}`;
    const { gitlab, pr } = fakeGitlab({ headStatus: 403, putStatus: 403, existing });
    const claim = gitlabClaim(1931);

    await runner(new ScopeCappedStub(nullLogger()), gitlab).execute(claim);

    assert.ok(!statuses(claim.run_id).includes("completed"));
    const failed = api.states.find((st) => st.runId === claim.run_id && st.body.status === "failed")?.body;
    assert.equal(failed?.failure_reason, "completion interlock: a closing directive could not be removed from a non-closing merge request");
    assert.equal(pr.description, existing, "the write really was refused");
  });

  it("an over-cap body: the read is refused, and the blind rewrite path runs (the run completes non-closing)", async () => {
    const existing = `Closes #1932\n\n${"x".repeat(7 * 1_048_576)}\n\n${SIZE_REGION}\n\n${closingCompletion(1932)}`;
    const { gitlab, pr, reads, calls } = fakeGitlab({ existing });
    followHead(pr);
    const claim = gitlabClaim(1932);

    await runner(new ScopeCappedStub(nullLogger()), gitlab).execute(claim);

    // One refused over-cap description read (permanent: not retried) plus, since #2815, the title
    // confirm read of the already rewritten (small) body.
    assert.equal(reads.length, 2, "one over-cap read (permanent: not retried) plus the title confirm read");
    assert.equal(calls.filter((c) => c.method === "PUT").length, 1, "the blind rewrite");
    assert.ok(statuses(claim.run_id).includes("completed"));
    assert.equal(closingDirectiveFor(pr.description, 1932, "org/repo"), false);
    assert.ok(pr.description.length < 1_048_576, "the body was rewritten whole");
  });
});


describe("m2 non-closing delivery", () => {
  for (const current of [undefined, "Current flight reason"]) {
    it(`M2 stored claim intent survives resume without redeclaration; current=${current}`, async () => {
      const { gitlab, pr } = fakeGitlab({ existing: adoptedBody(2172, "Fixes #2172"), existingTitle: "Fixes #2172" });
      const claim = interlockedClaim(2172);
      const secret = "inert-stored-secret-2172";
      claim.secrets.forge_pat = secret;
      claim.issue_title = "Fixes #2172";
      claim.config!.non_closing = { reason: `Remaining human acceptance ${secret}` };
      api.setCompletionPermitResponse(true);
      await runner(declaring(current), gitlab).execute(claim);
      assert.ok(statuses(claim.run_id).includes("completed"));
      assert.match(pr.description, current ? /Current flight reason/ : /Remaining human acceptance/);
      assert.ok(!pr.description.includes(secret), "stored reason is redacted");
      if (current) assert.doesNotMatch(pr.description, /Remaining human acceptance/);
      assert.equal(closingDirectiveFor(pr.title + "\n" + pr.description, 2172, "org/repo"), false);
      assert.equal(api.completionPermitRequests.length, 1, "stored metadata never skips the permit");
    });
  }

  it("M2 stored intent never waives a mandatory completion gate", async () => {
    const { gitlab, all } = fakeGitlab();
    const claim = interlockedClaim(2172);
    claim.config!.non_closing = { reason: "Human acceptance pending" };
    api.setCompletionPermitResponse(false, { denyReason: "missing_milestones" });
    await runner(declaring(), gitlab).execute(claim);
    assert.equal(all.length, 0, "denied permit blocks all publication");
    assert.ok(!statuses(claim.run_id).includes("completed"));
    assert.equal(api.completionHoldRequests.length, 1);
  });

  function declaring(reason?: string, scopeCapped?: { completedCount: number; total?: number }) {
    return new class extends StubExecutor {
      override async run(ctx: RunContext): Promise<ExecutorResult> {
        const result = await super.run(ctx);
        return { ...result, ...(reason === undefined ? {} : { nonClosing: { reason } }), scopeCapped };
      }
    }(nullLogger());
  }

  for (const interlocked of [false, true]) {
    for (const existing of [undefined, "Closes #2172", "Fixes #2172", "Resolves #2172"]) {
      it(`m2 non-closing ${interlocked ? "verified reconcile" : "legacy creation"} ${existing ?? "fresh"}`, async () => {
        const { gitlab, pr, all } = fakeGitlab({ existing: existing && adoptedBody(2172, existing) });
        const claim = interlocked ? interlockedClaim(2172) : gitlabClaim(2172);
        api.setCompletionPermitResponse(true);
        await runner(declaring("Approved follow-up remains"), gitlab).execute(claim);
        assert.ok(statuses(claim.run_id).includes("completed"));
        assert.match(pr.description, /Approved follow-up remains/, "declared reason is visible");
        assert.equal(closingDirectiveFor(pr.description, 2172, "org/repo"), false, "no closing directive anywhere");
        if (interlocked) assert.ok(all.filter(c => c.method === "GET").length >= 2, "both head checks remain");
      });
    }
  }

  for (const interlocked of [false, true]) {
    it(`m2 non-closing redacts registered exact and control-split secrets ${interlocked}`, async () => {
      const secret = "inert-run-secret-2172";
      const splitSecret = ["inert-split-", "secret-", "2172"].join("");
      const reason = `Follow-up ${secret} and ${splitSecret.slice(0, 7)}\u202E${splitSecret.slice(7)} remains`;
      const { gitlab, pr } = fakeGitlab({ existing: adoptedBody(2172, "Fixes #2172") });
      const claim = interlocked ? interlockedClaim(2172) : gitlabClaim(2172);
      claim.secrets.forge_pat = secret;
      claim.secrets.anthropic_oauth_token = splitSecret;
      api.setCompletionPermitResponse(true);
      await runner(declaring(reason), gitlab).execute(claim);
      assert.ok(statuses(claim.run_id).includes("completed"));
      assert.match(pr.description, /Follow-up .* remains/);
      assert.ok(!pr.description.includes(secret), "exact registered secret is redacted");
      assert.ok(!pr.description.includes(splitSecret), "control-split registered secret is redacted before controls are dropped");
      assert.equal(closingDirectiveFor(pr.description, 2172, "org/repo"), false);
    });
  }

  it("m2 non-closing refuses an adopted closing body that cannot be rewritten", async () => {
    const { gitlab } = fakeGitlab({ putStatus: 403, existing: adoptedBody(2172, "Resolves #2172") });
    const claim = interlockedClaim(2172);
    api.setCompletionPermitResponse(true);
    await runnerWith(() => ({ executor: declaring("Follow-up remains") }), gitlab, undefined, undefined, { recoveryRetryMs: 1 }).execute(claim);
    assert.ok(statuses(claim.run_id).includes("failed"));
    assert.ok(!statuses(claim.run_id).includes("completed"));
    assert.equal(api.completionHoldRequests.length, 0);
  });

  it("m2 non-closing still holds on a failed first head check", async () => {
    const { gitlab, pr } = fakeGitlab({ head: OTHER });
    const claim = interlockedClaim(2172);
    api.setCompletionPermitResponse(true);
    await runnerWith(() => ({ executor: declaring("Follow-up remains") }), gitlab, undefined, undefined, { recoveryRetryMs: 1 }).execute(claim);
    assert.equal(api.completionHoldRequests.length, 1);
    assert.ok(!statuses(claim.run_id).includes("completed"));
    assert.match(pr.description, /Follow-up remains/);
    assert.equal(closingDirectiveFor(pr.description, 2172, "org/repo"), false);
  });

  it("m2 non-closing still holds if the head moves during the verified write", async () => {
    let writes = 0;
    let gets = 0;
    const { gitlab, pr } = fakeGitlab({
      existing: adoptedBody(2172, "Maintainer notes."),
      // Refuse only the publisher read so the interlock must refresh the old canonical block.
      intercept: req => req.method === "GET" && ++gets === 1 ? { status: 403, text: async () => "{}" } : undefined,
      onWrite: p => {
        assert.ok(gets >= 2, "the first head read precedes the verified write");
        if (++writes === 1) p.head = OTHER;
      },
    });
    const claim = interlockedClaim(2172);
    api.setCompletionPermitResponse(true);
    await runnerWith(() => ({ executor: declaring("Follow-up remains") }), gitlab, undefined, undefined, { recoveryRetryMs: 1 }).execute(claim);
    assert.ok(writes >= 1, "the non-closing verified body changed and triggered head movement");
    assert.equal(api.completionHoldRequests.length, 1);
    assert.ok(!statuses(claim.run_id).includes("completed"));
    assert.equal(closingDirectiveFor(pr.description, 2172, "org/repo"), false);
  });

  it("m2 non-closing mr_rework refresh preserves existing completion semantics", async () => {
    const completion = [COMPLETION_START, "Related to #2172.", "", "Closes #2172", "", "---", "Opened by uzi.", COMPLETION_END].join("\n");
    const { gitlab, pr } = fakeGitlab({ existing: `Intro.\n\n${SIZE_REGION}\n\n${completion}\n\nBot text.` });
    const claim = gitlabClaim(2172, { kind: "mr_rework", branch: "agent/issue-2172", base_branch: "main" });
    claim.claim_generation ??= 7;
    await enableRefreshPublication(api, client, claim, fx.originPath, 2172, "closing");
    await runner(declaring("must be ignored"), gitlab).execute(claim);
    assert.ok(statuses(claim.run_id).includes("completed"));
    const parsed = parseOwnedBlocks(pr.description);
    assert.ok(parsed.kind === "ok");
    assert.equal(parsed.completion, completion, "refresh keeps the existing closing block byte-for-byte");
    assert.notEqual(parsed.region, SIZE_REGION, "refresh updates the description region");
    assert.ok(!pr.description.includes("must be ignored"));
    assert.ok(pr.description.startsWith("Intro.\n\n") && pr.description.endsWith("\n\nBot text."));
  });

  for (const partial of ["owner", "cap"] as const) {
    it(`m2 non-closing ${partial} annotation retains the declaration`, async () => {
      const claim = partial === "owner" ? interlockedClaim(2172, { deferred: [{ milestone_id: "m2", title: "Later", reason: "owner deferred" }] }) : interlockedClaim(2172);
      const { gitlab, pr } = fakeGitlab();
      api.setCompletionPermitResponse(true);
      await runner(declaring("Approved follow-up remains", partial === "cap" ? { completedCount: 1, total: 2 } : undefined), gitlab).execute(claim);
      assert.match(pr.description, partial === "owner" ? /owner scope decision/ : /operator scope directive/);
      assert.match(pr.description, /Approved follow-up remains/);
      assert.equal(closingDirectiveFor(pr.description, 2172, "org/repo"), false);
    });
  }

  it("m2 non-closing no declaration retains closing semantics", async () => {
    const { gitlab, pr } = fakeGitlab();
    const claim = interlockedClaim(2172);
    api.setCompletionPermitResponse(true);
    await runner(declaring(), gitlab).execute(claim);
    assert.ok(statuses(claim.run_id).includes("completed"));
    assert.match(pr.description, /Closes #2172/);
  });

  it("m2 non-closing ci_fix without pipeline ignores an issue declaration", async () => {
    const { gitlab, pr } = fakeGitlab();
    const claim = gitlabClaim(2172, { kind: "ci_fix" });
    await runner(declaring("must be ignored"), gitlab).execute(claim);
    assert.match(pr.description, /Closes #2172/);
    assert.ok(!pr.description.includes("must be ignored"));
  });

  it("m2 non-closing raw Claude frames through SdkExecutor deliver a real non-closing PR", async () => {
    const { gitlab, pr } = fakeGitlab();
    const claim = gitlabClaim(2172);
    api.setInputs(claim.run_id, [input("approve_plan")]);
    let turn = 0;
    const queryFn: SdkQueryFn = params => (async function* () {
      for await (const _ of params.prompt) { /* drain */ }
      if (turn++ === 0) {
        yield assistant([{ type: "tool_use", id: "plan", name: "mcp__uzi__submit_plan", input: { plan_md: "Approved follow-up remains open" } }]);
      } else {
        const cwd = params.options.cwd!;
        fs.writeFileSync(path.join(cwd, "m2.txt"), "delivered work\n");
        execFileSync("git", ["-C", cwd, "add", "m2.txt"]);
        execFileSync("git", ["-C", cwd, "-c", "user.name=Test", "-c", "user.email=test@example.test", "-c", "commit.gpgsign=false", "commit", "-m", "m2 work"]);
        yield assistant([{ type: "tool_use", id: "done", name: "mcp__uzi__signal_done", input: { non_closing: { reason: "Approved follow-up remains" } } }]);
      }
      yield resultOk();
    })();
    await runnerWith(() => ({ executor: new SdkExecutor(nullLogger(), homeDir, { queryFn }), homeDir }), gitlab).execute(claim);
    assert.ok(statuses(claim.run_id).includes("completed"));
    assert.match(pr.description, /Approved follow-up remains/);
    assert.equal(closingDirectiveFor(pr.description, 2172, "org/repo"), false);
  });

  // Issue #2779: the PR TITLE of a declared non-closing issue run must not close the issue either.
  const CLOSING_TITLE = "Fixes #2172";
  function failureReason(runId: string): string | undefined {
    return api.states.find(st => st.runId === runId && st.body.status === "failed")?.body.failure_reason;
  }
  const TITLE_REASON =
    "completion interlock: a closing directive could not be removed from a non-closing merge request's title; the merge request remains open and must not be merged as-is";

  it("m2 non-closing raw Claude frames through SdkExecutor retitle a closing issue title", async () => {
    const { gitlab, pr } = fakeGitlab();
    const claim = gitlabClaim(2172, { issue_title: CLOSING_TITLE });
    api.setInputs(claim.run_id, [input("approve_plan")]);
    let turn = 0;
    const queryFn: SdkQueryFn = params => (async function* () {
      for await (const _ of params.prompt) { /* drain */ }
      if (turn++ === 0) {
        yield assistant([{ type: "tool_use", id: "plan", name: "mcp__uzi__submit_plan", input: { plan_md: "Approved follow-up remains open" } }]);
      } else {
        const cwd = params.options.cwd!;
        fs.writeFileSync(path.join(cwd, "m2.txt"), "delivered work\n");
        execFileSync("git", ["-C", cwd, "add", "m2.txt"]);
        execFileSync("git", ["-C", cwd, "-c", "user.name=Test", "-c", "user.email=test@example.test", "-c", "commit.gpgsign=false", "commit", "-m", "m2 work"]);
        yield assistant([{ type: "tool_use", id: "done", name: "mcp__uzi__signal_done", input: { non_closing: { reason: "Approved follow-up remains" } } }]);
      }
      yield resultOk();
    })();
    await runnerWith(() => ({ executor: new SdkExecutor(nullLogger(), homeDir, { queryFn }), homeDir }), gitlab).execute(claim);
    assert.ok(statuses(claim.run_id).includes("completed"));
    assert.equal(closingDirectiveFor(pr.title, 2172, "org/repo"), false, "the created PR title does not close the issue");
    assert.ok(pr.title.includes("#2172"));
  });

  for (const interlocked of [false, true]) {
    it(`m2 non-closing ${interlocked ? "interlocked" : "legacy"} declared run creates its PR with a non-closing title`, async () => {
      const { gitlab, pr, calls } = fakeGitlab();
      const claim = interlocked ? interlockedClaim(2172) : gitlabClaim(2172);
      claim.issue_title = CLOSING_TITLE;
      api.setCompletionPermitResponse(true);
      await runner(declaring("Approved follow-up remains"), gitlab).execute(claim);
      assert.ok(statuses(claim.run_id).includes("completed"));
      assert.equal(closingDirectiveFor(pr.title, 2172, "org/repo"), false);
      const create = calls.find(c => c.method === "POST");
      assert.ok(create, "the create POST was recorded");
      const createdTitle = String(JSON.parse(create.body ?? "{}").title);
      assert.equal(closingDirectiveFor(createdTitle, 2172, "org/repo"), false, `create-time title is non-closing: ${createdTitle}`);
      assert.ok(createdTitle.includes("#2172"));
      assert.ok(!calls.some(c => c.method !== "POST" && "title" in JSON.parse(c.body ?? "{}") && !("description" in JSON.parse(c.body ?? "{}"))),
        "no title-only write was needed");
    });
  }

  it("m2 non-closing retitles an adopted PR whose title closes the issue", async () => {
    const { gitlab, pr, calls } = fakeGitlab({ existing: adoptedBody(2172, "Maintainer notes."), existingTitle: CLOSING_TITLE });
    const claim = gitlabClaim(2172);
    await runner(declaring("Approved follow-up remains"), gitlab).execute(claim);
    assert.ok(statuses(claim.run_id).includes("completed"));
    assert.equal(closingDirectiveFor(pr.title, 2172, "org/repo"), false);
    assert.ok(calls.some(c => (c.method === "PUT" || c.method === "PATCH") && JSON.parse(c.body ?? "{}").title !== undefined), "a title write was recorded");
    assert.match(pr.description, /Maintainer notes\./, "the description survives a title write");
  });

  it("m2 non-closing leaves a clean adopted title untouched (no title write)", async () => {
    const clean = fakeGitlab({ existing: adoptedBody(2172, "Maintainer notes."), existingTitle: "Tidy title" });
    await runner(declaring("Approved follow-up remains"), clean.gitlab).execute(gitlabClaim(2172));
    assert.equal(clean.pr.title, "Tidy title");
    assert.ok(!clean.calls.some(c => JSON.parse(c.body ?? "{}").title !== undefined && c.method !== "POST"));
  });

  it("m2 non-closing fails closed, without a hold, when the PR title cannot be read", async () => {
    const { gitlab } = fakeGitlab({ existing: adoptedBody(2172, "Maintainer notes."), omitTitle: true });
    const claim = interlockedClaim(2172);
    api.setCompletionPermitResponse(true);
    await runnerWith(() => ({ executor: declaring("Follow-up remains") }), gitlab, undefined, undefined, { recoveryRetryMs: 1 }).execute(claim);
    assert.ok(statuses(claim.run_id).includes("failed"));
    assert.ok(!statuses(claim.run_id).includes("completed"));
    assert.equal(api.completionHoldRequests.length, 0);
    assert.equal(failureReason(claim.run_id), TITLE_REASON);
  });

  it("m2 non-closing fails closed, without a hold, when the closing title cannot be rewritten", async () => {
    const { gitlab, pr } = fakeGitlab({ existing: adoptedBody(2172, "Maintainer notes."), existingTitle: CLOSING_TITLE, titleWriteStatus: 403 });
    const claim = interlockedClaim(2172);
    api.setCompletionPermitResponse(true);
    await runnerWith(() => ({ executor: declaring("Follow-up remains") }), gitlab, undefined, undefined, { recoveryRetryMs: 1 }).execute(claim);
    assert.ok(statuses(claim.run_id).includes("failed"));
    assert.ok(!statuses(claim.run_id).includes("completed"));
    assert.equal(api.completionHoldRequests.length, 0);
    assert.equal(failureReason(claim.run_id), TITLE_REASON);
    assert.equal(pr.title, CLOSING_TITLE, "the forge refused the write");
  });

  it("m2 non-closing falls back to a fixed title when the forge refuses the rewritten one (422)", async () => {
    const { gitlab, pr } = fakeGitlab({ existing: adoptedBody(2172, "Maintainer notes."), existingTitle: CLOSING_TITLE, titleWriteStatuses: [422] });
    const claim = gitlabClaim(2172);
    await runner(declaring("Approved follow-up remains"), gitlab).execute(claim);
    assert.ok(statuses(claim.run_id).includes("completed"));
    assert.equal(pr.title, "Work on issue #2172");
  });

  it("m2 non-closing fails closed, without a hold, when the title write succeeds but the re-read still closes", async () => {
    const { gitlab, pr } = fakeGitlab({ existing: adoptedBody(2172, "Maintainer notes."), existingTitle: CLOSING_TITLE, titleWriteIgnored: true });
    const claim = interlockedClaim(2172);
    api.setCompletionPermitResponse(true);
    await runnerWith(() => ({ executor: declaring("Follow-up remains") }), gitlab, undefined, undefined, { recoveryRetryMs: 1 }).execute(claim);
    assert.ok(statuses(claim.run_id).includes("failed"));
    assert.ok(!statuses(claim.run_id).includes("completed"));
    assert.equal(api.completionHoldRequests.length, 0);
    assert.equal(failureReason(claim.run_id), TITLE_REASON);
    assert.equal(pr.title, CLOSING_TITLE);
  });

  it("m2 non-closing legacy run: an unreadable title fails closed with no hold", async () => {
    const { gitlab } = fakeGitlab({ existing: adoptedBody(2172, "Maintainer notes."), omitTitle: true });
    const claim = gitlabClaim(2172);
    await runnerWith(() => ({ executor: declaring("Follow-up remains") }), gitlab, undefined, undefined, { recoveryRetryMs: 1 }).execute(claim);
    assert.ok(statuses(claim.run_id).includes("failed"));
    assert.ok(!statuses(claim.run_id).includes("completed"));
    assert.equal(api.completionHoldRequests.length, 0);
    assert.equal(failureReason(claim.run_id), TITLE_REASON);
  });
  // Issue #2815: owner-partial and #634 scope-capped deliveries never close their issue, so their PR
  // title gets the same neutralisation and fail-closed confirmation as a declared non-closing run.
  const DEFERRED = [{ milestone_id: "m2", title: "Later", reason: "owner deferred" }];
  const partialModes: { name: string; claim: () => ReturnType<typeof gitlabClaim>; exec: () => StubExecutor }[] = [
    { name: "owner-partial interlocked", claim: () => interlockedClaim(2172, { deferred: DEFERRED }), exec: () => declaring() },
    { name: "owner-partial legacy", claim: () => gitlabClaim(2172, { config: { completion_scope: { deferred: DEFERRED } } }), exec: () => declaring() },
    { name: "scope-capped interlocked", claim: () => interlockedClaim(2172), exec: () => declaring(undefined, { completedCount: 1, total: 2 }) },
    { name: "scope-capped legacy", claim: () => gitlabClaim(2172), exec: () => declaring(undefined, { completedCount: 1, total: 2 }) },
  ];

  for (const mode of partialModes) {
    describe(`#2815 ${mode.name}`, () => {
      it("creates its PR with a non-closing [partial] title", async () => {
        const { gitlab, pr, calls } = fakeGitlab();
        const claim = mode.claim();
        claim.issue_title = CLOSING_TITLE;
        api.setCompletionPermitResponse(true);
        await runner(mode.exec(), gitlab).execute(claim);
        assert.ok(statuses(claim.run_id).includes("completed"));
        const create = calls.find(c => c.method === "POST");
        assert.ok(create, "the create POST was recorded");
        const createdTitle = String(JSON.parse(create.body ?? "{}").title);
        assert.equal(closingDirectiveFor(createdTitle, 2172, "org/repo"), false, `create-time title: ${createdTitle}`);
        assert.ok(createdTitle.startsWith("[partial] ") && createdTitle.includes("#2172"), createdTitle);
        assert.equal(closingDirectiveFor(pr.title, 2172, "org/repo"), false);
        assert.ok(pr.title.startsWith("[partial] ") && pr.title.includes("#2172"), pr.title);
      });

      it("retitles an adopted PR whose title closes the issue", async () => {
        const { gitlab, pr, calls } = fakeGitlab({ existing: adoptedBody(2172, "Maintainer notes."), existingTitle: CLOSING_TITLE });
        const claim = mode.claim();
        api.setCompletionPermitResponse(true);
        await runner(mode.exec(), gitlab).execute(claim);
        assert.ok(statuses(claim.run_id).includes("completed"));
        assert.equal(closingDirectiveFor(pr.title, 2172, "org/repo"), false);
        assert.ok(calls.some(c => (c.method === "PUT" || c.method === "PATCH") && JSON.parse(c.body ?? "{}").title !== undefined), "a title write was recorded");
      });

      for (const [label, opts] of [["unreadable", { omitTitle: true }], ["write ignored", { existingTitle: CLOSING_TITLE, titleWriteIgnored: true }]] as const) {
        it(`fails closed, without a hold, when the title is ${label}`, async () => {
          const { gitlab } = fakeGitlab({ existing: adoptedBody(2172, "Maintainer notes."), ...opts });
          const claim = mode.claim();
          api.setCompletionPermitResponse(true);
          await runnerWith(() => ({ executor: mode.exec() }), gitlab, undefined, undefined, { recoveryRetryMs: 1 }).execute(claim);
          assert.ok(statuses(claim.run_id).includes("failed"));
          assert.ok(!statuses(claim.run_id).includes("completed"));
          assert.equal(api.completionHoldRequests.length, 0);
          assert.equal(failureReason(claim.run_id), TITLE_REASON);
        });
      }
    });
  }

  const fullModes: { name: string; claim: () => ReturnType<typeof gitlabClaim> }[] = [
    { name: "full delivery legacy", claim: () => gitlabClaim(2172) },
    { name: "full delivery interlocked", claim: () => interlockedClaim(2172) },
    { name: "accept-only (nothing deferred)", claim: () => gitlabClaim(2172, { config: { completion_scope: { deferred: [], accepted: [{ id: "c1", milestone_id: "m1", text: "crit", reason: "owner accepted" }] } } }) },
  ];
  for (const mode of fullModes) {
    it(`#2815 ${mode.name} keeps the plain closing-style title and writes no title`, async () => {
      const { gitlab, calls } = fakeGitlab();
      const claim = mode.claim();
      claim.issue_title = CLOSING_TITLE;
      api.setCompletionPermitResponse(true);
      await runner(declaring(), gitlab).execute(claim);
      assert.ok(statuses(claim.run_id).includes("completed"));
      const create = calls.find(c => c.method === "POST");
      assert.ok(create, "the create POST was recorded");
      assert.equal(JSON.parse(create.body ?? "{}").title, CLOSING_TITLE);
      assert.ok(!calls.some(c => c.method !== "POST" && c.method !== "GET" && "title" in JSON.parse(c.body ?? "{}") && !("description" in JSON.parse(c.body ?? "{}"))),
        "no title-only write");
    });
  }
});
