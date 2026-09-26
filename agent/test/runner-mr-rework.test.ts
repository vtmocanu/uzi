import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { randomUUID } from "node:crypto";
import { makeClaim, nullLogger } from "./helpers.js";
import { StubExecutor } from "../src/executor.js";
import type { ClaimResponse } from "../src/protocol.js";
import { api, fx, git, fakeGitlab, installHarness, runner } from "./runner-harness.js";

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
    // Pin the exact empty-title fallback mrTitle returns for an mr_rework claim; the
    // negative `#null` checks below stay as supplementary guards but would pass for any
    // wrong-but-not-`#null` title, so this equality is the load-bearing assertion.
    assert.equal(body.title, "MR rework");
    assert.ok(!String(body.title).includes("#null"), "no #null in the MR title");
    assert.ok(!String(body.description).includes("#null"), "no #null in the MR body");
    assert.ok(
      !/Closes #/.test(String(body.description)),
      "an mr_rework MR closes no issue",
    );
    assert.match(String(body.description), /rework/i);
  });

  // issue #1117 — the concurrent-writer branch_moved disposition. These drive REAL git against
  // the fixture origin: the origin's agent/issue-42 tip actually changes between clone and the
  // finalize push, so the detection's `originBranchTip`, `fetchDefaultTip`, and `isAncestorRef`
  // run for real. IDENT/gitOrigin mirror git.test.ts's non-fast-forward recipe.
  const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];
  const gitOrigin = (args: string[]): string =>
    execFileSync("git", ["-C", fx.originPath, ...args], {
      encoding: "utf8",
      env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" },
    }).trim();

  it("reports branch_moved (NOT agent_failure) when a concurrent writer advanced the MR branch, leaving the branch intact", async () => {
    // The pre-existing MR branch agent/issue-42 sits at O on origin. R is a child of O kept on a
    // side branch so it stays referenced — the commit a concurrent same-branch writer (a human,
    // or uzi-watcher landing review fixes) will land on the MR branch during the finalize push.
    gitOrigin(["checkout", "-b", "agent/issue-42"]);
    gitOrigin([...IDENT, "commit", "--allow-empty", "-m", "O: pre-existing MR branch tip"]);
    const shaO = gitOrigin(["rev-parse", "HEAD"]);
    gitOrigin(["checkout", "-b", "concurrent-writer"]);
    gitOrigin([...IDENT, "commit", "--allow-empty", "-m", "R: concurrent writer advanced the MR branch"]);
    const shaR = gitOrigin(["rev-parse", "HEAD"]);
    // Back to main so agent/issue-42 is not the checked-out branch; it is left at O (its tip when
    // the worker clones). Precondition: R strictly descends from O — a genuine forward advance.
    gitOrigin(["checkout", "main"]);
    assert.equal(gitOrigin(["rev-parse", "refs/heads/agent/issue-42"]), shaO, "precondition: the MR branch is at O at clone time");
    assert.notEqual(shaR, shaO, "precondition: R is distinct from O");

    // Wrap pushBranch so the FIRST finalize push of agent/issue-42 first lands R on origin
    // (advancing the tip to a descendant of O), then performs the REAL non-forced push — which
    // now genuinely rejects non-fast-forward. `git` is reassigned per-test by installHarness().
    const originalPush = git.pushBranch.bind(git);
    let advanced = false;
    git.pushBranch = (async (bare: string, branch: string, pat: string, url: string, user?: string) => {
      if (!advanced && branch === "agent/issue-42") {
        advanced = true;
        gitOrigin(["update-ref", "refs/heads/agent/issue-42", shaR]);
      }
      return originalPush(bare, branch, pat, url, user);
    }) as typeof git.pushBranch;

    const { gitlab, calls } = fakeGitlab();
    const claim = mrReworkClaim();
    try {
      await runner(new StubExecutor(nullLogger()), gitlab).execute(claim);
    } finally {
      git.pushBranch = originalPush;
    }

    // The terminal report is the branch_moved disposition — status failed + branch_moved:true —
    // NOT the generic agent_failure (which sets no branch_moved and defaults fail_origin).
    const terminal = api.states.find(
      (s) => s.runId === claim.run_id && s.body.status === "failed",
    );
    assert.ok(terminal, "the superseded run reported a terminal failed state");
    assert.equal(terminal!.body.branch_moved, true, "the failed report declares branch_moved:true");
    assert.equal(
      terminal!.body.fail_origin,
      undefined,
      "branch_moved rides a plain failed report with no fail_origin — the server owns the disposition",
    );
    const completed = api.states.find(
      (s) => s.runId === claim.run_id && s.body.status === "completed",
    );
    assert.equal(completed, undefined, "a superseded rework does not complete");

    // The branch and the concurrent commit are intact: origin's agent/issue-42 still equals R —
    // the run never force-pushed over the concurrent writer's work.
    assert.equal(
      gitOrigin(["rev-parse", "refs/heads/agent/issue-42"]),
      shaR,
      "origin's MR branch is left at the concurrent commit R (no force-push)",
    );
    // No MR was opened: the run failed at the push, before the finalize MR step.
    assert.equal(calls.length, 0, "a superseded rework opens no merge request");
  });

  it("falls through to the generic failure when the non-ff is a divergent (not forward-advanced) tip", async () => {
    // The MR branch sits at O. C is a DIVERGENT commit off main (init) — NOT a descendant of O
    // (a rewound/rebased history), so the non-ff is a genuine conflict, not a concurrent advance.
    gitOrigin(["checkout", "-b", "agent/issue-42"]);
    gitOrigin([...IDENT, "commit", "--allow-empty", "-m", "O: pre-existing MR branch tip"]);
    const shaO = gitOrigin(["rev-parse", "HEAD"]);
    gitOrigin(["checkout", "-b", "concurrent-diverged", "main"]);
    gitOrigin([...IDENT, "commit", "--allow-empty", "-m", "C: divergent history, not a descendant of O"]);
    const shaC = gitOrigin(["rev-parse", "HEAD"]);
    gitOrigin(["checkout", "main"]);
    assert.equal(gitOrigin(["rev-parse", "refs/heads/agent/issue-42"]), shaO, "precondition: the MR branch is at O at clone time");

    const originalPush = git.pushBranch.bind(git);
    let advanced = false;
    git.pushBranch = (async (bare: string, branch: string, pat: string, url: string, user?: string) => {
      if (!advanced && branch === "agent/issue-42") {
        advanced = true;
        // Rewind/diverge the MR branch to C — the worker's tip (a child of O) is not a
        // descendant of C, so the non-forced push rejects non-ff, but O is NOT an ancestor of C.
        gitOrigin(["update-ref", "refs/heads/agent/issue-42", shaC]);
      }
      return originalPush(bare, branch, pat, url, user);
    }) as typeof git.pushBranch;

    const { gitlab } = fakeGitlab();
    const claim = mrReworkClaim();
    try {
      await runner(new StubExecutor(nullLogger()), gitlab).execute(claim);
    } finally {
      git.pushBranch = originalPush;
    }

    // A real conflict must NOT be mislabeled benign: the run fails via today's generic path,
    // carrying NO branch_moved signal.
    const terminal = api.states.find(
      (s) => s.runId === claim.run_id && s.body.status === "failed",
    );
    assert.ok(terminal, "the divergent non-ff still reported a terminal failed state");
    assert.equal(
      terminal!.body.branch_moved,
      undefined,
      "a divergent/rewound non-ff is NOT dispositioned as branch_moved",
    );
    assert.match(
      terminal!.body.failure_reason ?? "",
      /scratch_publication_refused: cannot verify fresh remote floor/,
      "the generic failure names the fresh remote floor refusal",
    );
  });
});
