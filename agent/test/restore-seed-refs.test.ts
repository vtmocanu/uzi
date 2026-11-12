import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { GitCache, type IndependentSeedInput, type OwedCandidate, type PositiveOwedCandidateContext } from "../src/git.js";
import { restoreDigest, restoreProtects, restoreRefProtection, restoreTombstoneJson, restoreJournalKey } from "../src/restore-journal.js";
import { attemptClonePath, formatAttemptId } from "../src/attempt-path.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { noProofReseed, nullLogger, testGitCacheOptions } from "./helpers.js";

// issue #3644 Gate B (plan 3.7): while a restore entry or tombstone of a run exists, every deleter of refs/uzi-owed/,
// refs/uzi-coverage/ and refs/uzi-recovery-pin/ keeps the predecessor's generations (<= G) and its pin SHA; S's own later
// generations stay deletable; and once the exit cleared the entry everything is deletable again. Real Git, real seed.

function scrubGitEnv(base: NodeJS.ProcessEnv): NodeJS.ProcessEnv {
  const env: NodeJS.ProcessEnv = {};
  for (const [key, value] of Object.entries(base)) if (!key.startsWith("GIT_")) env[key] = value;
  return { ...env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
}
const ENV = scrubGitEnv(process.env);
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];
const BRANCH = "agent/issue-3644";
const KEY = "issue-3644";
const RUN = "run-3644";
const P_ID = formatAttemptId(new Date("2026-11-05T10:10:10Z"), 1, "0123456789abcdef");
const S_ID = formatAttemptId(new Date("2026-11-05T11:11:11Z"), 2, "fedcba9876543210");
const P_KEY = `uzi-recovery.${BRANCH}.clone`;
const S_KEY = restoreJournalKey(BRANCH);
const LEDGER_KEY = `uzi-attempts.${BRANCH}.entry`;

let fx: Fixture;
let cache: GitCache;
let bare: string;
let P_PATH: string;
let S_PATH: string;
let pRaw: string;
let pinSha: string;
let baseSha: string;
let childSha: string;
let context: PositiveOwedCandidateContext;

const gitIn = (dir: string, args: string[]): string =>
  execFileSync("git", ["-C", dir, ...args], { encoding: "utf8", env: ENV, stdio: ["pipe", "pipe", "pipe"] }).trim();
const refs = (prefix: string): string[] => gitIn(bare, ["for-each-ref", "--format=%(refname) %(objectname)", prefix]).split("\n").filter(Boolean);
const refSha = (ref: string): string | undefined => { try { return gitIn(bare, ["rev-parse", "--verify", "--quiet", ref]) || undefined; } catch { return undefined; } };
const ownedFiles = (): string[] => fs.readdirSync(path.join(bare, "uzi-owed")).sort();

async function seed(): Promise<void> {
  const input: IndependentSeedInput = {
    barePath: bare, branch: BRANCH, key: KEY, runId: RUN, attemptId: S_ID,
    predecessor: { clonePath: P_PATH, attemptId: P_ID, generation: 1, journalDigest: restoreDigest(pRaw), slotOwner: { worker: "w1", epoch: "e1" }, root: { dev: "2049", ino: "131072" } },
    boundary: { predecessor: "c".repeat(64), successor: "d".repeat(64) },
    pin: { captureId: "capture-1", generation: 1, sourceSha: pinSha, finalization: true }, defaultBranch: "main",
    predecessorUnchanged: async () => true, census: async () => ({ ok: true }), beforeRestoreAttribution: async () => {},
    isLive: () => false, beforeSeed: async () => {},
  };
  await cache.seedIndependentAttempt(input);
}

