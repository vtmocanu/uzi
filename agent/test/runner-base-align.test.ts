import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { nullLogger } from "./helpers.js";
import { type Executor, type RunContext, type ExecutorResult } from "../src/executor.js";
import { RunRunner, composeBaseAlignConflictReason } from "../src/runner.js";
import { isNonFastForwardRejection } from "../src/git.js";
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
    strategy: "merge" | "rebase" | "workflow-subtree",
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
    // A null diff (D6 fail-open) makes canOverlay false, so the overlay is skipped and this
    // exercises the UNCHANGED merge fallback — the primary overlay path has its own tests below.
    git.changedFiles = (async () => null) as typeof git.changedFiles;
    const { github, calls } = fakeGitHub();
    const strategies = spyAlign();
    let pushCalls = 0;
    const realPush = git.pushBranch.bind(git);
    git.pushBranch = (async (...args: Parameters<typeof git.pushBranch>) => {
      pushCalls++;
      return realPush(...args);
    }) as typeof git.pushBranch;
    const exec = committingExecutor({ "impl.ts": "export const x = 1;\n" }, { ".github/workflows/ci.yml": CI_V2 });

    const claim = githubClaim(50);
    await githubRunner(github, exec).execute(claim);

    const statuses = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body.status);
    assert.deepStrictEqual(statuses, ["running", "running", "completed"]);
    assert.deepStrictEqual(strategies, ["merge"], "the merge path aligns; no rebase fallback");
    // Pins the exactly-one-push `if (!alignPushed)` guard: the align path pushed, so the
    // normal push must be skipped — one push total, never two.
    assert.strictEqual(pushCalls, 1, "the aligned branch is pushed exactly once");
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
    // null diff → overlay skipped → this exercises the UNCHANGED merge→rebase fallback.
    git.changedFiles = (async () => null) as typeof git.changedFiles;
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
    // null diff → overlay skipped → this exercises the UNCHANGED merge→rebase→preserve chain.
    git.changedFiles = (async () => null) as typeof git.changedFiles;
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

  // An UNEXPECTED throw from alignBranchWithDefault (e.g. the S3 count-mismatch guard, or any
  // git error) must NOT escape to the generic catch — which would report `failed` with a raw
  // internal message and NO preserved_patch. It must route to the typed conflict-fail path so
  // the agent's work is still preserved. This is the whole point of the feature on failure.
  it("an unexpected align error still fails typed with preserved_patch, never the raw generic catch", async () => {
    seedWorkflowsOnOrigin();
    const { github, calls } = fakeGitHub();
    let pushed = false;
    git.pushBranch = (async () => {
      pushed = true;
    }) as typeof git.pushBranch;
    // Both align attempts throw an unexpected internal error (models the count-mismatch guard).
    git.alignBranchWithDefault = (async () => {
      throw new Error("alignBranchWithDefault: rebase dropped commits (2 → 1) — refusing to push truncated work");
    }) as typeof git.alignBranchWithDefault;
    const exec = committingExecutor({ "impl.ts": "export const x = 1;\n" }, { ".github/workflows/ci.yml": CI_V2 });

    const claim = githubClaim(54);
    await githubRunner(github, exec).execute(claim);

    const statuses = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body.status);
    assert.deepStrictEqual(statuses, ["running", "running", "failed"]);
    const failed = api.states.find((s) => s.runId === claim.run_id && s.body.status === "failed")!.body;
    assert.strictEqual(failed.fail_origin, "finalize_base_align_conflict");
    // The typed reason, NOT the raw internal error the generic catch would surface.
    assert.match(failed.failure_reason ?? "", /docs\/github-bot-setup\.md/);
    assert.ok(
      !(failed.failure_reason ?? "").includes("dropped commits"),
      "the raw internal error must not reach the failure_reason (that would mean the generic catch)",
    );
    assert.ok(failed.preserved_patch, "the diff is preserved even on an unexpected align error");
    assert.match(failed.preserved_patch!, /impl\.ts/);
    assert.strictEqual(pushed, false, "no push on an align error");
    assert.strictEqual(calls.length, 0, "no PR opened on an align error");
  });

  // (e) DOUBLE-TOCTOU: the merge push is workflow-scope-rejected → rebase fallback aligns →
  // but the POST-REBASE push is ITSELF workflow-scope-rejected (main's workflow files moved
  // AGAIN during our align). This must NOT escape to the generic catch (which would report a
  // raw message, a defaulted fail_origin, and NO preserved_patch — the very data-loss bug).
  // It must route to the typed conflict-fail path: failed + finalize_base_align_conflict +
  // preserved_patch, no PR, and the raw reject text absent from failure_reason.
  it("(e) rebase-aligned push STILL workflow-scope-rejected → typed fail + preserved_patch, no raw catch", async () => {
    seedWorkflowsOnOrigin();
    // null diff → overlay skipped → this exercises the UNCHANGED merge→rebase→preserve chain.
    git.changedFiles = (async () => null) as typeof git.changedFiles;
    const { github, calls } = fakeGitHub();
    const strategies = spyAlign();
    const rejectMsg =
      "git push origin ... failed: ! [remote rejected] refs/uzi-runner/agent/issue-55 -> agent/issue-55 " +
      "(refusing to allow a Personal Access Token to create or update workflow " +
      "`.github/workflows/ci.yml` without workflow scope)";
    let pushCalls = 0;
    git.pushBranch = (async () => {
      pushCalls++;
      // BOTH pushes are workflow-scope-rejected: the first drives the rebase fallback, the
      // second models main's workflow files moving again DURING the align.
      throw new Error(rejectMsg);
    }) as typeof git.pushBranch;
    const exec = committingExecutor({ "impl.ts": "export const x = 1;\n" }, { ".github/workflows/ci.yml": CI_V2 });

    const claim = githubClaim(55);
    await githubRunner(github, exec).execute(claim);

    const statuses = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body.status);
    assert.deepStrictEqual(statuses, ["running", "running", "failed"]);
    assert.deepStrictEqual(strategies, ["merge", "rebase"], "merge rejected → rebase, then the aligned push is rejected again");
    assert.strictEqual(pushCalls, 2, "the merge push and the post-rebase push were both attempted and rejected");
    const failed = api.states.find((s) => s.runId === claim.run_id && s.body.status === "failed")!.body;
    assert.strictEqual(failed.fail_origin, "finalize_base_align_conflict", "routed to the typed fail, not the generic catch");
    assert.match(failed.failure_reason ?? "", /docs\/github-bot-setup\.md/);
    assert.ok(
      !(failed.failure_reason ?? "").includes("Personal Access Token"),
      "the raw reject text must not reach failure_reason (that would mean the generic catch)",
    );
    assert.ok(failed.preserved_patch, "the pre-align diff is preserved even on a double-TOCTOU rejection");
    assert.match(failed.preserved_patch!, /impl\.ts/, "the preserved patch carries the agent's work");
    assert.strictEqual(calls.length, 0, "no PR opened when the aligned push was rejected");
  });

  // (f) mislabel guard: a NON-workflow-scope push error on the rebase-fallback path must still
  // rethrow to the generic catch — a genuine auth/transient/protected-branch failure must NOT
  // be mislabelled as a base-align conflict. It surfaces the raw message with a defaulted
  // fail_origin and no preserved_patch.
  it("(f) non-workflow-scope error on the rebase path still surfaces via the generic catch", async () => {
    seedWorkflowsOnOrigin();
    // null diff → overlay skipped → this exercises the UNCHANGED merge→rebase fallback.
    git.changedFiles = (async () => null) as typeof git.changedFiles;
    const { github, calls } = fakeGitHub();
    const strategies = spyAlign();
    let pushCalls = 0;
    git.pushBranch = (async () => {
      pushCalls++;
      if (pushCalls === 1) {
        throw new Error(
          "git push origin ... failed: ! [remote rejected] refs/uzi-runner/agent/issue-56 -> agent/issue-56 " +
            "(refusing to allow a Personal Access Token to create or update workflow " +
            "`.github/workflows/ci.yml` without workflow scope)",
        );
      }
      // The post-rebase push fails for an unrelated reason (transient/network) — NOT a
      // workflow-scope rejection, so it must rethrow rather than be preserved-and-typed.
      throw new Error("boom: transient push failure over the network");
    }) as typeof git.pushBranch;
    const exec = committingExecutor({ "impl.ts": "export const x = 1;\n" }, { ".github/workflows/ci.yml": CI_V2 });

    const claim = githubClaim(56);
    await githubRunner(github, exec).execute(claim);

    const statuses = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body.status);
    assert.deepStrictEqual(statuses, ["running", "running", "failed"]);
    assert.deepStrictEqual(strategies, ["merge", "rebase"], "merge rejected → rebase, then the non-scope error");
    assert.strictEqual(pushCalls, 2, "the merge push and the post-rebase push were both attempted");
    const failed = api.states.find((s) => s.runId === claim.run_id && s.body.status === "failed")!.body;
    assert.notStrictEqual(
      failed.fail_origin,
      "finalize_base_align_conflict",
      "a non-workflow-scope error must NOT be mislabelled as a base-align conflict",
    );
    assert.match(failed.failure_reason ?? "", /transient push failure/, "the raw error surfaces via the generic catch");
    assert.ok(!failed.preserved_patch, "no preserved_patch on the generic-catch path");
    assert.strictEqual(calls.length, 0, "no PR opened on the failed push");
  });

  // (g) NB2 — resumed / rewritten-history edge. The merge push is workflow-scope-rejected →
  // rebase fallback aligns → but the POST-REBASE push is rejected NON-FAST-FORWARD, because the
  // rebase rewound to the original agent tip and replayed the commits, rewriting SHAs that were
  // already published at origin (a resume, or the self_improve fixed branch). Force-push is
  // denied by the guardrails, so this must NOT escape to the generic catch (raw message,
  // defaulted fail_origin, no preserved_patch); it must route to the SAME typed conflict-fail
  // path a repeat workflow-scope rejection takes: failed + finalize_base_align_conflict +
  // preserved_patch, no PR, and the raw non-fast-forward text absent from failure_reason.
  it("(g) rebase-aligned push rejected non-fast-forward (resumed branch) → typed fail + preserved_patch, no raw catch", async () => {
    seedWorkflowsOnOrigin();
    // null diff → overlay skipped → this exercises the UNCHANGED merge→rebase→preserve chain.
    git.changedFiles = (async () => null) as typeof git.changedFiles;
    const { github, calls } = fakeGitHub();
    const strategies = spyAlign();
    const nonFfMsg =
      "git push origin ... failed: ! [rejected] agent/issue-57 -> agent/issue-57 (non-fast-forward)\n" +
      "error: failed to push some refs to '...'\n" +
      "hint: Updates were rejected because the tip of your current branch is behind\n" +
      "hint: its remote counterpart. Integrate the remote changes (e.g.\n" +
      "hint: 'git pull ...') before pushing again.";
    let pushCalls = 0;
    git.pushBranch = (async () => {
      pushCalls++;
      if (pushCalls === 1) {
        // The merge push is workflow-scope-rejected → drives the rebase fallback.
        throw new Error(
          "git push origin ... failed: ! [remote rejected] refs/uzi-runner/agent/issue-57 -> agent/issue-57 " +
            "(refusing to allow a Personal Access Token to create or update workflow " +
            "`.github/workflows/ci.yml` without workflow scope)",
        );
      }
      // The post-rebase push cannot fast-forward — the rebase rewrote already-published history.
      throw new Error(nonFfMsg);
    }) as typeof git.pushBranch;
    const exec = committingExecutor({ "impl.ts": "export const x = 1;\n" }, { ".github/workflows/ci.yml": CI_V2 });

    const claim = githubClaim(57);
    await githubRunner(github, exec).execute(claim);

    const statuses = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body.status);
    assert.deepStrictEqual(statuses, ["running", "running", "failed"]);
    assert.deepStrictEqual(strategies, ["merge", "rebase"], "merge rejected → rebase, then the aligned push is non-fast-forward rejected");
    assert.strictEqual(pushCalls, 2, "the merge push and the post-rebase push were both attempted and rejected");
    const failed = api.states.find((s) => s.runId === claim.run_id && s.body.status === "failed")!.body;
    assert.strictEqual(failed.fail_origin, "finalize_base_align_conflict", "routed to the typed fail, not the generic catch");
    assert.match(failed.failure_reason ?? "", /docs\/github-bot-setup\.md/);
    assert.ok(
      !(failed.failure_reason ?? "").includes("non-fast-forward"),
      "the raw non-fast-forward text must not reach failure_reason (that would mean the generic catch)",
    );
    assert.ok(failed.preserved_patch, "the pre-align diff is preserved even on a non-fast-forward rejection");
    assert.match(failed.preserved_patch!, /impl\.ts/, "the preserved patch carries the agent's work");
    assert.strictEqual(calls.length, 0, "no PR opened when the aligned push was rejected");
  });

  // (h) issue #627 PRIMARY: base-staleness on workflows PLUS an unrelated divergence on a
  // non-workflow file. A whole-tree merge/rebase WOULD conflict (main and the branch both
  // touch the same non-workflow file), but the narrow overlay only replaces the workflow
  // subtree, so it sails through: strategies is EXACTLY ["workflow-subtree"] (merge/rebase
  // never invoked), one push, one PR, and the agent's non-workflow work survives.
  it("(h) overlay primary: unrelated non-workflow divergence → workflow-subtree overlay aligns and keeps agent work", async () => {
    seedWorkflowsOnOrigin();
    const { github, calls } = fakeGitHub();
    const strategies = spyAlign();
    let pushCalls = 0;
    const realPush = git.pushBranch.bind(git);
    git.pushBranch = (async (...args: Parameters<typeof git.pushBranch>) => {
      pushCalls++;
      return realPush(...args);
    }) as typeof git.pushBranch;
    // The branch commits a non-workflow file; main advances BOTH a workflow file AND the SAME
    // non-workflow file DIVERGENTLY — a whole-tree merge/rebase would conflict on it.
    const exec = committingExecutor(
      { "conflict.txt": "branch side\n" },
      { "conflict.txt": "main side\n", ".github/workflows/ci.yml": CI_V2 },
    );

    const claim = githubClaim(58);
    await githubRunner(github, exec).execute(claim);

    const statuses = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body.status);
    assert.deepStrictEqual(statuses, ["running", "running", "completed"]);
    // The guard against a vacuous pass: the overlay handled it and NO merge/rebase ran.
    assert.deepStrictEqual(strategies, ["workflow-subtree"], "overlay is primary; merge/rebase not invoked");
    assert.strictEqual(pushCalls, 1, "the overlaid branch is pushed exactly once");
    assert.strictEqual(calls.length, 1, "the PR was opened once");
    // On origin: the agent's non-workflow content survives AND main's workflow content landed.
    assert.strictEqual(gitIn(fx.originPath, ["show", "agent/issue-58:conflict.txt"]), "branch side");
    assert.strictEqual(
      gitIn(fx.originPath, ["show", "agent/issue-58:.github/workflows/ci.yml"]).trim(),
      CI_V2.trim(),
      "the pushed branch's workflow file matches the current default (overlaid)",
    );
  });

  // (i) issue #627 clobber-safety: the branch DID modify a workflow file, but changedFiles
  // returns null for the whole run. #377 fails OPEN (so execution reaches base-align even for a
  // workflow-editing branch), and the overlay gate canOverlay is false — so the overlay is NOT
  // attempted (it would clobber the agent's workflow edit). The fallback merge/rebase then
  // conflicts on the shared workflow file, and the run fails typed with the diff preserved.
  it("(i) clobber-safety: null diff + branch edited a workflow → overlay NOT taken, fallback conflicts, work preserved", async () => {
    seedWorkflowsOnOrigin();
    const { github, calls } = fakeGitHub();
    const strategies = spyAlign();
    let pushed = false;
    git.pushBranch = (async () => {
      pushed = true;
    }) as typeof git.pushBranch;
    // null diff → #377 fails open AND canOverlay is false → the overlay must be skipped.
    git.changedFiles = (async () => null) as typeof git.changedFiles;
    // The branch edits the SAME workflow file main diverges, forcing the whole-tree conflict.
    const exec = committingExecutor(
      { ".github/workflows/ci.yml": "name: ci\non: [branch-edit]\njobs: {}\n" },
      { ".github/workflows/ci.yml": CI_V2 },
    );

    const claim = githubClaim(59);
    await githubRunner(github, exec).execute(claim);

    const statuses = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body.status);
    assert.deepStrictEqual(statuses, ["running", "running", "failed"]);
    assert.ok(!strategies.includes("workflow-subtree"), "the overlay must NOT be attempted for a workflow-editing branch");
    assert.deepStrictEqual(strategies, ["merge", "rebase"], "the fallback merge→rebase ran instead");
    const failed = api.states.find((s) => s.runId === claim.run_id && s.body.status === "failed")!.body;
    assert.strictEqual(failed.fail_origin, "finalize_base_align_conflict");
    assert.ok(failed.preserved_patch, "the agent's own workflow edit is preserved (never clobbered)");
    assert.match(failed.preserved_patch!, /workflows\/ci\.yml/, "the preserved patch carries the branch's workflow edit");
    assert.strictEqual(pushed, false, "no clobbering push");
    assert.strictEqual(calls.length, 0, "no PR opened on the conflict fail");
  });

  // (j) issue #627 post-overlay push rejection → fallback. The overlay aligns, but its push is
  // workflow-scope-rejected (main's workflow files moved again during the align). This routes
  // to the merge/rebase fallback (NOT preserve-and-fail), which then pushes successfully.
  it("(j) overlay push still workflow-scope-rejected → falls back to merge/rebase and completes", async () => {
    seedWorkflowsOnOrigin();
    const { github, calls } = fakeGitHub();
    const strategies = spyAlign();
    let pushCalls = 0;
    git.pushBranch = (async () => {
      pushCalls++;
      if (pushCalls === 1) {
        // The overlay's push is workflow-scope-rejected → must fall back to merge/rebase.
        throw new Error(
          "git push origin ... failed: ! [remote rejected] refs/uzi-runner/agent/issue-60 -> agent/issue-60 " +
            "(refusing to allow a Personal Access Token to create or update workflow " +
            "`.github/workflows/ci.yml` without workflow scope)",
        );
      }
      // The merge fallback push succeeds.
    }) as typeof git.pushBranch;
    const exec = committingExecutor({ "impl.ts": "export const x = 1;\n" }, { ".github/workflows/ci.yml": CI_V2 });

    const claim = githubClaim(60);
    await githubRunner(github, exec).execute(claim);

    const statuses = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body.status);
    assert.deepStrictEqual(statuses, ["running", "running", "completed"]);
    assert.strictEqual(strategies[0], "workflow-subtree", "the overlay was tried FIRST");
    assert.ok(
      strategies.slice(1).includes("merge") || strategies.slice(1).includes("rebase"),
      "a merge/rebase fallback was entered AFTER the overlay push rejection",
    );
    assert.strictEqual(pushCalls, 2, "overlay push (rejected) then the fallback push (succeeds)");
    assert.strictEqual(calls.length, 1, "the PR was opened once after the successful fallback push");
  });

  // (k) issue #631 (Item 1 regression) DOUBLE-FAULT: the overlay ALIGNS then its push is
  // workflow-scope-rejected → the merge/rebase fallback both CONFLICT on an unrelated
  // non-workflow file → failBaseAlignConflict preserves the diff. Pre-fix the preserved diff
  // was taken from `trackingRef`, which the overlay's fetchAndPush had already advanced to the
  // ALIGNED tip — so it carried the workflow-subtree change as a SUPERSET. Post-fix it is the
  // pre-align agent tip's diff: exactly the agent's human-landable work, with NO workflow file.
  it("(k) double-fault: overlay push rejected then merge+rebase conflict → preserved patch is the agent's work only, NOT a workflow superset", async () => {
    seedWorkflowsOnOrigin();
    const { github, calls } = fakeGitHub();
    const strategies = spyAlign();
    let pushCalls = 0;
    git.pushBranch = (async () => {
      pushCalls++;
      // The overlay's push is workflow-scope-rejected → falls back to merge/rebase; the
      // fallback then conflicts, so there is no second push.
      throw new Error(
        "git push origin ... failed: ! [remote rejected] refs/uzi-runner/agent/issue-61 -> agent/issue-61 " +
          "(refusing to allow a Personal Access Token to create or update workflow " +
          "`.github/workflows/ci.yml` without workflow scope)",
      );
    }) as typeof git.pushBranch;
    // Branch edits a non-workflow file; main advances BOTH the workflow file AND the SAME
    // non-workflow file divergently → the overlay aligns the workflow subtree (the branch
    // touched no workflow, so canOverlay is true) but a whole-tree merge AND rebase both
    // conflict on conflict.txt.
    const exec = committingExecutor(
      { "conflict.txt": "branch side\n" },
      { "conflict.txt": "main side\n", ".github/workflows/ci.yml": CI_V2 },
    );

    const claim = githubClaim(61);
    await githubRunner(github, exec).execute(claim);

    const statuses = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body.status);
    assert.deepStrictEqual(statuses, ["running", "running", "failed"]);
    assert.strictEqual(strategies[0], "workflow-subtree", "the overlay was tried FIRST");
    assert.ok(
      strategies.includes("merge") && strategies.includes("rebase"),
      "the fallback merge+rebase ran after the overlay push rejection",
    );
    assert.strictEqual(pushCalls, 1, "only the overlay push (rejected); the conflicting fallback pushes nothing");
    const failed = api.states.find((s) => s.runId === claim.run_id && s.body.status === "failed")!.body;
    assert.strictEqual(failed.fail_origin, "finalize_base_align_conflict");
    assert.ok(failed.preserved_patch, "the pre-align diff is preserved for a human to land");
    assert.match(failed.preserved_patch!, /conflict\.txt/, "the preserved patch carries the agent's work");
    // The regression guard: pre-fix this diff came from the aligned overlay tip and carried the
    // workflow-subtree change; post-fix it is the original agent tip's diff, so it must NOT.
    assert.ok(
      !failed.preserved_patch!.includes(".github/workflows/ci.yml"),
      "the preserved patch must not carry the workflow-subtree superset",
    );
    assert.strictEqual(calls.length, 0, "no PR opened on the conflict fail");
  });

  // (l) issue #631 (Item 3): the merge is PRIMARY (null #377 diff → overlay skipped) and its
  // push is rejected NON-FAST-FORWARD (an already-published branch — a resume, or the
  // self_improve fixed branch — whose merge push cannot fast-forward). The rebase fallback also
  // cannot force-push, so this must route to the typed preserve-and-fail — NOT rethrow to the
  // generic catch (raw message, no preserved_patch), and NOT attempt a rebase.
  it("(l) merge-fallback push rejected non-fast-forward → typed fail + preserved_patch, no rebase, no raw catch", async () => {
    seedWorkflowsOnOrigin();
    // null diff → overlay skipped → the merge is the primary align strategy.
    git.changedFiles = (async () => null) as typeof git.changedFiles;
    const { github, calls } = fakeGitHub();
    const strategies = spyAlign();
    const nonFfMsg =
      "git push origin ... failed: ! [rejected] agent/issue-62 -> agent/issue-62 (non-fast-forward)\n" +
      "error: failed to push some refs to '...'\n" +
      "hint: Updates were rejected because the tip of your current branch is behind\n" +
      "hint: its remote counterpart. Integrate the remote changes (e.g.\n" +
      "hint: 'git pull ...') before pushing again.";
    let pushCalls = 0;
    git.pushBranch = (async () => {
      pushCalls++;
      // The merge push cannot fast-forward an already-published branch.
      throw new Error(nonFfMsg);
    }) as typeof git.pushBranch;
    const exec = committingExecutor({ "impl.ts": "export const x = 1;\n" }, { ".github/workflows/ci.yml": CI_V2 });

    const claim = githubClaim(62);
    await githubRunner(github, exec).execute(claim);

    const statuses = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body.status);
    assert.deepStrictEqual(statuses, ["running", "running", "failed"]);
    assert.deepStrictEqual(strategies, ["merge"], "non-ff must not trigger a rebase attempt");
    assert.strictEqual(pushCalls, 1, "only the merge push (rejected non-ff); no rebase push");
    const failed = api.states.find((s) => s.runId === claim.run_id && s.body.status === "failed")!.body;
    assert.strictEqual(failed.fail_origin, "finalize_base_align_conflict", "typed fail, not the generic catch");
    assert.ok(failed.preserved_patch, "the pre-align diff is preserved for a human to land");
    assert.match(failed.preserved_patch!, /impl\.ts/, "the preserved patch carries the agent's work");
    assert.ok(
      !(failed.failure_reason ?? "").includes("non-fast-forward"),
      "the raw non-fast-forward text must not reach failure_reason (that would mean the generic catch)",
    );
    assert.strictEqual(calls.length, 0, "no PR opened");
  });

  // (m) issue #631 (Item 2 dedup): on the overlay-primary path the base-align gate REUSES the
  // #377 guard's changedFiles result (identical barePath+trackingRef) instead of recomputing
  // it. The two counted calls are the undeclared-zero-diff guard (:1471) plus the #377 guard;
  // the align gate adds NONE (pre-fix it recomputed, making it one more — see (n)).
  it("(m) overlay-primary path reuses the #377 changedFiles result (no redundant recompute)", async () => {
    seedWorkflowsOnOrigin();
    const { github, calls } = fakeGitHub();
    let changedCalls = 0;
    const realChanged = git.changedFiles.bind(git);
    git.changedFiles = (async (...args: Parameters<typeof git.changedFiles>) => {
      changedCalls++;
      return realChanged(...args);
    }) as typeof git.changedFiles;
    const realPush = git.pushBranch.bind(git);
    git.pushBranch = (async (...args: Parameters<typeof git.pushBranch>) =>
      realPush(...args)) as typeof git.pushBranch;
    // Real, non-null diff: the branch edits a non-workflow file, main advances the workflow
    // file → the overlay aligns and pushes (as in (h)).
    const exec = committingExecutor(
      { "conflict.txt": "branch side\n" },
      { ".github/workflows/ci.yml": CI_V2 },
    );

    const claim = githubClaim(63);
    await githubRunner(github, exec).execute(claim);

    const statuses = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body.status);
    assert.deepStrictEqual(statuses, ["running", "running", "completed"]);
    assert.strictEqual(calls.length, 1, "the overlaid branch was pushed and a PR opened");
    assert.strictEqual(
      changedCalls,
      2,
      "changedFiles: zero-diff guard + #377 guard; the align gate reuses, not recomputes",
    );
  });

  // (n) issue #631 (Item 2): when #377's changedFiles FAILS OPEN (null diff), the base-align
  // gate must RECOMPUTE rather than reuse the null — so a transient diff failure still gets a
  // retry. Here changedFiles always returns null: the zero-diff guard (:1471), the #377 guard,
  // AND the align-gate recompute all fire → exactly one more call than (m), and the run still
  // reaches the fallback merge→rebase.
  it("(n) null #377 diff still recomputes at the base-align gate", async () => {
    seedWorkflowsOnOrigin();
    const { github } = fakeGitHub();
    const strategies = spyAlign();
    let changedCalls = 0;
    git.changedFiles = (async () => {
      changedCalls++;
      return null;
    }) as typeof git.changedFiles;
    git.pushBranch = (async () => {}) as typeof git.pushBranch;
    // The branch edits the SAME workflow file main diverges → the fallback merge/rebase
    // conflicts (as in (i)), proving the run reached the fallback after the recompute.
    const exec = committingExecutor(
      { ".github/workflows/ci.yml": "name: ci\non: [branch-edit]\njobs: {}\n" },
      { ".github/workflows/ci.yml": CI_V2 },
    );

    const claim = githubClaim(64);
    await githubRunner(github, exec).execute(claim);

    const statuses = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body.status);
    assert.deepStrictEqual(statuses, ["running", "running", "failed"]);
    assert.deepStrictEqual(strategies, ["merge", "rebase"], "the run reached the fallback merge→rebase");
    assert.strictEqual(
      changedCalls,
      3,
      "zero-diff guard + #377 guard + align-gate recompute (a null result is not reused)",
    );
  });
});

