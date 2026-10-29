import { beforeEach, afterEach, test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { GitCache, RECOVERY_BUNDLE_REF, type PositiveOwedCandidateContext } from "../src/git.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { nullLogger, noProofReseed, testGitCacheOptions } from "./helpers.js";
import { fixtureFetchTracking, fixtureTrackingOptions } from "./runner-tracking-fixture.js";

const branch = "agent/issue-2512", runId = "run-2512";
const identity = ["-c", "user.name=fixture", "-c", "user.email=fixture@example.test", "-c", "commit.gpgsign=false"];
let fx: Fixture, cache: GitCache, bare: string, clone: string, head: string, context: PositiveOwedCandidateContext;
function git(dir: string, ...args: string[]): string {
  return execFileSync("git", ["--no-replace-objects", "-C", dir, ...args], {
    encoding: "utf8", stdio: "pipe",
    env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" },
  }).trim();
}
beforeEach(async () => {
  fx = makeFixture();
  cache = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
  bare = await cache.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700); fs.chmodSync(bare, 0o700);
  clone = (await cache.createOrAttachRunnerClone(bare, 2512, noProofReseed, runId)).path;
  fs.writeFileSync(path.join(clone, "RETAINED.txt"), "work before outage\n");
  git(clone, "add", "RETAINED.txt");
  git(clone, ...identity, "commit", "-m", "retained work");
  await fixtureFetchTracking(cache, bare, clone, branch, runId);
  head = git(clone, "rev-parse", "HEAD");
  context = (await fixtureTrackingOptions(cache, bare, branch, runId)).context as PositiveOwedCandidateContext;
});
afterEach(() => fx.cleanup());
const reuse = { reuseCoveredCurrent: "if-covered" as const };

test("repeated marker-free coverage reuse keeps its source and roots instead of nesting wrappers", async () => {
  const original = await cache.buildRecoveryCoverage(bare, context, [head], head);
  let current = original.sha;
  for (let generation = 2; generation <= 12; generation++) {
    const result = await cache.buildRecoveryCoverage(bare, { ...context, generation }, [head, current], current, reuse);
    assert.equal(result.sha, original.sha, "an already-covered clean aggregate is not wrapped again");
    assert.equal(git(bare, "rev-parse", result.coverageRef), original.sha);
    assert.equal(git(bare, "show", result.sha + ":RETAINED.txt"), "work before outage");
    git(bare, "merge-base", "--is-ancestor", head, result.sha);
    current = result.sha;
  }
  assert.equal(git(bare, "log", "--format=%s", current).split("\n").filter(s => s.startsWith("uzi recovery coverage ")).length, 1);
});

test("reuse re-proves without any coverage pins and keeps legacy reconstruction byte-identical", async () => {
  const original = await cache.buildRecoveryCoverage(bare, context, [head], head);
  const next = { ...context, generation: 2 };
  const legacy = await cache.buildRecoveryCoverage(bare, next, [head], original.sha);
  const result = await cache.buildRecoveryCoverage(bare, { ...next, generation: 3 }, [head], original.sha, reuse);
  assert.equal(result.sha, original.sha);
  assert.notEqual(result.sha, legacy.sha);
  for (const ref of git(bare, "for-each-ref", "--format=%(refname)", "refs/uzi-coverage/").split("\n"))
    git(bare, "update-ref", "-d", ref);
  assert.deepEqual(await cache.buildRecoveryCoverage(bare, { ...next, generation: 3 }, [head], original.sha, reuse), result);
  assert.deepEqual(await cache.buildRecoveryCoverage(bare, next, [head], original.sha), legacy);
});

test("a reused source has no optional redundancy claim, and its self-contained bundle imports all roots", async () => {
  const original = await cache.buildRecoveryCoverage(bare, context, [head], head);
  const result = await cache.buildRecoveryCoverage(bare, { ...context, generation: 2 }, [head], original.sha, reuse);
  assert.equal(result.sha, original.sha);
  const opts = { sourceSha: result.sha, fingerprint: result.fingerprint, roots: result.originalShas, currentSha: original.sha };
  assert.equal(await cache.readRecoveryCoverageClaim(bare, opts), undefined);
  const bundle = await cache.produceRecoveryBundle(bare, { sourceSha: result.sha, outPath: path.join(fx.dataDir, "reuse.bundle") });
  assert.equal(bundle.selfContained, true);
  const fresh = path.join(fx.dataDir, "fresh");
  git(fx.dataDir, "init", fresh);
  git(fresh, "fetch", bundle.bundlePath, RECOVERY_BUNDLE_REF + ":refs/heads/recovered");
  assert.equal(fs.existsSync(path.join(fresh, ".git", "objects", "info", "alternates")), false);
  git(fresh, "merge-base", "--is-ancestor", head, "recovered");
  assert.equal(git(fresh, "rev-parse", "recovered^{tree}"), git(bare, "rev-parse", head + "^{tree}"));
  assert.equal(git(fresh, "show", "recovered:RETAINED.txt"), "work before outage");
  assert.equal(await cache.readRecoveryCoverageClaim(fresh, opts), undefined);
});

