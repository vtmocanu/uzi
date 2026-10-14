import assert from "node:assert/strict";
import fs from "node:fs";
import { it } from "node:test";
import { git, installHarness } from "./runner-harness.js";
import { reportRetirementFixture } from "./report-retirement-fixture.js";

installHarness();

it("investigation baseline: physical archive retires with an uncovered historical pending FINAL source", {
  skip: process.platform === "linux" ? false : "physical report proof requires Linux",
}, async t => {
  const f = await reportRetirementFixture(t, {
    olderOpenHold: true, olderSource: false, priorPendingHeldFinal: true,
  });
  const prior = f.priorFinal!;
  assert.equal(prior.generation, 1);
  assert.equal(prior.finalAcknowledged === true, false);
  assert.equal(prior.finalRequest?.disposition.kind, "held_publication");
  if (prior.finalRequest?.disposition.kind !== "held_publication") assert.fail("missing held FINAL");
  assert.equal(prior.finalRequest.disposition.source_sha, prior.sourceSha);
  assert.equal(prior.finalRequest.disposition.coverage_digest, prior.coverageDigest);
  assert.notEqual(prior.sourceSha, prior.inventoryCurrentSha, "FINAL names the real synthetic wrapper");
  assert.equal(await git.ancestry(f.bare, prior.sourceSha, f.archive.sourceSha), "divergent",
    "competing unacknowledged FINAL source is outside the later archive");
  assert.equal(await git.ancestry(f.bare, f.archive.sourceSha, prior.sourceSha), "divergent");
  assert.equal(f.cmd(f.bare, ["rev-parse",
    `refs/uzi-coverage/${f.claim.run_id}/1/${prior.coverageDigest}`]), prior.sourceSha);

  const records = await f.coordinator.inspect(f.claim.run_id);
  assert.equal(records.filter(r => r.generation === f.generation && r.state === "uploaded").length, 1,
    "exactly-one uploaded filter applies only to the report generation");
  for (const record of records) {
    const heads = record.coverageDigest
      ? [record.originalSourceSha!, record.inventoryCurrentSha!, ...record.originalRoots!.map(root => root.sha)]
      : [record.sourceSha];
    for (const head of heads) assert.equal(await git.ancestry(f.bare, head, f.archive.sourceSha), "ancestor",
      "every frozen original/current head and root is covered");
  }
  const owed = await git.enumerateOwedCandidates(f.bare, f.claim.run_id);
  assert.ok(owed.length > 0, "real owed roots remain retained");
  for (const root of owed) {
    assert.equal(await git.ancestry(f.bare, root.sha, f.archive.sourceSha), "ancestor");
    const frozen = f.archive.originalRoots!.find(r => r.sha === root.sha);
    assert.ok(frozen);
    for (const context of root.contexts) assert.ok(frozen.contexts.some(c =>
      JSON.stringify(c) === JSON.stringify(context)), "every owed producer context is frozen");
    assert.notEqual(root.sha, prior.sourceSha, "coverage wrapper is not an owed root");
  }
  const budget = { signal: new AbortController().signal, deadline: Date.now() + 10_000 };
  const physical = await git.withReportProofBudget(budget, () => git.withBareLock(f.bare,
    () => git.readInventoryCloneHeadsUnderLock(f.bare, f.claim.run_id)));
  assert.equal(physical.kind, "verified");
  if (physical.kind !== "verified") assert.fail("physical inventory unavailable");
  assert.ok(physical.clones.some(clone => clone.clonePath === f.clone && clone.head === f.head && clone.identity));
  for (const head of physical.heads) assert.equal(await git.ancestry(f.bare, head, f.archive.sourceSha), "ancestor");
  assert.equal(await git.withReportProofBudget(budget,
    () => git.credentialFreeCancelCleanHead(f.clone, f.bare, f.head)), f.head,
  "real credential-free content proof verifies clean source");
  assert.ok(fs.existsSync(f.archive.bundlePath!));
  assert.equal(await f.coordinator.inventoryCleanupState(f.claim.run_id, 1), "pending");

  const restarted = await f.restartedRunner();
  const freshCoordinator = f.recoveryOf(restarted.r);
  assert.deepEqual(await freshCoordinator.inspect(f.claim.run_id), records,
    "restart parses and MAC-authenticates the production-written competing FINAL");
  const report = await restarted.outbox.readTerminalJournalForRetirement(f.claim.run_id, f.generation);
  assert.ok(report, "restarted Outbox reads the actual pending terminal journal");
  const heldCalls = f.calls.heldFinal;
  let result: Awaited<ReturnType<typeof restarted.r.retireRecoveryReport>> = { retired: false };
  // At most three fresh bounded production proofs; a failure retains custody and does not
  // authorize cleanup. Retries stop at unlink; the node test timeout bounds the whole case.
  for (let attempt = 0; attempt < 3; attempt++) {
    result = await restarted.r.retireRecoveryReport(f.claim.run_id, f.generation, report.context);
    await f.assertCustody();
    if (result.retired) break;
  }
  t.diagnostic("SYNTHETIC PROCESS PROOF: quiesceRun observations only; real physical Git inventory, content inspection and Outbox unlink");
  t.diagnostic(JSON.stringify({ priorFinalSource: prior.sourceSha, archiveSource: f.archive.sourceSha, result }));
  // Explicit test-only desired-contract mode records milestone 1's expected failure without
  // patching production or changing any journal field. Milestone 2 will make refusal unconditional.
  if (process.env.UZI_EXPECT_FINAL_SOURCE_REFUSAL === "1") {
    assert.deepEqual(result, { retired: false }, "desired refusal: uncovered pending FINAL source must retain report");
    assert.ok(await restarted.outbox.readTerminalJournal(f.claim.run_id, f.generation),
      "desired refusal preserves the terminal file");
  } else {
    assert.deepEqual(result, { retired: true, authority: "existing_archive" },
      "investigation baseline: current physical proof omits the historical FINAL source");
    assert.ok(restarted.observations.calls >= 4, "real runner brackets two physical content passes with process observations");
    assert.equal(await restarted.outbox.readTerminalJournal(f.claim.run_id, f.generation), undefined,
      "real Outbox terminal file was unlinked");
    assert.deepEqual(restarted.outbox.listPendingTerminals(), []);
  }
  assert.deepEqual(await freshCoordinator.inspect(f.claim.run_id), records,
    "report-only proof changes no authenticated recovery artifact");
  assert.equal(await freshCoordinator.inventoryCleanupState(f.claim.run_id, 1), "pending");
  assert.equal(f.calls.heldFinal, heldCalls, "proof sends no competing FINAL");
  await f.assertCustody();
});
