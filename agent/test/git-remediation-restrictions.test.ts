import { afterEach, beforeEach, test } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import { createHash } from "node:crypto";
import path from "node:path";
import { GitCache, type FetchAgentBranchOptions, type MemoryParkDurability } from "../src/git.js";
import { AuthorityRuntime } from "../src/authority-runtime.js";
import { acquireWorkerLifetime } from "../src/authority-lock.js";
import { testLockOptions } from "./authority-test-support.js";
import { formatAttemptId } from "../src/attempt-path.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { nullLogger, noProofReseed, testGitCacheOptions } from "./helpers.js";

const BRANCH = "agent/issue-3747", RUN = "run-3747", KEY = "issue-3747";
const ATTEMPT = formatAttemptId(new Date("2026-01-01T00:00:00Z"), 1, "0123456789abcdef");
const OTHER = formatAttemptId(new Date("2026-01-01T00:00:00Z"), 1, "fedcba9876543210");
const ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
const IDENT = ["-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"];
const ACTIVE = "uzi-recovery." + BRANCH + ".clone", PROTECTED = "uzi-retained." + RUN + ".journal";
let fx: Fixture, cache: GitCache, bare: string, clone: string, base: string, opts: FetchAgentBranchOptions;
const source = () => ({ runId: RUN, clonePath: clone, attemptId: ATTEMPT });
function git(dir: string, ...args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { env: ENV, encoding: "utf8", stdio: ["pipe", "pipe", "pipe"] }).trim();
}
const journal = () => JSON.parse(git(bare, "config", "--get", ACTIVE));
const write = (value: unknown) => git(bare, "config", ACTIVE, JSON.stringify(value));
const query = (durability: MemoryParkDurability = {}) =>
  cache.readRemediationRestrictionFacts(bare, BRANCH, RUN, source(), durability);
const held = (durability: MemoryParkDurability = {}) =>
  cache.readRemediationRestrictionFacts(bare, BRANCH, RUN, undefined, durability);
