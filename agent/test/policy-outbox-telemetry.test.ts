import { it, type TestContext } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";
import { createHmac } from "node:crypto";
import { Outbox, type OutboxOptions } from "../src/outbox.js";
import { type PolicyHoldRequest } from "../src/protocol.js";
import { nullLogger } from "./helpers.js";

const run = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa";
const body: PolicyHoldRequest = {
  outcome_id: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
  source_generation: 7, claim_generation: 7,
  detail: "refused", continuation_mode: "same_conversation", published: false,
  correlation_id: "pr-" + "a".repeat(32) + "-1", session_id: "session", tag: "cyberPolicy", phase: "implementation",
};

async function fixture(t: TestContext, options: Partial<OutboxOptions> = {}) {
  const dir = await fs.mkdtemp(path.resolve("../.uzi/scratch/policy-telemetry-"));
  t.after(() => fs.rm(dir, { recursive: true, force: true }));
  const root = path.join(dir, "outbox");
  const make = (extra: Partial<OutboxOptions> = {}) => new Outbox({
    root, log: nullLogger(), runMaxBytes: 1e6, maxBytes: 4e6,
    retentionMs: 86400000, ...options, ...extra,
  });
  const outbox = make();
  await outbox.init();
  return { root, outbox, make, file: path.join(root, run, "terminal-7.json") };
}

function facts(outbox: Outbox) {
  const depth = outbox.depthFor(run);
  assert.ok(depth);
  return {
    id: depth.policyPendingOutcomeId,
    source: depth.policyPendingSourceGeneration,
    count: depth.pendingTerminal,
  };
}

async function settle(outbox: Outbox, generation: number) {
  const sent = await outbox.readTerminalJournalForRetirement(run, generation);
  assert.ok(sent);
  await outbox.markTerminalServerSettled(run, generation, sent.context);
}

it("known pending facts survive authenticated restart and adoption; ACK preserves physical bytes", async t => {
  const f = await fixture(t);
  assert.equal((await f.outbox.journalPolicyHold(run, body)).journaled, true);
  const pending = { id: body.outcome_id, source: 7, count: 1 };
  assert.deepEqual(facts(f.outbox), pending);
  const bytes = await fs.readFile(f.file, "utf8");
  const restarted = f.make();
  await restarted.init();
  assert.deepEqual(facts(restarted), pending);
  assert.deepEqual(await restarted.journalTerminal(run, 7, "running", 0, { status: "failed" }), { journaled: true, adopted: true });
  assert.deepEqual(facts(restarted), pending);
  // A context without the exact authenticated identity cannot hide pending telemetry.
  await restarted.markTerminalServerSettled(run, 7, {});
  assert.deepEqual(facts(restarted), pending);
  await settle(restarted, 7);
  assert.deepEqual(facts(restarted), { id: undefined, source: undefined, count: 0 });
  assert.equal(restarted.depthFor(run)?.blockedReason, undefined);
  assert.equal(await fs.readFile(f.file, "utf8"), bytes);
  assert.deepEqual((await restarted.readTerminalJournal(run, 7))?.body, body);
  assert.equal(restarted.listPendingTerminals().length, 1);
  assert.equal(restarted.listPendingTerminals()[0]?.server_settled, true);
  // Settlement is an instance-local observation; restart authenticates the retained source again.
  const again = f.make();
  await again.init();
  assert.deepEqual(facts(again), pending);
});

it("authenticated exclusive-install adoption retains the first writer UUID", async t => {
  const f = await fixture(t);
  const competitor = f.make();
  await competitor.init();
  await f.outbox.journalPolicyHold(run, body);
  const bytes = await fs.readFile(f.file, "utf8");
  const replacement = { ...body, outcome_id: "cccccccc-cccc-4ccc-8ccc-cccccccccccc" };
  assert.deepEqual(await competitor.journalPolicyHold(run, replacement), { journaled: true, adopted: true });
  assert.deepEqual(facts(competitor), { id: body.outcome_id, source: 7, count: 1 });
  assert.equal(await fs.readFile(f.file, "utf8"), bytes);
});

