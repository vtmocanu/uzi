import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { nullLogger } from "./helpers.js";
import { StubExecutor, type Executor } from "../src/executor.js";
import { RunRunner, composeWorkflowScopeReason } from "../src/runner.js";
import { GitHubClient } from "../src/forge.js";
import { skillsPluginDir } from "../src/skills-plugin.js";
import {
  api,
  client,
  fakeForgejo,
  fakeGitHub,
  fakeGitlab,
  fx,
  git,
  gitlabClaim,
  installHarness,
  runner,
  simulateCommittedWork,
  worktreeDirFor,
} from "./runner-harness.js";

installHarness();

describe("RunRunner — worker-performed push + MR", () => {
  it("pushes the branch and opens an MR after the executor returns done", async () => {
    const { gitlab, calls } = fakeGitlab();
    const claim = gitlabClaim(7);
    await runner(new StubExecutor(nullLogger()), gitlab).execute(claim);

    const statuses = api.states
      .filter((s) => s.runId === claim.run_id)
      .map((s) => s.body.status);
    // Two `running` reports: the claim→running transition, then the post-checkout
    // one carrying the repo's detected agent roster (PRD #37).
    assert.deepStrictEqual(statuses, ["running", "running", "completed"]);
    const completed = api.states.find(
      (s) => s.body.status === "completed",
    )!.body;
    assert.strictEqual(completed.branch, "agent/issue-7");
    assert.strictEqual(completed.mr_iid, 42);
    // The forge-reported MR web URL is persisted on completion (PRD #65 D8), so the
    // web links it directly instead of reconstructing it.
    assert.strictEqual(
      completed.mr_web_url,
      "https://gitlab.example.test/org/repo/-/merge_requests/42",
    );

    // The MR was opened with the PAT in the PRIVATE-TOKEN header — never the URL
    // or the body (primary directive: the credential stays off argv/URL/logs).
    assert.strictEqual(calls.length, 1);
    const call = calls[0]!;
    assert.strictEqual(call.method, "POST");
    assert.match(call.url, /\/api\/v4\/projects\/org%2Frepo\/merge_requests$/);
    assert.strictEqual(
      call.headers["PRIVATE-TOKEN"],
      "fixture-forge-pat-000000",
    );
    assert.ok(
      !call.url.includes("fixture-forge-pat"),
      "PAT must not be in the URL",
    );
    assert.ok(
      !(call.body ?? "").includes("fixture-forge-pat"),
      "PAT must not be in the body",
    );
    const body = JSON.parse(call.body ?? "{}");
    assert.strictEqual(body.source_branch, "agent/issue-7");
    assert.strictEqual(body.target_branch, "main");
    assert.match(body.description, /Closes #7/);
    assert.strictEqual(body.remove_source_branch, false);

    // The branch really landed on origin, and the worktree was torn down.
    const log = execFileSync(
      "git",
      ["-C", fx.originPath, "log", "--oneline", "agent/issue-7"],
      { encoding: "utf8" },
    );
    assert.ok(log.includes("uzi stub: work on issue #7"));
    assert.strictEqual(fs.existsSync(worktreeDirFor(7)), false);
  });

  it("annotates the MR body with the unverified gates when a component's deps did not install (issue #293 M2)", async () => {
    const { gitlab, calls } = fakeGitlab();
    const claim = gitlabClaim(7);
    simulateCommittedWork(); // a real diff, so #279's empty-diff guard does not suppress the MR
    // An executor that delivers work but reports web's JS deps did not install, so
    // web's gates (vitest/knip) could not have run — the honest annotation posture.
    const exec: Executor = {
      run: async (ctx) => ({
        branch: ctx.branch,
        agentSelection: { source: "own", agents: ["coder", "reviewer"] },
        gatesUnverified: ["web"],
      }),
    };
    await runner(exec, gitlab).execute(claim);

    assert.strictEqual(calls.length, 1);
    const body = JSON.parse(calls[0]!.body ?? "{}");
    assert.match(body.description, /Closes #7/);
    assert.match(body.description, /Quality gates unverified/);
    assert.match(body.description, /`web`/, "the unverified component dir must be named in the MR body");
  });

  it("annotates the MR body with a discovery-truncation caveat even when no named dir failed (issue #293 M2, review F1)", async () => {
    const { gitlab, calls } = fakeGitlab();
    const claim = gitlabClaim(7);
    simulateCommittedWork(); // a real diff, so #279's empty-diff guard does not suppress the MR
    // Every reached dir installed, but discovery hit its scan cap: components past the cap
    // were never examined, so gatesUnverified names none of them. The caveat must still fire
    // so the capped coverage does not read as full coverage.
    const exec: Executor = {
      run: async (ctx) => ({
        branch: ctx.branch,
        agentSelection: { source: "own", agents: ["coder", "reviewer"] },
        gatesDiscoveryTruncated: true,
      }),
    };
    await runner(exec, gitlab).execute(claim);

    assert.strictEqual(calls.length, 1);
    const body = JSON.parse(calls[0]!.body ?? "{}");
    assert.match(body.description, /Quality gates unverified/);
    assert.match(body.description, /scan cap/, "the truncation caveat must be present");
  });

  it("a normal run's MR body carries NO unverified-gates annotation (issue #293 M2)", async () => {
    const { gitlab, calls } = fakeGitlab();
    const claim = gitlabClaim(7);
    simulateCommittedWork(); // a real diff, so #279's empty-diff guard does not suppress the MR
    // A done result with gatesUnverified absent — every component installed, the common case.
    const exec: Executor = {
      run: async (ctx) => ({
        branch: ctx.branch,
        agentSelection: { source: "own", agents: ["coder", "reviewer"] },
      }),
    };
    await runner(exec, gitlab).execute(claim);

    assert.strictEqual(calls.length, 1);
    const body = JSON.parse(calls[0]!.body ?? "{}");
    assert.match(body.description, /Closes #7/);
    assert.ok(
      !body.description.includes("Quality gates unverified"),
      "a fully-installed run must not carry the unverified-gates annotation",
    );
  });

  it("routes a forgejo claim to the Forgejo client and persists the PR web url (PRD #65 D9/D8)", async () => {
    const { forgejo, calls } = fakeForgejo();
    // A Forgejo run: same local clone_url (git is forge-agnostic), a Forgejo web url,
    // and forge_type=forgejo on the wire so the worker picks the Forgejo client.
    const claim = gitlabClaim(15, {
      repo: {
        id: "r1",
        url: "https://forgejo.example.test/org/repo",
        clone_url: fx.originPath,
        forge_type: "forgejo",
      },
    });
    const r = new RunRunner(
      client,
      git,
      () => ({ executor: new StubExecutor(nullLogger()) }),
      nullLogger(),
      20,
      undefined,
      {
        pollMs: 5,
        planApprovalTimeoutMs: 0,
        forgejo,
      },
    );
    await r.execute(claim);

    const completed = api.states.find(
      (s) => s.runId === claim.run_id && s.body.status === "completed",
    )!.body;
    assert.strictEqual(completed.branch, "agent/issue-15");
    assert.strictEqual(completed.mr_iid, 42);
    assert.strictEqual(
      completed.mr_web_url,
      "https://forgejo.example.test/org/repo/pulls/42",
    );

    // The PR was opened against Forgejo's /api/v1 pulls endpoint with the token header
    // — never the URL/body (primary directive).
    assert.strictEqual(calls.length, 1);
    const call = calls[0]!;
    assert.strictEqual(call.method, "POST");
    assert.match(call.url, /\/api\/v1\/repos\/org\/repo\/pulls$/);
    assert.strictEqual(
      call.headers["Authorization"],
      "token fixture-forge-pat-000000",
    );
    assert.ok(
      !call.url.includes("fixture-forge-pat"),
      "PAT must not be in the URL",
    );
    assert.ok(
      !(call.body ?? "").includes("fixture-forge-pat"),
      "PAT must not be in the body",
    );
    const body = JSON.parse(call.body ?? "{}");
    assert.strictEqual(body.head, "agent/issue-15");
    assert.strictEqual(body.base, "main");
  });

  it("routes a github claim to the GitHub client and persists the PR web url (PRD #238 D9/D8)", async () => {
    const { github, calls } = fakeGitHub();
    // A GitHub run: same local clone_url (git is forge-agnostic), a github.com web
    // url, and forge_type=github on the wire so the worker picks the GitHub client.
    const claim = gitlabClaim(15, {
      repo: {
        id: "r1",
        url: "https://github.com/org/repo",
        clone_url: fx.originPath,
        forge_type: "github",
      },
    });
    const r = new RunRunner(
      client,
      git,
      () => ({ executor: new StubExecutor(nullLogger()) }),
      nullLogger(),
      20,
      undefined,
      {
        pollMs: 5,
        planApprovalTimeoutMs: 0,
        github,
      },
    );
    await r.execute(claim);

    const completed = api.states.find(
      (s) => s.runId === claim.run_id && s.body.status === "completed",
    )!.body;
    assert.strictEqual(completed.branch, "agent/issue-15");
    assert.strictEqual(completed.mr_iid, 42);
    assert.strictEqual(completed.mr_web_url, "https://github.com/org/repo/pull/42");

    // The PR was opened against GitHub's api.github.com pulls endpoint with the
    // Bearer token header — never the URL/body (primary directive).
    assert.strictEqual(calls.length, 1);
    const call = calls[0]!;
    assert.strictEqual(call.method, "POST");
    assert.match(call.url, /^https:\/\/api\.github\.com\/repos\/org\/repo\/pulls$/);
    assert.strictEqual(
      call.headers["Authorization"],
      "Bearer fixture-forge-pat-000000",
    );
    assert.ok(
      !call.url.includes("fixture-forge-pat"),
      "PAT must not be in the URL",
    );
    assert.ok(
      !(call.body ?? "").includes("fixture-forge-pat"),
      "PAT must not be in the body",
    );
    const body = JSON.parse(call.body ?? "{}");
    assert.strictEqual(body.head, "agent/issue-15");
    assert.strictEqual(body.base, "main");
  });

  it("tears down the sibling skills plugin dir with the worktree (PRD #16 M6 follow-up)", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(11);
    const pluginDir = skillsPluginDir(worktreeDirFor(11));
    // An executor that materializes a plugin dir the way SdkExecutor does — the
    // runner's finally must remove it (it is OUTSIDE the worktree, so the worktree
    // teardown does not reach it).
    const exec: Executor = {
      run: async (ctx) => {
        fs.mkdirSync(pluginDir, { recursive: true });
        fs.writeFileSync(path.join(pluginDir, "marker"), "x");
        return { branch: ctx.branch };
      },
    };
    await runner(exec, gitlab).execute(claim);
    assert.strictEqual(
      fs.existsSync(pluginDir),
      false,
      "skills plugin dir must be cleaned up",
    );
    assert.strictEqual(
      fs.existsSync(worktreeDirFor(11)),
      false,
      "worktree still cleaned up",
    );
  });

  it("reports failed and opens NO merge request when the executor throws", async () => {
    const { gitlab, calls } = fakeGitlab();
    const boom: Executor = {
      run: async () => {
        throw new Error("kaboom");
      },
    };
    const claim = gitlabClaim(9);
    await runner(boom, gitlab).execute(claim);

    const statuses = api.states
      .filter((s) => s.runId === claim.run_id)
      .map((s) => s.body.status);
    // The second `running` is the post-checkout roster report; the executor throws
    // after it.
    assert.deepStrictEqual(statuses, ["running", "running", "failed"]);
    assert.ok(
      api.states
        .find((s) => s.body.status === "failed")
        ?.body.failure_reason?.includes("kaboom"),
    );
    assert.strictEqual(calls.length, 0, "no MR on failure");
    assert.strictEqual(fs.existsSync(worktreeDirFor(9)), false);
  });

  it("redacts and caps the failure_reason before it reaches reportState", async () => {
    // failure_reason bypasses the batcher's payload redactor (it goes straight to
    // reportState), so the runner scrubs it directly. A raw SDK error can carry a
    // live secret and run long — this is the defense-in-depth gap PRD #12 widened.
    const OAUTH = "dummy-oauth-secret-fail-000000";
    const PAT = "fixture-forge-pat-000000";
    const { gitlab, calls } = fakeGitlab();
    const boom: Executor = {
      run: async () => {
        throw new Error(
          `clone failed: oauth=${OAUTH} pat=${PAT} ${"x".repeat(1000)}`,
        );
      },
    };
    const claim = gitlabClaim(41, {
      secrets: { forge_pat: PAT, anthropic_oauth_token: OAUTH },
    });
    await runner(boom, gitlab).execute(claim);

    const reason =
      api.states.find(
        (s) => s.runId === claim.run_id && s.body.status === "failed",
      )!.body.failure_reason ?? "";
    assert.ok(
      !reason.includes(OAUTH),
      "the OAuth token must not reach reportState",
    );
    assert.ok(
      !reason.includes(PAT),
      "the forge PAT must not reach reportState",
    );
    assert.ok(
      reason.includes("***REDACTED***"),
      "the secret should be redacted in place",
    );
    assert.ok(
      reason.length <= 512,
      `failure_reason must be capped at 512 (got ${reason.length})`,
    );
    assert.strictEqual(calls.length, 0, "no MR on failure");
  });

  it("redacts the OAuth token AND the worker join token from emitted messages", async () => {
    const OAUTH = "dummy-oauth-token-runner-000000";
    const JOIN_TOKEN = "dummy-join-token-runner-111111";
    const { gitlab } = fakeGitlab();
    const leaky: Executor = {
      run: async (ctx) => {
        ctx.emit({
          kind: "tool_result",
          agent: "coder",
          payload: { content: `oauth=${OAUTH} join=${JOIN_TOKEN}` },
        });
        return { branch: ctx.branch };
      },
    };
    const claim = gitlabClaim(11, {
      secrets: {
        forge_pat: "fixture-forge-pat-000000",
        anthropic_oauth_token: OAUTH,
      },
    });
    await runner(leaky, gitlab, JOIN_TOKEN).execute(claim);

    const tr = api
      .messages(claim.run_id)
      .find((m) => m.kind === "tool_result")!;
    const serialized = JSON.stringify(tr.payload);
    assert.ok(
      !serialized.includes(OAUTH),
      "the OAuth token must not reach the API",
    );
    assert.ok(
      !serialized.includes(JOIN_TOKEN),
      "the join token must not reach the API",
    );
    assert.ok(
      serialized.includes("***REDACTED***"),
      "secrets should be redacted in place",
    );
  });

  // issue #279: a DECLARED report-only run completes with its findings and opens NO
  // merge request — the ci_fix not_code precedent, for an issue run's evidence deliverable.
  it("completes report-only with report_md and opens NEITHER a push NOR an MR (issue #279)", async () => {
    const { gitlab, calls } = fakeGitlab();
    let pushed = false;
    git.pushBranch = (async () => {
      pushed = true;
    }) as typeof git.pushBranch;
    const summary = "verified: config already correct; no code change needed";
    const exec: Executor = {
      run: async (ctx) => ({ branch: ctx.branch, reportOnly: true, summary }),
    };
    const claim = gitlabClaim(21);
    await runner(exec, gitlab).execute(claim);

    const statuses = api.states
      .filter((s) => s.runId === claim.run_id)
      .map((s) => s.body.status);
    assert.deepStrictEqual(statuses, ["running", "running", "completed"]);
    const completed = api.states.find(
      (s) => s.runId === claim.run_id && s.body.status === "completed",
    )!.body;
    assert.strictEqual(completed.report_only, true);
    assert.strictEqual(completed.report_md, summary);
    assert.ok(
      !("mr_iid" in completed) || completed.mr_iid === undefined,
      "no MR iid on a report-only completion",
    );
    assert.strictEqual(calls.length, 0, "no MR opened on a report-only run");
    assert.strictEqual(pushed, false, "no branch pushed on a report-only run");
    assert.strictEqual(fs.existsSync(worktreeDirFor(21)), false);
  });

  // issue #279: an issue run that signalled done but committed NOTHING and did NOT set
  // report_only is the ambiguous "forgot to commit / should have set report_only" case —
  // it must fail with an actionable reason rather than open an empty MR.
  it("fails an issue run with an empty diff and no report_only, opening NO MR (issue #279)", async () => {
    const { gitlab, calls } = fakeGitlab();
    // A confirmed-empty diff (changedFiles returns [], not null). The StubExecutor still
    // creates a real branch so fetchAgentBranch succeeds; the guard keys on the diff.
    git.changedFiles = (async () => []) as typeof git.changedFiles;
    const claim = gitlabClaim(22);
    await runner(new StubExecutor(nullLogger()), gitlab).execute(claim);

    const statuses = api.states
      .filter((s) => s.runId === claim.run_id)
      .map((s) => s.body.status);
    assert.deepStrictEqual(statuses, ["running", "running", "failed"]);
    const failed = api.states.find(
      (s) => s.runId === claim.run_id && s.body.status === "failed",
    )!.body;
    assert.match(failed.failure_reason ?? "", /report_only was not set/);
    assert.strictEqual(calls.length, 0, "no MR on an empty-diff issue run");
    assert.strictEqual(fs.existsSync(worktreeDirFor(22)), false);
  });

  // issue #299: a report-only completion opens NO branch/MR, so if this run already
  // published committed work to a checkpoint ref on origin, completing report-only would
  // orphan it. This pins the cross-worker leg of the union guard: hasCheckpointRef true
  // (origin carries a checkpoint a prior attempt landed, mirrored into the bare) while
  // this worker published nothing itself. It must FAIL with an actionable reason, opening
  // NEITHER a push NOR an MR — mirroring the undeclared-empty-diff FAIL path.
  it("fails a report-only run that published a checkpoint (orphan guard), opening NO MR (issue #299)", async () => {
    const { gitlab, calls } = fakeGitlab();
    let pushed = false;
    git.pushBranch = (async () => {
      pushed = true;
    }) as typeof git.pushBranch;
    // A checkpoint ref for this run's branch already exists on origin (mirrored into the
    // bare) — this worker did not publish it, so lastPublishedTip stays undefined and the
    // guard must key on hasCheckpointRef.
    git.hasCheckpointRef = (async () => true) as typeof git.hasCheckpointRef;
    const exec: Executor = {
      run: async (ctx) => ({
        branch: ctx.branch,
        reportOnly: true,
        summary: "verified: no code change needed",
      }),
    };
    const claim = gitlabClaim(23);
    await runner(exec, gitlab).execute(claim);

    const statuses = api.states
      .filter((s) => s.runId === claim.run_id)
      .map((s) => s.body.status);
    assert.deepStrictEqual(statuses, ["running", "running", "failed"]);
    const failed = api.states.find(
      (s) => s.runId === claim.run_id && s.body.status === "failed",
    )!.body;
    assert.match(failed.failure_reason ?? "", /report_only/);
    assert.match(failed.failure_reason ?? "", /checkpoint/);
    assert.ok(
      !("report_only" in failed) || failed.report_only !== true,
      "the run FAILED — it is not a report-only completion",
    );
    assert.strictEqual(calls.length, 0, "no MR opened on the guarded report-only run");
    assert.strictEqual(pushed, false, "no branch pushed on the guarded report-only run");
    assert.strictEqual(fs.existsSync(worktreeDirFor(23)), false);
  });

  // issue #279 GAP1: the undeclared-empty-diff guard is ISSUE-ONLY. A NON-issue kind
  // (here a scheduled `prompt` run) whose diff is a CONFIRMED-empty [] — the exact input
  // that fails an issue run above — must NOT trip the guard: it still pushes and opens
  // its MR. This pins the `(claim.kind ?? "issue") === "issue"` gate, not merely that the
  // empty-[] branch fires; folding that gate to `true` reddens this test.
  it("does NOT fire the empty-diff guard on a NON-issue (prompt) run — still pushes + opens an MR (issue #279)", async () => {
    const { gitlab, calls } = fakeGitlab();
    let pushed = false;
    git.pushBranch = (async () => {
      pushed = true;
    }) as typeof git.pushBranch;
    // A CONFIRMED-empty diff ([], not null) — the guard input that FAILS an issue run.
    // The StubExecutor still commits a real branch so fetchAgentBranch succeeds; the stub
    // drives the guard's view of the diff, not the real (non-empty) one.
    git.changedFiles = (async () => []) as typeof git.changedFiles;
    const claim = gitlabClaim(23, { kind: "prompt" });
    await runner(new StubExecutor(nullLogger()), gitlab).execute(claim);

    const statuses = api.states
      .filter((s) => s.runId === claim.run_id)
      .map((s) => s.body.status);
    assert.ok(
      !statuses.includes("failed"),
      `a non-issue empty-diff run must not fail (got ${statuses.join(",")})`,
    );
    const completed = api.states.find(
      (s) => s.runId === claim.run_id && s.body.status === "completed",
    )!.body;
    assert.strictEqual(completed.mr_iid, 42);
    assert.strictEqual(calls.length, 1, "the MR was opened on the non-issue run");
    assert.strictEqual(pushed, true, "the branch was pushed on the non-issue run");
  });

  // issue #279 GAP2: null is diff-FAILURE, which must FAIL OPEN. An ISSUE run whose
  // changedFiles returns null (not []) must NOT trip the guard — it pushes and opens its
  // MR exactly as a normal run would. This pins the `changedForGuard !== null` clause,
  // which a guard keyed only on `length === 0` (treating a null-ish empty as a fail)
  // would get wrong; removing that clause reddens this test.
  it("does NOT fire the empty-diff guard when changedFiles returns null (diff-failure — fail open) — still pushes + opens an MR (issue #279)", async () => {
    const { gitlab, calls } = fakeGitlab();
    let pushed = false;
    git.pushBranch = (async () => {
      pushed = true;
    }) as typeof git.pushBranch;
    // A diff FAILURE (null), distinct from a confirmed-empty []: the guard must fall
    // through to the normal push+MR path (fail open). StubExecutor commits a real branch.
    git.changedFiles = (async () => null) as typeof git.changedFiles;
    const claim = gitlabClaim(24);
    await runner(new StubExecutor(nullLogger()), gitlab).execute(claim);

    const statuses = api.states
      .filter((s) => s.runId === claim.run_id)
      .map((s) => s.body.status);
    assert.ok(
      !statuses.includes("failed"),
      `a null-diff issue run must fail open, not fail (got ${statuses.join(",")})`,
    );
    const completed = api.states.find(
      (s) => s.runId === claim.run_id && s.body.status === "completed",
    )!.body;
    assert.strictEqual(completed.branch, "agent/issue-24");
    assert.strictEqual(completed.mr_iid, 42);
    assert.strictEqual(calls.length, 1, "the MR was opened on the null-diff run");
    assert.strictEqual(pushed, true, "the branch was pushed on the null-diff run");
  });
});

// PRD #377 M1: a GitHub run whose branch touches .github/workflows/** cannot be pushed by
// the bot's repo-only PAT (privcheck forbids the workflow scope by design). The worker
// detects it at finalize, BEFORE the doomed push, and ends the run in a typed `failed`
// outcome that preserves the agent's diff. Every fixture below is a SYNTHETIC in-memory
// `changedFiles`/`workflowScopeDiff` stub — no real workflow file ever touches disk.
describe("RunRunner — workflow-scope early fail (PRD #377 M1)", () => {
  /** Build a runner wired to the fake GitHub client, mirroring the github routing test. */
  function githubRunner(github: GitHubClient): RunRunner {
    return new RunRunner(
      client,
      git,
      () => ({ executor: new StubExecutor(nullLogger()) }),
      nullLogger(),
      20,
      undefined,
      { pollMs: 5, planApprovalTimeoutMs: 0, github },
    );
  }

  const githubClaim = (iid: number, overrides = {}) =>
    gitlabClaim(iid, {
      repo: {
        id: "r1",
        url: "https://github.com/org/repo",
        clone_url: fx.originPath,
        forge_type: "github",
      },
      ...overrides,
    });

  it("github + a .github/workflows change fails early with workflow_scope_missing, skips the push, and preserves the redacted diff", async () => {
    const { github, calls } = fakeGitHub();
    let pushed = false;
    git.pushBranch = (async () => {
      pushed = true;
    }) as typeof git.pushBranch;
    // Synthetic fixtures only — a workflow path in the changed set, and a diff string that
    // carries the run's forge PAT so we can prove the CALLER's redactText was applied.
    const PAT = "fixture-forge-pat-000000";
    const SYNTH_DIFF =
      "diff --git a/.github/workflows/main-guard.yml b/.github/workflows/main-guard.yml\n" +
      `+ leaked_token=${PAT}\n+on: [push]\n`;
    git.changedFiles = (async () => [
      ".github/workflows/main-guard.yml",
    ]) as typeof git.changedFiles;
    git.workflowScopeDiff = (async () =>
      SYNTH_DIFF) as typeof git.workflowScopeDiff;

    const claim = githubClaim(31);
    await githubRunner(github).execute(claim);

    const statuses = api.states
      .filter((s) => s.runId === claim.run_id)
      .map((s) => s.body.status);
    assert.deepStrictEqual(statuses, ["running", "running", "failed"]);
    const failed = api.states.find(
      (s) => s.runId === claim.run_id && s.body.status === "failed",
    )!.body;
    assert.strictEqual(failed.fail_origin, "workflow_scope_missing");
    assert.match(
      failed.failure_reason ?? "",
      /docs\/github-bot-setup\.md/,
      "the reason must point at the setup doc",
    );
    assert.match(
      failed.failure_reason ?? "",
      /main-guard\.yml/,
      "the reason must name the offending path",
    );
    // The preserved patch is the REDACTED form: the PAT was scrubbed in place.
    assert.ok(
      failed.preserved_patch,
      "the agent's diff must be preserved on the failed report",
    );
    assert.ok(
      !failed.preserved_patch!.includes(PAT),
      "the PAT must be scrubbed from the preserved patch",
    );
    assert.match(
      failed.preserved_patch!,
      /REDACTED/,
      "the secret should be redacted in place",
    );
    assert.match(
      failed.preserved_patch!,
      /main-guard\.yml/,
      "the non-secret diff content is preserved",
    );

    assert.strictEqual(pushed, false, "the doomed push must be skipped");
    assert.strictEqual(calls.length, 0, "no PR opened on the early fail");
    assert.strictEqual(fs.existsSync(worktreeDirFor(31)), false);
  });

  it("github + a non-workflow change pushes and opens a PR normally (predicate is workflow-path-gated)", async () => {
    const { github, calls } = fakeGitHub();
    let pushed = false;
    git.pushBranch = (async () => {
      pushed = true;
    }) as typeof git.pushBranch;
    git.changedFiles = (async () => ["src/foo.ts"]) as typeof git.changedFiles;

    const claim = githubClaim(32);
    await githubRunner(github).execute(claim);

    const statuses = api.states
      .filter((s) => s.runId === claim.run_id)
      .map((s) => s.body.status);
    assert.ok(
      !statuses.includes("failed"),
      `a non-workflow github run must not fail (got ${statuses.join(",")})`,
    );
    const completed = api.states.find(
      (s) => s.runId === claim.run_id && s.body.status === "completed",
    )!.body;
    assert.strictEqual(completed.mr_iid, 42);
    assert.strictEqual(pushed, true, "the branch was pushed on the non-workflow run");
    assert.strictEqual(calls.length, 1, "the PR was opened on the non-workflow run");
  });

  it("github + changedFiles returns null (diff-failure) fails OPEN to the normal push (D6)", async () => {
    const { github, calls } = fakeGitHub();
    let pushed = false;
    git.pushBranch = (async () => {
      pushed = true;
    }) as typeof git.pushBranch;
    // null is diff-FAILURE, distinct from a confirmed-empty []; the guard must fall through
    // to the normal push+PR path (fail open), never blocking a possibly-legit run.
    git.changedFiles = (async () => null) as typeof git.changedFiles;

    const claim = githubClaim(33);
    await githubRunner(github).execute(claim);

    const statuses = api.states
      .filter((s) => s.runId === claim.run_id)
      .map((s) => s.body.status);
    assert.ok(
      !statuses.includes("failed"),
      `a null-diff github run must fail open, not fail (got ${statuses.join(",")})`,
    );
    const completed = api.states.find(
      (s) => s.runId === claim.run_id && s.body.status === "completed",
    )!.body;
    assert.strictEqual(completed.mr_iid, 42);
    assert.strictEqual(pushed, true, "the branch was pushed on the null-diff run");
    assert.strictEqual(calls.length, 1, "the PR was opened on the null-diff run");
  });

  it("a non-github (forgejo) claim with a workflow path is UNAFFECTED — normal push + PR", async () => {
    const { forgejo, calls } = fakeForgejo();
    let pushed = false;
    git.pushBranch = (async () => {
      pushed = true;
    }) as typeof git.pushBranch;
    // The exact input that fails a github run — but the predicate is github-only, so a
    // forgejo run must push and open its PR as normal.
    git.changedFiles = (async () => [
      ".github/workflows/main-guard.yml",
    ]) as typeof git.changedFiles;

    const claim = gitlabClaim(34, {
      repo: {
        id: "r1",
        url: "https://forgejo.example.test/org/repo",
        clone_url: fx.originPath,
        forge_type: "forgejo",
      },
    });
    const r = new RunRunner(
      client,
      git,
      () => ({ executor: new StubExecutor(nullLogger()) }),
      nullLogger(),
      20,
      undefined,
      { pollMs: 5, planApprovalTimeoutMs: 0, forgejo },
    );
    await r.execute(claim);

    const statuses = api.states
      .filter((s) => s.runId === claim.run_id)
      .map((s) => s.body.status);
    assert.ok(
      !statuses.includes("failed"),
      `a non-github run must be unaffected (got ${statuses.join(",")})`,
    );
    const completed = api.states.find(
      (s) => s.runId === claim.run_id && s.body.status === "completed",
    )!.body;
    assert.strictEqual(completed.mr_iid, 42);
    assert.strictEqual(pushed, true, "the branch was pushed on the forgejo run");
    assert.strictEqual(calls.length, 1, "the PR was opened on the forgejo run");
  });

  it("composeWorkflowScopeReason: a long path list stays within the cap and keeps the doc link intact", () => {
    const paths = Array.from(
      { length: 80 },
      (_, i) => `.github/workflows/generated-guard-${i}-with-a-long-name.yml`,
    );
    const reason = composeWorkflowScopeReason(paths);
    assert.ok(
      reason.length <= 512,
      `failure_reason must be capped at 512 (got ${reason.length})`,
    );
    assert.match(
      reason,
      /docs\/github-bot-setup\.md/,
      "the doc link must survive path-list truncation",
    );
    assert.match(
      reason,
      /and \d+ more/,
      "a truncated list must show the omitted count",
    );
    // The first path is always shown; the doc link never gets cut off the end.
    assert.match(reason, /generated-guard-0-/);
  });
});
