import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { nullLogger } from "./helpers.js";
import { StubExecutor, type Executor } from "../src/executor.js";
import { RunRunner } from "../src/runner.js";
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
});
