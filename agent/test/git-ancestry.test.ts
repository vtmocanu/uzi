import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { nullLogger } from "./helpers.js";
import { GitCache } from "../src/git.js";

// PRD #1416 M2 — the tri-state, error-safe ancestry helper `git.ancestry(bare, floor, tip)`,
// exercised over REAL on-disk repos (the git-align harness shape). The runner's divergence
// detection (M2) and the finalize bridge (M3) both hang off it, so a false "divergent" on a
// broken read would arm a spurious steer. It answers "ancestor" (floor fast-forwards to tip),
// "divergent" (both present, floor NOT an ancestor of tip), and "unknown" (any git error, a
// missing/unresolvable ref, or a malformed OID) — NEVER coercing an error to "divergent".

let fx: Fixture;
let git: GitCache;

const ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];

function gitIn(dir: string, args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { encoding: "utf8", env: ENV }).trim();
}

beforeEach(() => {
  fx = makeFixture();
  git = new GitCache(fx.dataDir, nullLogger());
});

afterEach(() => fx.cleanup());

describe("GitCache.ancestry (PRD #1416 M2)", () => {
  it("returns 'ancestor' when the floor is an ancestor of the tip (fast-forward)", async () => {
    const bare = await git.ensureClone(fx.originPath);
    const mainTip = gitIn(bare, ["rev-parse", "refs/remotes/origin/main"]);
    // A runner clone commits ON TOP of main → its tip strictly descends from main.
    const rc = await git.createOrAttachRunnerClone(bare, 1);
    fs.writeFileSync(path.join(rc.path, "impl.ts"), "export const x = 1;\n");
    gitIn(rc.path, ["add", "impl.ts"]);
    gitIn(rc.path, [...IDENT, "commit", "-m", "impl"]);
    await git.fetchAgentBranch(bare, rc.path, "agent/issue-1", "run-a");
    const descTip = await git.trackingTip(bare, "agent/issue-1");
    assert.ok(descTip && descTip !== mainTip);

    assert.equal(await git.ancestry(bare, mainTip, descTip), "ancestor");
    // Equal floor/tip is the degenerate ancestor case.
    assert.equal(await git.ancestry(bare, mainTip, mainTip), "ancestor");
  });

  it("returns 'divergent' for a rewritten/diverged tip (both present, floor not an ancestor)", async () => {
    const bare = await git.ensureClone(fx.originPath);
    const mainTip = gitIn(bare, ["rev-parse", "refs/remotes/origin/main"]);
    const rc = await git.createOrAttachRunnerClone(bare, 1);
    // First history X: commit on top of main, fetched into the bare (objects retained there).
    fs.writeFileSync(path.join(rc.path, "a.ts"), "export const a = 1;\n");
    gitIn(rc.path, ["add", "a.ts"]);
    gitIn(rc.path, [...IDENT, "commit", "-m", "X"]);
    await git.fetchAgentBranch(bare, rc.path, "agent/issue-1", "run-a");
    const tipX = await git.trackingTip(bare, "agent/issue-1");
    // REWRITE below X: reset to main and commit a DIFFERENT change → tipY shares main with X
    // but neither descends from the other. The `+` fetch force-moves the tracking ref to Y.
    gitIn(rc.path, ["reset", "--hard", mainTip]);
    fs.writeFileSync(path.join(rc.path, "b.ts"), "export const b = 2;\n");
    gitIn(rc.path, ["add", "b.ts"]);
    gitIn(rc.path, [...IDENT, "commit", "-m", "Y"]);
    await git.fetchAgentBranch(bare, rc.path, "agent/issue-1", "run-a");
    const tipY = await git.trackingTip(bare, "agent/issue-1");
    assert.ok(tipX && tipY && tipX !== tipY);

    // X is NOT an ancestor of Y (the rewrite) — both objects are present in the bare.
    assert.equal(await git.ancestry(bare, tipX!, tipY!), "divergent");
    // main is still an ancestor of the rewritten tip Y (the rewrite was BELOW X, above main).
    assert.equal(await git.ancestry(bare, mainTip, tipY!), "ancestor");
  });

  it("returns 'unknown' for a missing/unresolvable ref, never 'divergent'", async () => {
    const bare = await git.ensureClone(fx.originPath);
    const mainTip = gitIn(bare, ["rev-parse", "refs/remotes/origin/main"]);
    const absent = "0123456789abcdef0123456789abcdef01234567"; // valid shape, not in the repo
    assert.equal(await git.ancestry(bare, absent, mainTip), "unknown");
    assert.equal(await git.ancestry(bare, mainTip, absent), "unknown");
  });

  it("returns 'unknown' on a git error (a nonexistent bare path)", async () => {
    const mainTip = "0123456789abcdef0123456789abcdef01234567";
    assert.equal(
      await git.ancestry(path.join(fx.dataDir, "no-such-bare.git"), mainTip, mainTip),
      "unknown",
    );
  });

  it("returns 'unknown' for a malformed floor/tip without running git", async () => {
    const bare = await git.ensureClone(fx.originPath);
    const mainTip = gitIn(bare, ["rev-parse", "refs/remotes/origin/main"]);
    assert.equal(await git.ancestry(bare, "not-a-sha", mainTip), "unknown");
    assert.equal(await git.ancestry(bare, mainTip, "deadbeef"), "unknown"); // too short
    assert.equal(await git.ancestry(bare, "", mainTip), "unknown");
  });
});