function commit(label: string, parent = base): string {
  const sha = git(clone, ...IDENT, "commit-tree", base + "^{tree}", "-p", parent, "-m", label);
  git(clone, "update-ref", "refs/heads/" + BRANCH, sha);
  return sha;
}
async function fetch() {
  assert.equal((await cache.fetchAgentBranch(bare, clone, BRANCH, RUN, opts)).kind, "updated");
}
async function enroll() {
  await cache.recordRemediationScan(bare, BRANCH, source(),
    { trusted: true, tip: git(clone, "rev-parse", "HEAD"), flaggedOIDs: [git(clone, "rev-parse", "HEAD")], origin: "pre-exit" });
}
async function rewritten() {
  const q = commit("Q"); await fetch(); await enroll();
  const d = commit("D", q); await fetch();
  const s = commit("S"); await fetch();
  return { q, d, s };
}
function snapshot() {
  return { config: fs.readFileSync(path.join(bare, "config"), "utf8"),
    refs: git(bare, "for-each-ref", "--format=%(refname) %(objectname)"),
    head: git(clone, "rev-parse", "HEAD") };
}
async function detach() {
  const current = git(clone, "rev-parse", "HEAD");
  const reservation = await cache.reserveRecoveryIteration(bare, BRANCH, KEY, source());
  await cache.recordRecoveryCapture(bare, BRANCH, KEY, source(), reservation.attempts, current, current);
  await cache.detachTerminalRetained(bare, BRANCH, KEY, journal());
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

// Real producer setup: ordinary private S is fetched/pinned before the generic worker
// bridge update. Q is imported into the bare only; execution HEAD remains S.
async function firstFindingBridge(unrelated = false, foreign = false) {
  const s = commit("private source S"); await fetch();
  const q = git(bare, ...IDENT, "commit-tree", base + "^{tree}", "-p", base, "-m", "sibling Q");
  const parents = unrelated ? ["-p", q] : ["-p", s, "-p", q];
  const b = git(bare, ...IDENT, "commit-tree", s + "^{tree}", ...parents, "-m", "worker bridge B");
  const c = git(bare, ...IDENT, "commit-tree", s + "^{tree}", "-p", b, "-m", "actual candidate C");
  const context = { ...opts.context, ...(foreign ? { runId: "foreign-run" } : {}) };
  assert.equal((await cache.updateTrackingRef(bare, BRANCH, b, { context })).kind, "updated");
  assert.equal(git(clone, "rev-parse", "HEAD"), s);
  assert.equal(await cache.worktreeHead(clone), s, "worker observes the exact source S");
  assert.equal((await cache.committedTrackingOwnership(bare, BRANCH, RUN)).kind,
    foreign ? "not_owned" : "owned", "the tracking receipt is independently committed");
  assert.ok((await cache.enumerateOwedCandidates(bare, RUN)).some(candidate => candidate.sha === s), "source S has its exact owed anchor");
  assert.equal(await cache.ancestry(bare, s, b), unrelated ? "divergent" : "ancestor");
  assert.equal(journal().remediationRecovery, undefined);
  return { s, q, b, c };
}
function owedFiles() {
  const dir = path.join(bare, "uzi-owed");
  return Object.fromEntries(fs.readdirSync(dir).sort().map(name => [name, fs.readFileSync(path.join(dir, name), "utf8")]));
}
const receiptFile = () => path.join(bare, "uzi-owed",
  "receipt-" + createHash("sha256").update(BRANCH).digest("hex") + ".json");

for (const candidate of ["B", "C"] as const) {
  test("first negative finding on " + candidate + " enrolls independently pinned execution S only", async () => {
    const { s, q, b, c } = await firstFindingBridge();
    assert.equal(await cache.ancestry(bare, q, s), "divergent");
    const before = snapshot(), metadata = owedFiles(), oldJournal = journal(), absent = await query();
    const calls: string[] = [];
    await cache.recordRemediationRestrictions(bare, BRANCH, source(),
      { trusted: true, tip: candidate === "B" ? b : c, flaggedOIDs: [q] },
      { sync: async (h, kind) => { calls.push(kind); await h.sync(); } });
    assert.deepEqual(calls, ["config", "directory"]);
    const r = { version: 1, ...source(), branch: BRANCH, sequence: 0, ownedTip: s,
      flaggedOIDs: [q], excludedRoots: [], edges: [], blocked: false };
    assert.deepEqual(journal(), { ...oldJournal, remediationRecovery: r });
    assert.equal(snapshot().refs, before.refs); assert.equal(snapshot().head, before.head);
    assert.deepEqual(owedFiles(), metadata);
    assert.equal(JSON.parse(fs.readFileSync(receiptFile(), "utf8")).attemptId, undefined);
    const fresh = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
    assert.deepEqual(await fresh.readRuntimeRemediationRecovery(bare, BRANCH, source()), r);
    assert.equal(await fresh.readRemediationRecovery(bare, BRANCH, source()), undefined);
    const facts = await query();
    assert.deepEqual(facts.flaggedOIDs, [q]); assert.deepEqual(facts.roots, [q]);
    assert.notEqual(facts.fingerprint, absent.fingerprint);
    assert.deepEqual(Object.keys(facts).sort(), ["blocked", "enrolled", "fingerprint", "flaggedOIDs", "roots"]);
    const stable = snapshot();
    for (const tip of [b, c]) await assert.rejects(cache.recordRemediationScan(bare, BRANCH, source(),
      { trusted: true, tip, flaggedOIDs: [q], origin: "pre-exit" }), /scanned tip not owned/);
    await assert.rejects(cache.readRemediationRestrictionFacts(bare, BRANCH, RUN, { ...source(), attemptId: OTHER }));
    assert.deepEqual(snapshot(), stable);
  });
}

for (const fault of ["missing source ref", "changed source ref", "missing source metadata",
  "changed source metadata", "foreign owner", "missing receipt", "invalid receipt", "unknown ancestry", "unrelated tracking"] as const) {
  test("first negative enrollment holds without repair on " + fault, async t => {
    const { s, q, b } = await firstFindingBridge(fault === "unrelated tracking", fault === "foreign owner");
    const ref = "refs/uzi-owed/" + RUN + "/" + s;
    if (fault === "missing source ref") git(bare, "update-ref", "-d", ref);
    if (fault === "changed source ref") git(bare, "update-ref", ref, base);
    if (fault === "missing source metadata" || fault === "changed source metadata") {
      const dir = path.join(bare, "uzi-owed");
      const name = fs.readdirSync(dir).find(name => {
        const value = JSON.parse(fs.readFileSync(path.join(dir, name), "utf8"));
        return value.sha === s;
      });
      assert.ok(name, "the real fetch wrote source metadata");
      if (fault === "missing source metadata") fs.unlinkSync(path.join(dir, name));
      else fs.writeFileSync(path.join(dir, name), "{}");
    }
    if (fault === "missing receipt") fs.unlinkSync(receiptFile());
    if (fault === "invalid receipt") {
      const receipt = JSON.parse(fs.readFileSync(receiptFile(), "utf8"));
      fs.writeFileSync(receiptFile(), JSON.stringify({ ...receipt, trackingSha: s }));
    }
    if (fault === "unknown ancestry") {
      const ancestry = cache.ancestry.bind(cache);
      t.mock.method(cache, "ancestry", async (dir: string, from: string, to: string) =>
        from === s && to === b ? "unknown" : ancestry(dir, from, to));
    }
    const before = snapshot(), metadata = owedFiles();
    await assert.rejects(cache.recordRemediationRestrictions(bare, BRANCH, source(),
      { trusted: true, tip: b, flaggedOIDs: [q] }));
    assert.equal(journal().remediationRecovery, undefined);
    assert.deepEqual(snapshot(), before); assert.deepEqual(owedFiles(), metadata);
  });
}

test("exact tracking S needs no extra owed pin; empty and untrusted scans do not enroll", async () => {
  const s = commit("exact S"); await fetch();
  git(bare, "update-ref", "-d", "refs/uzi-owed/" + RUN + "/" + s);
  const before = snapshot();
  await cache.recordRemediationRestrictions(bare, BRANCH, source(), { trusted: true, tip: s, flaggedOIDs: [] });
  await cache.recordRemediationRestrictions(bare, BRANCH, source(), { trusted: false, tip: s, flaggedOIDs: [s] });
  assert.deepEqual(snapshot(), before);
  await cache.recordRemediationRestrictions(bare, BRANCH, source(), { trusted: true, tip: s, flaggedOIDs: [s] });
  assert.equal(journal().remediationRecovery.ownedTip, s);
  assert.equal(journal().remediationRecovery.active, undefined);
  assert.equal(snapshot().refs, before.refs);
});

test("confirmed absence is synced every time, frozen and never enrolls or writes", async () => {
  const before = snapshot(), calls: string[] = [];
  for (let i = 0; i < 2; i++) {
    const facts = await query({ sync: async (h, kind) => { calls.push(kind); await h.sync(); } });
    assert.equal(facts.enrolled, false); assert.equal(facts.blocked, false);
    assert.deepEqual(facts.flaggedOIDs, []); assert.deepEqual(facts.roots, []);
    assert.match(facts.fingerprint, /^[0-9a-f]{64}$/);
    assert.ok(Object.isFrozen(facts) && Object.isFrozen(facts.roots) && Object.isFrozen(facts.flaggedOIDs));
    assert.deepEqual(Object.keys(facts).sort(), ["blocked", "enrolled", "fingerprint", "flaggedOIDs", "roots"]);
  }
  assert.deepEqual(calls, ["config", "directory", "config", "directory"]);
  assert.deepEqual(snapshot(), before);
  git(bare, "config", ACTIVE, "");
  const absent = snapshot();
  assert.equal((await held()).enrolled, false);
  assert.deepEqual(snapshot(), absent);
});

test("HEAD alignment and staged candidate Q preserve negative facts while strict runtime refuses", async () => {
  const { q, d } = await rewritten();
  const before = journal().remediationRecovery;
  const first = await query();
  assert.deepEqual(first.flaggedOIDs, [q]);
  assert.deepEqual(first.roots, [q, d].sort());
  const candidateQ = git(bare, ...IDENT, "commit-tree", base + "^{tree}", "-p", base, "-m", "new Q");
  const b = git(bare, ...IDENT, "commit-tree", base + "^{tree}", "-p", candidateQ, "-m", "B");
  git(clone, "fetch", bare, b);
  git(clone, "update-ref", "refs/heads/" + BRANCH, b);
  await assert.rejects(cache.readRuntimeRemediationRecovery(bare, BRANCH, source()));
  assert.equal(await cache.readRemediationRecovery(bare, BRANCH, source()), undefined);
  assert.deepEqual(await query(), first);
  await cache.recordRemediationRestrictions(bare, BRANCH, source(), { trusted: true, tip: b, flaggedOIDs: [candidateQ] });
  const updated = journal().remediationRecovery, stable = snapshot(), facts = await query();
  assert.deepEqual(updated, { ...before, flaggedOIDs: [q, candidateQ] });
  assert.deepEqual(facts.flaggedOIDs, [q, candidateQ].sort());
  assert.notEqual(facts.fingerprint, first.fingerprint);
  assert.deepEqual(snapshot(), stable);
});

test("real authority A then B authenticates the exact live lease; held query does not bind", async () => {
  await enroll();
  const lifetime = await acquireWorkerLifetime(fx.dataDir, testLockOptions());
  const runtime = new AuthorityRuntime({ lifetime, workerToken: "unit-restrictions", timeoutMs: 5000 });
  cache = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({ authority: runtime.hooks, authorityRequired: true }));
  await assert.rejects(runtime.withTransaction(() => query()));
  assert.equal((await held()).enrolled, true);
  assert.equal(runtime.leaseFor(clone), undefined);
  const lease = await runtime.bindSource(clone);
  assert.equal((await runtime.withTransaction(() => query())).enrolled, true);
  assert.equal(runtime.leaseFor(clone), lease);
  await runtime.endSourceUse([clone]);
  await assert.rejects(runtime.withTransaction(() => query()));
  assert.equal((await held()).enrolled, true);
  assert.equal(runtime.leaseFor(clone), undefined);
});

