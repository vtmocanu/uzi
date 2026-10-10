import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";
import { git, client, fx, installHarness, deferred } from "./runner-harness.js";
import { reportRetirementFixture } from "./report-retirement-fixture.js";
import { makeTerminalOutboxDeps, resolvePendingTerminal } from "../src/terminal-resolve.js";
import { Outbox } from "../src/outbox.js";
import { Worker } from "../src/worker.js";
import type { Config } from "../src/config.js";
import type { ChatRunner } from "../src/chat-runner.js";
import type { JudgeRunner } from "../src/judge-runner.js";
import type { ReviewRunner } from "../src/review-runner.js";
import { nullLogger } from "./helpers.js";

installHarness();
// Archive report retirement needs Linux procfs/no-follow content inspection.
// Other hosts deliberately retain reports; portable checks still run below.

for (const blocker of ["messages", "held", "fence", "messages+held"] as const) {
  it(`worker final eligibility retains ${blocker} after positive archive inspection`, {
    skip: process.platform !== "linux" && "requires Linux procfs/no-follow proof before final eligibility hook",
  }, async t => {
    const f = await reportRetirementFixture(t);
    await f.replayPositive();
    await f.outbox.journalFinalize(f.claim.run_id, 2);
    const w = retirementWorker(f);
    const real = git.credentialFreeCancelCleanHead.bind(git);
    const retire = f.r.retireRecoveryReport.bind(f.r);
    const finalize = f.outbox.retireFinalizeIfEligible.bind(f.outbox);
    let inspections = 0, finalEntries = 0;
    t.mock.method(f.r, "retireRecoveryReport", async (...args: Parameters<typeof retire>) => {
      inspections = 0; // Counts belong to this proof, never two separate partial proofs.
      return retire(...args);
    });
    t.mock.method(git, "credentialFreeCancelCleanHead", async (...args: Parameters<typeof real>) => {
      const head = await real(...args);
      assert.equal(head, f.head);
      inspections++;
      return head;
    });
    t.mock.method(f.outbox, "retireFinalizeIfEligible", async (...args: Parameters<typeof finalize>) => {
      assert.equal(inspections, 2, "complete content inspection in this proof");
      finalEntries++;
      if (blocker.includes("messages")) await f.outbox.appendRangeRecord(f.claim.run_id, 2, 1, 1);
      if (blocker.includes("held")) f.outbox.holdTerminalResolve(f.claim.run_id, 2);
      if (blocker === "fence") w.admittedRunIds.set(f.claim.run_id, 2);
      const result = await finalize(...args);
      assert.equal(result, false, "actual final eligibility refuses the record");
      return result;
    });
    await f.positiveStage("worker final eligibility entry", () => w.sweepPendingFinalizes(),
      () => finalEntries === 1, f.assertFinalizePending);
    assert.equal(finalEntries, 1);
    assert.ok(f.outbox.finalizeRecordIdentity(f.claim.run_id, 2));
    if (blocker.includes("held")) f.outbox.releaseTerminalResolve(f.claim.run_id, 2);
    await f.assertCustody();
  });
}

function retirementWorker(f: Awaited<ReturnType<typeof reportRetirementFixture>>) {
  return new Worker({} as Config, client, f.r, {} as ChatRunner, {} as JudgeRunner,
    {} as ReviewRunner, nullLogger(), () => ({ ok: true, missing: [] }), f.outbox,
    new Map(), f.registry) as unknown as {
      sweepPendingFinalizes(): Promise<void>;
      admittedRunIds: Map<string, number>;
    };
}

