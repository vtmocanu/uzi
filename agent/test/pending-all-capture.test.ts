import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { ActiveRunRegistry } from "../src/active-run-registry.js";
import { Outbox, canonicalizeTerminalBody } from "../src/outbox.js";
import { nullLogger } from "./helpers.js";
import type { ActiveSnapshot } from "../src/protocol.js";

const runId = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa";
const identities = [
  { run_id: runId, claim_generation: 1 },
  { run_id: runId, claim_generation: 2 },
];

// Directory iteration order is not a wire contract. Keep both generations and
// every snapshot field, comparing this one-run fixture in canonical order.
const byGeneration = <T extends { claim_generation: number }>(entries: T[]): T[] =>
  [...entries].sort((a, b) => a.claim_generation - b.claim_generation);
const orderedSnapshot = (snapshot: ActiveSnapshot): ActiveSnapshot => ({
  ...snapshot, active: byGeneration(snapshot.active),
  ...(snapshot.pending_all ? { pending_all: byGeneration(snapshot.pending_all) } : {}),
});

async function capturesPending(root: string): Promise<Outbox> {
  const options = {
    root,
    log: nullLogger(),
    runMaxBytes: 67108864,
    maxBytes: 536870912,
    retentionMs: 604800000,
  };
  const writer = new Outbox(options);
  await writer.init();
  const body = canonicalizeTerminalBody({ status: "completed", report_md: "fixture" }, 1 << 20);
  for (const generation of [1, 2]) {
    assert.deepStrictEqual(
      await writer.journalTerminal(runId, generation, "running", 0, body),
      { journaled: true, adopted: false },
    );
  }
  // Reload authenticates the actual journal files rather than injecting pending entries.
  const reader = new Outbox(options);
  await reader.init();
  assert.deepStrictEqual(
    byGeneration(reader.listPendingTerminals().map(p => ({ run_id: p.run_id, claim_generation: p.claim_generation }))),
    identities,
  );
  return reader;
}

it("issue 2009 M4: real journals serialize the duplicate-active shared fixture", async t => {
  const temp = await fs.mkdtemp(path.join(os.tmpdir(), "pending-all-capture-"));
  t.after(() => fs.rm(temp, { recursive: true, force: true }));
  const outbox = await capturesPending(path.join(temp, "outbox"));
  let captures = 0;
  const registry = new ActiveRunRegistry(() => {
    captures += 1;
    return outbox.listPendingTerminals();
  }, () => 2);
  const snapshot = registry.build();
  const fixture = JSON.parse(await fs.readFile(
    new URL("../../fixtures/worker-pending-identities/duplicate-active.json", import.meta.url), "utf8",
  ));
  assert.deepStrictEqual(JSON.parse(JSON.stringify(orderedSnapshot(snapshot))), fixture);
  assert.equal(captures, 1);
  assert.deepStrictEqual(snapshot.active.map(p => p.run_id), [runId, runId]);
  assert.deepStrictEqual(byGeneration(snapshot.pending_all ?? []), identities);
  for (const identity of snapshot.pending_all ?? []) {
    assert.deepStrictEqual(Object.keys(identity).sort(), ["claim_generation", "run_id"]);
  }
});

it("issue 2009 M4: cap zero register and heartbeat keep complete journals through retirement", async t => {
  const temp = await fs.mkdtemp(path.join(os.tmpdir(), "pending-all-capture-"));
  t.after(() => fs.rm(temp, { recursive: true, force: true }));
  const outbox = await capturesPending(path.join(temp, "outbox"));
  let captures = 0;
  const registry = new ActiveRunRegistry(() => {
    captures += 1;
    return outbox.listPendingTerminals();
  }, () => 0);
  const register = registry.buildRegisterSnapshot();
  const heartbeat = registry.build();
  assert.deepStrictEqual(orderedSnapshot(register), {
    snapshot_epoch: 1, active: [], pending_overflow: true, pending_all: identities,
  });
  assert.deepStrictEqual(orderedSnapshot(heartbeat), {
    snapshot_epoch: 2, active: [], pending_overflow: true, pending_all: identities,
  });
  assert.equal(captures, 2);
  assert.equal(await outbox.retireTerminal(runId, 1), true);
  assert.deepStrictEqual(registry.buildRegisterSnapshot(), {
    snapshot_epoch: 3, active: [], pending_overflow: true, pending_all: [identities[1]],
  });
  assert.deepStrictEqual(registry.build(), {
    snapshot_epoch: 4, active: [], pending_overflow: true, pending_all: [identities[1]],
  });
  assert.equal(await outbox.retireTerminal(runId, 2), true);
  assert.deepStrictEqual(registry.buildRegisterSnapshot(), {
    snapshot_epoch: 5, active: [], pending_overflow: false, pending_all: [],
  });
  assert.deepStrictEqual(registry.build(), {
    snapshot_epoch: 6, active: [], pending_overflow: false, pending_all: [],
  });
  assert.equal(captures, 6);
});
