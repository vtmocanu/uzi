import assert from "node:assert/strict";
import { test } from "node:test";
import { WorkerClient } from "../src/client.js";
import type { StateRequest } from "../src/protocol.js";
import type { Logger } from "../src/log.js";

const run = "11111111-1111-4111-8111-111111111111";
const worker = "22222222-2222-4222-8222-222222222222";
const log: Logger = {
  debug() {}, info() {}, warn() {}, error() {}, addSecret() {}, removeSecret() {},
  child() { return this; },
};
const registration = (nonce = "nonce") => ({
  worker_id: worker, register_nonce: nonce, protocol_features: ["worker_memory_pressure_v1"],
});
const request = (): StateRequest => ({
  status: "recovery_wait", recovery_cause: "worker_memory_pressure",
  register_nonce: "nonce", claim_generation: 7, memory_episode: 0,
});
const dto = () => ({
  id: run, worker_id: worker, status: "recovery_wait",
  recovery_wait_cause: "worker_memory_pressure", memory_hold: { episode: 0 },
});
const binding = () => ({
  run_id: run, worker_id: worker, register_nonce: "nonce", claim_generation: 7, memory_episode: 0,
});
function json(value: unknown, status = 200) {
  return new Response(JSON.stringify(value), { status });
}
function client() {
  return new WorkerClient("http://example.invalid", "fixture", "test", log,
    { terminalRetrySchedule: [0], sleep: async () => {} });
}

test("fresh and exact-replay parks bind the wire request without a generation feature", async (t) => {
  const api = client();
  let status = 200;
  const sent: unknown[] = [];
  t.mock.method(globalThis, "fetch", async (url: string, init: RequestInit) => {
    if (url.endsWith("/register")) return json(registration());
    assert.equal(url, `http://example.invalid/api/worker/runs/${run}/state`);
    sent.push(JSON.parse(String(init.body)));
    // applied is intentionally the opposite of the HTTP result; it is not authority.
    return json({ run: dto(), applied: status !== 200 }, status);
  });
  await api.register("fixture");
  for (status of [200, 409]) {
    const ack = await api.reportState(run, request());
    assert.deepEqual(ack.memoryParkAccepted, binding());
    assert.equal(ack.applied, status === 200);
    assert.ok(Object.isFrozen(ack.memoryParkAccepted));
    assert.throws(() => { (ack.memoryParkAccepted as { memory_episode: number }).memory_episode = 9; });
    assert.throws(() => { Object.assign(ack, { memoryParkAccepted: undefined }); });
  }
  assert.deepEqual(sent, [request(), request()]);
});

test("unsupported and malformed local memory parks fail before HTTP", async (t) => {
  const api = client();
  let calls = 0;
  t.mock.method(globalThis, "fetch", async () => { calls++; return json(registration()); });
  await assert.rejects(api.reportState(run, request()));
  api.protocolFeatures = ["worker_memory_pressure_v1"];
  await assert.rejects(api.reportState(run, request()), "a feature without an incarnation is insufficient");
  assert.equal(calls, 0);
  await api.register("fixture");
  for (const patch of [
    { register_nonce: undefined }, { register_nonce: "" }, { register_nonce: "foreign" },
    { register_nonce: "é".repeat(65) }, { claim_generation: undefined }, { claim_generation: 0 },
    { claim_generation: -1 }, { claim_generation: 1.5 }, { claim_generation: Number.MAX_SAFE_INTEGER + 1 },
    { memory_episode: undefined }, { memory_episode: -1 }, { memory_episode: 0.5 },
    { memory_episode: Number.MAX_SAFE_INTEGER + 1 }, { status: "running" }, { require_running: true },
  ]) await assert.rejects(api.reportState(run, { ...request(), ...patch } as StateRequest));
  await assert.rejects(api.reportState("invalid-run", request()));
  assert.equal(calls, 1);
  api.protocolFeatures = [];
  await assert.rejects(api.reportState(run, request()));
  assert.equal(calls, 1);
});

test("foreign, generic, malformed, stale and unrelated decline DTOs never authorize", async (t) => {
  const api = client();
  let payload: unknown;
  let status = 409;
  t.mock.method(globalThis, "fetch", async (url: string) =>
    url.endsWith("/register") ? json(registration()) : json(payload, status));
  await api.register("fixture");
  const variants = [
    ...[
      { id: worker }, { id: undefined }, { worker_id: run }, { worker_id: null },
      { status: "running" }, { status: undefined }, { recovery_wait_cause: undefined },
      { recovery_wait_cause: "empty_turn" }, { recovery_wait_cause: null },
      { memory_hold: undefined }, { memory_hold: null }, { memory_hold: [] },
      { memory_hold: { episode: "0" } }, { memory_hold: { episode: 1 } },
      { memory_hold: { episode: -1 } }, { memory_hold: { episode: 0.5 } },
      { memory_hold: { episode: Number.MAX_SAFE_INTEGER + 1 } },
    ].map((patch) => ({ run: { ...dto(), ...patch } })),
    ...[
      { disposition: "stale_claim" }, { disposition: "released" }, { disposition: "unknown" },
      { disposition: null }, { reason: "wall_expired" }, { reason: null },
      { credential_switch: { generation: 3 } }, { credential_switch: null },
    ].map((patch) => ({ run: dto(), ...patch })),
    null, [], { run: [] },
  ];
  for (status of [200, 409]) {
    for (payload of variants) {
      assert.equal((await api.reportState(run, request())).memoryParkAccepted, undefined,
        JSON.stringify(payload));
    }
  }
});

