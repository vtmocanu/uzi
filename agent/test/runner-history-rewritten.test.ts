import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { randomUUID } from "node:crypto";
import { makeClaim, nullLogger } from "./helpers.js";
import { type Executor, type ExecutorResult, type RunContext } from "../src/executor.js";
import type { ClaimResponse } from "../src/protocol.js";
import { GitHubClient } from "../src/forge.js";
import {
  api,
  client,
  fakeGitHub,
  fakeGitlab,
  fx,
  git,
  installHarness,
  runner,
} from "./runner-harness.js";
import { RunRunner, composeHistoryRewrittenReason } from "../src/runner.js";

installHarness();

// PRD #1416 M4 — `history_rewritten`, typed, when the bridge cannot publish. When
// bridgeBareTrackingRefIfDivergent returns {kind:"failed"} (a divergent tip below the published
// floor P whose bridge B could not be built or validated), the finalize sinks route to
// failHistoryRewritten: `failed` / fail_origin=history_rewritten, NEVER the generic catch
// (agent_failure) and NEVER finalize_base_align_conflict. preserved_patch is attached ONLY from a
// scan-trusted range (fact 15). A valid B rejected non-fast-forward is concurrent branch movement,
// not a rewrite, and keeps its own classification. push_secret_blocked keeps precedence.
//
// Same REAL bare+clone harness as runner-bridge.test.ts (M3): a rewrite below P is modelled by
// amending the published tip in the runner clone so the fetched tip H genuinely diverges from P.

const ENV = {
  ...process.env,
  GIT_CONFIG_GLOBAL: "/dev/null",
  GIT_CONFIG_SYSTEM: "/dev/null",
  GIT_TERMINAL_PROMPT: "0",
};
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];
const CI_V1 = "name: ci\non: [push]\njobs: {}\n";
const CI_V2 = "name: ci\non: [pull_request]\njobs: {}\n";

function gitIn(dir: string, args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { encoding: "utf8", env: ENV }).trim();
}

/** Commit `files` onto the fixture origin's checked-out main. */
function commitToOriginMain(files: Record<string, string>, msg: string): void {
  for (const [rel, content] of Object.entries(files)) {
    const target = path.join(fx.originPath, rel);
    fs.mkdirSync(path.dirname(target), { recursive: true });
    fs.writeFileSync(target, content);
  }
  gitIn(fx.originPath, ["add", "."]);
  gitIn(fx.originPath, [...IDENT, "commit", "-m", msg]);
}

/** Publish `name` on the fixture origin with a commit of its own (from the current main), so its
 *  tip P is a real forge tip. Restores origin's HEAD to main. Returns P. */
function publishBranch(name: string): string {
  gitIn(fx.originPath, ["checkout", "-b", name]);
  fs.writeFileSync(path.join(fx.originPath, "PUBLISHED.md"), `# published work on ${name}\n`);
  gitIn(fx.originPath, ["add", "PUBLISHED.md"]);
  gitIn(fx.originPath, [...IDENT, "commit", "-m", `published commit on ${name}`]);
  const sha = gitIn(fx.originPath, ["rev-parse", "HEAD"]);
  gitIn(fx.originPath, ["checkout", "main"]);
  return sha;
}

/** An executor that amends the published tip in the runner clone (rewriting history BELOW P), then
 *  optionally advances the fixture origin's main. Returns the rewritten tip H via `obs`. */
function rewritingExecutor(obs: { H?: string }, mainFiles?: Record<string, string>): Executor {
  return {
    run: async (ctx: RunContext): Promise<ExecutorResult> => {
      fs.writeFileSync(path.join(ctx.worktreePath, "REWRITE.md"), `rewrite ${randomUUID()}\n`);
      gitIn(ctx.worktreePath, ["add", "."]);
      gitIn(ctx.worktreePath, [...IDENT, "commit", "--amend", "--no-edit"]);
      obs.H = gitIn(ctx.worktreePath, ["rev-parse", "HEAD"]);
      if (mainFiles) commitToOriginMain(mainFiles, "main advances");
      return { branch: ctx.branch };
    },
  };
}

