import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";
import { ActiveRunRegistry } from "../src/active-run-registry.js";
import { Outbox } from "../src/outbox.js";
import { makeTerminalOutboxDeps, resolvePendingTerminal } from "../src/terminal-resolve.js";
import { Worker } from "../src/worker.js";
import type { Config } from "../src/config.js";
import type { ChatRunner } from "../src/chat-runner.js";
import type { JudgeRunner } from "../src/judge-runner.js";
import type { ReviewRunner } from "../src/review-runner.js";
import { client, fx, gitlabClaim, installHarness } from "./runner-harness.js";
import { reportRetirementFixture } from "./report-retirement-fixture.js";
import { nullLogger } from "./helpers.js";

installHarness();

async function restartedOutbox() {
  const outbox = new Outbox({ root: path.join(fx.dataDir, "outbox"), log: nullLogger(),
    runMaxBytes: 64 * 1024 * 1024, maxBytes: 512 * 1024 * 1024, retentionMs: 7 * 86_400_000 });
  await outbox.init();
  return outbox;
}

it("server-cancelled reply releases snapshot and overflow admission while real custody retirement refuses", async t => {
  const f = await reportRetirementFixture(t);
  const registry = new ActiveRunRegistry(() => f.outbox.listPendingTerminals(), () => 0);
  const journalPath = path.join(fx.dataDir, "outbox", f.claim.run_id, "terminal-2.json");
  const journalBytes = await fs.readFile(journalPath);
  assert.equal(JSON.parse(journalBytes.toString()).server_settled, undefined, "marker is not a disk field");
  // Fixture execution already drove its real refused-terminal send and custody guard.
  assert.equal(registry.claimsPausedByPendingOverflow(), false);
  f.capture.mode = "throw";
  await f.replay();
  await f.assertPending();
  await f.assertCustody();
  assert.deepEqual(await fs.readFile(journalPath), journalBytes, "server settlement never rewrites journal evidence");
  assert.equal(f.r.isExecuting(f.claim.run_id), false);
  assert.equal(f.outbox.hasPendingTerminal(f.claim.run_id, 2), true);
  assert.deepEqual(registry.build().active, [], "settled outcome must not advertise an execution");
  assert.equal(registry.build().pending_overflow, false);
  assert.equal(registry.buildRegisterSnapshot().pending_overflow, false);
  assert.equal(registry.claimsPausedByPendingOverflow(), false);

  const controller = new AbortController();
  let admitted = false;
  const fresh = gitlabClaim(2921, { run_id: "22222222-2222-4222-8222-222222222222" });
  const pendingOverflow = registry.claimsPausedByPendingOverflow.bind(registry);
  t.mock.method(registry, "claimsPausedByPendingOverflow", () => {
    const blocked = pendingOverflow();
    if (blocked) controller.abort();
    return blocked;
  });
  t.mock.method(client, "claimRun", async () => fresh);
  t.mock.method(f.r, "execute", async (claim: typeof fresh) => {
    assert.equal(claim.run_id, fresh.run_id);
    admitted = true;
    controller.abort();
  });
  const worker = new Worker({ maxConcurrentRuns: 1, pollIntervalMs: 0, httpTimeoutMs: 1000 } as Config,
    client, f.r, {} as ChatRunner, {} as JudgeRunner, {} as ReviewRunner, nullLogger(),
    () => ({ ok: true, missing: [] }), f.outbox, new Map(), registry) as unknown as {
      claimLoop(signal: AbortSignal): Promise<void>;
  };
  await worker.claimLoop(controller.signal);
  assert.equal(admitted, true, "a fresh claim proceeds through the actual worker admission loop");
  f.capture.mode = "healthy";
  await f.replayPositive();
  assert.equal(f.outbox.hasPendingTerminal(f.claim.run_id, 2), false);
  assert.deepEqual(f.outbox.listPendingTerminals(), []);
  await f.assertCustody();

  // Admission settlement did not authorize deletion. Now drive the real FINAL
  // producer with an independently acknowledged server release, then cleanup.
  let released = false;
  const holds = client.listRecoveryHolds.bind(client);
  client.listRecoveryHolds = async run => released ? { run_id: run, holds: [] } : holds(run);
  client.releaseRecoveryCustody = async (run, generation) => {
    assert.equal(run, f.claim.run_id);
    assert.equal(generation, 2);
    f.calls.final++;
    released = true;
    return { run_id: run, generation, released: true, holds_released: 1 };
  };
  await f.r.resumePendingRecoveries(undefined, [f.archive]);
  assert.equal(f.calls.final, 1, "existing FINAL producer must obtain independent custody release");
  assert.deepEqual((await client.listRecoveryHolds(f.claim.run_id)).holds, []);
  assert.deepEqual(await f.coordinator.inspect(f.claim.run_id), [], "existing cleanup removes retired generation evidence");
  assert.equal(await fs.stat(f.archive.bundlePath!).then(() => true, () => false), false);
});

