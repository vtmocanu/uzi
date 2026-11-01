import { afterEach, beforeEach, test } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { parseRecoveryJournal as oldParse } from "./fixtures/base-parse-recovery-journal.js";
import { createHash } from "node:crypto";
import { GitCache, type FetchAgentBranchOptions, type PositiveOwedCandidateContext, type RemediationRecovery } from "../src/git.js";
import { formatAttemptId } from "../src/attempt-path.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { nullLogger, noProofReseed, testGitCacheOptions } from "./helpers.js";

const BRANCH = "agent/issue-3747";
const RUN = "run-3747";
const KEY = "issue-3747";
const ATTEMPT = formatAttemptId(new Date("2026-01-01T00:00:00Z"), 1, "0123456789abcdef");
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
  return result;
}
async function enroll() {
  await cache.recordRemediationScan(bare, BRANCH, source(),
    { trusted: true, tip: git(clone, "rev-parse", "HEAD"), flaggedOIDs: [git(clone, "rev-parse", "HEAD")], origin: "pre-exit" });
}
const raw = () => git(bare, "config", "--get", "uzi-recovery." + BRANCH + ".clone");
const journal = () => JSON.parse(raw());
function write(value: unknown) {
  git(bare, "config", "uzi-recovery." + BRANCH + ".clone", JSON.stringify(value));
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

for (const origin of ["checkpoint-pending", "pre-exit"] as const) {
  test(origin + ": real import, later pending head, displacement and restart query", async () => {
    const q = commit("flagged");
    await fetch();
    await cache.recordRemediationScan(bare, BRANCH, source(), { trusted: true, tip: q, flaggedOIDs: [q], origin });
    const later = commit("later pending", q);
    await fetch();
    const replacement = commit("replacement");
    await fetch();
    const restarted = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
    const evidence = await restarted.readRemediationRecovery(bare, BRANCH, source());
    assert.ok(evidence);
    assert.equal(evidence.edges.length, 1);
    assert.deepEqual(evidence.edges[0], { from: later, to: replacement, phase: "committed",
      context: evidence.edges[0]!.context, grant: { id: 1, origin, originSource: source(), scannedTip: q, flaggedOIDs: [q] } });
    assert.ok((await cache.enumerateOwedCandidates(bare, RUN)).some(c => c.sha === later));
    await cache.recordRemediationScan(bare, BRANCH, source(), { trusted: true, tip: replacement, flaggedOIDs: [] });
    assert.equal(journal().remediationRecovery.active, undefined);
    const arbitrary = commit("later arbitrary");
    await fetch();
    assert.equal(journal().remediationRecovery.edges.length, 1);
    assert.equal((await cache.readRemediationRecovery(bare, BRANCH, source()))?.ownedTip, arbitrary);
    const reservation = await cache.reserveRecoveryIteration(bare, BRANCH, KEY, source());
    await cache.recordRecoveryCapture(bare, BRANCH, KEY, source(), reservation.attempts, arbitrary, arbitrary);
    await assert.rejects(cache.prepareRecoverySuccessor(bare, BRANCH, KEY, source(), reservation.attempts,
      formatAttemptId(new Date("2026-01-01T00:00:00Z"), 1, "fedcba9876543210")), /remediation/);
  });
}

test("untrusted scans and arbitrary same-tree siblings create no edge", async () => {
  const q = commit("q"); await fetch();
  await cache.recordRemediationScan(bare, BRANCH, source(), { trusted: false, tip: q, flaggedOIDs: [q], origin: "checkpoint-pending" });
  commit("sibling"); await fetch();
  assert.equal(journal().remediationRecovery, undefined);
});

test("empty scan cannot close a grant whose flagged commit is hidden below its floor", async () => {
  const q = commit("q"); await fetch();
  await cache.recordRemediationScan(bare, BRANCH, source(), { trusted: true, tip: q, flaggedOIDs: [q], origin: "checkpoint-pending" });
  const later = commit("later", q); await fetch();
  await cache.recordRemediationScan(bare, BRANCH, source(), { trusted: true, tip: later, flaggedOIDs: [] });
  assert.equal(journal().remediationRecovery.active.id, 1);
});

test("wrong run, same-generation other attempt, stale source and absent trusted import binding refuse", async () => {
  await enroll();
  assert.equal(await cache.readRemediationRecovery(bare, BRANCH, { ...source(), runId: "other" }), undefined);
  const attemptId = formatAttemptId(new Date("2026-01-01T00:00:00Z"), 1, "fedcba9876543210");
  assert.equal(await cache.readRemediationRecovery(bare, BRANCH, { ...source(), attemptId }), undefined);
  assert.equal(await cache.readRemediationRecovery(bare, BRANCH, { ...source(), clonePath: clone + "-stale" }), undefined);
  await assert.rejects(cache.fetchAgentBranch(bare, clone, BRANCH, RUN, { context: opts.context }), /producing source required/);
});

test("strict bounded schema rejects unknown keys, versions, identifiers, duplicates, cycles and overflow", async () => {
  await enroll();
  const saved = journal();
  const mutations = [
    (r: any) => { r.extra = true; },
    (r: any) => { r.version = 2; },
    ...[null, 0, "false", {}, []].map(blocked => (r: any) => { r.blocked = blocked; }),
    (r: any) => { r.runId = "x".repeat(129); },
    (r: any) => { r.branch = "x".repeat(1025); },
    (r: any) => { r.clonePath = "/" + "x".repeat(4096); },
    (r: any) => { r.attemptId = "x".repeat(257); },
    (r: any) => { r.branch = "agent/\u202eunsafe"; },
    (r: any) => { r.sequence = Number.MAX_SAFE_INTEGER + 1; },
    (r: any) => { r.flaggedOIDs = [base, base]; },
    (r: any) => { r.flaggedOIDs = Array.from({ length: 65 }, (_, i) => i.toString(16).padStart(40, "0")); },
    (r: any) => { r.edges = Array(65).fill({}); },
    (r: any) => { r.extra = "x".repeat(65536); },
  ];
  for (const mutate of mutations) {
    const next = structuredClone(saved); mutate(next.remediationRecovery); write(next);
    await assert.rejects(cache.markRecoveryCapture(bare, clone, BRANCH, RUN, ATTEMPT), /remediation/);
  }
  write(saved);
  await cache.markRecoveryCapture(bare, clone, BRANCH, RUN, ATTEMPT);
  assert.deepEqual(journal().remediationRecovery, saved.remediationRecovery);
});

test("consumer refuses prepared edge, missing pin, altered receipt, failed sync and failed readback", async () => {
  const q = commit("q"); await fetch();
  await cache.recordRemediationScan(bare, BRANCH, source(), { trusted: true, tip: q, flaggedOIDs: [q], origin: "pre-exit" });
  commit("rewrite"); await fetch();
  const saved = journal();
  const prepared = structuredClone(saved);
  prepared.remediationRecovery.edges[0].phase = "prepared"; write(prepared);
  assert.equal(await cache.readRemediationRecovery(bare, BRANCH, source()), undefined);
  write(saved);
  assert.equal(await cache.readRemediationRecovery(bare, BRANCH, source(), { sync: async () => { throw Error("sync"); } }), undefined);
  assert.equal(await cache.readRemediationRecovery(bare, BRANCH, source(), { readback: async () => "" }), undefined);
  const receipt = path.join(bare, "uzi-owed", "receipt-" + createHash("sha256").update(BRANCH).digest("hex") + ".json");
  const receiptRaw = fs.readFileSync(receipt, "utf8");
  fs.writeFileSync(receipt, receiptRaw.replace('"committed"', '"pending"'));
  assert.equal(await cache.readRemediationRecovery(bare, BRANCH, source()), undefined);
  fs.writeFileSync(receipt, receiptRaw);
  git(bare, "update-ref", "-d", "refs/uzi-owed/" + RUN + "/" + q, q);
  assert.equal(await cache.readRemediationRecovery(bare, BRANCH, source()), undefined);
});

test("frozen old parser accepts baseline, rejects only the new journal key", async () => {
  await enroll();
  const { remediationRecovery, ...baseline } = journal();
  assert.deepEqual(oldParse(JSON.stringify(baseline)), baseline);
  assert.throws(() => oldParse(JSON.stringify({ ...baseline, remediationRecovery })), /invalid/);
});

test("64 flagged OIDs and 64 edges round-trip; conflicts, cycles and 64KiB overflow refuse", async () => {
  await enroll();
  const saved = journal();
  const r = saved.remediationRecovery;
  const sha = (i: number) => i.toString(16).padStart(40, "0");
  delete r.active;
  r.sequence = 64;
  r.flaggedOIDs = Array.from({ length: 64 }, (_, i) => sha(i + 100));
  r.edges = Array.from({ length: 64 }, (_, i) => ({ from: sha(i + 1), to: sha(i + 2),
    phase: "committed", context: "context-" + "a".repeat(64) + ".json",
    grant: { id: i + 1, origin: "checkpoint-pending", originSource: source(), scannedTip: sha(i + 100), flaggedOIDs: [sha(i + 100)] } }));
  write(saved);
  await cache.markRecoveryCapture(bare, clone, BRANCH, RUN, ATTEMPT);
  assert.equal(journal().remediationRecovery.edges.length, 64);
  for (const change of [
    (x: any) => { x.edges[63].to = x.edges[0].from; },
    (x: any) => { x.edges[1].from = x.edges[0].from; },
    (x: any) => { x.edges[0].extra = 1; },
    (x: any) => { x.edges[0].grant.extra = 1; },
    (x: any) => { for (const e of x.edges.slice(0, 16)) e.grant.flaggedOIDs = [...x.flaggedOIDs]; },
  ]) {
    const next = structuredClone(saved); change(next.remediationRecovery); write(next);
    await assert.rejects(cache.markRecoveryCapture(bare, clone, BRANCH, RUN, ATTEMPT), /remediation/);
  }
});

test("grant write fsync/readback failures yield no successful write; consumer re-syncs independently", async () => {
  const q = commit("q"); await fetch();
  for (const durability of [
    ...(["config", "directory"] as const).map(target => ({
      sync: async (handle: { sync(): Promise<void> }, what: "config" | "directory") => {
        if (what === target) throw Error(target);
        await handle.sync();
      },
    })),
    { readback: async () => "" },
  ]) {
    await assert.rejects(cache.recordRemediationScan(bare, BRANCH, source(),
      { trusted: true, tip: q, flaggedOIDs: [q], origin: "checkpoint-pending" }, durability));
    assert.equal(await cache.readRemediationRecovery(bare, BRANCH, source(), durability), undefined);
    // Readability is insufficient: the separate successful consumer must still sync and prove binding.
    assert.ok(await cache.readRemediationRecovery(bare, BRANCH, source()));
  }
});

test("failed tracking CAS leaves prepared evidence that restart never promotes", async t => {
  const q = commit("q"); await fetch();
  await cache.recordRemediationScan(bare, BRANCH, source(), { trusted: true, tip: q, flaggedOIDs: [q], origin: "pre-exit" });
  commit("rewrite");
  const internal = cache as any;
  const run = internal.runGit.bind(cache);
  t.mock.method(internal, "runGit", async (dir: string, args: string[]) => {
    if (args[0] === "update-ref" && args[1] === "refs/uzi-runner/" + BRANCH) throw Error("CAS");
    return run(dir, args);
  });
  const result = await cache.fetchAgentBranch(bare, clone, BRANCH, RUN, opts);
  assert.deepEqual(result, { kind: "not_updated", reason: "tracking_update_failed" });
  assert.equal(journal().remediationRecovery.edges[0].phase, "prepared");
  const restarted = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
  assert.equal(await restarted.readRemediationRecovery(bare, BRANCH, source()), undefined);
  t.mock.restoreAll();
  const retry = await cache.fetchAgentBranch(bare, clone, BRANCH, RUN, opts);
  assert.equal(retry.kind, "not_updated");
  assert.equal(journal().remediationRecovery.edges[0].phase, "prepared");
});

for (const phase of ["prepared", "committed"] as const) {
  test("import " + phase + " durability failure is not successful import authority", async t => {
    const q = commit("q"); await fetch();
    await cache.recordRemediationScan(bare, BRANCH, source(), { trusted: true, tip: q, flaggedOIDs: [q], origin: "pre-exit" });
    commit("rewrite");
    const internal = cache as any;
    const sync = internal.syncJournalAndReadBack.bind(cache);
    t.mock.method(internal, "syncJournalAndReadBack", async (...args: any[]) => {
      const record = JSON.parse(args[2]).remediationRecovery;
      if (record?.edges.some((e: any) => e.phase === phase)) throw Error("durability");
      return sync(...args);
    });
    await assert.rejects(fetch(), /durability/);
    assert.equal(await cache.readRemediationRecovery(bare, BRANCH, source()), undefined);
    t.mock.restoreAll();
    const restarted = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
    const confirmed = await restarted.readRemediationRecovery(bare, BRANCH, source());
    assert.equal(confirmed !== undefined, phase === "committed");
  });
}

test("same run and generation with two real producing attempts cannot substitute identity", async () => {
  await enroll();
  const saved = raw();
  git(bare, "config", "uzi-recovery." + BRANCH + ".clone", "");
  const attemptId = formatAttemptId(new Date("2026-01-01T00:00:00Z"), 1, "fedcba9876543210");
  const sibling = await cache.runnerCloneForBranch(bare, BRANCH, KEY, noProofReseed, RUN, false, undefined,
    { attemptId, isLive: p => p === clone, beforeSeed: async () => {} });
  git(bare, "config", "uzi-recovery." + BRANCH + ".clone", saved);
  assert.equal(await cache.readRemediationRecovery(bare, BRANCH, { runId: RUN, clonePath: sibling.path, attemptId }), undefined);
  await assert.rejects(cache.fetchAgentBranch(bare, sibling.path, BRANCH, RUN,
    { ...opts, producingSource: { runId: RUN, clonePath: sibling.path, attemptId } }), /binding/);
});

test("one pending checkpoint rewrite followed by a distinct pre-exit rewrite retains both grants", async () => {
  const q = commit("checkpoint q"); await fetch();
  await cache.recordRemediationScan(bare, BRANCH, source(), { trusted: true, tip: q, flaggedOIDs: [q], origin: "checkpoint-pending" });
  const later = commit("pending later", q); await fetch();
  const first = commit("first rewrite"); await fetch();
  await cache.recordRemediationScan(bare, BRANCH, source(), { trusted: true, tip: first, flaggedOIDs: [] });
  const q2 = commit("pre-exit q", first); await fetch();
  await cache.recordRemediationScan(bare, BRANCH, source(), { trusted: true, tip: q2, flaggedOIDs: [q2], origin: "pre-exit" });
  const last = commit("second rewrite", first); await fetch();
  const record = await cache.readRemediationRecovery(bare, BRANCH, source());
  assert.ok(record);
  assert.deepEqual(record.edges.map(e => [e.from, e.to, e.grant.id, e.grant.origin]),
    [[later, first, 1, "checkpoint-pending"], [q2, last, 2, "pre-exit"]]);
  assert.deepEqual(record.flaggedOIDs, [q, q2]);
});


test("ordinary imports and clean scans leave no field and permit the existing bridge and retirement lifecycle", async () => {
  assert.equal(journal().remediationRecovery, undefined);
  await cache.recordRemediationScan(bare, BRANCH, source(), { trusted: true, tip: base, flaggedOIDs: [] });
  assert.equal(journal().remediationRecovery, undefined);
  const tip = commit("ordinary"); await fetch();
  const next = commit("ordinary bridge", tip);
  // updateTrackingRef takes a bare commit, as a production synthesized bridge would.
  git(clone, "push", bare, next + ":refs/keep/ordinary");
  assert.equal((await cache.updateTrackingRef(bare, BRANCH, next, opts)).kind, "updated");
  assert.equal(journal().remediationRecovery, undefined);
  await cache.retireRunnerClone(bare, clone, BRANCH, RUN, { discard: true, attemptId: ATTEMPT });
  assert.equal(raw(), "");
});

test("findings require an authorized origin and every full commit identity before enrollment", async () => {
  const q = commit("q"); await fetch();
  for (const scan of [
    { trusted: true, tip: q, flaggedOIDs: [q] },
    { trusted: true, tip: q, flaggedOIDs: [q, "invalid"], origin: "pre-exit" as const },
    { trusted: true, tip: base, flaggedOIDs: [base], origin: "pre-exit" as const },
  ]) {
    await assert.rejects(cache.recordRemediationScan(bare, BRANCH, source(), scan));
    assert.equal(journal().remediationRecovery, undefined);
  }
});

test("metadata is preserved on refresh and unproven drop but independently authorized discard still works", async () => {
  await enroll();
  const saved = raw();
  await cache.markRecoveryCapture(bare, clone, BRANCH, RUN, ATTEMPT);
  assert.equal(raw(), saved);
  await assert.rejects(cache.releaseAttemptInPlace(bare, clone, BRANCH, RUN, "abandoned"), /remediation/);
  await assert.rejects(cache.updateTrackingRef(bare, BRANCH, base, opts), /remediation/);
  await assert.rejects(cache.retireRunnerClone(bare, clone, BRANCH, RUN, { discard: true, attemptId: ATTEMPT }), /remediation/);
  assert.equal(raw(), saved);
  assert.ok(fs.existsSync(clone));
  await cache.discardRetainedRecovery(bare, BRANCH, KEY, source(), true);
  assert.equal(raw(), "");
  assert.ok(fs.existsSync(clone), "metadata discard does not itself delete the source");
});

test("restart query refuses changed pins, missing objects, stale journal/owner/ledger and forged grant source", async t => {
  const q = commit("q"); await fetch();
  await cache.recordRemediationScan(bare, BRANCH, source(), { trusted: true, tip: q, flaggedOIDs: [q], origin: "pre-exit" });
  const tip = commit("rewrite"); await fetch();
  const saved = journal();
  const pin = "refs/uzi-owed/" + RUN + "/" + q;
  git(bare, "update-ref", pin, base, q);
  assert.equal(await cache.readRemediationRecovery(bare, BRANCH, source()), undefined);
  git(bare, "update-ref", pin, q, base);
  const internal = cache as any, run = internal.runGit.bind(cache);
  t.mock.method(internal, "runGit", async (dir: string, args: string[]) => {
    if (args.includes("cat-file") && args.includes(q)) throw Error("missing object");
    return run(dir, args);
  });
  assert.equal(await cache.readRemediationRecovery(bare, BRANCH, source()), undefined);
  t.mock.restoreAll();
  for (const mutate of [
    (j: any) => { j.remediationRecovery.ownedTip = base; },
    (j: any) => {
      for (const g of [j.remediationRecovery.active, ...j.remediationRecovery.edges.map((e: any) => e.grant)])
        g.originSource = { ...source(), clonePath: clone.replace(ATTEMPT, ATTEMPT.replace("0123", "abcd")), attemptId: ATTEMPT.replace("0123", "abcd") };
    },
  ]) {
    const next = structuredClone(saved); mutate(next); write(next);
    assert.equal(await cache.readRemediationRecovery(bare, BRANCH, source()), undefined);
  }
  write(saved);
  const ledgerKey = "uzi-attempts." + BRANCH + ".entry";
  const ledger = git(bare, "config", "--get-all", ledgerKey);
  git(bare, "config", "--unset-all", ledgerKey);
  assert.equal(await cache.readRemediationRecovery(bare, BRANCH, source()), undefined);
  for (const entry of ledger.split("\n")) git(bare, "config", "--add", ledgerKey, entry);
  const receipt = path.join(bare, "uzi-owed", "receipt-" + createHash("sha256").update(BRANCH).digest("hex") + ".json");
  const owner = fs.readFileSync(receipt, "utf8");
  fs.writeFileSync(receipt, JSON.stringify({ ...JSON.parse(owner), runId: "foreign" }));
  assert.equal(await cache.readRemediationRecovery(bare, BRANCH, source()), undefined);
  fs.writeFileSync(receipt, owner);
  assert.equal((await cache.readRemediationRecovery(bare, BRANCH, source()))?.ownedTip, tip);
});

test("completion episode, memory park and completion fence retain remediation metadata", async () => {
  await enroll();
  const r = journal().remediationRecovery;
  // A ready journal is an already completed same-source handoff. M1 never creates or adopts a successor.
  const now = Date.now();
  write({ ...journal(), restoreTip: base, recovery: { version: 1, source: { ...source(), restoreTip: base },
    successor: { ...source(), restoreTip: base }, attempts: 1, startedAt: now, deadline: now + 300000,
    backoffMs: 0, stage: "ready-for-model", restoreTip: base } });
  await cache.completeRecoveryEpisode(bare, BRANCH, KEY, source(), { settled: true, processedEvents: 1, attemptId: ATTEMPT });
  assert.deepEqual(journal().remediationRecovery, r);
  const uuid = "11111111-1111-4111-8111-111111111111";
  await cache.recordMemoryPark(bare, BRANCH, source(), { version: 1,
    binding: { runId: RUN, claimGeneration: 1, workerId: "worker", registerNonce: "nonce", memoryEpisode: 0 },
    ack: "sent_unconfirmed", origin: { flightId: uuid, attemptId: ATTEMPT, executorIncarnation: null,
      authorityIncarnation: uuid, sourceEpoch: uuid } });
  assert.deepEqual(journal().remediationRecovery, r);
  await cache.installCompletionSourceFence(bare, BRANCH, source(), { version: 1, sourceUseEpoch: uuid, sourceCoreDigest: "a".repeat(64) });
  assert.deepEqual(journal().remediationRecovery, r);
});

test("config write failure cannot durably enroll remediation", async t => {
  const internal = cache as any, run = internal.runGit.bind(cache);
  t.mock.method(internal, "runGit", async (dir: string, args: string[]) => {
    if (args[0] === "config" && args.some(a => a.includes('"remediationRecovery"'))) throw Error("config write");
    return run(dir, args);
  });
  await assert.rejects(enroll(), /config write/);
  assert.equal(journal().remediationRecovery, undefined);
});


test("partial checkpoint rewrite and trusted rescan retain both displacement edges", async () => {
  const q = commit("still flagged"); await fetch();
  const firstTip = commit("first scanned tip", q); await fetch();
  await cache.recordRemediationScan(bare, BRANCH, source(),
    { trusted: true, tip: firstTip, flaggedOIDs: [q], origin: "checkpoint-pending" });
  const partial = commit("partial rewrite", q); await fetch();
  await cache.recordRemediationScan(bare, BRANCH, source(),
    { trusted: true, tip: partial, flaggedOIDs: [q], origin: "checkpoint-pending" });
  const clean = commit("clean rewrite"); await fetch();
  const restarted = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
  const record = await restarted.readRemediationRecovery(bare, BRANCH, source());
  assert.ok(record);
  assert.deepEqual(record.edges.map(e => [e.from, e.to, e.grant.scannedTip, e.grant.id]),
    [[firstTip, partial, firstTip, 1], [partial, clean, partial, 2]]);
});

test("runtime restriction: positive absence succeeds, malformed enrollment and unavailable reads throw", async t => {
  assert.equal(await cache.readRuntimeRemediationRecovery(bare, BRANCH, source()), undefined);
  const saved = journal();
  write({ ...saved, remediationRecovery: {} });
  await assert.rejects(cache.readRuntimeRemediationRecovery(bare, BRANCH, source()));
  write(saved);
  const internal = cache as any;
  const run = internal.runGit.bind(cache);
  t.mock.method(internal, "runGit", async (dir: string, args: string[]) => {
    if (args[0] === "config" && args.includes("--list")) throw Error("config unavailable");
    return run(dir, args);
  });
  await assert.rejects(cache.readRuntimeRemediationRecovery(bare, BRANCH, source()), /config unavailable/);
});

test("runtime restriction: real committed M1 evidence confirms without writing pins or journal", async () => {
  const { replacement } = await rewritten();
  const before = raw();
  const pins = pinnedRefs();
  const synced: string[] = [];
  const result = await cache.readRuntimeRemediationRecovery(bare, BRANCH, source(), {
    sync: async (handle, what) => { synced.push(what); await handle.sync(); },
  });
  assert.equal(result?.ownedTip, replacement);
  assert.equal(result?.edges[0]?.phase, "committed");
  assert.deepEqual(synced, ["config", "directory"]);
  assert.deepEqual(result, await cache.readRemediationRecovery(bare, BRANCH, source()));
  assert.equal(raw(), before);
  assert.equal(pinnedRefs(), pins);
});

test("runtime restriction: exact source identities, live ledger and clone HEAD remain independent proofs", async t => {
  await enroll();
  for (const wrong of [
    { ...source(), runId: "other" },
    { ...source(), clonePath: clone + "-stale" },
    { ...source(), attemptId: successorAttempt },
    { ...source(), attemptId: undefined } as any,
  ]) await assert.rejects(cache.readRuntimeRemediationRecovery(bare, BRANCH, wrong));
  commit("unimported source head");
  await assert.rejects(cache.readRuntimeRemediationRecovery(bare, BRANCH, source()));
  git(clone, "update-ref", "refs/heads/" + BRANCH, base);
  t.mock.method(cache as any, "readAttemptLedger", async () => new Map());
  await assert.rejects(cache.readRuntimeRemediationRecovery(bare, BRANCH, source()), /source binding/);
});

test("runtime restriction: prepared evidence, missing pins, receipt and durability failures throw", async () => {
  const { displaced } = await rewritten();
  const saved = journal();
  const prepared = structuredClone(saved);
  prepared.remediationRecovery.edges[0].phase = "prepared";
  write(prepared);
  await assert.rejects(cache.readRuntimeRemediationRecovery(bare, BRANCH, source()));
  write(saved);
  for (const target of ["config", "directory"] as const) {
    await assert.rejects(cache.readRuntimeRemediationRecovery(bare, BRANCH, source(), {
      sync: async (handle, what) => { if (what === target) throw Error("sync " + target); await handle.sync(); },
    }), /sync/);
  }
  await assert.rejects(cache.readRuntimeRemediationRecovery(bare, BRANCH, source(), { readback: async () => "" }), /read-back/);
  const receipt = path.join(bare, "uzi-owed", "receipt-" + createHash("sha256").update(BRANCH).digest("hex") + ".json");
  const receiptRaw = fs.readFileSync(receipt, "utf8");
  fs.writeFileSync(receipt, receiptRaw.replace('"committed"', '"pending"'));
  await assert.rejects(cache.readRuntimeRemediationRecovery(bare, BRANCH, source()));
  fs.writeFileSync(receipt, receiptRaw);
  git(bare, "update-ref", "-d", "refs/uzi-owed/" + RUN + "/" + displaced, displaced);
  await assert.rejects(cache.readRuntimeRemediationRecovery(bare, BRANCH, source()));
});

for (const proof of ["missing anchor", "divergent B", "anchored descendant B"] as const) {
  test("runtime restriction: hypothetical B at receipt seam, " + proof, async t => {
    const { replacement: s } = await rewritten();
    if (proof !== "missing anchor") {
      assert.equal((await cache.retainCurrentOwedCandidate(bare, opts)).kind, "updated");
      assert.ok((await cache.enumerateOwedCandidates(bare, RUN)).some(c => c.sha === s));
    } else {
      // M1 import already preserves its replacement. Remove only S's pin for this refusal case.
      git(bare, "update-ref", "-d", "refs/uzi-owed/" + RUN + "/" + s, s);
      assert.ok(!(await cache.enumerateOwedCandidates(bare, RUN)).some(c => c.sha === s));
    }
    const b = git(bare, ...IDENT, "commit-tree", base + "^{tree}", "-p", proof === "divergent B" ? base : s, "-m", "hypothetical B");
    // Reader-only fixture: the next task owns guarded B production writes. Substitute only
    // the observed tracking SHA and receipt payload; real governance, stamp, context, source,
    // ancestry and existing owed-anchor validation still run. No tracking ref is rewritten.
    const internal = cache as any;
    const read = internal.readOwedFile.bind(cache);
    const receiptName = "receipt-" + createHash("sha256").update(BRANCH).digest("hex") + ".json";
    t.mock.method(internal, "checkedTrackingSha", async () => b);
    t.mock.method(internal, "readOwedFile", async (...args: any[]) => {
      const value = await read(...args);
      return args[1] === receiptName ? { ...value, trackingSha: b } : value;
    });
    const before = raw();
    const pins = pinnedRefs();
    assert.equal(await cache.readRemediationRecovery(bare, BRANCH, source()), undefined);
    if (proof === "anchored descendant B") {
      const result = await cache.readRuntimeRemediationRecovery(bare, BRANCH, source());
      assert.equal(result?.ownedTip, s);
      assert.equal(result?.edges.length, 1);
      // A receipt contains no attempt; journal identity is still mandatory with tracking B.
      await assert.rejects(cache.readRuntimeRemediationRecovery(bare, BRANCH, { ...source(), attemptId: successorAttempt }));
    } else await assert.rejects(cache.readRuntimeRemediationRecovery(bare, BRANCH, source()));
    assert.equal(raw(), before);
    assert.equal(pinnedRefs(), pins);
    assert.equal(git(bare, "rev-parse", "refs/uzi-runner/" + BRANCH), s);
  });
}

const successorAttempt = formatAttemptId(new Date("2026-01-01T00:00:00Z"), 2, "fedcba9876543210");
const admitSource = { admitRemediationSource: async () => true };
async function captureAdoption(roots: string[], current: string) {
  const coverage = await cache.buildRecoveryCoverage(bare, opts.context as PositiveOwedCandidateContext, roots, current);
  const reservation = await cache.reserveRecoveryIteration(bare, BRANCH, KEY, source());
  await cache.recordRecoveryCapture(bare, BRANCH, KEY, source(), reservation.attempts, coverage.sha, current);
  return { coverage: coverage.sha, iteration: reservation.attempts };
}
function pinnedRefs(): string {
  return git(bare, "for-each-ref", "--format=%(refname) %(objectname)",
    "refs/uzi-owed/", "refs/uzi-coverage/", "refs/uzi-recovery-episode/");
}
async function rewritten() {
  const displaced = commit("displaced"); await fetch(); await enroll();
  const replacement = commit("authorized replacement"); await fetch();
  return { displaced, replacement };
}
async function refusal(iteration: number, options: Parameters<GitCache["prepareRecoverySuccessor"]>[6] = admitSource) {
  const before = raw();
  const pins = pinnedRefs();
  const entries = git(bare, "config", "--get-all", "uzi-attempts." + BRANCH + ".entry");
  await assert.rejects(cache.prepareRecoverySuccessor(bare, BRANCH, KEY, source(), iteration, successorAttempt, options));
  assert.equal(raw(), before);
  assert.equal(pinnedRefs(), pins);
  assert.equal(git(bare, "config", "--get-all", "uzi-attempts." + BRANCH + ".entry"), entries);
}

test("M2 marker-free divergent wrapper without authority refuses before any effects", async () => {
  const displaced = commit("unapproved sibling"); await fetch();
  const current = commit("ordinary sibling"); await fetch();
  const { iteration } = await captureAdoption([displaced, current], current);
  await refusal(iteration);
});

test("M2 committed M1 evidence adopts exact S and durably transfers immutable provenance and pins", async () => {
  const { displaced, replacement } = await rewritten();
  const current = commit("ordinary progress after rewrite", replacement); await fetch();
  const { iteration, coverage } = await captureAdoption([displaced, current], current);
  const evidence = await cache.readRemediationRecovery(bare, BRANCH, source());
  assert.ok(evidence);
  const before = journal();
  const pins = pinnedRefs();
  const tracking = git(bare, "rev-parse", "refs/uzi-runner/" + BRANCH);
  await refusal(iteration, {});
  await refusal(iteration, { admitRemediationSource: async () => false });
  let admissions = 0;
  const successor = await cache.prepareRecoverySuccessor(bare, BRANCH, KEY, source(), iteration, successorAttempt, {
    admitRemediationSource: async input => {
      admissions++;
      assert.deepEqual(input, { ...source(), branch: BRANCH, sourceTip: current, captureTip: coverage });
      return true;
    },
  });
  assert.equal(admissions, 1);
  assert.equal(git(successor.path, "rev-parse", "HEAD"), current);
  assert.equal(git(successor.path, "write-tree"), git(bare, "rev-parse", current + "^{tree}"));
  assert.equal(git(successor.path, "status", "--porcelain"), "");
  assert.equal(successor.adoptedSource, current);
  assert.equal(pinnedRefs(), pins);
  assert.equal(git(bare, "rev-parse", "refs/uzi-runner/" + BRANCH), tracking);
  const transferred = journal();
  assert.deepEqual(transferred.remediationRecovery, { ...evidence, clonePath: successor.path, attemptId: successorAttempt });
  assert.deepEqual(transferred.recovery.source, before.recovery.source);
  const next = { runId: RUN, clonePath: successor.path, attemptId: successorAttempt };
  const restarted = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
  assert.deepEqual(await restarted.readRemediationRecovery(bare, BRANCH, next), transferred.remediationRecovery);
  await cache.completeRecoveryEpisode(bare, BRANCH, KEY, next, { settled: true, processedEvents: 1, attemptId: successorAttempt });
  assert.deepEqual(journal().remediationRecovery, transferred.remediationRecovery);
  assert.equal(pinnedRefs(), pins);
  await assert.rejects(cache.markRecoveryCapture(bare, clone, BRANCH, RUN, ATTEMPT));
});

test("M2 repeated authorized rewrites permit ordinary progress between replacements", async () => {
  const { displaced, replacement } = await rewritten();
  const q2 = commit("new finding after progress", replacement); await fetch(); await enroll();
  const d2 = commit("later displaced tip", q2); await fetch();
  const r2 = commit("second replacement"); await fetch();
  const current = commit("final progress", r2); await fetch();
  const { iteration } = await captureAdoption([displaced, replacement, d2, current], current);
  const successor = await cache.prepareRecoverySuccessor(bare, BRANCH, KEY, source(), iteration, successorAttempt, admitSource);
  assert.equal(git(successor.path, "rev-parse", "HEAD"), current);
  assert.equal(journal().remediationRecovery.edges.length, 2);
});

test("M2 partial rewrites under one active grant reach S through ordinary progress", async () => {
  const q = commit("finding"); await fetch(); await enroll();
  const first = commit("first displaced", q); await fetch();
  const partial = commit("partial replacement retaining finding", q); await fetch();
  const second = commit("ordinary progress before second displacement", partial); await fetch();
  const clean = commit("final replacement"); await fetch();
  const current = commit("ordinary progress after closure", clean); await fetch();
  await cache.recordRemediationScan(bare, BRANCH, source(), { trusted: true, tip: current, flaggedOIDs: [] });
  const evidence = await cache.readRemediationRecovery(bare, BRANCH, source());
  assert.ok(evidence);
  assert.deepEqual(evidence.edges.map(e => e.grant.id), [1, 1]);
  assert.equal(evidence.active, undefined);
  const { iteration } = await captureAdoption([q, first, partial, second, current], current);
  const successor = await cache.prepareRecoverySuccessor(bare, BRANCH, KEY, source(), iteration, successorAttempt, admitSource);
  assert.equal(git(successor.path, "rev-parse", "HEAD"), current);
});

for (const fault of ["prepared", "foreign", "unrelated leaf", "unrelated replacement", "source mismatch"] as const) {
  test("M2 refuses " + fault + " evidence without adoption effects", async () => {
    const { displaced, replacement } = await rewritten();
    let roots = [displaced, replacement];
    if (fault === "unrelated leaf") roots = [...roots, git(bare, ...IDENT, "commit-tree", base + "^{tree}", "-p", base, "-m", "unrelated")];
    const { iteration } = await captureAdoption(roots, replacement);
    const value = journal();
    if (fault === "prepared") value.remediationRecovery.edges[0].phase = "prepared";
    if (fault === "foreign") value.remediationRecovery.edges[0].grant.originSource.attemptId = successorAttempt;
    if (fault === "unrelated replacement") value.remediationRecovery.edges[0].to = git(bare, ...IDENT, "commit-tree", base + "^{tree}", "-p", base, "-m", "unrelated replacement");
    if (fault === "source mismatch") value.recovery.restoreSource = displaced;
    write(value);
    await refusal(iteration);
  });
}

for (const lineage of [false, true]) {
  test("M2 superseded marker requires lineage as well as displaced-parent proof: " + lineage, async () => {
    const displaced = commit("flagged marker parent"); await fetch(); await enroll();
    fs.writeFileSync(path.join(clone, "marker.txt"), "marker content\n");
    git(clone, "add", "marker.txt");
    assert.equal(await cache.commitWipMarker(clone), true);
    const marker = git(clone, "rev-parse", "HEAD"); await fetch();
    // Use a genuinely different clean tree, not a same-tree substitute for ancestry.
    git(clone, "reset", "--hard", base);
    const current = commit("clean replacement"); await fetch();
    if (lineage) await cache.markRecoveryCapture(bare, clone, BRANCH, RUN, ATTEMPT, marker);
    const { iteration } = await captureAdoption([displaced, marker, current], current);
    if (!lineage) await refusal(iteration);
    else {
      const successor = await cache.prepareRecoverySuccessor(bare, BRANCH, KEY, source(), iteration, successorAttempt, admitSource);
      assert.equal(git(successor.path, "rev-parse", "HEAD"), current);
      assert.equal(git(successor.path, "write-tree"), git(bare, "rev-parse", current + "^{tree}"));
      assert.deepEqual(journal().peeledMarkers, [marker]);
    }
  });
}

// Real tracking writes through the worker-only seam; no source-import authority for B.
async function transformationFixture() {
  const { displaced, replacement: s } = await rewritten();
  await cache.recordRemediationScan(bare, BRANCH, source(), { trusted: true, tip: s, flaggedOIDs: [] });
  const floor = git(bare, ...IDENT, "commit-tree", base + "^{tree}", "-m", "safe unrelated floor");
  const bridge = await cache.bridgeToFloors(bare, s, [floor]);
  assert.equal(bridge.kind, "built");
  if (bridge.kind !== "built") throw Error("bridge not built");
  return { s, b: bridge.sha, displaced, floor, context: { context: opts.context } };
}
const allowTransformation = { trusted: true, admit: async () => true };

test("worker transformation: real bridge advances tracking while execution and provenance stay S", async () => {
  const { s, b, displaced, context } = await transformationFixture();
  await captureAdoption([displaced, s], s);
  const before = raw(), pins = pinnedRefs().split("\n");
  const origins = git(bare, "for-each-ref", "--format=%(refname) %(objectname)", "refs/remotes/");
  const result = await cache.updateRemediationTrackingRef(bare, BRANCH, b, context, source(), s, {
    trusted: true, admit: async input => {
      assert.ok(Object.isFrozen(input));
      assert.deepEqual(input, { barePath: bare, branch: BRANCH, ...source(), generation: 1,
        sourceTip: s, trackingTip: s, candidateSha: b });
      return true;
    },
  });
  assert.equal(result.kind, "updated");
  assert.equal(git(bare, "rev-parse", "refs/uzi-runner/" + BRANCH), b);
  assert.equal(git(clone, "rev-parse", "HEAD"), s);
  assert.equal(git(clone, "rev-parse", "HEAD^{tree}"), git(bare, "rev-parse", b + "^{tree}"));
  assert.equal(raw(), before);
  for (const pin of pins) assert.ok(pinnedRefs().split("\n").includes(pin));
  assert.equal(git(bare, "for-each-ref", "--format=%(refname) %(objectname)", "refs/remotes/"), origins);
  assert.ok((await cache.enumerateOwedCandidates(bare, RUN)).some(c => c.sha === s));
  const fresh = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
  assert.equal((await fresh.readRuntimeRemediationRecovery(bare, BRANCH, source()))?.ownedTip, s);
  assert.equal(await fresh.readRemediationRecovery(bare, BRANCH, source()), undefined);
  // A second transform authenticates tracking H independently when H already differs from S.
  const next = git(bare, ...IDENT, "commit-tree", s + "^{tree}", "-p", b, "-m", "next worker transform");
  assert.equal((await fresh.updateRemediationTrackingRef(bare, BRANCH, next, context, source(), b,
    allowTransformation)).kind, "updated");
  assert.equal(raw(), before);
});

test("worker transformation: excluded merge history refuses even with trusted admission; generic update stays guarded", async () => {
  const { s, displaced, context } = await transformationFixture();
  const unsafe = await cache.bridgeToFloors(bare, s, [displaced]);
  assert.equal(unsafe.kind, "built");
  if (unsafe.kind !== "built") throw Error("unsafe bridge not built");
  const before = raw(), pins = pinnedRefs();
  let called = false;
  await assert.rejects(cache.updateRemediationTrackingRef(bare, BRANCH, unsafe.sha, context, source(), s,
    { trusted: true, admit: async () => { called = true; return true; } }), /excluded history/);
  assert.equal(called, false);
  await assert.rejects(cache.updateTrackingRef(bare, BRANCH, unsafe.sha, context), /remediation/);
  assert.equal(git(bare, "rev-parse", "refs/uzi-runner/" + BRANCH), s);
  assert.equal(raw(), before);
  assert.equal(pinnedRefs(), pins);
});

test("worker transformation: exact source, claim, expected tracking and fresh admission are mandatory", async () => {
  const { s, b, context } = await transformationFixture();
  const before = raw(), pins = pinnedRefs();
  const call = (options = context, identity = source(), expected = s, admission = allowTransformation) =>
    cache.updateRemediationTrackingRef(bare, BRANCH, b, options, identity, expected, admission);
  await assert.rejects(call(context, source(), s, { trusted: true, admit: async () => false }));
  await assert.rejects(call(context, source(), s, { trusted: false, admit: async () => true }));
  await assert.rejects(call(context, { ...source(), attemptId: undefined } as any));
  await assert.rejects(call(context, { ...source(), attemptId: successorAttempt }));
  await assert.rejects(call(context, source(), base));
  await assert.rejects(call({ context: { ...opts.context as PositiveOwedCandidateContext, generation: 2 } }));
  await assert.rejects(call(opts));
  await assert.rejects(call(context, source(), s, { trusted: true, admit: async () => { throw Error("scan unavailable"); } }));
  assert.equal(git(bare, "rev-parse", "refs/uzi-runner/" + BRANCH), s);
  assert.equal(raw(), before);
  assert.equal(pinnedRefs(), pins);
});

test("worker transformation: callback source or evidence changes and failed CAS cannot grant eligibility", async t => {
  const { s, b, context } = await transformationFixture();
  const before = raw(), pins = pinnedRefs();
  await assert.rejects(cache.updateRemediationTrackingRef(bare, BRANCH, b, context, source(), s, {
    trusted: true, admit: async () => { commit("changed during scan"); return true; },
  }));
  git(clone, "update-ref", "refs/heads/" + BRANCH, s);
  await assert.rejects(cache.updateRemediationTrackingRef(bare, BRANCH, b, context, source(), s, {
    trusted: true, admit: async () => {
      const changed = journal(); changed.remediationRecovery.sequence++; write(changed); return true;
    },
  }));
  write(JSON.parse(before));
  assert.equal(git(bare, "rev-parse", "refs/uzi-runner/" + BRANCH), s);
  assert.equal(pinnedRefs(), pins);
  const internal = cache as any, run = internal.runGit.bind(cache);
  t.mock.method(internal, "runGit", async (dir: string, args: string[]) => {
    if (args[0] === "update-ref" && args[1] === "refs/uzi-runner/" + BRANCH) throw Error("CAS unavailable");
    return run(dir, args);
  });
  assert.equal((await cache.updateRemediationTrackingRef(bare, BRANCH, b, context, source(), s,
    allowTransformation)).kind, "not_updated");
  assert.equal(git(bare, "rev-parse", "refs/uzi-runner/" + BRANCH), s);
  assert.equal(raw(), before);
  for (const pin of pins.split("\n")) assert.ok(pinnedRefs().split("\n").includes(pin));
  assert.ok((await cache.enumerateOwedCandidates(bare, RUN)).some(c => c.sha === s));
  await assert.rejects(cache.readRuntimeRemediationRecovery(bare, BRANCH, source()));
});

test("source import after transform: ordinary source progress preserves B and immutable roots", async () => {
  const { s, b, displaced, context } = await transformationFixture();
  const before = journal().remediationRecovery;
  assert.equal((await cache.updateRemediationTrackingRef(bare, BRANCH, b, context, source(), s,
    allowTransformation)).kind, "updated");
  const pins = pinnedRefs().split("\n");
  const r = commit("actual ordinary progress", s);
  await fetch();
  assert.equal(git(bare, "rev-parse", "refs/uzi-runner/" + BRANCH), r);
  assert.equal(journal().remediationRecovery.ownedTip, r);
  assert.deepEqual(journal().remediationRecovery.edges, before.edges);
  for (const pin of pins) assert.ok(pinnedRefs().split("\n").includes(pin));
  const owed = await cache.enumerateOwedCandidates(bare, RUN);
  for (const sha of [b, displaced, s]) assert.ok(owed.some(candidate => candidate.sha === sha));
  const fresh = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
  assert.equal((await fresh.readRuntimeRemediationRecovery(bare, BRANCH, source()))?.ownedTip, r);
});

test("source import after transform: later trusted finding displaces actual source only", async () => {
  const { s, b, context } = await transformationFixture();
  const original = journal().remediationRecovery.edges;
  assert.equal((await cache.updateRemediationTrackingRef(bare, BRANCH, b, context, source(), s,
    allowTransformation)).kind, "updated");
  const d = commit("new source finding", s); await fetch();
  await cache.recordRemediationScan(bare, BRANCH, source(),
    { trusted: true, tip: d, flaggedOIDs: [d], origin: "checkpoint-pending" });
  const grant = journal().remediationRecovery.active;
  const r = commit("actual second rewrite"); await fetch();
  const evidence = await cache.readRuntimeRemediationRecovery(bare, BRANCH, source());
  assert.ok(evidence);
  assert.deepEqual(evidence.edges.slice(0, -1), original);
  assert.deepEqual(evidence.edges.at(-1), { from: d, to: r, phase: "committed",
    context: original[0].context, grant });
  assert.deepEqual(grant.originSource, source());
  assert.ok(evidence.edges.every(edge => edge.from !== b && edge.to !== b));
});

test("source import after transform: crash handoff keeps old D context and producing attempt", async () => {
  const d = commit("generation one finding"); await fetch(); await enroll();
  const original = journal().remediationRecovery;
  const ownership = await cache.committedTrackingOwnership(bare, BRANCH, RUN, d, 1);
  assert.equal(ownership.kind, "owned");
  const r = commit("private rewrite before crash");
  opts = { ...opts, context: { ...opts.context as PositiveOwedCandidateContext, generation: 2 } };
  await assert.rejects(cache.fetchAgentBranch(bare, clone, BRANCH, RUN,
    { ...opts, producingSource: { ...source(), attemptId: successorAttempt } }), /source binding/);
  await assert.rejects(cache.fetchAgentBranch(bare, clone, BRANCH, RUN,
    { ...opts, producingSource: undefined }), /producing source required/);
  await fetch();
  const evidence = await cache.readRuntimeRemediationRecovery(bare, BRANCH, source());
  assert.ok(evidence);
  assert.equal(ownership.kind === "owned" && ownership.context.generation, 1);
  const oldContext = (cache as any).contextName(ownership.kind === "owned" && ownership.context);
  assert.deepEqual(evidence.edges, [{ from: d, to: r, phase: "committed", context: oldContext,
    grant: original.active }]);
  assert.deepEqual(evidence.edges[0]!.grant.originSource, source());
  assert.equal((await cache.committedTrackingOwnership(bare, BRANCH, RUN, r, 2)).kind, "owned");
});

test("source import after transform: reconciled S is repinned and ordinary progress needs no S pin", async () => {
  const { s, b, displaced, context } = await transformationFixture();
  const before = raw();
  const reconciled = await cache.reconcileOwedCandidates(bare, RUN, s);
  assert.ok(reconciled.removedShas.includes(s));
  assert.ok(reconciled.retainedShas.includes(displaced));
  assert.equal((await cache.updateRemediationTrackingRef(bare, BRANCH, b,
    { ...context, remotelyConfirmedSha: s }, source(), s, allowTransformation)).kind, "updated");
  assert.ok((await cache.enumerateOwedCandidates(bare, RUN)).some(candidate => candidate.sha === s));
  assert.equal(raw(), before);
  await cache.reconcileOwedCandidates(bare, RUN, s);
  assert.ok(!(await cache.enumerateOwedCandidates(bare, RUN)).some(candidate => candidate.sha === s));
  await fetch(); // Same-S re-observation creates no edge even after independent reconciliation.
  assert.equal(raw(), before);
  const next = git(bare, ...IDENT, "commit-tree", s + "^{tree}", "-p", b, "-m", "another safe bridge");
  assert.equal((await cache.updateRemediationTrackingRef(bare, BRANCH, next, context, source(), s,
    allowTransformation)).kind, "updated");
  await cache.reconcileOwedCandidates(bare, RUN, s);
  const r = commit("ordinary source after repin and reconcile", s); await fetch();
  assert.equal(journal().remediationRecovery.ownedTip, r);
  assert.deepEqual(journal().remediationRecovery.edges, JSON.parse(before).remediationRecovery.edges);
  assert.equal((await cache.readRuntimeRemediationRecovery(bare, BRANCH, source()))?.ownedTip, r);
});

for (const fault of ["unknown ancestry", "missing source anchor"] as const) {
  test("source import after transform: divergent import holds on " + fault, async t => {
    const { s, b, context } = await transformationFixture();
    assert.equal((await cache.updateRemediationTrackingRef(bare, BRANCH, b, context, source(), s,
      allowTransformation)).kind, "updated");
    if (fault === "missing source anchor") await cache.reconcileOwedCandidates(bare, RUN, s);
    const before = raw(), pins = pinnedRefs().split("\n");
    const r = commit("private divergent source");
    if (fault === "unknown ancestry") {
      const internal = cache as any, ancestry = internal.ancestry.bind(cache);
      t.mock.method(internal, "ancestry", async (dir: string, from: string, to: string) =>
        from === s && to === b ? "unknown" : ancestry(dir, from, to));
    }
    await assert.rejects(fetch(), /remediation (tracking does not cover source|source preservation unavailable)/);
    assert.equal(raw(), before);
    assert.equal(git(bare, "rev-parse", "refs/uzi-runner/" + BRANCH), b);
    assert.equal(git(clone, "rev-parse", "HEAD"), r);
    for (const pin of pins) assert.ok(pinnedRefs().split("\n").includes(pin));
    assert.ok((await cache.enumerateOwedCandidates(bare, RUN)).some(candidate => candidate.sha === r));
  });
}

test("new enrollment is exact false; legacy and blocked records are custody only through refresh and import", async () => {
  await enroll();
  const saved = journal();
  assert.equal(saved.remediationRecovery.blocked, false);
  assert.ok(await cache.readRuntimeRemediationRecovery(bare, BRANCH, source()));
  for (const blocked of [undefined, true]) {
    const value = structuredClone(saved);
    if (blocked === undefined) delete value.remediationRecovery.blocked;
    else value.remediationRecovery.blocked = blocked;
    write(value);
    await cache.markRecoveryCapture(bare, clone, BRANCH, RUN, ATTEMPT);
    assert.equal(journal().remediationRecovery.blocked, blocked);
    const fresh = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
    assert.equal(await fresh.readRemediationRecovery(bare, BRANCH, source()), undefined);
    await assert.rejects(fresh.readRuntimeRemediationRecovery(bare, BRANCH, source()));
    await assert.rejects(cache.recordRemediationScan(bare, BRANCH, source(),
      { trusted: true, tip: base, flaggedOIDs: [] }), /custody only/);
    commit("refused import " + String(blocked));
    await assert.rejects(cache.fetchAgentBranch(bare, clone, BRANCH, RUN, opts), /custody only/);
    assert.deepEqual(journal().remediationRecovery, value.remediationRecovery);
    git(clone, "update-ref", "refs/heads/" + BRANCH, base);
  }
});

test("65th trusted flag durably denies the old complete 64-flag body and zero continuation", async () => {
  const flags: string[] = [];
  for (let i = 0; i < 65; i++) flags.push(commit("flag " + i, flags.at(-1) ?? base));
  await fetch();
  const scan = { trusted: true, tip: flags[64]!, flaggedOIDs: flags.slice(0, 64), origin: "pre-exit" as const };
  await cache.recordRemediationScan(bare, BRANCH, source(), scan);
  // Remove the old flagged history, close its grant, then add a new finding.
  // Without durable denial a later zero-finding scan could forget only this Q.
  const clean = commit("clean after 64 findings");
  await fetch();
  await cache.recordRemediationScan(bare, BRANCH, source(),
    { trusted: true, tip: clean, flaggedOIDs: [] });
  const nextFinding = commit("new finding after clean rewrite", clean);
  await fetch();
  const before = journal().remediationRecovery;
  assert.equal(before.blocked, false);
  assert.equal(before.active, undefined);
  assert.equal(before.edges.length, 1);
  for (const flag of before.flaggedOIDs)
    assert.equal(await cache.ancestry(bare, flag, nextFinding), "divergent");
  await assert.rejects(cache.recordRemediationScan(bare, BRANCH, source(),
    { ...scan, tip: nextFinding, flaggedOIDs: [nextFinding] }), /invalid remediation/);
  assert.deepEqual(journal().remediationRecovery, { ...before, blocked: true });
  assert.equal(Buffer.byteLength(JSON.stringify(journal().remediationRecovery)),
    Buffer.byteLength(JSON.stringify(before)) - 1);
  const fresh = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
  assert.equal(await fresh.readRemediationRecovery(bare, BRANCH, source()), undefined);
  await assert.rejects(fresh.readRuntimeRemediationRecovery(bare, BRANCH, source()));
  await assert.rejects(fresh.recordRemediationScan(bare, BRANCH, source(),
    { ...scan, tip: nextFinding, flaggedOIDs: [] }));
  await cache.markRecoveryCapture(bare, clone, BRANCH, RUN, ATTEMPT);
  const { iteration } = await captureAdoption([flags[64]!, nextFinding], nextFinding);
  assert.equal(journal().recovery.stage, "captured", "private capture remains available under denial");
  assert.deepEqual(journal().remediationRecovery, { ...before, blocked: true });
  const pins = pinnedRefs();
  const captured = raw();
  let admissions = 0;
  await assert.rejects(cache.prepareRecoverySuccessor(bare, BRANCH, KEY, source(), iteration,
    successorAttempt, { admitRemediationSource: async () => { admissions++; return true; } }),
    /remediation adoption source unverified/);
  assert.equal(admissions, 0, "a positive scan callback cannot substitute for denied evidence");
  assert.equal(raw(), captured);
  assert.equal(pinnedRefs(), pins);
});

test("negative enrollment and structural confirmation confer no grant, even with prepared or missing evidence", async () => {
  assert.equal(await cache.blockRemediationRecovery(bare, BRANCH, source()), "confirmed");
  assert.deepEqual(journal().remediationRecovery, { version: 1, ...source(), branch: BRANCH,
    sequence: 0, ownedTip: base, flaggedOIDs: [], edges: [], blocked: true });
  assert.equal(await cache.readRemediationRecovery(bare, BRANCH, source()), undefined);
  // Simulate the latest complete body from another producer, then confirm its denial
  // without requiring its positive edge objects or overwriting it with the minimal body.
  write({ ...journal(), remediationRecovery: undefined });
  await enroll();
  const latest = journal();
  latest.remediationRecovery.edges = [{
    from: "a".repeat(40), to: "b".repeat(40), phase: "prepared",
    context: "context-" + "c".repeat(64) + ".json", grant: latest.remediationRecovery.active,
  }];
  write(latest);
  assert.equal(await cache.blockRemediationRecovery(bare, BRANCH, source()), "confirmed");
  assert.deepEqual(journal().remediationRecovery, { ...latest.remediationRecovery, blocked: true });
  assert.equal(await cache.blockRemediationRecovery(bare, BRANCH, source()), "confirmed");
  await assert.rejects(cache.blockRemediationRecovery(bare, BRANCH, { ...source(), runId: "other" }));
});

test("negative persistence failures throw and later confirmation independently re-syncs", async t => {
  await enroll();
  for (const durability of [
    ...(["config", "directory"] as const).map(target => ({
      sync: async (handle: { sync(): Promise<void> }, what: "config" | "directory") => {
        if (what === target) throw Error(target);
        await handle.sync();
      },
    })),
    { readback: async () => "" },
  ]) {
    await assert.rejects(cache.blockRemediationRecovery(bare, BRANCH, source(), durability));
    assert.equal(journal().remediationRecovery.blocked, true);
    assert.equal(await cache.blockRemediationRecovery(bare, BRANCH, source()), "confirmed");
  }
  const internal = cache as any, run = internal.runGit.bind(cache);
  t.mock.method(internal, "runGit", async (dir: string, args: string[]) => {
    if (args[0] === "config" && args.some(a => a.includes('"remediationRecovery"'))) throw Error("deny write");
    return run(dir, args);
  });
  await assert.rejects(cache.blockRemediationRecovery(bare, BRANCH, source()), /deny write/);
});

test("first enrollment flag overflow leaves a minimal durable denial", async () => {
  const flags: string[] = [];
  for (let i = 0; i < 65; i++) flags.push(commit("initial flag " + i, flags.at(-1) ?? base));
  await fetch();
  await assert.rejects(cache.recordRemediationScan(bare, BRANCH, source(),
    { trusted: true, tip: flags[64]!, flaggedOIDs: flags, origin: "pre-exit" }), /invalid remediation/);
  assert.equal(journal().remediationRecovery.blocked, true);
  assert.deepEqual(journal().remediationRecovery.flaggedOIDs, []);
});

test("import edge overflow denies without losing existing flags or edges", async () => {
  commit("flagged before overflow"); await fetch();
  await enroll();
  const saved = journal();
  const r = saved.remediationRecovery;
  r.edges = Array.from({ length: 64 }, (_, i) => ({
    from: (i + 1).toString(16).padStart(40, "0"), to: (i + 2).toString(16).padStart(40, "0"),
    grant: r.active, context: "context-" + "a".repeat(64) + ".json", phase: "committed",
  }));
  write(saved);
  commit("overflow replacement");
  await assert.rejects(cache.fetchAgentBranch(bare, clone, BRANCH, RUN, opts), /invalid remediation/);
  assert.deepEqual(journal().remediationRecovery, { ...r, blocked: true });
  assert.equal(await cache.readRemediationRecovery(bare, BRANCH, source()), undefined);
});

test("import encoded byte overflow durably denies below the edge-count limit", async () => {
  const flags: string[] = [];
  for (let i = 0; i < 64; i++) flags.push(commit("byte flag " + i, flags.at(-1) ?? base));
  await fetch();
  await cache.recordRemediationScan(bare, BRANCH, source(),
    { trusted: true, tip: flags[63]!, flaggedOIDs: flags, origin: "pre-exit" });
  const saved = journal();
  const r = saved.remediationRecovery;
  // Fill with complete bounded grants until one more edge crosses 64KiB.
  // At most 64 attempts; no filesystem work in the loop.
  for (let i = 0; i < 64; i++) {
    const edge = { from: (i + 1).toString(16).padStart(40, "0"), to: (i + 2).toString(16).padStart(40, "0"),
      grant: r.active, context: "context-" + "a".repeat(64) + ".json", phase: "committed" };
    if (Buffer.byteLength(JSON.stringify({ ...r, edges: [...r.edges, edge] })) > 65536) break;
    r.edges.push(edge);
  }
  assert.ok(r.edges.length > 0 && r.edges.length < 64);
  assert.ok(Buffer.byteLength(JSON.stringify(r)) <= 65536);
  write(saved);
  await cache.markRecoveryCapture(bare, clone, BRANCH, RUN, ATTEMPT);
  commit("byte overflow replacement");
  await assert.rejects(cache.fetchAgentBranch(bare, clone, BRANCH, RUN, opts), /invalid remediation/);
  assert.deepEqual(journal().remediationRecovery, { ...r, blocked: true });
});

test("overflow denial durability failure propagates and preserves the full old body", async () => {
  const flags: string[] = [];
  for (let i = 0; i < 65; i++) flags.push(commit("deny sync flag " + i, flags.at(-1) ?? base));
  await fetch();
  await cache.recordRemediationScan(bare, BRANCH, source(),
    { trusted: true, tip: flags[64]!, flaggedOIDs: flags.slice(0, 64), origin: "pre-exit" });
  const before = journal().remediationRecovery;
  await assert.rejects(cache.recordRemediationScan(bare, BRANCH, source(),
    { trusted: true, tip: flags[64]!, flaggedOIDs: [flags[64]!], origin: "pre-exit" },
    { sync: async () => { throw Error("negative sync failed"); } }), /negative sync failed/);
  assert.deepEqual(journal().remediationRecovery, { ...before, blocked: true });
  assert.equal(await cache.blockRemediationRecovery(bare, BRANCH, source()), "confirmed");
});

test("same-source lifecycle writes cannot clear denial or upgrade legacy evidence", async () => {
  await enroll();
  const complete = journal();
  const internal = cache as any;
  // A stale lifecycle snapshot must not overwrite a denial committed while it awaited.
  await cache.blockRemediationRecovery(bare, BRANCH, source());
  const denied = journal();
  await assert.rejects(internal.writeRecovery(bare, BRANCH, complete), /metadata loss/);
  const dropped = structuredClone(denied);
  delete dropped.remediationRecovery.blocked;
  await assert.rejects(internal.writeRecovery(bare, BRANCH, dropped), /metadata loss/);
  assert.deepEqual(journal(), denied);
  const legacy = structuredClone(complete);
  delete legacy.remediationRecovery.blocked;
  write(legacy);
  await assert.rejects(internal.writeRecovery(bare, BRANCH, complete), /metadata loss/);
  assert.deepEqual(journal(), legacy);
  write(complete);
  await assert.rejects(internal.writeRecovery(bare, BRANCH, legacy), /metadata loss/);
  assert.deepEqual(journal(), complete);
});

test("restriction writer: first enrollment retains flags without rewrite or adoption authority", async () => {
  const q = commit("negative enrollment"); await fetch();
  const scan = { trusted: true, tip: q, flaggedOIDs: [q, q] };
  await cache.recordRemediationRestrictions(bare, BRANCH, source(), scan);
  assert.deepEqual(journal().remediationRecovery, { version: 1, ...source(), branch: BRANCH,
    sequence: 0, ownedTip: q, flaggedOIDs: [q], edges: [], blocked: false });
  const replacement = commit("ungranted replacement"); await fetch();
  const evidence = await cache.readRuntimeRemediationRecovery(bare, BRANCH, source());
  assert.equal(evidence?.ownedTip, replacement);
  assert.deepEqual(evidence?.edges, []);
  assert.equal(evidence?.active, undefined);
  assert.deepEqual(evidence?.flaggedOIDs, [q]);
  const { iteration } = await captureAdoption([q, replacement], replacement);
  await refusal(iteration);
});

test("restriction writer: candidate coverage stays independent of source and refuses bad controls", async () => {
  const q = git(bare, ...IDENT, "commit-tree", base + "^{tree}", "-p", base, "-m", "candidate only");
  const c = git(bare, ...IDENT, "commit-tree", base + "^{tree}", "-p", q, "-m", "coverage only");
  const scan = { trusted: true, tip: c, flaggedOIDs: [q] };
  await cache.recordRemediationRestrictions(bare, BRANCH, source(), { ...scan, trusted: false });
  assert.equal(journal().remediationRecovery, undefined);
  for (const wrong of [{ ...source(), runId: "other" }, { ...source(), attemptId: successorAttempt }])
    await assert.rejects(cache.recordRemediationRestrictions(bare, BRANCH, wrong, scan), /source binding/);
  await assert.rejects(cache.recordRemediationRestrictions(bare, BRANCH, source(),
    { ...scan, tip: base }), /not reachable/);
  const saved = journal();
  commit("not yet owned");
  await assert.rejects(cache.recordRemediationRestrictions(bare, BRANCH, source(), scan), /tip not owned/);
  git(clone, "update-ref", "refs/heads/" + BRANCH, base);
  await cache.recordRemediationRestrictions(bare, BRANCH, source(), scan);
  const r = journal().remediationRecovery;
  assert.equal(await cache.ancestry(bare, q, base), "divergent");
  assert.deepEqual(r, { version: 1, ...source(), branch: BRANCH, sequence: 0,
    ownedTip: base, flaggedOIDs: [q], edges: [], blocked: false });
  assert.deepEqual((await cache.readRuntimeRemediationRecovery(bare, BRANCH, source()))?.flaggedOIDs, [q]);
  for (const blocked of [undefined, true]) {
    const held: RemediationRecovery = { ...r, version: 1, blocked };
    write({ ...saved, remediationRecovery: held });
    await assert.rejects(cache.recordRemediationRestrictions(bare, BRANCH, source(), scan), /custody only/);
    assert.deepEqual(journal().remediationRecovery, JSON.parse(JSON.stringify(held)));
  }
});

test("restriction writer: later findings preserve original grant and prepared phases", async () => {
  const oldQ = commit("original checkpoint finding"); await fetch();
  await cache.recordRemediationScan(bare, BRANCH, source(),
    { trusted: true, tip: oldQ, flaggedOIDs: [oldQ], origin: "checkpoint-pending" });
  const d = commit("later finding", oldQ); await fetch();
  const before = journal().remediationRecovery;
  await cache.recordRemediationRestrictions(bare, BRANCH, source(), { trusted: true, tip: d, flaggedOIDs: [d] });
  assert.deepEqual(journal().remediationRecovery, { ...before, flaggedOIDs: [oldQ, d] });
  await cache.recordRemediationRestrictions(bare, BRANCH, source(), { trusted: true, tip: d, flaggedOIDs: [] });
  assert.deepEqual(journal().remediationRecovery.active, before.active);
  const replacement = commit("rewrite under original grant"); await fetch();
  const r = await cache.readRuntimeRemediationRecovery(bare, BRANCH, source());
  assert.deepEqual(r?.active, before.active);
  assert.deepEqual(r?.edges.map(e => ({ from: e.from, to: e.to, grant: e.grant })),
    [{ from: d, to: replacement, grant: before.active }]);
  assert.deepEqual(r?.flaggedOIDs, [oldQ, d]);
  // A separate prepared snapshot must stay prepared when negative evidence arrives.
  const complete = journal();
  const prepared = structuredClone(complete);
  prepared.remediationRecovery.edges[0].phase = "prepared";
  write(prepared);
  const q = git(bare, ...IDENT, "commit-tree", base + "^{tree}", "-p", base, "-m", "later coverage Q");
  await cache.recordRemediationRestrictions(bare, BRANCH, source(), { trusted: true, tip: q, flaggedOIDs: [q] });
  assert.deepEqual(journal().remediationRecovery,
    { ...prepared.remediationRecovery, flaggedOIDs: [oldQ, d, q] });
  write({ ...complete, remediationRecovery: { ...complete.remediationRecovery, flaggedOIDs: [oldQ, d, q] } });
  const unsafe = git(bare, ...IDENT, "commit-tree", base + "^{tree}", "-p", replacement, "-p", q, "-m", "excluded Q");
  assert.equal(await cache.ancestry(bare, oldQ, unsafe), "divergent");
  assert.equal(await cache.ancestry(bare, d, unsafe), "divergent");
  let admissions = 0;
  await assert.rejects(cache.updateRemediationTrackingRef(bare, BRANCH, unsafe,
    { context: opts.context }, source(), replacement,
    { trusted: true, admit: async () => { admissions++; return true; } }), /includes excluded history/);
  assert.equal(admissions, 0);
  const { iteration } = await captureAdoption([d, replacement], replacement);
  await cache.prepareRecoverySuccessor(bare, BRANCH, KEY, source(), iteration, successorAttempt, admitSource);
});

test("restriction writer: failed durability never reports success and new bodies need independent confirmation", async t => {
  await enroll();
  const saved = journal();
  const q = git(bare, ...IDENT, "commit-tree", base + "^{tree}", "-p", base, "-m", "durable negative");
  const scan = { trusted: true, tip: q, flaggedOIDs: [q] };
  for (const durability of [
    ...(["config", "directory"] as const).map(target => ({
      sync: async (handle: { sync(): Promise<void> }, what: "config" | "directory") => {
        if (what === target) throw Error(target);
        await handle.sync();
      },
    })),
    { readback: async () => "" },
  ]) {
    write(saved);
    await assert.rejects(cache.recordRemediationRestrictions(bare, BRANCH, source(), scan, durability));
    const fresh = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
    assert.deepEqual((await fresh.readRuntimeRemediationRecovery(bare, BRANCH, source()))?.flaggedOIDs, [base, q]);
  }
  write(saved);
  const internal = cache as any, run = internal.runGit.bind(cache);
  t.mock.method(internal, "runGit", async (dir: string, args: string[]) => {
    if (args[0] === "config" && args.some(a => a.includes('"remediationRecovery"'))) throw Error("negative write");
    return run(dir, args);
  });
  await assert.rejects(cache.recordRemediationRestrictions(bare, BRANCH, source(), scan), /negative write/);
  assert.deepEqual(journal(), saved);
  assert.deepEqual((await cache.readRuntimeRemediationRecovery(bare, BRANCH, source()))?.flaggedOIDs, [base]);
  await assert.rejects(cache.recordRemediationRestrictions(bare, BRANCH, source(),
    { ...scan, flaggedOIDs: Array.from({ length: 65 }, (_, i) => (i + 1).toString(16).padStart(40, "0")) }),
    /negative write/);
  assert.deepEqual(journal(), saved, "failed denial cannot replace the old complete body");
});

test("restriction writer: bounded overflow retains old metadata and positive overflow skips per-Q validation", async t => {
  await enroll();
  const saved = journal();
  const flags = Array.from({ length: 65 }, (_, i) => (i + 1).toString(16).padStart(40, "0"));
  const internal = cache as any, run = internal.runGit.bind(cache);
  let perFindingCalls = 0;
  t.mock.method(internal, "runGit", async (dir: string, args: string[]) => {
    if (args.includes("cat-file") && flags.includes(args.at(-1)!)) perFindingCalls++;
    return run(dir, args);
  });
  for (const positive of [false, true]) {
    write(saved);
    await assert.rejects(positive
      ? cache.recordRemediationScan(bare, BRANCH, source(),
        { trusted: true, tip: base, flaggedOIDs: flags, origin: "pre-exit" })
      : cache.recordRemediationRestrictions(bare, BRANCH, source(),
        { trusted: true, tip: base, flaggedOIDs: flags }), /invalid remediation/);
    assert.equal(perFindingCalls, 0);
    assert.deepEqual(journal().remediationRecovery, { ...saved.remediationRecovery, blocked: true });
  }
  // Fill a valid old body to six bytes below the UTF-8 ceiling; adding Q must deny,
  // preserving all old grants and edges rather than truncating them.
  const large = structuredClone(saved), r = large.remediationRecovery;
  r.flaggedOIDs = [base, ...flags.slice(0, 62)];
  r.active.flaggedOIDs = [...r.flaggedOIDs];
  for (let i = 0; i < 64; i++) {
    const edge = { from: (i + 100).toString(16).padStart(40, "0"),
      to: (i + 101).toString(16).padStart(40, "0"), phase: "prepared",
      context: "context-" + "a".repeat(64) + ".json",
      grant: { ...r.active, id: i + 2, originSource: { ...source() } } };
    if (Buffer.byteLength(JSON.stringify({ ...r, sequence: i + 2, edges: [...r.edges, edge] })) > 65530) break;
    r.edges.push(edge); r.sequence = i + 2;
  }
  const padding = 65530 - Buffer.byteLength(JSON.stringify(r));
  r.edges.at(-1).grant.originSource.clonePath = clone.replace(".attempt-", "x".repeat(padding) + ".attempt-");
  assert.ok(Buffer.byteLength(r.edges.at(-1).grant.originSource.clonePath) <= 4096);
  assert.equal(Buffer.byteLength(JSON.stringify(r)), 65530);
  write(large);
  await cache.markRecoveryCapture(bare, clone, BRANCH, RUN, ATTEMPT);
  const q = git(bare, ...IDENT, "commit-tree", base + "^{tree}", "-p", base, "-m", "byte overflow Q");
  await assert.rejects(cache.recordRemediationRestrictions(bare, BRANCH, source(),
    { trusted: true, tip: q, flaggedOIDs: [q] }), /invalid remediation/);
  assert.deepEqual(journal().remediationRecovery, { ...r, blocked: true });
});
