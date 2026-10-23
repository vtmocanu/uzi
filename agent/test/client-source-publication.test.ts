import assert from "node:assert/strict";
import { afterEach, beforeEach, it, mock } from "node:test";
import { WorkerClient } from "../src/client.js";
import type { StateRequest } from "../src/protocol.js";
import { nullLogger } from "./helpers.js";

const FEATURE = "refresh_publication_context_v1";
const HEAD = "a".repeat(40);
let client: WorkerClient;
let features: string[];
let sent: StateRequest[];
let respond: () => Response;
let sleeping: () => Promise<void>;
const body = (): StateRequest => ({ status: "completed", claim_generation: 3, branch: "agent/issue-1", mr_iid: 7,
  source_publication: { version: 1, published_head: HEAD, non_closing: false } });

beforeEach(() => {
  features = [FEATURE]; // Deliberately no generation-fence or inventory capability.
  sent = [];
  respond = () => Response.json({ run: { status: "completed" } });
  sleeping = async () => {};
  mock.method(globalThis, "fetch", async (input: string | URL | Request, init?: RequestInit) => {
    if (String(input).endsWith("/register"))
      return Response.json({ worker_id: "worker", protocol_features: features });
    sent.push(JSON.parse(String(init?.body)));
    return respond();
  });
  client = new WorkerClient("http://source.test", "fixture-worker", "test", nullLogger(), {
    sleep: async () => sleeping(), terminalRetrySchedule: [0, 0],
  });
});
afterEach(() => mock.restoreAll());

it("sends proof and generation independently of recovery and generation capability", async () => {
  await client.register("worker");
  assert.equal((await client.reportState("run", body())).applied, true);
  assert.deepEqual(sent, [body()]);
});

for (const generation of [undefined, 0, -1, Number.MAX_SAFE_INTEGER + 1, 1.5]) {
  it("refuses unsafe or missing generation " + generation, async () => {
    await client.register("worker");
    await assert.rejects(client.reportState("run", { ...body(), claim_generation: generation }), /source publication/);
    assert.equal(sent.length, 0);
  });
}
for (const patch of [
  { branch: undefined }, { branch: "unsafe\u202ebranch" }, { mr_iid: 0 },
  { mr_iid: undefined }, { report_only: true }, { status: "failed" },
]) {
  it("refuses non-publication identity " + JSON.stringify(patch), async () => {
    await client.register("worker");
    await assert.rejects(client.reportState("run", { ...body(), ...patch } as StateRequest), /source publication/);
    assert.equal(sent.length, 0);
  });
}
for (const field of ["head", "completion_final_head"] as const) {
  it("refuses mismatching " + field, async () => {
    await client.register("worker");
    await assert.rejects(client.reportState("run", { ...body(), [field]: "b".repeat(40) }), /source publication/);
    assert.equal(sent.length, 0);
  });
}
it("refuses missing feature before any send", async () => {
  features = [];
  await client.register("worker");
  await assert.rejects(client.reportState("run", body()), /source publication/);
  assert.equal(sent.length, 0);
});
it("refuses feature shrink during transport retry sleep", async () => {
  await client.register("worker");
  respond = () => { throw new Error("lost ACK"); };
  sleeping = async () => { features = []; await client.register("worker"); };
  await assert.rejects(client.reportState("run", body()), /source publication/);
  assert.equal(sent.length, 1);
  assert.deepEqual(sent[0], body());
});
it("strict decoder refusal sends once, keeps proof and generation, and is definitely unapplied", async () => {
  await client.register("worker");
  respond = () => Response.json({ error: "invalid request body" }, { status: 400 });
  await assert.rejects(client.reportState("run", body()), { name: "StateReportUnappliedError" });
  assert.deepEqual(sent, [body()]);
});
it("lost ACK retries the immutable nested original despite caller mutation", async () => {
  await client.register("worker");
  const original = body();
  const expected = structuredClone(original);
  respond = () => {
    if (sent.length < 3) throw new Error("lost ACK");
    return Response.json({ run: { status: "completed" } });
  };
  sleeping = async () => {
    original.claim_generation = 9;
    original.source_publication!.published_head = "b".repeat(40);
    original.source_publication!.non_closing = true;
  };
  await client.reportState("run", original);
  assert.deepEqual(sent, [expected, expected, expected]);
});
for (const proof of [
  { version: 2, published_head: HEAD, non_closing: false },
  { version: 1, published_head: HEAD.toUpperCase(), non_closing: false },
  { version: 1, published_head: HEAD },
]) {
  it("refuses malformed proof " + JSON.stringify(proof), async () => {
    await client.register("worker");
    await assert.rejects(client.reportState("run", { ...body(), source_publication: proof } as StateRequest), /source publication/);
    assert.equal(sent.length, 0);
  });
}
