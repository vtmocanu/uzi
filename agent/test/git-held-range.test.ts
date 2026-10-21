import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { nullLogger, testGitCacheOptions } from "./helpers.js";
import { GitCache } from "../src/git.js";
import { prepareFrozenHeld, type FrozenHeldPolicy } from "../src/frozen-held.js";
import { defaultGitleaksShim } from "./gitleaks-shim.js";

// Issue #2545: the held publication's range. resolveCheckpointRange takes an explicit tip (the frozen
// record's aggregate, which no tracking ref names) and rangeTouchesWorkflows judges the scanned range
// commit by commit, merges by contribution, so a branch that is merely stale against the default
// branch's workflow files is not a workflow change. Real git throughout; file contents are ordinary text.

const GIT_ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };
const BRANCH = "agent/issue-77";
const WORKFLOW = ".github/workflows/ci.yml";

function git(cwd: string, ...args: string[]): string {
  return execFileSync("git", ["-C", cwd, ...args], { env: GIT_ENV, encoding: "utf8" }).trim();
}
function commitFile(repo: string, name: string, content: string): string {
  fs.mkdirSync(path.dirname(path.join(repo, name)), { recursive: true });
  fs.writeFileSync(path.join(repo, name), content);
  git(repo, "add", ".");
  git(repo, "-c", "commit.gpgsign=false", "commit", "-m", `edit ${name}`);
  return git(repo, "rev-parse", "HEAD");
}

let fx: Fixture;
let cache: GitCache;
let bare: string;
let work: string;

/** Mirror origin's branches into the bare, as the worker's fetches do. */
function sync(): void {
  git(bare, "fetch", "origin", "+refs/heads/*:refs/remotes/origin/*");
}

beforeEach(async () => {
  fx = makeFixture();
  cache = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
  bare = await cache.ensureClone(fx.originPath);
  work = fx.originPath;
  git(work, "config", "user.email", "t@t");
  git(work, "config", "user.name", "t");
});
afterEach(() => fx.cleanup());

describe("resolveCheckpointRange with an explicit tip", () => {
  it("uses the named commit as the tip and the usual floors", async () => {
    git(work, "checkout", "-q", "-b", "feature");
    const tip = commitFile(work, "a.txt", "a\n");
    sync();
    const base = git(bare, "rev-parse", "refs/remotes/origin/main");
    const range = await cache.resolveCheckpointRange(bare, BRANCH, { tipSha: tip });
    assert.ok(range);
    assert.equal(range.tipSha, tip);
    assert.equal(range.excludeSha, base, "the pack floor is the merge base with the default branch");
  });

  it("refuses a tip that is not a commit in the bare, and a malformed one", async () => {
    assert.equal(await cache.resolveCheckpointRange(bare, BRANCH, { tipSha: "0".repeat(40) }), null);
    assert.equal(await cache.resolveCheckpointRange(bare, BRANCH, { tipSha: "not-a-sha" }), null);
  });

  it("admits an extra floor only when it is an ancestor of the named tip", async () => {
    git(work, "checkout", "-q", "-b", "feature");
    const mid = commitFile(work, "a.txt", "a\n");
    const tip = commitFile(work, "b.txt", "b\n");
    git(work, "checkout", "-q", "-b", "other", "main");
    const foreign = commitFile(work, "c.txt", "c\n");
    sync();
    const range = await cache.resolveCheckpointRange(bare, BRANCH, { tipSha: tip, extraFloors: [mid, foreign] });
    assert.ok(range);
    assert.ok(range.scanFloorShas?.includes(mid));
    assert.equal(range.scanFloorShas?.includes(foreign), false, "a non-ancestor floor is dropped (#1932 admission rule)");
  });
});