describe("isNonFastForwardRejection", () => {
  it("matches the stable non-ff git phrases and rejects unrelated errors", () => {
    assert.ok(
      isNonFastForwardRejection(new Error("! [rejected] agent/issue-57 -> agent/issue-57 (non-fast-forward)")),
      "matches a non-fast-forward rejection",
    );
    assert.ok(
      isNonFastForwardRejection(new Error("Updates were rejected; fetch first before pushing")),
      "matches a fetch-first rejection",
    );
    assert.ok(
      !isNonFastForwardRejection(new Error("boom: transient push failure over the network")),
      "does not match an unrelated transient error",
    );
    assert.ok(
      !isNonFastForwardRejection(
        new Error(
          "refusing to allow a Personal Access Token to create or update workflow " +
            "`.github/workflows/ci.yml` without workflow scope",
        ),
      ),
      "does not match a pure workflow-scope rejection",
    );
  });
});

describe("composeBaseAlignConflictReason", () => {
  it("keeps the doc link and preserved-diff pointer within the cap, even for a long branch name", () => {
    const short = composeBaseAlignConflictReason("main");
    assert.ok(short.length <= 512, `reason must be capped at 512 (got ${short.length})`);
    assert.match(short, /docs\/github-bot-setup\.md/);
    assert.match(short, /\.github\/workflows/);
    assert.match(short, /main/, "names the default branch");

    const long = composeBaseAlignConflictReason("x".repeat(300));
    assert.ok(long.length <= 512, `reason must be capped at 512 (got ${long.length})`);
    assert.match(long, /docs\/github-bot-setup\.md/, "doc link survives a long branch name");
    assert.match(long, /Your diff is preserved below\./, "preserved-diff pointer survives a long branch name");
    assert.match(long, /\.github\/workflows/);

    // The issue's measured regression: a 64-char branch name pushed the assembled reason to
    // 599 chars, so a blind slice(0, 512) dropped the doc link and preserved pointer.
    const sixtyFour = composeBaseAlignConflictReason("a".repeat(64));
    assert.ok(sixtyFour.length <= 512, `reason must be capped at 512 (got ${sixtyFour.length})`);
    assert.match(sixtyFour, /docs\/github-bot-setup\.md/, "doc link survives a 64-char branch name");
    assert.match(sixtyFour, /Your diff is preserved below\./, "preserved-diff pointer survives a 64-char branch name");
    assert.match(sixtyFour, /\.github\/workflows/);
  });
});
