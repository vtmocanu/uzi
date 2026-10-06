import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";
import os from "node:os";
import { CandidateReservationRefusedError, MessageBatcher, replaySegment } from "../src/batcher.js";
import { Outbox, StaleClaimError, type RawWriteSeam } from "../src/outbox.js";
import { RequestError, type PlanCrossCheckResponse, type WorkerClient } from "../src/client.js";
import type { OutgoingMessage } from "../src/protocol.js";
import { nullLogger } from "./helpers.js";
import { makeRedactor } from "../src/redact.js";

const runId = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa";
function proof(tail = 4): PlanCrossCheckResponse {
  return {
    result: "candidate", verdict: "revise", candidate_generation: 3, lead_last_seq: tail,
    reconciliation: { leadLastSeq: tail, claimGeneration: 3, planCrossCheckSettled: true,
      gateRevision: 0, currentPlanSHA256: "a".repeat(64) },
  } as PlanCrossCheckResponse;
}
function fixture(post?: WorkerClient["postMessages"], outbox?: Outbox, cap?: number, batchMs = 60_000) {
  const sent: OutgoingMessage[][] = [];
  const client = {
    async postUsage() {},
    async postMessages(id: string, msgs: OutgoingMessage[], gen?: number, signal?: AbortSignal) {
      sent.push(structuredClone(msgs));
      await post?.(id, msgs, gen, signal);
    },
  } as unknown as WorkerClient;
  const b = new MessageBatcher(client, runId, 0, batchMs, nullLogger(), undefined, undefined,
    { generation: 3, outbox, spillBufferBytes: cap });
  return { b, sent };
}
const event = (text = "event") => ({ kind: "status" as const, payload: { text } });

it("waits existing ACK ownership; buffers during preparation and releases after terminal server tail", async () => {
  let ack!: () => void;
  let started!: () => void;
  const start = new Promise<void>((resolve) => { started = resolve; });
  const { b, sent } = fixture(async () => {
    if (sent.length === 1) {
      started();
      await new Promise<void>((resolve) => { ack = resolve; });
    }
  });
  b.emit(event("old"));
  const flush = b.flush();
  await start;
  const r = b.reserveCandidateTransport();
  const prepared = r.prepare();
  b.emit(event("held"));
  b.rearm();
  await b.flush();
  assert.equal(b.currentSeq(), 1);
  ack();
  await flush;
  assert.deepEqual(await prepared, { prepared: true, tail: 1 });
  for (const bad of [
    { ...proof(), verdict: "pending" }, { ...proof(), candidate_generation: 2 },
    { ...proof(), reconciliation: undefined },
    { ...proof(), reconciliation: { ...proof().reconciliation!, claimGeneration: 2 } },
    { ...proof(), reconciliation: { ...proof().reconciliation!, planCrossCheckSettled: false } },
    { ...proof(), lead_last_seq: 5 }, proof(0), proof(1.5), proof(0x80000000),
    { ...proof(), result: "no_row" },
  ]) {
    assert.equal(r.release(bad as PlanCrossCheckResponse), false);
    assert.equal(b.currentSeq(), 1);
  }
  assert.throws(() => b.reserveCandidateTransport());
  assert.equal(r.release(proof(4)), true); // server verdict occupies the intervening tail
  await b.flush();
  b.emit(event("later lead"));
  await b.flush();
  assert.deepEqual(sent.slice(1).flat().map((m) => [m.seq, m.payload]),
    [[5, { text: "held" }], [6, { text: "later lead" }]]);
  const next = b.reserveCandidateTransport();
  assert.equal(r.release(proof(8)), false);
  next.cancel();
  await b.close();
  assert.equal(b.currentSeq(), 6);
});

it("lost ACK retry retains identical old seq/body; failed preparation cannot release", async () => {
  let fail = true;
  const { b, sent } = fixture(async () => { if (fail) throw new Error("lost ACK"); });
  b.emit(event("original"));
  const r = b.reserveCandidateTransport();
  b.emit(event("later"));
  const failed = await r.prepare();
  assert.equal(failed.prepared, false);
  assert.equal(failed.permanent, false);
  b.emit(event("after failed prepare"));
  await b.flush();
  assert.equal(sent.length, 1, "ordinary flush stays blocked while held");
  assert.equal(r.release(proof()), false);
  fail = false;
  assert.equal((await r.prepare()).prepared, true);
  assert.deepEqual(sent[0], sent[1]);
  assert.equal(b.currentSeq(), 1);
  assert.equal(r.release(proof(2)), true);
  await b.close();
  assert.deepEqual(sent[2]?.map((m) => [m.seq, m.payload]),
    [[3, { text: "later" }], [4, { text: "after failed prepare" }]]);
});

it("cancel and close abort and await preparation without numbering held events", async () => {
  let started!: () => void;
  let settled = false;
  const start = new Promise<void>((resolve) => { started = resolve; });
  const { b } = fixture(async (_id, _msgs, _gen, signal) => {
    started();
    await new Promise<void>((_, reject) => {
      signal?.addEventListener("abort", () => { settled = true; reject(new Error("aborted")); }, { once: true });
    });
  });
  b.emit(event());
  const r = b.reserveCandidateTransport();
  const pending = r.prepare();
  await start;
  b.emit(event("held"));
  r.cancel();
  await b.close();
  assert.equal((await pending).prepared, false);
  assert.equal(settled, true);
  assert.equal(b.currentSeq(), 1);
  assert.equal(r.release(proof()), false);
});

it("sanitizes/redacts held events immediately and refuses bounded overflow", async () => {
  const secret = "private-" + "fixture-value";
  const sent: OutgoingMessage[][] = [];
  const b = new MessageBatcher({ async postUsage() {}, async postMessages(_id: string, msgs: OutgoingMessage[]) {
    sent.push(structuredClone(msgs));
  } } as unknown as WorkerClient, runId, 0, 60_000, nullLogger(), makeRedactor([secret]), undefined,
  { generation: 3, spillBufferBytes: 180 });
  const r = b.reserveCandidateTransport();
  const payload = { text: "private-\u0000fixture-value" };
  b.emit({ kind: "status", payload });
  payload.text = "mutated";
  assert.equal((await r.prepare()).prepared, true);
  assert.equal(r.release(proof(1)), true);
  await b.flush();
  assert.equal(JSON.stringify(sent).includes(secret), false);
  assert.equal(JSON.stringify(sent).includes("mutated"), false);
  const next = b.reserveCandidateTransport();
  b.emit(event());
  assert.throws(() => b.emit(event("x".repeat(200))));
  assert.equal(b.currentSeq(), 2);
  assert.equal((await next.prepare()).prepared, false);
  await b.close();
});