beforeEach(async () => {
  fx = makeFixture();
  cache = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({
    authority: { admitSourceUse: async () => {}, beginBareMutation: async () => ({ settle: async () => {} }), assertBareClear: async () => {}, beginDestructive: async () => ({ settle: async () => {} }) },
  }));
  bare = await cache.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700);
  fs.chmodSync(bare, 0o700);
  baseSha = gitIn(bare, ["rev-parse", "refs/remotes/origin/main"]);
  const fixtureClone = (await cache.runnerCloneForBranch(bare, BRANCH, "issue-fixture", noProofReseed, RUN)).path;
  gitIn(fixtureClone, [...IDENT, "commit", "--allow-empty", "-m", "the predecessor's saved work"]);
  pinSha = gitIn(fixtureClone, ["rev-parse", "HEAD"]);
  context = { runId: RUN, generation: 1, branch: BRANCH, kind: "issue", barePath: bare, defaultIdentity: { ref: "refs/remotes/origin/main", sha: baseSha } };
  assert.equal((await cache.fetchAgentBranch(bare, fixtureClone, BRANCH, RUN, { context })).kind, "updated");
  // S's later work publishes a descendant of P's SHA.
  gitIn(fixtureClone, [...IDENT, "commit", "--allow-empty", "-m", "the successor's work"]);
  childSha = gitIn(fixtureClone, ["rev-parse", "HEAD"]);
  gitIn(bare, ["fetch", "-q", fixtureClone, `HEAD:refs/uzi-incoming/child`]);
  gitIn(bare, ["update-ref", "-d", "refs/uzi-incoming/child"]);
  const canonical = cache.runnerClonePath(bare, KEY);
  P_PATH = attemptClonePath(canonical, P_ID);
  S_PATH = attemptClonePath(canonical, S_ID);
  pRaw = JSON.stringify({ runId: RUN, clonePath: P_PATH, attemptId: P_ID });
  gitIn(bare, ["config", P_KEY, pRaw]);
  gitIn(bare, ["config", "--add", LEDGER_KEY, JSON.stringify({ attemptId: P_ID, runId: RUN, clonePath: P_PATH, state: "live" })]);
  fs.mkdirSync(P_PATH, { recursive: true });
});
afterEach(() => fx.cleanup());

const owed = (): Promise<OwedCandidate[]> => cache.enumerateOwedCandidates(bare, RUN);

describe("the pure fold of restore values into protection", () => {
  const G3 = formatAttemptId(new Date("2026-11-05T10:10:10Z"), 3, "0123456789abcdef");
  const v = (stage: string, run = RUN): string => JSON.stringify({
    restore: { version: 1, predecessor: { clonePath: `/x/issue-3644.attempt-${G3}`, attemptId: G3, generation: 3, root: { dev: "2049", ino: "131072" } }, journalDigest: "a".repeat(64), boundary: { predecessor: "c".repeat(64), successor: "d".repeat(64) },
      slotOwner: { worker: "w", epoch: "e" }, pin: { captureId: "c", generation: 3, sourceSha: "b".repeat(40), finalization: true }, restoreTip: "b".repeat(40) },
    restoreStage: stage, ...(stage === "retired" ? { successor: { runId: run, attemptId: S_ID, clonePath: "/x/y." + S_ID } } : { runId: run, clonePath: "/x/issue-3644.attempt-" + S_ID, attemptId: S_ID }),
  });
  it("keeps generations at or below the predecessor's and the pin SHA, for its own run only", () => {
    const p = restoreRefProtection([v("ready"), ""]);
    assert.equal(restoreProtects(p, RUN, { generation: 3 }), true);
    assert.equal(restoreProtects(p, RUN, { generation: 1 }), true);
    assert.equal(restoreProtects(p, RUN, { generation: 4 }), false);
    assert.equal(restoreProtects(p, RUN, { generation: null }), false);
    assert.equal(restoreProtects(p, RUN, { sha: "b".repeat(40) }), true);
    assert.equal(restoreProtects(p, RUN, { sha: "c".repeat(40) }), false);
    assert.equal(restoreProtects(p, "other-run", { generation: 1, sha: "b".repeat(40) }), false);
    assert.equal(restoreProtects(restoreRefProtection([v("retired")]), RUN, { generation: 2 }), true, "a tombstone protects like an entry");
    assert.equal(restoreProtects(restoreRefProtection([""]), RUN, { generation: 1 }), false, "a cleared key protects nothing");
  });
  it("an undecodable value names no run and therefore protects every run", () => {
    for (const bad of ["x", "{}", JSON.stringify({ restore: {}, restoreStage: "ready" }), "\0"]) {
      const p = restoreRefProtection([bad]);
      assert.equal(restoreProtects(p, "any-run", { generation: 99 }), true, bad);
    }
  });
});

