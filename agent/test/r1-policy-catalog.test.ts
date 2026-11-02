import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { Outbox } from "../src/outbox.js";
import { ActiveRunRegistry } from "../src/active-run-registry.js";
import { makeTerminalOutboxDeps, journalAndResolveTerminal } from "../src/terminal-resolve.js";
import type { PolicyHoldRequest } from "../src/protocol.js";
import { makeScratchRoot } from "./fixture-repo.js";
import { nullLogger } from "./helpers.js";

const run = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa";
const other = "cccccccc-cccc-4ccc-8ccc-cccccccccccc";
const policy: PolicyHoldRequest = {
  outcome_id: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
  source_generation: 1, claim_generation: 1, detail: "refused",
  continuation_mode: "same_conversation", published: false,
  tag: "cyberPolicy", phase: "implementation", correlation_id: "pr-" + "a".repeat(32) + "-1",
};
async function fixture(t: { after(fn: () => Promise<void>): void }, bytes = 1e6) {
  const dir = makeScratchRoot("r1-policy-", await fs.realpath(os.tmpdir()));
  t.after(() => fs.rm(dir, { recursive: true, force: true }));
  const root = path.join(dir, "outbox");
  const make = (budget = bytes) => new Outbox({ root, log: nullLogger(),
    runMaxBytes: budget, maxBytes: budget, retentionMs: 1 });
  const outbox = make();
  await outbox.init();
  return { root, outbox, make };
}

it("ordinary metadata and original byte quotas do not exhaust live policy identities or pend another run", async t => {
  const { outbox } = await fixture(t, 1024);
  for (let generation = 1; generation <= 12; generation++)
    await outbox.journalTerminal(other, generation, "running", 0, { status: "failed" });
  for (let generation = 1; generation <= 270; generation++)
    await outbox.journalPolicyHold(run, { ...policy, source_generation: generation, claim_generation: generation });
  assert.equal(outbox.listPendingTerminals().filter(p => p.run_id === run).length, 270);
  assert.equal(await outbox.hasPendingPolicyOutcome(run), true);
  // The ordinary run's own content may exceed its inspection budget, so use a positively empty run.
  assert.equal(await outbox.hasPendingPolicyOutcome("dddddddd-dddd-4ddd-8ddd-dddddddddddd"), false);
  assert.deepEqual(await outbox.journalTerminal(run, 999, "running", 0, { status: "failed" }),
    { journaled: false, reason: "winner_unavailable" });
});

it("1024 unused flight slots stay outside terminal telemetry and can be reused only by their token", async t => {
  const { outbox } = await fixture(t);
  const tokens: symbol[] = [];
  for (let generation = 1; generation <= 1024; generation++) {
    const token = outbox.reservePolicyFlight(run, generation);
    assert.ok(token);
    tokens.push(token);
  }
  assert.equal(outbox.reservePolicyFlight(other, 1), undefined);
  assert.equal(outbox.hasPendingTerminal(run, 1), false);
  assert.deepEqual(outbox.listPendingTerminals(), []);
  assert.equal(await outbox.hasPendingPolicyOutcome(other), false);
  const old = tokens.shift()!;
  outbox.releasePolicyFlight(old);
  const replacement = outbox.reservePolicyFlight(other, 1);
  assert.ok(replacement);
  assert.equal(outbox.selectPolicyFlight(old, policy.outcome_id), false);
  assert.equal(outbox.selectPolicyFlight(replacement, policy.outcome_id), true);
  assert.equal(await outbox.hasPendingPolicyOutcome(other), true);
  for (const token of tokens) outbox.releasePolicyFlight(token);
});

