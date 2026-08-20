import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { nullLogger } from "./helpers.js";
import { GitCache, isWorkflowScopeRejection } from "../src/git.js";

// PRD #456 M1 — the finalize base-align git helpers, exercised over REAL on-disk repos so a
// genuine merge/rebase conflict is produced (not a stub). The fixture origin ships a real
// `.github/workflows/ci.yml`, and "main advancing on workflows after the clone base" is
// modelled by committing to the fixture origin's main AFTER `ensureClone`.

let fx: Fixture;
let git: GitCache;

const ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];

function gitIn(dir: string, args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { encoding: "utf8", env: ENV }).trim();
}

/** Advance the fixture origin's `main` by writing `files` and committing. Origin is a
 *  non-bare repo checked out on main, so this moves the branch the align target reads. */
function advanceOriginMain(originPath: string, files: Record<string, string>, msg: string): void {
  for (const [rel, content] of Object.entries(files)) {
    const target = path.join(originPath, rel);
    fs.mkdirSync(path.dirname(target), { recursive: true });
    fs.writeFileSync(target, content);
  }
  gitIn(originPath, ["add", "."]);
  gitIn(originPath, [...IDENT, "commit", "-m", msg]);
}

beforeEach(() => {
  // Origin carries a real workflow file plus a normal file used to force conflicts.
  fx = makeFixture({
    ".github/workflows/ci.yml": "name: ci\non: [push]\njobs: {}\n",
    "conflict.txt": "base\n",
  });
  git = new GitCache(fx.dataDir, nullLogger());
});

afterEach(() => fx.cleanup());

describe("isWorkflowScopeRejection", () => {
  it("is true for GitHub's workflow-scope push rejection (both phrasings)", () => {
    assert.strictEqual(
      isWorkflowScopeRejection(
        new Error(
          "git push origin ... failed: ! [remote rejected] refs/uzi-runner/agent/issue-1 -> agent/issue-1 " +
            "(refusing to allow a Personal Access Token to create or update workflow " +
            "`.github/workflows/brew.yml` without workflow scope)",
        ),
      ),
      true,
    );
    assert.strictEqual(
      isWorkflowScopeRejection("push rejected: WITHOUT WORKFLOW SCOPE"),
      true,
      "match is case-insensitive",
    );
  });

  it("is false for an unrelated push failure", () => {
    assert.strictEqual(isWorkflowScopeRejection(new Error("[rejected] non-fast-forward")), false);
    assert.strictEqual(isWorkflowScopeRejection(new Error("Authentication failed")), false);
    assert.strictEqual(isWorkflowScopeRejection(undefined), false);
    assert.strictEqual(isWorkflowScopeRejection(null), false);
  });
});

describe("fetchDefaultTip", () => {
  it("fetches the CURRENT default tip from origin (not the claim-time tip) and returns its SHA", async () => {
    const bare = await git.ensureClone(fx.originPath);
    const claimTip = gitIn(bare, ["rev-parse", "refs/remotes/origin/main"]);
    // main advances AFTER the clone — exactly the behind-on-workflows condition.
    advanceOriginMain(fx.originPath, { ".github/workflows/ci.yml": "name: ci\non: [pull_request]\n" }, "bump ci");
    const freshTip = gitIn(fx.originPath, ["rev-parse", "main"]);
    assert.notStrictEqual(freshTip, claimTip);

    const got = await git.fetchDefaultTip(bare, "main");
    assert.match(got, /^[0-9a-f]{40}$/);
    assert.strictEqual(got, freshTip, "returns the FRESH origin tip, not the stale claim-time one");
    // The remote-tracking ref was updated in the bare as a side effect.
    assert.strictEqual(gitIn(bare, ["rev-parse", "refs/remotes/origin/main"]), freshTip);
  });
});

