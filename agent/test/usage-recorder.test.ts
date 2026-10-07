import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import http from "node:http";
import type { AddressInfo } from "node:net";
import { RequestError, WorkerClient } from "../src/client.js";
import { MessageBatcher } from "../src/batcher.js";
import {
  USAGE_DRAIN_DEADLINE_MS,
  USAGE_DROPPED_IDS_MAX,
  USAGE_PENDING_MAX,
  USAGE_POST_MAX_RECORDS,
  USAGE_ROUTE_MISSING_LIMIT,
  UsageRecorder,
  resetUsageRouteStateForTests,
  type UsageClient,
  type UsageWireRequest,
} from "../src/usage-recorder.js";
import { nullLogger } from "./helpers.js";

// Issue #2014 (ADR-2014 D1/D3): the per-leg usage recorder and its bounded, abortable sender.

interface Call {
  body: UsageWireRequest;
  generation: number | undefined;
  signal: AbortSignal | undefined;
}

class FakeUsageClient implements UsageClient {
  readonly calls: Call[] = [];
  /** Per-call behaviour; the default accepts. */
  respond: (call: Call) => Promise<void> = async () => {};
  async postUsage(_runId: string, body: UsageWireRequest, generation?: number, signal?: AbortSignal): Promise<void> {
    const call = { body: structuredClone(body), generation, signal };
    this.calls.push(call);
    await this.respond(call);
  }
  get records() {
    return this.calls.flatMap((c) => c.body.messages);
  }
  get legs() {
    return this.calls.flatMap((c) => c.body.legs);
  }
}

/** A request that never settles on its own, as a hung api would: it settles only when aborted. */
function hang(call: Call): Promise<void> {
  return new Promise((_resolve, reject) => {
    call.signal?.addEventListener("abort", () => reject(new Error("aborted")), { once: true });
  });
}

function makeRecorder(client: FakeUsageClient, opts: { debounceMs?: number } = {}): UsageRecorder {
  return new UsageRecorder({
    client,
    runId: "run-1",
    claimGeneration: 3,
    log: nullLogger(),
    debounceMs: opts.debounceMs ?? 60_000,
    backoffBaseMs: 1,
  });
}

const sleep = (ms: number): Promise<void> => new Promise((r) => setTimeout(r, ms));

function assistant(id: string, usage: Record<string, unknown>, extra: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    type: "assistant",
    session_id: "sess-1",
    message: { id, model: "claude-sonnet-5-5", usage, content: [{ type: "text", text: "hi" }] },
    ...extra,
  };
}

function streamEvent(event: Record<string, unknown>, parent: string | null = null): Record<string, unknown> {
  return { type: "stream_event", session_id: "sess-1", parent_tool_use_id: parent, event };
}

function messageStart(id: string, usage: Record<string, unknown> = {}, parent: string | null = null): Record<string, unknown> {
  return streamEvent({ type: "message_start", message: { id, model: "claude-sonnet-5-5", usage } }, parent);
}

beforeEach(() => resetUsageRouteStateForTests());
afterEach(() => resetUsageRouteStateForTests());

describe("ACK-only usage drain", () => {
  it("waits existing ownership and preserves identical replay after a transient refusal", async () => {
    const client = new FakeUsageClient();
    const recorder = makeRecorder(client);
    const leg = recorder.startLeg();
    leg.observeAssistant(assistant("confirmed", { input_tokens: 7 }));
    client.respond = async () => { throw new Error("unknown ACK"); };
    assert.equal(await recorder.drainConfirmed(), false);
    const first = client.calls[0]!.body;
    leg.observeAssistant(assistant("confirmed", { input_tokens: 9 }));
    client.respond = async () => {};
    assert.equal(await recorder.drainConfirmed(), true);
    assert.deepEqual(client.calls[1]!.body, first);
    assert.equal(client.calls[2]!.body.messages[0]!.input_tokens, 9);
    recorder.release();
  });

  it("aborts and settles ownership on deadline without abandoning pending usage", async () => {
    const client = new FakeUsageClient();
    const recorder = makeRecorder(client);
    recorder.startLeg().observeAssistant(assistant("deadline", { input_tokens: 3 }));
    let aborted = false;
    client.respond = async (call) => {
      await hang(call).catch((err: unknown) => { aborted = true; throw err; });
    };
    assert.equal(await recorder.drainConfirmed(10), false);
    assert.equal(aborted, true);
    client.respond = async () => {};
    assert.equal(await recorder.drainConfirmed(), true);
    assert.deepEqual(client.calls[0]!.body, client.calls[1]!.body);
    recorder.release();
  });

  it("does not certify dropped/refused usage, even after the leg is pruned", async () => {
    const client = new FakeUsageClient();
    const recorder = makeRecorder(client);
    const leg = recorder.startLeg();
    leg.observeAssistant(assistant("refused", { input_tokens: 1 }));
    leg.close();
    client.respond = async () => { throw new RequestError("POST", "/usage", 400, "refused"); };
    await recorder.drain();
    client.respond = async () => {};
    await recorder.drain();
    assert.equal(await recorder.drainConfirmed(), false);
    recorder.release();
  });

  it("does not certify stale or globally disabled usage", async () => {
    const client = new FakeUsageClient();
    const recorder = makeRecorder(client);
    recorder.startLeg().observeAssistant(assistant("stale", { input_tokens: 1 }));
    client.respond = async () => { throw new RequestError("POST", "/usage", 409, "stale"); };
    assert.equal(await recorder.drainConfirmed(), false);
    recorder.release();
  });
});

