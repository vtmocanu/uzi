import { it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { writeFileSync } from "node:fs";
import path from "node:path";
import { GitCache } from "../src/git.js";
import { interruptedSnapshot } from "../src/interrupted-calls.js";
import { buildPlanPrompt, buildImplementPrompt } from "../src/prompt.js";
import { makeFixture } from "./fixture-repo.js";
import { nullLogger, testGitCacheOptions, noProofReseed } from "./helpers.js";
import { fixtureFetchTracking } from "./runner-tracking-fixture.js";

// rmTreePinned (src/rmtree.ts) refuses off Linux, so the checkpoint cleanup path cannot run on macOS (#1912).
const LINUX_ONLY = process.platform !== "linux"
  ? "needs the Linux-only descriptor-pinned tree removal (src/rmtree.ts removePinnedTree/rmTreePinned refuse off Linux)"
  : false;

function gitIn(dir: string, args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], {
    encoding: "utf8", timeout: 30_000,
    env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" },
  }).trim();
}

it("M3 clean checkpoint-committed mutant remains an unvalidated snapshot without telemetry", { skip: LINUX_ONLY }, async () => {
  const fx = makeFixture();
  try {
    const git = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
    const bare = await git.ensureClone(fx.originPath);
    const seed = await git.createOrAttachRunnerClone(bare, 700, noProofReseed, "run-A");
    const file = "temporary-experimental-mutant.txt";
    const mutant = "temporary experiment: deliberately wrong candidate\n";
    writeFileSync(path.join(seed.path, file), mutant);
    gitIn(seed.path, ["add", file]);
    gitIn(seed.path, ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false",
      "commit", "-m", "temporary experimental mutant"]);
    const cpSha = gitIn(seed.path, ["rev-parse", "HEAD"]);
    await fixtureFetchTracking(git, bare, seed.path, "agent/issue-700", "run-A");
    gitIn(bare, ["update-ref", "refs/uzi-checkpoints/agent/issue-700", cpSha]);
    gitIn(bare, ["update-ref", "-d", "refs/uzi-runner/agent/issue-700"]);
    await git.removeRunnerClone(seed.path);

    const rc = await git.createOrAttachRunnerClone(bare, 700, noProofReseed, "run-B", true, cpSha);
    assert.equal(rc.seededFrom, "checkpoint");
    assert.equal(rc.baseCommit, cpSha);
    assert.equal(gitIn(rc.path, ["rev-parse", "HEAD"]), cpSha);
    assert.equal(gitIn(rc.path, ["show", "HEAD:" + file]), mutant.trim());
    assert.equal(gitIn(rc.path, ["status", "--porcelain"]), "");
    assert.equal(gitIn(rc.path, ["diff", "HEAD"]), "");
    const snapshot = interruptedSnapshot(2, rc);
    assert.ok(snapshot);
    const common = { branch: "agent/issue-700", subagentNames: ["coder"], interruptedSnapshot: snapshot };
    // Actual recovered metadata, with no telemetry or positive PriorWork input.
    // Also pin zero/unknown count independence while retaining recovered provenance.
    for (const commits of [snapshot.commits, 0, null]) {
      const input = { ...common, interruptedSnapshot: { ...snapshot, commits } };
      const prompts = [
        buildPlanPrompt({ ...input, issueIid: 700, issueTitle: "Task", issueDescription: "Do task" }),
        buildImplementPrompt({ ...input, first: true, iteration: 1, seeded: true, seededPlan: "approved" }),
      ];
      for (const prompt of prompts) {
        assert.match(prompt, /unvalidated interrupted snapshot/);
        assert.match(prompt, /temporary mutations or incomplete experiments/);
        assert.match(prompt, /clean git status or an empty diff against the recovered HEAD/);
        assert.doesNotMatch(prompt, /interrupted_calls_|and build on it|Do not redo/);
      }
    }
  } finally {
    fx.cleanup();
  }
});