test("foreign run, normalized path, missing identity and two real same-generation attempts cannot substitute", async () => {
  await enroll();
  const saved = git(bare, "config", "--get", ACTIVE);
  git(bare, "config", ACTIVE, ""); // Manual coexistence fixture, matching the existing identity test.
  const sibling = await cache.runnerCloneForBranch(bare, BRANCH, KEY, noProofReseed, RUN, false, undefined,
    { attemptId: OTHER, isLive: p => p === clone, beforeSeed: async () => {} });
  git(bare, "config", ACTIVE, saved);
  const before = snapshot();
  for (const wrong of [
    { ...source(), runId: "other" }, { ...source(), attemptId: OTHER },
    { ...source(), clonePath: clone + "/." }, { ...source(), attemptId: undefined } as any,
    { runId: RUN, clonePath: sibling.path, attemptId: OTHER },
  ]) await assert.rejects(cache.readRemediationRestrictionFacts(bare, BRANCH, RUN, wrong), /facts unavailable/);
  assert.equal((await held()).enrolled, true);
  assert.deepEqual(snapshot(), before);
});

test("real protected detach and reclaimed ledger retain negative evidence without adoption", async () => {
  const { q, d } = await rewritten();
  await detach();
  assert.equal(git(bare, "config", "--get", ACTIVE), "");
  assert.ok(git(bare, "config", "--get-all", "uzi-attempts." + BRANCH + ".entry").includes("reclaimed"));
  const before = snapshot(), facts = await held();
  assert.equal(facts.enrolled, true);
  assert.deepEqual(facts.flaggedOIDs, [q]); assert.deepEqual(facts.roots, [q, d].sort());
  await assert.rejects(query());
  assert.equal(await cache.readRemediationRecovery(bare, BRANCH, source()), undefined);
  assert.equal(await cache.readRuntimeRemediationRecovery(bare, BRANCH, source()), undefined);
  assert.deepEqual(snapshot(), before);
  const next = await cache.runnerCloneForBranch(bare, BRANCH, KEY, noProofReseed, "new-run", false, undefined,
    { attemptId: OTHER, isLive: () => false, beforeSeed: async () => {} });
  await cache.markRecoveryCapture(bare, next.path, BRANCH, "new-run", OTHER);
  assert.deepEqual(await held(), facts, "a new run on the branch cannot silence prior custody");
});

