import assert from "node:assert/strict";
import { randomBytes, randomUUID } from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import { describe, it, type TestContext } from "node:test";
import { acquireWorkerLifetime } from "../src/authority-lock.js";
import { AuthorityRuntime } from "../src/authority-runtime.js";
import { openAuthorityStore, type SlotSnapshot, type SourceUseLease } from "../src/completion-authority.js";
import { makeTmp, testLockOptions } from "./authority-test-support.js";

// issue #3639 - verifyReceiptSlots AUTHORIZES a same-incarnation reclaim, so it refuses everything
// verifyEndedSlots merely reports: a pending footprint of any kind and a slot another sink still owns.

const TOKEN = "unit-join-" + randomBytes(12).toString("hex");

async function rig(t: TestContext) {
  const root = makeTmp(t);
  const lifetime = await acquireWorkerLifetime(root, testLockOptions());
  const clone = path.join(root, "runner", "repo", "issue-1");
  fs.mkdirSync(clone, { recursive: true });
  const rt = new AuthorityRuntime({ lifetime, workerToken: TOKEN, timeoutMs: 5000 });
  const slot = (): Promise<SlotSnapshot> => rt.withTransaction(async (tx) => {
    const store = await openAuthorityStore(tx, { workerToken: TOKEN });
    return (await store.readSlot(tx, await store.slotKey(tx, clone)))!;
  });
  const source = async () => { const s = await slot(); return { path: clone, authorityKey: s.key, epoch: s.epoch, seq: s.seq }; };
  /**
   * Writes the slot through an independent store of this worker (a fresh epoch), so the runtime's own sink ownership
   * is untouched: `bind` first admits the clone in `sink`, which then owns it with no confirmed end.
   */
  const withStore = async (fn: (store: Awaited<ReturnType<typeof openAuthorityStore>>, tx: Parameters<Parameters<typeof rt.withTransaction>[0]>[0], lease: SourceUseLease) => Promise<void>, bind?: Set<string>) => {
    if (bind) await rt.recordAdmissions(bind, () => rt.bindSource(clone));
    await rt.withTransaction(async (tx) => {
      const store = await openAuthorityStore(tx, { workerToken: TOKEN });
      await fn(store, tx, await store.admitSourceUse(tx, clone));
    });
  };
  return { rt, clone, slot, source, withStore };
}

describe("verifyReceiptSlots (issue #3639)", () => {
  it("is true for a clean matching ended slot and false for a wrong epoch, seq, owner, key or empty list", async (t) => {
    const r = await rig(t);
    const sink = new Set<string>();
    await r.rt.recordAdmissions(sink, () => r.rt.bindSource(r.clone));
    assert.equal((await r.rt.endSourceUse([r.clone])).endedSlots.length, 1);
    const good = await r.source();
    const worker = r.rt.authorityIncarnation()!;
    assert.equal(await r.rt.verifyReceiptSlots([good], worker, sink), true);
    assert.equal(await r.rt.verifyReceiptSlots([{ ...good, epoch: randomUUID() }], worker, sink), false, "wrong epoch");
    assert.equal(await r.rt.verifyReceiptSlots([{ ...good, seq: good.seq + 1 }], worker, sink), false, "wrong seq");
    assert.equal(await r.rt.verifyReceiptSlots([good], randomUUID(), sink), false, "wrong owner");
    assert.equal(await r.rt.verifyReceiptSlots([{ ...good, authorityKey: "x".repeat(good.authorityKey.length) }], worker, sink), false, "wrong key");
    assert.equal(await r.rt.verifyReceiptSlots([], worker, sink), false, "nothing to verify");
  });

  it("is false for a slot another sink still owns, and true for the owning sink", async (t) => {
    const r = await rig(t);
    const owner = new Set<string>();
    await r.withStore(async (store, tx, lease) => { await store.transition(tx, lease, "ineligible"); }, owner);
    const good = await r.source();
    const worker = (await r.slot()).owner.worker;
    assert.equal(r.rt.heldByOtherSink(r.clone, new Set()), true, "the end is not confirmed, so ownership stays");
    assert.equal(await r.rt.verifyReceiptSlots([good], worker, new Set()), false);
    assert.equal(await r.rt.verifyReceiptSlots([good], worker, owner), true);
    assert.equal(await r.rt.verifyEndedSlots([good], worker), true, "the diagnostic form does not look at ownership");
  });

  for (const kind of ["destructive", "bare_mutation"] as const) {
    it(`is false for a settled_failed ${kind} footprint although identity matches and verifyEndedSlots says true`, async (t) => {
      const r = await rig(t);
      const sink = new Set<string>();
      await r.withStore(async (store, tx, lease) => {
        const op = await store.beginPending(tx, lease, { kind, footprint: r.clone });
        await store.settlePending(tx, lease, op, { ok: false, residue: [r.clone] });
      });
      const slot = await r.slot();
      assert.equal(slot.state, "ineligible");
      assert.equal(slot.pending?.status, "settled_failed");
      const good = await r.source();
      const worker = slot.owner.worker;
      assert.equal(await r.rt.verifyEndedSlots([good], worker), true);
      assert.equal(await r.rt.verifyReceiptSlots([good], worker, sink), false);
    });
  }

  it("is false for a running pending footprint", async (t) => {
    const r = await rig(t);
    const sink = new Set<string>();
    await r.withStore(async (store, tx, lease) => { await store.beginPending(tx, lease, { kind: "bare_mutation", footprint: r.clone }); });
    const slot = await r.slot();
    assert.equal(slot.pending?.status, "running");
    assert.equal(await r.rt.verifyReceiptSlots([await r.source()], slot.owner.worker, sink), false);
  });
});
