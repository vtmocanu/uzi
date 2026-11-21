import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { GitCache, type FetchAgentBranchOptions, type PositiveOwedCandidateContext } from "../src/git.js";
import { formatAttemptId } from "../src/attempt-path.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { nullLogger, noProofReseed, testGitCacheOptions } from "./helpers.js";

// issue #4481: P5 (updateRemediationTrackingRef) and P6 (remediation marker peel) honour the seal and the fence,
// and a producing-source import re-checks the fence in its second lock section.
const BRANCH = "agent/issue-3747";
const RUN = "run-3747";
const KEY = "issue-3747";
const ATTEMPT = formatAttemptId(new Date("2026-01-01T00:00:00Z"), 1, "0123456789abcdef");
const SUCCESSOR = formatAttemptId(new Date("2026-01-01T00:00:00Z"), 2, "fedcba9876543210");
const ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
const IDENT = ["-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"];
let fx: Fixture;
let cache: GitCache;
let bare: string;
let clone: string;
let base: string;
let opts: FetchAgentBranchOptions;
let ctx: PositiveOwedCandidateContext;
const source = () => ({ runId: RUN, clonePath: clone, attemptId: ATTEMPT });
function git(dir: string, ...args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { env: ENV, encoding: "utf8", stdio: ["pipe", "pipe", "pipe"] }).trim();
}
function commit(label: string, parent = base): string {
  const sha = git(clone, ...IDENT, "commit-tree", base + "^{tree}", "-p", parent, "-m", label);
  git(clone, "update-ref", "refs/heads/" + BRANCH, sha);
  return sha;
}
async function fetch() {
  const result = await cache.fetchAgentBranch(bare, clone, BRANCH, RUN, opts);
  assert.equal(result.kind, "updated", JSON.stringify(result));
}
async function enroll() {
  await cache.recordRemediationScan(bare, BRANCH, source(),
    { trusted: true, tip: git(clone, "rev-parse", "HEAD"), flaggedOIDs: [git(clone, "rev-parse", "HEAD")], origin: "pre-exit" });
}
const pins = (): string[] => git(bare, "for-each-ref", "--format=%(refname)", `refs/uzi-owed/${RUN}/`).split("\n").filter(Boolean);
const incoming = (): string[] => git(bare, "for-each-ref", "--format=%(refname) %(objectname)", "refs/uzi-incoming/").split("\n").filter(Boolean);
const refDir = (): string => path.join(bare, "info", "uzi-refusals");
const ledger = (): any => {
  const file = path.join(refDir(), `refusals-${RUN}.json`);
  return fs.existsSync(file) ? JSON.parse(fs.readFileSync(file, "utf8")) : undefined;
};
const delay = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms));
/** A cache whose cap equals the current pin count: every new pin is refused. */
const capped = (): GitCache => new GitCache(fx.dataDir, nullLogger(), undefined,
  testGitCacheOptions({ maxOwedCandidates: pins().length }));
const cover = (...parents: string[]): string => git(bare, ...IDENT, "commit-tree", base + "^{tree}", ...parents.flatMap((p) => ["-p", p]), "-m", "cover");
/** Hold the bare lock, start `call`, close the generation while it waits, then release. */
async function closeWhileWaiting<T>(call: () => Promise<T>): Promise<T> {
  let release!: () => void;
  const held = cache["withLock"](bare, () => new Promise<void>((resolve) => { release = resolve; }));
  const pending = call();
  await delay(600);
  cache.closeRefusalGeneration(RUN, 1);
  release();
  await held;
  return pending;
}

beforeEach(async () => {
  fx = makeFixture();
  cache = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
  bare = await cache.ensureClone(fx.originPath);
  base = git(bare, "rev-parse", "refs/remotes/origin/main");
  clone = (await cache.runnerCloneForBranch(bare, BRANCH, KEY, noProofReseed, RUN, false, undefined,
    { attemptId: ATTEMPT, isLive: () => false, beforeSeed: async () => {} })).path;
  await cache.markRecoveryCapture(bare, clone, BRANCH, RUN, ATTEMPT);
  ctx = { runId: RUN, generation: 1, kind: "issue", branch: BRANCH, barePath: bare,
    defaultIdentity: { ref: "refs/remotes/origin/main", sha: base } };
  opts = { context: ctx, producingSource: source() };
  await fetch();
});
afterEach(() => fx.cleanup());

/** Real worker transformation rig: S is the owned tip, B a bridge over S. S's pin is removed so P5 pins it again. */
async function transformation() {
  const displaced = commit("displaced"); await fetch(); await enroll();
  const s = commit("authorized replacement"); await fetch();
  await cache.recordRemediationScan(bare, BRANCH, source(), { trusted: true, tip: s, flaggedOIDs: [] });
  const floor = git(bare, ...IDENT, "commit-tree", base + "^{tree}", "-m", "safe unrelated floor");
  const bridge = await cache.bridgeToFloors(bare, s, [floor]);
  if (bridge.kind !== "built") throw new Error("bridge not built");
  git(bare, "update-ref", "-d", `refs/uzi-owed/${RUN}/${s}`);
  return { displaced, s, b: bridge.sha };
}
const admission = { trusted: true, admit: async () => true };
const callP5 = (c: GitCache, b: string, s: string) =>
  c.updateRemediationTrackingRef(bare, BRANCH, b, { context: ctx }, source(), s, admission);

