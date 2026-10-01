// Issue #1800: the steering channel reports a follow-up the executor really put into a prompt
// (POST /inputs/included) and re-queues one an earlier claim consumed but never reported. These
// drive a real WorkerClient against FakeApi, which models the receipt state and the route.
import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { FakeApi } from "./fake-api.js";
import { nullLogger } from "./helpers.js";
import { WorkerClient } from "../src/client.js";
import { ChatSteering, SteeringChannel } from "../src/steering.js";
import type { UserInput } from "../src/protocol.js";

const TOKEN = "worker-join-token-0123456789";
const RUN = "inclusion-run";
const GENERATION = 3;
let api: FakeApi;
let baseUrl: string;
const channels: SteeringChannel[] = [];

beforeEach(async () => {
  api = new FakeApi(TOKEN);
  api.strictReceiptGenerations = true;
  api.setInputClaimGeneration(RUN, GENERATION);
  baseUrl = await api.listen();
});

afterEach(async () => {
  await Promise.all(channels.splice(0).map((ch) => ch.stop()));
  await api.close();
});

function client(): WorkerClient {
  return new WorkerClient(baseUrl, TOKEN, "0.1.0-test", nullLogger(), { sleep: async () => {}, terminalRetrySchedule: [1, 1, 1] });
}

function channel(): SteeringChannel {
  const ch = new SteeringChannel(client(), RUN, 1, nullLogger(), new AbortController(), { claimGeneration: GENERATION });
  channels.push(ch);
  return ch;
}

const until = async (ready: () => boolean): Promise<void> => {
  for (let i = 0; i < 500 && !ready(); i++) await new Promise((r) => setTimeout(r, 2));
  assert.ok(ready(), "condition reached");
};
const settle = (ms = 40): Promise<void> => new Promise((r) => setTimeout(r, ms));

/** Start the channel and pull the follow-ups 7 and 8; returns the second (a real later id). */
async function pullSevenAndEight(ch: SteeringChannel): Promise<{ first: number; second: number }> {
  api.setInputs(RUN, [
    { id: 7, kind: "follow_up", body: "seven" },
    { id: 8, kind: "follow_up", body: "eight" },
  ]);
  ch.start();
  const got: number[] = [];
  await until(() => {
    const f = ch.pullFollowUp();
    if (f) got.push(f.id);
    return got.length >= 2;
  });
  return { first: got[0]!, second: got[1]! };
}

/** Start the channel and pull the one follow-up the poll routes (id 7 body "seven"). */
async function pullSeven(ch: SteeringChannel): Promise<{ id: number; body: string }> {
  api.setInputs(RUN, [{ id: 7, kind: "follow_up", body: "seven" }]);
  ch.start();
  let pulled: { id: number; body: string } | undefined;
  await until(() => (pulled ??= ch.pullFollowUp()) !== undefined);
  return pulled!;
}

describe("SteeringChannel follow-up inclusion receipts (issue #1800)", () => {
  it("pullFollowUp returns the input id with the body", async () => {
    assert.deepStrictEqual(await pullSeven(channel()), { id: 7, body: "seven" });
  });

  it("posts one receipt with the channel's claim generation, once per id however often it is marked", async () => {
    const ch = channel();
    const { id } = await pullSeven(ch);
    ch.markFollowUpIncluded(id);
    ch.markFollowUpIncluded(id);
    await until(() => api.inclusionCalls.length >= 1);
    ch.markFollowUpIncluded(id); // after the flush: still a no-op
    await settle();
    assert.deepStrictEqual(api.inclusionCalls, [{ runId: RUN, ids: [7], generation: GENERATION }]);
  });

  it("keeps the id queued and retries after a failed receipt", async () => {
    api.failingInclusions = { status: 503, times: 1 };
    const ch = channel();
    const { id } = await pullSeven(ch);
    ch.markFollowUpIncluded(id);
    await until(() => api.inclusionCalls.length >= 2);
    await settle();
    assert.strictEqual(api.inclusionCalls.length, 2, "one failed attempt, one that landed, then nothing more");
    assert.deepStrictEqual(api.inclusionCalls[1], { runId: RUN, ids: [7], generation: GENERATION });
  });

  for (const status of [404, 405]) {
    it(`re-sends after one transient untyped ${status} (a rolling api upgrade) and the id is stamped`, async () => {
      api.inclusionTracking = true;
      api.failingInclusions = { status, times: 1 };
      const ch = channel();
      const { id } = await pullSeven(ch);
      ch.markFollowUpIncluded(id);
      await until(() => api.inclusionCalls.length >= 2);
      await settle();
      assert.strictEqual(api.inclusionCalls.length, 2, "one refused attempt, one that landed, then nothing more");
      assert.deepStrictEqual(api.inclusionCalls[1], { runId: RUN, ids: [7], generation: GENERATION });
      assert.ok(api.isInputIncluded(RUN, 7), "the fake stamped the id included");
    });
  }

  it("stops after one typed not-owned 404", async () => {
    api.inclusionNotOwned = true;
    const ch = channel();
    const { first, second } = await pullSevenAndEight(ch);
    ch.markFollowUpIncluded(first);
    await until(() => api.inclusionCalls.length >= 1);
    ch.markFollowUpIncluded(second);
    await settle();
    assert.strictEqual(api.inclusionCalls.length, 1, "a typed stale 404 is final: nothing is sent after it");
  });

  it("drops an id whose receipt says the claim is no longer active, and sends nothing for a later mark", async () => {
    const ch = channel();
    const { first, second } = await pullSevenAndEight(ch);
    api.setInputClaimGeneration(RUN, GENERATION + 1); // a newer claim took the run
    ch.markFollowUpIncluded(first);
    await until(() => api.inclusionCalls.length >= 1);
    ch.markFollowUpIncluded(second);
    await settle();
    assert.strictEqual(api.inclusionCalls.length, 1, "an inactive receipt is final: no retry, no later send");
  });

  it("flushes at stop, with bounded attempts against a failing api", async () => {
    const ch = channel();
    const { id } = await pullSeven(ch);
    api.failingInclusions = { status: 503, times: 1000 };
    ch.markFollowUpIncluded(id);
    await ch.stop();
    const attempts = api.inclusionCalls.length;
    assert.ok(attempts >= 1 && attempts <= 5, `a bounded number of attempts at stop (saw ${attempts})`);
    await settle();
    assert.strictEqual(api.inclusionCalls.length, attempts, "nothing is sent after stop returned");
  });
});