function githubRunner(github: GitHubClient, executor: Executor): RunRunner {
  return new RunRunner(client, git, () => ({ executor }), nullLogger(), 20, undefined, {
    pollMs: 5,
    planApprovalTimeoutMs: 0,
    github,
  });
}

function taskClaim(branch: string, overrides: Partial<ClaimResponse> = {}): ClaimResponse {
  return makeClaim({
    run_id: (overrides.run_id as string | undefined) ?? randomUUID(),
    kind: "task",
    issue_iid: null,
    issue_title: "Handoff: published-branch work",
    issue_description: "Continue the work on the already-published branch.",
    branch,
    open_mr: false,
    repo: { id: "r1", url: "https://gitlab.example.test/org/repo", clone_url: fx.originPath },
    last_seq: 0,
    secrets: {
      forge_pat: "fixture-forge-pat-000000",
      anthropic_oauth_token: "dummy-oauth-do-not-scan",
    },
    ...overrides,
  });
}

function githubTaskClaim(branch: string, overrides: Partial<ClaimResponse> = {}): ClaimResponse {
  return taskClaim(branch, {
    repo: {
      id: "r1",
      url: "https://github.com/org/repo",
      clone_url: fx.originPath,
      forge_type: "github",
    },
    ...overrides,
  });
}

const statusesFor = (runId: string): string[] =>
  api.states.filter((s) => s.runId === runId).map((s) => s.body.status);

const failedBody = (runId: string) =>
  api.states.find((s) => s.runId === runId && s.body.status === "failed")!.body;

/** Force the ancestry bridge to be UNBUILDABLE: a divergent tip is detected, but bridgeToFloors
 *  returns null, so bridgeBareTrackingRefIfDivergent returns {kind:"failed"} — the M4 trigger. */
function stubBridgeUnbuildable(): void {
  (git as unknown as { bridgeToFloors: unknown }).bridgeToFloors = async () => null;
}

