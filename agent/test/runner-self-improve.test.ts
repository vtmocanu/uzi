import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import { makeClaim, nullLogger } from "./helpers.js";
import { StubExecutor } from "../src/executor.js";
import type { ClaimResponse } from "../src/protocol.js";
import type { CheckRunner } from "../src/self-improve.js";
import { api, fx, fakeGitlab, installHarness, runner } from "./runner-harness.js";

installHarness();

// CHARACTERIZATION test (PRD #983 M4a). This pins TODAY's `self_improve` behaviour
// against the CURRENT, unmodified runner.ts, BEFORE any refactor — so a later
// refactor that changes an observable trips exactly one of these assertions. It is
// deliberately test-only and touches no `agent/src/` file.
//
// A self_improve claim (unlike mr_rework): kind="self_improve", issue_iid SET (it
// carries a stable tracking issue), repo set, `branch` UNSET (worker-derived to a
// fresh-per-cycle `uzi/self-improve/<runId>`), and optionally self_improve_dogfood.
function selfImproveClaim(overrides: Partial<ClaimResponse> = {}): ClaimResponse {
  const runId = (overrides.run_id as string | undefined) ?? randomUUID();
  return makeClaim({
    run_id: runId,
    kind: "self_improve",
    issue_iid: 77,
    issue_title: "Self-improvement cycle",
    issue_description: "Pick one top improvement and land it.",
    // branch deliberately UNSET: a self_improve run derives its own fresh-per-cycle
    // branch from the run id in runnerCloneForClaim.
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

// A CheckRunner that reports every check "skipped" without spawning go/npm. The
// dogfood evidence block runs SELF_IMPROVE_CHECKS against uzi's own layout, which
// the fixture repo does not have; injecting this keeps the dogfood test hermetic and
// fast. It does NOT affect the observable we pin (the status emit fires BEFORE any
// check runs), it only avoids spawning real toolchains in the fixture.
const skipAllChecks: CheckRunner = async (check) => ({
  name: check.name,
  status: "skipped",
  detail: "characterization test: not run",
});

describe("RunRunner — self_improve kind (PRD #46 / characterization, PRD #983 M4a)", () => {
  it("clones the fresh-per-cycle uzi/self-improve/<runId> branch and COMPLETES", async () => {
    const { gitlab } = fakeGitlab();
    const claim = selfImproveClaim();
    await runner(new StubExecutor(nullLogger()), gitlab).execute(claim);

    const completed = api.states.find(
      (s) => s.runId === claim.run_id && s.body.status === "completed",
    );
    assert.ok(completed, "the self_improve run completed (did not fail)");
    // The StubExecutor returns ctx.branch (= the runner clone's branch), so the
    // completed state's branch proves runnerCloneForClaim derived the exact
    // fresh-per-cycle self-improve branch (selfImproveBranch(runId)).
    assert.equal(completed!.body.branch, `uzi/self-improve/${claim.run_id}`);

    // The run must NOT have failed.
    const failed = api.states.find(
      (s) => s.runId === claim.run_id && s.body.status === "failed",
    );
    assert.equal(failed, undefined, "a self_improve run must not fail here");
  });

  it("opens an MR whose body links (not closes) the tracking issue", async () => {
    // openMr is true for a non-task kind, so the finalize path runs
    // createMergeRequest and the fake returns a fresh 201 — so we inspect the POST
    // body's description directly (the exact path that renders the self_improve MR
    // description). Dogfood is OFF here so selfImproveSection is unset ("") and the
    // description is just the self_improve preamble + tracking line.
    const { gitlab, calls } = fakeGitlab();
    const claim = selfImproveClaim();
    await runner(new StubExecutor(nullLogger()), gitlab).execute(claim);

    assert.equal(calls.length, 1, "the self_improve finalize opened exactly one MR");
    const body = JSON.parse(calls[0]!.body ?? "{}");
    assert.equal(body.source_branch, `uzi/self-improve/${claim.run_id}`);
    const description = String(body.description);
    // Starts "Autonomous self-improvement change (PRD #46)..." — matches /self-improvement/i.
    assert.match(description, /self-improvement/i);
    // References the stable tracking issue by its iid.
    assert.match(description, /Tracking issue: #77/);
    // A self_improve MR references but does NOT close its tracking issue.
    assert.ok(
      !/Closes #/.test(description),
      "a self_improve MR closes no issue",
    );
  });

  it("gathers the dogfood test-evidence ONLY when self_improve_dogfood is true", async () => {
    // OBSERVABLE (proxy) CHOICE: the `selfImproveSection` gather runs the full
    // SELF_IMPROVE_CHECKS suite, which shells out to go/npm against uzi's own repo
    // layout and is not reachable in this fixture harness. The nearest reliable
    // observable the harness DOES surface is the worker status emit
    // "self-improvement: running the test suites for MR evidence", emitted on the
    // exact line the dogfood guard controls
    // (`if (claim.kind === "self_improve" && claim.self_improve_dogfood)`), BEFORE
    // any check runs. Pinning it pins the iff directly and hermetically.
    const statusTexts = (runId: string): string[] =>
      api
        .messages(runId)
        .filter((m) => m.kind === "status")
        .map((m) => String(m.payload.text));
    const evidenceEmit = /running the test suites for MR evidence/;

    // Direction 1: dogfood TRUE → the evidence emit appears, run still completes.
    // The stub CheckRunner (injected via the runner factory's RunnerOptions) keeps
    // the check step hermetic; it does not gate the emit, which fires before it.
    {
      const { gitlab } = fakeGitlab();
      const claim = selfImproveClaim({ self_improve_dogfood: true });
      await runner(new StubExecutor(nullLogger()), gitlab, undefined, {
        checkRunner: skipAllChecks,
      }).execute(claim);
      assert.ok(
        statusTexts(claim.run_id).some((t) => evidenceEmit.test(t)),
        "dogfood=true must emit the test-evidence status",
      );
      assert.ok(
        api.states.some(
          (s) => s.runId === claim.run_id && s.body.status === "completed",
        ),
        "dogfood=true run still completes",
      );
    }

    // Direction 2: dogfood FALSE (absent) → the evidence emit does NOT appear.
    {
      const { gitlab } = fakeGitlab();
      const claim = selfImproveClaim(); // self_improve_dogfood absent ⇒ falsy
      await runner(new StubExecutor(nullLogger()), gitlab, undefined, {
        checkRunner: skipAllChecks,
      }).execute(claim);
      assert.ok(
        !statusTexts(claim.run_id).some((t) => evidenceEmit.test(t)),
        "dogfood off must NOT emit the test-evidence status",
      );
    }
  });
});