describe("workflowTreeDiffers", () => {
  it("is true when the branch's workflow tree differs from the fresh default, false when equal", async () => {
    const bare = await git.ensureClone(fx.originPath);
    const rc = await git.createOrAttachRunnerClone(bare, 1);
    // Agent commits a NON-workflow change (the branch never touches workflows).
    fs.writeFileSync(path.join(rc.path, "impl.ts"), "export const x = 1;\n");
    gitIn(rc.path, ["add", "impl.ts"]);
    gitIn(rc.path, [...IDENT, "commit", "-m", "impl"]);
    const trackingRef = await git.fetchAgentBranch(bare, rc.path, "agent/issue-1", "run-a");

    // Before main moves, the workflow trees match → no divergence.
    const tipBefore = await git.fetchDefaultTip(bare, "main");
    assert.strictEqual(await git.workflowTreeDiffers(bare, trackingRef, tipBefore), false);

    // main advances a workflow file → divergence.
    advanceOriginMain(fx.originPath, { ".github/workflows/ci.yml": "name: ci\non: [pull_request]\n" }, "bump ci");
    const tipAfter = await git.fetchDefaultTip(bare, "main");
    assert.strictEqual(await git.workflowTreeDiffers(bare, trackingRef, tipAfter), true);
  });
});

