import { afterEach, beforeEach, test } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import {
  GitCache, retainedAdoptionRefusal, annotateRetainedSuccessorError, type FetchAgentBranchOptions,
  type PositiveOwedCandidateContext,
} from "../src/git.js";
import { formatAttemptId } from "../src/attempt-path.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { nullLogger, noProofReseed, testGitCacheOptions } from "./helpers.js";

// issue #4177 m2: the closed-set classification of a retained adoption refusal, and the read-only crash
// rediscovery of it from a blocked journal. Real Git, real bare config.

const BRANCH = "agent/issue-4177";
const RUN = "run-4177";
const KEY = "issue-4177";
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
const raw = () => git(bare, "config", "--get", "uzi-recovery." + BRANCH + ".clone");
async function enroll(q: string) {
  await cache.recordRemediationScan(bare, BRANCH, source(), { trusted: true, tip: q, flaggedOIDs: [q], origin: "pre-exit" });
}
async function capture(roots: string[], current: string) {
  const coverage = await cache.buildRecoveryCoverage(bare, opts.context as PositiveOwedCandidateContext, roots, current);
  const reservation = await cache.reserveRecoveryIteration(bare, BRANCH, KEY, source());
  await cache.recordRecoveryCapture(bare, BRANCH, KEY, source(), reservation.attempts, coverage.sha, current);
  return reservation;
}
async function refusal(options: Parameters<GitCache["prepareRecoverySuccessor"]>[6], iteration: number): Promise<Error> {
  try {
    await cache.prepareRecoverySuccessor(bare, BRANCH, KEY, source(), iteration, SUCCESSOR, options);
  } catch (error) { return error as Error; }
  throw new Error("adoption unexpectedly succeeded");
}
const admit = { admitRemediationSource: async () => true };

/** The flagged commit Q stays an ancestor of the owned source D: active remediation. */
async function activeScene() {
  const q = commit("flagged"); await fetch(); await enroll(q);
  const d = commit("progress on the flagged tip", q); await fetch();
  const reservation = await capture([q, d], d);
  return { q, d, reservation };
}
/** Q displaced by an authorized replacement R; the source S descends from R, so the restricted roots are divergent. */
async function rewrittenScene() {
  const q = commit("flagged"); await fetch(); await enroll(q);
  const replacement = commit("authorized replacement"); await fetch();
  const s = commit("progress after the rewrite", replacement); await fetch();
  const reservation = await capture([q, s], s);
  return { q, replacement, s, reservation };
}

beforeEach(async () => {
  fx = makeFixture();
  cache = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
  bare = await cache.ensureClone(fx.originPath);
  base = git(bare, "rev-parse", "refs/remotes/origin/main");
  clone = (await cache.runnerCloneForBranch(bare, BRANCH, KEY, noProofReseed, RUN, false, undefined,
    { attemptId: ATTEMPT, isLive: () => false, beforeSeed: async () => {} })).path;
  await cache.markRecoveryCapture(bare, clone, BRANCH, RUN, ATTEMPT);
  opts = { context: { runId: RUN, generation: 1, kind: "issue", branch: BRANCH, barePath: bare,
    defaultIdentity: { ref: "refs/remotes/origin/main", sha: base } }, producingSource: source() };
  await fetch();
});
afterEach(() => fx.cleanup());

test("retainedAdoptionRefusal maps only the two remediation reasons", () => {
  assert.equal(retainedAdoptionRefusal(annotateRetainedSuccessorError(new Error("x"), "remediation_active")), "secret_remediation_active");
  assert.equal(retainedAdoptionRefusal(annotateRetainedSuccessorError(new Error("x"), "remediation_proof_unknown")), "remediation_proof_unknown");
  assert.equal(retainedAdoptionRefusal(annotateRetainedSuccessorError(new Error("x"), "seed_failed")), undefined);
  assert.equal(retainedAdoptionRefusal(new Error("remediation adoption source still restricted")), undefined);
  assert.equal(retainedAdoptionRefusal("remediation_active"), undefined);
});