test("valid protected no-RR confirms absence; malformed, duplicate and wrong-branch custody holds", async () => {
  await detach();
  assert.equal((await held()).enrolled, false);
  const saved = git(bare, "config", "--get", PROTECTED);
  for (const value of ["{", JSON.stringify({ ...JSON.parse(saved), extra: true }),
    JSON.stringify({ ...JSON.parse(saved), branch: "agent/other" })]) {
    git(bare, "config", PROTECTED, value);
    await assert.rejects(held());
  }
  git(bare, "config", PROTECTED, saved);
  git(bare, "config", "--add", PROTECTED, saved);
  await assert.rejects(held());
});

test("negative-only manual coexisting snapshots union restrictions; positive custody conflict remains", async () => {
  const { q, d } = await rewritten();
  await detach();
  const protectedValue = JSON.parse(git(bare, "config", "--get", PROTECTED));
  const active = structuredClone(protectedValue.journal);
  const extra = "a".repeat(40);
  active.remediationRecovery.flaggedOIDs.push(extra);
  active.remediationRecovery.excludedRoots.push(base);
  write(active); // Negative-only fixture: no producer or grant authority claimed.
  const facts = await held();
  assert.deepEqual(facts.flaggedOIDs, [q, extra].sort());
  assert.deepEqual(facts.roots, [q, d, extra, base].sort());
  await assert.rejects(cache.terminalRetainedSnapshot(bare, BRANCH, KEY, source()), /conflicting active/);
  const before = facts.fingerprint;
  active.remediationRecovery.active = undefined;
  write(active);
  assert.notEqual((await held()).fingerprint, before, "whole grant body, not just roots, affects fingerprint");
});