async function withOutbox(body: (o: Outbox, root: string) => Promise<void>, rawWrite?: RawWriteSeam, runMaxBytes = 64 * 1024 * 1024) {
  const root = await fs.mkdtemp(path.join(await fs.realpath(os.tmpdir()), "reservation-outbox-"));
  try {
    const o = new Outbox({ root: path.join(root, "outbox"), log: nullLogger(),
      runMaxBytes, maxBytes: 512 * 1024 * 1024, retentionMs: 86400_000, rawWrite });
    await o.init();
    await body(o, path.join(root, "outbox"));
  } finally { await fs.rm(root, { recursive: true, force: true }); }
}
it("real durable segment and range replay retain generation and sequence, with concurrent rearm", async () => {
  await withOutbox(async (o) => {
    const original = { seq: 1, kind: "status" as const, payload: { text: "durable" } };
    await o.appendSegment(runId, 3, [original]);
    await o.appendRangeRecord(runId, 3, 2, 3);
    const sent: OutgoingMessage[][] = [];
    const b = new MessageBatcher({ async postUsage() {},
      async postMessages(_id: string, msgs: OutgoingMessage[], generation: number) {
        assert.equal(generation, 3);
        sent.push(structuredClone(msgs));
        b.rearm();
        b.emit(event("held"));
      } } as unknown as WorkerClient, runId, 3, 60_000, nullLogger(), undefined, undefined,
      { generation: 3, outbox: o });
    const r = b.reserveCandidateTransport();
    assert.equal((await r.prepare()).prepared, true);
    assert.deepEqual(sent[0], [original]);
    assert.deepEqual(sent[1]?.map((m) => m.seq), [2, 3]);
    assert.equal(o.hasUndrainedMessages(runId), false);
    assert.equal(b.currentSeq(), 3);
    await b.close();
  });
});
it("wrong-generation and failed durable ACK stay closed without retiring records", async () => {
  for (const generation of [2, 3]) await withOutbox(async (o) => {
    await o.appendSegment(runId, generation, [{ seq: 1, kind: "status", payload: {} }]);
    const { b } = fixture(async () => { throw new Error("ACK unknown"); }, o);
    b.emit(event("assigned"));
    const r = b.reserveCandidateTransport();
    assert.equal((await r.prepare()).prepared, false);
    assert.equal(r.release(proof()), false);
    assert.equal(o.hasUndrainedMessages(runId), true);
    await b.close();
  });
});
it("external retirement without this reservation's ACK proof fails closed", async () => {
  await withOutbox(async (o) => {
    await o.appendSegment(runId, 3, [{ seq: 1, kind: "status", payload: {} }]);
    const b = new MessageBatcher({ async postUsage() {}, async postMessages() {} } as unknown as WorkerClient,
      runId, 1, 60_000, nullLogger(), undefined, undefined, { generation: 3, outbox: o });
    const r = b.reserveCandidateTransport();
    await o.drainRun(runId, async () => {});
    assert.equal((await r.prepare()).prepared, false);
    assert.equal(r.release(proof()), false);
    await b.close();
  });
});

for (const stop of ["deadline", "cancel", "close", "close-with-tail"] as const) {
  it(`queued preparation and ${stop} settle without releasing an independent drainer's lock`, async () => {
    await withOutbox(async (o) => {
      const original = { seq: 1, kind: "status" as const, payload: { text: "independent" } };
      await o.appendSegment(runId, 3, [original]);
      const identities = o.pendingDeliveryIdentities(runId);
      let entered!: () => void;
      const started = new Promise<void>((resolve) => { entered = resolve; });
      let release!: () => void;
      const held = new Promise<void>((resolve) => { release = resolve; });
      let ownerSettled = false;
      const owner = o.drainRun(runId, async () => {
        entered();
        await held;
        throw new Error("independent ACK unknown");
      }).then((result) => { ownerSettled = true; return result; });
      await started;
      const sent: OutgoingMessage[][] = [];
      const b = new MessageBatcher({ async postUsage() {}, async postMessages(_id: string, msgs: OutgoingMessage[]) {
        sent.push(msgs);
      } } as unknown as WorkerClient, runId, 1, 60_000, nullLogger(), undefined, undefined,
      { generation: 3, outbox: o });
      if (stop === "close-with-tail") b.emit(event("assigned tail"));
      const r = b.reserveCandidateTransport();
      b.emit(event("unassigned"));
      const preparing = r.prepare(stop === "deadline" ? 10 : 3000);
      // Let preparation reach the outbox queue before cancelling its owner.
      await new Promise<void>((resolve) => { setImmediate(resolve); });
      let watchdog: ReturnType<typeof setTimeout> | undefined;
      const bounded = async <T>(work: Promise<T>): Promise<T> => {
        try {
          return await Promise.race([work, new Promise<never>((_, reject) => {
            watchdog = setTimeout(() => reject(new Error("queued operation exceeded 100ms")), 100);
          })]);
        } finally { clearTimeout(watchdog); }
      };
      let later: Promise<{ retired: boolean; staleRetired: number }> | undefined;
      let laterSent = false;
      try {
        if (stop === "cancel") r.cancel();
        if (stop.startsWith("close")) await bounded(b.close(AbortSignal.timeout(10)));
        assert.equal((await bounded(preparing)).prepared, false);
        if (!stop.startsWith("close")) await bounded(b.close(AbortSignal.abort()));
        assert.equal(ownerSettled, false);
        assert.equal(sent.length, 0);
        assert.deepEqual(o.pendingDeliveryIdentities(runId), identities);
        assert.equal(b.bufferedCount(), stop === "close-with-tail" ? 1 : 0);
        assert.equal(b.currentSeq(), stop === "close-with-tail" ? 2 : 1);
        assert.equal(r.release(proof()), false);
        later = o.drainRun(runId, async (msgs) => {
          laterSent = true;
          assert.deepEqual(msgs, [original]);
        });
        await new Promise<void>((resolve) => { setImmediate(resolve); });
        assert.equal(laterSent, false, "cancelled acquisition must not let a later drain overtake the live owner");
      } finally {
        release();
        assert.equal((await owner).retired, false);
        await preparing;
        await b.close(AbortSignal.abort());
        if (later) assert.equal((await later).retired, true);
      }
      assert.equal(laterSent, true);
      assert.equal(sent.length, 0, "cancelled queued callback must never run after the owner releases");
      assert.equal(o.hasUndrainedMessages(runId), false);
    });
  });
}

