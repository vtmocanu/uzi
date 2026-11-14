import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { asWriter, HibernationBusyError, HibernationWriterRegistry } from "../src/hibernation-inventory.js";

const tick = (ms = 5) => new Promise<void>((r) => setTimeout(r, ms));

describe("HibernationWriterRegistry", () => {
  it("refuses enter while latched and admits again after unlatch", () => {
    const r = new HibernationWriterRegistry();
    const rel = r.enter("a");
    assert.ok(rel);
    r.latch();
    assert.equal(r.enter("b"), null);
    rel();
    rel(); // idempotent
    assert.equal(r.inFlight(), 0);
    r.unlatch();
    assert.ok(r.enter("b"));
  });

  it("settle waits for a writer that entered before the latch, then resolves", async () => {
    const r = new HibernationWriterRegistry();
    let proceed!: () => void;
    const gate = new Promise<void>((res) => { proceed = res; });
    const writer = asWriter("slow", () => gate, r);
    assert.equal(r.inFlight(), 1);
    r.latch();
    let settled = false;
    const settle = r.settle(2000).then(() => { settled = true; });
    await tick(80);
    assert.equal(settled, false);
    assert.equal(r.enter("late"), null, "a new enter is refused during settle");
    proceed();
    await writer;
    await settle;
    assert.equal(settled, true);
  });

  it("settle rejects with busy on timeout and names the classes", async () => {
    const r = new HibernationWriterRegistry();
    r.enter("stuck");
    r.latch();
    await assert.rejects(r.settle(60), (e: unknown) => e instanceof HibernationBusyError && /stuck/.test(e.message));
  });

  it("settle also requires the external trackers to be idle", async () => {
    const r = new HibernationWriterRegistry();
    r.latch();
    let idle = false;
    setTimeout(() => { idle = true; }, 60);
    await r.settle(2000, undefined, () => idle);
    assert.equal(idle, true);
    await assert.rejects(r.settle(40, undefined, () => false), HibernationBusyError);
  });

  it("asWriter skips while latched and releases on failure", async () => {
    const r = new HibernationWriterRegistry();
    await assert.rejects(asWriter("x", async () => { throw new Error("boom"); }, r), /boom/);
    assert.equal(r.inFlight(), 0);
    r.latch();
    assert.deepEqual(await asWriter("x", async () => 1, r), { ran: false });
  });

  it("reports uncovered classes", () => {
    const r = new HibernationWriterRegistry();
    r.markUncovered("z");
    r.markUncovered("a");
    assert.deepEqual(r.uncovered(), ["a", "z"]);
  });
});
