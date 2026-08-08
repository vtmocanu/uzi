import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { makeClaim, nullLogger } from "./helpers.js";
import { StubExecutor } from "../src/executor.js";
import type { ClaimResponse, RunKind } from "../src/protocol.js";
import {
  api,
  fx,
  fakeGitlab,
  installHarness,
  runner,
} from "./runner-harness.js";

installHarness();

// A schedule-fired ad-hoc prompt run (PRD #241 Decision 10 / M8): kind="prompt",
// ISSUE-LESS (issue_iid null), repo set, and issue_description carrying the schedule's
// stored task text. It must claim + execute on a derived per-run branch and open an MR
// whose title + description never render `#null` — the ci_fix shape, not the issue path.
function promptClaim(overrides: Partial<ClaimResponse> = {}): ClaimResponse {
  return makeClaim({
    kind: "prompt",
    issue_iid: null,
    issue_title: "Hunt for flaky tests and open an MR",
    issue_description:
      "Find flaky tests across the poller package and open a merge request.",
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

describe("RunRunner — ad-hoc prompt kind (PRD #241 M8)", () => {
  it("the RunKind union accepts \"prompt\"", () => {
    // Compile-time proof the kind is in the union (the assignment fails typecheck
    // otherwise); the runtime assert keeps the test non-empty.
    const k: RunKind = "prompt";
    assert.equal(k, "prompt");
  });

  it("clones onto a derived per-run branch and opens an MR with no #null text", async () => {
    const { gitlab, calls } = fakeGitlab();
    const claim = promptClaim();
    await runner(new StubExecutor(nullLogger()), gitlab).execute(claim);

    // The run completed via the seam (not the issue-path throw), on the derived branch.
    const completed = api.states.find(
      (s) => s.runId === claim.run_id && s.body.status === "completed",
    )!.body;
    assert.equal(completed.branch, `uzi/prompt-${claim.run_id}`);
    assert.equal(completed.mr_iid, 42);

    // The MR the worker opened.
    assert.equal(calls.length, 1);
    const body = JSON.parse(calls[0]!.body ?? "{}");
    assert.equal(body.source_branch, `uzi/prompt-${claim.run_id}`);

    // Title comes from the scheduler-derived issue_title — never `Resolve issue #null`.
    assert.equal(body.title, "Hunt for flaky tests and open an MR");
    assert.ok(!String(body.title).includes("#null"));

    // The description references the ad-hoc prompt run and, crucially, never renders
    // `#null` and never `Closes` an issue (there is none).
    assert.ok(body.description.includes("Ad-hoc scheduled prompt run"));
    assert.ok(!body.description.includes("#null"), "no #null in MR body");
    assert.ok(!/Closes #/.test(body.description), "prompt run closes no issue");
  });

  it("falls back to a non-#null title when the scheduler set no title", async () => {
    const { gitlab, calls } = fakeGitlab();
    const claim = promptClaim({ issue_title: "" });
    await runner(new StubExecutor(nullLogger()), gitlab).execute(claim);

    const body = JSON.parse(calls[0]!.body ?? "{}");
    assert.equal(body.title, "Scheduled prompt run");
    assert.ok(!String(body.title).includes("#null"));
  });
});
