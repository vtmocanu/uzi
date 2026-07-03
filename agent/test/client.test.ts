import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { FakeApi } from "./fake-api.js";
import { makeClaim, nullLogger } from "./helpers.js";
import { WorkerClient, RequestError, isTransient } from "../src/client.js";
import { MessageBatcher } from "../src/batcher.js";

const TOKEN = "worker-join-token-0123456789";

let api: FakeApi;
let baseUrl: string;

function newClient(): WorkerClient {
  return new WorkerClient(baseUrl, TOKEN, "0.1.0-test", nullLogger(), {
    // Instant sleeps + a short schedule keep the retry tests fast.
    sleep: async () => {},
    terminalRetrySchedule: [1, 1, 1],
  });
}

beforeEach(async () => {
  api = new FakeApi(TOKEN);
  baseUrl = await api.listen();
});

afterEach(async () => {
  await api.close();
});

describe("register / heartbeat / claim", () => {
  it("registers with name + version and Bearer auth", async () => {
    const res = await newClient().register("vlad-laptop");
    assert.ok(res.worker_id);
    assert.deepStrictEqual(api.registers, [{ name: "vlad-laptop", version: "0.1.0-test", authorized: true }]);
    assert.strictEqual(api.unauthorized, 0);
  });

  it("rejects a wrong token with 401", async () => {
    const bad = new WorkerClient(baseUrl, "nope", "0.1.0-test", nullLogger());
    await assert.rejects(bad.heartbeat(), RequestError);
    assert.strictEqual(api.unauthorized, 1);
  });

  it("heartbeats", async () => {
    await newClient().heartbeat();
    assert.strictEqual(api.heartbeats, 1);
  });

  it("returns the claim on 200 and null on 204", async () => {
    const claim = makeClaim({ issue_iid: 42 });
    api.enqueueClaim(claim);
    const client = newClient();

    const got = await client.claimRun();
    assert.strictEqual(got?.run_id, claim.run_id);
    assert.strictEqual(got?.issue_iid, 42);
    assert.strictEqual(got?.secrets.forge_pat, claim.secrets.forge_pat);

    assert.strictEqual(await client.claimRun(), null);
  });
});

describe("reportState", () => {
  it("posts a non-terminal state once", async () => {
    const client = newClient();
    await client.reportState("run-1", { status: "running" });
    assert.deepStrictEqual(api.states, [{ runId: "run-1", body: { status: "running" } }]);
    assert.strictEqual(api.stateAttempts, 1);
  });

  it("retries a terminal report through transient failures", async () => {
    api.failStateNext(2, 503);
    const client = newClient();
    await client.reportState("run-1", { status: "completed", branch: "agent/issue-1" });
    // 2 injected failures + 1 success.
    assert.strictEqual(api.stateAttempts, 3);
    assert.deepStrictEqual(api.states, [{ runId: "run-1", body: { status: "completed", branch: "agent/issue-1" } }]);
  });

  it("treats an already-terminal (409) terminal report as success", async () => {
    api.markAlreadyTerminal("run-1");
    const client = newClient();
    // The server rejected it as already-terminal, so nothing was recorded, yet
    // the client did not surface an error.
    assert.strictEqual(await client.reportState("run-1", { status: "failed", failure_reason: "boom" }), undefined);
    assert.strictEqual(api.states.length, 0);
  });

  it("gives up on a permanent 4xx for a terminal report", async () => {
    api.failStateNext(1, 400);
    const client = newClient();
    await assert.rejects(client.reportState("run-1", { status: "completed", branch: "b" }), RequestError);
    assert.strictEqual(api.stateAttempts, 1); // no retry on a permanent error
  });
});

describe("MessageBatcher seq numbering", () => {
  it("continues gapless numbering from last_seq across flushes", async () => {
    const client = newClient();
    const runId = "run-seq";
    const batcher = new MessageBatcher(client, runId, 5, 60_000, nullLogger());

    batcher.emit({ kind: "status", payload: { i: 1 } });
    batcher.emit({ kind: "text", payload: { i: 2 } });
    batcher.emit({ kind: "text", payload: { i: 3 } });
    await batcher.flush();

    batcher.emit({ kind: "text", payload: { i: 4 } });
    batcher.emit({ kind: "text", payload: { i: 5 } });
    await batcher.close();

    const seqs = api.messages(runId).map((m) => m.seq);
    assert.deepStrictEqual(seqs, [6, 7, 8, 9, 10]);
  });

  it("re-sends a failed batch on the next flush, preserving order and gaps", async () => {
    api.failMessagesNext(1, 503); // first /messages POST fails
    const client = newClient();
    const runId = "run-retry";
    const batcher = new MessageBatcher(client, runId, 0, 60_000, nullLogger());

    batcher.emit({ kind: "text", payload: { i: 1 } });
    await batcher.flush(); // fails; seq 1 is put back at the head of the buffer
    assert.strictEqual(api.messages(runId).length, 0);

    batcher.emit({ kind: "text", payload: { i: 2 } });
    await batcher.close(); // succeeds; delivers [1, 2] in order

    assert.deepStrictEqual(api.messages(runId).map((m) => m.seq), [1, 2]);
  });
});

describe("isTransient", () => {
  it("classifies retryable vs permanent", () => {
    assert.strictEqual(isTransient(new RequestError("POST", "/x", 500, "")), true);
    assert.strictEqual(isTransient(new RequestError("POST", "/x", 503, "")), true);
    assert.strictEqual(isTransient(new RequestError("POST", "/x", 429, "")), true);
    assert.strictEqual(isTransient(new RequestError("POST", "/x", 408, "")), true);
    assert.strictEqual(isTransient(new RequestError("POST", "/x", 400, "")), false);
    assert.strictEqual(isTransient(new RequestError("POST", "/x", 404, "")), false);
    assert.strictEqual(isTransient(new Error("network down")), true);
  });
});
