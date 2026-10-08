import assert from "node:assert/strict";
import { test } from "node:test";
import { WorkerClient } from "../src/client.js";
import type { MemoryReservationRequest } from "../src/protocol.js";
import type { Logger } from "../src/log.js";

const run = "11111111-1111-4111-8111-111111111111";
const worker = "22222222-2222-4222-8222-222222222222";
const intervention = "33333333-3333-4333-8333-333333333333";
const request = (): MemoryReservationRequest => ({
  run_id: run, worker_id: worker, intervention_id: intervention,
  register_nonce: "nonce", claim_generation: 1, memory_episode: 0,
  policy: { version: 1, max_interventions: 2 },
});
const response = () => ({
  ...request(), admitted: true, authorizing: true, allowance: { limit: 2, used: 1, remaining: 1 },
});
const registration = (nonce = "nonce") => ({
  worker_id: worker, register_nonce: nonce, protocol_features: ["worker_memory_pressure_v1"],
});
const log: Logger = {
  debug() {}, info() {}, warn() {}, error() {}, addSecret() {}, removeSecret() {},
  child() { return this; },
};
function client() { return new WorkerClient("http://example.invalid", "fixture", "test", log); }
function json(value: unknown) { return new Response(JSON.stringify(value), { status: 200 }); }

test("missing feature fails closed with zero HTTP; unavailable API never supplies a permit", async (t) => {
  let calls = 0;
  t.mock.method(globalThis, "fetch", async () => { calls++; throw new Error("raw transport detail"); });
  const api = client();
  assert.equal(api.memoryIncarnation, undefined);
  await assert.rejects(api.reserveMemoryIntervention(request()), /memory intervention/);
  await assert.rejects(api.reportMemoryInterventionOutcome({ ...request(), outcome: "unknown" }), /memory intervention/);
  assert.equal(calls, 0);
  api.protocolFeatures = ["worker_memory_pressure_v1"];
  await assert.rejects(api.reserveMemoryIntervention(request()), /memory intervention/);
  assert.equal(calls, 1);
});

test("exact bounded request excludes commands/env; denial and history remain typed", async (t) => {
  const api = client();
  let result: unknown = response();
  const sent: { url: string; body: unknown; signal: AbortSignal | null | undefined }[] = [];
  t.mock.method(globalThis, "fetch", async (url: string, init: RequestInit) => {
    if (url.endsWith("/register")) return json(registration());
    sent.push({ url, body: JSON.parse(String(init.body)), signal: init.signal });
    return json(result);
  });
  await api.register("fixture");
  assert.ok(Object.isFrozen(api.memoryIncarnation));
  const controller = new AbortController();
  assert.equal((await api.reserveMemoryIntervention({ ...request(), command: "excluded", env: {} } as MemoryReservationRequest, controller.signal, 1000)).authorizing, true);
  assert.equal(sent[0]?.url, `http://example.invalid/api/worker/runs/${run}/memory/reserve`);
  assert.deepEqual(sent[0]?.body, request());
  assert.ok(sent[0]?.signal);
  result = { ...response(), admitted: false, authorizing: false, allowance: { limit: 2, used: 2, remaining: 0 } };
  assert.equal((await api.reserveMemoryIntervention(request())).admitted, false);
  await api.register("fixture");
  const historical = { ...request(), register_nonce: "old-nonce" };
  result = { ...response(), ...historical, authorizing: false, outcome: "unknown" };
  assert.equal((await api.reserveMemoryIntervention(historical)).outcome, "unknown");
  assert.equal((await api.reportMemoryInterventionOutcome({ ...historical, outcome: "unknown" })).authorizing, false);
  assert.equal(sent.at(-1)?.url, `http://example.invalid/api/worker/runs/${run}/memory/outcome`);
  assert.deepEqual(sent.at(-1)?.body, {
    run_id: run, worker_id: worker, intervention_id: intervention, register_nonce: "old-nonce",
    claim_generation: 1, memory_episode: 0, outcome: "unknown",
  });
});

test("invalid local bindings/policies/outcomes cause no transport", async (t) => {
  let calls = 0;
  t.mock.method(globalThis, "fetch", async () => { calls++; return json(response()); });
  const api = client();
  api.protocolFeatures = ["worker_memory_pressure_v1"];
  for (const patch of [
    { run_id: "run" }, { worker_id: "" }, { intervention_id: "00000000-0000-0000-0000-000000000000" },
    { register_nonce: "" }, { register_nonce: "é".repeat(65) }, { claim_generation: 0 },
    { claim_generation: Number.MAX_SAFE_INTEGER + 1 }, { memory_episode: -1 },
    { policy: { version: 2, max_interventions: 2 } }, { policy: { version: 1, max_interventions: 10000 } },
    { policy: { version: 1, max_interventions: 0 } },
  ]) await assert.rejects(api.reserveMemoryIntervention({ ...request(), ...patch } as MemoryReservationRequest));
  await assert.rejects(api.reportMemoryInterventionOutcome({ ...request(), outcome: "bad" } as never));
  assert.equal(calls, 0);
});

