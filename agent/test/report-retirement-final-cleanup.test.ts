import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";
import { installHarness, client, fx } from "./runner-harness.js";
import { reportRetirementFixture } from "./report-retirement-fixture.js";
import { makeTerminalOutboxDeps, resolvePendingTerminal } from "../src/terminal-resolve.js";
import { nullLogger } from "./helpers.js";
import { Worker } from "../src/worker.js";
import { reportProofTiming } from "../src/report-proof-timing.js";

installHarness();
const linux = { skip: process.platform !== "linux" && "requires the real Linux inventory source boundary" };
type Fixture = Awaited<ReturnType<typeof reportRetirementFixture>>;

it("real FINAL ACK retires finalize before an archive proof can exhaust its authority deadline", linux, async t => {
  const f = await reportRetirementFixture(t);
  await f.outbox.journalFinalize(f.claim.run_id, 2);
  await f.acknowledgeArchive();
  await replay(f);
  assert.equal(f.outbox.hasPendingTerminal(f.claim.run_id, 2), false);
  assert.ok(f.outbox.finalizeRecordIdentity(f.claim.run_id, 2));
  assert.equal(await f.r.recoveryInventoryPending(f.claim.run_id, 2), false);
  const restoreHeadroom = f.useHeadroom();
  t.after(() => restoreHeadroom());
  const timers = f.captureTimers(ms => ms === reportProofTiming.finalizeAuthorityMs());
  t.after(() => timers.restore());
  let expensiveProofs = 0;
  t.mock.method(f.r, "retireRecoveryReport", async () => {
    expensiveProofs++;
    timers.fire();
    return false;
  });
  const worker = new Worker({} as never, client, f.r, {} as never, {} as never, {} as never,
    nullLogger(), () => ({ ok: true, missing: [] }), f.outbox, new Map(), f.registry);
  await (worker as unknown as { sweepPendingFinalizes(): Promise<void> }).sweepPendingFinalizes();
  assert.equal(f.outbox.finalizeRecordIdentity(f.claim.run_id, 2), undefined,
    "the existing ACK clause must retire finalize before the alternative proof expires");
  assert.equal(expensiveProofs, 0);
  await f.r.resumePendingRecoveries(undefined, await f.coordinator.inspect(f.claim.run_id));
  assert.deepEqual(await f.coordinator.inspect(f.claim.run_id), [], "retiring finalize permits final ACK cleanup");
});

it("without a FINAL ACK the worker still retires finalize through the real archive proof", linux, async t => {
  const f = await reportRetirementFixture(t);
  await f.outbox.journalFinalize(f.claim.run_id, 2);
  await replay(f);
  assert.equal(f.outbox.hasPendingTerminal(f.claim.run_id, 2), false);
  assert.equal(await f.r.recoveryFinalAcknowledged(f.claim.run_id, 2), false);
  assert.ok(f.outbox.finalizeRecordIdentity(f.claim.run_id, 2));
  const restore = f.useHeadroom();
  t.after(restore);
  const realProof = f.r.retireRecoveryReport.bind(f.r);
  let attempts = 0;
  t.mock.method(f.r, "retireRecoveryReport", async (...args: Parameters<typeof realProof>) => {
    attempts++;
    return realProof(...args);
  });
  const worker = new Worker({} as never, client, f.r, {} as never, {} as never, {} as never,
    nullLogger(), () => ({ ok: true, missing: [] }), f.outbox, new Map(), f.registry);
  await (worker as unknown as { sweepPendingFinalizes(): Promise<void> }).sweepPendingFinalizes();
  assert.equal(attempts, 1, "archive-only authority remains reachable");
  assert.equal(f.outbox.finalizeRecordIdentity(f.claim.run_id, 2), undefined);
  assert.equal(f.calls.final, 0, "report retirement grants no FINAL or source release");
  assert.deepEqual({ clone: f.calls.cloneCleanup, source: f.calls.sourceDelete, pins: f.calls.pinDelete },
    { clone: 0, source: 0, pins: 0 }, "archive-only report proof does not delete sources");
  assert.notEqual((await f.coordinator.inspect(f.claim.run_id)).find(row => row.captureId === f.archive.captureId)?.finalAcknowledged, true);
});

async function replay(f: Fixture, r = f.r, outbox = f.outbox) {
  const deps = makeTerminalOutboxDeps(outbox, client, { gapFillMax: 100,
    terminalMaxBytes: 64 * 1024, log: nullLogger() });
  assert.ok(deps);
  const restore = f.useHeadroom();
  try {
    await resolvePendingTerminal(r.protectRecoveryTerminalDeps(deps), {
      runId: f.claim.run_id, claimGeneration: 2,
      send: async () => ({ applied: false, status: "cancelled" }),
    });
  } finally { restore(); }
}