/** Real marker rig: the peel pins the marker's execution parent, which has no pin yet. */
async function marker() {
  const displaced = commit("displaced"); await fetch(); await enroll();
  const replacement = commit("authorized replacement"); await fetch();
  const parent = commit("ordinary progress before source marker", replacement);
  git(clone, "reset", "--hard", parent);
  fs.writeFileSync(path.join(clone, "source-marker.txt"), "recovered staged content\n");
  git(clone, "add", "source-marker.txt");
  assert.equal(await cache.commitWipMarker(clone), true);
  const m = git(clone, "rev-parse", "HEAD");
  await fetch();
  const r = await cache.reserveRecoveryIteration(bare, BRANCH, KEY, source());
  await cache.recordRecoveryCapture(bare, BRANCH, KEY, source(), r.attempts, m, m);
  assert.equal(pins().some((p) => p.endsWith(parent)), false);
  return { displaced, parent, marker: m, iteration: r.attempts };
}
const callP6 = (c: GitCache, iteration: number) =>
  c.prepareRecoverySuccessor(bare, BRANCH, KEY, source(), iteration, SUCCESSOR, { admitRemediationSource: async () => true });

describe("issue4481 remediation paths", { timeout: 300_000 }, () => {
  it("P5 records its refusal", async () => {
    const { s, b } = await transformation();
    const low = capped();
    await assert.rejects(callP5(low, b, s), /owed candidate limit/);
    assert.deepEqual(ledger().entries, [{ generation: 1, sha: s, path: "P5" }]);
    assert.ok(incoming().includes(`refs/uzi-incoming/${RUN}/1/refused-${s} ${s}`));
  });

  it("P5 after the seal: nothing is recorded, no refused ref is created", async () => {
    const { s, b } = await transformation();
    const low = capped();
    assert.equal(await low.admitRefusalProvenance(ctx), "written");
    assert.equal(await low.sealRefusalRelease(bare, RUN, 1, cover(s)), "sealed");
    await assert.rejects(callP5(low, b, s), /owed candidate limit/);
    assert.equal(ledger(), undefined);
    assert.deepEqual(incoming(), []);
  });

  it("P5 refuses a closed generation before any pin, and a close that lands while it waits for the lock", async () => {
    const { s, b } = await transformation();
    const raced = await closeWhileWaiting(() => callP5(cache, b, s));
    assert.deepEqual(raced, { kind: "not_updated", reason: "ownership_unknown" });
    assert.equal(git(bare, "rev-parse", "refs/uzi-runner/" + BRANCH), s);
    assert.equal(pins().includes(`refs/uzi-owed/${RUN}/${s}`), false, "the in-lock fence stops the pin");
    assert.deepEqual(await callP5(cache, b, s), { kind: "not_updated", reason: "ownership_unknown" });
    assert.equal(ledger(), undefined);
  });

  it("P6 records its refusal", async () => {
    const { parent, iteration } = await marker();
    await assert.rejects(callP6(capped(), iteration), /owed candidate limit/);
    assert.deepEqual(ledger().entries, [{ generation: 1, sha: parent, path: "P6" }]);
  });

  it("P6 after the seal: nothing is recorded", async () => {
    const { parent, marker: m, iteration } = await marker();
    const low = capped();
    assert.equal(await low.admitRefusalProvenance(ctx), "written");
    assert.equal(await low.sealRefusalRelease(bare, RUN, 1, cover(m, parent)), "sealed");
    await assert.rejects(callP6(low, iteration), /owed candidate limit/);
    assert.equal(ledger(), undefined);
    assert.deepEqual(incoming(), []);
  });

  it("P6 refuses a closed generation before pinning or recording", async () => {
    const { iteration } = await marker();
    const low = capped();
    const before = pins();
    low.closeRefusalGeneration(RUN, 1);
    await assert.rejects(callP6(low, iteration), /refusal generation closed/);
    assert.deepEqual(pins(), before);
    assert.equal(ledger(), undefined);
    assert.deepEqual(incoming(), []);
  });

  it("a producing-source import re-checks the fence after its authority phase and drops its stage", async () => {
    commit("next head");
    const before = pins();
    let phases = 0;
    const racing: FetchAgentBranchOptions = { ...opts,
      withSourceAuthority: async <T>(action: () => Promise<T>) => {
        // phase 1 authenticates, phase 2 promotes: the generation closes between acquisition and promotion.
        if (++phases === 2) cache.closeRefusalGeneration(RUN, 1);
        return action();
      } };
    assert.deepEqual(await cache.fetchAgentBranch(bare, clone, BRANCH, RUN, racing), { kind: "not_updated", reason: "ownership_unknown" });
    assert.equal(phases, 2);
    assert.deepEqual(pins(), before);
    assert.deepEqual(incoming(), [], "the private stage is dropped");
  });
});
