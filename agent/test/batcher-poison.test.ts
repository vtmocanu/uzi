import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { MessageBatcher, MAX_BATCH_BYTES, MAX_MESSAGE_BYTES, MAX_BACKOFF_MS } from "../src/batcher.js";
import { RequestError } from "../src/client.js";
import { recordingLogger } from "./helpers.js";
import { sleep } from "../src/util.js";
import type { WorkerClient } from "../src/client.js";
import type { OutgoingMessage } from "../src/protocol.js";

/**
 * PRD #108 — the 2026-07-21 batcher wedge. Written as M1b characterization tests
 * pinning the DEFECT, and FLIPPED here by M3 steps 1-3 into the invariants that
 * replace it. Each `it` keeps its subject and inverts its assertion, so the fix
 * reads as a flip in one diff rather than a deletion.
 *
 * The incident: run 4d4762cf posted a `tool_result` carrying 84 NUL bytes. The
 * store rejected it (SQLSTATE 22P05), `worker_protocol.go`'s `default:` arm
 * returned 500, the batcher read 500 as retryable, re-buffered the identical
 * batch, and looped at ~2 Hz for 27 minutes with the batch climbing 206 → 239
 * messages. Cancelling logged `dropped: 239`.
 *
 * Measured against the UNFIXED batcher, and each number is what the flipped test
 * below now refutes:
 *
 *   1. 62 posts in a 400ms window, 6.49ms apart, flat — no backoff.
 *   2. batch 1 → 13 messages, body 81 → 843 bytes, monotone — unbounded growth.
 *   3. crossed the 1 MiB cap at attempt 18 (11 messages, 1,082,141 bytes), status
 *      500 → 400, peaking at 1,475,645 bytes — the class changed under a batcher
 *      that could not tell.
 *
 * Still M3b's, and deliberately NOT asserted here yet: bisection, the tombstone,
 * 4xx-as-fatal and the breaker. Retries remain unbounded in COUNT after this
 * change — what M3 steps 1-3 remove is the hot loop and the growth, which is
 * precisely the Decision 4 ordering (cap and split BEFORE 4xx is made fatal).
 */

/** The api's real cap: `maxBodyBytes = 1 << 20` in `api/internal/httpx/respond.go`,
 *  enforced by `io.LimitReader` inside `DecodeJSON`. */
const MAX_BODY_BYTES = 1 << 20;

interface Attempt {
  count: number;
  bytes: number;
  status: number;
}

/**
 * A `postMessages` stub that models what the API ACTUALLY does on this route, so
 * the status class a test observes is DERIVED from the batch the batcher built
 * rather than stipulated by the test:
 *
 *  - over 1 MiB → `io.LimitReader` truncates the body, `DecodeJSON` fails on the
 *    truncated JSON, and `WorkerRunMessages` maps a decode error to **400
 *    "invalid request body"** (`worker_protocol.go:333-336`);
 *  - otherwise → **500 "internal error"**, the `default:` arm a store rejection
 *    falls to (`:344-346`) — the incident's own status.
 *
 * The wire size is measured the way the client sends it: `JSON.stringify` of the
 * `MessagesRequest` body (`client.ts:129-130`).
 */
function poisonedApi(): { client: WorkerClient; attempts: Attempt[] } {
  const attempts: Attempt[] = [];
  const client = {
    async postMessages(runId: string, messages: OutgoingMessage[]): Promise<void> {
      const bytes = Buffer.byteLength(JSON.stringify({ messages }), "utf8");
      const oversize = bytes > MAX_BODY_BYTES;
      const status = oversize ? 400 : 500;
      attempts.push({ count: messages.length, bytes, status });
      throw new RequestError(
        "POST",
        `/api/worker/runs/${runId}/messages`,
        status,
        oversize ? '{"error":"invalid request body"}' : '{"error":"internal error"}',
      );
    },
  } as unknown as WorkerClient;
  return { client, attempts };
}

