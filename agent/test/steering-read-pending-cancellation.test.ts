// issue #3860: SteeringChannel.readPendingCancellation is a NON-COMMITTING read for the held-predecessor
// seam. It never marks the channel cancelled and never routes; the caller commits after its own checks.
import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { FakeApi } from "./fake-api.js";
import { nullLogger } from "./helpers.js";
import { WorkerClient } from "../src/client.js";
import { SteeringChannel } from "../src/steering.js";
import type { UserInput } from "../src/protocol.js";

const TOKEN = "worker-join-token-0123456789";
const RUN = "6f1c2b3a-1111-4222-8333-444455556666";
const GEN = 4;
const ACCEPTED = ["running", "claimed"];
let api: FakeApi;
let client: WorkerClient;
let cancel: AbortController;
let routed: string[];

beforeEach(async () => {
  api = new FakeApi(TOKEN);
  api.strictReceiptGenerations = true;
  const baseUrl = await api.listen();
  client = new WorkerClient(baseUrl, TOKEN, "0.1.0-test", nullLogger(), { sleep: async () => {}, terminalRetrySchedule: [1, 1] });
  cancel = new AbortController();
  routed = [];
});

afterEach(async () => {
  await api.close();
});

function channel(generation = GEN): SteeringChannel {
  // Never started: the poll loop must not be what delivers or routes anything here.
  const ch = new SteeringChannel(client, RUN, 1, nullLogger(), cancel, { claimGeneration: generation });
  const route = (ch as unknown as { route: (...a: unknown[]) => void }).route.bind(ch);
  (ch as unknown as { route: (...a: unknown[]) => void }).route = (...a: unknown[]) => { routed.push(String(a[0])); route(...a); };
  return ch;
}

const owner = (status: string, generation: number | undefined, released = false): void => {
  api.ownershipWire.set(RUN, res => {
    res.writeHead(200, { "Content-Type": "application/json" });
    res.end(JSON.stringify({ status, claim_released: released, ...(generation !== undefined ? { claim_generation: generation } : {}) }));
  });
};

const row = (id: number, kind: UserInput["kind"], body: string | null = null): UserInput => ({ id, kind, body });
const signal = (): AbortSignal => AbortSignal.timeout(5_000);