for (const retirement of ["archive", "ack"] as const) for (const restart of [false, true]) {
  it(`real FINAL preserves ${retirement} retirement then cleans evidence (restart=${restart})`, linux, async t => {
    const f = await reportRetirementFixture(t, { olderOpenHold: true, olderSource: false });
    await f.acknowledgeArchive();
    assert.ok(await f.outbox.readTerminalJournal(f.claim.run_id, 2));
    await assert.rejects(fs.stat(f.archive.bundlePath!), { code: "ENOENT" });
    const owner = restart ? await f.restartedRunner() : { outbox: f.outbox, r: f.r };
    await owner.r.resumePendingRecoveries(undefined, await f.coordinator.inspect(f.claim.run_id));
    // Pin each existing authority independently: a fallback must not mask the other path's regression.
    if (retirement === "archive") t.mock.method(owner.r, "recoveryInventoryPending", async () => true);
    else {
      t.mock.method(f.recoveryOf(owner.r), "retireArchivedReport", async () => {});
      // Real claimRun remembers guarded provenance; direct fixture execute does not.
      // Preserve that provenance so the feature-present legacy fallback cannot mask the ACK clause.
      t.mock.method(client, "knowsInventoryGuardedClaim", () => true);
    }
    await replay(f, owner.r, owner.outbox);
    assert.equal(await owner.outbox.readTerminalJournal(f.claim.run_id, 2), undefined,
      "the cancelled report must retire after real FINAL, despite the older undecided hold");
    assert.equal((await f.coordinator.inspect(f.claim.run_id)).find(row => row.captureId === f.archive.captureId)?.finalAcknowledged, true);
    assert.equal(owner.outbox.listPendingTerminals().length, 0);
    assert.equal((await owner.outbox.journalTerminal(f.claim.run_id, 3, "running", 0,
      { status: "failed", failure_reason: "later generation" })).journaled, true);
    await owner.r.resumePendingRecoveries(undefined, await f.coordinator.inspect(f.claim.run_id));
    assert.deepEqual(await f.coordinator.inspect(f.claim.run_id), []);
    assert.ok(await owner.outbox.readTerminalJournal(f.claim.run_id, 3));
    assert.equal(f.cmd(f.bare, ["for-each-ref", "--format=%(refname)",
      `refs/uzi-recovery-pin/${f.claim.run_id}/2`, `refs/uzi-coverage/${f.claim.run_id}/2/`]), "");
    assert.equal(f.calls.final, 1, "cleanup must not replay FINAL or resolve the sibling hold");
    assert.deepEqual((await client.listRecoveryHolds(f.claim.run_id)).holds.map(h => h.generation), [1]);
  });
}

it("finalize-pending protects its FINAL evidence and later permits exact cleanup", linux, async t => {
  const f = await reportRetirementFixture(t, { olderOpenHold: true, olderSource: false });
  await f.replayPositive();
  assert.equal((await f.outbox.journalFinalize(f.claim.run_id, 2)).written, true);
  await f.acknowledgeArchive();
  await f.assertFinalizePending();
  await f.r.resumePendingRecoveries(undefined, await f.coordinator.inspect(f.claim.run_id));
  await f.assertFinalizePending();
  const restore = f.useHeadroom();
  try {
    assert.equal(await f.r.retireRecoveryReport(f.claim.run_id, 2, {
      expectedIdentity: f.outbox.finalizeRecordIdentity(f.claim.run_id, 2)!,
      signal: new AbortController().signal, deadline: Date.now() + f.headroomMs,
    }, "finalize"), true);
  } finally { restore(); }
  await f.r.resumePendingRecoveries(undefined, await f.coordinator.inspect(f.claim.run_id));
  assert.deepEqual(await f.coordinator.inspect(f.claim.run_id), []);
  assert.equal(f.calls.final, 1);
});

it("disabled init retains ACKs without bundle bytes, logs once, and retries after store repair", linux, async t => {
  const f = await reportRetirementFixture(t, { olderOpenHold: true, olderSource: false });
  await f.replayPositive();
  const broken = await f.restartedRunner({ root: path.join(fx.dataDir, "outbox-disabled"), brokenRoot: true });
  assert.equal(broken.outbox.isDisabled(), true);
  assert.equal(broken.outbox.listPendingTerminals().length, 0);
  const messages: unknown[] = [];
  const log = (f.recoveryOf(broken.r) as unknown as { log: ReturnType<typeof nullLogger> }).log;
  t.mock.method(log, "debug", (message: string, fields?: Record<string, unknown>) => {
    if (message === "recovery cleanup deferred: pending report") messages.push(fields);
  });
  await f.acknowledgeArchive(broken.r);
  for (let i = 0; i < 2; i++) await broken.r.resumePendingRecoveries(undefined, await f.coordinator.inspect(f.claim.run_id));
  assert.equal((await f.coordinator.inspect(f.claim.run_id)).find(row => row.captureId === f.archive.captureId)?.finalAcknowledged, true);
  await assert.rejects(fs.stat(f.archive.bundlePath!), { code: "ENOENT" });
  assert.deepEqual(messages, [{ run_id: f.claim.run_id, generation: 2 }]);
  await fs.unlink(broken.root);
  const repaired = await f.restartedRunner({ root: broken.root });
  assert.equal(repaired.outbox.isDisabled(), false);
  await repaired.r.resumePendingRecoveries(undefined, await f.coordinator.inspect(f.claim.run_id));
  assert.deepEqual(await f.coordinator.inspect(f.claim.run_id), []);
  assert.equal(f.calls.final, 1);
});
