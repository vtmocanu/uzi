import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { nullLogger } from "./helpers.js";
import { GitCache, bareDirName } from "../src/git.js";

let fx: Fixture;
let git: GitCache;

beforeEach(() => {
  fx = makeFixture();
  git = new GitCache(fx.dataDir, nullLogger());
});

afterEach(() => fx.cleanup());

function gitIn(dir: string, args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], {
    encoding: "utf8",
    env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" },
  }).trim();
}

describe("bareDirName", () => {
  it("maps repo URLs to collision-free directory names", () => {
    assert.strictEqual(bareDirName("https://gitlab.com/org/repo.git"), "gitlab.com+org+repo.git");
    assert.strictEqual(bareDirName("https://gitlab.com/org/repo"), "gitlab.com+org+repo.git");
    assert.strictEqual(bareDirName("git@gitlab.com:org/repo.git"), "gitlab.com+org+repo.git");
    assert.strictEqual(bareDirName("ssh://git@gitlab.com:22/org/repo.git"), "gitlab.com%3A22+org+repo.git");
  });
});

describe("ensureClone", () => {
  it("clones bare on first call and fetches on the second", async () => {
    const bare = await git.ensureClone(fx.originPath);
    assert.strictEqual(fs.existsSync(path.join(bare, "HEAD")), true);
    // Second call refreshes the existing cache and returns the same path.
    const bareAgain = await git.ensureClone(fx.originPath);
    assert.strictEqual(bareAgain, bare);
  });

  it("never writes the PAT to the bare repo config", async () => {
    const secret = "super-secret-pat-value-999";
    const bare = await git.ensureClone(fx.originPath, secret);
    const cfg = fs.readFileSync(path.join(bare, "config"), "utf8");
    assert.ok(!cfg.includes(secret));
    assert.ok(!cfg.includes("extraHeader"));
  });
});

describe("worktree lifecycle", () => {
  it("creates a worktree on agent/issue-N off the default branch", async () => {
    const bare = await git.ensureClone(fx.originPath);
    const wt = await git.createOrAttachWorktree(bare, 42);

    assert.strictEqual(wt.branch, "agent/issue-42");
    assert.strictEqual(fs.existsSync(path.join(wt.path, "README.md")), true); // origin content checked out
    // A worktree's .git is a file pointing at the shared repo, not a directory.
    assert.strictEqual(fs.statSync(path.join(wt.path, ".git")).isFile(), true);
    assert.match(gitIn(bare, ["rev-parse", "--verify", "refs/heads/agent/issue-42"]), /^[0-9a-f]{40}$/);
  });

  it("attaches to an existing agent/issue-N branch on a later run", async () => {
    const bare = await git.ensureClone(fx.originPath);
    const first = await git.createOrAttachWorktree(bare, 7);

    // Advance the branch beyond the default so attach-vs-recreate is observable.
    fs.writeFileSync(path.join(first.path, "EXTRA.txt"), "x");
    gitIn(first.path, ["add", "EXTRA.txt"]);
    gitIn(first.path, ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false", "commit", "-m", "extra"]);
    const advancedSha = gitIn(first.path, ["rev-parse", "HEAD"]);

    await git.removeWorktree(bare, first.path);
    const second = await git.createOrAttachWorktree(bare, 7);

    assert.strictEqual(second.branch, "agent/issue-7");
    assert.strictEqual(second.path, first.path);
    // Attached to the advanced branch (has EXTRA.txt), not recreated off default.
    assert.strictEqual(fs.existsSync(path.join(second.path, "EXTRA.txt")), true);
    assert.strictEqual(gitIn(bare, ["rev-parse", "refs/heads/agent/issue-7"]), advancedSha);
  });

  it("removes the worktree but keeps the bare clone and branch", async () => {
    const bare = await git.ensureClone(fx.originPath);
    const wt = await git.createOrAttachWorktree(bare, 5);

    await git.removeWorktree(bare, wt.path);

    assert.strictEqual(fs.existsSync(wt.path), false);
    assert.strictEqual(fs.existsSync(path.join(bare, "HEAD")), true);
    assert.match(gitIn(bare, ["rev-parse", "--verify", "refs/heads/agent/issue-5"]), /^[0-9a-f]{40}$/);
  });
});