test("a flagged ancestor of the owned source refuses with the same message, classified active", async () => {
  const { reservation } = await activeScene();
  const error = await refusal(admit, reservation.attempts);
  assert.equal(error.message, "remediation adoption source still restricted");
  assert.equal(retainedAdoptionRefusal(error), "secret_remediation_active");
});

test("an unknown ancestry probe with no ancestor found is classified proof unknown", async t => {
  const { q, d, reservation } = await activeScene();
  const real = cache.ancestry.bind(cache);
  t.mock.method(cache, "ancestry", async (b: string, floor: string, tip: string) =>
    floor === q && tip === d ? "unknown" : real(b, floor, tip));
  const error = await refusal(admit, reservation.attempts);
  assert.equal(error.message, "remediation adoption source still restricted");
  assert.equal(retainedAdoptionRefusal(error), "remediation_proof_unknown");
});

test("an ancestor found after an unknown probe still classifies active", async t => {
  const q1 = commit("first flagged"); await fetch();
  const q2 = commit("second flagged", q1); await fetch();
  await cache.recordRemediationScan(bare, BRANCH, source(), { trusted: true, tip: q2, flaggedOIDs: [q1, q2], origin: "pre-exit" });
  const d = commit("progress on the flagged tip", q2); await fetch();
  const reservation = await capture([q1, q2, d], d);
  const real = cache.ancestry.bind(cache);
  // The first restricted-root probe is unknown; the next root of the same record is a real ancestor.
  t.mock.method(cache, "ancestry", async (b: string, floor: string, tip: string) =>
    floor === q1 && tip === d ? "unknown" : real(b, floor, tip));
  const error = await refusal(admit, reservation.attempts);
  assert.equal(retainedAdoptionRefusal(error), "secret_remediation_active");
});

test("an interrupted operation yields no class even when a probe answered unknown", async t => {
  const { q, d, reservation } = await activeScene();
  const real = cache.ancestry.bind(cache);
  const controller = new AbortController();
  t.mock.method(cache, "ancestry", async (b: string, floor: string, tip: string) => {
    if (floor === q && tip === d) { controller.abort(new Error("operation aborted")); return "unknown"; }
    return real(b, floor, tip);
  });
  let caught: Error | undefined;
  await assert.rejects(cache.withRecoveryOperation(controller.signal, Date.now() + 60_000, async () => {
    try { await cache.prepareRecoverySuccessor(bare, BRANCH, KEY, source(), reservation.attempts, SUCCESSOR, admit); }
    catch (error) { caught = error as Error; throw error; }
  }));
  assert.ok(caught);
  assert.equal(retainedAdoptionRefusal(caught), undefined);
});

test("an unknown replacement-ancestry proof is classified proof unknown", async t => {
  const { replacement, s, reservation } = await rewrittenScene();
  const real = cache.ancestry.bind(cache);
  t.mock.method(cache, "ancestry", async (b: string, floor: string, tip: string) =>
    floor === replacement && tip === s ? "unknown" : real(b, floor, tip));
  const error = await refusal(admit, reservation.attempts);
  assert.equal(error.message, "remediation replacement ancestry unknown");
  assert.equal(retainedAdoptionRefusal(error), "remediation_proof_unknown");
});

test("the git refusals that are not remediation classes stay unclassified", async () => {
  const { reservation } = await rewrittenScene();
  const error = await refusal({ admitRemediationSource: async () => false }, reservation.attempts);
  assert.equal(error.message, "remediation continuation scan admission required");
  assert.equal(retainedAdoptionRefusal(error), undefined);
});