test("malformed response, foreign tuple, unsafe allowance and unknown outcome fail closed", async (t) => {
  const api = client();
  api.protocolFeatures = ["worker_memory_pressure_v1"];
  let result: unknown;
  t.mock.method(globalThis, "fetch", async () => json(result));
  const base = response();
  for (const patch of [
    { run_id: worker }, { worker_id: run }, { intervention_id: run }, { register_nonce: "other" },
    { claim_generation: 2 }, { memory_episode: 1 }, { run_id: undefined },
    { admitted: "true" }, { authorizing: 1 }, { admitted: false },
    { admitted: false, authorizing: false },
    { outcome: "bad" }, { outcome: "unknown" }, { outcome: null },
    { policy: { version: 2, max_interventions: 2 } }, { policy: { version: 1, max_interventions: 3 } },
    { allowance: { limit: 2, used: 1, remaining: 2 } },
    { allowance: { limit: 2, used: -1, remaining: 3 } },
    { allowance: { limit: 2, used: 3, remaining: 0 } },
    { allowance: { limit: 2, used: Number.MAX_SAFE_INTEGER + 1, remaining: 0 } },
    { allowance: { limit: 2, used: 0, remaining: 2 } },
  ]) {
    result = { ...base, ...patch };
    await assert.rejects(api.reserveMemoryIntervention(request()), /memory intervention/);
  }
  for (const raw of ["", "{", "x".repeat(65537), JSON.stringify({ ...base, extra: "x".repeat(65536) })]) {
    t.mock.method(globalThis, "fetch", async () => new Response(raw));
    await assert.rejects(api.reserveMemoryIntervention(request()), /memory intervention/);
  }
  t.mock.method(globalThis, "fetch", async () => new Response("raw private failure", { status: 503 }));
  await assert.rejects(api.reserveMemoryIntervention(request()), (err: Error) => !err.message.includes("private"));
});

test("outcome echo must match and can never authorize", async (t) => {
  const api = client();
  api.protocolFeatures = ["worker_memory_pressure_v1"];
  let result: unknown;
  t.mock.method(globalThis, "fetch", async () => json(result));
  for (const outcome of ["no_signal", "unknown", "confirmed_drained"] as const) {
    result = { ...response(), authorizing: false, outcome };
    assert.equal((await api.reportMemoryInterventionOutcome({ ...request(), outcome })).outcome, outcome);
  }
  result = { ...response(), authorizing: false, outcome: "unknown" };
  await assert.rejects(api.reportMemoryInterventionOutcome({ ...request(), outcome: "confirmed_drained" }));
  result = response();
  await assert.rejects(api.reportMemoryInterventionOutcome({ ...request(), outcome: "unknown" }));
});

test("registration invalidates synchronously before fetch; late completion cannot restore old capture or nonce", async (t) => {
  const api = client();
  let invalidateCount = 0;
  const unsubscribe = api.subscribeMemoryInvalidation(() => {
    invalidateCount++;
    assert.equal(api.memoryIncarnation, undefined);
  });
  assert.throws(() => api.subscribeMemoryInvalidation(() => {}));
  let resolveOld!: (value: Response) => void;
  let count = 0;
  t.mock.method(globalThis, "fetch", async () => {
    count++;
    assert.equal(invalidateCount, count);
    assert.equal(api.memoryIncarnation, undefined);
    if (count === 2) return new Promise<Response>((resolve) => { resolveOld = resolve; });
    return json(registration(count === 3 ? "new-nonce" : "nonce"));
  });
  await api.register("fixture");
  assert.deepEqual(api.memoryIncarnation, { worker_id: worker, register_nonce: "nonce", revision: 1 });
  const old = api.register("fixture");
  await api.register("fixture");
  resolveOld(json(registration("old-nonce")));
  await old;
  assert.deepEqual(api.memoryIncarnation, { worker_id: worker, register_nonce: "new-nonce", revision: 3 });
  unsubscribe();
  api.clearFeatures();
  assert.equal(api.memoryIncarnation, undefined);
  assert.equal(invalidateCount, 3);
});

test("unsupported or malformed registration never captures an incarnation; failed newer registration fences old completion", async (t) => {
  const api = client();
  for (const result of [
    {}, { ...registration(), worker_id: "worker" }, { ...registration(), register_nonce: "" },
    { ...registration(), register_nonce: "x".repeat(129) }, { ...registration(), protocol_features: [] },
  ]) {
    t.mock.method(globalThis, "fetch", async () => json(result));
    await api.register("fixture");
    assert.equal(api.memoryIncarnation, undefined);
  }
  let resolveOld!: (value: Response) => void;
  t.mock.method(globalThis, "fetch", async () => new Promise<Response>((resolve) => { resolveOld = resolve; }));
  const old = api.register("fixture");
  t.mock.method(globalThis, "fetch", async () => { throw new Error("unavailable"); });
  await assert.rejects(api.register("fixture"));
  resolveOld(json(registration()));
  await old;
  assert.equal(api.memoryIncarnation, undefined);
});
