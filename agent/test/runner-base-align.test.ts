import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { nullLogger } from "./helpers.js";
import { type Executor, type RunContext, type ExecutorResult } from "../src/executor.js";
import { RunRunner, composeBaseAlignConflictReason } from "../src/runner.js";
import { GitHubClient } from "../src/forge.js";
import {
  api,
  client,
  fakeGitHub,
  fx,
  git,
  gitlabClaim,
  installHarness,
} from "./runner-harness.js";

installHarness();

// PRD #456 M1/M2: the finalize base-align. A GitHub run merely BEHIND main on
// .github/workflows/** (main advanced those files after the clone base) must not lose its
// work at the finalize push. These drive the REAL runner over a REAL on-disk fixture origin
// carrying a real workflow file; "main advanced" is a genuine commit to the fixture origin's
// main during the executor turn, so the merge/rebase are exercised for real (no pure stubs).

const ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
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

/** Seed the fixture origin with a real workflow file (plus optional extras) BEFORE the run,
 *  so the clone base carries it and a later origin commit models "main moved ahead". */
function seedWorkflowsOnOrigin(extra: Record<string, string> = {}): void {
  commitToOriginMain({ ".github/workflows/ci.yml": CI_V1, ...extra }, "seed workflows");
}

function githubRunner(github: GitHubClient, executor: Executor): RunRunner {
  return new RunRunner(
    client,
    git,
    () => ({ executor }),
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

/** An executor that commits `branchFiles` as the agent's work in the runner clone, then (if
 *  `mainFiles` given) advances the fixture origin's main to model main moving ahead. */
function committingExecutor(
  branchFiles: Record<string, string>,
  mainFiles?: Record<string, string>,
): Executor {
  return {
    run: async (ctx: RunContext): Promise<ExecutorResult> => {
      for (const [rel, content] of Object.entries(branchFiles)) {
        const target = path.join(ctx.worktreePath, rel);
        fs.mkdirSync(path.dirname(target), { recursive: true });
        fs.writeFileSync(target, content);
      }
      execFileSync("git", ["-C", ctx.worktreePath, "add", "."], { env: ENV });
      execFileSync("git", ["-C", ctx.worktreePath, ...IDENT, "commit", "-m", "agent work"], {
        env: ENV,
      });
      if (mainFiles) commitToOriginMain(mainFiles, "main advances");
      return { branch: ctx.branch };
    },
  };
}

/** Record every strategy `alignBranchWithDefault` is invoked with, delegating to the real one. */
function spyAlign(): string[] {
  const strategies: string[] = [];
  const orig = git.alignBranchWithDefault.bind(git);
  git.alignBranchWithDefault = (async (
    clonePath: string,
    branch: string,
    baseTip: string,
    defaultTip: string,
    strategy: "merge" | "rebase",
  ) => {
    strategies.push(strategy);
    return orig(clonePath, branch, baseTip, defaultTip, strategy);
  }) as typeof git.alignBranchWithDefault;
  return strategies;
}

/** Count fetchDefaultTip invocations, delegating to the real one. */
function spyFetchDefaultTip(): { count: () => number } {
  let n = 0;
  const orig = git.fetchDefaultTip.bind(git);
  git.fetchDefaultTip = (async (...args: Parameters<typeof git.fetchDefaultTip>) => {
    n++;
    return orig(...args);
  }) as typeof git.fetchDefaultTip;
  return { count: () => n };
}

describe("RunRunner — finalize base-align (PRD #456)", () => {
  // (a) behind-on-workflows: main advanced a workflow file the branch never touched → the
  // merge aligns the tree and the push proceeds, landing BOTH the agent's work and the fresh
  // workflow content on origin.
  it("(a) behind-on-workflows → merge aligns → push proceeds and lands the merged tree", async () => {
    seedWorkflowsOnOrigin();
    const { github, calls } = fakeGitHub();
    const strategies = spyAlign();
    const exec = committingExecutor({ "impl.ts": "export const x = 1;\n" }, { ".github/workflows/ci.yml": CI_V2 });

    const claim = githubClaim(50);
    await githubRunner(github, exec).execute(claim);

    const statuses = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body.status);
    assert.deepStrictEqual(statuses, ["running", "running", "completed"]);
    assert.deepStrictEqual(strategies, ["merge"], "the merge path aligns; no rebase fallback");
    assert.strictEqual(calls.length, 1, "the PR was opened once");

    // The branch really landed on origin, carrying the agent's file AND the fresh workflow.
    assert.strictEqual(gitIn(fx.originPath, ["show", "agent/issue-50:impl.ts"]), "export const x = 1;");
    assert.strictEqual(
      gitIn(fx.originPath, ["show", "agent/issue-50:.github/workflows/ci.yml"]).trim(),
      CI_V2.trim(),
      "the pushed branch's workflow file matches the current default (aligned)",
    );
  });

  // (b) the merged push is STILL workflow-scope-rejected → rebase fallback → push proceeds.
  // pushBranch is stubbed to reject once (as GitHub would) then succeed, so both strategies run.
  it("(b) merge push rejected for workflow scope → rebase fallback → push proceeds", async () => {
    seedWorkflowsOnOrigin();
    const { github, calls } = fakeGitHub();
    const strategies = spyAlign();
    let pushCalls = 0;
    git.pushBranch = (async () => {
      pushCalls++;
      if (pushCalls === 1) {
        throw new Error(
          "git push origin ... failed: ! [remote rejected] refs/uzi-runner/agent/issue-51 -> agent/issue-51 " +
            "(refusing to allow a Personal Access Token to create or update workflow " +
            "`.github/workflows/ci.yml` without workflow scope)",
        );
      }
    }) as typeof git.pushBranch;
    const exec = committingExecutor({ "impl.ts": "export const x = 1;\n" }, { ".github/workflows/ci.yml": CI_V2 });

    const claim = githubClaim(51);
    await githubRunner(github, exec).execute(claim);

    const statuses = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body.status);
    assert.deepStrictEqual(statuses, ["running", "running", "completed"]);
    assert.deepStrictEqual(strategies, ["merge", "rebase"], "merge rejected → rebase fallback");
    assert.strictEqual(pushCalls, 2, "pushed once (rejected), then once after the rebase");
    assert.strictEqual(calls.length, 1, "the PR was opened once after the successful align-push");
  });

  // (c) the branch and default edited the SAME non-workflow file divergently (plus the
  // workflow divergence that triggers the align) → merge AND rebase conflict → the run fails
  // typed with the diff preserved, and NO push happens.
  it("(c) merge and rebase both conflict → failed finalize_base_align_conflict + preserved_patch, no push", async () => {
    seedWorkflowsOnOrigin({ "conflict.txt": "base\n" });
    const { github, calls } = fakeGitHub();
    const strategies = spyAlign();
    let pushed = false;
    git.pushBranch = (async () => {
      pushed = true;
    }) as typeof git.pushBranch;
    const exec = committingExecutor(
      { "conflict.txt": "branch side\n" },
      { "conflict.txt": "main side\n", ".github/workflows/ci.yml": CI_V2 },
    );

    const claim = githubClaim(52);
    await githubRunner(github, exec).execute(claim);

    const statuses = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body.status);
    assert.deepStrictEqual(statuses, ["running", "running", "failed"]);
    assert.deepStrictEqual(strategies, ["merge", "rebase"], "both strategies attempted before failing");
    const failed = api.states.find((s) => s.runId === claim.run_id && s.body.status === "failed")!.body;
    assert.strictEqual(failed.fail_origin, "finalize_base_align_conflict");
    assert.match(failed.failure_reason ?? "", /docs\/github-bot-setup\.md/);
    assert.match(failed.failure_reason ?? "", /workflows/);
    assert.ok(failed.preserved_patch, "the pre-align diff is preserved for a human to land");
    assert.match(failed.preserved_patch!, /conflict\.txt/, "the preserved patch carries the agent's work");
    assert.strictEqual(pushed, false, "the doomed push was skipped");
    assert.strictEqual(calls.length, 0, "no PR opened on the conflict fail");
  });

  // (d) already aligned: the branch's workflow tree already matches the fresh default (main
  // never moved on workflows) → no align, a single normal push, exactly one fetchDefaultTip.
  it("(d) already aligned → no align work, single push, exactly one fetchDefaultTip call", async () => {
    seedWorkflowsOnOrigin();
    const { github, calls } = fakeGitHub();
    const strategies = spyAlign();
    const fetchSpy = spyFetchDefaultTip();
    let pushCalls = 0;
    const realPush = git.pushBranch.bind(git);
    git.pushBranch = (async (...args: Parameters<typeof git.pushBranch>) => {
      pushCalls++;
      return realPush(...args);
    }) as typeof git.pushBranch;
    // The agent changes a NON-workflow file; main does NOT move → workflow trees stay equal.
    const exec = committingExecutor({ "impl.ts": "export const x = 1;\n" });

    const claim = githubClaim(53);
    await githubRunner(github, exec).execute(claim);

    const statuses = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body.status);
    assert.deepStrictEqual(statuses, ["running", "running", "completed"]);
    assert.deepStrictEqual(strategies, [], "no align was performed");
    assert.strictEqual(fetchSpy.count(), 1, "fetchDefaultTip runs exactly once (the detection probe)");
    assert.strictEqual(pushCalls, 1, "exactly one push");
    assert.strictEqual(calls.length, 1, "the PR was opened once");
    assert.strictEqual(gitIn(fx.originPath, ["show", "agent/issue-53:impl.ts"]), "export const x = 1;");
  });
});

describe("composeBaseAlignConflictReason", () => {
  it("stays within the failure_reason cap and keeps the doc link, even for a long branch name", () => {
    const short = composeBaseAlignConflictReason("main");
    assert.ok(short.length <= 512, `reason must be capped at 512 (got ${short.length})`);
    assert.match(short, /docs\/github-bot-setup\.md/);
    assert.match(short, /\.github\/workflows/);
    assert.match(short, /main/, "names the default branch");

    const long = composeBaseAlignConflictReason("x".repeat(300));
    assert.ok(long.length <= 512, `reason must be capped at 512 (got ${long.length})`);
  });
});
