import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { nullLogger, testGitCacheOptions } from "./helpers.js";
import { GitCache } from "../src/git.js";

// issue #1398 — the finalize secret scan must floor on the REAL push delta, not the
// default branch. On a RESUMED branch (already at refs/remotes/origin/<branch>) the floor
// is the FRESH remote branch tip; on a FIRST push it is the default branch; and any
// unreadable / diverged case fails open (null). These tests exercise
// git.resolvePublicationFloor directly — they do NOT need the gitleaks binary, and the
// "fixture" commit F carries ORDINARY text (never a secret-shaped literal, which the
// repo's gate:repo secret scan would flag).

const BRANCH = "agent/issue-99";
const TRACK = `refs/uzi-runner/${BRANCH}`;
const SCRATCH = `refs/uzi-secret-scan-floor/${BRANCH}`;
const ORIGIN_MIRROR = `refs/remotes/origin/${BRANCH}`;
const DEFAULT_MIRROR = "refs/remotes/origin/main";

// Isolate every helper git call from host/global config, matching fixture-repo.ts.
const GIT_ENV = {
  ...process.env,
  GIT_CONFIG_GLOBAL: "/dev/null",
  GIT_CONFIG_SYSTEM: "/dev/null",
  GIT_TERMINAL_PROMPT: "0",
};

function git(cwd: string, ...args: string[]): string {
  return execFileSync("git", ["-C", cwd, ...args], { env: GIT_ENV, encoding: "utf8" }).trim();
}

// Commit a benign file on `repo`'s current branch; return the new commit SHA. The content
// is deliberately ordinary text — NEVER a secret-shaped literal — because these tests
// never run gitleaks and the repo's gate:repo scan:secrets would flag a real one.
function commitFile(repo: string, name: string, content: string, msg: string): string {
  fs.writeFileSync(path.join(repo, name), content);
  git(repo, "add", ".");
  git(repo, "commit", "-m", msg);
  return git(repo, "rev-parse", "HEAD");
}

// Plant a ref in the bare from origin's `srcBranch`, WITHOUT disturbing the configured
// remote-tracking refs (`--refmap=`), so a test can carry commits origin's <branch> does
// not point at. Mirrors the non-clobber fetch resolvePublicationFloor itself uses.
function fetchIntoRef(bare: string, srcBranch: string, dstRef: string): void {
  execFileSync(
    "git",
    ["-C", bare, "fetch", "--refmap=", "origin", `+refs/heads/${srcBranch}:${dstRef}`],
    { env: GIT_ENV },
  );
}

// True when `ref` does NOT resolve in the bare (rev-parse --verify --quiet exits nonzero).
function refMissing(bare: string, ref: string): boolean {
  try {
    execFileSync("git", ["-C", bare, "rev-parse", "--verify", "--quiet", ref], {
      env: GIT_ENV,
      stdio: "pipe",
    });
    return false;
  } catch {
    return true;
  }
}

function revList(bare: string, range: string): string[] {
  const out = git(bare, "rev-list", range);
  return out.length ? out.split("\n") : [];
}

let fx: Fixture;

beforeEach(() => {
  fx = makeFixture();
});

afterEach(() => fx.cleanup());

// Build origin's history base -> F -> R on BRANCH, leaving origin checked out on `main`
// (so the worker bare's default branch resolves to `main`, distinct from R). Returns the
// three SHAs.
function seedBaseFR(): { shaBase: string; shaF: string; shaR: string } {
  const shaBase = git(fx.originPath, "rev-parse", "main");
  git(fx.originPath, "checkout", "-b", BRANCH);
  const shaF = commitFile(fx.originPath, "fixture.txt", "benign fixture marker\n", "F");
  const shaR = commitFile(fx.originPath, "r.txt", "resumed tip\n", "R");
  git(fx.originPath, "checkout", "main");
  return { shaBase, shaF, shaR };
}