it("cancelled queued outbox writes neither commit nor clear the unclean marker later", async () => {
  let writes = 0;
  await withOutbox(async (o, root) => {
    const original = { seq: 1, kind: "status" as const, payload: {} };
    await o.appendSegment(runId, 3, [original]);
    await o.markSpillUnclean(runId);
    const baselineWrites = writes;
    const identities = o.pendingDeliveryIdentities(runId);
    let entered!: () => void;
    const started = new Promise<void>((resolve) => { entered = resolve; });
    let release!: () => void;
    const held = new Promise<void>((resolve) => { release = resolve; });
    const owner = o.drainRun(runId, async () => {
      entered();
      await held;
      throw new Error("owner retains record");
    });
    await started;
    const controller = new AbortController();
    let committed = 0;
    const writesQueued = [
      o.appendSegment(runId, 3, [{ ...original, seq: 2 }], () => { committed++; }, controller.signal),
      o.appendRangeRecord(runId, 3, 3, 4, () => { committed++; }, controller.signal),
      o.markSpillUnclean(runId, controller.signal),
      o.clearSpillUnclean(runId, controller.signal),
    ];
    const settled = Promise.allSettled(writesQueued);
    controller.abort();
    let timeout: ReturnType<typeof setTimeout> | undefined;
    try {
      const results = await Promise.race([settled, new Promise<never>((_, reject) => {
        timeout = setTimeout(() => reject(new Error("queued writes exceeded 100ms")), 100);
      })]);
      assert.equal(results.every((result) => result.status === "rejected" &&
        result.reason === controller.signal.reason), true);
      assert.equal(writes, baselineWrites);
      assert.equal(committed, 0);
    } finally {
      clearTimeout(timeout);
      release();
      await owner;
      await settled;
    }
    // This drain is behind all cancelled slots, so inspect their final effects.
    const delivered: OutgoingMessage[] = [];
    const barrier = await o.drainRun(runId, async (msgs) => {
      delivered.push(...msgs);
      throw new Error("keep original");
    });
    assert.equal(barrier.retired, false);
    assert.deepEqual(delivered, [original]);
    assert.equal(writes, baselineWrites);
    assert.equal(committed, 0);
    assert.deepEqual(o.pendingDeliveryIdentities(runId), identities);
    const restarted = new Outbox({ root, log: nullLogger(), runMaxBytes: 64 * 1024 * 1024,
      maxBytes: 512 * 1024 * 1024, retentionMs: 86400_000 });
    await restarted.init();
    assert.equal(restarted.uncleanRuns().includes(runId), true);
  }, async (write) => { writes++; await write(); });
});

it("close awaits the real failure of a running owned delivery and preserves its durable record", async () => {
  await withOutbox(async (o) => {
    const original = { seq: 1, kind: "status" as const, payload: { text: "ACK unknown" } };
    await o.appendSegment(runId, 3, [original]);
    let entered!: () => void;
    const started = new Promise<void>((resolve) => { entered = resolve; });
    let release!: () => void;
    const held = new Promise<void>((resolve) => { release = resolve; });
    let requestSignal: AbortSignal | undefined;
    const b = new MessageBatcher({ async postUsage() {}, async postMessages(
      _id: string, _msgs: OutgoingMessage[], _generation: number, signal?: AbortSignal,
    ) {
      requestSignal = signal;
      entered();
      await held;
      throw new Error("delivery failed after cancellation");
    } } as unknown as WorkerClient, runId, 1, 60_000, nullLogger(), undefined, undefined,
    { generation: 3, outbox: o });
    const r = b.reserveCandidateTransport();
    let preparedSettled = false;
    const preparing = r.prepare().then((result) => { preparedSettled = true; return result; });
    await started;
    let closed = false;
    const closing = b.close(AbortSignal.abort()).then(() => { closed = true; });
    try {
      await new Promise<void>((resolve) => { setImmediate(resolve); });
      assert.equal(requestSignal?.aborted, true);
      assert.equal(preparedSettled, false);
      assert.equal(closed, false);
      assert.equal(o.hasUndrainedMessages(runId), true);
    } finally { release(); await closing; }
    assert.equal((await preparing).prepared, false);
    assert.equal(r.release(proof()), false);
    assert.equal((await o.drainRun(runId, async (msgs) => {
      assert.deepEqual(msgs, [original]);
    })).retired, true);
  });
});

async function within100ms<T>(work: Promise<T>): Promise<T> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  try {
    return await Promise.race([work, new Promise<never>((_, reject) => {
      timer = setTimeout(() => reject(new Error("owned queued spill exceeded 100ms")), 100);
    })]);
  } finally { clearTimeout(timer); }
}

for (const stage of ["marker", "range", "segment", "clear", "transition"] as const) {
  for (const stop of ["prepare", "close"] as const) {
    it(`queued ${stage} spill acquisition cancels on ${stop} without overtaking or late writes`, async () => {
      await withOutbox(async (o) => {
        const sent: OutgoingMessage[][] = [];
        const b = new MessageBatcher({ async postUsage() {},
          async postMessages(_id: string, msgs: OutgoingMessage[]) {
            sent.push(structuredClone(msgs));
            throw new Error("offline");
          },
        } as unknown as WorkerClient, runId, 0, 60_000, nullLogger(), undefined, undefined,
        { generation: 3, outbox: o, transientTripMs: 0, spillBufferBytes: 256 });
        b.emit(event("durable seed"));
        await b.flush();
        await b.flush();
        assert.equal(b.isSpilled(), true);
        if (stage === "transition") b.rearm();
        b.emit(event("assigned tail"));
        if (stage === "range") b.emit(event("dropped".repeat(100)));
        const tail = b.currentSeq();
        let entered!: () => void;
        const started = new Promise<void>((resolve) => { entered = resolve; });
        let release!: () => void;
        const held = new Promise<void>((resolve) => { release = resolve; });
        let ownerSettled = false;
        let owner: ReturnType<Outbox["drainRun"]> | undefined;
        let queued!: () => void;
        const queueReady = new Promise<void>((resolve) => { queued = resolve; });
        let armed = true;
        const acquire = async (work: () => Promise<void>) => {
          if (armed) {
            armed = false;
            owner = o.drainRun(runId, async () => {
              entered();
              await held;
              throw new Error("independent ACK unknown");
            }).then((result) => { ownerSettled = true; return result; });
            await started;
            const pending = work();
            queued();
            await pending;
          } else await work();
        };
        if (stage === "marker" || stage === "transition") {
          const original = o.markSpillUnclean.bind(o);
          o.markSpillUnclean = (...args) => acquire(() => original(...args));
        } else if (stage === "range") {
          const original = o.appendRangeRecord.bind(o);
          o.appendRangeRecord = (...args) => acquire(() => original(...args));
        } else if (stage === "segment") {
          const original = o.appendSegment.bind(o);
          o.appendSegment = (...args) => acquire(() => original(...args));
        } else {
          const original = o.clearSpillUnclean.bind(o);
          o.clearSpillUnclean = (...args) => acquire(() => original(...args));
        }
        const flushing = b.flush();
        await queueReady;
        const identities = o.pendingDeliveryIdentities(runId);
        const count = stage === "segment" ? 1 : b.bufferedCount();
        const posts = sent.length;
        let preparing: Promise<{ prepared: boolean; tail: number }> | undefined;
        let later: ReturnType<Outbox["drainRun"]> | undefined;
        let laterSent = false;
        try {
          if (stop === "prepare") {
            const r = b.reserveCandidateTransport();
            preparing = r.prepare(10);
            assert.equal((await within100ms(preparing)).prepared, false);
            assert.equal(r.release(proof(tail + 1)), false);
          }
          await within100ms(b.close(AbortSignal.abort()));
          await within100ms(flushing);
          assert.equal(ownerSettled, false);
          assert.equal(sent.length, posts);
          assert.equal(b.bufferedCount(), count);
          assert.equal(b.currentSeq(), tail);
          assert.equal(b.isTripped(), false, "cancellation is not a persistence failure");
          assert.deepEqual(o.pendingDeliveryIdentities(runId), identities);
          later = o.drainRun(runId, async (msgs) => {
            laterSent = true;
            assert.ok(msgs.every((m) => m.seq <= (stage === "clear" ? 2 : 1)),
              "cancelled range/segment must not reach the later drainer");
          });
          await new Promise<void>((resolve) => { setImmediate(resolve); });
          assert.equal(laterSent, false);
        } finally {
          release();
          if (owner) assert.equal((await owner).retired, false);
          await flushing;
          if (preparing) await preparing;
          await b.close(AbortSignal.abort());
          if (later) await later;
        }
        assert.deepEqual(o.pendingDeliveryIdentities(runId), [],
          "cancelled callback must not append after the independent owner releases");
      });
    });
  }
}

