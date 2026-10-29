import assert from "node:assert/strict";
import { randomBytes, randomUUID } from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import { describe, it, type TestContext } from "node:test";
import { acquireWorkerLifetime, SourceUseHeldRefusal, type WorkerLifetime } from "../src/authority-lock.js";
import { AuthorityRuntime } from "../src/authority-runtime.js";
import { leaseEpoch, openAuthorityStore, type SlotSnapshot } from "../src/completion-authority.js";
import { makeTmp, testLockOptions } from "./authority-test-support.js";

// issue #3638 D2 — sink retirement, per-key ownership and the confirmed-end evidence, driven through the real
// AuthorityRuntime over a real kernel flock. A "sink" is the set a flight passes to recordAdmissions.

const TOKEN = "unit-join-" + randomBytes(12).toString("hex");

interface Rig {
  root: string;
  lifetime: WorkerLifetime;
  rt: AuthorityRuntime;
  clone: string;
  other: string;
}

async function rig(t: TestContext, extra: Partial<ConstructorParameters<typeof AuthorityRuntime>[0]> = {}): Promise<Rig> {
  const root = makeTmp(t);
  const lifetime = await acquireWorkerLifetime(root, testLockOptions());
  const clone = path.join(root, "runner", "repo", "issue-1");
  const other = path.join(root, "runner", "repo", "issue-2");
  for (const dir of [clone, other]) fs.mkdirSync(dir, { recursive: true });
  return { root, lifetime, rt: new AuthorityRuntime({ lifetime, workerToken: TOKEN, timeoutMs: 5000, ...extra }), clone, other };
}

async function slotOf(rt: AuthorityRuntime, p: string): Promise<SlotSnapshot | null> {
  return rt.withTransaction(async (tx) => {
    const store = await openAuthorityStore(tx, { workerToken: TOKEN });
    return store.readSlot(tx, await store.slotKey(tx, p));
  });
}

/** Every byte the authority store holds, so "refused before any effect" is a byte comparison. */
function authorityBytes(root: string): string {
  const out: string[] = [];
  for (const dir of ["slots", "heads"]) {
    const full = path.join(root, ".uzi-authority", dir);
    if (!fs.existsSync(full)) continue;
    for (const name of fs.readdirSync(full).sort()) out.push(`${dir}/${name}:${fs.readFileSync(path.join(full, name), "utf8")}`);
  }
  return out.join("\n");
}

/** A flight's admission: `bindSource` inside its sink. */
const bindIn = (rt: AuthorityRuntime, sink: Set<string>, p: string, executor?: string) =>
  rt.recordAdmissions(sink, () => rt.bindSource(p, executor));

const hookIn = (rt: AuthorityRuntime, sink: Set<string>, p: string, op: string) =>
  rt.recordAdmissions(sink, () => rt.withTransaction(() => rt.hooks.admitSourceUse(p, op)));

const isHeld = (e: unknown): boolean => e instanceof SourceUseHeldRefusal;

