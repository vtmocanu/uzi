import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { SinkGate } from "../src/sink-gate.js";

// issue #1597 M2 — the per-flight sink gate: gated paths serialise, a tick only try-acquires and is
// preempted by a gated path (which still waits for it to settle), and the owner may re-enter.

const tick = (): Promise<void> => new Promise((r) => setImmediate(r));

describe("SinkGate (issue #1597 M2)", () => {
  it("serialises gated paths", async () => {
    const gate = new SinkGate();
    const order: string[] = [];
    let releaseA!: () => void;
    const a = gate.run(async () => {
      order.push("a:start");
      await new Promise<void>((r) => (releaseA = r));
      order.push("a:end");
    });
    const b = gate.run(async () => {
      order.push("b");
    });
    await tick();
    assert.deepEqual(order, ["a:start"]);
    releaseA();
    await Promise.all([a, b]);
    assert.deepEqual(order, ["a:start", "a:end", "b"]);
    assert.equal(gate.isHeld(), false);
  });

  it("tryAcquire misses while a gated path holds the gate or is queued", async () => {
    const gate = new SinkGate();
    let release!: () => void;
    const held = gate.run(() => new Promise<void>((r) => (release = r)));
    await tick();
    assert.equal(gate.tryAcquire(() => undefined), null);
    release();
    await held;
    const rel = gate.tryAcquire(() => undefined);
    assert.ok(rel);
    rel();
  });

  it("a gated path preempts a tick holder and waits until it releases", async () => {
    const gate = new SinkGate();
    const events: string[] = [];
    let settle!: () => void;
    const settled = new Promise<void>((r) => (settle = r));
    const release = gate.tryAcquire(() => {
      events.push("preempted");
      // The tick settles asynchronously after being asked to stop.
      setTimeout(() => {
        events.push("tick:settled");
        settle();
      }, 20);
    })!;
    void settled.then(release);
    const path = gate.run(async () => {
      events.push("path");
    });
    // A second queued path does not re-fire preempt.
    const path2 = gate.run(async () => {
      events.push("path2");
    });
    await Promise.all([path, path2]);
    assert.deepEqual(events, ["preempted", "tick:settled", "path", "path2"]);
  });

  it("is re-entrant for the owning async call tree (no self-deadlock)", async () => {
    const gate = new SinkGate();
    const out = await gate.run(async () => {
      await tick();
      return gate.run(async () => gate.run(async () => "nested"));
    });
    assert.equal(out, "nested");
    assert.equal(gate.isHeld(), false);
  });

  it("a timer created inside run() that fires AFTER release does not inherit the ownership (review item 10)", async () => {
    const gate = new SinkGate();
    const events: string[] = [];
    let deferred!: Promise<void>;
    await gate.run(async () => {
      deferred = new Promise<void>((resolve) => {
        setTimeout(() => {
          void gate.run(async () => {
            events.push("deferred:ran");
          }).then(resolve);
        }, 20);
      });
    });
    // Another holder takes the gate before the timer fires; the deferred call must WAIT for it.
    let releaseOther!: () => void;
    const other = gate.run(async () => {
      events.push("other:start");
      await new Promise<void>((r) => (releaseOther = r));
      events.push("other:end");
    });
    await new Promise((r) => setTimeout(r, 60));
    assert.deepEqual(events, ["other:start"], "the deferred call is queued, not let through");
    releaseOther();
    await Promise.all([other, deferred]);
    assert.deepEqual(events, ["other:start", "other:end", "deferred:ran"]);
  });

  it("releases on a throw", async () => {
    const gate = new SinkGate();
    await assert.rejects(gate.run(async () => { throw new Error("x"); }), /x/);
    assert.equal(gate.isHeld(), false);
  });
});