for (const stage of ["marker", "range", "segment", "clear", "transition"] as const) {
  it(`active ${stage} spill write awaits real settlement after close cancellation`, async () => {
    let armed = false;
    let entered!: () => void;
    const started = new Promise<void>((resolve) => { entered = resolve; });
    let resume!: () => void;
    const resumed = new Promise<void>((resolve) => { resume = resolve; });
    let target: "manifest" | "range" | "segment" = "manifest";
    await withOutbox(async (o) => {
      const b = new MessageBatcher({ async postUsage() {},
        async postMessages() { throw new Error("offline"); },
      } as unknown as WorkerClient, runId, 0, 60_000, nullLogger(), undefined, undefined,
      { generation: 3, outbox: o, transientTripMs: 0, spillBufferBytes: 256 });
      b.emit(event("seed"));
      await b.flush();
      await b.flush();
      if (stage === "transition") b.rearm();
      b.emit(event("assigned"));
      if (stage === "range") b.emit(event("pressure".repeat(100)));
      if (stage === "marker" || stage === "transition") {
        const original = o.markSpillUnclean.bind(o);
        o.markSpillUnclean = (...args) => { armed = true; return original(...args); };
      } else if (stage === "range") {
        target = "range";
        const original = o.appendRangeRecord.bind(o);
        o.appendRangeRecord = (...args) => { armed = true; return original(...args); };
      } else if (stage === "segment") {
        target = "segment";
        const original = o.appendSegment.bind(o);
        o.appendSegment = (...args) => { armed = true; return original(...args); };
      } else {
        const original = o.clearSpillUnclean.bind(o);
        o.clearSpillUnclean = (...args) => { armed = true; return original(...args); };
      }
      const flushing = b.flush();
      await started;
      let closed = false;
      const closing = b.close(AbortSignal.abort()).then(() => { closed = true; });
      try {
        await new Promise<void>((resolve) => { setImmediate(resolve); });
        assert.equal(closed, false, "active write must settle before close returns");
      } finally {
        resume();
        await flushing;
        await closing;
      }
      assert.equal(b.isTripped(), false);
      await b.close(); // normal cleanup persists the retained assigned tail, if any
      const delivered: OutgoingMessage[] = [];
      const identities = o.pendingDeliveryIdentities(runId);
      const receipts: symbol[] = [];
      assert.equal((await o.drainRun(runId, async (msgs) => { delivered.push(...msgs); },
        (identity) => { receipts.push(identity); })).retired, true);
      assert.equal(receipts.length, identities.length);
      assert.deepEqual(new Set(receipts), new Set(identities));
      assert.deepEqual(delivered.slice(0, 2).map((m) => [m.seq, m.payload]),
        [[1, { text: "seed" }], [2, { text: "assigned" }]]);
      if (stage === "range") {
        assert.equal(delivered[2]?.seq, 3);
        assert.equal(delivered[2]?.payload["event"], "message_dropped");
      } else assert.equal(delivered.length, 2);
    }, async (write, ctx) => {
      if (armed && ctx.kind === target) {
        armed = false;
        entered();
        await resumed;
      }
      await write();
    });
  });
}

it("normal empty-tail close retries a failed periodic spill-marker clear", async () => {
  let failManifest = false;
  await withOutbox(async (o, root) => {
    const b = new MessageBatcher({ async postUsage() {},
      async postMessages() { throw new Error("offline"); },
    } as unknown as WorkerClient, runId, 0, 60_000, nullLogger(), undefined, undefined,
    { generation: 3, outbox: o, transientTripMs: 0 });
    b.emit(event("persisted tail"));
    await b.flush();
    const clear = o.clearSpillUnclean.bind(o);
    let failOnce = true;
    o.clearSpillUnclean = async (...args) => {
      failManifest = failOnce;
      failOnce = false;
      try { await clear(...args); } finally { failManifest = false; }
    };
    const restart = async () => {
      const fresh = new Outbox({ root, log: nullLogger(), runMaxBytes: 64 * 1024 * 1024,
        maxBytes: 512 * 1024 * 1024, retentionMs: 86400_000 });
      await fresh.init();
      return fresh;
    };
    try {
      await b.flush();
      assert.equal(b.bufferedCount(), 0);
      assert.equal((await restart()).uncleanRuns().includes(runId), true);
      await b.close();
      const fresh = await restart();
      assert.equal(fresh.uncleanRuns().includes(runId), false);
      const delivered: OutgoingMessage[] = [];
      await fresh.drainRun(runId, async (msgs) => { delivered.push(...msgs); });
      assert.deepEqual(delivered.map((m) => [m.seq, m.payload]), [[1, { text: "persisted tail" }]]);
    } finally { await b.close(AbortSignal.abort()); }
  }, async (write, ctx) => {
    if (failManifest && ctx.kind === "manifest") throw new Error("transient storage failure");
    await write();
  });
});

