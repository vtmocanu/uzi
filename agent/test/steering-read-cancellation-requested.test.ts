// issue #3860: SteeringChannel.readCancellationRequested is a READ-ONLY, NON-COMMITTING probe for the
// held-predecessor seam: exactly one ownership read, never an input read (an older api consumes
// inputs on read), never markCancelled, never a route.
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

/** Serve an ownership body verbatim, so each field's presence and type is under test control. */
const serve = (body: Record<string, unknown>): void => {
  api.ownershipWire.set(RUN, res => {
    res.writeHead(200, { "Content-Type": "application/json" });
    res.end(JSON.stringify(body));
  });
};
const good = (over: Record<string, unknown> = {}): Record<string, unknown> =>
  ({ status: "running", claim_generation: GEN, claim_released: false, cancellation_requested: true, ...over });
const signal = (): AbortSignal => AbortSignal.timeout(5_000);
const cancelRow: UserInput = { id: 1, kind: "cancel", body: null };

function assertUntouched(ch: SteeringChannel): void {
  assert.equal(ch.isCancelled(), false, "never commits the cancel");
  assert.equal(cancel.signal.aborted, false);
  assert.deepEqual(routed, [], "never routes");
  assert.equal(api.inputGets.get(RUN) ?? 0, 0, "never reads inputs");
  assert.equal(api.pendingInputRows(RUN).length, 1, "the persisted cancel stays pending");
}

describe("readCancellationRequested", () => {
  beforeEach(() => { api.setInputs(RUN, [cancelRow]); });

  it("cancel for a running or claimed row at this generation, one ownership read, nothing committed", async () => {
    for (const status of ACCEPTED) {
      api.ownershipRequests.length = 0;
      serve(good({ status }));
      const ch = channel();
      assert.equal(await ch.readCancellationRequested(signal(), ACCEPTED), "cancel", status);
      assert.equal(api.ownershipRequests.length, 1, "exactly one ownership read");
      assertUntouched(ch);
    }
  });

  describe("unknown", () => {
    const cases: Array<[string, Record<string, unknown>]> = [
      ["field absent (older api)", (() => { const b = good(); delete b.cancellation_requested; return b; })()],
      ["field false", good({ cancellation_requested: false })],
      ["field null", good({ cancellation_requested: null })],
      ["field the string true", good({ cancellation_requested: "true" })],
      ["field the number 1", good({ cancellation_requested: 1 })],
      ["status not accepted", good({ status: "paused" })],
      ["status terminal", good({ status: "cancelled" })],
      ["a later generation", good({ claim_generation: GEN + 1 })],
      ["an earlier generation", good({ claim_generation: GEN - 1 })],
      ["generation absent", (() => { const b = good(); delete b.claim_generation; return b; })()],
      ["claim released", good({ claim_released: true })],
      ["claim_released absent", (() => { const b = good(); delete b.claim_released; return b; })()],
    ];
    for (const [name, body] of cases) {
      it(name, async () => {
        serve(body);
        const ch = channel();
        assert.equal(await ch.readCancellationRequested(signal(), ACCEPTED), "unknown");
        assertUntouched(ch);
      });
    }

    it("a status outside the caller's accepted set", async () => {
      serve(good());
      const ch = channel();
      assert.equal(await ch.readCancellationRequested(signal(), ["claimed"]), "unknown");
      assertUntouched(ch);
    });

    it("an invalid claim generation, with no ownership read", async () => {
      serve(good());
      for (const generation of [0, -1, 1.5, Number.NaN]) {
        assert.equal(await channel(generation).readCancellationRequested(signal(), ACCEPTED), "unknown");
      }
      assert.equal(api.ownershipRequests.length, 0);
    });

    it("a fenced claim, with no ownership read", async () => {
      serve(good());
      const ch = channel();
      (ch as unknown as { fence: string }).fence = "fenced";
      assert.equal(await ch.readCancellationRequested(signal(), ACCEPTED), "unknown");
      assert.equal(api.ownershipRequests.length, 0);
      assertUntouched(ch);
    });

    it("an aborted terminal lifecycle, with no ownership read", async () => {
      serve(good());
      const ch = channel();
      (ch as unknown as { terminalLifecycle: AbortController }).terminalLifecycle.abort();
      assert.equal(await ch.readCancellationRequested(signal(), ACCEPTED), "unknown");
      assert.equal(api.ownershipRequests.length, 0);
      assertUntouched(ch);
    });

    it("the claim fenced while the read is in flight", async () => {
      const ch = channel();
      api.ownershipWire.set(RUN, res => {
        (ch as unknown as { fence: string }).fence = "fenced";
        res.writeHead(200, { "Content-Type": "application/json" });
        res.end(JSON.stringify(good()));
      });
      assert.equal(await ch.readCancellationRequested(signal(), ACCEPTED), "unknown");
      assertUntouched(ch);
    });
  });

  it("a definitive not-owned answer and a transient error both reject, committing nothing", async () => {
    for (const arm of [() => api.setOwnershipNotOwned(RUN), () => api.failOwnership(RUN, 503)]) {
      api.ownershipWire.delete(RUN);
      arm();
      const ch = channel();
      await assert.rejects(ch.readCancellationRequested(signal(), ACCEPTED));
      assertUntouched(ch);
    }
  });

  it("an aborted signal rejects instead of concluding anything", async () => {
    serve(good());
    const controller = new AbortController();
    controller.abort(new Error("bound reached"));
    const ch = channel();
    await assert.rejects(ch.readCancellationRequested(controller.signal, ACCEPTED));
    assertUntouched(ch);
  });

  it("the default fake ownership body serves the field only when a test sets it", async () => {
    api.setOwnershipStatus(RUN, "running", GEN);
    api.ownershipWire.delete(RUN);
    assert.equal(await channel().readCancellationRequested(signal(), ACCEPTED), "unknown");
    api.cancellationRequested.set(RUN, true);
    api.setOwnershipStatus(RUN, "running", GEN);
    // The default body omits claim_released, so the probe stays unknown rather than guessing.
    assert.equal(await channel().readCancellationRequested(signal(), ACCEPTED), "unknown");
  });
});