describe("sink retirement", () => {
  it("an old-sink callback is refused before any effect, after teardown and after a successor admitted", async (t) => {
    const r = await rig(t);
    const old = new Set<string>();
    await bindIn(r.rt, old, r.clone);
    const ended = await r.rt.endSourceUse([r.clone]);
    assert.equal(ended.endedSlots.length, 1);
    r.rt.retireSink(old);

    const afterTeardown = authorityBytes(r.root);
    for (const op of ["seed-canonical", "seed-attempt", "replace", "normalize"]) {
      await assert.rejects(hookIn(r.rt, old, r.clone, op), isHeld, `${op} through the retired sink`);
    }
    await assert.rejects(bindIn(r.rt, old, r.clone), isHeld, "bindSource through the retired sink");
    await assert.rejects(bindIn(r.rt, old, r.clone, "exec-1"), isHeld, "a codex bind through the retired sink");
    assert.equal(authorityBytes(r.root), afterTeardown, "the refusals wrote nothing");

    // A successor flight admits (its own sink); the old callback is still refused and the successor's epoch survives.
    const next = new Set<string>();
    const lease = await bindIn(r.rt, next, r.clone);
    const successorBytes = authorityBytes(r.root);
    await assert.rejects(hookIn(r.rt, old, r.clone, "seed-canonical"), isHeld);
    await assert.rejects(bindIn(r.rt, old, r.clone), isHeld);
    assert.equal(authorityBytes(r.root), successorBytes, "the successor's slot is untouched");
    assert.equal((await slotOf(r.rt, r.clone))?.epoch, leaseEpoch(lease));

    // A path the old sink never recorded is refused before it is even named or seeded.
    const before = authorityBytes(r.root);
    const notRecorded = path.join(r.root, "runner", "fresh", "issue-9");
    await assert.rejects(hookIn(r.rt, old, notRecorded, "seed-canonical"), isHeld);
    await assert.rejects(bindIn(r.rt, old, r.other), isHeld);
    assert.equal(authorityBytes(r.root), before);
    assert.equal(fs.existsSync(path.dirname(notRecorded)), false, "the seed parent was not created");
    assert.deepEqual([...old], [r.clone], "the refused admissions were not even named");
  });

  it("the retirement of one sink does not touch another sink or a call outside any sink", async (t) => {
    const r = await rig(t);
    const old = new Set<string>();
    r.rt.retireSink(old);
    const live = new Set<string>();
    await bindIn(r.rt, live, r.clone);
    await r.rt.withTransaction(() => r.rt.hooks.admitSourceUse(r.other, "seed-canonical"));
    assert.equal((await slotOf(r.rt, r.other))?.state, "active");
  });
});