test("equal trees do not authorize reuse across a divergent owed root", async () => {
  const original = await cache.buildRecoveryCoverage(bare, context, [head], head);
  const sibling = git(bare, ...identity, "commit-tree", head + "^{tree}", "-p", head + "^", "-m", "divergent retained work");
  const result = await cache.buildRecoveryCoverage(bare, context, [head, sibling], original.sha, reuse);
  assert.notEqual(result.sha, original.sha);
  for (const root of [head, sibling, original.sha]) git(bare, "merge-base", "--is-ancestor", root, result.sha);
});

test("marker-involved coverage retains legacy wrapping even when every root is covered", async () => {
  fs.writeFileSync(path.join(clone, "DIRTY.txt"), "uncommitted work\n");
  assert.equal(await cache.commitWipMarker(clone), true);
  await fixtureFetchTracking(cache, bare, clone, branch, runId);
  const marker = git(clone, "rev-parse", "HEAD");
  const original = await cache.buildRecoveryCoverage(bare, context, [marker], marker);
  const legacy = await cache.buildRecoveryCoverage(bare, context, [marker], original.sha);
  const result = await cache.buildRecoveryCoverage(bare, context, [marker], original.sha, reuse);
  assert.deepEqual(result, legacy);
  assert.notEqual(result.sha, original.sha);
});

test("unknown root ancestry cannot authorize current-source reuse", async t => {
  const original = await cache.buildRecoveryCoverage(bare, context, [head], head);
  const seam = cache as any;
  const ancestry = seam.ancestry.bind(cache);
  t.mock.method(seam, "ancestry", async (dir: string, ancestor: string, descendant: string) =>
    descendant === original.sha && ancestor === head ? "unknown" : ancestry(dir, ancestor, descendant));
  await assert.rejects(cache.buildRecoveryCoverage(bare, context, [head], original.sha, reuse), /coverage.*(proof|ancestry)/);
});

test("a physical source already over the wrapper bound is never reused", async () => {
  let current = head;
  for (let generation = 1; generation <= 9; generation++)
    current = (await cache.buildRecoveryCoverage(bare, { ...context, generation }, [head], current)).sha;
  const result = await cache.buildRecoveryCoverage(bare, { ...context, generation: 10 }, [head], current, reuse);
  assert.notEqual(result.sha, current);
  await assert.rejects((cache as any).resolveMarkerWrapperAdoption(bare, { runId }, result.sha, current), /successor recovery verification failed/);
});

test("generation cleanup preserves a later reused source and its adoption after restart", async () => {
  const first = await cache.buildRecoveryCoverage(bare, context, [head], head);
  const laterContext = { ...context, generation: 2 };
  await fixtureFetchTracking(cache, bare, clone, branch, runId, 2);
  const later = await cache.buildRecoveryCoverage(bare, laterContext, [head], first.sha, reuse);
  assert.equal(later.sha, first.sha);
  const frozen = await cache.enumerateOwedCandidates(bare, runId);
  assert.equal(await cache.cleanupRecoveryGeneration(bare, context, frozen,
    [{ fingerprint: first.fingerprint, sha: first.sha, coveredHeads: [head] }],
    [head, first.sha], first.sha), "removed", "another generation's coverage ref is not an orphan");
  assert.throws(() => git(bare, "rev-parse", "--verify", first.coverageRef));
  assert.equal(git(bare, "rev-parse", later.coverageRef), first.sha);
  const restarted = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
  assert.deepEqual(await restarted.buildRecoveryCoverage(bare, laterContext, [head], first.sha,
    { reuseCoveredCurrent: "required" }), later);
  commandFetch();
  await restarted.markRecoveryCapture(bare, clone, branch, runId);
  const source = { runId, clonePath: clone };
  const reservation = await restarted.reserveRecoveryIteration(bare, branch, "issue-2512", source, 1, 2, 0, 0);
  await restarted.recordRecoveryCapture(bare, branch, "issue-2512", source, reservation.attempts, later.sha, first.sha);
  const successor = await restarted.prepareRecoverySuccessor(bare, branch, "issue-2512", source,
    reservation.attempts, "20261029T123600Z-g2-0123456789abcdef");
  assert.equal(git(successor.path, "rev-parse", "HEAD"), first.sha);
  assert.equal(fs.readFileSync(path.join(successor.path, "RETAINED.txt"), "utf8"), "work before outage\n");
  function commandFetch() {
    git(clone, "fetch", bare, first.sha);
    git(clone, "reset", "--hard", first.sha);
  }
});

