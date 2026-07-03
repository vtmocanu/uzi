import { afterEach, beforeEach, describe, expect, it } from "vitest";
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
    expect(res.worker_id).toBeTruthy();
    expect(api.registers).toEqual([{ name: "vlad-laptop", version: "0.1.0-test", authorized: true }]);
    expect(api.unauthorized).toBe(0);
  });

  it("rejects a wrong token with 401", async () => {
    const bad = new WorkerClient(baseUrl, "nope", "0.1.0-test", nullLogger());
    await expect(bad.heartbeat()).rejects.toBeInstanceOf(RequestError);
    expect(api.unauthorized).toBe(1);
  });

  it("heartbeats", async () => {
    await newClient().heartbeat();
    expect(api.heartbeats).toBe(1);
  });

  it("returns the claim on 200 and null on 204", async () => {
    const claim = makeClaim({ issue_iid: 42 });
    api.enqueueClaim(claim);
    const client = newClient();

    const got = await client.claimRun();
    expect(got?.run_id).toBe(claim.run_id);
    expect(got?.issue_iid).toBe(42);
    expect(got?.secrets.forge_pat).toBe(claim.secrets.forge_pat);

    expect(await client.claimRun()).toBeNull();
  });
});

describe("reportState", () => {
  it("posts a non-terminal state once", async () => {
    const client = newClient();
    await client.reportState("run-1", { status: "running" });
    expect(api.states).toEqual([{ runId: "run-1", body: { status: "running" } }]);
    expect(api.stateAttempts).toBe(1);
  });

  it("retries a terminal report through transient failures", async () => {
    api.failStateNext(2, 503);
    const client = newClient();
    await client.reportState("run-1", { status: "completed", branch: "agent/issue-1" });
    // 2 injected failures + 1 success.
    expect(api.stateAttempts).toBe(3);
    expect(api.states).toEqual([{ runId: "run-1", body: { status: "completed", branch: "agent/issue-1" } }]);
  });

  it("treats an already-terminal (409) terminal report as success", async () => {
    api.markAlreadyTerminal("run-1");
    const client = newClient();
    await expect(client.reportState("run-1", { status: "failed", failure_reason: "boom" })).resolves.toBeUndefined();
    // The server rejected it as already-terminal, so nothing was recorded, yet
    // the client did not surface an error.
    expect(api.states).toHaveLength(0);
  });

  it("gives up on a permanent 4xx for a terminal report", async () => {
    api.failStateNext(1, 400);
    const client = newClient();
    await expect(client.reportState("run-1", { status: "completed", branch: "b" })).rejects.toBeInstanceOf(RequestError);
    expect(api.stateAttempts).toBe(1); // no retry on a permanent error
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
    expect(seqs).toEqual([6, 7, 8, 9, 10]);
  });

  it("re-sends a failed batch on the next flush, preserving order and gaps", async () => {
    api.failMessagesNext(1, 503); // first /messages POST fails
    const client = newClient();
    const runId = "run-retry";
    const batcher = new MessageBatcher(client, runId, 0, 60_000, nullLogger());

    batcher.emit({ kind: "text", payload: { i: 1 } });
    await batcher.flush(); // fails; seq 1 is put back at the head of the buffer
    expect(api.messages(runId)).toHaveLength(0);

    batcher.emit({ kind: "text", payload: { i: 2 } });
    await batcher.close(); // succeeds; delivers [1, 2] in order

    expect(api.messages(runId).map((m) => m.seq)).toEqual([1, 2]);
  });
});

describe("isTransient", () => {
  it("classifies retryable vs permanent", () => {
    expect(isTransient(new RequestError("POST", "/x", 500, ""))).toBe(true);
    expect(isTransient(new RequestError("POST", "/x", 503, ""))).toBe(true);
    expect(isTransient(new RequestError("POST", "/x", 429, ""))).toBe(true);
    expect(isTransient(new RequestError("POST", "/x", 408, ""))).toBe(true);
    expect(isTransient(new RequestError("POST", "/x", 400, ""))).toBe(false);
    expect(isTransient(new RequestError("POST", "/x", 404, ""))).toBe(false);
    expect(isTransient(new Error("network down"))).toBe(true);
  });
});