describe("confirmed usage ownership", () => {
  it("rejects invalid deadlines before taking ownership", async () => {
    const recorder = makeRecorder(new FakeUsageClient());
    for (const value of [NaN, Infinity, -1, 1.5])
      await assert.rejects(recorder.drainConfirmed(value), RangeError);
    assert.equal(await recorder.drainConfirmed(), true);
    recorder.release();
  });

  it("keeps the second request unchanged after the first ACK and a later ambiguous ACK", async () => {
    const client = new FakeUsageClient();
    const recorder = makeRecorder(client, { debounceMs: 1 });
    const leg = recorder.startLeg();
    for (let i = 0; i <= USAGE_POST_MAX_RECORDS; i++)
      leg.observeAssistant(assistant(`batch-${i}`, { input_tokens: 1 }));

    client.respond = async () => {
      if (client.calls.length === 2) {
        leg.observeAssistant(assistant(`batch-${USAGE_POST_MAX_RECORDS}`, { input_tokens: 9 }));
        throw new Error("second ACK lost");
      }

    };
    assert.equal(await recorder.drainConfirmed(), false);
    const original = client.calls[1]!.body;
    const later = recorder.startLeg();
    later.observeAssistant(assistant("later-leg", { input_tokens: 2 }));
    later.close();
    await sleep(30);
    assert.equal(client.calls.length, 2, "new legs and updates cannot schedule a retained confirmed retry");
    assert.equal(await recorder.drainConfirmed(), true);
    assert.deepEqual(client.calls[2]!.body, original);
    assert.equal(client.calls[3]!.body.messages[0]!.input_tokens, 9);
    const acknowledgedCalls = client.calls.length;
    let debounced!: () => void;
    const debounce = new Promise<void>((resolve) => { debounced = resolve; });
    client.respond = async () => { debounced(); };
    leg.observeAssistant(assistant("after-ACK", { input_tokens: 4 }));
    await debounce;
    assert.equal(client.calls.length, acknowledgedCalls + 1, "successful ACK resumes ordinary debounce");
    recorder.release();
  });

  it("a provided signal aborts existing ownership and replays its original body after a leg update", async () => {
    const client = new FakeUsageClient();
    const recorder = makeRecorder(client, { debounceMs: 0 });
    const leg = recorder.startLeg();
    let started!: () => void;
    const start = new Promise<void>((resolve) => { started = resolve; });
    let settled = false;
    client.respond = async (call) => {
      started();
      await hang(call).catch((err: unknown) => { settled = true; throw err; });
    };
    leg.observeAssistant(assistant("owned", { input_tokens: 1 }));
    await start;
    const controller = new AbortController();
    const confirming = recorder.drainConfirmed(3000, controller.signal);
    leg.observeAssistant(assistant("owned", { input_tokens: 9 }));
    controller.abort();
    assert.equal(await confirming, false);
    assert.equal(settled, true);
    client.respond = async () => {};
    assert.equal(await recorder.drainConfirmed(), true);
    assert.deepEqual(client.calls[0]!.body, client.calls[1]!.body);
    assert.equal(client.calls[2]!.body.messages[0]!.input_tokens, 9);
    recorder.release();
  });

  it("does not certify bounded drops even after all kept records were acknowledged", async () => {
    const client = new FakeUsageClient();
    const recorder = makeRecorder(client);
    const leg = recorder.startLeg();
    for (let i = 0; i <= USAGE_PENDING_MAX; i++)
      leg.observeAssistant(assistant(`drop-${i}`, { input_tokens: 1 }));
    leg.close();
    await recorder.drain();
    assert.equal(await recorder.drainConfirmed(), false);
    recorder.release();
  });

  it("does not certify a process-wide disabled route", async () => {
    const missing = new FakeUsageClient();
    missing.respond = async () => { throw new RequestError("POST", "/usage", 404, "missing route"); };
    const recorder = makeRecorder(missing);
    recorder.startLeg().observeAssistant(assistant("missing", { input_tokens: 1 }));
    for (let i = 0; i < USAGE_ROUTE_MISSING_LIMIT; i++) await recorder.drain();
    const healthy = makeRecorder(new FakeUsageClient());
    assert.equal(await healthy.drainConfirmed(), false);
    recorder.release();
    healthy.release();
  });
});