it("cancelled empty-tail close leaves the marker and skips its queued cleanup callback", async () => {
  let failManifest = false;
  await withOutbox(async (o, root) => {
    const b = new MessageBatcher({ async postUsage() {},
      async postMessages() { throw new Error("offline"); },
    } as unknown as WorkerClient, runId, 0, 60_000, nullLogger(), undefined, undefined,
    { generation: 3, outbox: o, transientTripMs: 0 });
    b.emit(event("durable"));
    await b.flush();
    const clear = o.clearSpillUnclean.bind(o);
    o.clearSpillUnclean = async (...args) => {
      failManifest = true;
      try { await clear(...args); } finally { failManifest = false; }
    };
    await b.flush();
    o.clearSpillUnclean = clear;
    assert.equal(b.bufferedCount(), 0);
    let entered!: () => void;
    const started = new Promise<void>((resolve) => { entered = resolve; });
    let release!: () => void;
    const held = new Promise<void>((resolve) => { release = resolve; });
    const owner = o.drainRun(runId, async () => {
      entered();
      await held;
      throw new Error("independent ACK unknown");
    });
    await started;
    try { await within100ms(b.close(AbortSignal.timeout(10))); }
    finally { release(); await owner; }
    const fresh = new Outbox({ root, log: nullLogger(), runMaxBytes: 64 * 1024 * 1024,
      maxBytes: 512 * 1024 * 1024, retentionMs: 86400_000 });
    await fresh.init();
    assert.equal(fresh.uncleanRuns().includes(runId), true);
    await b.close();
    const clean = new Outbox({ root, log: nullLogger(), runMaxBytes: 64 * 1024 * 1024,
      maxBytes: 512 * 1024 * 1024, retentionMs: 86400_000 });
    await clean.init();
    assert.equal(clean.uncleanRuns().includes(runId), false);
  }, async (write, ctx) => {
    if (failManifest && ctx.kind === "manifest") throw new Error("transient storage failure");
    await write();
  });
});

it("invalid preparation deadlines fail before changing ownership", async () => {
  const { b } = fixture();
  const r = b.reserveCandidateTransport();
  for (const value of [NaN, Infinity, -1, 1.5]) assert.throws(() => r.prepare(value), RangeError);
  assert.equal((await r.prepare()).prepared, true);
  assert.equal(r.release({ ...proof(0), result: "parked" } as PlanCrossCheckResponse), true);
  await b.close();
});

it("a provided signal settles an already-owned ordinary request and retains its exact body", async () => {
  let started!: () => void;
  const start = new Promise<void>((resolve) => { started = resolve; });
  let settled = false;
  let fail = true;
  const { b, sent } = fixture(async (_id, _msgs, _gen, signal) => {
    if (!fail) return;
    started();
    await new Promise<void>((_, reject) => {
      const abort = () => { settled = true; reject(new Error("cancelled")); };
      if (signal?.aborted) abort();
      else signal?.addEventListener("abort", abort, { once: true });
    });
  });
  b.emit(event("owned"));
  const flush = b.flush();
  await start;
  b.emit(event("assigned behind owned"));
  const r = b.reserveCandidateTransport();
  const controller = new AbortController();
  const preparing = r.prepare(3000, controller.signal);
  b.emit(event("held"));
  controller.abort();
  assert.equal((await preparing).prepared, false);
  await flush;
  assert.equal(settled, true);
  assert.equal(r.release(proof()), false);
  fail = false;
  assert.equal((await r.prepare()).prepared, true);
  assert.deepEqual(sent[0], sent[1]);
  r.cancel();
  await b.close();
});

it("failure of an ordinary request after reservation ends preparation without poison replacement", async () => {
  let refuse!: () => void;
  let started!: () => void;
  const start = new Promise<void>((resolve) => { started = resolve; });
  let fail = true;
  const { b, sent } = fixture(async () => {
    if (!fail) return;
    started();
    if (sent.length > 1) throw new Error("ACK lost");
    await new Promise<void>((_, reject) => { refuse = () => reject(new Error("ACK lost")); });
  });
  b.emit(event("original one"));
  b.emit(event("original two"));
  const flushing = b.flush();
  await start;
  const r = b.reserveCandidateTransport();
  const preparing = r.prepare();
  refuse();
  assert.equal((await preparing).prepared, false);
  await flushing;
  assert.equal(sent.length, 1);
  fail = false;
  assert.equal((await r.prepare()).prepared, true);
  assert.deepEqual(sent[0], sent[1]);
  r.cancel();
  await b.close();
});

it("previous stale retirement is not ACK proof even with an empty durable queue", async () => {
  await withOutbox(async (o) => {
    await o.appendSegment(runId, 3, [{ seq: 1, kind: "status", payload: {} }]);
    const retired = await o.drainRun(runId, async () => { throw new StaleClaimError(); });
    assert.equal(retired.staleRetired, 1);
    const { b } = fixture(undefined, o);
    const r = b.reserveCandidateTransport();
    assert.equal((await r.prepare()).prepared, false);
    assert.equal(r.release(proof()), false);
    await b.close();
  });
});