it("worker archive branch rotates past overflow and an unsettled slow sibling", async t => {
  const f = await reportRetirementFixture(t);
  if (process.platform === "linux") await f.replayPositive();
  else {
    await f.replay();
    await f.assertPending();
  }
  const release = deferred();
  const slow = "00000000-0000-4000-8000-000000000000";
  const real = f.r.retireRecoveryReport.bind(f.r);
  const attempts: string[] = [];
  let slowAttempts = 0;
  t.mock.method(f.r, "retireRecoveryReport", async (...args: Parameters<typeof real>) => {
    attempts.push(args[0]);
    if (args[0] === slow) { slowAttempts++; await release.promise; return; }
    return real(...args);
  });
  t.mock.method(f.r, "recoveryInventoryPending", async () => true);
  for (let i = 0; i < 18; i++)
    await f.outbox.journalFinalize("00000000-0000-4000-8000-" + String(i).padStart(12, "0"), 2);
  await f.outbox.journalFinalize(f.claim.run_id, 2);
  const w = retirementWorker(f);
  t.after(() => release.resolve());
  await w.sweepPendingFinalizes();
  assert.equal(attempts.length, 16, "existing sixteen-candidate cap");
  assert.equal(slowAttempts, 1);
  if (process.platform === "linux") {
    await f.positiveStage("rotated archive sibling retirement", () => w.sweepPendingFinalizes(),
      () => !f.outbox.finalizeRecordIdentity(f.claim.run_id, 2), f.assertFinalizePending);
    assert.equal(f.outbox.finalizeRecordIdentity(f.claim.run_id, 2), undefined,
      "real archive branch retires valid sibling in the next fair pass");
  } else {
    await w.sweepPendingFinalizes();
    assert.ok(attempts.includes("00000000-0000-4000-8000-000000000017"),
      "fair pass reaches overflow sibling while pending terminal fences its own run");
    await f.assertFinalizePending();
    await f.assertPending();
  }
  assert.equal(slowAttempts, 1, "unsettled whole authority remains keyed after deadline");
  release.resolve();
  await new Promise<void>(r => setImmediate(r));
  await f.assertCustody();
});

it("worker quarantine includes started archive unlink beyond deadline", {
  skip: process.platform !== "linux" && "unlink barrier requires Linux procfs/no-follow report content proof",
}, async t => {
  const f = await reportRetirementFixture(t);
  await f.replayPositive();
  await f.outbox.journalFinalize(f.claim.run_id, 2);
  const w = retirementWorker(f);
  const entered = deferred(), release = deferred();
  const unlink = fs.unlink.bind(fs);
  let attempts = 0;
  t.mock.method(fs, "unlink", async (...args: Parameters<typeof unlink>) => {
    if (String(args[0]).endsWith("/finalize-2.json")) {
      attempts++; entered.resolve(); await release.promise;
    }
    return unlink(...args);
  });
  const first = w.sweepPendingFinalizes();
  t.after(async () => { release.resolve(); await first; });
  await entered.promise;
  await first;
  assert.equal(f.r.isExecuting(f.claim.run_id), true);
  assert.equal(attempts, 1);
  await w.sweepPendingFinalizes();
  assert.equal(attempts, 1, "no second candidate while started unlink remains unsettled");
  release.resolve();
  // Drain the actual O queue and then the runner's reservation completion.
  await f.outbox.readTerminalJournal(f.claim.run_id, 2);
  for (let i = 0; i < 20 && f.r.isExecuting(f.claim.run_id); i++)
    await new Promise<void>(r => setImmediate(r));
  assert.equal(f.r.isExecuting(f.claim.run_id), false);
  assert.equal(f.outbox.finalizeRecordIdentity(f.claim.run_id, 2), undefined);
  await f.assertCustody();
});


for (const ownership of [
  { status: "cancelled" },
  { status: "cancelled", claim_generation: 1 },
  { status: "cancelled", claim_generation: 2.5 },
  { status: "running", claim_generation: 2 },
  { status: "cancelled", claim_generation: 2, inventory_guarded: "yes" },
]) {
  it(`malformed or contradictory ownership retains report: ${JSON.stringify(ownership)}`, async t => {
    const f = await reportRetirementFixture(t);
    t.mock.method(client, "getRunOwnership", async () => ownership);
    await f.replay();
    await f.assertPending();
  });
}

it("missing server capture provenance retains report", async t => {
  const f = await reportRetirementFixture(t);
  t.mock.method(client, "getRecoveryCaptureStatus", async () => ({
    capture_id: f.archive.serverCaptureId, state: "available", manifest_bound: true,
    expires_at: "2099-01-01T00:00:00Z",
  }));
  await f.replay();
  await f.assertPending();
});