describe("UsageLeg records", () => {
  it("keys by message.id: dense ordinals in stream order, duplicate frames merge with GREATEST", async () => {
    const client = new FakeUsageClient();
    const rec = makeRecorder(client);
    const leg = rec.startLeg();
    leg.observeAssistant(assistant("m1", { input_tokens: 10, output_tokens: 1, cache_read_input_tokens: 100 }));
    leg.observeAssistant(assistant("m2", { input_tokens: 20, output_tokens: 2 }));
    // two more frames of m1 (one block each), the later one carrying the larger output and a LOWER input
    leg.observeAssistant(assistant("m1", { input_tokens: 4, output_tokens: 9, cache_read_input_tokens: 100 }));
    leg.observeAssistant(assistant("m1", { input_tokens: 10, output_tokens: 5 }));
    leg.observeAssistant(assistant("m3", { input_tokens: 1, output_tokens: 1 }));
    assert.equal(leg.maxOrdinal, 3);
    leg.close();
    await rec.drain();

    const byId = new Map(client.records.map((r) => [r.message_id, r]));
    assert.equal(client.records.length, 3, "one record per distinct message.id");
    assert.deepEqual(
      ["m1", "m2", "m3"].map((id) => byId.get(id)!.ordinal),
      [1, 2, 3],
      "dense ordinals in first-seen order",
    );
    const m1 = byId.get("m1")!;
    assert.equal(m1.input_tokens, 10);
    assert.equal(m1.output_tokens, 9);
    assert.equal(m1.cache_read_input_tokens, 100);
    assert.equal(m1.leg_id, leg.legId);
    assert.equal(m1.frame_session_id, "sess-1");
    assert.equal(m1.subagent, false);
    assert.equal(m1.output_final, false);
    assert.deepEqual(client.legs, [{ leg_id: leg.legId, closed_through: 3 }]);
    assert.equal(client.calls[0]!.generation, 3, "claim generation handed to the client");
  });

  it("carries the cache-creation split, tier, speed and geo when present on usage, and omits them otherwise", async () => {
    const client = new FakeUsageClient();
    const rec = makeRecorder(client);
    const leg = rec.startLeg();
    leg.observeAssistant(
      assistant("m1", {
        input_tokens: 1,
        output_tokens: 2,
        cache_creation_input_tokens: 300,
        cache_creation: { ephemeral_5m_input_tokens: 200, ephemeral_1h_input_tokens: 100 },
        service_tier: "standard",
        speed: "fast",
        inference_geo: "us",
      }),
    );
    leg.observeAssistant(assistant("m2", { input_tokens: 1, output_tokens: 1 }));
    await rec.drain();
    const [m1, m2] = client.records;
    assert.equal(m1!.cache_creation_input_tokens, 300);
    assert.equal(m1!.cache_creation_5m_input_tokens, 200);
    assert.equal(m1!.cache_creation_1h_input_tokens, 100);
    assert.equal(m1!.service_tier, "standard");
    assert.equal(m1!.speed, "fast");
    assert.equal(m1!.inference_geo, "us");
    for (const key of ["cache_creation_5m_input_tokens", "cache_creation_1h_input_tokens", "service_tier", "speed", "inference_geo"]) {
      assert.equal(key in m2!, false, `${key} absent when usage does not carry it`);
    }
  });

  it("ignores a frame with no message id and a synthetic worker notice; hostile numbers floor to 0", async () => {
    const client = new FakeUsageClient();
    const rec = makeRecorder(client);
    const leg = rec.startLeg();
    leg.observeAssistant({ type: "assistant", message: { usage: { input_tokens: 5 } } });
    leg.observeAssistant({ type: "assistant", message: { id: "syn", model: "<synthetic>", usage: { input_tokens: 5 } } });
    leg.observeAssistant(assistant("m1", { input_tokens: -4, output_tokens: Number.NaN, cache_read_input_tokens: "9" }));
    assert.equal(leg.maxOrdinal, 1);
    await rec.drain();
    assert.equal(client.records.length, 1);
    assert.equal(client.records[0]!.input_tokens, 0);
    assert.equal(client.records[0]!.output_tokens, 0);
    assert.equal(client.records[0]!.cache_read_input_tokens, 0);
  });

  it("stream events: message_start creates a record no assistant frame ever arrives for; message_delta is final", async () => {
    const client = new FakeUsageClient();
    const rec = makeRecorder(client);
    const leg = rec.startLeg();
    leg.observeStream(messageStart("s1", { input_tokens: 50, cache_read_input_tokens: 7, output_tokens: 1 }));
    leg.observeStream(streamEvent({ type: "content_block_delta", index: 0, delta: { type: "text_delta", text: "x" } }));
    leg.observeStream(streamEvent({ type: "ping" }));
    leg.observeStream(streamEvent({ type: "message_delta", delta: { stop_reason: "end_turn" }, usage: { output_tokens: 42 } }));
    leg.observeStream(streamEvent({ type: "message_stop" }));
    await rec.drain();
    assert.equal(client.records.length, 1);
    const r = client.records[0]!;
    assert.equal(r.message_id, "s1");
    assert.equal(r.input_tokens, 50);
    assert.equal(r.cache_read_input_tokens, 7);
    assert.equal(r.output_tokens, 42);
    assert.equal(r.output_final, true);
  });

  it("a message_delta lands on its own lane's current message; subagent lanes are marked and kept apart", async () => {
    const client = new FakeUsageClient();
    const rec = makeRecorder(client);
    const leg = rec.startLeg();
    leg.observeStream(messageStart("main-1", { input_tokens: 1 }, null));
    leg.observeStream(messageStart("sub-1", { input_tokens: 2 }, "toolu_a"));
    leg.observeStream(streamEvent({ type: "message_delta", usage: { output_tokens: 11 } }, "toolu_a"));
    leg.observeStream(streamEvent({ type: "message_delta", usage: { output_tokens: 22 } }, null));
    // a delta for a lane that never started a message is ignored
    leg.observeStream(streamEvent({ type: "message_delta", usage: { output_tokens: 99 } }, "toolu_other"));
    await rec.drain();
    const byId = new Map(client.records.map((r) => [r.message_id, r]));
    assert.equal(byId.get("main-1")!.output_tokens, 22);
    assert.equal(byId.get("main-1")!.subagent, false);
    assert.equal(byId.get("sub-1")!.output_tokens, 11);
    assert.equal(byId.get("sub-1")!.subagent, true);
    assert.equal(client.records.length, 2);
  });

  it("an assistant frame after message_start/delta of the same id keeps the record: one ordinal, final stays set", async () => {
    const client = new FakeUsageClient();
    const rec = makeRecorder(client);
    const leg = rec.startLeg();
    leg.observeStream(messageStart("m1", { input_tokens: 10, output_tokens: 1 }));
    leg.observeStream(streamEvent({ type: "message_delta", usage: { output_tokens: 30 } }));
    leg.observeAssistant(assistant("m1", { input_tokens: 10, output_tokens: 2 }));
    leg.observeAssistant(assistant("m1", { input_tokens: 10, output_tokens: 3 }));
    await rec.drain();
    assert.equal(client.records.length, 1);
    assert.equal(client.records[0]!.ordinal, 1);
    assert.equal(client.records[0]!.output_tokens, 30);
    assert.equal(client.records[0]!.output_final, true);
  });

  it("a subagent assistant frame (parent_tool_use_id or subagent_type) is a subagent record", async () => {
    const client = new FakeUsageClient();
    const rec = makeRecorder(client);
    const leg = rec.startLeg();
    leg.observeAssistant(assistant("a", { output_tokens: 1 }, { parent_tool_use_id: "toolu_1" }));
    leg.observeAssistant(assistant("b", { output_tokens: 1 }, { subagent_type: "coder" }));
    leg.observeAssistant(assistant("c", { output_tokens: 1 }));
    await rec.drain();
    assert.deepEqual(client.records.map((r) => r.subagent), [true, true, false]);
  });
});