it("synchronous selection survives capture gap, disappearance and a competing ordinary writer", async t => {
  const { outbox, root } = await fixture(t);
  const token = outbox.reservePolicyFlight(run, 1)!;
  assert.equal(outbox.selectPolicyFlight(token, policy.outcome_id), true);
  assert.equal(outbox.listPendingTerminals()[0]?.outcome_kind, "unknown");
  assert.deepEqual(await outbox.journalTerminal(run, 1, "running", 0, { status: "failed" }),
    { journaled: false, reason: "winner_unavailable" });
  assert.equal((await outbox.journalPolicyHold(run, policy)).journaled, true);
  await fs.unlink(path.join(root, run, "terminal-1.json"));
  assert.equal(await outbox.readTerminalJournal(run, 1), undefined);
  assert.equal(await outbox.hasPendingPolicyOutcome(run), true);
  assert.deepEqual(await outbox.journalTerminal(run, 2, "running", 0, { status: "failed" }),
    { journaled: false, reason: "winner_unavailable" });
  assert.equal(outbox.listPendingTerminals()[0]?.outcome_kind, "policy_hold");
});

it("same-config boot budget deferral allows fresh demand reads of every signed ordinary record", async t => {
  const { outbox, make } = await fixture(t, 1024);
  for (let generation = 1; generation <= 8; generation++)
    assert.equal((await outbox.journalTerminal(run, generation, "running", 0, { status: "failed" })).journaled, true);
  const restarted = make();
  await restarted.init();
  assert.ok(restarted.listPendingTerminals().some(p => p.outcome_kind === "unknown"));
  const page = await restarted.scanTerminalObservationsPage();
  assert.equal(page.observations.some(o => o.kind === "oversized"), false);
  await restarted.closeTerminalObservationScan();
  for (let generation = 1; generation <= 8; generation++)
    assert.equal((await restarted.readTerminalJournal(run, generation))?.body.status, "failed");
  assert.equal(await restarted.hasPendingPolicyOutcome(other), false);
});

it("cached disabled-empty deps arbitrate later same-run selection while empty and other-run fallbacks send once", async t => {
  const { root, make } = await fixture(t);
  await fs.writeFile(path.join(root, ".key"), "invalid");
  const outbox = make();
  await outbox.init();
  assert.equal(outbox.isDisabled(), true);
  const client = { hasFeature: () => false, getMessageGaps: async () => ({ gaps: [] }),
    postMessages: async () => {} };
  const cached = makeTerminalOutboxDeps(outbox, client,
    { gapFillMax: 1, terminalMaxBytes: 1024, log: nullLogger() });
  assert.ok(cached);
  let sends = 0;
  const send = async () => { sends++; return { applied: true }; };
  const report = (runId: string, generation: number) => journalAndResolveTerminal(cached, {
    runId, claimGeneration: generation, phase: "running", messagesThroughSeq: 0,
    body: { status: "failed" }, send,
  });
  await report(other, 1);
  assert.equal(sends, 1);
  await outbox.journalPolicyHold(run, policy);
  await report(run, 2);
  assert.equal(sends, 1, "cached dependencies must see a later same-run first writer");
  await report(other, 2);
  assert.equal(sends, 2);
});

it("scoped capacity counts unknown runs without inventing a source generation lease", () => {
  const registry = new ActiveRunRegistry(() => [
    { run_id: run, claim_generation: 1, outcome_kind: "unknown", phase: "running", since: 1, blocked: false },
    { run_id: run.toUpperCase(), claim_generation: 1, outcome_kind: "unknown", phase: "running", since: 1, blocked: false },
  ], () => 1);
  assert.equal(registry.claimsPausedByPendingOverflow(), true);
  assert.equal(registry.claimsPausedByPendingOverflow(true), false);
  const snapshot = registry.build(true);
  assert.deepEqual(snapshot.active, []);
  assert.deepEqual(snapshot.pending_all, [{ run_id: run, claim_generation: 1 }]);
  assert.deepEqual(snapshot.pending_policy_protection, []);
  assert.equal(snapshot.pending_overflow, true);
  registry.notePolicyOwnership(run, 3);
  assert.deepEqual(registry.build(true).pending_policy_protection, [{ run_id: run, owner_generation: 3 }]);
});
