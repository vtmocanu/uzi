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
      if (c.kind === "reject_plan") {
        // Issue #1604: a taken reject is never applied by the worker; the run's plan_rejected
        // `failed` transition settles it server-side, so until then it stays replayable.
        await new Promise((r) => setTimeout(r, 50));
        assert.deepStrictEqual(
          api.inputReceiptCalls.map((call) => [call.kind, call.ids, call.generation]),
          [["ack", [11], 3], ["ack", [11], 3]],
          "the lost ACK was retried with the same id; the reject awaits its failed transition",
        );
        assert.deepStrictEqual(await unapplied(), [11], "the reject stays replayable");
        return;
      }
      await until(() => api.inputReceiptCalls.some((call) => call.kind === "applied"));
      // Not awaitReceiptSettlement: after a routed cancel the aborted controller makes a waiting
      // report reject (it must not go out uncertain). Wait for the server to record the apply.
      for (let i = 0; i < 200 && (await unapplied()).length > 0; i++) await new Promise((r) => setTimeout(r, 5));
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

  // Issue #1604: the revise in the batch awaits its revised plan, so the batch's applied receipt
  // carries only the follow-up; the revise stays unapplied (replayable) until it is settled.
  it("retries a lost applied reply with the same ids without routing again", async () => {
    api.setInputClaimGeneration(RUN, 1);
    api.setInputs(RUN, [{ id: 7, kind: "revise_plan", body: "tighten it" }, { id: 8, kind: "follow_up", body: "once" }]);
    api.loseNextInputReceiptReply("applied");
    const ch = channel(clientLosingGets(0), 1);
    await until(() => api.inputReceiptCalls.filter((call) => call.kind === "applied").length >= 2);
    await ch.awaitReceiptSettlement();
    assert.deepStrictEqual(
      api.inputReceiptCalls.map((call) => [call.kind, call.ids]),
      [["ack", [7, 8]], ["applied", [8]], ["applied", [8]]],
    );
    assert.strictEqual(ch.pullFollowUp(), "once");
    assert.strictEqual(ch.pullFollowUp(), undefined);
    assert.deepStrictEqual(await unapplied(), [7], "the revise awaits its revised plan");
  });

  it("ends the old flight and leaves an ACKed batch to the next claim when the ACK reply is lost across a reclaim", async () => {
    api.setInputClaimGeneration(RUN, 1);
    api.setInputs(RUN, [{ id: 7, kind: "cancel", body: null }]);
    // The old flight's ACK commits and its reply is lost; the claim moves to generation 2 before
    // the retry. Move it where the lost reply surfaces, not from a test poll: the steering loop
    // retries within ~1 ms, so a poll could let the retry land on generation 1 and read active.
    api.loseNextInputReceiptReply("ack");
    const oldCancel = new AbortController();
    const oldClient = clientLosingGets(0);
    const ack = oldClient.ackInputs.bind(oldClient);
    const ackReplies: boolean[] = [];
    let reclaimed = false;
    oldClient.ackInputs = async (...args: Parameters<WorkerClient["ackInputs"]>) => {
      let reply;
      try {
        reply = await ack(...args);
      } catch (err) {
        if (!reclaimed) {
          reclaimed = true;
          api.setInputClaimGeneration(RUN, 2);
        }
        throw err;
      }
      ackReplies.push(reply.active);
      return reply;
    };
    const old = channel(oldClient, 1, oldCancel);
    await until(() => ackReplies.length >= 1);
    assert.ok(reclaimed, "the first ACK reply was lost");
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

  it("routes a consume-on-read reply from an older api pod at once, with no receipts", async () => {
    api.setInputClaimGeneration(RUN, 1);
    api.legacyConsumeOnRead = true;
    api.setInputs(RUN, [{ id: 7, kind: "cancel", body: null }]);
    const cancel = new AbortController();
    const ch = channel(clientLosingGets(0), 1, cancel);
    await until(() => ch.isCancelled());
    assert.strictEqual(cancel.signal.aborted, true);
    assert.deepStrictEqual(api.inputReceiptCalls, [], "no ACK or APPLIED for a reply without the receipts marker");
  });
});