it("writer killed only after positive source inspection retains report", {
  skip: process.platform !== "linux" && "requires Linux procfs/no-follow positive source inspection hook",
}, async t => {
  const f = await reportRetirementFixture(t);
  const read = git.credentialFreeCancelCleanHead.bind(git);
  let reached = false;
  t.mock.method(git, "credentialFreeCancelCleanHead", async (...args: Parameters<typeof read>) => {
    const head = await read(...args);
    assert.equal(head, f.head);
    assert.equal(f.processObservation.killed.length, 0);
    reached = true;
    f.processObservation.killed.push(12345);
    return head;
  });
  await f.replay();
  assert.equal(reached, true);
  await f.assertPending();
});

it("duplicate queued during ended-flight report reservation replaces its exact tail", {
  skip: process.platform !== "linux" && "requires Linux procfs/no-follow report reservation proof hook",
}, async t => {
  const f = await reportRetirementFixture(t);
  const read = git.credentialFreeCancelCleanHead.bind(git);
  let queued: Promise<void> | undefined;
  let reached = false;
  t.after(async () => { await queued; });
  t.mock.method(git, "credentialFreeCancelCleanHead", async (...args: Parameters<typeof read>) => {
    const head = await read(...args);
    if (!reached) {
      assert.equal(head, f.head);
      assert.equal(f.r.isExecuting(f.claim.run_id), true, "report reservation owns the ended SDK tail");
      reached = true;
      queued = f.r.execute(f.claim);
      await new Promise<void>(resolve => setImmediate(resolve));
      assert.equal(f.liveFlight.executions(), 1, "duplicate SDK cannot enter while reservation is held");
    }
    return head;
  });
  await f.replay();
  assert.equal(reached, true);
  await queued;
  assert.equal(f.liveFlight.executions(), 1, "cancelled duplicate never entered SDK");
  await f.assertPending();
});

it("same-generation adoption and blocked metadata preserve identity, but blocked report is ineligible", async t => {
  const f = await reportRetirementFixture(t);
  const first = (await f.outbox.readTerminalJournalForRetirement(f.claim.run_id, 2))!;
  const adoption = await f.outbox.journalTerminal(f.claim.run_id, 2, "running", 0, first.journal.body);
  assert.equal(adoption.journaled && adoption.adopted, true);
  const adopted = (await f.outbox.readTerminalJournalForRetirement(f.claim.run_id, 2))!;
  assert.equal(adopted.context.expectedIdentity, first.context.expectedIdentity);
  await f.outbox.markTerminalBlocked(f.claim.run_id, 2, "gap_unrecoverable");
  const blocked = (await f.outbox.readTerminalJournalForRetirement(f.claim.run_id, 2))!;
  assert.equal(blocked.context.expectedIdentity, first.context.expectedIdentity);
  await f.r.retireRecoveryReport(f.claim.run_id, 2, first.context);
  await f.assertPending();
});

it("remove and reinstall identical terminal bytes refuses old lifetime proof", async t => {
  const f = await reportRetirementFixture(t);
  const file = path.join(fx.dataDir, "outbox", f.claim.run_id, "terminal-2.json");
  const bytes = await fs.readFile(file);
  const first = (await f.outbox.readTerminalJournalForRetirement(f.claim.run_id, 2))!;
  await f.outbox.retireTerminal(f.claim.run_id, 2);
  await fs.writeFile(file, bytes);
  const adoption = await f.outbox.journalTerminal(f.claim.run_id, 2, "running", 0, first.journal.body);
  assert.equal(adoption.journaled && adoption.adopted, true);
  const replacement = (await f.outbox.readTerminalJournalForRetirement(f.claim.run_id, 2))!;
  assert.notEqual(replacement.context.expectedIdentity, first.context.expectedIdentity);
  await f.r.retireRecoveryReport(f.claim.run_id, 2, first.context);
  await f.assertPending();
  if (process.platform !== "linux") {
    await f.replay();
    await f.assertPending();
    return;
  }
  await f.replayPositive();
  assert.equal(f.outbox.hasPendingTerminal(f.claim.run_id, 2), false);
  await f.assertCustody();
});

