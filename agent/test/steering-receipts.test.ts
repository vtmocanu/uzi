// Issue #1673: the /inputs drain is recoverable. GET is read-only, ACK records receipt, applied
// confirms it, and a reply lost at any step is retried with the same ids. These drive a real
// WorkerClient against FakeApi, which models the server's receipt state and lost replies.
import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { FakeApi } from "./fake-api.js";
import { nullLogger } from "./helpers.js";
import { WorkerClient } from "../src/client.js";
import { SteeringChannel } from "../src/steering.js";
import type { UserInput } from "../src/protocol.js";

const TOKEN = "worker-join-token-0123456789";
const RUN = "receipt-run";
let api: FakeApi;
let baseUrl: string;
const channels: SteeringChannel[] = [];

beforeEach(async () => {
  api = new FakeApi(TOKEN);
  // Every receipt test names its claim generation; an unset one fails loudly.
  api.strictReceiptGenerations = true;
  baseUrl = await api.listen();
});

afterEach(async () => {
  // Stop every channel even when an assertion threw first, so no poll loop outlives its test.
  await Promise.all(channels.splice(0).map((ch) => ch.stop()));
  await api.close();
});

/** A real client whose first `lostGets` GET replies are dropped AFTER the server answered. */
function clientLosingGets(lostGets: number): WorkerClient {
  const client = new WorkerClient(baseUrl, TOKEN, "0.1.0-test", nullLogger(), {
    sleep: async () => {},
    terminalRetrySchedule: [1, 1, 1],
  });
  const getInputs = client.getInputs.bind(client);
  client.getInputs = async (runId: string) => {
    const reply = await getInputs(runId);
    if (lostGets-- > 0) throw new Error("GET reply lost after the server answered");
    return reply;
  };
  return client;
}

function channel(client: WorkerClient, generation: number, cancel = new AbortController()): SteeringChannel {
  const ch = new SteeringChannel(client, RUN, 1, nullLogger(), cancel, { claimGeneration: generation });
  channels.push(ch);
  ch.start();
  return ch;
}

const until = async (ready: () => boolean): Promise<void> => {
  for (let i = 0; i < 500 && !ready(); i++) await new Promise((r) => setTimeout(r, 2));
  assert.ok(ready(), "condition reached");
};

const unapplied = async (): Promise<number[]> =>
  (await clientLosingGets(0).getInputs(RUN)).inputs.map((row) => row.id);

