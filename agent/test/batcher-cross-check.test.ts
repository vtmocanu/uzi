import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";
import { MessageBatcher } from "../src/batcher.js";
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
  assert.equal((await r.prepare()).prepared, false);
  assert.equal(r.release(proof()), false);
  fail = false;
  assert.equal((await r.prepare()).prepared, true);
  assert.deepEqual(sent[0], sent[1]);
  assert.equal(b.currentSeq(), 1);
  assert.equal(r.release(proof(2)), true);
  await b.close();
  assert.deepEqual(sent[2]?.map((m) => m.seq), [3]);
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

async function withOutbox(body: (o: Outbox) => Promise<void>, rawWrite?: RawWriteSeam) {
  const root = await fs.mkdtemp(path.resolve("../.uzi/scratch/reservation-outbox-"));
  try {
    const o = new Outbox({ root: path.join(root, "outbox"), log: nullLogger(),
      runMaxBytes: 64 * 1024 * 1024, maxBytes: 512 * 1024 * 1024, retentionMs: 86400_000, rawWrite });
    await o.init();
    await body(o);
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
  const root = await fs.mkdtemp(path.resolve("../.uzi/scratch/reservation-disabled-"));
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
