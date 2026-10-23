import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { StubExecutor, type Executor, type ExecutorResult, type RunContext } from "../src/executor.js";
import { nullLogger } from "./helpers.js";
import { followSuccessfulPush } from "./publication-fixture.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, installHarness, runner, runnerWith } from "./runner-harness.js";

installHarness();

// #3360: an ISSUE run that declares report_only after publishing a checkpoint is delivered as a
// non-closing merge request. These tests drive the real RunRunner.execute() with hasCommittedCheckpoint
// forced true (the published checkpoint) and a StubExecutor that commits real work.

const FEATURES = ["claim_generation_fence", "completion_non_closing_v1", "refresh_publication_context_v1"];
const STATIC_REASON = "The lead declared report_only after committing work; the worker delivered it without closing the issue.";
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];

function reportOnlyExecutor(opts: { message?: string; extra?: Partial<ExecutorResult>; reportOnly?: boolean } = {}): Executor {
  return new class extends StubExecutor {
    override async run(ctx: RunContext): Promise<ExecutorResult> {
      const result = await super.run(ctx);
      if (opts.message !== undefined) {
        fs.writeFileSync(path.join(ctx.worktreePath, "closing.txt"), "work\n");
        execFileSync("git", ["-C", ctx.worktreePath, "add", "closing.txt"]);
        execFileSync("git", ["-C", ctx.worktreePath, ...IDENT, "commit", "-m", opts.message]);
      }
      return { ...result, ...(opts.reportOnly === false ? {} : { reportOnly: true, summary: "verified" }), ...opts.extra };
    }
  }(nullLogger());
}

async function negotiate(features: string[] = FEATURES): Promise<void> {
  api.setRegisterProtocolFeatures(features);
  await client.register("fallback");
}

function interlockedClaim(iid: number, extraConfig: Record<string, unknown> = {}) {
  const claim = gitlabClaim(iid, {
    claim_generation: 7,
    config: { completion_contract_version: 1, contract_revision: 1, ...extraConfig },
  });
  api.setPublicationState(claim.run_id, [], 1);
  return claim;
}

function setup() {
  git.hasCommittedCheckpoint = (async () => true) as typeof git.hasCommittedCheckpoint;
  const forge = fakeGitlab();
  const pushed = followSuccessfulPush(git, forge.pr);
  return { forge, pushed };
}

function descriptions(calls: { method: string; body?: string }[]): string[] {
  return calls.filter((c) => c.method === "POST" || c.method === "PUT")
    .map((c) => String((JSON.parse(c.body ?? "{}") as { description?: unknown }).description ?? ""));
}

function terminal(runId: string, status: string) {
  return api.states.find((s) => s.runId === runId && s.body.status === status)?.body;
}

function originHasBranch(iid: number): boolean {
  try {
    execFileSync("git", ["-C", fx.originPath, "rev-parse", "--verify", `refs/heads/agent/issue-${iid}`], { stdio: "ignore" });
    return true;
  } catch { return false; }
}

