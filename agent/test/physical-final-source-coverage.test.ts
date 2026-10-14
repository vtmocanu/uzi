import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { RECOVERY_BUNDLE_REF } from "../src/git.js";
import { it } from "node:test";
import { fx, git, installHarness } from "./runner-harness.js";
import { reportRetirementFixture } from "./report-retirement-fixture.js";

installHarness();

for (const coveredFinal of [true, false]) {
  it(coveredFinal
    ? "real competing FINAL covered by SDK merge retires terminal and finalize reports"
    : "real earlier divergent non-FINAL wrapper retires terminal and finalize reports", {
    skip: process.platform === "linux" ? false : "physical source proof requires Linux",
  }, async t => {
    const f = await reportRetirementFixture(t, {
      olderOpenHold: true, olderSource: false, priorPendingArchiveFinal: true,
      coverPriorFinal: coveredFinal, priorArchiveWithoutFinal: !coveredFinal,
    });
    const prior = f.priorFinal!;
    assert.equal(prior.generation, 1);
    assert.notEqual(prior.finalAcknowledged, true);
    assert.equal(prior.finalRequest?.disposition.kind, coveredFinal ? "archive" : undefined);
    assert.notEqual(prior.sourceSha, prior.inventoryCurrentSha, "real synthetic coverage wrapper");
    assert.equal(await git.ancestry(f.bare, prior.sourceSha, f.archive.sourceSha),
      coveredFinal ? "ancestor" : "divergent");
    assert.equal(await git.ancestry(f.bare, f.successorAdmission!.head, f.head), "ancestor");
    assert.deepEqual(prior.originalRoots!.map(root => root.sha).sort(),
      [f.predecessorH1, f.predecessorH2].sort());
    const records = await f.coordinator.inspect(f.claim.run_id);
    assert.deepEqual(records.find(record => record.captureId === prior.captureId), prior);
    assert.equal(records.filter(record => record.generation === 2 && record.state === "uploaded").length, 1);
    for (const head of [prior.originalSourceSha!, prior.inventoryCurrentSha!,
      ...prior.originalRoots!.map(root => root.sha)]) {
      assert.equal(await git.ancestry(f.bare, head, f.archive.sourceSha), "ancestor");
    }
    const restore = f.useHeadroom();
    try {
      const restarted = await f.restartedRunner();
      const coordinator = f.recoveryOf(restarted.r);
      assert.deepEqual(await coordinator.inspect(f.claim.run_id), records);
      const report = await restarted.outbox.readTerminalJournalForRetirement(f.claim.run_id, f.generation);
      assert.ok(report);
      const oldFinalCalls = f.calls.priorArchiveFinal;
      assert.deepEqual(await restarted.r.retireRecoveryReport(f.claim.run_id, f.generation, report.context),
        { retired: true, authority: "existing_archive" });
      assert.equal(await restarted.outbox.readTerminalJournal(f.claim.run_id, f.generation), undefined);
      assert.deepEqual(restarted.outbox.listPendingTerminals(), []);
      assert.ok(restarted.observations.calls >= 4);
      await f.assertCustody();
      // Finalize proof is admitted only after real terminal retirement removes its prohibition.
      assert.equal((await restarted.outbox.journalFinalize(f.claim.run_id, f.generation)).written, true);
      const identity = restarted.outbox.finalizeRecordIdentity(f.claim.run_id, f.generation);
      assert.ok(identity);
      assert.deepEqual(await restarted.r.retireRecoveryReport(f.claim.run_id, f.generation, {
        expectedIdentity: identity, signal: new AbortController().signal,
        deadline: Date.now() + f.headroomMs,
      }, "finalize", restarted.outbox), { retired: true, authority: "existing_archive" });
      assert.equal(restarted.outbox.finalizeRecordIdentity(f.claim.run_id, f.generation), undefined);
      assert.deepEqual(await coordinator.inspect(f.claim.run_id), records);
      assert.equal(f.calls.priorArchiveFinal, oldFinalCalls, "report proofs send no old FINAL");
      await f.assertCustody();
      t.diagnostic("SYNTHETIC SERVER/PROCESS/TIME OBSERVATIONS; real producer, SDK ancestry, archives and Outbox retirement");
      t.diagnostic(JSON.stringify({ events: f.historyEvents, coveredFinal }));
    } finally { restore(); }
  });
}