test("crash rediscovery classifies a blocked adoption_failed journal and writes nothing", async () => {
  const { reservation } = await activeScene();
  await cache.blockRecoveryEpisode(bare, BRANCH, KEY, source(), "adoption_failed", reservation);
  const journalBefore = raw();
  const configBefore = git(bare, "config", "--local", "--list");
  const refsBefore = git(bare, "for-each-ref");
  assert.equal(await cache.classifyBlockedRemediationAdoption(bare, BRANCH, KEY, source()), "secret_remediation_active");
  assert.equal(raw(), journalBefore, "journal bytes unchanged");
  assert.equal(git(bare, "config", "--local", "--list"), configBefore, "no ledger, pin or journal content written");
  assert.equal(git(bare, "for-each-ref"), refsBefore);
});

test("crash rediscovery classifies an unknown proof and never a clean source", async t => {
  const { q, d, reservation } = await activeScene();
  await cache.blockRecoveryEpisode(bare, BRANCH, KEY, source(), "adoption_failed", reservation);
  const real = cache.ancestry.bind(cache);
  const probe = t.mock.method(cache, "ancestry", async (b: string, floor: string, tip: string) =>
    floor === q && tip === d ? "unknown" : real(b, floor, tip));
  assert.equal(await cache.classifyBlockedRemediationAdoption(bare, BRANCH, KEY, source()), "remediation_proof_unknown");
  probe.mock.restore();
});

test("crash rediscovery gives no class for a clean source, a non-adoption block or an unblocked journal", async () => {
  const clean = await rewrittenScene();
  assert.equal(await cache.classifyBlockedRemediationAdoption(bare, BRANCH, KEY, source()), undefined, "captured, not blocked");
  await cache.blockRecoveryEpisode(bare, BRANCH, KEY, source(), "adoption_failed", clean.reservation);
  assert.equal(await cache.classifyBlockedRemediationAdoption(bare, BRANCH, KEY, source()), undefined, "source is clean: adoption would proceed");
});

test("crash rediscovery gives no class for another blocker", async () => {
  const { reservation } = await activeScene();
  await cache.blockRecoveryEpisode(bare, BRANCH, KEY, source(), "budget_exhausted", reservation);
  assert.equal(await cache.classifyBlockedRemediationAdoption(bare, BRANCH, KEY, source()), undefined);
});

test("crash rediscovery gives no class for a changed identity or a missing record", async () => {
  const { reservation } = await activeScene();
  await cache.blockRecoveryEpisode(bare, BRANCH, KEY, source(), "adoption_failed", reservation);
  assert.equal(await cache.classifyBlockedRemediationAdoption(bare, BRANCH, KEY, { ...source(), runId: "other-run" }), undefined);
  assert.equal(await cache.classifyBlockedRemediationAdoption(bare, BRANCH, KEY, { ...source(), attemptId: SUCCESSOR }), undefined);
  assert.equal(await cache.classifyBlockedRemediationAdoption(bare, "agent/issue-1", KEY, source()), undefined);
  assert.equal(await cache.classifyBlockedRemediationAdoption(bare, BRANCH, KEY, { runId: RUN, clonePath: clone }), undefined);
});

test("crash rediscovery gives no class when the journal has no remediation record", async () => {
  const bareFixture = makeFixture();
  try {
    const other = new GitCache(bareFixture.dataDir, nullLogger(), undefined, testGitCacheOptions());
    const b = await other.ensureClone(bareFixture.originPath);
    const c = (await other.runnerCloneForBranch(b, "agent/issue-4178", "issue-4178", noProofReseed, "run-4178", false, undefined,
      { attemptId: ATTEMPT, isLive: () => false, beforeSeed: async () => {} })).path;
    await other.markRecoveryCapture(b, c, "agent/issue-4178", "run-4178", ATTEMPT);
    const src = { runId: "run-4178", clonePath: c, attemptId: ATTEMPT };
    const reservation = await other.reserveRecoveryIteration(b, "agent/issue-4178", "issue-4178", src);
    await other.blockRecoveryEpisode(b, "agent/issue-4178", "issue-4178", src, "adoption_failed", reservation);
    assert.equal(await other.classifyBlockedRemediationAdoption(b, "agent/issue-4178", "issue-4178", src), undefined);
  } finally { bareFixture.cleanup(); }
});