describe("report_only fallback to non-closing delivery (#3360)", () => {
  it("interlocked + completion_non_closing_v1: records one forced attempt then delivers non-closing", async () => {
    await negotiate();
    const { forge, pushed } = setup();
    const claim = interlockedClaim(3360);
    api.setCompletionPermitResponse(true);
    await runner(reportOnlyExecutor(), forge.gitlab).execute(claim);

    assert.equal(api.completionAttemptRequests.length, 1, "exactly one fallback attempt");
    const attempt = api.completionAttemptRequests[0]!;
    assert.deepEqual(attempt.milestones_completed, []);
    assert.equal(attempt.worktree_fingerprint, null);
    assert.deepEqual(attempt.non_closing, { reason: STATIC_REASON });
    assert.notEqual(pushed(), "", "the branch was pushed");
    const done = terminal(claim.run_id, "completed");
    assert.ok(done, JSON.stringify(api.states.map((s) => s.body)));
    assert.notEqual(done.report_only, true);
    assert.equal(done.source_publication?.non_closing, true);
    for (const d of descriptions(forge.calls)) assert.doesNotMatch(d, /Closes #3360/);
    assert.match(forge.pr.description, /Issue remains open/);
  });

  for (const [label, configure, expectOrigin, expectedAttempts] of [
    ["non-stale 4xx", () => { api.completionAttemptHttpStatus = 400; }, false, 1],
    ["transient 5xx exhausting the retries", () => { api.completionAttemptHttpStatus = 503; }, false, 3],
  ] as const) {
    it(`fails closed with the #299 reason when the forced attempt hits a ${label}: no push, no MR`, async () => {
      await negotiate();
      const { forge, pushed } = setup();
      configure();
      const claim = interlockedClaim(3361);
      api.setCompletionPermitResponse(true);
      await runner(reportOnlyExecutor(), forge.gitlab).execute(claim);

      assert.equal(api.completionAttemptRequests.length, expectedAttempts);
      const failed = terminal(claim.run_id, "failed");
      assert.ok(failed, JSON.stringify(api.states.map((s) => s.body)));
      assert.match(failed.failure_reason ?? "", /report_only/);
      assert.match(failed.failure_reason ?? "", /checkpoint/);
      assert.equal(pushed(), "", "nothing was pushed");
      assert.equal(originHasBranch(3361), expectOrigin);
      assert.equal(forge.all.length, 0, "no MR opened");
    });
  }

  it("a stale 409 on the forced attempt stops without the #299 failure, a push or an MR", async () => {
    await negotiate();
    const { forge, pushed } = setup();
    api.completionAttemptHttpStatus = 409;
    const claim = interlockedClaim(3362);
    api.setCompletionPermitResponse(true);
    await runner(reportOnlyExecutor(), forge.gitlab).execute(claim);

    assert.equal(api.completionAttemptRequests.length, 1, "a stale claim stops immediately");
    assert.equal(api.states.some((s) => /report_only/.test(s.body.failure_reason ?? "")), false);
    assert.equal(terminal(claim.run_id, "completed"), undefined);
    assert.equal(pushed(), "");
    assert.equal(forge.all.length, 0);
  });

  it("interlocked WITHOUT completion_non_closing_v1 records no fallback attempt and still delivers non-closing", async () => {
    await negotiate(["claim_generation_fence", "refresh_publication_context_v1"]);
    const { forge } = setup();
    const claim = interlockedClaim(3363);
    api.setCompletionPermitResponse(true);
    await runner(reportOnlyExecutor(), forge.gitlab).execute(claim);

    assert.equal(api.completionAttemptRequests.length, 0, "no extra attempt from the fallback");
    assert.equal(api.states.some((s) => /report_only/.test(s.body.failure_reason ?? "")), false);
    for (const d of descriptions(forge.calls)) assert.doesNotMatch(d, /Closes #3363/);
    assert.match(forge.pr.description, /Issue remains open/);
  });

  it("permit denied holds with the attempt carrying non_closing; the resume delivers non-closing from stored config", async () => {
    await negotiate();
    const { forge } = setup();
    const claim = interlockedClaim(3364);
    api.setCompletionPermitResponse(false, { denyReason: "missing_milestones" });
    git.checkpointPack = (async () => null) as typeof git.checkpointPack;
    let executor = reportOnlyExecutor();
    const worker = runnerWith(() => ({ executor }), forge.gitlab, undefined, undefined, { recoveryRetryMs: 1 });
    await worker.execute(claim);

    assert.deepEqual(api.completionAttemptRequests[0]?.non_closing, { reason: STATIC_REASON });
    assert.equal(api.completionHoldRequests.length, 1, "the run is held");
    assert.equal(forge.all.length, 0, "no MR while held");
    assert.equal(terminal(claim.run_id, "completed"), undefined);

    // Resume: the stored declaration rides config.non_closing; the executor returns a plain result.
    const resumed = interlockedClaim(3364, { non_closing: { reason: STATIC_REASON } });
    resumed.run_id = claim.run_id;
    api.setPublicationState(resumed.run_id, [], 1);
    api.setOwnershipStatus(resumed.run_id, "running", 7); // the owner resumed the held run
    api.setCompletionPermitResponse(true);
    executor = reportOnlyExecutor({ reportOnly: false });
    await worker.execute(resumed);
    assert.ok(terminal(resumed.run_id, "completed"), JSON.stringify(api.states.map((s) => s.body)));
    for (const d of descriptions(forge.calls)) assert.doesNotMatch(d, /Closes #3364/);
    assert.match(forge.pr.description, /Issue remains open/);
  });

  it("a candidate whose commit message closes the issue is refused before push", async () => {
    await negotiate();
    const { forge, pushed } = setup();
    const claim = gitlabClaim(3365, { claim_generation: 7 });
    await runner(reportOnlyExecutor({ message: "Closes #3365" }), forge.gitlab).execute(claim);

    const failed = terminal(claim.run_id, "failed");
    assert.ok(failed, JSON.stringify(api.states.map((s) => s.body)));
    assert.match(failed.failure_reason ?? "", /non-closing delivery refused: commit message\(s\) close issue #3365/);
    assert.equal(pushed(), "", "refused before push");
    assert.equal(originHasBranch(3365), false);
    assert.equal(forge.all.length, 0, "no MR");
  });

  it("code_cross_check_required: the MR body discloses the cross-check as skipped, even when a completed row exists", async (t) => {
    await negotiate();
    const { forge } = setup();
    const completed: import("../src/code-cross-check-contract.js").CodeCrossCheckRecord = {
      stage: "code", round: 1, candidate_generation: 1, head_commit: "b".repeat(40), base_commit: "a".repeat(40),
      candidate_digest: "c".repeat(64), checker_run_id: "child", checker_harness: "codex", checker_model: "test-checker",
      checker_effort: "high", outcome: "completed", reason_class: null, findings: [], dispositions: null,
      finalized_at: new Date().toISOString(), interrupted_at: null, deadline_at: new Date().toISOString(),
    };
    t.mock.method(client, "codeCrossCheckStatus", async () => completed);
    const claim = gitlabClaim(3366, { claim_generation: 7, code_cross_check_required: true });
    await runner(reportOnlyExecutor(), forge.gitlab).execute(claim);

    assert.ok(terminal(claim.run_id, "completed"), JSON.stringify(api.states.map((s) => s.body)));
    assert.match(forge.pr.description, /Code cross-check incomplete: skipped: report\\?_only was declared/);
    assert.doesNotMatch(forge.pr.description, /checked `/);
    assert.doesNotMatch(forge.pr.description, /Closes #3366/);
  });

  it("a config.non_closing declaration keeps its own reason in the attempt and the MR body", async () => {
    await negotiate();
    const { forge } = setup();
    const reason = "Config declared reason A: maintainer owns the remaining milestones.";
    const claim = interlockedClaim(3367, { non_closing: { reason } });
    api.setCompletionPermitResponse(true);
    await runner(reportOnlyExecutor(), forge.gitlab).execute(claim);

    assert.equal(api.completionAttemptRequests.length, 1);
    assert.deepEqual(api.completionAttemptRequests[0]!.non_closing, { reason });
    assert.ok(terminal(claim.run_id, "completed"), JSON.stringify(api.states.map((s) => s.body)));
    assert.ok(forge.pr.description.includes(reason), forge.pr.description);
    assert.equal(forge.pr.description.includes(STATIC_REASON), false);
  });

  it("an executor-returned nonClosing reason is not overwritten by the static fallback reason", async () => {
    await negotiate();
    const { forge } = setup();
    const reason = "Executor declared reason B: follow-up work remains.";
    const claim = interlockedClaim(3368);
    api.setCompletionPermitResponse(true);
    await runner(reportOnlyExecutor({ extra: { nonClosing: { reason } } }), forge.gitlab).execute(claim);

    assert.equal(api.completionAttemptRequests.length, 1);
    assert.deepEqual(api.completionAttemptRequests[0]!.non_closing, { reason });
    assert.ok(terminal(claim.run_id, "completed"), JSON.stringify(api.states.map((s) => s.body)));
    assert.ok(forge.pr.description.includes(reason), forge.pr.description);
    assert.equal(forge.pr.description.includes(STATIC_REASON), false);
  });

  it("a non-issue run declaring report_only after a published checkpoint keeps the #299 failure (no fallback)", async () => {
    await negotiate();
    const { forge, pushed } = setup();
    const claim = gitlabClaim(3369, { kind: "prompt", claim_generation: 7 });
    await runner(reportOnlyExecutor(), forge.gitlab).execute(claim);

    const failed = terminal(claim.run_id, "failed");
    assert.ok(failed, JSON.stringify(api.states.map((s) => s.body)));
    assert.match(failed.failure_reason ?? "", /report_only/);
    assert.match(failed.failure_reason ?? "", /checkpoint/);
    assert.equal(pushed(), "", "nothing was pushed");
    assert.equal(forge.all.length, 0, "no MR opened");
    assert.equal(api.completionAttemptRequests.length, 0);
    assert.equal(api.states.some((s) => s.runId === claim.run_id && s.body.source_publication?.non_closing === true), false);
    assert.equal(api.states.some((s) => /will attempt non-closing delivery/.test(JSON.stringify(s.body))), false);
  });
});