describe("readPendingCancellation", () => {
  it("receipt mode: reports a confirmed cancel without committing, routing or consuming it", async () => {
    api.setInputs(RUN, [row(1, "cancel")]);
    owner("running", GEN);
    const ch = channel();
    const result = await ch.readPendingCancellation(signal(), ACCEPTED);
    assert.deepEqual(result, { kind: "cancel" });
    assert.equal(ch.isCancelled(), false);
    assert.equal(cancel.signal.aborted, false);
    assert.deepEqual(routed, []);
    const again = await client.getInputs(RUN, GEN);
    assert.deepEqual(again.inputs.map(i => i.id), [1], "the row is still pending for the poller");
  });

  it("accepts only the statuses the caller names", async () => {
    api.setInputs(RUN, [row(1, "cancel")]);
    owner("claimed", GEN);
    assert.equal((await channel().readPendingCancellation(signal(), ACCEPTED)).kind, "cancel");
    assert.equal((await channel().readPendingCancellation(signal(), ["running"])).kind, "unknown");
    owner("paused", GEN);
    assert.equal((await channel().readPendingCancellation(signal(), ACCEPTED)).kind, "unknown");
  });

  for (const [name, status, generation, released] of [
    ["a later generation owns the run", "running", GEN + 1, false],
    ["an unknown generation", "running", undefined, false],
    ["the claim is released", "running", GEN, true],
  ] as const) {
    it(`unknown when ${name}`, async () => {
      api.setInputs(RUN, [row(1, "cancel")]);
      owner(status, generation, released);
      const ch = channel();
      assert.equal((await ch.readPendingCancellation(signal(), ACCEPTED)).kind, "unknown");
      assert.equal(ch.isCancelled(), false);
    });
  }

  it("none, with no ownership read, when no cancel row is pending", async () => {
    api.setInputs(RUN, [row(1, "follow_up", "hello")]);
    const result = await channel().readPendingCancellation(signal(), ACCEPTED);
    assert.deepEqual(result, { kind: "none" });
    assert.equal(api.ownershipRequests.length, 0);
    assert.deepEqual(routed, []);
  });

  it("unknown, with no input read, for an invalid generation", async () => {
    api.setInputs(RUN, [row(1, "cancel")]);
    owner("running", GEN);
    for (const generation of [0, -1, 1.5, Number.NaN]) {
      assert.equal((await channel(generation).readPendingCancellation(signal(), ACCEPTED)).kind, "unknown");
    }
    assert.equal(api.inputGets.get(RUN) ?? 0, 0);
  });

  it("unknown, with no input read, once the claim is fenced", async () => {
    api.setInputs(RUN, [row(1, "cancel")]);
    owner("running", GEN);
    const ch = channel();
    (ch as unknown as { fence: string }).fence = "fenced";
    assert.equal((await ch.readPendingCancellation(signal(), ACCEPTED)).kind, "unknown");
    assert.equal(api.inputGets.get(RUN) ?? 0, 0);
  });

  it("unknown for a stale observation", async () => {
    owner("running", GEN);
    client.getInputs = async () => ({ inputs: [row(1, "cancel")], receipts: true, staleObservation: true });
    const ch = channel();
    assert.equal((await ch.readPendingCancellation(signal(), ACCEPTED)).kind, "unknown");
    assert.equal(ch.isCancelled(), false);
  });

  it("unknown for a terminal cancel and a malformed batch", async () => {
    owner("running", GEN);
    for (const body of [
      { inputs: [row(1, "cancel")], receipts: true, terminal_cancelled: { run_id: RUN, generation: GEN } },
      { inputs: [{ id: "x", kind: "cancel" }], receipts: true },
    ]) {
      api.rawInputGets(RUN, body, 1);
      const ch = channel();
      assert.equal((await ch.readPendingCancellation(signal(), ACCEPTED)).kind, "unknown", JSON.stringify(body).slice(0, 80));
      assert.equal(ch.isCancelled(), false);
    }
  });

  it("an aborted signal rejects instead of concluding anything", async () => {
    api.setInputs(RUN, [row(1, "cancel")]);
    owner("running", GEN);
    const controller = new AbortController();
    controller.abort(new Error("bound reached"));
    await assert.rejects(channel().readPendingCancellation(controller.signal, ACCEPTED));
  });

  describe("legacy mode (the server consumed the rows)", () => {
    it("returns the rows unrouted; routeLegacyRows withholding the cancel routes the rest", async () => {
      api.legacyConsumeOnRead = true;
      api.setInputs(RUN, [row(1, "cancel"), row(2, "follow_up", "carry on")]);
      owner("running", GEN);
      const ch = channel();
      const result = await ch.readPendingCancellation(signal(), ACCEPTED);
      assert.equal(result.kind, "cancel");
      assert.deepEqual(result.legacyRows?.map(r => r.id), [1, 2]);
      assert.deepEqual(routed, [], "reading routes nothing");
      assert.equal(ch.isCancelled(), false);
      ch.routeLegacyRows(result.legacyRows!, { withholdCancel: true });
      assert.deepEqual(routed, ["follow_up"]);
      assert.equal(ch.isCancelled(), false, "a withheld cancel is never committed");
      ch.routeLegacyRows(result.legacyRows!, { withholdCancel: true });
      assert.deepEqual(routed, ["follow_up"], "an already routed row is not routed twice");
    });

    it("routeLegacyRows without withholding routes the cancel with the normal path", async () => {
      api.legacyConsumeOnRead = true;
      api.setInputs(RUN, [row(1, "cancel")]);
      owner("running", GEN);
      const ch = channel();
      const result = await ch.readPendingCancellation(signal(), ACCEPTED);
      ch.routeLegacyRows(result.legacyRows!, { withholdCancel: false });
      assert.deepEqual(routed, ["cancel"]);
      assert.equal(ch.isCancelled(), true);
    });

    it("none with the rows when there is no cancel, unknown with the rows when ownership disagrees", async () => {
      api.legacyConsumeOnRead = true;
      api.setInputs(RUN, [row(2, "follow_up", "carry on")]);
      const none = await channel().readPendingCancellation(signal(), ACCEPTED);
      assert.equal(none.kind, "none");
      assert.deepEqual(none.legacyRows?.map(r => r.id), [2]);
      api.setInputs(RUN, [row(3, "cancel")]);
      owner("running", GEN + 1);
      const unknown = await channel().readPendingCancellation(signal(), ACCEPTED);
      assert.equal(unknown.kind, "unknown");
      assert.deepEqual(unknown.legacyRows?.map(r => r.id), [3]);
    });
  });
});
