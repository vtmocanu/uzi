// Issue #3149: SteeringChannel keeps the owner follow-ups earlier claims consumed as a history a
// reviewed-plan resume can replay. Rows come through the same seams the runner uses (no network;
// the channel is never started).
import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { SteeringChannel, MAX_FOLLOW_UP_BATCH_RUNES, MAX_PRIOR_OWNER_HISTORY_ENTRIES } from "../src/steering.js";
import type { WorkerClient } from "../src/client.js";
import type { UserInput } from "../src/protocol.js";
import { nullLogger } from "./helpers.js";

function channel(): SteeringChannel {
  return new SteeringChannel({} as WorkerClient, "history-run", 1, nullLogger(), new AbortController());
}
const row = (id: number, body: string | null, extra: Partial<UserInput> = {}): UserInput =>
  ({ id, kind: "follow_up", body, ...extra }) as UserInput;

describe("SteeringChannel prior owner history (issue #3149)", () => {
  it("bounds the history by the per-turn owner-text budget", () => {
    assert.equal(MAX_FOLLOW_UP_BATCH_RUNES, 40000);
  });

  it("caps the entry count: 100 tiny entries are complete, 101 are over_bound", () => {
    assert.equal(MAX_PRIOR_OWNER_HISTORY_ENTRIES, 100);
    const rows = (n: number) => Array.from({ length: n }, (_, k) => row(k + 1, "x"));
    const at = channel();
    at.recordPriorOwnerHistory(rows(MAX_PRIOR_OWNER_HISTORY_ENTRIES));
    assert.equal(at.priorOwnerHistory().status, "complete");
    const past = channel();
    past.recordPriorOwnerHistory(rows(MAX_PRIOR_OWNER_HISTORY_ENTRIES + 1));
    assert.deepEqual(past.priorOwnerHistory(), { status: "over_bound" });
  });

  it("is unavailable until recorded", () => {
    assert.deepEqual(channel().priorOwnerHistory(), { status: "unavailable" });
  });

  it("records trimmed {id, body} in id order, skipping blank and non-follow_up rows and repeated ids", () => {
    const ch = channel();
    ch.recordPriorOwnerHistory([
      row(9, " nine "),
      row(3, "three"),
      row(5, "   "),
      row(6, null),
      { id: 7, kind: "answer", body: "{}" } as UserInput,
      row(3, "three again"),
    ]);
    assert.deepEqual(ch.priorOwnerHistory(), {
      status: "complete",
      entries: [{ id: 3, body: "three" }, { id: 9, body: "nine" }],
    });
  });

  it("excludes rows requeueUnincludedFollowUps re-delivers to the lead", () => {
    const ch = channel();
    const rows = [
      row(1, "consumed and included", { inclusion_reported: true, included_at: "2026-10-01T00:00:00Z" }),
      row(2, "consumed, never included", { inclusion_reported: true }),
      row(3, "older api row"),
    ];
    ch.requeueUnincludedFollowUps(rows);
    ch.recordPriorOwnerHistory(rows);
    assert.deepEqual(ch.priorOwnerHistory(), {
      status: "complete",
      entries: [{ id: 1, body: "consumed and included" }, { id: 3, body: "older api row" }],
    });
    assert.equal(ch.isLiveDeliveryId(2), true, "the requeued id reaches the lead live");
    assert.equal(ch.isLiveDeliveryId(1), false);
    assert.equal(ch.isLiveDeliveryId(3), false);
  });

  it("accepts exactly the bound and reports over_bound one code point past it", () => {
    const atBound = channel();
    atBound.recordPriorOwnerHistory([row(1, "😀".repeat(MAX_FOLLOW_UP_BATCH_RUNES - 1)), row(2, "x")]);
    assert.equal(atBound.priorOwnerHistory().status, "complete");
    const past = channel();
    past.recordPriorOwnerHistory([row(1, "😀".repeat(MAX_FOLLOW_UP_BATCH_RUNES - 1)), row(2, "xx")]);
    assert.deepEqual(past.priorOwnerHistory(), { status: "over_bound" });
  });

  it("computes the bound over the whole snapshot even when an entry is also live (runner order)", () => {
    // Runner order: requeueUnincludedFollowUps, THEN recordPriorOwnerHistory. Id 2 is live through
    // the requeue (its unincluded view) and is also in the history snapshot as a legacy row. A bound
    // computed after dropping live ids would see 30000 runes and report complete.
    const ch = channel();
    const big = "b".repeat(10001);
    ch.requeueUnincludedFollowUps([row(2, big, { inclusion_reported: true })]);
    assert.equal(ch.isLiveDeliveryId(2), true);
    ch.recordPriorOwnerHistory([row(1, "a".repeat(30000)), row(2, big)]);
    assert.deepEqual(ch.priorOwnerHistory(), { status: "over_bound" });
  });

  it("is unavailable after the reload is marked lost, and records only once", () => {
    const lost = channel();
    lost.recordPriorOwnerHistory([row(1, "kept")]);
    lost.markOperatorConstraintsUnavailable();
    assert.deepEqual(lost.priorOwnerHistory(), { status: "unavailable" });

    const once = channel();
    once.recordPriorOwnerHistory([row(1, "first")]);
    once.recordPriorOwnerHistory([row(2, "second")]);
    assert.deepEqual(once.priorOwnerHistory(), { status: "complete", entries: [{ id: 1, body: "first" }] });
  });

  it("returns copies the caller cannot use to rewrite the record", () => {
    const ch = channel();
    ch.recordPriorOwnerHistory([row(1, "kept")]);
    const first = ch.priorOwnerHistory();
    if (first.status === "complete") first.entries[0]!.body = "tampered";
    assert.deepEqual(ch.priorOwnerHistory(), { status: "complete", entries: [{ id: 1, body: "kept" }] });
  });
});