test("blocked, legacy and prepared negative-only fixtures never upgrade positive authority", async () => {
  await rewritten();
  const saved = journal();
  for (const mutate of [
    (r: any) => { r.blocked = true; },
    (r: any) => { delete r.blocked; },
    (r: any) => { delete r.excludedRoots; },
    (r: any) => { r.edges[0].phase = "prepared"; },
  ]) {
    const value = structuredClone(saved); mutate(value.remediationRecovery); write(value);
    const before = snapshot(), facts = await query();
    assert.equal(facts.enrolled, true); assert.equal(facts.blocked, true);
    assert.deepEqual(facts.flaggedOIDs, saved.remediationRecovery.flaggedOIDs);
    await assert.rejects(cache.readRuntimeRemediationRecovery(bare, BRANCH, source()));
    assert.equal(await cache.readRemediationRecovery(bare, BRANCH, source()), undefined);
    assert.deepEqual(snapshot(), before);
  }
});

test("negative evidence survives missing positive pins; strict reader still refuses", async () => {
  await rewritten();
  const facts = await query();
  for (const ref of git(bare, "for-each-ref", "--format=%(refname)", "refs/uzi-owed/").split("\n"))
    if (ref) git(bare, "update-ref", "-d", ref);
  await assert.rejects(cache.readRuntimeRemediationRecovery(bare, BRANCH, source()));
  const before = snapshot();
  assert.deepEqual(await query(), facts);
  assert.deepEqual(snapshot(), before);
});

for (const fault of ["config", "directory", "readback", "active-change", "protected-change"] as const) {
  test("independent confirmation holds on " + fault, async () => {
    await enroll();
    if (fault === "protected-change") await detach();
    const before = snapshot();
    const durability: MemoryParkDurability = {
      sync: async (h, kind) => { if (kind === fault) throw Error("fault"); await h.sync(); },
      readback: async read => {
        if (fault === "readback") return "different";
        if (fault === "active-change") { const j = journal(); j.remediationRecovery.active.origin = "checkpoint-pending"; write(j); }
        if (fault === "protected-change") {
          const value = JSON.parse(git(bare, "config", "--get", PROTECTED));
          value.journal.remediationRecovery.active.origin = "checkpoint-pending";
          git(bare, "config", PROTECTED, JSON.stringify(value));
        }
        return read();
      },
    };
    await assert.rejects(fault === "protected-change" ? held(durability) : query(durability));
    if (fault !== "active-change" && fault !== "protected-change") assert.deepEqual(snapshot(), before);
  });
}

test("failed producer sync needs fresh consumer confirmation; old body does not confirm pending Q", async () => {
  await enroll();
  const old = journal();
  const q = git(bare, ...IDENT, "commit-tree", base + "^{tree}", "-p", base, "-m", "pending Q");
  const fail: MemoryParkDurability = { sync: async () => { throw Error("producer failed"); } };
  await assert.rejects(cache.recordRemediationRestrictions(bare, BRANCH, source(),
    { trusted: true, tip: q, flaggedOIDs: [q] }, fail));
  const calls: string[] = [];
  assert.ok((await query({ sync: async (h, k) => { calls.push(k); await h.sync(); } })).flaggedOIDs.includes(q));
  assert.deepEqual(calls, ["config", "directory"]);
  write(old); // Simulated old readable body; consumer must compare its pending Q separately.
  assert.ok(!(await query()).flaggedOIDs.includes(q));
});

test("coexisting negative-only union overflow holds without truncating either snapshot", async () => {
  await enroll(); await detach();
  const value = JSON.parse(git(bare, "config", "--get", PROTECTED));
  const active = structuredClone(value.journal);
  value.journal.remediationRecovery.flaggedOIDs = Array.from({ length: 64 }, (_, i) => (i + 1).toString(16).padStart(40, "0"));
  value.journal.remediationRecovery.active = undefined;
  active.remediationRecovery.flaggedOIDs = ["f".repeat(40)];
  active.remediationRecovery.active = undefined;
  git(bare, "config", PROTECTED, JSON.stringify(value));
  write(active); // Manual negative-only fixture, no grant or producer claim.
  const before = snapshot();
  await assert.rejects(held());
  assert.deepEqual(snapshot(), before);
});