describe("SteeringChannel.requeueUnincludedFollowUps (issue #1800)", () => {
  const row = (id: number, extra: Partial<UserInput> = {}): UserInput => ({ id, kind: "follow_up", body: `body-${id}`, inclusion_reported: true, ...extra });

  it("re-queues only consumed follow-ups the api never saw included, oldest first", async () => {
    const ch = channel();
    ch.requeueUnincludedFollowUps([
      row(9),
      row(3, { included_at: "2026-10-01T00:00:00Z" }), // already included
      row(4, { inclusion_reported: false }), // an api that predates inclusion tracking
      row(5, { body: "  " }), // blank
      row(6, { kind: "answer" }), // not a follow-up
      row(2),
      row(9), // a duplicate id
    ]);
    assert.deepStrictEqual(ch.pullFollowUp(), { id: 2, body: "body-2" });
    assert.deepStrictEqual(ch.pullFollowUp(), { id: 9, body: "body-9" });
    assert.strictEqual(ch.pullFollowUp(), undefined);
  });

  it("does not queue a re-queued follow-up a second time when the live poll routes the same id", async () => {
    const ch = channel();
    ch.requeueUnincludedFollowUps([row(7, { body: "seven" })]);
    api.setInputs(RUN, [{ id: 7, kind: "follow_up", body: "seven" }, { id: 8, kind: "follow_up", body: "eight" }]);
    ch.start();
    assert.deepStrictEqual(ch.pullFollowUp(), { id: 7, body: "seven" });
    let second: { id: number; body: string } | undefined;
    await until(() => (second ??= ch.pullFollowUp()) !== undefined);
    assert.deepStrictEqual(second, { id: 8, body: "eight" }, "the next delivery is the NEW row, not a replay of 7");
    await settle();
    assert.strictEqual(ch.pullFollowUp(), undefined, "7 was delivered exactly once");
  });
});

describe("ChatSteering inclusion receipts (issue #1800)", () => {
  it("delivers the seeded first message with its input id and reports it included with the claim generation", async () => {
    // A chat's first message is a seeded follow_up row; it reaches the executor through the same
    // poll as every later one, so its id is known and it is stamped like any other.
    api.setInputs(RUN, [{ id: 21, kind: "follow_up", body: "how does the gate work?" }]);
    const chat = new ChatSteering(client(), RUN, 1, nullLogger(), new AbortController(), {}, GENERATION);
    chat.start();
    const input = await chat.awaitFollowUp(100_000);
    assert.deepStrictEqual(input, { kind: "message", text: "how does the gate work?", id: 21 });
    chat.markFollowUpIncluded(21);
    chat.markFollowUpIncluded(21);
    await chat.stop(); // the final turn's receipt is drained at stop, not left to a poll tick
    assert.deepStrictEqual(api.inclusionCalls, [{ runId: RUN, ids: [21], generation: GENERATION }]);
  });
});
