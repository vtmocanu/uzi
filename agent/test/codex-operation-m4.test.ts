import assert from "node:assert/strict";
import { it } from "node:test";
import { WorkerClient, CodexOperationAdmissionError } from "../src/client.js";
import { ActiveRunRegistry } from "../src/active-run-registry.js";
import { acquireCodexOperationScope } from "../src/codex-operation-scope.js";
import { AdmissionClock, flushUntil } from "./operation-admission-clock.js";
import { nullLogger } from "./helpers.js";
import type { CodexOperationRequest } from "../src/protocol.js";

const id = "00000000-0000-4000-8000-000000000001";
const op = { kind: "root" as const, operation_id: id };
const admitted = (r: CodexOperationRequest) => ({ decision: "admit" as const, operation_id: r.operation_id, claim_generation: r.claim_generation });

it("M4 real registry retains a scope registration through held proof and fences phase and successor writes", async () => {
  const registry = new ActiveRunRegistry();
  const registration = registry.add(id, 7);
  let finish!: () => void;
  let proving = false;
  const scope = acquireCodexOperationScope({
    hasFeature: () => true, codexOperationNegotiation: 1,
    checkCodexOperationAdmission: async (_run, request) => admitted(request),
  }, id, 7, {}, operation => {
    const token = registry.setAccountCooldown(id, 7, operation, registration)!;
    return () => registry.clearAccountCooldown(id, 7, token);
  });
  const pending = scope.controller.admit(op, new AbortController().signal, () => {}, async () => {
    proving = true;
    await new Promise<void>(resolve => { finish = resolve; });
  });
  try {
    await flushUntil(() => proving);
    assert.deepEqual(registry.build().active[0]!.account_cooldown, op);
    registry.setPhase(id, "awaiting_input");
    assert.equal(registry.build().active[0]!.account_cooldown, undefined);
    registry.remove(id);
    const successor = registry.add(id, 7);
    registry.setAccountCooldown(id, 7, { ...op, kind: "child" }, successor);
    assert.equal(registry.setAccountCooldown(id, 7, op, registration), undefined);
    finish();
    await pending;
    assert.equal(registry.build().active[0]!.account_cooldown!.kind, "child", "old clear cannot erase successor");
  } finally { finish?.(); await pending; scope.close(); }
});

for (const throws of [false, true]) it(`M4 unavailable diagnostics are fixed, bounded and tolerate logger failure=${throws}`, async () => {
  const clock = new AdmissionClock();
  const codes: string[] = [];
  let reads = 0;
  const scope = acquireCodexOperationScope({
    hasFeature: () => true, codexOperationNegotiation: 1,
    checkCodexOperationAdmission: async (_run, request) => {
      reads++;
      if (reads <= 2) throw new Error("RAW provider body and exception");
      if (reads === 3) return { decision: "wait", ...request, class: "overload",
        episode_id: id, server_now: new Date(0).toISOString(), eligible_at: new Date(1000).toISOString() };
      return admitted(request);
    },
  }, id, 7, { now: () => clock.now, sleep: clock.sleep }, undefined, undefined,
  { ...nullLogger(), warn: code => { codes.push(code); if (throws) throw new Error("logger failed"); } });
  const abort = new AbortController();
  const pending = scope.prepare("root", abort.signal, () => {});
  try {
    for (const expected of [1, 2, 3]) {
      await flushUntil(() => reads === expected && clock.sleepers.size === 1);
      assert.deepEqual(codes, expected < 3 ? ["codex_operation_admission_unavailable"] :
        ["codex_operation_admission_unavailable", "codex_operation_cooldown"]);
      clock.advance(expected < 3 ? 15000 : 1000);
    }
    assert.equal((await pending)(), true);
    assert.equal(reads, 4);
  } finally { abort.abort(); await pending.catch(() => {}); scope.close(); }
});

for (const reason of ["authority_lost", "stale_source", "control", "source"] as const)
it(`M4 ${reason} abort is not diagnosed as unavailable`, async () => {
  const codes: string[] = [];
  let obsolete = false;
  const scope = acquireCodexOperationScope({
    hasFeature: () => true, codexOperationNegotiation: 1,
    checkCodexOperationAdmission: async (_run, request) => {
      if (reason === "authority_lost" || reason === "stale_source") throw new CodexOperationAdmissionError(409, reason);
      obsolete = true;
      return admitted(request);
    },
  }, id, 7, {}, undefined, undefined, { ...nullLogger(), warn: code => { codes.push(code); } });
  try {
    await assert.rejects(scope.prepare("root", new AbortController().signal, () => { if (obsolete) throw new Error(reason); }));
    assert.deepEqual(codes, []);
  } finally { scope.close(); }
});