describe("RunRunner — history_rewritten typed terminal (PRD #1416 M4)", () => {
  it("(plain path) a bridge that cannot be built fails history_rewritten, names P + the doc, never the generic catch", async () => {
    const { gitlab } = fakeGitlab();
    const branch = "feature/hr-plain";
    const P = publishBranch(branch);
    stubBridgeUnbuildable();
    const obs: { H?: string } = {};
    const claim = taskClaim(branch, { open_mr: false });
    await runner(rewritingExecutor(obs), gitlab).execute(claim);

    const failed = failedBody(claim.run_id);
    // Typed history_rewritten — NOT the generic catch (which omits fail_origin → agent_failure),
    // NOT finalize_base_align_conflict.
    assert.strictEqual(failed.fail_origin, "history_rewritten");
    assert.notStrictEqual(failed.fail_origin, "finalize_base_align_conflict");
    assert.ok(!statusesFor(claim.run_id).includes("completed"), "the run did not complete");
    // The reason NAMES the published tip P and points at the doc.
    const reason = failed.failure_reason ?? "";
    assert.ok(reason.includes(P), "the reason names the published tip P");
    assert.match(reason, /docs\/github-bot-setup\.md/, "the reason points at the setup doc");
    assert.match(reason, /never force-pushes|NEVER force-pushes/i, "the reason states uzi never force-pushes");
    // No push happened: origin's branch tip is unchanged (still P).
    assert.strictEqual(gitIn(fx.originPath, ["rev-parse", branch]), P, "nothing was pushed");
  });

  it("(plain path) omits preserved_patch when the scan floor is untrusted (the divergent case)", async () => {
    // A gitlab claim never runs the github secret scan, so scanRangeTrusted stays false — the
    // conservative default. The divergent range was never scanned, so no diff is written.
    const { gitlab } = fakeGitlab();
    const branch = "feature/hr-no-patch";
    publishBranch(branch);
    stubBridgeUnbuildable();
    const claim = taskClaim(branch, { open_mr: false });
    await runner(rewritingExecutor({}), gitlab).execute(claim);

    const failed = failedBody(claim.run_id);
    assert.strictEqual(failed.fail_origin, "history_rewritten");
    assert.strictEqual(failed.preserved_patch, undefined, "an unscanned range is never preserved");
  });

  it("(github, untrusted scan) omits preserved_patch — the realistic divergent case", async () => {
    // A github claim whose secret scan fails OPEN (trusted:false) on the divergent tip: the
    // pushable range was NOT scanned, so failHistoryRewritten must OMIT the diff.
    const { github } = fakeGitHub();
    const branch = "feature/hr-gh-untrusted";
    publishBranch(branch);
    git.secretScanRange = (async () => ({ trusted: false, findings: [] })) as typeof git.secretScanRange;
    stubBridgeUnbuildable();
    const claim = githubTaskClaim(branch, { open_mr: false });
    await githubRunner(github, rewritingExecutor({})).execute(claim);

    const failed = failedBody(claim.run_id);
    assert.strictEqual(failed.fail_origin, "history_rewritten");
    assert.strictEqual(failed.preserved_patch, undefined, "the untrusted-floor divergent case omits the patch");
  });

  it("(github, trusted scan) attaches the redacted preserved_patch when the scanned range is trustworthy", async () => {
    // With a trusted scanned range (no workflow drift → the plain push path), the redacted agent
    // diff IS preserved even though the bridge failed.
    const { github } = fakeGitHub();
    const branch = "feature/hr-gh-trusted";
    publishBranch(branch);
    git.secretScanRange = (async () => ({ trusted: true, findings: [] })) as typeof git.secretScanRange;
    stubBridgeUnbuildable();
    const claim = githubTaskClaim(branch, { open_mr: false });
    await githubRunner(github, rewritingExecutor({})).execute(claim);

    const failed = failedBody(claim.run_id);
    assert.strictEqual(failed.fail_origin, "history_rewritten");
    assert.ok(typeof failed.preserved_patch === "string" && failed.preserved_patch.length > 0, "a trusted range preserves the diff");
    assert.match(failed.preserved_patch!, /REWRITE\.md/, "the preserved patch carries the agent's rewritten work");
  });

  it("(align path) a bridge failure under workflow drift is history_rewritten, NOT finalize_base_align_conflict", async () => {
    // main advances a workflow file → the branch is behind-on-workflows → the align chain engages;
    // inside it fetchAndPush bridges and, on {kind:"failed"}, throws HistoryRewrittenError, which
    // the M4 wrap types history_rewritten rather than letting the align arms mislabel it.
    commitToOriginMain({ ".github/workflows/ci.yml": CI_V1 }, "seed workflows");
    const branch = "feature/hr-align";
    const P = publishBranch(branch);
    const { github } = fakeGitHub();
    git.secretScanRange = (async () => ({ trusted: false, findings: [] })) as typeof git.secretScanRange;
    stubBridgeUnbuildable();
    const claim = githubTaskClaim(branch, { open_mr: false });
    await githubRunner(github, rewritingExecutor({}, { ".github/workflows/ci.yml": CI_V2 })).execute(claim);

    const failed = failedBody(claim.run_id);
    assert.strictEqual(failed.fail_origin, "history_rewritten", "typed history_rewritten on the align path");
    assert.notStrictEqual(failed.fail_origin, "finalize_base_align_conflict", "never mislabelled as a base-align conflict");
    assert.ok((failed.failure_reason ?? "").includes(P), "the reason names P");
    // The align conflict path preserves the diff; this one must NOT (untrusted scan floor).
    assert.strictEqual(failed.preserved_patch, undefined, "the divergent align case omits the patch");
  });

  it("(precedence) GH013 on a successfully-bridged B is push_secret_blocked, NOT history_rewritten", async () => {
    // Do NOT stub bridgeToFloors: a valid B is built and the tracking ref advances to it. The push
    // of B is then rejected by GitHub Push Protection (GH013), which must keep precedence — a
    // bridge FAILURE throws before any push, so the two never collide.
    const { github } = fakeGitHub();
    const branch = "feature/hr-gh013";
    publishBranch(branch);
    git.secretScanRange = (async () => ({ trusted: false, findings: [] })) as typeof git.secretScanRange;
    git.pushBranch = (async () => {
      throw new Error(
        "remote: error: GH013: Repository rule violations found — push cannot contain secrets (push protection)",
      );
    }) as typeof git.pushBranch;
    const claim = githubTaskClaim(branch, { open_mr: false });
    await githubRunner(github, rewritingExecutor({})).execute(claim);

    const failed = failedBody(claim.run_id);
    assert.strictEqual(failed.fail_origin, "push_secret_blocked", "GH013 keeps precedence over history_rewritten");
    assert.notStrictEqual(failed.fail_origin, "history_rewritten");
    assert.strictEqual(failed.preserved_patch, undefined, "no diff on the secret path");
  });

  it("(not a rewrite) a valid B rejected non-fast-forward keeps branch_moved, never history_rewritten", async () => {
    // The agent adds work ON TOP of P (a fast-forward — NOT a rewrite), so P stays an ancestor and
    // no bridge is built. A concurrent writer advances the remote branch → the push is rejected
    // non-fast-forward. For mr_rework that is branch_moved, and it must NEVER read as history_rewritten.
    const { gitlab } = fakeGitlab();
    const branch = "agent/issue-1416-concurrent";
    publishBranch(branch);
    const concurrentAdvance: Executor = {
      run: async (ctx) => {
        fs.writeFileSync(path.join(ctx.worktreePath, "rework.ts"), "1\n");
        gitIn(ctx.worktreePath, ["add", "."]);
        gitIn(ctx.worktreePath, [...IDENT, "commit", "-m", "rework on top of P"]);
        gitIn(fx.originPath, ["checkout", branch]);
        fs.writeFileSync(path.join(fx.originPath, "concurrent.md"), "someone else\n");
        gitIn(fx.originPath, ["add", "."]);
        gitIn(fx.originPath, [...IDENT, "commit", "-m", "concurrent writer"]);
        gitIn(fx.originPath, ["checkout", "main"]);
        return { branch: ctx.branch };
      },
    };
    const claim = taskClaim(branch, { kind: "mr_rework", open_mr: true });
    await runner(concurrentAdvance, gitlab).execute(claim).catch(() => undefined);

    assert.ok(
      api.states.some((s) => s.runId === claim.run_id && s.body.branch_moved === true),
      "a concurrent advance of a valid (non-rewritten) branch is branch_moved",
    );
    assert.ok(
      !api.states.some((s) => s.runId === claim.run_id && s.body.fail_origin === "history_rewritten"),
      "a concurrent advance is NEVER typed history_rewritten",
    );
  });
});

describe("composeHistoryRewrittenReason (PRD #1416 M4)", () => {
  it("names the published tip, points at the doc, and fits MAX_FAILURE_REASON_LEN", () => {
    const P = "0123456789abcdef0123456789abcdef01234567";
    const reason = composeHistoryRewrittenReason(P);
    assert.ok(reason.includes(P), "names the published tip");
    assert.match(reason, /docs\/github-bot-setup\.md/, "points at the setup doc");
    assert.match(reason, /never force-pushes|NEVER force-pushes/i, "states uzi never force-pushes");
    // Accurate whether or not a patch is attached: it must NOT hard-promise the diff below.
    assert.doesNotMatch(reason, /preserved below/i, "makes no unconditional 'preserved below' claim");
    assert.ok(reason.length <= 512, `reason is within MAX_FAILURE_REASON_LEN (was ${reason.length})`);
  });
});