test("clean coverage-shaped forgery has the same adoption outcome with and without reuse", async () => {
  const tree = git(bare, "rev-parse", head + "^{tree}");
  const who = "uzi-agent <uzi-agent@uzi.local> 946684800 +0000";
  const body = `tree ${tree}\nparent ${head}\nauthor ${who}\ncommitter ${who}\n\nuzi recovery coverage ${"0".repeat(64)} round 0 batch 0\n`;
  const forged = execFileSync("git", ["-C", bare, "hash-object", "-t", "commit", "-w", "--stdin"],
    { encoding: "utf8", input: body, env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" } }).trim();
  const old = await cache.buildRecoveryCoverage(bare, context, [head], forged);
  const result = await cache.buildRecoveryCoverage(bare, { ...context, generation: 2 }, [head], forged, reuse);
  assert.equal(result.sha, forged, "shape recognition adds no claim of provenance");
  const resolver = (cache as any).resolveMarkerWrapperAdoption.bind(cache);
  assert.equal(await resolver(bare, { runId }, old.sha, forged), undefined);
  assert.equal(await resolver(bare, { runId }, result.sha, forged), undefined);
  git(bare, "merge-base", "--is-ancestor", head, result.sha);
});

test("required reuse refuses without overwriting its generation pin", async t => {
  const first = await cache.buildRecoveryCoverage(bare, context, [head], head);
  const later = { ...context, generation: 2 };
  const result = await cache.buildRecoveryCoverage(bare, later, [head], first.sha, reuse);
  const seam = cache as unknown as { reusableRecoveryCoverage(): Promise<boolean> };
  t.mock.method(seam, "reusableRecoveryCoverage", async () => false);
  await assert.rejects(cache.buildRecoveryCoverage(bare, later, [head], first.sha,
    { reuseCoveredCurrent: "required" }), /reuse proof unavailable/);
  assert.equal(git(bare, "rev-parse", result.coverageRef), result.sha);
});

for (const damage of ["message", "author", "committer", "date"] as const) {
  test(`coverage reuse refuses a mismatched ${damage} shape`, async () => {
    const tree = git(bare, "rev-parse", head + "^{tree}");
    const worker = "uzi-agent <uzi-agent@uzi.local>";
    const author = damage === "author" ? "other <other@example.test>" : worker;
    const committer = damage === "committer" ? "other <other@example.test>" : worker;
    const timestamp = damage === "date" ? 946771200 : 946684800;
    const subject = damage === "message" ? "uzi recovery coverage malformed" :
      `uzi recovery coverage ${"0".repeat(64)} round 0 batch 0`;
    const body = `tree ${tree}\nparent ${head}\nauthor ${author} ${timestamp} +0000\ncommitter ${committer} ${timestamp} +0000\n\n${subject}\n`;
    const candidate = execFileSync("git", ["-C", bare, "hash-object", "-t", "commit", "-w", "--stdin"], {
      encoding: "utf8", input: body, env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" },
    }).trim();
    const result = await cache.buildRecoveryCoverage(bare, context, [head], candidate, reuse);
    assert.notEqual(result.sha, candidate);
    git(bare, "merge-base", "--is-ancestor", candidate, result.sha);
  });
}

test("an unreadable reuse candidate creates no coverage ref", async t => {
  const first = await cache.buildRecoveryCoverage(bare, context, [head], head);
  const seam = cache as unknown as { readBoundedCommit(...args: unknown[]): Promise<unknown> };
  t.mock.method(seam, "readBoundedCommit", async () => { throw new Error("simulated object read failure"); });
  const next = { ...context, generation: 2 };
  await assert.rejects(cache.buildRecoveryCoverage(bare, next, [head], first.sha, reuse), /object read failure/);
  assert.equal(git(bare, "for-each-ref", "--format=%(refname)", `refs/uzi-coverage/${runId}/2/`), "");
});