describe("recoverable /inputs drain (issue #1673)", () => {
  const lostReplyCases: Array<{ kind: UserInput["kind"]; body: string | null; delivered: (ch: SteeringChannel, cancel: AbortController) => Promise<void> }> = [
    {
      kind: "cancel",
      body: null,
      delivered: async (ch, cancel) => {
        await until(() => ch.isCancelled());
        assert.strictEqual(cancel.signal.aborted, true);
      },
    },
    {
      kind: "approve_plan",
      body: null,
      delivered: async (ch) => assert.deepStrictEqual(await ch.awaitVerdict(), { kind: "approve", selection: { status: "absent" } }),
    },
    {
      kind: "reject_plan",
      body: "no",
      delivered: async (ch) => assert.deepStrictEqual(await ch.awaitVerdict(), { kind: "reject", reason: "no" }),
    },
    {
      kind: "stop",
      body: null,
      delivered: async (ch) => assert.deepStrictEqual(await ch.awaitFollowUp(100_000), { kind: "ended", reason: "stopped" }),
    },
    {
      kind: "follow_up",
      body: "constraint",
      delivered: async (ch) => assert.deepStrictEqual(await ch.awaitFollowUp(100_000), { kind: "followup", body: "constraint" }),
    },
  ];

  for (const c of lostReplyCases) {
    it(`re-delivers a ${c.kind} whose GET and ACK replies were both lost, once`, async () => {
      api.setInputClaimGeneration(RUN, 3);
      api.setInputs(RUN, [{ id: 11, kind: c.kind, body: c.body }]);
      // The ACK commits server-side and its reply is lost: under the old consume-on-read GET this
      // is exactly the reply that dropped a cancel or a plan verdict for good.
      api.loseNextInputReceiptReply("ack");
      const cancel = new AbortController();
      const ch = channel(clientLosingGets(1), 3, cancel);
      await c.delivered(ch, cancel);
      await until(() => api.inputReceiptCalls.some((call) => call.kind === "applied"));
      await ch.awaitReceiptSettlement();
      assert.deepStrictEqual(
        api.inputReceiptCalls.map((call) => [call.kind, call.ids, call.generation]),
        [["ack", [11], 3], ["ack", [11], 3], ["applied", [11], 3]],
        "the lost ACK was retried with the same id, then applied once",
      );
      assert.deepStrictEqual(await unapplied(), [], "nothing is left to replay");
    });
  }

  it("delivers follow-up 7 before 8, once each, across a failed GET and a lost ACK", async () => {
    api.setInputClaimGeneration(RUN, 1);
    api.setInputs(RUN, [
      { id: 7, kind: "follow_up", body: "seven" },
      { id: 8, kind: "follow_up", body: "eight" },
    ]);
    api.loseNextInputReceiptReply("ack");
    const ch = channel(clientLosingGets(1), 1);
    await until(() => api.inputReceiptCalls.some((call) => call.kind === "applied"));
    await ch.awaitReceiptSettlement();
    assert.strictEqual(ch.pullFollowUp(), "seven");
    assert.strictEqual(ch.pullFollowUp(), "eight");
    assert.strictEqual(ch.pullFollowUp(), undefined, "each follow-up reaches the lead once");
    assert.deepStrictEqual(ch.operatorConstraints(), ["seven", "eight"]);
  });

  it("retries a lost applied reply with the same ids without routing again", async () => {
    api.setInputClaimGeneration(RUN, 1);
    api.setInputs(RUN, [{ id: 7, kind: "revise_plan", body: "tighten it" }, { id: 8, kind: "follow_up", body: "once" }]);
    api.loseNextInputReceiptReply("applied");
    const ch = channel(clientLosingGets(0), 1);
    await until(() => api.inputReceiptCalls.filter((call) => call.kind === "applied").length >= 2);
    await ch.awaitReceiptSettlement();
    assert.deepStrictEqual(
      api.inputReceiptCalls.map((call) => [call.kind, call.ids]),
      [["ack", [7, 8]], ["applied", [7, 8]], ["applied", [7, 8]]],
    );
    assert.strictEqual(ch.pullFollowUp(), "once");
    assert.strictEqual(ch.pullFollowUp(), undefined);
    assert.deepStrictEqual(await unapplied(), []);
  });

  it("ends the old flight and leaves an ACKed batch to the next claim when the ACK reply is lost across a reclaim", async () => {
    api.setInputClaimGeneration(RUN, 1);
    api.setInputs(RUN, [{ id: 7, kind: "cancel", body: null }]);
    // Hold the old flight's ACK reply until the claim has moved to generation 2.
    api.loseNextInputReceiptReply("ack");
    const oldCancel = new AbortController();
    const oldClient = clientLosingGets(0);
    const ack = oldClient.ackInputs.bind(oldClient);
    const ackReplies: boolean[] = [];
    oldClient.ackInputs = async (...args: Parameters<WorkerClient["ackInputs"]>) => {
      const reply = await ack(...args);
      ackReplies.push(reply.active);
      return reply;
    };
    const old = channel(oldClient, 1, oldCancel);
    await until(() => api.inputReceiptCalls.length >= 1);
    api.setInputClaimGeneration(RUN, 2);
    await until(() => ackReplies.length >= 1);
    assert.deepStrictEqual(ackReplies, [false], "the retried ACK reports the old claim inactive");
    await until(() => oldCancel.signal.aborted);
    assert.strictEqual((oldCancel.signal.reason as Error).name, "ClaimFencedSignal", "the superseded flight ends");
    assert.strictEqual(old.isCancelled(), false, "the fenced flight routed nothing");
    assert.ok(!api.inputReceiptCalls.some((call) => call.kind === "applied" && call.generation === 1));
    await old.stop();

    const nextCancel = new AbortController();
    const next = channel(clientLosingGets(0), 2, nextCancel);
    await until(() => next.isCancelled());
    await until(() => api.inputReceiptCalls.some((call) => call.kind === "applied" && call.generation === 2));
    assert.strictEqual(nextCancel.signal.aborted, true);
    assert.deepStrictEqual(await unapplied(), []);
  });
});