// Issue #1604 round 4: the plan-gate replay edges, driven through a real WorkerClient and FakeApi.
describe("plan-gate replay edges (issue #1604 round 4)", () => {
  const UNJUDGED = "Could not confirm which plan this verdict was for — it was ignored; re-send it if you still want it.";
  const REPLAY = "A plan verdict sent before this plan was shown was ignored — re-send it if you still want it.";
  const LEGACY =
    "An approval sent before this plan was shown was already recorded by the server and could not be withdrawn — cancel the run if it should not proceed.";

  function gateChannel(generation: number, opts: { cutoff?: string | "absent"; client?: WorkerClient } = {}) {
    const notices: string[] = [];
    const ch = new SteeringChannel(opts.client ?? clientLosingGets(0), RUN, 1, nullLogger(), new AbortController(), {
      claimGeneration: generation,
      notify: (t) => notices.push(t),
    });
    if (opts.cutoff !== undefined) ch.setReplayCutoff(opts.cutoff === "absent" ? undefined : opts.cutoff);
    channels.push(ch);
    return { ch, notices };
  }

  /** Resolves true if `p` settles within `ms`. */
  const settlesWithin = (p: Promise<unknown>, ms: number): Promise<boolean> =>
    Promise.race([p.then(() => true, () => true), new Promise<boolean>((r) => setTimeout(() => r(false), ms))]);

  it("finding 1(c): on an unjudged claim a gate shown before the replayed backlog is read does not end the fail-closed window", async () => {
    api.setInputClaimGeneration(RUN, 1);
    api.setInputs(RUN, [{ id: 5, kind: "approve_plan", body: null }]);
    api.failInputGets(RUN, 3, 503); // the replayed approve's read fails transiently
    const { ch, notices } = gateChannel(1, { cutoff: "absent" });
    ch.start();
    const epoch = ch.bumpEpoch(); // a gate shown before delivery (an executor that does not wait)
    const verdict = ch.awaitGateEvent(epoch);
    assert.equal(await settlesWithin(verdict, 150), false, "the replayed approve does not settle the gate");
    assert.ok(api.isAcked(RUN, 5), "the approve was read");
    assert.deepEqual(notices.filter((n) => n === UNJUDGED), [UNJUDGED]);
    assert.equal(api.humanPlanApproved(RUN), false, "no approval recorded");
    await until(() => api.isDiscarded(RUN, 5));
    // Delivery is complete and the gate is shown: a fresh approve now acts.
    api.appendInputs(RUN, [{ id: 7, kind: "approve_plan", body: null }]);
    assert.equal((await verdict).kind, "approve");
    await until(() => api.isApplied(RUN, 7) && !api.isDiscarded(RUN, 7));
    assert.equal(api.humanPlanApproved(RUN), true);
  });

  it("finding 1(a): initial delivery completes only once the capped replay list is drained", async () => {
    api.setInputClaimGeneration(RUN, 1);
    api.inputPageSize = 2;
    api.setInputs(RUN, [
      { id: 1, kind: "follow_up", body: "one" },
      { id: 2, kind: "follow_up", body: "two" },
      { id: 3, kind: "follow_up", body: "three" },
      { id: 4, kind: "approve_plan", body: null },
    ]);
    const { ch, notices } = gateChannel(1, { cutoff: "absent" });
    ch.guardInitialDelivery();
    ch.start();
    await ch.awaitInitialDelivery();
    assert.ok(api.isAcked(RUN, 4), "the approve past the first batch was read before delivery completed");
    assert.deepEqual(notices, [UNJUDGED], "it was judged replayed");
    assert.equal(ch.takeResumedGateEvent(), undefined);
    const epoch = ch.bumpEpoch();
    assert.equal(await settlesWithin(ch.awaitGateEvent(epoch), 100), false, "no approve settles the gate");
  });

  it("finding 4: with a known cutoff, a verdict with no comparable created_at is stale", async () => {
    api.setInputClaimGeneration(RUN, 1);
    api.setInputs(RUN, [{ id: 5, kind: "approve_plan", body: null }]); // no created_at stamped
    const { ch, notices } = gateChannel(1, { cutoff: "2026-09-26T10:00:00.000001Z" });
    ch.start();
    await ch.awaitInitialDelivery();
    assert.deepEqual(notices, [REPLAY]);
    assert.equal(await settlesWithin(ch.awaitGateEvent(0), 100), false, "the approve does not settle the gate");
    await until(() => api.isDiscarded(RUN, 5));
    // One created after the cutoff still acts.
    api.appendInputs(RUN, [{ id: 6, kind: "approve_plan", body: null, created_at: "2026-09-26T10:00:01Z" }]);
    assert.equal((await ch.awaitGateEvent(0)).kind, "approve");
  });

  it("finding 2: a disposed approve is discarded — not counted as approval, not served again — and a 404 falls back to leaving it", async () => {
    api.setInputClaimGeneration(RUN, 1);
    api.setInputs(RUN, [{ id: 5, kind: "approve_plan", body: null }]);
    const first = gateChannel(1);
    first.ch.start();
    await until(() => api.isAcked(RUN, 5));
    const epoch = first.ch.bumpEpoch(); // a re-gate: the approve is stale
    assert.equal(await settlesWithin(first.ch.awaitGateEvent(epoch), 50), false);
    await until(() => api.isDiscarded(RUN, 5));
    assert.equal(api.humanPlanApproved(RUN), false, "a discarded approve is not the human approval");
    assert.deepEqual(await unapplied(), [], "and it leaves the replay list");
    assert.ok(!api.inputReceiptCalls.some((c) => c.kind === "applied" && c.ids.includes(5)), "never sent to /inputs/applied");
    await first.ch.stop();

    // An api without the route: the approve stays unapplied, and the lane stops asking.
    api.discardRouteMissing = true;
    api.setInputClaimGeneration(RUN, 2);
    // 9 supersedes 8 in the buffer (8 is disposed of at once), then a re-gate makes 9 stale.
    api.appendInputs(RUN, [{ id: 8, kind: "approve_plan", body: null }, { id: 9, kind: "approve_plan", body: null }]);
    const second = gateChannel(2);
    second.ch.start();
    await until(() => api.inputReceiptCalls.some((c) => c.kind === "discarded" && c.ids.includes(8)));
    const next = second.ch.bumpEpoch();
    assert.equal(await settlesWithin(second.ch.awaitGateEvent(next), 50), false, "the stale approve is ignored");
    assert.equal(api.inputReceiptCalls.filter((c) => c.kind === "discarded" && c.generation === 2).length, 1, "one 404, then no more discards");
    assert.deepEqual(await unapplied(), [8, 9], "both stay unapplied for the next claim");
    assert.equal(api.humanPlanApproved(RUN), false);
  });

  it("finding 3: a stale approve an older api consumed on read gets the could-not-withdraw notice", async () => {
    api.setInputClaimGeneration(RUN, 1);
    api.legacyConsumeOnRead = true;
    api.setInputs(RUN, [{ id: 5, kind: "approve_plan", body: null }]);
    const { ch, notices } = gateChannel(1);
    ch.start();
    await until(() => api.isApplied(RUN, 5));
    await new Promise((r) => setTimeout(r, 10));
    const epoch = ch.bumpEpoch();
    assert.equal(await settlesWithin(ch.awaitGateEvent(epoch), 50), false, "the stale approve is ignored");
    assert.deepEqual(notices, [LEGACY], "the notice says the approval is already recorded");
    assert.equal(api.humanPlanApproved(RUN), true, "the residual: the server recorded it when it returned it");
    assert.deepEqual(api.inputReceiptCalls, [], "no receipt of any kind for a consume-on-read row");
  });
});