test("unparseable, missing and oversized bounded ACKs cannot supply proof", async (t) => {
  const api = client();
  let raw = "";
  t.mock.method(globalThis, "fetch", async (url: string) =>
    url.endsWith("/register") ? json(registration()) : new Response(raw));
  await api.register("fixture");
  for (raw of ["", "{", "null", "[]"]) {
    assert.equal((await api.reportState(run, request())).memoryParkAccepted, undefined);
  }
  raw = JSON.stringify({ run: dto(), padding: "x".repeat(6 * 1024 * 1024) });
  await assert.rejects(api.reportState(run, request()));
});

test("strict-decode 400 is not retried or downgraded", async (t) => {
  const api = client();
  const sent: unknown[] = [];
  t.mock.method(globalThis, "fetch", async (url: string, init: RequestInit) => {
    if (url.endsWith("/register")) return json(registration());
    sent.push(JSON.parse(String(init.body)));
    return json({ error: "invalid request body" }, 400);
  });
  await api.register("fixture");
  await assert.rejects(api.reportState(run, request()));
  assert.deepEqual(sent, [request()]);
});

test("delayed registration, same-nonce rotation and revoke invalidate pending acceptance", async (t) => {
  for (const action of ["delayed", "same-nonce", "nonce", "revoke"] as const) {
    const api = client();
    let resolveState!: (res: Response) => void;
    let resolveRegister!: (res: Response) => void;
    let delayRegister = false;
    let nonce = "nonce";
    t.mock.method(globalThis, "fetch", async (url: string) => {
      if (url.endsWith("/register")) {
        if (delayRegister) return new Promise<Response>((resolve) => { resolveRegister = resolve; });
        return json(registration(nonce));
      }
      return new Promise<Response>((resolve) => { resolveState = resolve; });
    });
    await api.register("fixture");
    const pending = api.reportState(run, request());
    let registering: Promise<unknown> | undefined;
    if (action === "revoke") api.revokeMemoryAuthority();
    else {
      delayRegister = action === "delayed";
      nonce = action === "nonce" ? "new-nonce" : "nonce";
      registering = api.register("fixture");
      if (!delayRegister) await registering;
    }
    resolveState(json({ run: dto() }, 409));
    const ack = await pending;
    assert.equal(ack.status, "recovery_wait", action);
    assert.equal(ack.memoryParkAccepted, undefined, action);
    if (delayRegister) {
      resolveRegister(json(registration()));
      await registering;
    }
  }
});

test("caller mutation cannot change wire retries or the captured acceptance binding", async (t) => {
  for (const rotate of [false, true]) {
    const api = client();
    const body = request();
    let resolveState!: (res: Response) => void;
    let count = 0;
    const sent: unknown[] = [];
    t.mock.method(globalThis, "fetch", async (url: string, init: RequestInit) => {
      if (url.endsWith("/register")) return json(registration("nonce"));
      sent.push(JSON.parse(String(init.body)));
      if (++count === 1) return new Promise<Response>((resolve) => { resolveState = resolve; });
      return json({ run: dto() }, 409);
    });
    await api.register("fixture");
    const pending = api.reportState(run, body);
    body.memory_episode = 9;
    body.claim_generation = 99;
    body.register_nonce = "mutated";
    body.status = "failed";
    if (rotate) await api.register("fixture");
    resolveState(json({ error: "retry" }, 503));
    const ack = await pending;
    assert.deepEqual(sent, [request(), request()]);
    assert.deepEqual(ack.memoryParkAccepted, rotate ? undefined : binding());
  }
});

test("caller-mutated episode cannot make a changed response episode authorize", async (t) => {
  const api = client();
  let resolveState!: (res: Response) => void;
  t.mock.method(globalThis, "fetch", async (url: string) => url.endsWith("/register")
    ? json(registration()) : new Promise<Response>((resolve) => { resolveState = resolve; }));
  await api.register("fixture");
  const body = request();
  const pending = api.reportState(run, body);
  body.memory_episode = 9;
  resolveState(json({ run: { ...dto(), memory_hold: { episode: 9 } } }));
  assert.equal((await pending).memoryParkAccepted, undefined);
});
