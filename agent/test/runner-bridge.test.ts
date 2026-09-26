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
import { RunRunner } from "../src/runner.js";
import { ScratchPublicationError } from "../src/git.js";

installHarness();

// PRD #1416 M3 — the ancestry bridge at the finalize/park/reseed publication boundaries, driven
// through the REAL runner over a REAL on-disk fixture origin. A history rewrite below the published
// floor P is modelled by amending the published tip in the runner clone (the M2 test's technique),
// so the clone HEAD H genuinely diverges from origin's P.

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

/** True when `ancestor` is an ancestor of (or equal to) the origin ref `ref`. */
function originAncestor(ancestor: string, ref: string): boolean {
  try {
    execFileSync("git", ["-C", fx.originPath, "merge-base", "--is-ancestor", ancestor, ref], {
      env: ENV,
    });
    return true;
  } catch {
    return false;
  }
}

/** An executor that amends the published tip in the runner clone (rewriting history BELOW P), then
 *  optionally advances the fixture origin's main. Returns the rewritten tip H via `obs`. */
function rewritingExecutor(
  obs: { H?: string },
  mainFiles?: Record<string, string>,
): Executor {
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

const bridgeStatusLines = (runId: string): string[] =>
  api
    .messages(runId)
    .filter(
      (m) => m.kind === "status" && String(m.payload.text).includes("contains a history bridge"),
    )
    .map((m) => String(m.payload.text));

describe("RunRunner — scratch publication refusal", () => {
  for (const aligned of [false, true]) {
    it(`${aligned ? "align" : "normal"} finalize refuses a scratch-bearing H before bridge custody or remote push`, async () => {
      if (aligned) commitToOriginMain({ ".github/workflows/ci.yml": CI_V1 }, "seed workflows");
      const branch = `feature/scratch-${aligned ? "align" : "plain"}`;
      const P = publishBranch(branch);
      const { gitlab } = fakeGitlab();
      const { github } = fakeGitHub();
      let moves = 0;
      const originalMove = git.updateTrackingRef.bind(git);
      git.updateTrackingRef = (async (...args: Parameters<typeof git.updateTrackingRef>) => {
        moves++;
        return originalMove(...args);
      }) as typeof git.updateTrackingRef;
      const executor: Executor = {
        run: async (ctx) => {
          fs.mkdirSync(path.join(ctx.worktreePath, ".uzi", "scratch"), { recursive: true });
          fs.writeFileSync(path.join(ctx.worktreePath, ".uzi", "scratch", "note"), "local only\n");
          gitIn(ctx.worktreePath, ["add", "-f", ".uzi/scratch/note"]);
          gitIn(ctx.worktreePath, [...IDENT, "commit", "-m", "scratch in history"]);
          if (aligned) commitToOriginMain({ ".github/workflows/ci.yml": CI_V2 }, "main advances");
          return { branch: ctx.branch };
        },
      };
      const claim = aligned ? githubTaskClaim(branch) : taskClaim(branch);
      try {
        await (aligned ? githubRunner(github, executor) : runner(executor, gitlab)).execute(claim);
      } finally {
        git.updateTrackingRef = originalMove;
      }
      assert.equal(gitIn(fx.originPath, ["rev-parse", branch]), P, "no remote push changed the branch");
      assert.equal(moves, 0, "bridge never moved custody");
      const failed = api.states.find((s) => s.runId === claim.run_id && s.body.status === "failed")?.body;
      assert.match(failed?.failure_reason ?? "", /^scratch_publication_refused:/);
      assert.notEqual(failed?.fail_origin, "history_rewritten");
      assert.notEqual(failed?.branch_moved, true);
    });
  }
});

describe("RunRunner — bridge candidate preflight", () => {
  it("refuses B without advancing tracking custody or attempting a push", async () => {
    const branch = "feature/scratch-bridge-candidate";
    const P = publishBranch(branch);
    const { gitlab } = fakeGitlab();
    const obs: { H?: string } = {};
    const originalPreflight = git.scratchPublicationPreflight.bind(git);
    const originalMove = git.updateTrackingRef.bind(git);
    const originalPush = git.pushBranch.bind(git);
    let moves = 0;
    let pushes = 0;
    git.scratchPublicationPreflight = (async (bare, name, candidate) => {
      if (candidate && obs.H && candidate !== obs.H) {
        throw new ScratchPublicationError("candidate history contains scratch");
      }
      return originalPreflight(bare, name, candidate);
    }) as typeof git.scratchPublicationPreflight;
    git.updateTrackingRef = (async (...args: Parameters<typeof git.updateTrackingRef>) => {
      moves++;
      return originalMove(...args);
    }) as typeof git.updateTrackingRef;
    git.pushBranch = (async (...args: Parameters<typeof git.pushBranch>) => {
      pushes++;
      return originalPush(...args);
    }) as typeof git.pushBranch;
    const claim = taskClaim(branch);
    try {
      await runner(rewritingExecutor(obs), gitlab).execute(claim);
    } finally {
      git.scratchPublicationPreflight = originalPreflight;
      git.updateTrackingRef = originalMove;
      git.pushBranch = originalPush;
    }
    assert.equal(gitIn(fx.originPath, ["rev-parse", branch]), P);
    assert.equal(moves, 0);
    assert.equal(pushes, 0);
    const failed = api.states.find((s) => s.runId === claim.run_id && s.body.status === "failed")?.body;
    assert.match(failed?.failure_reason ?? "", /^scratch_publication_refused:/);
  });
});

describe("RunRunner — finalize ancestry bridge (PRD #1416 M3)", () => {
  it("(plain path) a rewritten branch with no workflow drift is bridged, B is pushed, the run completes", async () => {
    const { gitlab } = fakeGitlab();
    const branch = "feature/rewritten-plain";
    const P = publishBranch(branch);
    const obs: { H?: string } = {};
    const claim = taskClaim(branch, { open_mr: false });
    await runner(rewritingExecutor(obs), gitlab).execute(claim);

    assert.ok(statusesFor(claim.run_id).includes("completed"), "the run completed (not the generic catch)");
    // The pushed branch fast-forwards from P: P is an ancestor of what landed on origin.
    assert.ok(originAncestor(P, branch), "P is an ancestor of the pushed tip");
    assert.ok(obs.H && originAncestor(obs.H, branch), "the rewritten H is also an ancestor of the pushed tip");
    // The pushed tip is NOT P or H themselves — it is the synthesised bridge B.
    const pushed = gitIn(fx.originPath, ["rev-parse", branch]);
    assert.notStrictEqual(pushed, P);
    assert.notStrictEqual(pushed, obs.H);
  });

  it("(overlay arm) rewrite + workflow drift → the overlaid+bridged tip has BOTH P and H as ancestors", async () => {
    commitToOriginMain({ ".github/workflows/ci.yml": CI_V1 }, "seed workflows");
    const branch = "feature/rewritten-overlay";
    const P = publishBranch(branch);
    const { github } = fakeGitHub();
    const obs: { H?: string } = {};
    // The agent rewrites below P AND main advances a workflow file → behind-on-workflows + divergent.
    const claim = githubTaskClaim(branch, { open_mr: false });
    await githubRunner(github, rewritingExecutor(obs, { ".github/workflows/ci.yml": CI_V2 })).execute(claim);

    assert.ok(statusesFor(claim.run_id).includes("completed"), "the run completed");
    assert.ok(originAncestor(P, branch), "P is an ancestor of the pushed tip");
    assert.ok(obs.H && originAncestor(obs.H, branch), "H is an ancestor of the pushed tip (overlay preserved it)");
    // The aligned workflow tree matches the current default (v2).
    assert.strictEqual(
      gitIn(fx.originPath, ["show", `${branch}:.github/workflows/ci.yml`]).trim(),
      CI_V2.trim(),
      "the pushed branch's workflow file is aligned to the current default",
    );
  });

  it("(merge arm) overlay skipped (null diff) → merge-aligned+bridged tip has BOTH P and H as ancestors", async () => {
    commitToOriginMain({ ".github/workflows/ci.yml": CI_V1 }, "seed workflows");
    const branch = "feature/rewritten-merge";
    const P = publishBranch(branch);
    const { github } = fakeGitHub();
    // A null diff (D6 fail-open) makes canOverlay false → the merge fallback aligns.
    git.changedFiles = (async () => null) as typeof git.changedFiles;
    const obs: { H?: string } = {};
    const claim = githubTaskClaim(branch, { open_mr: false });
    await githubRunner(github, rewritingExecutor(obs, { ".github/workflows/ci.yml": CI_V2 })).execute(claim);

    assert.ok(statusesFor(claim.run_id).includes("completed"), "the run completed");
    assert.ok(originAncestor(P, branch), "P is an ancestor of the pushed tip");
    assert.ok(obs.H && originAncestor(obs.H, branch), "H is an ancestor of the pushed tip (merge preserved it)");
  });

  it("(rebase fallback) merge push rejected for workflow scope → rebase → bridged + pushed with P an ancestor", async () => {
    commitToOriginMain({ ".github/workflows/ci.yml": CI_V1 }, "seed workflows");
    const branch = "feature/rewritten-rebase";
    const P = publishBranch(branch);
    const { github } = fakeGitHub();
    // null diff → overlay skipped → merge→rebase fallback chain.
    git.changedFiles = (async () => null) as typeof git.changedFiles;
    // Reject the FIRST push as GitHub's workflow-scope rejection, then let it through — so the merge
    // push is rejected and the rebase fallback (which rewrites P, fact 14) runs and is bridged.
    let pushCalls = 0;
    const realPush = git.pushBranch.bind(git);
    git.pushBranch = (async (...args: Parameters<typeof git.pushBranch>) => {
      pushCalls++;
      if (pushCalls === 1) {
        throw new Error(
          "remote rejected refs/uzi-runner -> ci.yml without workflow scope",
        );
      }
      return realPush(...args);
    }) as typeof git.pushBranch;
    const obs: { H?: string } = {};
    const claim = githubTaskClaim(branch, { open_mr: false });
    await githubRunner(github, rewritingExecutor(obs, { ".github/workflows/ci.yml": CI_V2 })).execute(claim);

    assert.ok(statusesFor(claim.run_id).includes("completed"), "the run completed via the rebase fallback");
    assert.ok(pushCalls >= 2, "the first push was rejected and a second push landed");
    assert.ok(originAncestor(P, branch), "P is an ancestor of the pushed (rebased+bridged) tip");
  });

  it("(no rewrite) a clean fast-forward branch is NOT bridged and completes as today", async () => {
    const { gitlab } = fakeGitlab();
    const branch = "feature/clean";
    const P = publishBranch(branch);
    // The agent adds a commit ON TOP of P (no rewrite) → still fast-forwards, no bridge needed.
    const cleanExec: Executor = {
      run: async (ctx) => {
        fs.writeFileSync(path.join(ctx.worktreePath, "ontop.ts"), "1\n");
        gitIn(ctx.worktreePath, ["add", "."]);
        gitIn(ctx.worktreePath, [...IDENT, "commit", "-m", "clean work on top"]);
        return { branch: ctx.branch };
      },
    };
    const claim = taskClaim(branch, { open_mr: false });
    await runner(cleanExec, gitlab).execute(claim);

    assert.ok(statusesFor(claim.run_id).includes("completed"));
    assert.ok(originAncestor(P, branch), "P is still an ancestor (a plain fast-forward)");
    assert.deepStrictEqual(bridgeStatusLines(claim.run_id), [], "no history-bridge status on a clean branch");
  });
});

describe("RunRunner — the reseed after a bridge adopts B (PRD #1416 M3, SC2)", () => {
  it("a divergent tracking tip advanced to B is adopted by the reseed (seededFrom='tracking') where H alone is set aside", async () => {
    const branch = "feature/reseed";
    const P = publishBranch(branch);
    const bare = await git.ensureClone(fx.originPath);
    // A run owns the branch: seed from origin (P), then the agent rewrites below P → H.
    const rc = await git.runnerCloneForBranch(bare, branch, "feature-reseed", "R1");
    assert.strictEqual(rc.seededFrom, "origin");
    fs.writeFileSync(path.join(rc.path, "REWRITE.md"), "rewrite\n");
    gitIn(rc.path, ["add", "."]);
    gitIn(rc.path, [...IDENT, "commit", "--amend", "--no-edit"]);
    const H = gitIn(rc.path, ["rev-parse", "HEAD"]);
    await git.fetchAgentBranch(bare, rc.path, branch, "R1"); // tracking ref = H (divergent), owner R1

    // CONTROL — today's behaviour: with the tracking ref left at the divergent H, a reseed sets it
    // aside and seeds from origin, losing the rewritten work.
    const before = await git.runnerCloneForBranch(bare, branch, "feature-reseed", "R1");
    assert.strictEqual(before.seededFrom, "origin", "an un-bridged divergent tip is set aside (origin wins)");

    // BRIDGE the tracking ref to B (what bridgeBareTrackingRefIfDivergent does at the park sink).
    const bridgeResult = await git.bridgeToFloors(bare, H, [P]);
    assert.strictEqual(bridgeResult.kind, "built", "a bridge was built");
    const B = (bridgeResult as { kind: "built"; sha: string }).sha;
    await git.updateTrackingRef(bare, branch, B);

    // Now the reseed adopts B — the run resumes on its rewritten work.
    const after = await git.runnerCloneForBranch(bare, branch, "feature-reseed", "R1");
    assert.strictEqual(after.seededFrom, "tracking", "the bridged tip descends from P → adopted");
    const head = gitIn(after.path, ["rev-parse", "HEAD"]);
    assert.strictEqual(head, B, "the clone HEAD is the bridge B");
  });
});

describe("RunRunner — the MR bridge-note is history-derived, not flight-local (PRD #1416 M3, Part D)", () => {
  it("a reclaim whose finalize builds NO new bridge still reports the earlier bridge in the pushed history", async () => {
    const { gitlab, calls } = fakeGitlab();
    const branch = "feature/reclaim-note";
    const P = publishBranch(branch);
    // Model a RECLAIM: the branch's committed history already contains a bridge from a prior run (the
    // agent's own `git merge -s ours P` from the M2 steer). This finalize builds NO new bridge — the
    // clone HEAD already descends from P — so a flight-local flag would be false; only the
    // history-derived rangeContainsBridge over P..pushedTip finds it.
    const reclaimExec: Executor = {
      run: async (ctx) => {
        // clone HEAD = P (seeded from origin). Diverge below P, then restore P as an ancestor.
        gitIn(ctx.worktreePath, ["reset", "--hard", "HEAD~1"]); // to the root, below P
        fs.writeFileSync(path.join(ctx.worktreePath, "impl.ts"), "1\n");
        gitIn(ctx.worktreePath, ["add", "."]);
        gitIn(ctx.worktreePath, [...IDENT, "commit", "-m", "rewritten work"]);
        gitIn(ctx.worktreePath, [...IDENT, "merge", "-s", "ours", P, "-m", "restore published tip"]);
        return { branch: ctx.branch };
      },
    };
    const claim = taskClaim(branch, { open_mr: true });
    await runner(reclaimExec, gitlab).execute(claim);

    assert.ok(statusesFor(claim.run_id).includes("completed"), "the run completed");
    // The finalize STATUS line names the history bridge (derived from the pushed history).
    assert.ok(bridgeStatusLines(claim.run_id).length === 1, "exactly one history-bridge status line");
    // The MR body carries the generic bridge sentence.
    const post = calls.find((c) => c.method === "POST" && c.body?.includes("history bridge"));
    assert.ok(post, "the opened MR body contains the generic history-bridge sentence");
    assert.match(post!.body!, /git log --first-parent/, "the sentence is the generic one");
    assert.doesNotMatch(post!.body!, /the worker bridged/, "the sentence never says the worker bridged it");
  });
});

describe("RunRunner — a concurrent remote advance is not a rewrite (PRD #1416 M3)", () => {
  it("an aligned GitHub mr_rework whose remote branch advances reports branch_moved", async () => {
    commitToOriginMain({ ".github/workflows/ci.yml": CI_V1 }, "seed workflows");
    const branch = "agent/issue-779";
    const P = publishBranch(branch);
    const { github } = fakeGitHub();
    let concurrentTip = "";
    let alignCalls = 0;
    const originalAlign = git.alignBranchWithDefault.bind(git);
    git.alignBranchWithDefault = (async (...args: Parameters<typeof git.alignBranchWithDefault>) => {
      alignCalls++;
      return originalAlign(...args);
    }) as typeof git.alignBranchWithDefault;
    const executor: Executor = {
      run: async (ctx) => {
        fs.writeFileSync(path.join(ctx.worktreePath, "rework.ts"), "1\n");
        gitIn(ctx.worktreePath, ["add", "rework.ts"]);
        gitIn(ctx.worktreePath, [...IDENT, "commit", "-m", "rework"]);
        commitToOriginMain({ ".github/workflows/ci.yml": CI_V2 }, "main advances workflows");
        gitIn(fx.originPath, ["checkout", branch]);
        fs.writeFileSync(path.join(fx.originPath, "concurrent.md"), "someone else\n");
        gitIn(fx.originPath, ["add", "concurrent.md"]);
        gitIn(fx.originPath, [...IDENT, "commit", "-m", "concurrent writer"]);
        concurrentTip = gitIn(fx.originPath, ["rev-parse", "HEAD"]);
        gitIn(fx.originPath, ["checkout", "main"]);
        return { branch: ctx.branch };
      },
    };
    const remoteTip = () => gitIn(fx.originPath, ["rev-parse", branch]);
    const claim = githubTaskClaim(branch, { kind: "mr_rework", open_mr: true });
    try {
      await githubRunner(github, executor).execute(claim);
    } finally {
      git.alignBranchWithDefault = originalAlign;
    }
    assert.ok(alignCalls > 0, "the GitHub workflow alignment ran before publication");
    const failed = api.states.find((s) => s.runId === claim.run_id && s.body.status === "failed")?.body;
    assert.equal(failed?.branch_moved, true);
    assert.equal(failed?.fail_origin, undefined);
    assert.notEqual(concurrentTip, P);
    assert.equal(remoteTip(), concurrentTip, "the concurrent writer's commit remains the remote tip");
    assert.equal(statusesFor(claim.run_id).includes("completed"), false);
    assert.deepStrictEqual(bridgeStatusLines(claim.run_id), []);
  });

  it("an mr_rework whose branch was advanced by a concurrent writer takes branch_moved, no bridge", async () => {
    const { gitlab } = fakeGitlab();
    const branch = "agent/issue-777";
    publishBranch(branch);
    // The agent adds work on top of P (a plain fast-forward — NOT a rewrite), so P stays an ancestor.
    const exec: Executor = {
      run: async (ctx) => {
        fs.writeFileSync(path.join(ctx.worktreePath, "rework.ts"), "1\n");
        gitIn(ctx.worktreePath, ["add", "."]);
        gitIn(ctx.worktreePath, [...IDENT, "commit", "-m", "rework"]);
        // A concurrent writer advances the remote branch AFTER the clone → non-fast-forward push.
        gitIn(fx.originPath, ["checkout", branch]);
        fs.writeFileSync(path.join(fx.originPath, "concurrent.md"), "someone else\n");
        gitIn(fx.originPath, ["add", "."]);
        gitIn(fx.originPath, [...IDENT, "commit", "-m", "concurrent writer"]);
        gitIn(fx.originPath, ["checkout", "main"]);
        return { branch: ctx.branch };
      },
    };
    const claim = taskClaim(branch, {
      kind: "mr_rework",
      open_mr: true,
    });
    await runner(exec, gitlab).execute(claim).catch(() => undefined);

    const branchMoved = api.states.some(
      (s) => s.runId === claim.run_id && s.body.branch_moved === true,
    );
    assert.ok(branchMoved, "the concurrent advance is reported branch_moved (never history_rewritten)");
    assert.deepStrictEqual(bridgeStatusLines(claim.run_id), [], "no bridge on a concurrent advance");
  });

  it("refuses a concurrent remote advance whose new history contains scratch", async () => {
    const { gitlab } = fakeGitlab();
    const branch = "agent/issue-778";
    publishBranch(branch);
    const exec: Executor = {
      run: async (ctx) => {
        fs.writeFileSync(path.join(ctx.worktreePath, "rework.ts"), "1\n");
        gitIn(ctx.worktreePath, ["add", "rework.ts"]);
        gitIn(ctx.worktreePath, [...IDENT, "commit", "-m", "rework"]);
        gitIn(fx.originPath, ["checkout", branch]);
        fs.mkdirSync(path.join(fx.originPath, ".uzi", "scratch"), { recursive: true });
        fs.writeFileSync(path.join(fx.originPath, ".uzi", "scratch", "note"), "scratch\n");
        gitIn(fx.originPath, ["add", "-f", ".uzi/scratch/note"]);
        gitIn(fx.originPath, [...IDENT, "commit", "-m", "concurrent scratch writer"]);
        gitIn(fx.originPath, ["checkout", "main"]);
        return { branch: ctx.branch };
      },
    };
    const claim = taskClaim(branch, { kind: "mr_rework", open_mr: true });
    await runner(exec, gitlab).execute(claim);
    const failed = api.states.find((s) => s.runId === claim.run_id && s.body.status === "failed")?.body;
    assert.match(failed?.failure_reason ?? "", /^scratch_publication_refused:/);
    assert.notEqual(failed?.branch_moved, true);
    assert.equal(gitIn(fx.originPath, ["show", `${branch}:.uzi/scratch/note`]), "scratch");
    assert.deepStrictEqual(bridgeStatusLines(claim.run_id), []);
  });
});

describe("RunRunner.bridgeBareTrackingRefIfDivergent (PRD #1416 M3 — the shared boundary helper)", () => {
  const callBridge = (r: RunRunner, bare: string, branch: string, flight: unknown) =>
    (r as unknown as {
      bridgeBareTrackingRefIfDivergent: (
        b: string,
        br: string,
        f: unknown,
        l: unknown,
      ) => Promise<{ kind: string; bridge?: string }>;
    }).bridgeBareTrackingRefIfDivergent(bare, branch, flight, nullLogger());

  it("bridges a divergent tracking tip, advances the ref to B, and advances C to B", async () => {
    const { gitlab } = fakeGitlab();
    const branch = "feature/helper-divergent";
    const P = publishBranch(branch);
    const bare = await git.ensureClone(fx.originPath);
    const rc = await git.runnerCloneForBranch(bare, branch, "feature-h", "R1");
    gitIn(rc.path, ["add", "."]);
    fs.writeFileSync(path.join(rc.path, "REWRITE.md"), "rewrite\n");
    gitIn(rc.path, ["add", "."]);
    gitIn(rc.path, [...IDENT, "commit", "--amend", "--no-edit"]);
    const H = gitIn(rc.path, ["rev-parse", "HEAD"]);
    await git.fetchAgentBranch(bare, rc.path, branch, "R1");

    const flight = { runId: "R1", publishedTip: P, checkpointFloor: P };
    const r = runner({ run: async (c) => ({ branch: c.branch }) }, gitlab);
    const outcome = await callBridge(r, bare, branch, flight);
    assert.strictEqual(outcome.kind, "bridged");
    const B = outcome.bridge!;
    assert.strictEqual(await git.trackingTip(bare, branch), B, "the tracking ref now points at B");
    assert.strictEqual(flight.checkpointFloor, B, "C advanced to B");
    assert.strictEqual(await git.ancestry(bare, P, B), "ancestor", "P is an ancestor of B");
    assert.strictEqual(await git.ancestry(bare, H, B), "ancestor", "H is an ancestor of B");
  });

  it("is a no-op (clean) when P is already an ancestor of the tracking tip", async () => {
    const { gitlab } = fakeGitlab();
    const branch = "feature/helper-clean";
    const P = publishBranch(branch);
    const bare = await git.ensureClone(fx.originPath);
    const rc = await git.runnerCloneForBranch(bare, branch, "feature-hc", "R1");
    fs.writeFileSync(path.join(rc.path, "ontop.ts"), "1\n");
    gitIn(rc.path, ["add", "."]);
    gitIn(rc.path, [...IDENT, "commit", "-m", "clean work on top of P"]);
    await git.fetchAgentBranch(bare, rc.path, branch, "R1");
    const tipBefore = await git.trackingTip(bare, branch);

    const flight = { runId: "R1", publishedTip: P, checkpointFloor: P };
    const r = runner({ run: async (c) => ({ branch: c.branch }) }, gitlab);
    const outcome = await callBridge(r, bare, branch, flight);
    assert.strictEqual(outcome.kind, "clean");
    assert.strictEqual(await git.trackingTip(bare, branch), tipBefore, "the ref was not advanced");
  });

  it("leaves scratch-bearing clean checkpoint custody to the best-effort publish outcome", async () => {
    const { gitlab } = fakeGitlab();
    const branch = "feature/helper-clean-scratch";
    const P = publishBranch(branch);
    const bare = await git.ensureClone(fx.originPath);
    const rc = await git.runnerCloneForBranch(bare, branch, "feature-hcs", "R1");
    fs.mkdirSync(path.join(rc.path, ".uzi", "scratch"), { recursive: true });
    fs.writeFileSync(path.join(rc.path, ".uzi", "scratch", "note"), "local only\n");
    gitIn(rc.path, ["add", "-f", ".uzi/scratch/note"]);
    gitIn(rc.path, [...IDENT, "commit", "-m", "scratch checkpoint"]);
    await git.fetchAgentBranch(bare, rc.path, branch, "R1");
    const H = await git.trackingTip(bare, branch);
    const lines: string[] = [];
    const flight = {
      runId: "R1",
      publishedTip: P,
      checkpointFloor: P,
      lastCheckpointRefTip: "CONFIRMED",
      lastAttemptedCheckpointRefTip: "ATTEMPTED",
      reportedPublishOutcomes: new Set<string>(),
      runLog: nullLogger(),
      batcher: { emit(message: { payload?: { text?: string } }) {
        if (message.payload?.text) lines.push(message.payload.text);
      } },
    };
    const r = runner({ run: async (c) => ({ branch: c.branch }) }, gitlab);
    assert.deepEqual(await callBridge(r, bare, branch, flight), { kind: "clean" });
    const outcome = await (r as unknown as {
      publishCheckpointOutcome: (f: unknown, b: string, name: string) => Promise<unknown>;
    }).publishCheckpointOutcome(flight, bare, branch);
    assert.deepEqual(outcome, { published: false, reason: "scratch_publication_refused" });
    assert.equal(await git.trackingTip(bare, branch), H);
    assert.equal(flight.checkpointFloor, P);
    assert.equal(flight.lastCheckpointRefTip, "CONFIRMED");
    assert.equal(flight.lastAttemptedCheckpointRefTip, "ATTEMPTED");
    assert.deepEqual(lines, ["checkpoint publish failed: scratch_publication_refused"]);
  });

  it("returns 'unknown' (never bridges) when ancestry cannot be determined", async () => {
    const { gitlab } = fakeGitlab();
    const branch = "feature/helper-unknown";
    const P = publishBranch(branch);
    const bare = await git.ensureClone(fx.originPath);
    const rc = await git.runnerCloneForBranch(bare, branch, "feature-hu", "R1");
    fs.writeFileSync(path.join(rc.path, "REWRITE.md"), "rewrite\n");
    gitIn(rc.path, ["add", "."]);
    gitIn(rc.path, [...IDENT, "commit", "--amend", "--no-edit"]);
    await git.fetchAgentBranch(bare, rc.path, branch, "R1");
    const tipBefore = await git.trackingTip(bare, branch);

    const origAncestry = git.ancestry.bind(git);
    (git as unknown as { ancestry: unknown }).ancestry = async () => "unknown";
    try {
      const flight = { runId: "R1", publishedTip: P, checkpointFloor: P };
      const r = runner({ run: async (c) => ({ branch: c.branch }) }, gitlab);
      const outcome = await callBridge(r, bare, branch, flight);
      assert.strictEqual(outcome.kind, "unknown");
      assert.strictEqual(await git.trackingTip(bare, branch), tipBefore, "a broken read never bridges");
    } finally {
      (git as unknown as { ancestry: unknown }).ancestry = origAncestry;
    }
  });

  it("returns 'failed' when the built bridge is DEFINITIVELY malformed (tree resolves but differs from H's) — FIX 3", async () => {
    const { gitlab } = fakeGitlab();
    const branch = "feature/helper-validate-fail";
    const P = publishBranch(branch);
    const bare = await git.ensureClone(fx.originPath);
    const rc = await git.runnerCloneForBranch(bare, branch, "feature-hvf", "R1");
    fs.writeFileSync(path.join(rc.path, "REWRITE.md"), "rewrite\n");
    gitIn(rc.path, ["add", "."]);
    gitIn(rc.path, [...IDENT, "commit", "--amend", "--no-edit"]);
    const H = gitIn(rc.path, ["rev-parse", "HEAD"]);
    await git.fetchAgentBranch(bare, rc.path, branch, "R1"); // tracking ref = divergent H
    const tipBefore = await git.trackingTip(bare, branch);

    // A malformed "bridge": parents H and P (so BOTH are definitively ancestors) but P's tree, NOT
    // H's → the tree-equality check must fail DEFINITIVELY (both trees resolve and differ). Stub
    // bridgeToFloors to return it so only the tree-mismatch decides the verdict.
    const pTree = gitIn(bare, ["rev-parse", `${P}^{tree}`]);
    const malformed = gitIn(bare, [...IDENT, "commit-tree", pTree, "-p", H, "-p", P, "-m", "wrong-tree bridge"]);
    const origBridge = git.bridgeToFloors.bind(git);
    (git as unknown as { bridgeToFloors: unknown }).bridgeToFloors = async () => ({
      kind: "built",
      sha: malformed,
    });
    try {
      const flight = { runId: "R1", publishedTip: P, checkpointFloor: P };
      const r = runner({ run: async (c) => ({ branch: c.branch }) }, gitlab);
      const outcome = await callBridge(r, bare, branch, flight);
      assert.strictEqual(outcome.kind, "failed", "a tree-mismatched bridge is a definitive failure");
      assert.strictEqual(await git.trackingTip(bare, branch), tipBefore, "a failed validation never advances the ref");
    } finally {
      (git as unknown as { bridgeToFloors: unknown }).bridgeToFloors = origBridge;
    }
  });

  it("returns 'unknown' (never 'failed') when a validation READ is transient (revParse null) — FIX 3", async () => {
    const { gitlab } = fakeGitlab();
    const branch = "feature/helper-validate-unknown";
    const P = publishBranch(branch);
    const bare = await git.ensureClone(fx.originPath);
    const rc = await git.runnerCloneForBranch(bare, branch, "feature-hvu", "R1");
    fs.writeFileSync(path.join(rc.path, "REWRITE.md"), "rewrite\n");
    gitIn(rc.path, ["add", "."]);
    gitIn(rc.path, [...IDENT, "commit", "--amend", "--no-edit"]);
    await git.fetchAgentBranch(bare, rc.path, branch, "R1"); // tracking ref = divergent H
    const tipBefore = await git.trackingTip(bare, branch);

    // The divergence detection + the REAL bridge build both run; only the post-build validation's
    // tree read is stubbed transient (revParse → null). That must map to 'unknown', NOT 'failed' —
    // a finalize sink turns 'failed' into a thrown HistoryRewrittenError that would fail the run.
    const origRevParse = git.revParse.bind(git);
    (git as unknown as { revParse: unknown }).revParse = async () => null;
    try {
      const flight = { runId: "R1", publishedTip: P, checkpointFloor: P };
      const r = runner({ run: async (c) => ({ branch: c.branch }) }, gitlab);
      const outcome = await callBridge(r, bare, branch, flight);
      assert.strictEqual(outcome.kind, "unknown", "a transient validation read maps to unknown, not failed");
      assert.strictEqual(await git.trackingTip(bare, branch), tipBefore, "a transient read never advances the ref");
    } finally {
      (git as unknown as { revParse: unknown }).revParse = origRevParse;
    }
  });
});
