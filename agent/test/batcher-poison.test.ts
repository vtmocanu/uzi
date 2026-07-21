import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { MessageBatcher } from "../src/batcher.js";
import { RequestError } from "../src/client.js";
import { recordingLogger } from "./helpers.js";
import { sleep } from "../src/util.js";
import type { WorkerClient } from "../src/client.js";
import type { OutgoingMessage } from "../src/protocol.js";

/**
 * PRD #108 M1b — reproduce the 2026-07-21 wedge in the batcher, characterising
 * TODAY's behaviour so M3 has something to flip.
 *
 * These are CHARACTERIZATION tests: every assertion here pins the DEFECT, and is
 * named `TODAY (pre-M3)` so nobody mistakes one for a desired invariant. M3
 * inverts each of them in place — unbounded → bounded, growth → capped+split,
 * class-change → split-and-retry — so the flip is visible in one diff rather than
 * hidden in a deletion.
 *
 * The incident: run 4d4762cf posted a `tool_result` carrying 84 NUL bytes. The
 * store rejected it (SQLSTATE 22P05), `worker_protocol.go`'s `default:` arm
 * returned 500, the batcher read 500 as retryable, re-buffered the identical
 * batch, and looped at ~2 Hz for 27 minutes with the batch climbing 206 → 239
 * messages. Cancelling logged `dropped: 239`.
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

describe("MessageBatcher — the poison-pill wedge (PRD #108 M1b)", () => {
  it("TODAY (pre-M3): a permanently-failing POST is retried without bound, at a fixed cadence", async () => {
    const { logger } = recordingLogger();
    const { client, attempts } = poisonedApi();
    const batchMs = 5;
    const batcher = new MessageBatcher(client, "run-1", 0, batchMs, logger);

    batcher.emit({ kind: "text", agent: "lead", payload: { text: "the poisoned message" } });
    // A bounded observation window — the loop really is unbounded, so the test
    // must be what stops, not the batcher.
    const windowMs = 400;
    await sleep(windowMs);

    // There is no give-up in the steady state: `doFlush` re-buffers and calls
    // `scheduleFlush()` again, forever (batcher.ts:113-118). The exact count is
    // timer scheduling, so assert the ORDER of magnitude, not a number.
    assert.ok(
      attempts.length > 20,
      `expected an unbounded retry loop in ${windowMs}ms at ${batchMs}ms cadence, saw only ${attempts.length} attempts`,
    );
    // Every attempt is the identical batch and every one takes the same 500.
    assert.ok(attempts.every((a) => a.count === 1), "the same single-message batch is re-posted every time");
    assert.ok(attempts.every((a) => a.status === 500), "a store rejection returns 500, which the batcher reads as retryable");

    // And the cadence is FIXED, not backed off: the last interval is no longer
    // than the first. This is the property M3 replaces with exponential backoff.
    const perAttemptMs = windowMs / attempts.length;
    assert.ok(
      perAttemptMs < batchMs * 4,
      `no backoff today: ${attempts.length} attempts in ${windowMs}ms is ~${perAttemptMs.toFixed(1)}ms each`,
    );

    // close() does its own bounded 3 retries on top and then gives up, dropping
    // the buffer — the incident's `dropped: 239`.
    await batcher.close();
  });

  it("TODAY (pre-M3): the re-buffered batch GROWS as new messages pile in behind the poison", async () => {
    const { logger } = recordingLogger();
    const { client, attempts } = poisonedApi();
    const batcher = new MessageBatcher(client, "run-2", 0, 5, logger);

    batcher.emit({ kind: "text", agent: "lead", payload: { text: "poison" } });
    // Keep emitting while the flush loop fails, exactly as a working agent does:
    // the incident's worker was still creating files at 18:36 and 18:37.
    for (let i = 0; i < 12; i++) {
      await sleep(15);
      batcher.emit({ kind: "text", agent: "lead", payload: { text: `message ${i}` } });
    }
    await sleep(30);

    // `doFlush` puts the WHOLE failed batch back at the head (batcher.ts:114,
    // `this.buffer = batch.concat(this.buffer)`), so the next post carries the
    // poison plus everything that arrived since. 206 → 239, in miniature.
    const first = attempts[0];
    const last = attempts.at(-1);
    assert.ok(first && last);
    assert.strictEqual(first.count, 1);
    assert.ok(last.count > first.count, `batch must grow: first=${first.count} last=${last.count}`);
    // Monotone: it never sheds anything, because nothing ever succeeds.
    for (let i = 1; i < attempts.length; i++) {
      const prev = attempts[i - 1];
      const cur = attempts[i];
      assert.ok(prev && cur);
      assert.ok(cur.count >= prev.count, `attempt ${i} shrank: ${prev.count} → ${cur.count}`);
      assert.ok(cur.bytes >= prev.bytes, `attempt ${i} body shrank: ${prev.bytes} → ${cur.bytes}`);
    }

    await batcher.close();
  });

  it("TODAY (pre-M3): that growth crosses the api's 1 MiB cap and the failure CHANGES CLASS from 500 to 400", async () => {
    const { logger } = recordingLogger();
    const { client, attempts } = poisonedApi();
    const batcher = new MessageBatcher(client, "run-3", 0, 5, logger);

    // 96 KiB apiece: ~11 of them cross 1 MiB. A single oversized tool_result
    // would do it alone — this is the SECOND poison trigger, and sanitation does
    // not touch it.
    batcher.emit(fatMessage(96 * 1024));
    for (let i = 0; i < 14; i++) {
      await sleep(12);
      batcher.emit(fatMessage(96 * 1024));
    }
    await sleep(40);

    const firstOversize = attempts.findIndex((a) => a.bytes > MAX_BODY_BYTES);
    assert.ok(firstOversize > 0, `the growing batch must cross ${MAX_BODY_BYTES} bytes; largest seen was ${Math.max(...attempts.map((a) => a.bytes))}`);

    // Before the crossing it is a 500 (retryable, per the contract). After it, a
    // 400 the batcher retries just as blindly — and one that no amount of waiting
    // can clear, because the body only ever gets bigger.
    assert.ok(
      attempts.slice(0, firstOversize).every((a) => a.status === 500),
      "every under-cap attempt is a 500",
    );
    assert.ok(
      attempts.slice(firstOversize).every((a) => a.status === 400),
      "every attempt from the crossing on is a 400 — the class changed under a batcher that cannot tell",
    );
    // Which is exactly why "never retry a 4xx" cannot ship alone (PRD Decision
    // 4): a healthy run riding out a transient 500 outage grows into this 400 and
    // would be failed for a payload that was never poisoned.
    assert.ok(attempts.some((a) => a.status === 500) && attempts.some((a) => a.status === 400));

    await batcher.close();
  });

  it("TODAY (pre-M3): close() gives up after 3 retries and DROPS the whole buffer", async () => {
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