it("selects the highest unsettled source and preserves ordinary terminal counts", async t => {
  const { outbox } = await fixture(t);
  await outbox.journalPolicyHold(run, body);
  const newer = { ...body, outcome_id: "cccccccc-cccc-4ccc-8ccc-cccccccccccc", source_generation: 8, claim_generation: 8 };
  await outbox.journalPolicyHold(run, newer);
  assert.deepEqual(facts(outbox), { id: newer.outcome_id, source: 8, count: 2 });
  await settle(outbox, 8);
  assert.deepEqual(facts(outbox), { id: body.outcome_id, source: 7, count: 1 });
  await settle(outbox, 7);
  await outbox.journalTerminal(run, 9, "running", 0, { status: "failed" });
  assert.deepEqual(facts(outbox), { id: undefined, source: undefined, count: 1 });
  await settle(outbox, 9);
  assert.deepEqual(facts(outbox), { id: undefined, source: undefined, count: 1 });
  assert.equal(outbox.listPendingTerminals().length, 3);
});

it("ENOSPC retains validated unpersisted policy facts", async t => {
  const { outbox, file } = await fixture(t, { rawWrite: async (write, ctx) => {
    if (ctx.kind === "terminal") throw Object.assign(new Error("full"), { code: "ENOSPC" });
    await write();
  } });
  assert.deepEqual(await outbox.journalPolicyHold(run, body), { journaled: false, reason: "reserve_exhausted" });
  assert.deepEqual(facts(outbox), { id: body.outcome_id, source: 7, count: 1 });
  await assert.rejects(fs.stat(file), { code: "ENOENT" });
});

for (const bad of ["malformed", "MAC", "run-cap", "total-cap", "disabled"] as const) {
  it(bad + " evidence remains unknown without typed policy facts", async t => {
    const f = await fixture(t);
    await f.outbox.journalPolicyHold(run, body);
    if (bad === "malformed" || bad === "MAC") {
      const data = JSON.parse(await fs.readFile(f.file, "utf8"));
      data.body.outcome_id = "invalid";
      if (bad === "malformed") {
        delete data.mac;
        const canonical = (v: unknown): string => v !== null && typeof v === "object" ?
          "{" + Object.entries(v).sort(([a], [b]) => a < b ? -1 : a > b ? 1 : 0)
            .map(([k, value]) => JSON.stringify(k) + ":" + canonical(value)).join(",") + "}" : JSON.stringify(v);
        data.mac = createHmac("sha256", await fs.readFile(path.join(f.root, ".key")))
          .update("uzi.outbox.terminal.v1").update(canonical(data)).digest("hex");
      }
      await fs.writeFile(f.file, JSON.stringify(data));
    }
    if (bad === "disabled") await fs.writeFile(path.join(f.root, ".key"), "bad key");
    const outbox = f.make(bad === "run-cap" ? { runMaxBytes: 10 } : bad === "total-cap" ? { maxBytes: 10 } : {});
    await outbox.init();
    assert.deepEqual(facts(outbox), { id: undefined, source: undefined, count: 1 });
    assert.equal(outbox.listPendingTerminals()[0]?.outcome_kind, "unknown");
    assert.equal(outbox.listPendingTerminals()[0]?.server_settled, undefined);
    assert.equal(await outbox.hasPendingPolicyOutcome(run), true);
  });
}

it("disabled store retains validated memory facts without trusting settlement", async t => {
  const f = await fixture(t);
  await fs.writeFile(path.join(f.root, ".key"), "bad key");
  const outbox = f.make();
  await outbox.init();
  assert.equal(outbox.isDisabled(), true);
  await outbox.journalPolicyHold(run, body);
  await outbox.markTerminalServerSettled(run, 7, { expectedIdentity: {} });
  assert.deepEqual(facts(outbox), { id: body.outcome_id, source: 7, count: 1 });
});

it("body budget exhaustion retains unknown memory without a fabricated UUID", async t => {
  const { outbox } = await fixture(t, { runMaxBytes: 10 });
  await outbox.journalPolicyHold(run, body);
  assert.deepEqual(facts(outbox), { id: undefined, source: undefined, count: 1 });
  assert.equal(outbox.listPendingTerminals()[0]?.outcome_kind, "unknown");
});
