import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { randomUUID } from "node:crypto";
import { makeClaim, nullLogger } from "./helpers.js";
import { StubExecutor } from "../src/executor.js";
import type { ClaimResponse, RunKind } from "../src/protocol.js";
import { api, fx, fakeGitlab, installHarness, runner } from "./runner-harness.js";

installHarness();

// A handoff task run (PRD #400 M2): kind="task", ISSUE-LESS (issue_iid null), repo set,
// issue_description carrying the inline context, and `branch` carrying the PRE-SEEDED,
// server-named uzi/task/<run-id> name the CLI (M3) already pushed the user's HEAD to.
// The worker clones THAT branch, works, pushes back — and opens an MR only when open_mr.
function taskClaim(overrides: Partial<ClaimResponse> = {}): ClaimResponse {
  const runId = (overrides.run_id as string | undefined) ?? randomUUID();
  return makeClaim({
    run_id: runId,
    kind: "task",
    issue_iid: null,
    issue_title: "Handoff: tidy the poller",
    issue_description: "Refactor the poller for readability; I'll pull the branch.",
    branch: `uzi/task/${runId}`,
    base_branch: "develop",
    open_mr: false,
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

describe("RunRunner — handoff task kind (PRD #400 M2)", () => {
  it("the RunKind union accepts \"task\"", () => {
    // Compile-time proof the kind is in the union (the assignment fails typecheck
    // otherwise); the runtime assert keeps the test non-empty.
    const k: RunKind = "task";
    assert.equal(k, "task");
  });

  it("clones the pre-seeded uzi/task/<id> branch, pushes it back, opens NO MR when open_mr is false", async () => {
    const { gitlab, calls } = fakeGitlab();
    const claim = taskClaim({ open_mr: false });
    await runner(new StubExecutor(nullLogger()), gitlab).execute(claim);

    // (a) The clone used claim.branch — the task arm of runnerCloneForClaim, not a
    // derived name. The StubExecutor returns ctx.branch (= the runner clone's branch),
    // so a completion on uzi/task/<id> proves the pre-seeded branch was the working ref.
    const completed = api.states.find(
      (s) => s.runId === claim.run_id && s.body.status === "completed",
    )!.body;
    assert.equal(completed.branch, `uzi/task/${claim.run_id}`);

    // (b) The push WAS performed — the branch really landed on origin (the deliverable
    // the user pulls). This is a real git push to the fixture bare, not a mock.
    const log = execFileSync(
      "git",
      ["-C", fx.originPath, "log", "--oneline", `uzi/task/${claim.run_id}`],
      { encoding: "utf8" },
    );
    assert.ok(log.includes("uzi stub"), "the worker's commit landed on the task branch");

    // (b, negative) The MR-open function was NOT called — no createMergeRequest POST
    // reached the forge transport. Paired with the push assertion above, this makes the
    // no-MR claim non-vacuous: it pushed AND it opened nothing.
    assert.equal(calls.length, 0, "a no-MR task opens no merge request");
    assert.ok(
      !("mr_iid" in completed) || completed.mr_iid === undefined,
      "no MR iid on a no-MR task completion",
    );
    // It did NOT take the report_only path — a task still pushes, so report_only stays unset.
    assert.ok(
      !("report_only" in completed) || completed.report_only !== true,
      "a no-MR task completes normally, not report-only",
    );
  });

  it("opens an MR when open_mr is true, still on the uzi/task/<id> branch", async () => {
    const { gitlab, calls } = fakeGitlab();
    const claim = taskClaim({ open_mr: true });
    await runner(new StubExecutor(nullLogger()), gitlab).execute(claim);

    const completed = api.states.find(
      (s) => s.runId === claim.run_id && s.body.status === "completed",
    )!.body;
    assert.equal(completed.branch, `uzi/task/${claim.run_id}`);
    assert.equal(completed.mr_iid, 42, "an --mr task opens exactly one merge request");

    // Exactly one MR was opened, sourced from the task branch — never `Resolve issue #null`.
    assert.equal(calls.length, 1);
    const body = JSON.parse(calls[0]!.body ?? "{}");
    assert.equal(body.source_branch, `uzi/task/${claim.run_id}`);
    assert.equal(body.title, "Handoff: tidy the poller");
    assert.ok(!String(body.title).includes("#null"));
    assert.ok(body.description.includes("Handoff task"));
    assert.ok(!body.description.includes("#null"), "no #null in the MR body");
    assert.ok(!/Closes #/.test(body.description), "a task closes no issue");
  });

  it("throws a clear error when a task claim is missing its branch", async () => {
    const { gitlab } = fakeGitlab();
    // A task must always carry its server-named branch; a missing one is a create-time
    // bug the worker must surface rather than invent a push destination for.
    const claim = taskClaim({ branch: undefined });
    await runner(new StubExecutor(nullLogger()), gitlab).execute(claim);

    const failed = api.states.find(
      (s) => s.runId === claim.run_id && s.body.status === "failed",
    )!.body;
    assert.match(failed.failure_reason ?? "", /missing its branch/);
  });
});