for (const gapFill of [false, true]) {
  it(`replacement between send and ACK retains new report (gap-fill=${gapFill})`, async t => {
    const f = await reportRetirementFixture(t);
    const file = path.join(fx.dataDir, "outbox", f.claim.run_id, "terminal-2.json");
    const bytes = await fs.readFile(file);
    const first = (await f.outbox.readTerminalJournalForRetirement(f.claim.run_id, 2))!;
    let sends = 0, gaps = 0;
    t.mock.method(client, "getMessageGaps", async () => { gaps++; return { gaps: [] }; });
    const deps = makeTerminalOutboxDeps(f.outbox, client, {
      gapFillMax: 100, terminalMaxBytes: 64 * 1024, log: nullLogger(),
    })!;
    await resolvePendingTerminal(f.r.protectRecoveryTerminalDeps(deps), {
      runId: f.claim.run_id, claimGeneration: 2,
      send: async () => {
        sends++;
        if (sends === 1) {
          await f.outbox.retireTerminal(f.claim.run_id, 2);
          await fs.writeFile(file, bytes);
          assert.equal((await f.outbox.journalTerminal(f.claim.run_id, 2, "running", 0,
            first.journal.body)).journaled, true);
          if (gapFill) return { applied: false, reason: "messages_pending" };
        }
        return { applied: false, status: "cancelled" };
      },
    });
    assert.equal(sends, gapFill ? 2 : 1);
    assert.equal(gaps, gapFill ? 1 : 0);
    assert.notEqual((await f.outbox.readTerminalJournalForRetirement(f.claim.run_id, 2))!
      .context.expectedIdentity, first.context.expectedIdentity);
    await f.assertPending();
    if (process.platform !== "linux") {
      await f.replay();
      await f.assertPending();
      return;
    }
    await f.replayPositive();
    assert.equal(f.outbox.hasPendingTerminal(f.claim.run_id, 2), false);
    await f.assertCustody();
  });
}

it("unlink failure retains pending map and next replay succeeds", {
  skip: process.platform !== "linux" && "unlink hook requires Linux procfs/no-follow report content proof",
}, async t => {
  const f = await reportRetirementFixture(t);
  const unlink = fs.unlink.bind(fs);
  let attempts = 0;
  t.mock.method(fs, "unlink", async (file: Parameters<typeof unlink>[0]) => {
    if (String(file).endsWith("/terminal-2.json") && attempts++ === 0) throw new Error("fixture unlink refusal");
    return unlink(file);
  });
  await f.replay();
  assert.equal(attempts, 1);
  await f.assertPending();
  await f.replayPositive();
  assert.equal(attempts, 2);
  assert.equal(f.outbox.hasPendingTerminal(f.claim.run_id, 2), false);
  await f.assertCustody();
});

it("started unlink retains reservation beyond deadline until actual settlement", {
  skip: process.platform !== "linux" && "unlink barrier requires Linux procfs/no-follow report content proof",
}, async t => {
  const f = await reportRetirementFixture(t);
  const unlink = fs.unlink.bind(fs);
  const entered = deferred(), release = deferred();
  let attempts = 0, settled = false;
  t.mock.method(fs, "unlink", async (file: Parameters<typeof unlink>[0]) => {
    if (String(file).endsWith("/terminal-2.json")) {
      attempts++;
      entered.resolve();
      await release.promise;
    }
    return unlink(file);
  });
  const replay = f.replay().then(() => { settled = true; });
  t.after(async () => { release.resolve(); await replay; });
  await entered.promise;
  await new Promise(resolve => setTimeout(resolve, 1100));
  assert.equal(settled, false, "deadline cannot falsely complete started unlink");
  assert.equal(f.r.isExecuting(f.claim.run_id), true);
  assert.equal(f.outbox.hasPendingTerminal(f.claim.run_id, 2), true);
  let readSettled = false;
  const read = f.outbox.readTerminalJournal(f.claim.run_id, 2).then(() => { readSettled = true; });
  await new Promise<void>(resolve => setImmediate(resolve));
  assert.equal(readSettled, false, "outbox lock remains owned by actual unlink");
  assert.equal(attempts, 1);
  await f.assertCustody();
  release.resolve();
  await Promise.all([replay, read]);
  assert.equal(f.r.isExecuting(f.claim.run_id), false);
  assert.equal(f.outbox.hasPendingTerminal(f.claim.run_id, 2), false);
  await f.assertCustody();
});