it("a real disabled outbox cannot certify an empty queue", async () => {
  const root = await fs.mkdtemp(path.join(await fs.realpath(os.tmpdir()), "reservation-disabled-"));
  try {
    const blocked = path.join(root, "file");
    await fs.writeFile(blocked, "not a directory");
    const o = new Outbox({ root: path.join(blocked, "outbox"), log: nullLogger(),
      runMaxBytes: 1024, maxBytes: 4096, retentionMs: 86400_000 });
    await o.init();
    assert.equal(o.isDisabled(), true);
    const { b } = fixture(undefined, o);
    const r = b.reserveCandidateTransport();
    assert.equal((await r.prepare()).prepared, false);
    assert.equal(r.release(proof()), false);
    await b.close();
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});

it("usage recorded during assigned-message ACK waits is confirmed before preparation completes", async () => {
  let usagePosts = 0;
  const b = new MessageBatcher({
    async postUsage() { usagePosts++; },
    async postMessages() {
      b.usage.startLeg().observeAssistant({ type: "assistant", message: {
        id: "during-message", model: "claude-sonnet-5-5", usage: { input_tokens: 4 }, content: [],
      } });
    },
  } as unknown as WorkerClient, runId, 0, 60_000, nullLogger(), undefined, undefined, { generation: 3 });
  b.emit(event());
  const r = b.reserveCandidateTransport();
  assert.equal((await r.prepare()).prepared, true);
  assert.equal(usagePosts, 1);
  r.cancel();
  await b.close();
});

it("reserving during a real spilled append waits durability, drains it and resists external rearm", async () => {
  let block = false;
  let installed!: () => void;
  const installing = new Promise<void>((resolve) => { installed = resolve; });
  let resume!: () => void;
  const resumed = new Promise<void>((resolve) => { resume = resolve; });
  await withOutbox(async (o) => {
    let failing = true;
    const delivered: OutgoingMessage[] = [];
    const b = new MessageBatcher({ async postUsage() {},
      async postMessages(_id: string, msgs: OutgoingMessage[], gen: number) {
        assert.equal(gen, 3);
        if (failing) throw new Error("offline");
        delivered.push(...structuredClone(msgs));
        b.rearm();
      },
    } as unknown as WorkerClient, runId, 0, 60_000, nullLogger(), undefined, undefined,
    { generation: 3, outbox: o, transientTripMs: 0 });
    b.emit(event("first durable"));
    await b.flush();
    assert.equal(b.isSpilled(), true);
    await b.flush();
    assert.equal(o.depthFor(runId)?.pendingMessages, 1);
    block = true;
    b.emit(event("append in flight"));
    const spilling = b.flush();
    await installing;
    const r = b.reserveCandidateTransport();
    b.emit(event("held after reserve"));
    b.rearm();
    assert.equal(b.isSpilled(), true);
    const preparing = r.prepare();
    failing = false;
    resume();
    await spilling;
    assert.equal((await preparing).prepared, true);
    assert.equal(o.hasUndrainedMessages(runId), false);
    assert.deepEqual(delivered.map((m) => [m.seq, m.payload]),
      [[1, { text: "first durable" }], [2, { text: "append in flight" }]]);
    assert.equal(b.currentSeq(), 2);
    assert.equal(r.release(proof(3)), true);
    await b.flush();
    assert.equal(delivered[2]?.seq, 4);
    assert.deepEqual(delivered[2]?.payload, { text: "held after reserve" });
    await b.close();
  }, async (write, ctx) => {
    if (block && ctx.kind === "segment") {
      block = false;
      installed();
      await resumed;
    }
    await write();
  });
});

it("close preserves assigned uncertain messages durably without numbering held events", async () => {
  await withOutbox(async (o) => {
    const { b } = fixture(async () => { throw new Error("unknown ACK"); }, o);
    b.emit(event("assigned before reservation"));
    const r = b.reserveCandidateTransport();
    b.emit(event("unassigned after reservation"));
    assert.equal((await r.prepare()).prepared, false);
    await b.close();
    assert.equal(b.currentSeq(), 1);
    assert.equal(o.depthFor(runId)?.pendingMessages, 1);
    const delivered: OutgoingMessage[] = [];
    const drained = await o.drainRun(runId, async (msgs, generation) => {
      assert.equal(generation, 3);
      delivered.push(...msgs);
    });
    assert.equal(drained.retired, true);
    assert.deepEqual(delivered.map((m) => [m.seq, m.payload]),
      [[1, { text: "assigned before reservation" }]]);
    assert.equal(r.release(proof(2)), false);
  });
});

it("partially ACK-retired durable records leave only the failed original record to retry", async () => {
  await withOutbox(async (o) => {
    const first = { seq: 1, kind: "status" as const, payload: { text: "first" } };
    const second = { seq: 2, kind: "status" as const, payload: { text: "second" } };
    await o.appendSegment(runId, 3, [first]);
    await o.appendSegment(runId, 3, [second]);
    let fail = true;
    const sent: OutgoingMessage[][] = [];
    const b = new MessageBatcher({ async postUsage() {},
      async postMessages(_id: string, msgs: OutgoingMessage[], gen: number) {
        assert.equal(gen, 3);
        sent.push(structuredClone(msgs));
        if (fail && msgs[0]?.seq === 2) throw new Error("second ACK lost");
      },
    } as unknown as WorkerClient, runId, 2, 60_000, nullLogger(), undefined, undefined,
    { generation: 3, outbox: o });
    const r = b.reserveCandidateTransport();
    assert.equal((await r.prepare()).prepared, false);
    assert.equal(o.depthFor(runId)?.pendingMessages, 1);
    assert.equal(r.release(proof()), false);
    fail = false;
    assert.equal((await r.prepare()).prepared, true);
    assert.deepEqual(sent, [[first], [second], [second]]);
    assert.equal(o.hasUndrainedMessages(runId), false);
    r.cancel();
    await b.close();
  });
});

it("timer-started poison refusal preserves the original batch instead of bisecting after reserve", async () => {
  let started!: () => void;
  const start = new Promise<void>((resolve) => { started = resolve; });
  let refuse!: () => void;
  let failing = true;
  const { b, sent } = fixture(async () => {
    if (!failing) return;
    started();
    await new Promise<void>((_, reject) => {
      refuse = () => reject(new RequestError("POST", "/messages", 400, "refused"));
    });
  }, undefined, undefined, 0);
  b.emit(event("one"));
  b.emit(event("two"));
  await start;
  const r = b.reserveCandidateTransport();
  const preparing = r.prepare();
  refuse();
  assert.equal((await preparing).prepared, false);
  assert.equal(sent.length, 1);
  failing = false;
  assert.equal((await r.prepare()).prepared, true);
  assert.deepEqual(sent[0], sent[1]);
  assert.equal(sent[1]?.length, 2);
  r.cancel();
  await b.close();
});

it("partial range ACKs cannot mask another record's external retirement", async () => {
  await withOutbox(async (o) => {
    await o.appendSegment(runId, 3, [{ seq: 1, kind: "status", payload: { text: "original" } }]);
    await o.appendRangeRecord(runId, 3, 2, 502);
    let failLast = true;
    const owned: number[] = [];
    const b = new MessageBatcher({
      async postUsage() {},
      async postMessages(_id: string, msgs: OutgoingMessage[]) {
        if (failLast && msgs[0]?.seq === 502) throw new Error("last ACK unknown");
        owned.push(...msgs.map((m) => m.seq));
      },
    } as unknown as WorkerClient, runId, 502, 60_000, nullLogger(), undefined, undefined,
    { generation: 3, outbox: o });
    const r = b.reserveCandidateTransport();
    let replaced = false;
    const external = {
      async postMessages(_id: string, msgs: OutgoingMessage[]) {
        if (msgs[0]?.seq !== 1) throw new Error("external drain stopped");
        if (msgs[0]?.payload["text"] === "original")
          throw new RequestError("POST", "/messages", 400, "refused");
        replaced = true;
      },
    } as unknown as WorkerClient;
    await o.drainRun(runId, (msgs, gen) => replaySegment(external, runId, msgs, gen, nullLogger()));
    assert.equal(replaced, true);
    assert.equal((await r.prepare()).prepared, false);
    failLast = false;
    assert.equal((await r.prepare()).prepared, false,
      "must not certify externally retired seq 1 without its ACK evidence");
    assert.equal(owned.includes(1), false);
    assert.equal(r.release(proof(503)), false);
    await b.close();
  });
});

it("complete gapped and overlapping records have distinct ACK obligations", async () => {
  for (const gapped of [true, false]) await withOutbox(async (o) => {
    if (gapped) {
      await o.appendSegment(runId, 3, [
        { seq: 1, kind: "status", payload: { text: "one" } },
        { seq: 3, kind: "status", payload: { text: "three" } },
      ]);
      await o.appendRangeRecord(runId, 3, 2, 2);
    } else {
      await o.appendRangeRecord(runId, 3, 1, 3);
      await o.appendRangeRecord(runId, 3, 2, 4);
    }
    const b = new MessageBatcher({ async postUsage() {}, async postMessages() {} } as unknown as WorkerClient,
      runId, gapped ? 3 : 4, 60_000, nullLogger(), undefined, undefined, { generation: 3, outbox: o });
    const r = b.reserveCandidateTransport();
    assert.equal((await r.prepare()).prepared, true, "each complete record must count once");
    assert.equal(o.hasUndrainedMessages(runId), false);
    r.cancel();
    await b.close();
  });
});

it("record identities survive quota replacement and do not reuse retired names", async () => {
  await withOutbox(async (o) => {
    let appended: symbol | undefined;
    await o.appendSegment(runId, 3, [
      { seq: 1, kind: "status", payload: { text: "x".repeat(500) } },
    ], (identity) => { appended = identity; });
    assert.equal(o.pendingDeliveryIdentities(runId)[0], appended);
    await o.appendSegment(runId, 3, [
      { seq: 2, kind: "status", payload: { text: "y".repeat(500) } },
    ]);
    const afterQuota = o.pendingDeliveryIdentities(runId);
    assert.equal(afterQuota.length, 2);
    assert.equal(afterQuota[0], appended, "quota replacement must retain its logical obligation");
    const received: symbol[] = [];
    const delivered: OutgoingMessage[] = [];
    assert.equal((await o.drainRun(runId, async (msgs) => {
      delivered.push(...msgs);
    }, (identity) => { received.push(identity); })).retired, true);
    assert.deepEqual(received, afterQuota);
    assert.equal(delivered[0]?.payload["event"], "message_dropped",
      "the real quota replacement must have executed");
    assert.deepEqual(o.pendingDeliveryIdentities(runId), []);
    await o.appendSegment(runId, 3, [{ seq: 1, kind: "status", payload: {} }]);
    assert.notEqual(o.pendingDeliveryIdentities(runId)[0], appended,
      "a later append needs a fresh identity even when its filename/version can be reused");
  }, undefined, 1024);
});

it("an externally retired late spill cannot disappear before append commit observation", async () => {
  let block = false;
  let started!: () => void;
  const installing = new Promise<void>((resolve) => { started = resolve; });
  let resume!: () => void;
  const resumed = new Promise<void>((resolve) => { resume = resolve; });
  await withOutbox(async (o) => {
    const b = new MessageBatcher({
      async postUsage() {}, async postMessages() { throw new Error("offline"); },
    } as unknown as WorkerClient, runId, 0, 60_000, nullLogger(), undefined, undefined,
    { generation: 3, outbox: o, transientTripMs: 0 });
    b.emit(event("earlier spill"));
    await b.flush();
    await b.flush();
    await o.drainRun(runId, async () => {});
    assert.equal(o.pendingDeliveryIdentities(runId).length, 0);
    block = true;
    b.emit(event("late committed spill"));
    const flushing = b.flush();
    await installing;
    const r = b.reserveCandidateTransport();
    const external = o.drainRun(runId, async () => {});
    const preparing = r.prepare();
    resume();
    await flushing;
    assert.equal((await external).retired, true);
    assert.equal((await preparing).prepared, false,
      "late append retirement needs the reservation's own receipt");
    assert.equal(r.release(proof(3)), false);
    await b.close();
  }, async (write, ctx) => {
    if (block && ctx.kind === "segment") {
      block = false;
      started();
      await resumed;
    }
    await write();
  });
});

it("healthy senders cannot certify wrong-generation or beyond-tail durable records", async () => {
  for (const [generation, seq] of [[2, 1], [3, 2]]) await withOutbox(async (o) => {
    await o.appendSegment(runId, generation!, [{ seq: seq!, kind: "status", payload: {} }]);
    let sends = 0;
    const b = new MessageBatcher({
      async postUsage() {}, async postMessages() { sends++; },
    } as unknown as WorkerClient, runId, 1, 60_000, nullLogger(), undefined, undefined,
    { generation: 3, outbox: o });
    const r = b.reserveCandidateTransport();
    assert.equal((await r.prepare()).prepared, false, "unproven durable replay cannot prepare");
    assert.equal(sends, 0);
    assert.equal(o.hasUndrainedMessages(runId), true);
    await b.close();
  });
});

it("an already-owned bisection probe retries that exact prefix before the remaining assigned frames", async () => {
  let started!: () => void;
  const start = new Promise<void>((resolve) => { started = resolve; });
  let refuse!: () => void;
  let failing = true;
  const { b, sent } = fixture(async () => {
    if (!failing) return;
    if (sent.length === 1) throw new RequestError("POST", "/messages", 400, "refused");
    started();
    await new Promise<void>((_, reject) => {
      refuse = () => reject(new RequestError("POST", "/messages", 400, "probe refused"));
    });
  });
  for (let i = 0; i < 4; i++) b.emit(event(`original-${i}`));
  const flushing = b.flush();
  await start;
  const r = b.reserveCandidateTransport();
  const preparing = r.prepare();
  refuse();
  assert.equal((await preparing).prepared, false);
  await flushing;
  failing = false;
  assert.equal((await r.prepare()).prepared, true);
  assert.deepEqual(sent[1], sent[2]);
  assert.deepEqual(sent[3]?.map((m) => [m.seq, m.payload]),
    [[3, { text: "original-2" }], [4, { text: "original-3" }]]);
  r.cancel();
  await b.close();
});

it("settled parked proof releases transport without a checker verdict", async () => {
  const { b, sent } = fixture();
  const r = b.reserveCandidateTransport();
  b.emit(event("held"));
  assert.equal((await r.prepare()).prepared, true);
  const parked = { result: "parked", verdict: "", reason_class: "", lead_last_seq: 4,
    reconciliation: proof().reconciliation! } as Extract<PlanCrossCheckResponse, { result: "parked" }>;
  assert.equal(r.release({ ...parked, verdict: "pending" }), false);
  assert.equal(r.release(parked), true);
  await b.close();
  assert.deepEqual(sent.flat().map((m) => [m.seq, m.payload]), [[5, { text: "held" }]]);
});

it("no-row release is allowed only before any submit attempt", async () => {
  for (const submitted of [false, true]) {
    const { b, sent } = fixture();
    const r = b.reserveCandidateTransport();
    b.emit(event("held"));
    assert.equal((await r.prepare()).prepared, true);
    if (submitted) r.markSubmitted();
    const noRow: PlanCrossCheckResponse = { result: "no_row", reason_class: "no_candidate",
      lead_last_seq: 4, reconciliation: proof().reconciliation! };
    assert.equal(r.release(noRow), !submitted);
    if (submitted) {
      assert.equal((await r.prepare()).prepared, true);
      assert.equal(r.release(noRow), false, "preparing again never clears submitted identity");
      r.cancel();
    }
    await b.close();
    assert.equal(sent.length, submitted ? 0 : 1);
  }
});

it("forced applied gate ACK requires current settled proof and completed receipts", async () => {
  const { b, sent } = fixture();
  b.emit(event("assigned"));
  const r = b.reserveCandidateTransport();
  b.emit(event("held"));
  const ack = { applied: true, status: "awaiting_approval" as const,
    reconciliation: { ...proof().reconciliation!, gateRevision: 2 } };
  assert.equal(r.releaseAppliedGate(ack), false);
  assert.equal((await r.prepare()).prepared, true);
  for (const bad of [
    { ...ack, applied: false }, { ...ack, status: "running" as const },
    { ...ack, staleClaim: true }, { ...ack, reconciliation: undefined },
    { ...ack, reconciliation: { ...ack.reconciliation, claimGeneration: 2 } },
    { ...ack, reconciliation: { ...ack.reconciliation, planCrossCheckSettled: false } },
    ...[-1, NaN, Infinity, 1.5, 0x80000000].map((leadLastSeq) =>
      ({ ...ack, reconciliation: { ...ack.reconciliation, leadLastSeq } })),
  ]) assert.equal(r.releaseAppliedGate(bad), false);
  assert.equal(r.releaseAppliedGate(ack), true);
  assert.equal(r.releaseAppliedGate(ack), false);
  await b.close();
  assert.deepEqual(sent.flat().map((m) => m.seq), [1, 5]);
});

it("irreversible usage loss is detected at preparation without retrying its missing receipt", async () => {
  let refused = true;
  let usagePosts = 0;
  let messagePosts = 0;
  const b = new MessageBatcher({
    async postUsage() {
      usagePosts++;
      if (refused) throw new RequestError("POST", "/usage", 400, "refused");
    },
    async postMessages() { messagePosts++; },
  } as unknown as WorkerClient, runId, 0, 60_000, nullLogger(), undefined, undefined, { generation: 3 });
  const leg = b.usage.startLeg();
  leg.observeAssistant({ type: "assistant", message: {
    id: "irreversible", model: "claude-sonnet-5-5", usage: { input_tokens: 1 }, content: [],
  } });
  leg.close();
  await b.usage.drain();
  const refusedPosts = usagePosts;
  assert.ok(refusedPosts > 0);
  refused = false;
  b.emit(event("assigned"));
  const r = b.reserveCandidateTransport();
  b.emit(event("held"));
  const result = await r.prepare();
  assert.equal(result.prepared, false);
  assert.equal(result.permanent, true, "a lost receipt cannot be recovered by another preparation");
  assert.equal(result.reason, "usage_unconfirmed");
  assert.equal((await r.prepare()).permanent, true);
  assert.equal(usagePosts, refusedPosts);
  assert.equal(messagePosts, 0);
  assert.equal(r.release(proof()), false);
  assert.equal(b.currentSeq(), 1);
  assert.equal(b.bufferedCount(), 1);
  await b.close();
});

it("already-failed reservations refuse with a typed bounded reason without advancing sequence", async () => {
  for (const reason of ["cancelled", "overflow"] as const) {
    const { b, sent } = fixture(undefined, undefined, 180);
    b.emit(event("assigned"));
    const r = b.reserveCandidateTransport();
    if (reason === "cancelled") r.cancel();
    else assert.throws(() => b.emit(event("x".repeat(200))), (error: unknown) => {
      assert.ok(error instanceof Error);
      assert.ok(!(error instanceof CandidateReservationRefusedError), "first overflow must propagate as an ordinary error");
      return true;
    });
    assert.throws(() => b.emit(event("diagnostic")), (error: unknown) => {
      assert.ok(error instanceof CandidateReservationRefusedError);
      assert.equal(error.reason, reason);
      return true;
    });
    assert.equal(b.currentSeq(), 1);
    assert.equal(b.bufferedCount(), 1);
    assert.equal((await r.prepare()).reason, reason);
    await b.close();
    assert.equal(sent.length, 0);
  }
});

it("overflow and cancellation are permanent and cannot resurrect through rearm or preparation", async () => {
  for (const stop of ["overflow", "cancel", "close"] as const) {
    const { b, sent } = fixture(undefined, undefined, 180);
    b.emit(event("assigned"));
    const r = b.reserveCandidateTransport();
    b.emit(event("held"));
    if (stop === "overflow") assert.throws(() => b.emit(event("x".repeat(200))));
    if (stop === "cancel") r.cancel();
    if (stop === "close") await b.close();
    const outcome = await r.prepare();
    assert.equal(outcome.prepared, false);
    assert.equal(outcome.permanent, true);
    b.rearm();
    await b.flush();
    assert.equal(r.release(proof()), false);
    assert.equal(sent.length, 0);
    assert.equal(b.currentSeq(), 1);
    assert.equal(b.bufferedCount(), 1);
    await b.close();
  }
});

it("an old valid applied gate ACK releases only never-submitted transport after delayed receipts", async () => {
  for (const submitted of [false, true]) {
    let offline = true;
    const { b, sent } = fixture(async () => {
      if (offline) throw new Error("offline before forced gate");
    });
    b.emit(event("original first"));
    b.emit(event("original second"));
    const r = b.reserveCandidateTransport();
    b.emit(event("held completion"));
    try {
      assert.equal((await r.prepare()).prepared, false);
      const ack = { applied: true, status: "awaiting_approval" as const,
        reconciliation: { ...proof(1).reconciliation!, gateRevision: 2 } };
      if (submitted) r.markSubmitted();
      assert.equal(r.releaseAppliedGate(ack), false, "delayed receipts must first complete");
      offline = false;
      assert.equal((await r.prepare()).prepared, true);
      assert.deepEqual(sent[1], sent[0], "the HTTP retry preserves assigned seq and payload");
      assert.equal(r.releaseAppliedGate(ack), !submitted);
      assert.equal(b.currentSeq(), submitted ? 2 : 3, "submitted transport retains the strict ACK cursor guard");
      if (!submitted) {
        await b.flush();
        assert.deepEqual(sent.at(-1)?.map((m) => [m.seq, m.payload]), [[3, { text: "held completion" }]]);
      }
    } finally {
      r.cancel();
      await b.close();
    }
  }
});
