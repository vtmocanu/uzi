import { beforeEach, afterEach, test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { GitCache } from "../src/git.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { nullLogger, noProofReseed, testGitCacheOptions } from "./helpers.js";
import { fixtureFetchTracking } from "./runner-tracking-fixture.js";

// issue #4627: legacy discoverRetainedRecovery on a not-blocked journal with a typed bare-identity
// mismatch on the owner record must stay read-only, including the legacy attribution repair (a ledger append).
const branch = "agent/issue-4627";
const key = "issue-4627";
const runId = "run-4627";
const aid = (n: number) => `20261008T171401Z-g${n}-0123456789abcdef`;
let fx: Fixture;
let cache: GitCache;
let bare: string;
let source: { runId: string; clonePath: string };

function git(dir: string, args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], {
    encoding: "utf8", env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" },
    stdio: ["pipe", "pipe", "pipe"],
  }).trim();
}
function journal(): any { return JSON.parse(git(bare, ["config", `uzi-recovery.${branch}.clone`])); }
function recreate(): GitCache { return new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions()); }
const ledger = (): string => { try { return git(bare, ["config", "--get-all", `uzi-attempts.${branch}.entry`]); } catch { return ""; } };
const configBytes = (): string => fs.readFileSync(path.join(bare, "config"), "utf8");

beforeEach(async () => {
  fx = makeFixture();
  cache = recreate();
  bare = await cache.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700);
  fs.chmodSync(bare, 0o700);
  const clone = await cache.createOrAttachRunnerClone(bare, 4627, noProofReseed, runId);
  source = { runId, clonePath: clone.path };
  await cache.markRecoveryCapture(bare, clone.path, branch, runId);
});
afterEach(() => fx.cleanup());

/** A legacy adopting journal: the successor was journaled before its ledger attribution existed. */
async function legacyAdopting() {
  await cache.reserveRecoveryIteration(bare, branch, key, source, 1000, 1, 7, 0);
  fs.writeFileSync(path.join(source.clonePath, "work.txt"), "retained work\n");
  git(source.clonePath, ["add", "work.txt"]);
  git(source.clonePath, ["-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "milestone"]);
  await fixtureFetchTracking(cache, bare, source.clonePath, branch, runId);
  const tip = git(source.clonePath, ["rev-parse", "HEAD"]);
  await cache.recordRecoveryCapture(bare, branch, key, source, 1, tip);
  const successor = { runId, clonePath: source.clonePath + ".attempt-" + aid(2), attemptId: aid(2), restoreTip: tip };
  git(bare, ["config", `uzi-recovery.${branch}.clone`,
    JSON.stringify({ ...journal(), recovery: { ...journal().recovery, successor, stage: "adopting" } })]);
  return { successor };
}
function withMismatch(c: GitCache): GitCache {
  Object.assign(c, { recoveryOwnerBareIdentity: async () => JSON.stringify([bare, "9", "9"]) });
  return c;
}

test("a typed bare-identity mismatch makes legacy discovery refuse read-only instead of repairing the missing attribution", async () => {
  const { successor } = await legacyAdopting();
  assert.equal(await recreate().classifyOwnerClonePath(bare, branch, key, runId, successor.clonePath), undefined);
  const ledgerBefore = ledger(), configBefore = configBytes(), journalBefore = journal();
  await assert.rejects(withMismatch(recreate()).discoverRetainedRecovery(fx.originPath, branch, key, runId),
    /recovery source does not belong to this key/);
  assert.equal(ledger(), ledgerBefore, "no attempt ledger append");
  assert.equal(configBytes(), configBefore, "the whole bare config is byte-identical");
  assert.deepEqual(journal(), journalBefore);
  assert.equal(await recreate().classifyOwnerClonePath(bare, branch, key, runId, successor.clonePath), undefined);
});

test("control: the same journal without the mismatch still repairs the attribution", async () => {
  const { successor } = await legacyAdopting();
  const ledgerBefore = ledger();
  assert.ok((await recreate().discoverRetainedRecovery(fx.originPath, branch, key, runId)));
  assert.notEqual(ledger(), ledgerBefore);
  assert.equal(await recreate().classifyOwnerClonePath(bare, branch, key, runId, successor.clonePath), "attempt");
});