describe("resolvePublicationFloor (issue #1398)", () => {
  it("resumed clean delta: floors on the fresh remote tip R, not the default branch", async () => {
    const { shaBase, shaF, shaR } = seedBaseFR();
    const gc = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
    const bare = await gc.ensureClone(fx.originPath); // mirror ORIGIN_MIRROR = R

    // Plant the tracking ref C (a clean child of R). origin's BRANCH stays at R.
    git(fx.originPath, "checkout", "-b", "src-clean", shaR);
    const shaC = commitFile(fx.originPath, "c.txt", "clean new work\n", "C");
    fetchIntoRef(bare, "src-clean", TRACK);

    const floor = await gc.resolvePublicationFloor(bare, TRACK, BRANCH);
    assert.equal(floor, shaR, "floor is the fresh remote tip R");
    assert.notEqual(floor, shaBase, "floor is NOT the default-branch tip");

    // The push delta floor..track is EXACTLY {C} and excludes the already-pushed F.
    const newRange = revList(bare, `${floor}..${TRACK}`);
    assert.deepEqual(newRange, [shaC], "delta is exactly {C}");
    assert.ok(!newRange.includes(shaF), "the already-pushed fixture F is NOT re-scanned");

    // Contrast: the OLD default-branch floor WOULD have re-scanned F (the #1398 bug).
    const oldRange = revList(bare, `${DEFAULT_MIRROR}..${TRACK}`);
    assert.ok(
      oldRange.includes(shaF),
      "sanity: the retired default-branch floor would have included F",
    );
  });

  it("fresh scratch-ref against an advanced remote: returns R2, leaves origin mirror pinned at R, cleans the scratch ref", async () => {
    const { shaF, shaR } = seedBaseFR();
    const gc = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
    const bare = await gc.ensureClone(fx.originPath); // mirror ORIGIN_MIRROR = R

    // Advance the REAL origin branch to a descendant R2.
    git(fx.originPath, "checkout", BRANCH);
    const shaR2 = commitFile(fx.originPath, "r2.txt", "advanced remote tip\n", "R2");
    // Build the tracking ref C descending from R2 (base->F->R->R2->C); origin BRANCH stays R2.
    git(fx.originPath, "checkout", "-b", "src-adv", shaR2);
    const shaC = commitFile(fx.originPath, "c.txt", "clean new work\n", "C");
    fetchIntoRef(bare, "src-adv", TRACK); // brings R2 + C objects into the bare

    const floor = await gc.resolvePublicationFloor(bare, TRACK, BRANCH);

    // (a) the FRESH tip R2, not the stale mirror R.
    assert.equal(floor, shaR2, "floor is the freshly-fetched R2, not the stale mirror R");
    assert.notEqual(floor, shaR, "floor is not the stale mirror R");

    // (b) NON-CLOBBER: refs/remotes/origin/<branch> is still pinned at R.
    assert.equal(
      git(bare, "rev-parse", ORIGIN_MIRROR),
      shaR,
      "the origin mirror must NOT be moved by the fresh fetch (issue #1117 dependency)",
    );

    // (c) the scratch ref is gone after a successful call.
    assert.ok(refMissing(bare, SCRATCH), "the scratch ref is deleted on success");

    // (d) R2..track excludes F and R.
    const range = revList(bare, `${floor}..${TRACK}`);
    assert.deepEqual(range, [shaC], "delta is exactly {C}");
    assert.ok(!range.includes(shaF), "F excluded");
    assert.ok(!range.includes(shaR), "R excluded");
  });

  it("new-secret delta placement: the new commit C is inside floor..track", async () => {
    const { shaR } = seedBaseFR();
    const gc = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
    const bare = await gc.ensureClone(fx.originPath);

    git(fx.originPath, "checkout", "-b", "src-place", shaR);
    const shaC = commitFile(fx.originPath, "c.txt", "would-carry-a-secret\n", "C");
    fetchIntoRef(bare, "src-place", TRACK);

    const floor = await gc.resolvePublicationFloor(bare, TRACK, BRANCH);
    assert.notEqual(floor, null);
    const range = revList(bare, `${floor}..${TRACK}`);
    assert.ok(range.includes(shaC), "the new commit C is inside the scanned delta");
  });

  it("first push: no origin mirror -> floors on the default branch (range contains F)", async () => {
    const shaBase = git(fx.originPath, "rev-parse", "main");
    // Build base -> F -> R -> C on a src branch; NEVER create origin's agent/issue-99.
    git(fx.originPath, "checkout", "-b", "src-first");
    const shaF = commitFile(fx.originPath, "fixture.txt", "benign fixture marker\n", "F");
    const shaR = commitFile(fx.originPath, "r.txt", "second commit\n", "R");
    const shaC = commitFile(fx.originPath, "c.txt", "third commit\n", "C");
    git(fx.originPath, "checkout", "main");

    const gc = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
    const bare = await gc.ensureClone(fx.originPath);
    fetchIntoRef(bare, "src-first", TRACK);

    assert.ok(refMissing(bare, ORIGIN_MIRROR), "precondition: no origin mirror for the branch");

    const floor = await gc.resolvePublicationFloor(bare, TRACK, BRANCH);
    assert.notEqual(floor, null, "first push resolves the default-branch floor");
    assert.equal(
      git(bare, "rev-parse", floor as string),
      shaBase,
      "the floor resolves to the default-branch tip (base)",
    );

    const range = revList(bare, `${floor}..${TRACK}`);
    assert.ok(range.includes(shaF), "a finding in F would still block on a first push");
    assert.ok(range.includes(shaR) && range.includes(shaC), "R and C are in the first-push range");
  });

  it("diverged: fresh remote tip is not an ancestor of track -> null (fail open), scratch removed", async () => {
    const { shaR } = seedBaseFR();
    const gc = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
    const bare = await gc.ensureClone(fx.originPath); // mirror = R

    // Tracking ref C: a child of R.
    git(fx.originPath, "checkout", "-b", "src-div", shaR);
    commitFile(fx.originPath, "c.txt", "our work\n", "C");
    fetchIntoRef(bare, "src-div", TRACK);

    // Advance the REAL origin branch to D — a DIFFERENT child of R, diverging from C.
    git(fx.originPath, "checkout", BRANCH); // HEAD at R
    commitFile(fx.originPath, "d.txt", "someone else's rewind\n", "D");

    const floor = await gc.resolvePublicationFloor(bare, TRACK, BRANCH);
    assert.equal(floor, null, "a diverged remote tip fails open (null)");
    assert.ok(refMissing(bare, SCRATCH), "the scratch ref is removed on the fail-open path");
  });

  it("unreadable: the fresh fetch fails -> null", async () => {
    const { shaR } = seedBaseFR();
    const gc = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
    const bare = await gc.ensureClone(fx.originPath); // mirror = R

    git(fx.originPath, "checkout", "-b", "src-unread", shaR);
    commitFile(fx.originPath, "c.txt", "our work\n", "C");
    fetchIntoRef(bare, "src-unread", TRACK);

    // Break the fresh fetch: remove the origin repo the bare points at. The local mirror
    // ref still resolves (so this is the RESUMED path), but the network fetch now fails.
    fs.rmSync(fx.originPath, { recursive: true, force: true });
    assert.ok(!refMissing(bare, ORIGIN_MIRROR), "precondition: still the resumed path");

    const floor = await gc.resolvePublicationFloor(bare, TRACK, BRANCH);
    assert.equal(floor, null, "an unreadable/unresolvable floor fails open (null)");
    assert.ok(refMissing(bare, SCRATCH), "no scratch ref is left behind");
  });
});
