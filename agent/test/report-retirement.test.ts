import { it } from "node:test";
import assert from "node:assert/strict";
import { fx, installHarness } from "./runner-harness.js";
import { reportRetirementFixture } from "./report-retirement-fixture.js";
import { GitCache } from "../src/git.js";
import path from "node:path";
import { noProofReseed, nullLogger, testGitCacheOptions } from "./helpers.js";

installHarness();
// Archive report retirement needs Linux procfs/no-follow content inspection.
// Other hosts deliberately retain reports; portable checks still run below.
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
  // The logical one-second budget includes cancellation; actual settlement can
  // exceed 1000ms. Fresh attempts retain that deadline rather than extending it.
  if (process.platform !== "linux") {
    await f.replay();
    await f.assertPending();
    return;
  }
  const attempts = await f.replayPositive();
  assert.equal(f.calls.sends, attempts);
  assert.equal(f.calls.cancelledAcks, attempts);
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
  if (process.platform !== "linux") {
    await f.replay();
    await f.assertPending();
    return;
  }
  await f.replayPositive();
  assert.equal(await f.outbox.readTerminalJournal(f.claim.run_id, 2), undefined);
  await f.assertCustody();
});

it("finalize retirement consumes the exact archived report while keeping source custody", async t => {
  const f = await reportRetirementFixture(t);
  if (process.platform !== "linux") {
    await f.replay();
    await f.assertPending();
    await f.outbox.journalFinalize(f.claim.run_id, 2);
    const identity = f.outbox.finalizeRecordIdentity(f.claim.run_id, 2)!;
    await f.r.retireRecoveryReport(f.claim.run_id, 2, {
      expectedIdentity: identity, signal: new AbortController().signal, deadline: Date.now() + 1_000,
    }, "finalize", f.outbox);
    await f.assertFinalizePending();
    return;
  }
  await f.replayPositive();
  assert.equal(await f.outbox.readTerminalJournal(f.claim.run_id, 2), undefined);
  assert.equal((await f.outbox.journalFinalize(f.claim.run_id, 2)).written, true);
  const identity = f.outbox.finalizeRecordIdentity(f.claim.run_id, 2)!;
  await f.positiveStage("finalize retirement", () => f.r.retireRecoveryReport(f.claim.run_id, 2, {
    expectedIdentity: identity, signal: new AbortController().signal, deadline: Date.now() + f.headroomMs,
  }, "finalize", f.outbox), () => !f.outbox.finalizeRecordIdentity(f.claim.run_id, 2), f.assertFinalizePending);
  assert.equal(f.outbox.finalizeRecordIdentity(f.claim.run_id, 2), undefined);
  await f.assertCustody();
  assert.equal(await f.r.recoveryInventoryPending(f.claim.run_id, 2), true);
});

// Without real waiting, the pre-proof path of the next retireRecoveryReport advances an offset
// clock by `jumpMs`. Restores both mocks; call it in a finally before any later replay.
function stallBeforeProof(t: Parameters<typeof reportRetirementFixture>[0],
  f: Awaited<ReturnType<typeof reportRetirementFixture>>, jumpMs: number) {
  let offset = 0;
  const realNow = Date.now.bind(Date);
  const clock = t.mock.method(Date, "now", () => realNow() + offset);
  const reserve = f.r as unknown as { withReportSourceReservation: (id: string, action: () => Promise<void>) => Promise<void> };
  const realReserve = reserve.withReportSourceReservation.bind(f.r);
  const stall = t.mock.method(reserve, "withReportSourceReservation", (id: string, action: () => Promise<void>) => {
    offset = jumpMs;
    return realReserve(id, action);
  });
  return () => { offset = 0; stall.mock.restore(); clock.mock.restore(); };
}

it("fresh positive replay succeeds after a deterministic first proof budget expiry", async t => {
  const f = await reportRetirementFixture(t);
  // A 1.5 s pre-boundary stall exceeds the production 1 s budget but not the headroom budget: the
  // proof still reaches its boundary, and only the hand-fired production deadline expires it.
  const unstall = stallBeforeProof(t, f, 1_500);
  const expiry = f.expireNextProof();
  try { await f.replay(); } finally { unstall(); expiry.restore(); }
  assert.equal(expiry.observation.reached, true, "the proof reached its first budgeted boundary");
  assert.ok(expiry.observation.timersCaptured >= 1, "production registered its proof-deadline timer");
  assert.equal(expiry.observation.abortedBefore, false, "the proof signal was live before the deadline callback fired");
  assert.equal(expiry.observation.aborted, true, "the production deadline abort fired");
  assert.equal(expiry.observation.reason, "report proof deadline", "aborted by the production deadline, not an earlier abort");
  assert.equal(f.calls.cancelledAcks, 1);
  await f.assertPending();
  if (process.platform !== "linux") {
    await f.replay();
    assert.equal(f.calls.cancelledAcks, 2);
    await f.assertPending();
    return;
  }
  const attempts = await f.replayPositive();
  assert.equal(f.calls.cancelledAcks, 1 + attempts);
  assert.equal(f.outbox.hasPendingTerminal(f.claim.run_id, 2), false);
  await f.assertCustody();
});

it("a proof that lapses before the expiry boundary reports reached=false", async t => {
  const f = await reportRetirementFixture(t);
  // The jump exceeds even the headroom budget, so the proof refuses before it calls the hook.
  const unstall = stallBeforeProof(t, f, 120_000);
  const expiry = f.expireNextProof();
  try { await f.replay(); } finally { unstall(); expiry.restore(); }
  assert.equal(expiry.observation.reached, false, "the missed boundary is reported, not silently passed");
  assert.equal(expiry.observation.aborted, false);
  assert.equal(f.calls.cancelledAcks, 1);
  await f.assertPending();
  if (process.platform !== "linux") return;
  const attempts = await f.replayPositive();
  assert.equal(f.calls.cancelledAcks, 1 + attempts);
  assert.equal(f.outbox.hasPendingTerminal(f.claim.run_id, 2), false);
  await f.assertCustody();
});

it("positive replay fails diagnostically after three persistent refusals", async t => {
  const f = await reportRetirementFixture(t);
  f.capture.mode = "throw";
  await assert.rejects(f.replayPositive(), /terminal retirement: persistent refusal after 3 fresh bounded attempts/);
  assert.equal(f.calls.cancelledAcks, 3);
  await f.assertPending();
});