describe("the deleters keep the restored predecessor's refs", () => {
  it("control: without a restore entry the pin of generation 1 and the owed pin are deleted", async () => {
    gitIn(bare, ["update-ref", `refs/uzi-recovery-pin/${RUN}/1`, pinSha]);
    await cache.deleteRecoveryPin(bare, RUN, 1);
    assert.equal(refSha(`refs/uzi-recovery-pin/${RUN}/1`), undefined);
    const result = await cache.reconcileOwedCandidates(bare, RUN, pinSha);
    assert.deepEqual(result.removedShas, [pinSha]);
    assert.equal(refSha(`refs/uzi-owed/${RUN}/${pinSha}`), undefined);
  });

  it("deleteRecoveryPin (no sha and with a sha) keeps generation <= G and still deletes S's generation", async () => {
    await seed();
    gitIn(bare, ["update-ref", `refs/uzi-recovery-pin/${RUN}/1`, pinSha]);
    gitIn(bare, ["update-ref", `refs/uzi-recovery-pin/${RUN}/2`, pinSha]);
    await cache.deleteRecoveryPin(bare, RUN, 1);
    await cache.deleteRecoveryPin(bare, RUN, 1, pinSha);
    assert.equal(refSha(`refs/uzi-recovery-pin/${RUN}/1`), pinSha, "P's pin is kept");
    await cache.deleteRecoveryPin(bare, RUN, 2);
    assert.equal(refSha(`refs/uzi-recovery-pin/${RUN}/2`), undefined, "S's own generation stays deletable");
  });

  it("cleanupArchivedRecoveryPin retains P's pin ineligible, whichever tombstone/entry state", async () => {
    await seed();
    gitIn(bare, ["update-ref", `refs/uzi-recovery-pin/${RUN}/1`, pinSha]);
    const result = await cache.cleanupArchivedRecoveryPin({ runId: RUN, generation: 1, sourceSha: pinSha, recoveryPinBareDir: path.basename(bare), isEligible: () => true });
    assert.deepEqual(result, { state: "retained", reason: "ineligible" });
    assert.equal(refSha(`refs/uzi-recovery-pin/${RUN}/1`), pinSha);
    // Another run's pin on the same bare is not held by this run's entry.
    gitIn(bare, ["update-ref", `refs/uzi-recovery-pin/other-run/1`, pinSha]);
    const other = await cache.cleanupArchivedRecoveryPin({ runId: "other-run", generation: 1, sourceSha: pinSha, recoveryPinBareDir: path.basename(bare), isEligible: () => true });
    assert.equal(other.state, "deleted");
  });

  it("reconcileOwedCandidates keeps the pin and its metadata when S's publication covers P's SHA", async () => {
    await seed();
    const filesBefore = ownedFiles();
    const result = await cache.reconcileOwedCandidates(bare, RUN, childSha);
    assert.deepEqual(result.removedShas, []);
    assert.deepEqual(result.retainedShas, [pinSha]);
    assert.equal(refSha(`refs/uzi-owed/${RUN}/${pinSha}`), pinSha);
    assert.deepEqual(ownedFiles(), filesBefore);
    assert.equal((await owed()).length, 1);
  });

  it("cleanupRecoveryGeneration retains every cleanup of generation <= G, and the exact-sha candidate of a later one", async () => {
    await seed();
    const candidates = await owed();
    const roots = candidates.map(c => ({ sha: c.sha, contexts: c.contexts }));
    const before = refs("refs/uzi-owed/");
    assert.equal(await cache.cleanupRecoveryGeneration(bare, context, roots, [], [], childSha), "retained");
    assert.equal(await cache.cleanupRecoveryGeneration(bare, { ...context, generation: 1 }, roots, [], [], childSha, () => true, "completion-check"), "retained");
    assert.deepEqual(refs("refs/uzi-owed/"), before);
    assert.deepEqual(ownedFiles().filter(n => n.startsWith("candidate-")).length, 1);
  });

  it("the public predicate answers for callers that delete by another route (recovery.ts)", async () => {
    assert.equal(await cache.restoreKeepsRecoveryRef(bare, RUN, { generation: 1 }), false);
    await seed();
    assert.equal(await cache.restoreKeepsRecoveryRef(bare, RUN, { generation: 1 }), true);
    assert.equal(await cache.restoreKeepsRecoveryRef(bare, RUN, { sha: pinSha }), true);
    assert.equal(await cache.restoreKeepsRecoveryRef(bare, RUN, { generation: 2 }), false);
    assert.equal(await cache.restoreKeepsRecoveryRef(bare, "other-run", { generation: 1, sha: pinSha }), false);
  });

  it("a tombstone protects like an entry, and the exit (entry cleared) releases everything", async () => {
    await seed();
    fs.rmSync(S_PATH, { recursive: true, force: true });
    gitIn(bare, ["config", "--add", LEDGER_KEY, JSON.stringify({ attemptId: S_ID, runId: RUN, clonePath: S_PATH, state: "retired" })]);
    await cache.advanceRestoreEntry(bare, BRANCH, "retired");
    const tombstone = gitIn(bare, ["config", "--get", S_KEY]);
    assert.equal(tombstone, restoreTombstoneJson(JSON.parse(tombstone).restore, JSON.parse(tombstone).successor));
    const kept = await cache.reconcileOwedCandidates(bare, RUN, childSha);
    assert.deepEqual(kept.removedShas, []);
    // P's guarded discard clears both keys together; afterwards S's publication reconciles P's pin away.
    gitIn(bare, ["config", S_KEY, ""]);
    gitIn(bare, ["config", P_KEY, ""]);
    const released = await cache.reconcileOwedCandidates(bare, RUN, childSha);
    assert.deepEqual(released.removedShas, [pinSha]);
  });

  it("a valueless restore key is undecodable for every deleter, cleanupArchivedRecoveryPin included", async () => {
    fs.appendFileSync(path.join(bare, "config"), `[uzi-restore "${BRANCH}"]\n\tjournal\n`);
    assert.equal(gitIn(bare, ["config", "--local", "--null", "--list"]).split("\0").some(i => i === S_KEY), true, "the key has no value");
    assert.equal(await cache.restoreKeepsRecoveryRef(bare, "other-run", { generation: 9 }), true);
    gitIn(bare, ["update-ref", `refs/uzi-recovery-pin/other-run/1`, pinSha]);
    const result = await cache.cleanupArchivedRecoveryPin({ runId: "other-run", generation: 1, sourceSha: pinSha, recoveryPinBareDir: path.basename(bare), isEligible: () => true });
    assert.deepEqual(result, { state: "retained", reason: "ineligible" });
    assert.equal(refSha(`refs/uzi-recovery-pin/other-run/1`), pinSha);
  });

  it("an unreadable restore value keeps refs of every run", async () => {
    gitIn(bare, ["config", S_KEY, "not a restore value"]);
    const kept = await cache.reconcileOwedCandidates(bare, RUN, childSha);
    assert.deepEqual(kept.removedShas, []);
    gitIn(bare, ["update-ref", `refs/uzi-recovery-pin/other-run/9`, pinSha]);
    await cache.deleteRecoveryPin(bare, "other-run", 9);
    assert.equal(refSha(`refs/uzi-recovery-pin/other-run/9`), pinSha);
  });
});
