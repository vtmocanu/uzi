import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { nullLogger, testGitCacheOptions, noProofReseed } from "./helpers.js";
import { GitCache, MAX_OWED_CANDIDATES_PER_RUN, type FetchAgentBranchOptions, type TrackingUpdateResult } from "../src/git.js";

// Issue #1924: the per-run cap on retained owed candidates, enforced under the bare lock.
const ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];
const BRANCH = "agent/issue-1924";
const RUN = "19241924-aaaa-bbbb-cccc-444444444444";
const CAP = 6; // the cache under test is built with this cap; the real constant is exercised separately
let fx: Fixture;
let cache: GitCache;
let realCap: GitCache;
let bare: string;
let clone: string;
let base: string;
let opts: FetchAgentBranchOptions;
let seq = 0;

function gitIn(dir: string, args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { encoding: "utf8", env: ENV }).trim();
}
/** A new commit with its own tree-identical content; moves the clone branch only when asked. */
function commitTree(label: string, moveBranch: boolean): string {
  const tree = gitIn(clone, ["rev-parse", `${base}^{tree}`]);
  const sha = gitIn(clone, [...IDENT, "commit-tree", tree, "-p", base, "-m", `${label} ${seq++}`]);
  if (moveBranch) gitIn(clone, ["update-ref", `refs/heads/${BRANCH}`, sha]);
  return sha;
}
const fetchHead = (): Promise<TrackingUpdateResult> => cache.fetchAgentBranch(bare, clone, BRANCH, RUN, opts);
async function pinned(): Promise<string[]> {
  return (await cache.enumerateOwedCandidates(bare, RUN)).map((c) => c.sha).sort();
}
async function rewriteTimes(n: number): Promise<string[]> {
  const heads: string[] = [];
  for (let i = 0; i < n; i++) {
    heads.push(commitTree("rewrite", true));
    const result = await fetchHead();
    assert.equal(result.kind, "updated", `rewrite ${i + 1}: ${JSON.stringify(result)}`);
  }
  return heads;
}
const stageRefs = (): string[] => gitIn(bare, ["for-each-ref", "--format=%(refname)", "refs/uzi-incoming/"]).split("\n").filter(Boolean);

beforeEach(async () => {
  fx = makeFixture();
  cache = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({ maxOwedCandidates: CAP }));
  realCap = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
  bare = await cache.ensureClone(fx.originPath);
  base = gitIn(bare, ["rev-parse", "refs/remotes/origin/main"]);
  clone = (await cache.runnerCloneForBranch(bare, BRANCH, "issue-1924", noProofReseed, RUN)).path;
  opts = { context: { runId: RUN, generation: 1, branch: BRANCH, kind: "issue", barePath: bare,
    defaultIdentity: { ref: "refs/remotes/origin/main", sha: base } } };
});
afterEach(() => fx.cleanup());

describe("issue1924 owed candidate cap", { timeout: 300_000 }, () => {
  it("N-1 and N distinct heads are retained, N+1 is refused, re-pinning is free, release restores capacity", async () => {
    const kept = await rewriteTimes(CAP - 1);
    assert.equal((await pinned()).length, CAP - 1, "N-1 heads: all retained");
    kept.push(...await rewriteTimes(1));
    assert.deepEqual(await pinned(), [...kept].sort(), "N heads: all retained");

    // N+1: refused, tracking ref unmoved, no stage ref, nothing retained was deleted.
    const tipBefore = gitIn(bare, ["rev-parse", `refs/uzi-runner/${BRANCH}`]);
    commitTree("over the cap", true);
    assert.deepEqual(await fetchHead(), { kind: "not_updated", reason: "owed_limit" });
    assert.equal(gitIn(bare, ["rev-parse", `refs/uzi-runner/${BRANCH}`]), tipBefore, "tracking ref is not advanced");
    assert.deepEqual(stageRefs(), [], "the refused head leaves no stage ref behind");
    assert.deepEqual(await pinned(), [...kept].sort(), "every retained head is still recoverable after the cap is hit");
    for (const sha of kept) assert.equal(gitIn(bare, ["rev-parse", `refs/uzi-owed/${RUN}/${sha}`]), sha);

    // Re-pinning an already-pinned head (displaced and incoming both pinned) consumes no capacity.
    gitIn(clone, ["update-ref", `refs/heads/${BRANCH}`, kept[0]!]);
    for (let i = 0; i < 3; i++) assert.equal((await fetchHead()).kind, "updated");
    assert.equal((await pinned()).length, CAP);

    // A confirmed descendant of every head releases every pin AND its candidate metadata.
    const tree = gitIn(clone, ["rev-parse", `${base}^{tree}`]);
    const merge = gitIn(clone, [...IDENT, "commit-tree", tree, ...kept.flatMap((k) => ["-p", k]), "-m", "confirm"]);
    gitIn(clone, ["update-ref", "refs/heads/confirm", merge]);
    gitIn(bare, ["fetch", "-q", "--no-tags", "--no-write-fetch-head", `file://${clone}`, "+refs/heads/confirm:refs/uzi-test/confirm"]);
    const out = await cache.reconcileOwedCandidates(bare, RUN, merge);
    assert.equal(out.removedShas.length, CAP);
    assert.deepEqual(await pinned(), []);
    const files = execFileSync("ls", [`${bare}/uzi-owed`], { encoding: "utf8" }).split("\n").filter((n) => n.startsWith("candidate-"));
    assert.deepEqual(files, [], "candidate metadata does not outlive its pin");
    commitTree("after release", true);
    assert.equal((await fetchHead()).kind, "updated", "capacity is free again");
  });

  it("concurrent attempts cannot overshoot the cap", async () => {
    await rewriteTimes(CAP - 2);
    const shas = Array.from({ length: 6 }, () => commitTree("racer", false));
    for (const [i, sha] of shas.entries()) {
      gitIn(clone, ["update-ref", `refs/heads/racer-${i}`, sha]);
      gitIn(bare, ["fetch", "-q", "--no-tags", "--no-write-fetch-head", `file://${clone}`, `+refs/heads/racer-${i}:refs/uzi-test/racer-${i}`]);
    }
    const results = await Promise.all(shas.map((sha) => cache.updateTrackingRef(bare, BRANCH, sha, opts)));
    assert.equal(results.filter((r) => r.kind === "updated").length >= 1, true);
    assert.ok(results.some((r) => r.kind === "not_updated" && r.reason === "owed_limit"), "an attempt past the cap is refused");
    assert.equal((await pinned()).length, CAP, "exactly the cap, never more");
  });
  it("the shipped cap is 64, enforced by a default cache", async () => {
    assert.equal(MAX_OWED_CANDIDATES_PER_RUN, 64);
    // 64 ref-only pins stand in for 64 retained heads (the cap counts refs under the run's prefix).
    for (let i = 0; i < MAX_OWED_CANDIDATES_PER_RUN; i++) {
      gitIn(bare, ["update-ref", `refs/uzi-owed/${RUN}/${String(i).padStart(40, "a")}`, base]);
    }
    commitTree("one too many", true);
    const result = await realCap.fetchAgentBranch(bare, clone, BRANCH, RUN, opts);
    assert.deepEqual(result, { kind: "not_updated", reason: "owed_limit" });
    assert.equal(gitIn(bare, ["for-each-ref", `refs/uzi-owed/${RUN}/`]).split("\n").length, MAX_OWED_CANDIDATES_PER_RUN);
  });
});