describe("UsageRecorder bounds", () => {
  it("past USAGE_PENDING_MAX distinct pending messages new ones are dropped and counted; the marker reports them", async () => {
    const client = new FakeUsageClient();
    const rec = makeRecorder(client);
    const leg = rec.startLeg();
    const over = 5;
    for (let i = 0; i < USAGE_PENDING_MAX + over; i++) leg.observeAssistant(assistant(`m${i}`, { output_tokens: 1 }));
    // while a dropped id is remembered (up to USAGE_DROPPED_IDS_MAX) a re-sighting does not resurrect it
    // half-way, and one of a kept id still merges
    leg.observeAssistant(assistant(`m${USAGE_PENDING_MAX + 1}`, { output_tokens: 7 }));
    leg.observeAssistant(assistant("m0", { output_tokens: 9 }));
    leg.close();
    await rec.drain();

    assert.equal(client.records.length, USAGE_PENDING_MAX, "exactly the pending cap was kept");
    assert.ok(client.calls.every((c) => c.body.messages.length <= USAGE_POST_MAX_RECORDS), "no request over the record cap");
    assert.equal(client.calls.length, USAGE_PENDING_MAX / USAGE_POST_MAX_RECORDS);
    assert.deepEqual(
      client.records.map((r) => r.ordinal),
      Array.from({ length: USAGE_PENDING_MAX }, (_, i) => i + 1),
      "ordinals stay dense over what was recorded",
    );
    assert.equal(client.records[0]!.output_tokens, 9);
    assert.deepEqual(client.legs, [{ leg_id: leg.legId, closed_through: USAGE_PENDING_MAX, dropped_records: over }]);
    assert.deepEqual(client.calls.at(-1)!.body.legs.length, 1, "the marker rides the request that empties the leg");
    assert.deepEqual(client.calls.slice(0, -1).flatMap((c) => c.body.legs), [], "a close never overtakes the records it closes");
  });

  it("dropped ids are remembered only up to USAGE_DROPPED_IDS_MAX; later rejections are counted, not remembered", async () => {
    const client = new FakeUsageClient();
    const rec = makeRecorder(client);
    const leg = rec.startLeg();
    const extra = 50;
    for (let i = 0; i < USAGE_PENDING_MAX; i++) leg.observeAssistant(assistant(`k${i}`, { output_tokens: 1 }));
    const rejected = USAGE_DROPPED_IDS_MAX + extra;
    for (let i = 0; i < rejected; i++) leg.observeAssistant(assistant(`d${i}`, { output_tokens: 1 }));
    // an early rejected id is remembered (not counted again); a late one is not (counted again)
    leg.observeAssistant(assistant("d0", { output_tokens: 1 }));
    leg.observeAssistant(assistant(`d${rejected - 1}`, { output_tokens: 1 }));
    leg.close();
    await rec.drain();

    assert.equal(client.records.length, USAGE_PENDING_MAX);
    const dropped = client.legs.at(-1)?.dropped_records ?? 0;
    assert.ok(dropped > 0);
    assert.equal(dropped, rejected + 1, "only the untracked re-sighting was counted again");
  });

  it("the pending cap is recorder-wide: legs together never hold more than USAGE_PENDING_MAX", async () => {
    const client = new FakeUsageClient();
    const rec = makeRecorder(client);
    const legs = [rec.startLeg(), rec.startLeg(), rec.startLeg()];
    const each = 800; // 3 x 800 = 2400 > USAGE_PENDING_MAX, though no leg alone is over it
    for (const leg of legs) for (let i = 0; i < each; i++) leg.observeAssistant(assistant(`${leg.legId}-${i}`, { output_tokens: 1 }));
    for (const leg of legs) leg.close();
    await rec.drain();
    await rec.drain();

    assert.equal(client.records.length, USAGE_PENDING_MAX, "the cap held across legs");
    const dropped = client.legs.reduce((n, l) => n + (l.dropped_records ?? 0), 0);
    assert.equal(dropped, legs.length * each - USAGE_PENDING_MAX, "the overflow is counted");
  });

  it("a final output count lost to a full pending set is counted as dropped, once", async () => {
    const client = new FakeUsageClient();
    const rec = makeRecorder(client);
    const leg = rec.startLeg();
    leg.observeStream(messageStart("m1", { input_tokens: 5, output_tokens: 1 }));
    await rec.drain(); // m1 is on the wire; its record leaves the pending set
    for (let i = 0; i < USAGE_PENDING_MAX; i++) leg.observeAssistant(assistant(`f${i}`, { output_tokens: 1 }));
    // m1's final output count arrives with the pending set full: it cannot be carried
    leg.observeStream(streamEvent({ type: "message_delta", usage: { output_tokens: 50 } }));
    leg.observeStream(streamEvent({ type: "message_delta", usage: { output_tokens: 60 } }));
    leg.close();
    await rec.drain();
    await rec.drain();

    assert.ok(client.records.every((r) => r.message_id !== "m1" || r.output_tokens === 1), "m1 was not resent");
    assert.equal(client.legs.at(-1)?.dropped_records, 1, "the lost update is reported so coverage is partial");
  });

  it("posts promptly after the debounce, before the leg ends", async () => {
    const client = new FakeUsageClient();
    const rec = makeRecorder(client, { debounceMs: 20 });
    const leg = rec.startLeg();
    leg.observeAssistant(assistant("m1", { input_tokens: 3, output_tokens: 1 }));
    assert.equal(client.calls.length, 0, "not synchronous");
    await sleep(150);
    assert.equal(client.calls.length, 1, "posted by the debounce timer, no drain, no close");
    assert.equal(client.calls[0]!.body.messages[0]!.message_id, "m1");
    assert.deepEqual(client.calls[0]!.body.legs, [], "the leg is not closed yet");
    // an update to the sent record posts again
    leg.observeStream(messageStart("m1", { input_tokens: 3 }));
    leg.observeStream(streamEvent({ type: "message_delta", usage: { output_tokens: 8 } }));
    await sleep(150);
    assert.equal(client.calls.length, 2);
    assert.equal(client.calls[1]!.body.messages[0]!.output_final, true);
    leg.close();
    await rec.drain();
  });

  it("a leg that recorded nothing sends no marker", async () => {
    const client = new FakeUsageClient();
    const rec = makeRecorder(client);
    rec.startLeg().close();
    await rec.drain();
    assert.equal(client.calls.length, 0);
  });
});

