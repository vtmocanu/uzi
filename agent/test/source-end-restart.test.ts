import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { randomUUID } from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { it } from "node:test";
import { acquireWorkerLifetime } from "../src/authority-lock.js";
import { AuthorityRuntime } from "../src/authority-runtime.js";
import { openAuthorityStore, type SlotSnapshot } from "../src/completion-authority.js";
import { makeTmp, testLockOptions } from "./authority-test-support.js";

const agentRoot = fileURLToPath(new URL("..", import.meta.url));
type ChildResult = { pid: number; sources: string[]; slots: Array<{ source: string; slot: SlotSnapshot }> };
function setup(root: string, token: string, mode: string): ChildResult {
  // One attempt, bounded wall time and output. Failure aborts this case, never supplies a witness.
  const child = spawnSync(process.execPath, ["--import", "tsx",
    fileURLToPath(new URL("./source-end-restart-child.mts", import.meta.url)), root, token, mode],
  { cwd: agentRoot, encoding: "utf8", timeout: 30_000, maxBuffer: 1024 * 1024 });
  assert.ifError(child.error);
  assert.equal(child.signal, null, child.stderr);
  assert.equal(child.status, 0, child.stderr);
  const result = JSON.parse(child.stdout) as ChildResult;
  assert.notEqual(result.pid, process.pid, "setup ran in an actual fresh Node process");
  assert.equal(result.sources.length, 2);
  assert.equal(result.slots.length, 2);
  return result;
}
async function runtime(root: string, token: string) {
  return new AuthorityRuntime({ lifetime: await acquireWorkerLifetime(root, testLockOptions()),
    workerToken: token, timeoutMs: 5000 });
}
async function load(rt: AuthorityRuntime, token: string, source: string) {
  return rt.withTransaction(async tx => {
    const store = await openAuthorityStore(tx, { workerToken: token });
    return store.readSlot(tx, await store.slotKey(tx, source));
  });
}

it("a real process restart loses declined-end witnesses and protects both exact old active slots", async t => {
  const root = makeTmp(t);
  const token = randomUUID();
  const old = setup(root, token, "declined");
  const rt = await runtime(root, token);
  for (const source of old.sources) {
    const before = await rt.peekSlot(source);
    assert.equal(before?.state, "active");
    assert.equal(await rt.sourceEndDeclined(source, new Set()), false, "no inherited process-local witness");
    const classification = await rt.classifySourceAdmission(source, new Set());
    assert.equal(classification.kind, "foreign_active");
    await assert.rejects(rt.bindSource(source), /active|foreign|held/i);
    const end = await rt.endSourceUse([source]);
    assert.deepEqual(end.ended, []);
    assert.deepEqual(end.kept, [source], "the successor cannot end the old epoch");
    assert.deepEqual(await load(rt, token, source), before, "neither admission nor load steals or ends the epoch");
    const saved = old.slots.find(entry => entry.source === source)!.slot;
    assert.equal(before?.epoch, saved.epoch);
    assert.equal(before?.seq, saved.seq);
    assert.deepEqual(before?.owner, saved.owner);
  }
  // No runner/server is composed here: this proves authority protection, not hold-commit ordering.
});

for (const [mode, name, state] of [
  ["active-ahead", "SourceEndDeclinedStartupActiveSlotAheadOfHead", "active"],
  ["ended-ahead", "SourceEndDeclinedStartupEndedSlotAheadOfHead", "ineligible"],
] as const) {
  it(name, async t => {
    const root = makeTmp(t);
    const token = randomUUID();
    const old = setup(root, token, mode);
    const rt = await runtime(root, token);
    for (const source of old.sources) {
      // The child's authenticated slot is one rename ahead of its head, with no parent high-water mark.
      const slot = old.slots.find(entry => entry.source === source)?.slot;
      assert.ok(slot);
      assert.equal(slot.state, state);
      const head = path.join(root, ".uzi-authority", "heads", slot.key.slice(0, 2) + ".json");
      const before = fs.readFileSync(head);
      await assert.rejects(rt.peekSlot(source), /head|ahead/i);
      assert.equal((await rt.classifySourceAdmission(source, new Set())).kind, "other");
      assert.equal(await rt.sourceEndDeclined(source, new Set()), false);
      assert.deepEqual(fs.readFileSync(head), before, "read-only startup probes never repair");
      const repaired = await load(rt, token, source);
      // The wire file also contains a MAC; compare only the public snapshot's fields.
      assert.equal(repaired?.state, slot.state);
      assert.equal(repaired?.epoch, slot.epoch);
      assert.equal(repaired?.seq, slot.seq);
      assert.deepEqual(repaired?.owner, slot.owner);
      assert.deepEqual(await rt.peekSlot(source), repaired);
    }
  });
}
