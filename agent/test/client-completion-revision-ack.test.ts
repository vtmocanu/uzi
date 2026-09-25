import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { FakeApi } from "./fake-api.js";
import { nullLogger } from "./helpers.js";
import { WorkerClient } from "../src/client.js";

// Issue #1626 — WorkerClient.reportState threads the /state ACK's RunDTO.completion_revision onto
// the returned StateAck as `contractRevision`, the channel a fresh interlocked run uses to learn the
// contract revision frozen after its claim (the runner's finalize binds the permit to it).

const TOKEN = "worker-join-token-0123456789";
let api: FakeApi;
let baseUrl: string;

beforeEach(async () => {
  api = new FakeApi(TOKEN);
  baseUrl = await api.listen();
});

afterEach(async () => {
  await api.close();
});

function newClient(): WorkerClient {
  return new WorkerClient(baseUrl, TOKEN, "0.1.0-test", nullLogger(), {
    sleep: async () => {},
    terminalRetrySchedule: [1],
  });
}

describe("reportState completion_revision (issue #1626)", () => {
  it("carries the ACK's completion_revision as StateAck.contractRevision", async () => {
    api.setStateAckCompletionRevision("run-1", 1);
    const ack = await newClient().reportState("run-1", { status: "running" });
    assert.equal(ack.contractRevision, 1);
  });

  it("leaves contractRevision undefined when the ACK carries none or a malformed one", async () => {
    const none = await newClient().reportState("run-1", { status: "running" });
    assert.equal(none.contractRevision, undefined, "no completion_revision on the ACK");
    api.setStateAckCompletionRevision("run-2", null);
    const nulled = await newClient().reportState("run-2", { status: "running" });
    assert.equal(nulled.contractRevision, undefined, "a null (never-frozen) revision");
    api.setStateAckCompletionRevision("run-3", "1");
    const str = await newClient().reportState("run-3", { status: "running" });
    assert.equal(str.contractRevision, undefined, "a string revision is never coerced");
  });
});