describe("UsageRecorder drain deadline", () => {
  it("a hanging route: drain returns within its deadline AND the request's signal is aborted", async () => {
    const client = new FakeUsageClient();
    client.respond = hang;
    const rec = makeRecorder(client);
    const leg = rec.startLeg();
    leg.observeAssistant(assistant("m1", { output_tokens: 1 }));
    leg.close();
    const t0 = Date.now();
    await rec.drain(150);
    const took = Date.now() - t0;
    assert.ok(took < 1500, `drain returned in ${took} ms`);
    assert.equal(client.calls.length, 1);
    assert.equal(client.calls[0]!.signal?.aborted, true, "the in-flight request was ABORTED, not merely left unawaited");
    // the unsent records were abandoned: nothing is retried afterwards
    await rec.drain(50);
    assert.equal(client.calls.length, 1);
  });

  it("a caller signal shortens the drain and aborts the request", async () => {
    const client = new FakeUsageClient();
    client.respond = hang;
    const rec = makeRecorder(client);
    rec.startLeg().observeAssistant(assistant("m1", { output_tokens: 1 }));
    const t0 = Date.now();
    await rec.drain(USAGE_DRAIN_DEADLINE_MS, AbortSignal.timeout(100));
    assert.ok(Date.now() - t0 < 1500);
    assert.equal(client.calls[0]!.signal?.aborted, true);
  });

  it("the drain deadline can only be shortened: a longer request is capped at USAGE_DRAIN_DEADLINE_MS", async () => {
    const client = new FakeUsageClient();
    client.respond = hang;
    const rec = makeRecorder(client);
    rec.startLeg().observeAssistant(assistant("m1", { output_tokens: 1 }));
    const t0 = Date.now();
    await rec.drain(60_000);
    const took = Date.now() - t0;
    assert.ok(took >= USAGE_DRAIN_DEADLINE_MS - 100 && took < USAGE_DRAIN_DEADLINE_MS + 1500, `took ${took} ms`);
    assert.equal(client.calls[0]!.signal?.aborted, true);
  });

  it("an open leg keeps recording after an abandoned drain", async () => {
    const client = new FakeUsageClient();
    client.respond = hang;
    const rec = makeRecorder(client);
    const leg = rec.startLeg();
    leg.observeAssistant(assistant("m1", { output_tokens: 1 }));
    await rec.drain(50);
    client.respond = async () => {};
    leg.observeAssistant(assistant("m2", { output_tokens: 1 }));
    await rec.drain();
    const last = client.calls.at(-1)!;
    assert.deepEqual(last.body.messages.map((m) => [m.message_id, m.ordinal]), [["m2", 2]], "the gap (m1) is the server's ordinal_gap");
  });

  it("a failed attempt ends the drain at once and the records stay pending for the retry", async () => {
    const client = new FakeUsageClient();
    let n = 0;
    client.respond = async () => {
      if (++n === 1) throw new RequestError("POST", "/usage", 503, "down");
    };
    const rec = makeRecorder(client);
    rec.startLeg().observeAssistant(assistant("m1", { output_tokens: 1 }));
    const t0 = Date.now();
    await rec.drain();
    assert.ok(Date.now() - t0 < 1000);
    assert.equal(client.calls.length, 1);
    await rec.drain();
    assert.equal(client.calls.length, 2, "the next drain retried the same record");
    assert.equal(client.calls[1]!.body.messages[0]!.message_id, "m1");
  });
});