it("restart conservatively advertises then re-marks after one real refused replay", async t => {
  const f = await reportRetirementFixture(t);
  f.capture.mode = "throw";
  await f.replay();
  const restarted = await restartedOutbox();
  const registry = new ActiveRunRegistry(() => restarted.listPendingTerminals(), () => 0);
  assert.equal(registry.claimsPausedByPendingOverflow(), true, "no marker survives restart");
  const deps = makeTerminalOutboxDeps(restarted, client, { gapFillMax: 100, terminalMaxBytes: 64 * 1024, log: nullLogger() })!;
  let sends = 0;
  await resolvePendingTerminal(f.r.protectRecoveryTerminalDeps(deps), {
    runId: f.claim.run_id, claimGeneration: 2,
    send: async () => { sends++; return { applied: false, status: "cancelled" }; },
  });
  assert.equal(sends, 1);
  assert.equal(restarted.hasPendingTerminal(f.claim.run_id, 2), true);
  assert.equal(registry.claimsPausedByPendingOverflow(), false);
  assert.deepEqual(registry.build().active, []);
  await f.assertCustody();
});

for (const status of ["running", undefined] as const) {
  it(`nonterminal/unknown ${status} reply does not settle admission`, async t => {
    const f = await reportRetirementFixture(t);
    const outbox = await restartedOutbox();
    const deps = makeTerminalOutboxDeps(outbox, client, { gapFillMax: 100, terminalMaxBytes: 64 * 1024, log: nullLogger() })!;
    await resolvePendingTerminal(f.r.protectRecoveryTerminalDeps(deps), {
      runId: f.claim.run_id, claimGeneration: 2, send: async () => ({ applied: false, status }),
    });
    assert.equal(outbox.listPendingTerminals()[0]?.server_settled, undefined);
    await f.assertPending();
    await f.assertCustody();
  });
}

it("an ABA file replacement cannot inherit an earlier send's settled observation", async t => {
  const f = await reportRetirementFixture(t);
  f.capture.mode = "throw";
  const file = path.join(fx.dataDir, "outbox", f.claim.run_id, "terminal-2.json");
  const deps = makeTerminalOutboxDeps(f.outbox, client, { gapFillMax: 100, terminalMaxBytes: 64 * 1024, log: nullLogger() })!;
  await resolvePendingTerminal(f.r.protectRecoveryTerminalDeps(deps), {
    runId: f.claim.run_id, claimGeneration: 2,
    send: async () => {
      const bytes = await fs.readFile(file);
      await fs.writeFile(`${file}.replacement`, bytes);
      await fs.rename(`${file}.replacement`, file);
      return { applied: false, status: "cancelled" };
    },
  });
  assert.equal(f.outbox.hasPendingTerminal(f.claim.run_id, 2), true);
  assert.equal(f.outbox.listPendingTerminals()[0]?.server_settled, undefined);
  await f.assertCustody();
  await f.replay();
  assert.equal(f.outbox.listPendingTerminals()[0]?.server_settled, true);
});

it("another generation's journal cannot be marked by an old send identity", async t => {
  const f = await reportRetirementFixture(t);
  const captured = (await f.outbox.readTerminalJournalForRetirement(f.claim.run_id, 2))!;
  await f.outbox.journalTerminal(f.claim.run_id, 3, "running", 0, { status: "failed", claim_generation: 3 });
  await f.outbox.markTerminalServerSettled(f.claim.run_id, 3, captured.context);
  assert.equal(f.outbox.listPendingTerminals().find(entry => entry.claim_generation === 3)?.server_settled, undefined);
  assert.equal(f.outbox.hasPendingTerminal(f.claim.run_id, 2), true);
});

it("an unsafe journal observation clears settled advertising and replays conservatively", async t => {
  const f = await reportRetirementFixture(t);
  f.capture.mode = "throw";
  const file = path.join(fx.dataDir, "outbox", f.claim.run_id, "terminal-2.json");
  assert.equal(f.outbox.listPendingTerminals()[0]?.server_settled, true);
  await fs.rename(file, `${file}.kept`);
  await fs.symlink(`${file}.kept`, file);
  const observed = await f.outbox.observeTerminalAuthentication(f.claim.run_id, 2);
  assert.equal(observed.kind, "symlink");
  assert.equal(f.outbox.hasPendingTerminal(f.claim.run_id, 2), true);
  assert.equal(f.outbox.listPendingTerminals()[0]?.server_settled, undefined);
  const registry = new ActiveRunRegistry(() => f.outbox.listPendingTerminals(), () => 0);
  assert.equal(registry.claimsPausedByPendingOverflow(), true);
  await fs.unlink(file);
  await fs.rename(`${file}.kept`, file);
  await f.replay();
  assert.equal(registry.claimsPausedByPendingOverflow(), false);
  await f.assertCustody();
});

it("stale-claim precedence does not settle the replacement generation", async t => {
  const f = await reportRetirementFixture(t);
  await f.outbox.journalTerminal(f.claim.run_id, 3, "running", 0, { status: "failed", claim_generation: 3 });
  const deps = makeTerminalOutboxDeps(f.outbox, client, { gapFillMax: 100, terminalMaxBytes: 64 * 1024, log: nullLogger() })!;
  await resolvePendingTerminal(f.r.protectRecoveryTerminalDeps(deps), {
    runId: f.claim.run_id, claimGeneration: 2,
    send: async () => ({ applied: false, staleClaim: true, status: "cancelled" }),
  });
  assert.equal(f.outbox.hasPendingTerminal(f.claim.run_id, 2), false);
  assert.equal(f.outbox.hasPendingTerminal(f.claim.run_id, 3), true);
  assert.equal(f.outbox.listPendingTerminals()[0]?.server_settled, undefined);
  await f.assertCustody();
});
