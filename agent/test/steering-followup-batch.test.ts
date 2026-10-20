// Issue #3137 M1: SteeringChannel.pullFollowUps drains pending owner follow-ups as one batch,
// oldest first, bounded by a code-point budget. Entries are fed through requeueUnincludedFollowUps
// (no network); the channel is never started.
import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { SteeringChannel, followUpRunes, MAX_FOLLOW_UP_BATCH_RUNES } from "../src/steering.js";
import type { WorkerClient } from "../src/client.js";
import type { UserInput } from "../src/protocol.js";
import { nullLogger } from "./helpers.js";

function channelWith(entries: { id: number; body: string }[]): SteeringChannel {
  const ch = new SteeringChannel({} as WorkerClient, "batch-run", 1, nullLogger(), new AbortController());
  const rows = entries.map((e) => ({ id: e.id, kind: "follow_up", body: e.body, inclusion_reported: true }) as UserInput);
  ch.requeueUnincludedFollowUps(rows);
  return ch;
}

describe("SteeringChannel.pullFollowUps", () => {
  it("exports a 40000 code-point cap and counts code points", () => {
    assert.equal(MAX_FOLLOW_UP_BATCH_RUNES, 40000);
    assert.equal(followUpRunes("😀😀😀"), 3);
  });

  it("takes all small entries in FIFO order", () => {
    const ch = channelWith([{ id: 4, body: "a" }, { id: 5, body: "b" }, { id: 9, body: "c" }]);
    const got = ch.pullFollowUps(100, true);
    assert.deepEqual(got, [{ id: 4, body: "a" }, { id: 5, body: "b" }, { id: 9, body: "c" }]);
    assert.equal(ch.getLastDeliveredFollowUpId(), 9);
    assert.equal(ch.hasPendingFollowUpOutcome(), false);
    assert.deepEqual(ch.pullFollowUps(100, true), []);
  });

  it("leaves the overflow queued and advances the watermark only over taken ids", () => {
    const ch = channelWith([{ id: 1, body: "aaaa" }, { id: 2, body: "bbbb" }, { id: 3, body: "cccc" }]);
    const got = ch.pullFollowUps(8, true);
    assert.deepEqual(got.map((f) => f.id), [1, 2]);
    assert.equal(ch.getLastDeliveredFollowUpId(), 2);
    assert.equal(ch.hasPendingFollowUpOutcome(), true);
    assert.deepEqual(ch.pullFollowUps(8, true).map((f) => f.id), [3]);
    assert.equal(ch.getLastDeliveredFollowUpId(), 3);
  });

  it("takes an oversized head alone into an empty batch", () => {
    const ch = channelWith([{ id: 1, body: "x".repeat(50) }, { id: 2, body: "y" }]);
    const got = ch.pullFollowUps(10, true);
    assert.deepEqual(got.map((f) => f.id), [1]);
    assert.equal(ch.getLastDeliveredFollowUpId(), 1);
    assert.equal(ch.hasPendingFollowUpOutcome(), true);
  });

  it("does not take an oversized head when the batch is not empty", () => {
    const ch = channelWith([{ id: 1, body: "x".repeat(50) }]);
    assert.deepEqual(ch.pullFollowUps(10, false), []);
    assert.equal(ch.getLastDeliveredFollowUpId(), 0);
    assert.equal(ch.hasPendingFollowUpOutcome(), true);
  });

  it("stops at an over-cap middle entry even if later ones would fit", () => {
    const ch = channelWith([{ id: 1, body: "aa" }, { id: 2, body: "x".repeat(50) }, { id: 3, body: "b" }]);
    const got = ch.pullFollowUps(10, true);
    assert.deepEqual(got.map((f) => f.id), [1]);
    assert.equal(ch.getLastDeliveredFollowUpId(), 1);
    assert.deepEqual(ch.pullFollowUps(10, false), []);
    assert.deepEqual(ch.pullFollowUps(10, true).map((f) => f.id), [2]);
    assert.deepEqual(ch.pullFollowUps(10, true).map((f) => f.id), [3]);
  });

  it("counts code points, not UTF-16 units", () => {
    const ch = channelWith([{ id: 1, body: "😀😀😀" }]);
    assert.equal("😀😀😀".length, 6);
    assert.deepEqual(ch.pullFollowUps(3, false).map((f) => f.id), [1]);
  });
});
