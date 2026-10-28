import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";
import { test } from "node:test";
import { GitCache } from "../src/git.js";
import { makeFixture } from "./fixture-repo.js";
import { fixtureFetchTracking } from "./runner-tracking-fixture.js";
import { noProofReseed, nullLogger, testGitCacheOptions } from "./helpers.js";

// Deferred proof harness: tester supplies an isolated git-archive export of f265e42d
// under .uzi/scratch and links its agent/node_modules to the existing installed tree.
// Run this file with UZI_M2_OLD_EXPORT=<absolute export path>. No extraction, installs,
// copied parser, or simulated old worker is performed by this harness.
const supplied = process.env.UZI_M2_OLD_EXPORT;
test("actual base worker parses new accounting inventory/budget journals and refuses blocked adoption", {
  skip: supplied ? false : "tester owns isolated actual-base rollback proof after commit",
}, async t => {
  const root = fileURLToPath(new URL("../../", import.meta.url));
  const scratch = fs.realpathSync(path.join(root, ".uzi/scratch"));
  const exported = fs.realpathSync(supplied!);
  assert.ok(exported.startsWith(scratch + path.sep), "old implementation must come from an isolated scratch export");
  const oldGitModule = await import(pathToFileURL(path.join(exported, "agent/src/git.ts")).href);
  const oldProgress = await import(pathToFileURL(path.join(exported, "agent/src/recovery-progress.ts")).href);
  const oldRunnerModule = await import(pathToFileURL(path.join(exported, "agent/src/runner.ts")).href);
  const fx = makeFixture(); t.after(() => fx.cleanup());
  const current = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
  const bare = await current.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700); fs.chmodSync(bare, 0o700);
  const runId = "owner-rollback", key = "issue-3527";
  const clone = await current.createOrAttachRunnerClone(bare, 3527, noProofReseed, runId);
  const source = { runId, clonePath: clone.path }, branch = clone.branch;
  await current.markRecoveryCapture(bare, clone.path, branch, runId);
  const reservation = await current.reserveRecoveryIteration(bare, branch, key, source, 1, 1, 7, 0);
  await fixtureFetchTracking(current, bare, clone.path, branch, runId);
  const old = new oldGitModule.GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
  // readRecoveryCapture invokes the actual private journal parser from the old git.ts.
  const parsed = await old.readRecoveryCapture(bare, branch);
  assert.deepEqual(oldProgress.recoveryProgress(parsed.recovery), reservation);
  assert.equal((await old.readInventoryCloneHeads(bare, runId)).kind, "verified");
  assert.ok(await old.discoverRetainedRecovery(fx.originPath, branch, key, runId));
  await current.blockRecoveryEpisode(bare, branch, key, source, "adoption_failed", reservation);
  const blocked = (await old.discoverRetainedRecovery(fx.originPath, branch, key, runId)).journal;
  let terminal = 0, effects = 0;
  const runner = Object.assign(Object.create(oldRunnerModule.RunRunner.prototype), {
    git: old, client: { protocolFeatures: ["recovery_cause_retained_recovery_exhausted"] },
    cloneCoordinates: () => ({ branch }), codexBoundaryDeadlineMs: 1000,
    retainedLifecycleGuard: () => {}, retainedLifecycleSignal: () => new AbortController().signal,
    captureRecoveryRestorePoint: async () => { effects++; throw new Error("old worker must not capture adoption blocker"); },
    makeExecutor: () => { effects++; throw new Error("old worker must not admit model"); },
    reportGenericFailure: async () => { terminal++; },
  });
  await assert.rejects(runner.recoverRetainedClaim({ run_id: runId, worker_recovery_episode: 8,
    worker_recovery_owner_episode: 1 }, { barePath: bare, runId, claimGeneration: 2, runLog: nullLogger() }, key, blocked));
  assert.equal(effects, 0); assert.equal(terminal, 1);
  assert.equal((await old.readRecoveryCapture(bare, branch)).recovery.blocker, "adoption_failed");
  // A separate old budget journal remains usable despite the accounting-only namespace.
  // Explicit owner disposition retires the first journal; no paths are deleted.
  await current.discardRetainedRecovery(bare, branch, key, source, true);
  await current.markRecoveryCapture(bare, clone.path, branch, runId);
  for (let i = 0; i < 3; i++) await old.reserveRecoveryIteration(bare, branch, key, source, 1, 2, 7);
  await assert.rejects(old.reserveRecoveryIteration(bare, branch, key, source, 1, 2, 7), /budget/);
  assert.equal((await old.reserveRecoveryIteration(bare, branch, key, source, 1, 3, 8)).attempts, 1);
});