describe("MessageBatcher owns the usage drain", () => {
  function batcherWith(usageClient: FakeUsageClient): MessageBatcher {
    const client = {
      postMessages: async () => {},
      postUsage: (...args: Parameters<UsageClient["postUsage"]>) => usageClient.postUsage(...args),
    } as unknown as WorkerClient;
    return new MessageBatcher(client, "run-1", 0, 60_000, nullLogger(), undefined, undefined, { generation: 4 });
  }

  it("flush() posts the pending usage first, even with no message buffered", async () => {
    const usageClient = new FakeUsageClient();
    const batcher = batcherWith(usageClient);
    batcher.usage.startLeg().observeAssistant(assistant("m1", { output_tokens: 1 }));
    await batcher.flush();
    assert.equal(usageClient.records.length, 1);
    assert.equal(usageClient.calls[0]!.generation, 4, "stamped with the batcher's claim generation");
  });

  it("close() drains the usage too", async () => {
    const usageClient = new FakeUsageClient();
    const batcher = batcherWith(usageClient);
    const leg = batcher.usage.startLeg();
    leg.observeAssistant(assistant("m1", { output_tokens: 1 }));
    leg.close();
    await batcher.close();
    assert.equal(usageClient.records.length, 1);
    assert.deepEqual(usageClient.legs, [{ leg_id: leg.legId, closed_through: 1 }]);
  });

  it("a hanging usage route: flush() and close() return within the drain deadline and abort the request", async () => {
    for (const op of ["flush", "close"] as const) {
      const usageClient = new FakeUsageClient();
      usageClient.respond = hang;
      const batcher = batcherWith(usageClient);
      batcher.usage.startLeg().observeAssistant(assistant("m1", { output_tokens: 1 }));
      const t0 = Date.now();
      await (op === "flush" ? batcher.flush() : batcher.close());
      const took = Date.now() - t0;
      assert.ok(took < USAGE_DRAIN_DEADLINE_MS + 1500, `${op} took ${took} ms`);
      assert.equal(usageClient.calls[0]!.signal?.aborted, true, `${op} aborted the request`);
    }
  });

  it("close() stops the recorder: a persistently failing api is not retried after close", async () => {
    const usageClient = new FakeUsageClient();
    usageClient.respond = async () => {
      throw new RequestError("POST", "/usage", 503, "down");
    };
    const batcher = batcherWith(usageClient);
    batcher.usage.startLeg().observeAssistant(assistant("m1", { output_tokens: 1 }));
    await batcher.close();
    assert.equal(usageClient.calls.length, 1);
    await sleep(900); // past the recorder's first 500 ms backoff
    assert.equal(usageClient.calls.length, 1, "no retry is scheduled after close");
    assert.equal(batcher.usage.inactive, true);
  });

  it("a durability-boundary signal that fires sooner shortens the drain", async () => {
    const usageClient = new FakeUsageClient();
    usageClient.respond = hang;
    const batcher = batcherWith(usageClient);
    batcher.usage.startLeg().observeAssistant(assistant("m1", { output_tokens: 1 }));
    const t0 = Date.now();
    await batcher.close(AbortSignal.timeout(100));
    assert.ok(Date.now() - t0 < 1500);
    assert.equal(usageClient.calls[0]!.signal?.aborted, true);
  });
});