describe("rangeTouchesWorkflows", () => {
  async function rangeOf(tip: string) {
    sync();
    const range = await cache.resolveCheckpointRange(bare, BRANCH, { tipSha: tip });
    assert.ok(range);
    return range;
  }

  it("is clean for ordinary commits", async () => {
    git(work, "checkout", "-q", "-b", "feature");
    commitFile(work, "a.txt", "a\n");
    const tip = commitFile(work, "b.txt", "b\n");
    assert.equal(await cache.rangeTouchesWorkflows(bare, await rangeOf(tip)), "clean");
  });

  it("detects a commit that adds, edits or removes a workflow file", async () => {
    git(work, "checkout", "-q", "-b", "feature");
    commitFile(work, "a.txt", "a\n");
    const tip = commitFile(work, WORKFLOW, "name: ci\n");
    const later = commitFile(work, "z.txt", "z\n");
    assert.equal(await cache.rangeTouchesWorkflows(bare, await rangeOf(later)), "touches");
    assert.equal(await cache.rangeTouchesWorkflows(bare, await rangeOf(tip)), "touches");
  });

  it("a branch merely stale against the default branch's workflow change is clean", async () => {
    git(work, "checkout", "-q", "-b", "feature");
    const tip = commitFile(work, "a.txt", "a\n");
    git(work, "checkout", "-q", "main");
    commitFile(work, WORKFLOW, "name: ci\n");
    assert.equal(await cache.rangeTouchesWorkflows(bare, await rangeOf(tip)), "clean");
  });

  it("a merge of the newer default branch contributes no workflow change of its own", async () => {
    git(work, "checkout", "-q", "-b", "feature");
    commitFile(work, "a.txt", "a\n");
    git(work, "checkout", "-q", "main");
    commitFile(work, WORKFLOW, "name: ci\n");
    git(work, "checkout", "-q", "feature");
    git(work, "-c", "commit.gpgsign=false", "merge", "-q", "--no-ff", "-m", "merge main", "main");
    const tip = git(work, "rev-parse", "HEAD");
    assert.equal(await cache.rangeTouchesWorkflows(bare, await rangeOf(tip)), "clean");
  });

  it("an evil merge that changes a workflow beyond both parents is detected", async () => {
    git(work, "checkout", "-q", "-b", "feature");
    commitFile(work, "a.txt", "a\n");
    git(work, "checkout", "-q", "main");
    commitFile(work, "m.txt", "m\n");
    git(work, "checkout", "-q", "feature");
    git(work, "-c", "commit.gpgsign=false", "merge", "-q", "--no-ff", "--no-commit", "main");
    fs.mkdirSync(path.join(work, ".github", "workflows"), { recursive: true });
    fs.writeFileSync(path.join(work, WORKFLOW), "name: sneaky\n");
    git(work, "add", ".");
    git(work, "-c", "commit.gpgsign=false", "commit", "-m", "evil merge");
    const tip = git(work, "rev-parse", "HEAD");
    assert.equal(await cache.rangeTouchesWorkflows(bare, await rangeOf(tip)), "touches");
  });

  it("is unknown, never clean, for a range it cannot read", async () => {
    git(work, "checkout", "-q", "-b", "feature");
    const tip = commitFile(work, "a.txt", "a\n");
    const range = await rangeOf(tip);
    assert.equal(await cache.rangeTouchesWorkflows(bare, { ...range, tipSha: "1".repeat(40) }), "unknown");
    assert.equal(await cache.rangeTouchesWorkflows(bare, { ...range, excludeSha: "bad" }), "unknown");
  });
});

// Issue #3242: a zero range (the held tip is still a default-branch commit the default has moved
// past, with no origin branch) packs the tip over its literal parents, so the pack carries the commit
// the broker must find. The scan floor, the policy checks and the pack cap are unchanged.
describe("a zero-range held publication", () => {
  const policy: FrozenHeldPolicy = { version: 1, blocked: false, everKnown: false, flaggedCommits: [], ciGlobs: null, extraFloors: [] };
  let scanCache: GitCache;
  let parent: string, tip: string, moved: string;
  beforeEach(() => {
    scanCache = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({ gitleaksBin: defaultGitleaksShim() }));
    parent = commitFile(work, "a.txt", "a\n");
    tip = commitFile(work, "b.txt", "b\n");
    commitFile(work, "c.txt", "c\n");
    git(work, "rm", "-q", "a.txt");
    git(work, "-c", "commit.gpgsign=false", "commit", "-q", "-m", "drop a.txt");
    moved = git(work, "rev-parse", "HEAD");
    sync();
  });

  it("keeps the scan floors and excludes only the tip's parents from the pack", async () => {
    const range = await scanCache.resolveCheckpointRange(bare, BRANCH, { tipSha: tip });
    assert.ok(range);
    assert.equal(range.excludeSha, tip, "the floor is the merge base, which is the tip itself");
    assert.deepEqual(range.scanFloorShas, [moved], "the scan floor is still the default tip");
    assert.deepEqual(range.packExcludeShas, [parent], "the transport excludes the literal parent");
    assert.notEqual(moved, tip);
  });

  it("prepares a bounded pack that carries the tip commit, after a trusted scan", async () => {
    let scanned: { trusted: boolean; findings: unknown[] } | undefined;
    const out = await prepareFrozenHeld({
      git: scanCache, barePath: bare, branch: BRANCH, sourceSha: tip, policy, deadlineMs: 60_000,
      onScan: (s) => { scanned = s; },
    });
    assert.ok("pack" in out, `prepared: ${JSON.stringify("reason" in out ? out.reason : "")}`);
    assert.equal(scanned?.trusted, true);
    assert.deepEqual(scanned?.findings, []);
    const probe = fs.mkdtempSync(path.join(fx.dataDir, "probe-"));
    git(probe, "init", "-q", "--bare");
    execFileSync("git", ["-C", probe, "unpack-objects", "-q"], { env: GIT_ENV, input: out.pack });
    assert.equal(git(probe, "cat-file", "-t", tip), "commit", "the pack carries the declared commit");
    assert.throws(() => git(probe, "cat-file", "-e", parent), "the parent is excluded");
  });

  it("still refuses a pack over the byte cap", async () => {
    const out = await prepareFrozenHeld({
      git: scanCache, barePath: bare, branch: BRANCH, sourceSha: tip, policy, deadlineMs: 60_000, maxBytes: 16,
    });
    assert.deepEqual(out, { reason: "pack_too_large" });
  });
});
