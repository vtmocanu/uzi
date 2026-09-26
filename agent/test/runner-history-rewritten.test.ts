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
 *  returns {kind:"failed"}, so bridgeBareTrackingRefIfDivergent returns {kind:"failed"} — the M4
 *  trigger. */
function stubBridgeUnbuildable(): void {
  (git as unknown as { bridgeToFloors: unknown }).bridgeToFloors = async () => ({ kind: "failed" });
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

describe("RunRunner — post-bridge secret scan (PRD #1416 MR-rework)", () => {
  // FIX finding 1 (P1 SECURITY): a bridge NEWLY makes a rewritten branch pushable, so the run
  // re-scans the now-trusted P..B delta on EVERY forge before pushing B. A trusted finding blocks
  // (push_secret_blocked, no preserved_patch, no push); on a gitlab/forgejo claim this worker-side
  // scan is the ONLY gate (no GH013 backstop). bridgeToFloors is NEVER stubbed here — the
  // rewritingExecutor produces a real divergent H that is bridged to a real B.
  const leakFinding = () => ({
    trusted: true as const,
    findings: [{ commit: "deadbeef", file: "leaked.env", startLine: 1, ruleId: "generic-api-key" }],
  });
  /** A gitlab task claim with forge_type set (so composePushSecretBlockedReason picks the
   *  forge-neutral wording, not the GitHub GH013 wording). */
  const gitlabClaimTyped = (branch: string) =>
    taskClaim(branch, {
      open_mr: false,
      repo: {
        id: "r1",
        url: "https://gitlab.example.test/org/repo",
        clone_url: fx.originPath,
        forge_type: "gitlab",
      },
    });
  /** A stub that fails the FIRST call open (the github top-of-finalize scan) then returns a trusted
   *  finding on every later call (the post-bridge scan). Returns a counter to assert both ran. */
  const countingLeakStub = (): { calls: () => number } => {
    let n = 0;
    git.secretScanRange = (async () => {
      n++;
      return n === 1 ? { trusted: false, findings: [] } : leakFinding();
    }) as typeof git.secretScanRange;
    return { calls: () => n };
  };

  it("(GitLab, plain path) a trusted post-bridge finding fails push_secret_blocked, no push, forge-neutral reason", async () => {
    const { gitlab } = fakeGitlab();
    const branch = "feature/pb-gitlab-plain";
    const P = publishBranch(branch);
    // A gitlab claim never runs the github top scan, so this CONSTANT stub is hit ONLY by the
    // post-bridge scan.
    git.secretScanRange = (async () => leakFinding()) as typeof git.secretScanRange;
    const claim = gitlabClaimTyped(branch);
    await runner(rewritingExecutor({}), gitlab).execute(claim);

    const statuses = statusesFor(claim.run_id);
    assert.ok(!statuses.includes("completed"), "the run did not complete (blocked before the push)");
    assert.strictEqual(gitIn(fx.originPath, ["rev-parse", branch]), P, "nothing was pushed (origin tip still P)");
    const failed = failedBody(claim.run_id);
    assert.strictEqual(failed.fail_origin, "push_secret_blocked");
    assert.strictEqual(failed.preserved_patch, undefined, "no preserved_patch on a secret block");
    const reason = failed.failure_reason ?? "";
    assert.doesNotMatch(reason, /GH013/, "the gitlab reason is forge-neutral: no GH013");
    assert.doesNotMatch(reason, /GitHub Push Protection/i, "the gitlab reason is forge-neutral: no GitHub Push Protection");
    assert.match(reason, /pre-push secret scan detected a secret/i, "the reason cites the pre-push scan");
  });

  it("(GitLab, OMITTED forge_type) a trusted post-bridge finding still gets forge-neutral wording (finding 8, R8)", async () => {
    // R8: an OMITTED forge_type means GitLab, which has no GH013 backstop. The default taskClaim repo
    // sets NO forge_type. Without the call-site normalization, composePushSecretBlockedReason would
    // default undefined → github and wrongly cite GH013/"GitHub Push Protection"; the fix normalizes
    // the omitted value to gitlab at this call site.
    const { gitlab } = fakeGitlab();
    const branch = "feature/pb-gitlab-untyped";
    const P = publishBranch(branch);
    git.secretScanRange = (async () => leakFinding()) as typeof git.secretScanRange;
    const claim = taskClaim(branch, { open_mr: false }); // repo carries no forge_type
    assert.strictEqual(claim.repo.forge_type, undefined, "the claim genuinely omits forge_type");
    await runner(rewritingExecutor({}), gitlab).execute(claim);

    assert.ok(!statusesFor(claim.run_id).includes("completed"), "the run did not complete (blocked before the push)");
    assert.strictEqual(gitIn(fx.originPath, ["rev-parse", branch]), P, "nothing was pushed (origin tip still P)");
    const failed = failedBody(claim.run_id);
    assert.strictEqual(failed.fail_origin, "push_secret_blocked");
    const reason = failed.failure_reason ?? "";
    assert.doesNotMatch(reason, /GH013/, "an omitted forge_type reason is forge-neutral: no GH013");
    assert.doesNotMatch(
      reason,
      /GitHub Push Protection/i,
      "an omitted forge_type reason is forge-neutral: no GitHub Push Protection",
    );
    assert.match(reason, /pre-push secret scan detected a secret/i, "the reason cites the pre-push scan");
  });

  it("(GitHub, plain path) the post-bridge re-scan blocks a secret the top scan failed open on", async () => {
    const { github } = fakeGitHub();
    const branch = "feature/pb-github-plain";
    const P = publishBranch(branch);
    const counter = countingLeakStub(); // 1st call (top scan) fails open; 2nd (post-bridge) finds a secret
    const claim = githubTaskClaim(branch, { open_mr: false });
    await githubRunner(github, rewritingExecutor({})).execute(claim);

    assert.ok(counter.calls() >= 2, "the post-bridge scan ran after the top scan failed open");
    const failed = failedBody(claim.run_id);
    assert.strictEqual(failed.fail_origin, "push_secret_blocked");
    assert.strictEqual(failed.preserved_patch, undefined, "no preserved_patch on a secret block");
    assert.ok(!statusesFor(claim.run_id).includes("completed"), "the run did not complete");
    assert.strictEqual(gitIn(fx.originPath, ["rev-parse", branch]), P, "nothing was pushed");
  });

  it("(GitHub, align path) a trusted post-bridge finding blocks via the align path", async () => {
    // main advances a workflow file → behind-on-workflows → the align chain engages; inside it
    // fetchAndPush bridges, re-scans P..B, and on a trusted finding reports push_secret_blocked and
    // throws the unwind sentinel (which the M4 wrap + outer catch stop without pushing).
    commitToOriginMain({ ".github/workflows/ci.yml": CI_V1 }, "seed workflows");
    const branch = "feature/pb-github-align";
    const P = publishBranch(branch);
    const { github } = fakeGitHub();
    const counter = countingLeakStub();
    const claim = githubTaskClaim(branch, { open_mr: false });
    await githubRunner(github, rewritingExecutor({}, { ".github/workflows/ci.yml": CI_V2 })).execute(claim);

    assert.ok(counter.calls() >= 2, "the post-bridge scan ran on the align path");
    const failed = failedBody(claim.run_id);
    assert.strictEqual(failed.fail_origin, "push_secret_blocked", "typed push_secret_blocked via the align path");
    assert.notStrictEqual(failed.fail_origin, "finalize_base_align_conflict");
    assert.strictEqual(failed.preserved_patch, undefined, "no preserved_patch on a secret block");
    assert.ok(!statusesFor(claim.run_id).includes("completed"), "the run did not complete");
    assert.strictEqual(gitIn(fx.originPath, ["rev-parse", branch]), P, "nothing was pushed");
  });

  it("(clean scan) a trusted post-bridge scan with no findings pushes B and completes", async () => {
    const { gitlab } = fakeGitlab();
    const branch = "feature/pb-clean";
    const P = publishBranch(branch);
    git.secretScanRange = (async () => ({ trusted: true, findings: [] })) as typeof git.secretScanRange;
    const claim = gitlabClaimTyped(branch);
    await runner(rewritingExecutor({}), gitlab).execute(claim);

    assert.ok(statusesFor(claim.run_id).includes("completed"), "a clean post-bridge scan completes the run");
    assert.notStrictEqual(
      gitIn(fx.originPath, ["rev-parse", branch]),
      P,
      "the bridge B was pushed over P (the branch advanced)",
    );
  });
});

// A bridge adopted BEFORE finalize (the agent's `git merge -s ours <P>` steer mid-run, or a worker
// bridge carried in by a resume reseed) leaves the tracking tip already descending P, so finalize
// builds no bridge of its own (kind "clean") and, before the fix, only a finalize-built bridge was
// re-scanned. On a non-GitHub forge that pushed the rewritten history unscanned: nothing else
// scans a GitLab/Forgejo push. The bridged range, and every commit made after the bridge, must be
// scanned whenever the pushed history carries a bridge.
describe("RunRunner — a bridge adopted before finalize is still secret-scanned", () => {
  const leak = () => ({
    trusted: true as const,
    findings: [{ commit: "deadbeef", file: "leaked.env", startLine: 1, ruleId: "generic-api-key" }],
  });
  /** Rewrite below P, restore P as an ancestor the agent's way (`merge -s ours`), then keep
   *  working: the tracking tip descends P before finalize ever looks. */
  const bridgedEarlyExecutor = (P: string, mainFiles?: Record<string, string>): Executor => ({
    run: async (ctx: RunContext): Promise<ExecutorResult> => {
      fs.writeFileSync(path.join(ctx.worktreePath, "REWRITE.md"), `rewrite ${randomUUID()}\n`);
      gitIn(ctx.worktreePath, ["add", "."]);
      gitIn(ctx.worktreePath, [...IDENT, "commit", "--amend", "--no-edit"]);
      gitIn(ctx.worktreePath, [...IDENT, "merge", "-s", "ours", P, "-m", "restore published tip"]);
      fs.writeFileSync(path.join(ctx.worktreePath, "AFTER.md"), "work after the bridge\n");
      gitIn(ctx.worktreePath, ["add", "."]);
      gitIn(ctx.worktreePath, [...IDENT, "commit", "-m", "work after the bridge"]);
      if (mainFiles) commitToOriginMain(mainFiles, "main advances");
      return { branch: ctx.branch };
    },
  });

  it("(GitHub, align path) a trusted top scan does not cover the re-fetched aligned tip", async () => {
    // The top-of-finalize scan walked the pre-align tip TRUSTED and clean. The align merge then
    // moves the tip, and the early bridge is carried into the pushed history, so that trust must
    // not carry over: the aligned tip is scanned, and its finding blocks the push.
    commitToOriginMain({ ".github/workflows/ci.yml": CI_V1 }, "seed workflows");
    const branch = "feature/early-bridge-align";
    const P = publishBranch(branch);
    const { github } = fakeGitHub();
    let scans = 0;
    git.secretScanRange = (async () => {
      scans++;
      return scans === 1 ? { trusted: true as const, findings: [] } : leak();
    }) as typeof git.secretScanRange;
    const claim = githubTaskClaim(branch, { open_mr: false });
    await githubRunner(github, bridgedEarlyExecutor(P, { ".github/workflows/ci.yml": CI_V2 })).execute(claim);

    assert.ok(scans >= 2, "the aligned tip was scanned after the trusted top scan");
    assert.strictEqual(failedBody(claim.run_id).fail_origin, "push_secret_blocked");
    assert.strictEqual(gitIn(fx.originPath, ["rev-parse", branch]), P, "nothing was pushed");
  });

  it("(GitLab) a trusted finding in an early-bridged branch blocks the push", async () => {
    const { gitlab } = fakeGitlab();
    const branch = "feature/early-bridge-gitlab";
    const P = publishBranch(branch);
    let scans = 0;
    git.secretScanRange = (async () => {
      scans++;
      return leak();
    }) as typeof git.secretScanRange;
    const claim = taskClaim(branch, {
      repo: { id: "r1", url: "https://gitlab.example.test/org/repo", clone_url: fx.originPath, forge_type: "gitlab" },
    });
    await runner(bridgedEarlyExecutor(P), gitlab).execute(claim);

    assert.strictEqual(scans, 1, "the early-bridged range was scanned once");
    assert.ok(!statusesFor(claim.run_id).includes("completed"), "blocked before the push");
    assert.strictEqual(gitIn(fx.originPath, ["rev-parse", branch]), P, "nothing was pushed (origin tip still P)");
    const failed = failedBody(claim.run_id);
    assert.strictEqual(failed.fail_origin, "push_secret_blocked");
    assert.strictEqual(failed.preserved_patch, undefined);
  });

  it("(GitLab) a clean scan of an early-bridged branch pushes and completes", async () => {
    const { gitlab } = fakeGitlab();
    const branch = "feature/early-bridge-clean";
    const P = publishBranch(branch);
    git.secretScanRange = (async () => ({ trusted: true, findings: [] })) as typeof git.secretScanRange;
    const claim = taskClaim(branch, {
      repo: { id: "r1", url: "https://gitlab.example.test/org/repo", clone_url: fx.originPath, forge_type: "gitlab" },
    });
    await runner(bridgedEarlyExecutor(P), gitlab).execute(claim);
    assert.ok(statusesFor(claim.run_id).includes("completed"));
    assert.notStrictEqual(gitIn(fx.originPath, ["rev-parse", branch]), P, "the bridged branch was pushed");
  });

  // history_rewritten preserves a patch only from a trusted range scan AND a clean exact-text scan.
  const tokenExecutor = (): Executor => ({
    run: async (ctx: RunContext): Promise<ExecutorResult> => {
      const t = ["gh", "p_", "x7Rq".repeat(9)].join("");
      fs.writeFileSync(path.join(ctx.worktreePath, "REWRITE.md"), `rewrite ${t}\n`);
      gitIn(ctx.worktreePath, ["add", "."]);
      gitIn(ctx.worktreePath, [...IDENT, "commit", "--amend", "--no-edit"]);
      return { branch: ctx.branch };
    },
  });

  it("(history_rewritten) a foreign secret in the diff withholds the patch despite a trusted range scan", async () => {
    const { github } = fakeGitHub();
    const branch = "feature/hr-patch-secret";
    publishBranch(branch);
    git.secretScanRange = (async () => ({ trusted: true, findings: [] })) as typeof git.secretScanRange;
    stubBridgeUnbuildable();
    const claim = githubTaskClaim(branch, { open_mr: false });
    await githubRunner(github, tokenExecutor()).execute(claim);
    const failed = failedBody(claim.run_id);
    assert.strictEqual(failed.fail_origin, "history_rewritten");
    assert.strictEqual(failed.preserved_patch, undefined);
  });

  it("(history_rewritten) an untrustworthy patch scan withholds the patch despite a trusted range scan", async () => {
    const { github } = fakeGitHub();
    const branch = "feature/hr-patch-untrusted";
    publishBranch(branch);
    git.secretScanRange = (async () => ({ trusted: true, findings: [] })) as typeof git.secretScanRange;
    git.scanPatchForSecrets = (async () => ({ trusted: false, findings: [] })) as typeof git.scanPatchForSecrets;
    stubBridgeUnbuildable();
    const claim = githubTaskClaim(branch, { open_mr: false });
    await githubRunner(github, rewritingExecutor({})).execute(claim);
    const failed = failedBody(claim.run_id);
    assert.strictEqual(failed.fail_origin, "history_rewritten");
    assert.strictEqual(failed.preserved_patch, undefined);
  });

  it("(GitLab) an ordinary branch with no bridge is not scanned (unchanged)", async () => {
    const { gitlab } = fakeGitlab();
    const branch = "feature/no-bridge";
    publishBranch(branch);
    let scans = 0;
    git.secretScanRange = (async () => {
      scans++;
      return leak();
    }) as typeof git.secretScanRange;
    const claim = taskClaim(branch, {
      repo: { id: "r1", url: "https://gitlab.example.test/org/repo", clone_url: fx.originPath, forge_type: "gitlab" },
    });
    const plain: Executor = {
      run: async (ctx: RunContext): Promise<ExecutorResult> => {
        fs.writeFileSync(path.join(ctx.worktreePath, "MORE.md"), "more\n");
        gitIn(ctx.worktreePath, ["add", "."]);
        gitIn(ctx.worktreePath, [...IDENT, "commit", "-m", "more work"]);
        return { branch: ctx.branch };
      },
    };
    await runner(plain, gitlab).execute(claim);
    assert.strictEqual(scans, 0);
    assert.ok(statusesFor(claim.run_id).includes("completed"));
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