test("memory park and completion fence coexist without journal, ledger or pin writes", async () => {
  await enroll();
  const original = await query();
  const uuid = "11111111-1111-4111-8111-111111111111";
  assert.equal(await cache.recordMemoryPark(bare, BRANCH, source(), { version: 1,
    binding: { runId: RUN, claimGeneration: 1, workerId: "worker", registerNonce: "nonce", memoryEpisode: 0 },
    ack: "sent_unconfirmed", origin: { flightId: uuid, attemptId: ATTEMPT, executorIncarnation: null,
      authorityIncarnation: uuid, sourceEpoch: uuid } }), "recorded");
  await cache.installCompletionSourceFence(bare, BRANCH, source(),
    { version: 1, sourceUseEpoch: uuid, sourceCoreDigest: "a".repeat(64) });
  const before = snapshot(), facts = await query();
  assert.equal(facts.enrolled, true);
  assert.deepEqual(facts.flaggedOIDs, original.flaggedOIDs);
  assert.notEqual(facts.fingerprint, original.fingerprint);
  assert.deepEqual(snapshot(), before);
});

test("strict parser bounds and duplicate active entries hold without a partial result", async () => {
  await enroll();
  const saved = journal();
  for (const mutate of [
    (r: any) => { r.extra = true; }, (r: any) => { r.version = 2; },
    (r: any) => { r.clonePath += "/."; }, (r: any) => { r.branch = "agent/\u202eunsafe"; },
    (r: any) => { r.runId = "x".repeat(129); }, (r: any) => { r.attemptId = "x".repeat(257); },
    (r: any) => { r.branch = "agent/" + "é".repeat(510); },
    (r: any) => { r.clonePath = "/" + "é".repeat(2048); },
    (r: any) => { r.branch = "agent/control\\n"; },
    (r: any) => { r.flaggedOIDs = Array.from({ length: 65 }, (_, i) => i.toString(16).padStart(40, "0")); },
    (r: any) => { r.edges = Array(65).fill({}); },
    (r: any) => { r.excludedRoots = Array.from({ length: 1525 }, (_, i) => i.toString(16).padStart(40, "0")); },
    (r: any) => { r.excludedRoots = Array.from({ length: 1524 }, (_, i) => i.toString(16).padStart(40, "0")); },
  ]) {
    const j = structuredClone(saved); mutate(j.remediationRecovery); write(j);
    await assert.rejects(held());
  }
  write(saved);
  const j = journal();
  j.remediationRecovery.excludedRoots = Array.from({ length: 80 }, (_, i) => i.toString(16).padStart(40, "0"));
  write(j); // Negative-only roots do not claim object preservation.
  assert.equal((await query()).roots.length, 81, "there is no invented 64-root ceiling");
  git(bare, "config", "--add", ACTIVE, JSON.stringify(j));
  await assert.rejects(held());
});


test("duplicate valid large active and protected records retain the full per-record bound", async () => {
  await enroll();
  const j = journal();
  // Schema-only negative roots: no object-preservation or producer grant is claimed.
  j.remediationRecovery.excludedRoots = Array.from({ length: 900 }, (_, i) =>
    (i + 1).toString(16).padStart(40, "0"));
  const bytes = Buffer.byteLength(JSON.stringify(j.remediationRecovery));
  assert.ok(bytes > 32768 && bytes < 65536);
  write(j);
  await detach();
  const single = await held();
  const protectedValue = JSON.parse(git(bare, "config", "--get", PROTECTED));
  write(protectedValue.journal);
  const before = snapshot(), duplicated = await held();
  assert.equal(duplicated.enrolled, true);
  assert.deepEqual(duplicated.flaggedOIDs, single.flaggedOIDs);
  assert.deepEqual(duplicated.roots, single.roots);
  assert.equal(duplicated.blocked, single.blocked);
  assert.deepEqual(snapshot(), before);
});

test("query snapshots the worker-selected identity before awaited confirmation", async () => {
  await enroll();
  const expected = await query(), supplied = source();
  const facts = await cache.readRemediationRestrictionFacts(bare, BRANCH, RUN, supplied, {
    readback: async read => { supplied.attemptId = OTHER; return read(); },
  });
  assert.deepEqual(facts, expected, "the supplied object's mutation cannot redirect source authentication");
  await assert.rejects(cache.readRemediationRestrictionFacts(bare, BRANCH, RUN, supplied));
});
