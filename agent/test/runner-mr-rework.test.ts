import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { randomUUID } from "node:crypto";
import { makeClaim, nullLogger } from "./helpers.js";
import { StubExecutor } from "../src/executor.js";
import type { ClaimResponse } from "../src/protocol.js";
import { api, fx, fakeGitlab, installHarness, runner } from "./runner-harness.js";

installHarness();

// An mr_rework run (PRD #700 / issue #778): kind="mr_rework", ISSUE-LESS (issue_iid
// null), repo set, and `branch` carrying the MR's EXISTING branch, which the server
// now sources from the run's pipeline_ref (e.g. agent/issue-42). The worker clones THAT
// branch, folds its rework onto it, pushes back, and (openMr is true for a non-task
// kind) idempotently adopts the existing MR.
function mrReworkClaim(overrides: Partial<ClaimResponse> = {}): ClaimResponse {
  const runId = (overrides.run_id as string | undefined) ?? randomUUID();
  return makeClaim({
    run_id: runId,
    kind: "mr_rework",
    issue_iid: null,
    issue_title: "Rework: address MR review",
    issue_description: "Apply the reviewer's feedback on the open MR.",
    branch: "agent/issue-42",
    base_branch: "main",
    repo: {
      id: "r1",
      url: "https://gitlab.example.test/org/repo",
      clone_url: fx.originPath,
    },
    last_seq: 0,
    secrets: {
      forge_pat: "fixture-forge-pat-000000",
      anthropic_oauth_token: "dummy-oauth-do-not-scan",
    },
    ...overrides,
  });
}

describe("RunRunner — mr_rework kind (PRD #700 / issue #778)", () => {
  it("resolves the MR branch and does NOT throw the issue_iid error", async () => {
    const { gitlab } = fakeGitlab();
    const claim = mrReworkClaim();
    await runner(new StubExecutor(nullLogger()), gitlab).execute(claim);

    // The run reached completion, NOT failure — the mr_rework arm cloned claim.branch
    // instead of falling through to the issue path that throws on a NULL issue_iid.
    const completed = api.states.find(
      (s) => s.runId === claim.run_id && s.body.status === "completed",
    );
    assert.ok(completed, "the mr_rework run completed");
    // The StubExecutor returns ctx.branch (= the runner clone's branch), so a completion
    // on agent/issue-42 proves the mr_rework arm cloned claim.branch, not the issue path.
    assert.equal(completed!.body.branch, "agent/issue-42");

    // The exact regression (#778): the run must NOT have failed with the issue_iid error.
    const issueIidFailure = api.states.find(
      (s) =>
        s.runId === claim.run_id &&
        s.body.status === "failed" &&
        /issue run claim is missing issue_iid/.test(s.body.failure_reason ?? ""),
    );
    assert.equal(
      issueIidFailure,
      undefined,
      "an mr_rework run must not fall through to the issue_iid error",
    );
  });

  it("pushes the rework onto the MR branch on origin", async () => {
    const { gitlab } = fakeGitlab();
    const claim = mrReworkClaim();
    await runner(new StubExecutor(nullLogger()), gitlab).execute(claim);

    // The push landed on the MR branch — a real git push to the fixture bare origin.
    const log = execFileSync(
      "git",
      ["-C", fx.originPath, "log", "--oneline", "agent/issue-42"],
      { encoding: "utf8" },
    );
    assert.ok(
      log.includes("uzi stub"),
      "the worker's commit landed on the MR branch",
    );
  });

  it("throws a clear error when an mr_rework claim is missing its branch", async () => {
    const { gitlab } = fakeGitlab();
    // An mr_rework run must always carry its MR branch (server-sourced from pipeline_ref);
    // a missing one is a create-time bug the worker must surface rather than fall through.
    const claim = mrReworkClaim({ branch: undefined });
    await runner(new StubExecutor(nullLogger()), gitlab).execute(claim);

    const failed = api.states.find(
      (s) => s.runId === claim.run_id && s.body.status === "failed",
    );
    assert.ok(failed, "a branchless mr_rework run failed");
    assert.match(failed!.body.failure_reason ?? "", /missing its MR branch/);
  });

  it("opens the MR from the rework branch with no #null in title or body", async () => {
    // openMr is true for a non-task kind, so the finalize path runs createMergeRequest.
    // The fake returns a fresh 201 (not the 409-adopt path production usually takes), so
    // this exercises the description builder directly — the exact path where a missing
    // mr_rework arm would render `Implements issue #null` / `Closes #null`.
    const { gitlab, calls } = fakeGitlab();
    // Empty issue_title forces mrTitle past its trimmed-title branch to the empty-title
    // fallback, so the "#null" title assertion below actually exercises the mr_rework arm
    // rather than returning the fixture's non-empty title (which made it vacuous).
    const claim = mrReworkClaim({ issue_title: "" });
    await runner(new StubExecutor(nullLogger()), gitlab).execute(claim);

    assert.equal(calls.length, 1, "the mr_rework finalize opened exactly one MR");
    const body = JSON.parse(calls[0]!.body ?? "{}");
    assert.equal(body.source_branch, "agent/issue-42");
    assert.ok(!String(body.title).includes("#null"), "no #null in the MR title");
    assert.ok(!String(body.description).includes("#null"), "no #null in the MR body");
    assert.ok(
      !/Closes #/.test(String(body.description)),
      "an mr_rework MR closes no issue",
    );
    assert.match(String(body.description), /rework/i);
  });
});
