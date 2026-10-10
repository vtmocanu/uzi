import { it } from "node:test";
import assert from "node:assert/strict";
import { fx, installHarness } from "./runner-harness.js";
import { reportRetirementFixture } from "./report-retirement-fixture.js";
import { GitCache } from "../src/git.js";
import path from "node:path";
import { noProofReseed, nullLogger, testGitCacheOptions } from "./helpers.js";

installHarness();
it("cancelled exact-generation archived report retires without releasing source custody", async t => {
  const f = await reportRetirementFixture(t);
  assert.equal(f.r.isExecuting(f.claim.run_id), false);
  assert.equal(f.archive.generation, 2);
  assert.equal(f.archive.state, "uploaded");
  assert.equal(f.cmd(f.clone, ["status", "--porcelain"]), "");
  await f.assertPending();
  const protectedGit = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions({
    terminalRecordProtection: id => f.outbox.hasPhysicalTerminalProtection(id),
  }));
  await assert.rejects(protectedGit.retireRunnerClone(f.bare, f.clone, f.context.branch!, f.claim.run_id,
    { discard: true }), /terminal record custody/);
  await f.assertCustody();
  const start = Date.now();
  await f.replay();
  assert.ok(Date.now() - start < 1000, "real retirement fits the one-second proof budget");
  assert.equal(f.calls.sends, 1);
  assert.equal(f.calls.cancelledAcks, 1);
  await f.assertCustody();
  assert.equal(await f.outbox.readTerminalJournal(f.claim.run_id, 2), undefined);
  assert.deepEqual(f.outbox.listPendingTerminals(), []);
  assert.equal(await f.outbox.hasPhysicalTerminalProtection(f.claim.run_id), false);
  await assert.rejects(protectedGit.runnerCloneForBranch(f.bare, f.context.branch!,
    path.basename(f.clone), noProofReseed, f.claim.run_id), /captur.*before reseeding/);
  await f.assertCustody();
  assert.equal(f.outbox.depthFor(f.claim.run_id)?.pendingTerminal ?? 0, 0);
  assert.equal(f.registry.build().active.some(e => e.run_id === f.claim.run_id), false);
  await f.assertCustody();
  assert.equal(await f.r.recoveryInventoryPending(f.claim.run_id, 2), true);
});

it("real thin archive retires exact-generation report with source custody held", async t => {
  const f = await reportRetirementFixture(t, { thinArchive: true });
  const start = Date.now();
  await f.replay();
  assert.ok(Date.now() - start < 1000, "thin retirement fits the one-second proof budget");
  assert.equal(await f.outbox.readTerminalJournal(f.claim.run_id, 2), undefined);
  await f.assertCustody();
});

it("finalize retirement consumes the exact archived report while keeping source custody", async t => {
  const f = await reportRetirementFixture(t);
  await f.replay();
  assert.equal(await f.outbox.readTerminalJournal(f.claim.run_id, 2), undefined);
  assert.equal((await f.outbox.journalFinalize(f.claim.run_id, 2)).written, true);
  const identity = f.outbox.finalizeRecordIdentity(f.claim.run_id, 2)!;
  await f.r.retireRecoveryReport(f.claim.run_id, 2, {
    expectedIdentity: identity, signal: new AbortController().signal, deadline: Date.now() + 1_000,
  }, "finalize", f.outbox);
  assert.equal(f.outbox.finalizeRecordIdentity(f.claim.run_id, 2), undefined);
  await f.assertCustody();
  assert.equal(await f.r.recoveryInventoryPending(f.claim.run_id, 2), true);
});