// Physical source/content proof uses Linux process semantics; other platforms skip explicitly.
it("paused archive FINAL root drift retains the successor report", {
  skip: process.platform === "linux" ? false : "physical source proof requires Linux",
}, async t => {
  const f = await reportRetirementFixture(t, {
    olderOpenHold: true, olderSource: false, priorPendingArchiveFinal: true,
  });
  const prior = f.priorFinal!;
  assert.ok(f.predecessorClone && f.successorAdmission);
  assert.equal(prior.generation, 1);
  assert.notEqual(prior.finalAcknowledged, true);
  assert.equal(prior.finalRequest?.disposition.kind, "archive");
  if (prior.finalRequest?.disposition.kind !== "archive") assert.fail("missing archive FINAL");
  assert.equal(prior.finalRequest.disposition.source_sha, prior.sourceSha);
  assert.equal(prior.finalRequest.disposition.coverage_digest, prior.coverageDigest);
  assert.notEqual(prior.sourceSha, prior.inventoryCurrentSha, "real frozen coverage wrapper");
  assert.notEqual(f.successorAdmission.clone, f.predecessorClone, "fresh admitted physical successor");
  assert.equal(f.successorAdmission.stage, "ready-for-model");
  assert.ok(f.successorAdmission.attemptId);
  assert.equal(f.successorAdmission.restoreTip, f.successorAdmission.head);
  assert.equal(await git.recoveryAttemptMode(fx.originPath, "issue-2652"), true);
  const records = await f.coordinator.inspect(f.claim.run_id);
  const admission = records.find(r => r.generation === 2 && r.coverageDigest &&
    r.sourceSha === f.successorAdmission!.head)!;
  assert.ok(admission, "real predecessor capture provenance");
  assert.notEqual(admission.state, "uploaded", "transient reserve ambiguity grants no uploaded authority");
  assert.equal(admission.finalRequest, undefined);
  assert.ok(await f.coordinator.verifiedLocalCapture(admission), "independently verified local bundle adopted");
  assert.equal(admission.inventoryCurrentSha, prior.inventoryCurrentSha);
  assert.deepEqual(admission.originalRoots, [], "confirmed H1/H2 owed contexts pruned before freeze");
  assert.equal(admission.originalSourceSha, prior.inventoryCurrentSha);
  assert.deepEqual(prior.originalRoots!.map(root => root.sha).sort(),
    [f.predecessorH1, f.predecessorH2].sort());
  assert.deepEqual(records.find(record => record.captureId === prior.captureId), prior,
    "checkpoint reconciliation did not rewrite the already-frozen G1 journal");
  assert.notEqual(admission.coverageDigest, prior.coverageDigest, "confirmation pruned old owed roots before freeze");
  assert.equal(records.filter(r => r.generation === 2 && r.state === "uploaded").length, 1);
  assert.equal(await git.ancestry(f.bare, prior.sourceSha, f.archive.sourceSha), "divergent",
    "pending old FINAL wrapper is outside actual successor archive");
  assert.equal(await git.ancestry(f.bare, prior.sourceSha, f.successorAdmission.head), "divergent");
  assert.equal(await git.ancestry(f.bare, f.successorAdmission.head, f.head), "ancestor",
    "H3 committed during actual admitted SDK flight");
  const retained = await git.discoverRetainedRecovery(fx.originPath, f.context.branch, "issue-2652", f.claim.run_id);
  assert.ok(retained);
  assert.equal(retained.journal.clonePath, f.clone);
  assert.ok(retained.journal.retainedSources?.some(source => source.clonePath === f.predecessorClone));
  assert.equal(f.cmd(f.predecessorClone, ["rev-parse", "HEAD"]), prior.inventoryCurrentSha);
  for (const record of records) {
    const heads = record.coverageDigest
      ? [record.originalSourceSha!, record.inventoryCurrentSha!, ...record.originalRoots!.map(r => r.sha)]
      : [record.sourceSha];
    for (const head of heads) assert.equal(await git.ancestry(f.bare, head, f.archive.sourceSha), "ancestor");
  }
  const owed = await git.enumerateOwedCandidates(f.bare, f.claim.run_id);
  assert.ok(owed.length);
  for (const root of owed) {
    assert.equal(await git.ancestry(f.bare, root.sha, f.archive.sourceSha), "ancestor");
    const frozen = f.archive.originalRoots!.find(r => r.sha === root.sha);
    assert.ok(frozen);
    for (const context of root.contexts) assert.ok(frozen.contexts.some(c =>
      JSON.stringify(c) === JSON.stringify(context)), "all owed contexts frozen");
  }
  const restore = f.useHeadroom();
  try {
    const budget = { signal: new AbortController().signal, deadline: Date.now() + f.headroomMs };
    const physical = await git.withReportProofBudget(budget, () => git.withBareLock(f.bare,
      () => git.readInventoryCloneHeadsUnderLock(f.bare, f.claim.run_id)));
    assert.equal(physical.kind, "verified");
    if (physical.kind !== "verified") assert.fail("physical inventory unavailable");
    for (const clone of [f.predecessorClone, f.clone]) {
      assert.ok(physical.clones.some(source => source.clonePath === clone && source.identity));
    }
    for (const head of physical.heads) assert.equal(await git.ancestry(f.bare, head, f.archive.sourceSha), "ancestor");
    assert.equal(await git.withReportProofBudget(budget,
      () => git.credentialFreeCancelCleanHead(f.clone, f.bare, f.head)), f.head);
    assert.ok(fs.existsSync(f.archive.bundlePath!));
    assert.equal(await git.withReportProofBudget(budget,
      () => git.credentialFreeCancelCleanHead(f.predecessorClone!, f.bare, f.predecessorH2)), f.predecessorH2);
    const fresh = path.join(fx.dataDir, "physical-final-import");
    f.cmd(fx.dataDir, ["clone", "--no-local", fx.originPath, fresh]);
    assert.throws(() => f.cmd(fresh, ["cat-file", "-e", f.head]), "H3 absent on public forge");
    f.cmd(fresh, ["bundle", "verify", f.archive.bundlePath!]);
    f.cmd(fresh, ["fetch", f.archive.bundlePath!, RECOVERY_BUNDLE_REF + ":refs/heads/recovered"]);
    for (const file of ["predecessor-H1.txt", "predecessor-H2.txt", "unpublished.txt"]) {
      assert.equal(f.cmd(fresh, ["show", "recovered:" + file]), file);
    }
    for (const head of [f.predecessorH1, f.predecessorH2, f.head]) {
      f.cmd(fresh, ["merge-base", "--is-ancestor", head, "recovered"]);
    }
    assert.equal((await f.outbox.readTerminalJournal(f.claim.run_id, 2))?.body.status, "failed",
      "G2 execute wrote the real terminal report");
    assert.equal(await f.coordinator.inventoryCleanupState(f.claim.run_id, 1), "pending");
    await f.assertCustody();
    const restarted = await f.restartedRunner();
    const freshCoordinator = f.recoveryOf(restarted.r);
    assert.deepEqual(await freshCoordinator.inspect(f.claim.run_id), records);
    const report = await restarted.outbox.readTerminalJournalForRetirement(f.claim.run_id, f.generation);
    assert.ok(report);
    const oldFinalCalls = f.calls.priorArchiveFinal;
    const result = await restarted.r.retireRecoveryReport(f.claim.run_id, f.generation, report.context);
    t.diagnostic("SYNTHETIC SERVER/PROCESS OBSERVATIONS; real paused producer, source boundary, admission, bundles, physical inventory and Outbox retention");
    t.diagnostic(JSON.stringify({ events: f.historyEvents, oldFinal: prior.sourceSha,
      admitted: f.successorAdmission.head, archive: f.archive.sourceSha, result }));
    assert.deepEqual(result, { retired: false }, "uncovered pending FINAL must retain report");
    assert.ok(await restarted.outbox.readTerminalJournal(f.claim.run_id, f.generation));
    assert.deepEqual(restarted.outbox.listPendingTerminals().map(p => [p.run_id, p.claim_generation]),
      [[f.claim.run_id, f.generation]]);
    assert.deepEqual(await freshCoordinator.inspect(f.claim.run_id), records);
    assert.equal(f.calls.priorArchiveFinal, oldFinalCalls, "report proof sends no old FINAL");
    await f.assertCustody();
  } finally { restore(); }
});