for (const missing of ["feature", "nonce", "snapshot"] as const)
it(`M4 missing reporting ${missing} logs once only for engaged flights and keeps operation enforcement`, async () => {
  const codes: string[] = [];
  const sent: { path: string; body: Record<string, unknown> }[] = [];
  const features = ["codex_account_operation_admission_v1", ...(missing === "feature" ? [] : ["active_run_snapshot"])];
  const client = new WorkerClient("http://example.com", "token", "test",
    { ...nullLogger(), warn: code => { codes.push(code); } }, { fetch: async (url, init) => {
      const path = String(url);
      sent.push({ path, body: JSON.parse(String(init?.body)) });
      if (path.endsWith("/register")) return Response.json({ worker_id: id, protocol_features: features,
        ...(missing === "nonce" ? {} : { register_nonce: "nonce" }) });
      if (path.endsWith("/heartbeat")) return Response.json({ protocol_features: features });
      if (path.endsWith("/codex/operation-admission")) return Response.json(admitted(sent.at(-1)!.body as unknown as CodexOperationRequest));
      return new Response(null, { status: 204 });
    } });
  await client.register("worker");
  await client.heartbeat();
  await client.claimRun();
  assert.deepEqual(codes, [], "ordinary flights have no diagnostic");
  const scope = acquireCodexOperationScope(client, id, 7);
  const snapshot = missing === "snapshot" ? undefined : { snapshot_epoch: 1, active: [], pending_overflow: false };
  try {
    await client.heartbeat(undefined, undefined, snapshot);
    await client.claimRun(snapshot);
    await client.heartbeat(undefined, undefined, snapshot);
    assert.deepEqual(codes, ["codex_operation_snapshot_unavailable"]);
    assert.equal(sent.slice(-3).every(call => call.body.active_snapshot === undefined), true);
    assert.equal((await scope.prepare("root", new AbortController().signal, () => {}))(), true);
    assert.equal(sent.filter(call => call.path.endsWith("/codex/operation-admission")).length, 1);
  } finally { scope.close(); }
  await client.heartbeat();
  assert.deepEqual(codes, ["codex_operation_snapshot_unavailable"], "released flights cannot repeat diagnostics");
});

it("M4 required root keeps its snapshot when optional advice exits and root HTTP takes over", async () => {
  const registry = new ActiveRunRegistry();
  const registration = registry.add(id, 7);
  let releaseAdvice!: () => void;
  let releaseRoot!: () => void;
  let reads = 0;
  const client = {
    hasFeature: () => true, codexOperationNegotiation: 1,
    checkCodexOperationAdmission: async (_run: string, request: CodexOperationRequest) => {
      reads++;
      await new Promise<void>(resolve => { if (request.kind === "advice") releaseAdvice = resolve; else releaseRoot = resolve; });
      return admitted(request);
    },
  };
  // The first creator is optional, as in the production model-pass factory.
  const advice = acquireCodexOperationScope(client, id, 7);
  const root = acquireCodexOperationScope(client, id, 7, {}, operation => {
    const token = registry.setAccountCooldown(id, 7, operation, registration)!;
    return () => registry.clearAccountCooldown(id, 7, token);
  }, ["root", "child"]);
  const marker = () => registry.build().active[0]!.account_cooldown;
  const optional = advice.prepare("advice", new AbortController().signal, () => {});
  await flushUntil(() => reads === 1);
  assert.equal(marker(), undefined);
  const required = root.prepare("root", new AbortController().signal, () => {});
  try {
    await flushUntil(() => marker()?.kind === "root");
    const operation = marker();
    releaseAdvice();
    await optional;
    await flushUntil(() => reads === 2);
    assert.deepEqual(marker(), operation, "queue handoff and held HTTP retain the required operation");
    releaseRoot();
    await required;
    assert.equal(marker(), undefined);
  } finally { releaseAdvice?.(); releaseRoot?.(); await Promise.allSettled([optional, required]); root.close(); advice.close(); }
});

it("M4 registry falls back to the surviving child after selected child cancels", () => {
  const registry = new ActiveRunRegistry();
  const registration = registry.add(id, 7);
  const first = { ...op, kind: "child" as const };
  const second = { ...first, operation_id: "00000000-0000-4000-8000-000000000002" };
  const a = registry.setAccountCooldown(id, 7, first, registration)!;
  const b = registry.setAccountCooldown(id, 7, second, registration)!;
  assert.deepEqual(registry.build().active[0]!.account_cooldown, second);
  registry.clearAccountCooldown(id, 7, b);
  assert.deepEqual(registry.build().active[0]!.account_cooldown, first);
  registry.clearAccountCooldown(id, 7, b);
  assert.deepEqual(registry.build().active[0]!.account_cooldown, first);
  registry.clearAccountCooldown(id, 7, a);
  assert.equal(registry.build().active[0]!.account_cooldown, undefined);
});

it("M4 replacement source churn retains the unavailable deadline and bounds diagnostics", async () => {
  const clock = new AdmissionClock();
  const codes: string[] = [];
  let reads = 0;
  const scope = acquireCodexOperationScope({
    hasFeature: () => true, codexOperationNegotiation: 1,
    checkCodexOperationAdmission: async (_run, request) => {
      if (++reads === 1) throw new Error("untrusted response body");
      return admitted(request);
    },
  }, id, 7, { now: () => clock.now, sleep: clock.sleep }, undefined, undefined,
  { ...nullLogger(), warn: code => { codes.push(code); } });
  try {
    // Three independent replacements; a failure never blocks sibling cleanup.
    // Each read is HTTP-bounded in production; these virtual waits are explicitly cancelled.
    for (let replacement = 0; replacement < 3; replacement++) {
      const abort = new AbortController();
      const pending = scope.prepare("root", abort.signal, () => {});
      const rejected = assert.rejects(pending, /source replaced/);
      await flushUntil(() => clock.sleepers.size === 1);
      assert.equal(reads, 1);
      abort.abort(new Error("source replaced"));
      await rejected;
    }
    const abort = new AbortController();
    const final = scope.prepare("root", abort.signal, () => {});
    try {
      await flushUntil(() => clock.sleepers.size === 1);
      clock.advance(14999);
      assert.equal(reads, 1);
      clock.advance(1);
      assert.equal((await final)(), true);
      assert.equal(reads, 2);
      assert.deepEqual(codes, ["codex_operation_admission_unavailable"]);
    } finally { abort.abort(); await final.catch(() => {}); }
  } finally { scope.close(); }
});