describe("UsageRecorder error handling", () => {
  const typedStale = (): RequestError =>
    new RequestError("POST", "/usage", 404, JSON.stringify({ error: "run not found for this worker", reason: "stale" }));

  it("a typed stale 404 stops THIS recorder only", async () => {
    const stale = new FakeUsageClient();
    stale.respond = async () => {
      throw typedStale();
    };
    const fine = new FakeUsageClient();
    const a = makeRecorder(stale);
    const b = makeRecorder(fine);
    a.startLeg().observeAssistant(assistant("m1", { output_tokens: 1 }));
    b.startLeg().observeAssistant(assistant("m1", { output_tokens: 1 }));
    await a.drain();
    await b.drain();
    assert.equal(a.inactive, true);
    assert.equal(b.inactive, false);
    // a stopped recorder never posts again, even for a new leg
    a.startLeg().observeAssistant(assistant("m2", { output_tokens: 1 }));
    await a.drain();
    assert.equal(stale.calls.length, 1);
    // the other run's recorder is unaffected, and a fresh recorder still posts
    const next = makeRecorder(fine);
    next.startLeg().observeAssistant(assistant("m9", { output_tokens: 1 }));
    await next.drain();
    assert.equal(fine.calls.length, 2);
  });

  it("a 409 stale_claim and a 409 missing claim generation each stop that recorder", async () => {
    for (const body of [JSON.stringify({ disposition: "stale_claim" }), JSON.stringify({ error: "this worker must stamp claim_generation on every usage report" })]) {
      const client = new FakeUsageClient();
      client.respond = async () => {
        throw new RequestError("POST", "/usage", 409, body);
      };
      const rec = makeRecorder(client);
      rec.startLeg().observeAssistant(assistant("m1", { output_tokens: 1 }));
      await rec.drain();
      assert.equal(rec.inactive, true, body);
      assert.equal(client.calls.length, 1);
    }
  });

  it("an untyped 404/405 streak disables the route for the whole process", async () => {
    const missing = new FakeUsageClient();
    missing.respond = async () => {
      throw new RequestError("POST", "/usage", 404, "404 page not found");
    };
    const rec = makeRecorder(missing);
    rec.startLeg().observeAssistant(assistant("m1", { output_tokens: 1 }));
    for (let i = 0; i < USAGE_ROUTE_MISSING_LIMIT - 1; i++) {
      await rec.drain();
      assert.equal(rec.inactive, false, `still trying after ${i + 1} untyped answers`);
    }
    await rec.drain();
    assert.equal(rec.inactive, true);
    assert.equal(missing.calls.length, USAGE_ROUTE_MISSING_LIMIT);

    // a different run's recorder, with a healthy client, is now off too (module-level flag)
    const healthy = new FakeUsageClient();
    const other = makeRecorder(healthy);
    other.startLeg().observeAssistant(assistant("m1", { output_tokens: 1 }));
    await other.drain();
    assert.equal(healthy.calls.length, 0, "route disabled for the process");
    assert.equal(other.inactive, true);
  });

  it("an accepted post resets the untyped-404 streak", async () => {
    const client = new FakeUsageClient();
    let fail = true;
    client.respond = async () => {
      if (fail) throw new RequestError("POST", "/usage", 405, "");
    };
    const rec = makeRecorder(client);
    const leg = rec.startLeg();
    leg.observeAssistant(assistant("m1", { output_tokens: 1 }));
    for (let i = 0; i < USAGE_ROUTE_MISSING_LIMIT - 1; i++) await rec.drain();
    fail = false;
    await rec.drain();
    fail = true;
    for (let i = 0; i < USAGE_ROUTE_MISSING_LIMIT - 1; i++) {
      leg.observeAssistant(assistant(`n${i}`, { output_tokens: 1 }));
      await rec.drain();
    }
    assert.equal(rec.inactive, false, "7 + success + 7 never reached the limit of 8 in a row");
  });

  it("a 400 refusal drops the request (no retry loop) and counts the lost records", async () => {
    const client = new FakeUsageClient();
    let first = true;
    client.respond = async () => {
      if (first) {
        first = false;
        throw new RequestError("POST", "/usage", 400, "invalid usage report");
      }
    };
    const rec = makeRecorder(client);
    const leg = rec.startLeg();
    leg.observeAssistant(assistant("m1", { output_tokens: 1 }));
    await rec.drain();
    leg.close();
    await rec.drain();
    assert.equal(client.calls[0]!.body.messages.length, 1, "the refused request is not re-sent");
    assert.equal(client.records.length, 1);
    assert.deepEqual(client.calls.at(-1)!.body.legs, [{ leg_id: leg.legId, closed_through: 1, dropped_records: 1 }]);
  });
});

