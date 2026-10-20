import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { readRunAck, WorkerClient } from "../src/client.js";
import { nullLogger } from "./helpers.js";

const CAP = 6 * 1024 * 1024;
const CHUNK = 64 * 1024;
const WORKER = "11111111-1111-4111-8111-111111111111";
const run = { id: "run", worker_id: WORKER, status: "running", completion_revision: 2, completion_deferred: [], completion_attempts: 0 };
function newClient() {
  return new WorkerClient("http://example.com", "fake-worker-token", "test", nullLogger(), { terminalRetrySchedule: [], sleep: async () => {} });
}
function stream(bytes: Uint8Array, length?: string) {
  let consumed = 0;
  let cancelled = false;
  const response = new Response(new ReadableStream<Uint8Array>({
    pull(controller) {
      if (consumed === bytes.length) return controller.close();
      const end = Math.min(consumed + CHUNK, bytes.length);
      controller.enqueue(bytes.subarray(consumed, end));
      consumed = end;
    },
    cancel() { cancelled = true; },
  }, { highWaterMark: 0 }), { headers: length === undefined ? {} : { "Content-Length": length } });
  return { response, consumed: () => consumed, cancelled: () => cancelled };
}

describe("M3 running ACK byte bound and cohesive projection", () => {
  for (const length of [undefined, "1"]) {
    for (const size of [CAP, CAP + 1, CAP + CHUNK * 2]) {
      it(`actual ${size} bytes, Content-Length=${length}: ${size === CAP ? "accepts" : "throws"}`, async t => {
        const bytes = Buffer.alloc(size, " ");
        bytes.set(Buffer.from(JSON.stringify({ run })));
        const source = stream(bytes, length);
        t.mock.method(globalThis, "fetch", async () => source.response);
        t.mock.method(source.response, "text", async () => { assert.fail("must stream before buffering"); });
        if (size === CAP) {
          const ack = await newClient().reportState("run", { status: "running" });
          assert.deepEqual(ack.completionProjection, { revision: 2, deferredIds: [] });
          assert.equal(source.cancelled(), false);
        } else {
          await assert.rejects(newClient().reportState("run", { status: "running" }), /response body exceeds 6291456 bytes/);
          assert.equal(source.cancelled(), true);
        }
        assert.equal(source.consumed(), Math.min(size, CAP + CHUNK));
      });
    }
  }
  it("stream errors escape rather than becoming an empty ACK", async t => {
    const failure = new Error("torn ACK");
    t.mock.method(globalThis, "fetch", async () => new Response(new ReadableStream({
      start(controller) { controller.error(failure); },
    })));
    await assert.rejects(newClient().reportState("run", { status: "running" }), error => error === failure);
  });

  for (const revision of [undefined, null, 0, -1, 1.5, "2", Number.MAX_SAFE_INTEGER + 1]) {
    it(`revision ${revision} never yields a projection`, async () => {
      const ack = await readRunAck(new Response(JSON.stringify({ run: { ...run, completion_revision: revision } })));
      assert.equal(ack.completionProjection, undefined);
    });
  }
  for (const deferred of [undefined, null, {}, [null], ["m"], [{}], [{ milestone_id: null }], [{ milestone_id: "" }], [{ milestone_id: "-m" }], [{ milestone_id: "m".repeat(65) }], [{ milestone_id: "m" }, null]]) {
    it(`rejects whole invalid deferred projection ${JSON.stringify(deferred)}`, async () => {
      const ack = await readRunAck(new Response(JSON.stringify({ run: { ...run, completion_deferred: deferred } })));
      assert.equal(ack.completionProjection, undefined);
      assert.equal(ack.contractRevision, 2, "the independent monotone-cache revision remains available");
    });
  }
  it("deduplicates/sorts IDs and copies no titles or reasons", async () => {
    const ack = await readRunAck(new Response(JSON.stringify({ run: { ...run, completion_deferred: [
      { milestone_id: "z", title: "untrusted", reason: "untrusted" }, { milestone_id: "A._-0" }, { milestone_id: "z" },
    ] } })));
    assert.deepEqual(ack.completionProjection, { revision: 2, deferredIds: ["A._-0", "z"] });
  });
  for (const [attempts, expected] of [
    [undefined, undefined], [null, undefined], [-1, undefined], [1.2, undefined], ["1", undefined],
    [Number.MAX_SAFE_INTEGER + 1, undefined], [0, 0], [2, 2], [Number.MAX_SAFE_INTEGER, Number.MAX_SAFE_INTEGER],
  ] as const) {
    it(`attempt count ${attempts} is strictly decoded`, async () => {
      const ack = await readRunAck(new Response(JSON.stringify({ run: { ...run, completion_attempts: attempts } })));
      assert.equal(ack.completionAttempts, expected);
    });
  }
  for (const registered of [false, true]) {
    for (const workerId of [undefined, null, {}, 42, "", " ", WORKER, "other-worker"]) {
      it(`optional ACK identities registered=${registered}, worker=${JSON.stringify(workerId)}`, async t => {
        t.mock.method(globalThis, "fetch", async (url: string | URL | Request) =>
          String(url).endsWith("/register")
            ? Response.json({ worker_id: WORKER, protocol_features: ["claim_generation_fence"] })
            : Response.json({ run: { id: "run", worker_id: workerId, status: "running" } }));
        const client = newClient();
        if (registered) await client.register("m3");
        const ack = await client.reportState("run", { status: "running" });
        assert.equal(ack.runId, "run");
        const meaningful = typeof workerId === "string" && workerId.trim().length > 0;
        assert.equal("workerId" in ack, meaningful);
        assert.equal("workerIdentityMatches" in ack, registered && meaningful);
        if (registered && meaningful) assert.equal(ack.workerIdentityMatches, workerId === WORKER);
        for (const key of ["sentClaimGeneration", "completionProjection", "completionAttempts"])
          assert.equal(key in ack, false, key);
      });
    }
  }
  for (const runId of [undefined, null, {}, 42, "", " "]) {
    it(`invalid run identity ${JSON.stringify(runId)} stays absent`, async t => {
      t.mock.method(globalThis, "fetch", async () => Response.json({ run: { id: runId, status: "running" } }));
      const ack = await newClient().reportState("run", { status: "running" });
      assert.equal("runId" in ack, false);
    });
  }
  for (const strip of [false, true]) {
    it(`records actual outbound generation after strict-decode fallback strip=${strip}`, async t => {
      const posts: Record<string, unknown>[] = [];
      t.mock.method(globalThis, "fetch", async (url: string | URL | Request, init?: RequestInit) => {
        if (String(url).endsWith("/register")) return Response.json({ worker_id: WORKER, protocol_features: ["claim_generation_fence"] });
        const body = JSON.parse(String(init?.body)) as Record<string, unknown>;
        posts.push(body);
        if (strip && posts.length === 1) return Response.json({ error: "invalid request body" }, { status: 400 });
        return Response.json({ run });
      });
      const client = newClient();
      await client.register("m3");
      const ack = await client.reportState("run", { status: "running", claim_generation: 7 });
      assert.equal(ack.sentClaimGeneration, strip ? undefined : 7);
      assert.equal(ack.workerIdentityMatches, true);
      assert.equal(ack.runId, "run");
      assert.equal(ack.workerId, WORKER);
      assert.equal(posts.length, strip ? 2 : 1);
      assert.equal(posts.at(-1)!.claim_generation, strip ? undefined : 7);
    });
  }
});