describe("alignBranchWithDefault", () => {
  /** Seed a clone, commit `branchFile` as the agent's non-workflow work, and return the
   *  clone + its committed tip + the fresh default tip after main advances with `mainFiles`. */
  async function setup(
    branchFiles: Record<string, string>,
    mainFiles: Record<string, string>,
  ): Promise<{ bare: string; clonePath: string; baseTip: string; defaultTip: string }> {
    const bare = await git.ensureClone(fx.originPath);
    const rc = await git.createOrAttachRunnerClone(bare, 1);
    for (const [rel, content] of Object.entries(branchFiles)) {
      fs.writeFileSync(path.join(rc.path, rel), content);
    }
    gitIn(rc.path, ["add", "."]);
    gitIn(rc.path, [...IDENT, "commit", "-m", "agent work"]);
    const baseTip = gitIn(rc.path, ["rev-parse", "HEAD"]);
    advanceOriginMain(fx.originPath, mainFiles, "main advances");
    const defaultTip = await git.fetchDefaultTip(bare, "main");
    return { bare, clonePath: rc.path, baseTip, defaultTip };
  }

  it("merge: aligns a behind-on-workflows branch, keeping the agent's work AND landing the fresh workflow", async () => {
    const { clonePath, baseTip, defaultTip } = await setup(
      { "impl.ts": "export const x = 1;\n" },
      { ".github/workflows/ci.yml": "name: ci\non: [pull_request]\n" },
    );
    const res = await git.alignBranchWithDefault(clonePath, "agent/issue-1", baseTip, defaultTip, "merge");
    assert.strictEqual(res, "aligned");
    // The clone's branch now carries BOTH the agent's file and the fresh workflow content.
    assert.strictEqual(gitIn(clonePath, ["show", "HEAD:impl.ts"]), "export const x = 1;");
    assert.strictEqual(gitIn(clonePath, ["show", "HEAD:.github/workflows/ci.yml"]), "name: ci\non: [pull_request]");
    // A merge is SHA-preserving: the agent's commit is still an ancestor of the tip.
    assert.strictEqual(gitIn(clonePath, ["merge-base", "--is-ancestor", baseTip, "HEAD"]) , "");
    // The temporary align-target ref was cleaned up (rev-parse --verify exits non-zero).
    assert.throws(() =>
      execFileSync("git", ["-C", clonePath, "rev-parse", "--verify", "refs/uzi-align/target"], {
        env: ENV,
        stdio: "pipe",
      }),
    );
  });

  it("merge: returns \"conflict\" and aborts when the branch and default edit the same file divergently", async () => {
    const { clonePath, baseTip, defaultTip } = await setup(
      { "conflict.txt": "branch side\n" },
      { "conflict.txt": "main side\n", ".github/workflows/ci.yml": "name: ci\non: [pull_request]\n" },
    );
    const res = await git.alignBranchWithDefault(clonePath, "agent/issue-1", baseTip, defaultTip, "merge");
    assert.strictEqual(res, "conflict");
    // Aborted cleanly: no merge in progress, branch back at the agent tip, no conflict markers.
    assert.strictEqual(fs.existsSync(path.join(clonePath, ".git", "MERGE_HEAD")), false);
    assert.strictEqual(gitIn(clonePath, ["rev-parse", "HEAD"]), baseTip);
  });

  it("rebase: aligns cleanly and preserves the agent's commit count (S3)", async () => {
    const { clonePath, baseTip, defaultTip } = await setup(
      { "impl.ts": "export const x = 1;\n" },
      { ".github/workflows/ci.yml": "name: ci\non: [pull_request]\n" },
    );
    const res = await git.alignBranchWithDefault(clonePath, "agent/issue-1", baseTip, defaultTip, "rebase");
    assert.strictEqual(res, "aligned");
    // Rebased onto the fresh default: the default tip is now an ancestor, workflow content fresh.
    assert.strictEqual(gitIn(clonePath, ["merge-base", "--is-ancestor", defaultTip, "HEAD"]), "");
    assert.strictEqual(gitIn(clonePath, ["show", "HEAD:.github/workflows/ci.yml"]), "name: ci\non: [pull_request]");
    assert.strictEqual(gitIn(clonePath, ["show", "HEAD:impl.ts"]), "export const x = 1;");
  });

  it("rebase: returns \"conflict\" and aborts on a divergent same-file edit", async () => {
    const { clonePath, baseTip, defaultTip } = await setup(
      { "conflict.txt": "branch side\n" },
      { "conflict.txt": "main side\n", ".github/workflows/ci.yml": "name: ci\non: [pull_request]\n" },
    );
    const res = await git.alignBranchWithDefault(clonePath, "agent/issue-1", baseTip, defaultTip, "rebase");
    assert.strictEqual(res, "conflict");
    assert.strictEqual(fs.existsSync(path.join(clonePath, ".git", "rebase-merge")), false);
    assert.strictEqual(fs.existsSync(path.join(clonePath, ".git", "rebase-apply")), false);
    assert.strictEqual(gitIn(clonePath, ["rev-parse", "HEAD"]), baseTip, "branch restored to the agent tip");
  });

  it("merge-then-rebase: a rebase fallback after a clean merge replays the ORIGINAL agent commits", async () => {
    // Model the M1 fallback sequence at the git layer: merge cleanly, THEN rebase from the
    // same baseTip. The rebase must start from the original agent work, not the merge commit,
    // so the result is a linear history whose base is the fresh default.
    const { clonePath, baseTip, defaultTip } = await setup(
      { "impl.ts": "export const x = 1;\n" },
      { ".github/workflows/ci.yml": "name: ci\non: [pull_request]\n" },
    );
    assert.strictEqual(
      await git.alignBranchWithDefault(clonePath, "agent/issue-1", baseTip, defaultTip, "merge"),
      "aligned",
    );
    // refs/heads/agent/issue-1 is now the merge commit; the fallback passes the same baseTip.
    assert.strictEqual(
      await git.alignBranchWithDefault(clonePath, "agent/issue-1", baseTip, defaultTip, "rebase"),
      "aligned",
    );
    // Linear on top of the fresh default (no merge commit): HEAD has exactly one parent, and
    // the default tip is its ancestor.
    assert.strictEqual(gitIn(clonePath, ["rev-list", "--count", "--merges", `${defaultTip}..HEAD`]), "0");
    assert.strictEqual(gitIn(clonePath, ["merge-base", "--is-ancestor", defaultTip, "HEAD"]), "");
    assert.strictEqual(gitIn(clonePath, ["show", "HEAD:impl.ts"]), "export const x = 1;");
  });
});
