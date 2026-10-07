import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { RequestError, WorkerClient, type PlanCrossCheckCandidate } from "../src/client.js";
import { nullLogger } from "./helpers.js";

const CAP = 6 * 1024 * 1024;
const CHUNK_BYTES = 64 * 1024;
const candidate: PlanCrossCheckCandidate = {
  plan_md: "plan",
  milestones: [],
  required_capabilities: [],
  required_tools: [],
  size_class: "s",
  base_commit: "a".repeat(40),
  planning_diff: "",
};
const operations = [
  { name: "submitPlanCrossCheck", call: (client: WorkerClient) => client.submitPlanCrossCheck("run", 1, candidate) },
  { name: "planCrossCheckStatus", call: (client: WorkerClient) => client.planCrossCheckStatus("run", 1) },
  {
    name: "reportCrossCheckVerdict",
    call: (client: WorkerClient) => client.reportCrossCheckVerdict("run", 1, {
      verdict: "approve", reason_class: "approve", summary: "good", items: [],
    }),
  },
];

function newClient(): WorkerClient {
  return new WorkerClient("http://example.com", "fake-worker-token", "test", nullLogger());
}

// A pull-only stream makes consumption and cancellation observable without a server
// correcting the deliberately false Content-Length. No prefetch hides extra reads.
function streamingResponse(bytes: Uint8Array, status = 200, chunkBytes = CHUNK_BYTES) {
  let consumed = 0;
  let cancelled = false;
  const response = new Response(new ReadableStream<Uint8Array>({
    pull(controller) {
      if (consumed === bytes.byteLength) {
        controller.close();
        return;
      }
      const end = Math.min(consumed + chunkBytes, bytes.byteLength);
      controller.enqueue(bytes.subarray(consumed, end));
      consumed = end;
    },
    cancel() { cancelled = true; },
  }, { highWaterMark: 0 }), {
    status,
    headers: { "Content-Length": "1", "Retry-After": "3" },
  });
  return { response, consumed: () => consumed, cancelled: () => cancelled };
}

describe("cross-check response byte bounds", () => {
  for (const operation of operations) {
    it(`${operation.name} rejects an 8 MiB stream despite Content-Length: 1 and cancels at overflow`, async (t) => {
      // The cap-sized prefix is itself valid JSON, so truncating instead of
      // rejecting could wrongly accept an incomplete response.
      const bytes = Buffer.alloc(8 * 1024 * 1024, " ");
      bytes.set(Buffer.from("{}"));
      const stream = streamingResponse(bytes);
      const fetchMock = t.mock.method(globalThis, "fetch", async () => stream.response);
      t.mock.method(stream.response, "text", async () => { assert.fail("must stream before buffering"); });
      await assert.rejects(operation.call(newClient()), /response body exceeds 6291456 bytes/);
      assert.equal(stream.consumed(), CAP + CHUNK_BYTES);
      assert.ok(stream.consumed() < bytes.byteLength);
      assert.equal(stream.cancelled(), true);
      assert.equal(fetchMock.mock.callCount(), 1, "must not add retries");
    });

    it(`${operation.name} accepts exactly the byte boundary including UTF-8 across chunks`, async (t) => {
      const bytes = Buffer.alloc(CAP, " ");
      // Put the two-byte character across a chunk boundary.
      const json = '{"padding":"' + "a".repeat(CHUNK_BYTES - 13) +
        'é","result":"no_row","reason_class":"no_candidate","lead_last_seq":0}';
      bytes.set(Buffer.from(json));
      const stream = streamingResponse(bytes);
      t.mock.method(globalThis, "fetch", async () => stream.response);
      const result = await operation.call(newClient());
      if (operation.name !== "reportCrossCheckVerdict") {
        assert.deepEqual(result, JSON.parse(json));
      } else {
        assert.equal(result, undefined);
      }
      assert.equal(stream.consumed(), CAP);
      assert.equal(stream.cancelled(), false, "EOF at the cap must be accepted");
    });

    it(`${operation.name} rejects one byte beyond the cap even in a single chunk`, async (t) => {
      const bytes = Buffer.alloc(CAP + 1, " ");
      bytes.set(Buffer.from("{}"));
      const stream = streamingResponse(bytes, 200, bytes.byteLength);
      t.mock.method(globalThis, "fetch", async () => stream.response);
      await assert.rejects(operation.call(newClient()), /response body exceeds 6291456 bytes/);
      assert.equal(stream.cancelled(), true);
    });

    it(`${operation.name} preserves HTTP status and Retry-After with bounded error reads`, async (t) => {
      const bytes = Buffer.alloc(8 * 1024 * 1024, "x");
      const stream = streamingResponse(bytes, 429, 4096);
      t.mock.method(globalThis, "fetch", async () => stream.response);
      await assert.rejects(operation.call(newClient()), (error: unknown) => {
        assert.ok(error instanceof RequestError);
        assert.equal(error.status, 429);
        assert.equal(error.body, "x".repeat(4096));
        assert.equal(error.retryAfterHeaderMs, 3000);
        return true;
      });
      assert.equal(stream.consumed(), 4096);
      assert.equal(stream.cancelled(), true);
    });

    it(`${operation.name} preserves malformed JSON and stream failures`, async (t) => {
      t.mock.method(globalThis, "fetch", async () => new Response("{"));
      await assert.rejects(operation.call(newClient()), SyntaxError);
      const failure = new Error("torn response");
      t.mock.method(globalThis, "fetch", async () => new Response(new ReadableStream({
        start(controller) { controller.error(failure); },
      })));
      await assert.rejects(operation.call(newClient()), (error: unknown) => error === failure);
    });
  }

  it("ordinary JSON callers keep their uncapped default", async (t) => {
    const workerId = "w".repeat(CAP + 1);
    t.mock.method(globalThis, "fetch", async () => new Response(JSON.stringify({ worker_id: workerId })));
    assert.equal((await newClient().register("worker")).worker_id, workerId);
  });

  it("cross-check POSTs preserve empty and 204 responses", async (t) => {
    for (const operation of operations.filter((entry) => entry.name !== "planCrossCheckStatus")) {
      t.mock.method(globalThis, "fetch", async () => new Response(""));
      if (operation.name === "submitPlanCrossCheck") await assert.rejects(operation.call(newClient()), /invalid cross-check/);
      else assert.equal(await operation.call(newClient()), undefined);
      t.mock.method(globalThis, "fetch", async () => new Response(null, { status: 204 }));
      if (operation.name === "submitPlanCrossCheck") await assert.rejects(operation.call(newClient()), /invalid cross-check/);
      else assert.equal(await operation.call(newClient()), undefined);
    }
  });
});