/** ~`size` bytes of payload, so a handful of messages can cross 1 MiB without a
 *  test that takes minutes. Mirrors the incident's 25,418-byte tool_result. */
function fatMessage(size: number): { kind: "tool_result"; agent: string; payload: Record<string, unknown> } {
  return { kind: "tool_result", agent: "lead", payload: { content: "x".repeat(size) } };
}

describe("MessageBatcher — the poison-pill wedge (PRD #108 M1b/M3)", () => {
  it("M3: a permanently-failing POST backs off exponentially instead of hot-looping", async () => {
    const { logger } = recordingLogger();
    const { client, attempts } = poisonedApi();
    const batchMs = 5;
    const batcher = new MessageBatcher(client, "run-1", 0, batchMs, logger);

    // Measure the REQUESTED backoff delays, not wall-clock gaps between attempts.
    // A wall-clock gap is the scheduled delay PLUS event-loop scheduling latency,
    // and under CI CPU contention that latency floor is large and additive: it
    // swamps the small early scheduled delays and the >2x growth ratio flakes.
    // Capturing what the batcher ASKS setTimeout for isolates the schedule itself,
    // exactly as the sibling "backoff delay is capped" test below does.
    const requested: number[] = [];
    const realSetTimeout = globalThis.setTimeout;
    (globalThis as { setTimeout: typeof setTimeout }).setTimeout = ((fn: () => void, ms?: number) => {
      requested.push(ms ?? 0);
      // Delegate with the REAL ms so the retry cadence is preserved, not collapsed.
      return realSetTimeout(fn, ms);
    }) as typeof setTimeout;
    try {
      batcher.emit({ kind: "text", agent: "lead", payload: { text: "the poisoned message" } });
      // Wait until a small fixed number of attempts have failed. Poll via the
      // captured realSetTimeout so these wait timers are NOT recorded into
      // `requested`, and cap on real wall-clock (Date.now, a safety bound only —
      // never an assertion) so contention cannot hang the test.
      const deadline = Date.now() + 3000;
      while (attempts.length < 5 && Date.now() < deadline) {
        await new Promise((r) => realSetTimeout(r, 5));
      }
    } finally {
      (globalThis as { setTimeout: typeof setTimeout }).setTimeout = realSetTimeout;
    }

    assert.ok(attempts.every((a) => a.status === 500), "a store rejection still returns 500 and is still retried");
    // The retry COUNT is still unbounded (the breaker is M3b) — what is gone is
    // the hot loop. A handful, not the unfixed batcher's 62-in-400ms.
    assert.ok(
      attempts.length > 0 && attempts.length <= 12,
      `expected a backed-off handful of attempts, saw ${attempts.length} (unfixed batcher: 62)`,
    );

    // The SCHEDULED delays grow. The leading entry is the healthy cadence
    // (consecutiveFailures===0 → batchMs); every real backoff is ≥ batchMs*2, so
    // drop the healthy 5ms to get the backoff series ~[10,20,40,80,...]. Compare
    // first with last rather than asserting a schedule: ±20% jitter is dominated
    // by the doubling, so this is robust under contention.
    const backoff = requested.filter((ms) => ms > batchMs);
    assert.ok(backoff.length >= 2, `need at least two backoff delays to show growth, saw ${backoff.length}`);
    assert.ok(
      backoff.at(-1)! > backoff[0]! * 2,
      `the scheduled delay must grow: first backoff ${backoff[0]}ms, last ${backoff.at(-1)}ms (unfixed: flat at 6.49ms)`,
    );

    await batcher.close();
  });

  it("M3: the backoff delay is capped, so a long outage never stalls delivery forever", async () => {
    const { logger } = recordingLogger();
    const { client } = poisonedApi();
    // A base already above the cap: every computed delay must clamp to it.
    const batcher = new MessageBatcher(client, "run-1b", 0, MAX_BACKOFF_MS * 4, logger);
    const delays: number[] = [];
    const realSetTimeout = globalThis.setTimeout;
    // Capture what the batcher ASKS for, rather than waiting out real delays.
    (globalThis as { setTimeout: typeof setTimeout }).setTimeout = ((fn: () => void, ms?: number) => {
      delays.push(ms ?? 0);
      return realSetTimeout(fn, 1);
    }) as typeof setTimeout;
    try {
      batcher.emit({ kind: "text", agent: "lead", payload: { text: "x" } });
      await sleep(120);
    } finally {
      (globalThis as { setTimeout: typeof setTimeout }).setTimeout = realSetTimeout;
    }
    assert.ok(delays.length > 1, `expected repeated scheduling, saw ${delays.length}`);
    // The first is the base cadence; every retry after it is clamped (+jitter).
    for (const d of delays.slice(1)) {
      assert.ok(d <= MAX_BACKOFF_MS * (1 + 0.2), `delay ${d}ms exceeds the ${MAX_BACKOFF_MS}ms cap plus jitter`);
    }
    await batcher.close();
  });

  it("M3: the re-buffered batch never grows past the byte cap, however long the failure lasts", async () => {
    const { logger } = recordingLogger();
    const { client, attempts } = poisonedApi();
    const batcher = new MessageBatcher(client, "run-2", 0, 5, logger);

    // Keep emitting fat messages while the flush loop fails, exactly as a working
    // agent does: the incident's worker was still creating files at 18:36/18:37.
    // 64 KiB apiece, so ~24 of them would blow past 1 MiB in one body if the
    // batcher still posted the whole buffer.
    batcher.emit(fatMessage(64 * 1024));
    for (let i = 0; i < 30; i++) {
      await sleep(4);
      batcher.emit(fatMessage(64 * 1024));
    }
    await sleep(60);

    assert.ok(attempts.length > 0, "the batcher must have tried at least once");
    const largest = Math.max(...attempts.map((a) => a.bytes));
    assert.ok(
      largest <= MAX_BATCH_BYTES,
      `no body may exceed the ${MAX_BATCH_BYTES}-byte grouping cap, saw ${largest}`,
    );
    // The buffer behind it still grows — nothing is succeeding — but that growth
    // no longer reaches the wire. This is the whole point of splitting.
    assert.ok(
      attempts.some((a) => a.count > 1),
      "batches still carry multiple messages; the cap bounds bytes, it does not force one-at-a-time",
    );

    await batcher.close();
  });

  it("M3: the batch never crosses the api's 1 MiB cap, so the failure cannot change class to 400", async () => {
    const { logger } = recordingLogger();
    const { client, attempts } = poisonedApi();
    const batcher = new MessageBatcher(client, "run-3", 0, 5, logger);

    // The exact shape that crossed the cap at attempt 18 on the unfixed batcher.
    batcher.emit(fatMessage(96 * 1024));
    for (let i = 0; i < 20; i++) {
      await sleep(6);
      batcher.emit(fatMessage(96 * 1024));
    }
    await sleep(60);

    assert.ok(attempts.length > 0);
    const oversize = attempts.filter((a) => a.bytes > MAX_BODY_BYTES);
    assert.strictEqual(
      oversize.length,
      0,
      `no attempt may exceed the api's ${MAX_BODY_BYTES}-byte cap; ${oversize.length} did, largest ${Math.max(...attempts.map((a) => a.bytes))} (unfixed batcher peaked at 1,475,645)`,
    );
    // And because nothing is oversized, the stub — which derives its status the
    // same way the api does — never has cause to answer 400. The failure class
    // stays honest, which is the precondition for making 4xx fatal in M3b.
    assert.ok(
      attempts.every((a) => a.status === 500),
      "every attempt stays a retryable 500; the class never rotates under the batcher",
    );

    await batcher.close();
  });

  it("M3: a message too large to ever be accepted is replaced by a marker under its OWN seq", async () => {
    const { logger, lines } = recordingLogger();
    const { client, attempts } = poisonedApi();
    const batcher = new MessageBatcher(client, "run-5", 0, 5, logger);

    batcher.emit({ kind: "text", agent: "lead", payload: { text: "before" } });
    batcher.emit(fatMessage(MAX_MESSAGE_BYTES + 4096));
    batcher.emit({ kind: "text", agent: "lead", payload: { text: "after" } });
    await sleep(40);

    const posted = attempts[0];
    assert.ok(posted, "the batch must have been attempted");
    assert.ok(posted.bytes < MAX_BATCH_BYTES, `the oversized message must not reach the wire, body was ${posted.bytes}`);
    // Seq CONTIGUITY is the point: `web/src/lib/runStream.ts` renders nothing past
    // a gap (`seq > lastSeq + 1` buffers into `pending` and never advances
    // `lastSeq`), so a dropped seq would freeze the live view for the rest of the
    // run. All three messages are still there, 1/2/3.
    assert.strictEqual(posted.count, 3);
    assert.strictEqual(batcher.currentSeq(), 3);

    const warned = lines.find(
      (l): l is Record<string, unknown> =>
        !!l && typeof l === "object" &&
        (l as { msg?: string }).msg === "run message too large for the api to accept; replaced with a marker",
    );
    assert.ok(warned, "the replacement is logged, never silent");
    assert.strictEqual(warned["seq"], 2, "and it names the seq it replaced");

    await batcher.close();
  });

  it("M3: a message that is large but ACCEPTABLE is delivered intact, alone if need be", async () => {
    // The regression guard for the cap: a payload between the grouping cap and the
    // hard cap is one the api takes today, so the worker must not mangle it.
    const { logger } = recordingLogger();
    const sent: OutgoingMessage[][] = [];
    const client = {
      async postMessages(_runId: string, messages: OutgoingMessage[]): Promise<void> {
        sent.push(messages);
      },
    } as unknown as WorkerClient;
    const batcher = new MessageBatcher(client, "run-6", 0, 5, logger);

    const big = 700 * 1024; // > MAX_BATCH_BYTES, < MAX_MESSAGE_BYTES
    batcher.emit(fatMessage(big));
    batcher.emit({ kind: "text", agent: "lead", payload: { text: "small" } });
    await batcher.close();

    const all = sent.flat();
    assert.strictEqual(all.length, 2, "both messages are delivered");
    const content = (all[0]?.payload as { content?: string } | undefined)?.content;
    assert.strictEqual(content?.length, big, "the large payload is delivered byte-for-byte, not truncated");
    // It exceeds the grouping cap, so it went out in a sub-batch of its own rather
    // than dragging the next message over the cap with it.
    assert.strictEqual(sent[0]?.length, 1, "the oversized-for-grouping message goes alone");
  });

  it("STILL TODAY (M3b's): close() gives up after 3 failed attempts and DROPS the whole buffer", async () => {
    const { logger, lines } = recordingLogger();
    const { client } = poisonedApi();
    const batcher = new MessageBatcher(client, "run-4", 0, 5, logger);

    for (let i = 0; i < 5; i++) batcher.emit({ kind: "text", agent: "lead", payload: { text: `m${i}` } });
    await batcher.close();

    const dropped = lines.find(
      (l): l is Record<string, unknown> =>
        !!l && typeof l === "object" && (l as { msg?: string }).msg === "message batcher closed with undelivered messages",
    );
    assert.ok(dropped, "close() logs the drop");
    assert.strictEqual(dropped["dropped"], 5, "every buffered message is lost — the incident's dropped: 239");
    // The loss is permanent and silent to the user: nothing routes it anywhere a
    // consumer can see. M3 owns keeping this to ONE message via bisection.
  });
});