it("expired report proof never starts unlink", async t => {
  const f = await reportRetirementFixture(t);
  const captured = (await f.outbox.readTerminalJournalForRetirement(f.claim.run_id, 2))!;
  let attempts = 0;
  t.mock.method(fs, "unlink", async () => { attempts++; throw new Error("unexpected unlink"); });
  await f.r.retireRecoveryReport(f.claim.run_id, 2, { ...captured.context, deadline: Date.now() - 1 });
  assert.equal(attempts, 0);
  await f.assertPending();
});

it("authenticated restart redoes archive proof before retiring report", async t => {
  const f = await reportRetirementFixture(t);
  const restarted = new Outbox({ root: path.join(fx.dataDir, "outbox"), log: nullLogger(),
    runMaxBytes: 64 * 1024 * 1024, maxBytes: 512 * 1024 * 1024, retentionMs: 7 * 86400000 });
  await restarted.init();
  const captured = (await restarted.readTerminalJournalForRetirement(f.claim.run_id, 2))!;
  f.capture.mode = "throw";
  await f.r.retireRecoveryReport(f.claim.run_id, 2, captured.context, "terminal", restarted);
  assert.equal(restarted.hasPendingTerminal(f.claim.run_id, 2), true);
  f.capture.mode = "healthy";
  if (process.platform !== "linux") {
    const fresh = (await restarted.readTerminalJournalForRetirement(f.claim.run_id, 2))!;
    await f.r.retireRecoveryReport(f.claim.run_id, 2, fresh.context, "terminal", restarted);
    assert.ok(await restarted.readTerminalJournal(f.claim.run_id, 2));
    await f.assertPending();
    return;
  }
  await f.positiveStage("authenticated restart retirement", async () => {
    const fresh = (await restarted.readTerminalJournalForRetirement(f.claim.run_id, 2))!;
    await f.r.retireRecoveryReport(f.claim.run_id, 2, fresh.context, "terminal", restarted);
  }, () => !restarted.hasPendingTerminal(f.claim.run_id, 2), async () => {
    assert.ok(await restarted.readTerminalJournal(f.claim.run_id, 2));
    await f.assertCustody();
  });
  assert.equal(restarted.hasPendingTerminal(f.claim.run_id, 2), false);
  await f.assertCustody();
});

it("worker finalize archive proof respects admission fence then retires under own reservation", async t => {
  const f = await reportRetirementFixture(t);
  if (process.platform === "linux") await f.replayPositive();
  else {
    await f.replay();
    await f.assertPending();
  }
  await f.outbox.journalFinalize(f.claim.run_id, 2);
  const identity = f.outbox.finalizeRecordIdentity(f.claim.run_id, 2);
  const w = new Worker({} as Config, client, f.r, {} as ChatRunner, {} as JudgeRunner,
    {} as ReviewRunner, nullLogger(), () => ({ ok: true, missing: [] }), f.outbox,
    new Map(), f.registry) as unknown as {
      admittedRunIds: Map<string, number>;
      sweepPendingFinalizes(): Promise<void>;
    };
  w.admittedRunIds.set(f.claim.run_id, 2);
  await w.sweepPendingFinalizes();
  assert.equal(f.outbox.finalizeRecordIdentity(f.claim.run_id, 2), identity);
  w.admittedRunIds.delete(f.claim.run_id);
  f.registry.add(f.claim.run_id, 2);
  await w.sweepPendingFinalizes();
  assert.equal(f.outbox.finalizeRecordIdentity(f.claim.run_id, 2), identity, "active fence is independent of admission");
  f.registry.remove(f.claim.run_id);
  if (process.platform !== "linux") {
    await w.sweepPendingFinalizes();
    await f.assertFinalizePending();
    await f.assertPending();
    return;
  }
  await f.positiveStage("unfenced worker finalize", () => w.sweepPendingFinalizes(),
    () => !f.outbox.finalizeRecordIdentity(f.claim.run_id, 2), f.assertFinalizePending);
  assert.equal(f.outbox.finalizeRecordIdentity(f.claim.run_id, 2), undefined);
  await f.assertCustody();
});