describe("WorkerClient.postUsage", () => {
  let server: http.Server;
  let baseUrl: string;
  const seen: { url: string; body: Record<string, unknown> }[] = [];

  beforeEach(async () => {
    seen.length = 0;
    server = http.createServer((req, res) => {
      const chunks: Buffer[] = [];
      req.on("data", (c: Buffer) => chunks.push(c));
      req.on("end", () => {
        if (req.url?.endsWith("/register")) {
          res.setHeader("Content-Type", "application/json");
          res.end(JSON.stringify({ worker_id: "w1", protocol_features: [] }));
          return;
        }
        seen.push({ url: req.url ?? "", body: JSON.parse(Buffer.concat(chunks).toString("utf8")) as Record<string, unknown> });
        res.statusCode = 204;
        res.end();
      });
    });
    await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
    baseUrl = `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
  });
  afterEach(async () => {
    await new Promise<void>((resolve) => server.close(() => resolve()));
  });

  const body: UsageWireRequest = {
    legs: [{ leg_id: "6f1d2a40-7c1e-4a0e-9f0b-1a2b3c4d5e01", closed_through: 1 }],
    messages: [],
  };

  it("posts to /usage and stamps the claim generation exactly as postMessages does", async () => {
    const client = new WorkerClient(baseUrl, "tok", "0.0.0", nullLogger(), { sleep: async () => {} });
    await client.register("w", undefined, 1, undefined, ["credential_switch_v1"]);
    await client.postUsage("run-9", body, 3);
    assert.match(seen[0]!.url, /\/runs\/run-9\/usage$/);
    assert.equal(seen[0]!.body["claim_generation"], 3);
    assert.deepEqual(seen[0]!.body["legs"], body.legs);
    await client.postUsage("run-9", body, 0);
    assert.equal("claim_generation" in seen[1]!.body, false, "generation 0 (legacy) is never sent");
  });

  it("a bare worker (no capability, no feature) omits the generation", async () => {
    const client = new WorkerClient(baseUrl, "tok", "0.0.0", nullLogger(), { sleep: async () => {} });
    await client.postUsage("run-9", body, 5);
    assert.equal("claim_generation" in seen[0]!.body, false);
  });
});

describe("WorkerClient.postUsage abort", () => {
  it("aborting the signal rejects promptly AND closes the request socket on the server", async () => {
    let reqStarted!: () => void;
    const started = new Promise<void>((r) => (reqStarted = r));
    let closed!: () => void;
    const serverSawClose = new Promise<void>((r) => (closed = r));
    const server = http.createServer((req) => {
      if (!req.url?.endsWith("/usage")) return;
      req.resume();
      req.socket.on("close", () => closed());
      reqStarted(); // never answered: a hung route
    });
    await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
    try {
      const baseUrl = `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
      const client = new WorkerClient(baseUrl, "tok", "0.0.0", nullLogger(), { sleep: async () => {} });
      const abort = new AbortController();
      const posted = client.postUsage("run-9", { legs: [{ leg_id: "6f1d2a40-7c1e-4a0e-9f0b-1a2b3c4d5e01", closed_through: 1 }], messages: [] }, 3, abort.signal);
      const outcome = posted.then(() => "resolved", () => "rejected");
      await started;
      const t0 = Date.now();
      abort.abort();
      assert.equal(await outcome, "rejected");
      assert.ok(Date.now() - t0 < 1000, "rejected promptly, not after the client's own timeout");
      await Promise.race([
        serverSawClose,
        sleep(2000).then(() => assert.fail("the server never saw the request socket close")),
      ]);
    } finally {
      server.closeAllConnections();
      await new Promise<void>((resolve) => server.close(() => resolve()));
    }
  });
});
