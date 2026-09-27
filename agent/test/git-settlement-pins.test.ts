import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { nullLogger, testGitCacheOptions } from "./helpers.js";
import { GitCache } from "../src/git.js";

// issue #1582 — GitCache.pinSettlementRefs against a REAL bare. The runner's containment
// pre-filter (isAncestorRef) now stops an absent predecessor source before it reaches this
// method, so the method's own under-lock presence guard is pinned directly here.

let fx: Fixture;
let git: GitCache;

const ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];
const RUN = "11111111-2222-3333-4444-555555555555";
const HOLD = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee";
// A well-formed 40-hex SHA that names no object in the fixture bare.
const ABSENT = "9".repeat(40);

function gitIn(dir: string, args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { encoding: "utf8", env: ENV }).trim();
}

function settleRefs(bare: string): string[] {
  const out = gitIn(bare, ["for-each-ref", "--format=%(refname) %(objectname)", "refs/uzi-settle/"]);
  return out === "" ? [] : out.split("\n").sort();
}

async function bareWithTwoCommits(): Promise<{ bare: string; mainTip: string; descTip: string }> {
  const bare = await git.ensureClone(fx.originPath);
  const mainTip = gitIn(bare, ["rev-parse", "refs/remotes/origin/main"]);
  const rc = await git.createOrAttachRunnerClone(bare, 1);
  fs.writeFileSync(path.join(rc.path, "impl.ts"), "export const x = 1;\n");
  gitIn(rc.path, ["add", "impl.ts"]);
  gitIn(rc.path, [...IDENT, "commit", "-m", "impl"]);
  await git.fetchAgentBranch(bare, rc.path, "agent/issue-1", "run-a");
  const descTip = await git.trackingTip(bare, "agent/issue-1");
  assert.ok(descTip && descTip !== mainTip);
  return { bare, mainTip, descTip };
}

beforeEach(() => {
  fx = makeFixture();
  git = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
});

afterEach(() => fx.cleanup());

describe("GitCache.pinSettlementRefs presence guard (issue #1582)", () => {
  it("pins every kind when all SHAs are present in the bare (positive control)", async () => {
    const { bare, mainTip, descTip } = await bareWithTwoCommits();
    assert.equal(await git.pinSettlementRefs(bare, RUN, HOLD, { source: mainTip, adopted: descTip }), true);
    assert.deepEqual(settleRefs(bare), [
      `refs/uzi-settle/${RUN}/${HOLD}/adopted ${descTip}`,
      `refs/uzi-settle/${RUN}/${HOLD}/source ${mainTip}`,
    ]);
  });

  it("a valid 40-hex source ABSENT from the bare returns false and writes no refs/uzi-settle ref", async () => {
    const { bare } = await bareWithTwoCommits();
    assert.equal(await git.pinSettlementRefs(bare, RUN, HOLD, { source: ABSENT }), false);
    assert.deepEqual(settleRefs(bare), []);
  });

  // Without the guard, git update-ref would itself reject a lone absent object (the throw is caught,
  // so the result is still false). The guard is what keeps the two cases below correct: a present
  // kind pinned BEFORE the absent one is refused up front, and a present non-commit object is refused.
  it("an absent SHA ordered after a present one refuses the whole set (no partial pin)", async () => {
    const { bare, mainTip } = await bareWithTwoCommits();
    // Pin order is source, adopted, pushed: the present source would be written before the absent
    // adopted failed, leaving an orphan refs/uzi-settle pin, if presence were not checked first.
    assert.equal(await git.pinSettlementRefs(bare, RUN, HOLD, { source: mainTip, adopted: ABSENT }), false);
    assert.deepEqual(settleRefs(bare), []);
  });

  it("a present object that is not a commit (a tree) is refused and pins nothing", async () => {
    const { bare, mainTip } = await bareWithTwoCommits();
    const tree = gitIn(bare, ["rev-parse", `${mainTip}^{tree}`]);
    assert.match(tree, /^[0-9a-f]{40}$/);
    assert.equal(await git.pinSettlementRefs(bare, RUN, HOLD, { source: tree }), false);
    assert.deepEqual(settleRefs(bare), []);
  });

  it("issue #1751 M2: pins the live `published` kind; deleteSettlementPin drops ONLY it; deleteSettlementRefs drops every kind", async () => {
    const { bare, mainTip, descTip } = await bareWithTwoCommits();
    assert.equal(
      await git.pinSettlementRefs(bare, RUN, HOLD, { source: mainTip, adopted: mainTip, published: descTip }),
      true,
    );
    assert.deepEqual(settleRefs(bare), [
      `refs/uzi-settle/${RUN}/${HOLD}/adopted ${mainTip}`,
      `refs/uzi-settle/${RUN}/${HOLD}/published ${descTip}`,
      `refs/uzi-settle/${RUN}/${HOLD}/source ${mainTip}`,
    ]);
    await git.deleteSettlementPin(bare, RUN, HOLD, "published");
    assert.deepEqual(settleRefs(bare), [
      `refs/uzi-settle/${RUN}/${HOLD}/adopted ${mainTip}`,
      `refs/uzi-settle/${RUN}/${HOLD}/source ${mainTip}`,
    ]);
    assert.equal(await git.pinSettlementRefs(bare, RUN, HOLD, { published: descTip }), true);
    await git.deleteSettlementRefs(bare, RUN, HOLD);
    assert.deepEqual(settleRefs(bare), [], "the released cleanup drops the published pin too");
  });

  it("issue #1751 M2: an absent published SHA refuses the whole set", async () => {
    const { bare, mainTip } = await bareWithTwoCommits();
    assert.equal(await git.pinSettlementRefs(bare, RUN, HOLD, { source: mainTip, published: ABSENT }), false);
    assert.deepEqual(settleRefs(bare), []);
  });
});
