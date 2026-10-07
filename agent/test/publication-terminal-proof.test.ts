import { it } from "node:test";
import assert from "node:assert/strict";
import path from "node:path";
import fs from "node:fs";
import { makeFixture } from "./fixture-repo.js";
import { nullLogger } from "./helpers.js";
import { Outbox } from "../src/outbox.js";
import { resolvePendingTerminal } from "../src/terminal-resolve.js";

it("reloads and resends canonical supersession bytes with original generation and retires a cancelled ACK", async () => {
  const fx = makeFixture();
  try {
    const runId = "24042404-0000-4000-8000-000000000001";
    const root = path.join(fx.dataDir, "outbox");
    const make = () => new Outbox({ root, log: nullLogger(), runMaxBytes: 1 << 20,
      maxBytes: 1 << 24, retentionMs: 86400000 });
    const first = make();
    await first.init();
    const reason = "branch_moved: remote_branch_advanced; superseding_tip=" + "a".repeat(40);
    const body = { status: "failed" as const, branch_moved: true, failure_reason: reason, claim_generation: 7 };
    assert.equal((await first.journalTerminal(runId, 7, "running", 23, body)).journaled, true);
    const reloaded = make();
    await reloaded.init();
    const journal = await reloaded.readTerminalJournal(runId, 7);
    assert.ok(journal, "the authenticated terminal journal survives reload");
    const persisted = JSON.parse(fs.readFileSync(path.join(root, runId, "terminal-7.json"), "utf8"));
    assert.equal(persisted.claim_generation, 7);
    assert.equal(journal.messagesThroughSeq, 23);
    assert.equal(journal.phase, "running");
    assert.deepEqual(journal.body, body);
    assert.equal(await reloaded.readTerminalJournal(runId, 8), undefined);
    const sent: unknown[] = [];
    const deps = { outbox: reloaded, log: nullLogger(), gapFillMax: 100, terminalMaxBytes: 1 << 20,
      client: { hasFeature: () => false, getMessageGaps: async () => { throw new Error("unexpected gaps"); },
        postMessages: async () => { throw new Error("unexpected messages"); } } };
    await resolvePendingTerminal(deps, { runId, claimGeneration: 7,
      send: async (b) => { sent.push(b); throw new Error("response lost"); } });
    assert.equal(reloaded.hasPendingTerminal(runId, 7), true);
    await resolvePendingTerminal(deps, { runId, claimGeneration: 7,
      send: async (b) => { sent.push(b); return { applied: false, status: "running" }; } });
    assert.equal(reloaded.hasPendingTerminal(runId, 7), true);
    await resolvePendingTerminal(deps, { runId, claimGeneration: 7,
      send: async (b) => { sent.push(b); return { applied: false, status: "cancelled" }; } });
    assert.deepEqual(sent, Array.from({ length: 3 }, () => ({ ...body, claim_generation: 7 })));
    assert.equal(reloaded.hasPendingTerminal(runId, 7), false);
    const retired = make();
    await retired.init();
    assert.equal(retired.hasPendingTerminal(runId, 7), false);
  } finally { fx.cleanup(); }
});