describe("per-key ownership", () => {
  it("an unjoined or kept owner blocks a later sink; a confirmed end releases it", async (t) => {
    const r = await rig(t);
    const first = new Set<string>();
    const l1 = await bindIn(r.rt, first, r.clone);
    r.rt.retireSink(first);
    // The first flight never reached a confirmed end (its tails were not joined): the key stays owned.
    assert.equal(r.rt.heldByOtherSink(r.clone, new Set()), true);
    assert.equal(r.rt.heldByOtherSink(r.clone, first), false);
    const before = authorityBytes(r.root);
    const second = new Set<string>();
    await assert.rejects(bindIn(r.rt, second, r.clone), isHeld, "bindSource by a later sink");
    await assert.rejects(hookIn(r.rt, second, r.clone, "seed-canonical"), isHeld, "a seed hook by a later sink");
    await assert.rejects(hookIn(r.rt, second, r.clone, "normalize"), isHeld, "a normalize by a later sink");
    await assert.rejects(r.rt.withTransaction(() => r.rt.hooks.admitSourceUse(r.clone, "seed-canonical")), isHeld,
      "an admission outside any sink");
    assert.equal(authorityBytes(r.root), before, "refused before any effect");
    assert.equal((await slotOf(r.rt, r.clone))?.epoch, leaseEpoch(l1));

    const outcome = await r.rt.endSourceUse([r.clone]);
    assert.equal(outcome.endedSlots.length, 1);
    assert.equal(r.rt.heldByOtherSink(r.clone, second), false);
    const l2 = await bindIn(r.rt, second, r.clone);
    assert.notEqual(leaseEpoch(l2), leaseEpoch(l1));
  });

  it("the same sink rebinds and normalizes freely", async (t) => {
    const r = await rig(t);
    const sink = new Set<string>();
    const l1 = await bindIn(r.rt, sink, r.clone);
    assert.equal(await bindIn(r.rt, sink, r.clone), l1, "the held lease is kept");
    await hookIn(r.rt, sink, r.clone, "normalize");
    assert.equal((await slotOf(r.rt, r.clone))?.epoch, leaseEpoch(l1), "a normalize keeps the epoch");
    const codex = await bindIn(r.rt, sink, r.clone, "exec-1");
    assert.notEqual(leaseEpoch(codex), leaseEpoch(l1), "a codex rebind mints a fresh epoch inside the same sink");
    await hookIn(r.rt, sink, r.clone, "seed-canonical");
  });

  it("a kept end (a running pending record) keeps the key owned until the drain applies it", async (t) => {
    const r = await rig(t);
    const first = new Set<string>();
    await bindIn(r.rt, first, r.clone);
    r.rt.retireSink(first);
    const settlement = await r.rt.withTransaction(() => r.rt.hooks.beginDestructive([r.clone], "runner-uid-delete"));
    const outcome = await r.rt.endSourceUse([r.clone]);
    assert.deepEqual(outcome.kept, [r.clone]);
    assert.deepEqual(outcome.endedSlots, [], "a kept end is not a confirmed end");
    const next = new Set<string>();
    await assert.rejects(bindIn(r.rt, next, r.clone), isHeld);
    await r.rt.withTransaction(() => settlement.settle({ ok: true }));
    await r.rt.withTransaction(async () => undefined); // acquiring A applies the queued end
    assert.equal((await slotOf(r.rt, r.clone))?.state, "ineligible");
    assert.equal(r.rt.heldByOtherSink(r.clone, next), false, "the drain's confirmed end released the key (no late receipt)");
    await bindIn(r.rt, next, r.clone);
  });

  it("a failed end write keeps the key owned and reports no confirmed end", async (t) => {
    const flags = { head: false };
    const r = await rig(t, {
      ops: {
        syncFile: (h) => h.sync(), syncDir: (h) => h.sync(),
        rename: async (a, b) => {
          if (flags.head && path.basename(path.dirname(b)) === "heads") throw new Error("injected head failure");
          fs.renameSync(a, b);
        },
      },
      retryMs: 3_600_000,
    });
    const first = new Set<string>();
    await bindIn(r.rt, first, r.clone);
    flags.head = true;
    const outcome = await r.rt.endSourceUse([r.clone]);
    assert.deepEqual(outcome.endedSlots, [], "a transition that failed adds no entry");
    assert.deepEqual(outcome.ended, []);
    assert.equal(r.rt.heldByOtherSink(r.clone, new Set()), true);
    // Still failing: the queued end cannot be applied by the next transaction either, so the key stays owned.
    await assert.rejects(bindIn(r.rt, new Set(), r.clone), isHeld);
    flags.head = false;
    // Once the write works the next transaction's drain ends it and the key is free again (no late receipt).
    await bindIn(r.rt, new Set(), r.clone);
  });

  it("a paused A keeps the key owned; the queued end is applied once A is free", async (t) => {
    const r = await rig(t, { retryMs: 20 });
    const first = new Set<string>();
    await bindIn(r.rt, first, r.clone);
    r.rt.retireSink(first);
    const { withAuthorityTransaction } = await import("../src/authority-lock.js");
    let release!: () => void;
    const held = new Promise<void>((resolve) => { release = resolve; });
    let acquired!: () => void;
    const holding = new Promise<void>((resolve) => { acquired = resolve; });
    const holder = withAuthorityTransaction(r.lifetime, { timeoutMs: 5000 }, async () => { acquired(); await held; });
    await holding;
    const outcome = await r.rt.endSourceUse([r.clone], { timeoutMs: 100 });
    assert.deepEqual(outcome.endedSlots, []);
    assert.equal(r.rt.heldByOtherSink(r.clone, new Set()), true, "owned while the end is only queued");
    release();
    await holder;
    for (let i = 0; i < 200 && r.rt.heldByOtherSink(r.clone, new Set()); i++) await new Promise((resolve) => setTimeout(resolve, 20));
    assert.equal(r.rt.heldByOtherSink(r.clone, new Set()), false, "the drain applied the end and released the key");
    assert.equal((await slotOf(r.rt, r.clone))?.state, "ineligible");
  });

  it("a late old end after a successor epoch is dropped, and the drop releases the stale owner", async (t) => {
    const r = await rig(t);
    const first = new Set<string>();
    const l1 = await bindIn(r.rt, first, r.clone);
    r.rt.retireSink(first);
    // Another epoch of this very worker replaces the slot out of band (an independent store).
    const l2epoch = await r.rt.withTransaction(async (tx) => {
      const store = await openAuthorityStore(tx, { workerToken: TOKEN });
      const lease = await store.admitSourceUse(tx, r.clone);
      return leaseEpoch(lease);
    });
    assert.notEqual(l2epoch, leaseEpoch(l1));
    const outcome = await r.rt.endSourceUse([r.clone]);
    assert.deepEqual(outcome.endedSlots, [], "dropped is not an end");
    assert.deepEqual(outcome.ended, []);
    const slot = await slotOf(r.rt, r.clone);
    assert.equal(slot?.state, "active");
    assert.equal(slot?.epoch, l2epoch, "the successor epoch is untouched");
    assert.equal(r.rt.heldByOtherSink(r.clone, new Set()), false, "no wedge: the recorded epoch is provably gone");
  });

  it("a slot that left active behind the runtime's back is rechecked at the end, so a recorded owner cannot wedge the path", async (t) => {
    const r = await rig(t);
    const first = new Set<string>();
    await bindIn(r.rt, first, r.clone);
    r.rt.retireSink(first);
    assert.equal(r.rt.heldByOtherSink(r.clone, new Set()), true, "owned before the out-of-band end");
    // An independent store of this very worker replaces the epoch and ends it: the slot is no longer `active`,
    // and the runtime's own lease and owner record were never told.
    await r.rt.withTransaction(async (tx) => {
      const store = await openAuthorityStore(tx, { workerToken: TOKEN });
      const lease = await store.admitSourceUse(tx, r.clone);
      await store.transition(tx, lease, "ineligible");
    });
    assert.equal((await slotOf(r.rt, r.clone))?.state, "ineligible");
    assert.equal(r.rt.heldByOtherSink(r.clone, new Set()), true, "nothing released the owner yet");
    const outcome = await r.rt.endSourceUse([r.clone, r.other]);
    assert.deepEqual(outcome.endedSlots, [], "an end this call did not write is not a confirmed end");
    assert.deepEqual(outcome.ended, []);
    assert.deepEqual(outcome.kept, [r.other], "a key nobody holds is only reported");
    assert.equal(r.rt.heldByOtherSink(r.clone, new Set()), false, "the recheck saw the slot gone and released the owner");
    await bindIn(r.rt, new Set(), r.clone);
  });

  it("an end named by a different sink leaves the owner's slot active, queues nothing and releases nothing", async (t) => {
    const r = await rig(t);
    const a = new Set<string>();
    const b = new Set<string>();
    const lease = await bindIn(r.rt, a, r.clone);
    r.rt.retireSink(a);
    const before = authorityBytes(r.root);
    const outcome = await r.rt.endSourceUse([r.clone], { sink: b });
    assert.deepEqual(outcome.endedSlots, [], "no end receipt for a key B does not own");
    assert.deepEqual(outcome.ended, []);
    assert.deepEqual(outcome.kept, [r.clone]);
    await r.rt.withTransaction(async () => undefined); // a queued end would be applied by acquiring A
    await new Promise((resolve) => setTimeout(resolve, 50));
    assert.equal(authorityBytes(r.root), before, "nothing was written");
    const slot = await slotOf(r.rt, r.clone);
    assert.equal(slot?.state, "active");
    assert.equal(slot?.epoch, leaseEpoch(lease));
    assert.equal(r.rt.heldByOtherSink(r.clone, b), true, "A is still the owner");
    assert.equal(r.rt.heldByOtherSink(r.clone, a), false);
    // A's own end still works afterwards.
    const own = await r.rt.endSourceUse([r.clone], { sink: a });
    assert.equal(own.endedSlots.length, 1);
    assert.equal((await slotOf(r.rt, r.clone))?.state, "ineligible");
  });

  it("a multi-path end reports only the paths this call ended; a kept sibling has no receipt", async (t) => {
    const r = await rig(t);
    const sink = new Set<string>();
    await bindIn(r.rt, sink, r.clone);
    await bindIn(r.rt, sink, r.other);
    r.rt.retireSink(sink);
    // A running destructive record on `other` keeps its end pending.
    const settlement = await r.rt.withTransaction(() => r.rt.hooks.beginDestructive([r.other], "runner-uid-delete"));
    const outcome = await r.rt.endSourceUse([r.clone, r.other], { sink });
    assert.deepEqual(outcome.ended, [r.clone]);
    assert.deepEqual(outcome.kept, [r.other]);
    assert.deepEqual(outcome.endedSlots.map((e) => e.path), [r.clone], "only the ended path has a receipt");
    assert.equal((await slotOf(r.rt, r.other))?.state, "active");
    await r.rt.withTransaction(() => settlement.settle({ ok: true }));
  });

  it("a refused admission (a pending footprint) records no owner", async (t) => {
    const r = await rig(t);
    // A destructive record on a bare, unowned slot: footprint pending, no flight sink involved.
    await r.rt.withTransaction(() => r.rt.hooks.admitSourceUse(r.clone, "seed-canonical"));
    await r.rt.withTransaction(() => r.rt.hooks.beginDestructive([r.clone], "runner-uid-delete"));
    const sink = new Set<string>();
    await assert.rejects(bindIn(r.rt, sink, r.clone, "exec-1"), /unresolved pending footprint/);
    assert.equal(r.rt.heldByOtherSink(r.clone, new Set()), false, "the refusal created no owner");
    assert.deepEqual([...sink], [r.clone], "the path is named for endSourceUse");
  });

  it("an operator discard of an owned key succeeds and its own end releases the owner", async (t) => {
    const r = await rig(t);
    const first = new Set<string>();
    await bindIn(r.rt, first, r.clone);
    r.rt.retireSink(first);
    assert.equal(r.rt.heldByOtherSink(r.clone, new Set()), true);
    // The discard is unguarded: it proves its writers gone itself.
    await r.rt.withTransaction(() => r.rt.hooks.admitSourceUse(r.clone, "discard"));
    assert.equal((await slotOf(r.rt, r.clone))?.state, "retiring");
    const outcome = await r.rt.endSourceUse([r.clone]);
    assert.deepEqual(outcome.ended, [r.clone]);
    assert.deepEqual(outcome.endedSlots, [], "a discard's end is not a cooperative end receipt");
    assert.equal(r.rt.heldByOtherSink(r.clone, new Set()), false);
    await bindIn(r.rt, new Set(), r.clone);
  });

  it("a destructive record is unguarded and never creates an owner", async (t) => {
    const r = await rig(t);
    const settlement = await r.rt.withTransaction(() => r.rt.hooks.beginDestructive([r.other], "runner-uid-delete"));
    assert.equal(r.rt.heldByOtherSink(r.other, new Set()), false);
    await r.rt.withTransaction(() => settlement.settle({ ok: true }));
  });

  it("the ending receipt names the slot's saved identity", async (t) => {
    const r = await rig(t);
    const sink = new Set<string>();
    const lease = await bindIn(r.rt, sink, r.clone);
    const [receipt] = (await r.rt.endSourceUse([r.clone])).endedSlots;
    const slot = await slotOf(r.rt, r.clone);
    assert.deepEqual(receipt, { path: r.clone, key: slot!.key, epoch: leaseEpoch(lease), seq: slot!.seq, worker: slot!.owner.worker });
    assert.equal(slot!.state, "ineligible");
  });

  it("a different runtime (another worker incarnation) has its own ownership map", async (t) => {
    const r = await rig(t);
    const first = new Set<string>();
    await bindIn(r.rt, first, r.clone);
    const next = new AuthorityRuntime({ lifetime: r.lifetime, workerToken: TOKEN, workerIncarnation: randomUUID(), timeoutMs: 5000 });
    await assert.rejects(bindIn(next, new Set(), r.clone), /active under another worker incarnation/, "refused by the slot, not by ownership");
    assert.equal(next.heldByOtherSink(r.clone, new Set()), false);
  });
});
