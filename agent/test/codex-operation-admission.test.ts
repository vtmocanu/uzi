import assert from "node:assert/strict";
import { it } from "node:test";
import { WorkerClient, CodexOperationAdmissionError } from "../src/client.js";
import { ActiveRunRegistry } from "../src/active-run-registry.js";
import type { CodexOperationRequest, ActiveSnapshot } from "../src/protocol.js";
import { nullLogger } from "./helpers.js";

const id = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa";
const request: CodexOperationRequest = { operation_id: id, kind: "root", claim_generation: 2 };
const operation = { operation_id: id, kind: "root" as const };
const admit = { decision: "admit", operation_id: id, claim_generation: 2 };
const wait = { ...admit, decision: "wait", class: "rate_limit", episode_id: id,
  server_now: "2026-10-27T01:00:00Z", eligible_at: "2026-10-27T01:01:00Z" };
function client(fetcher: typeof fetch) {
  return new WorkerClient("http://example.com", "token", "test", nullLogger(), { fetch: fetcher });
}
function stream(bytes: Buffer, status: number, length?: string) {
  let offset = 0;
  let cancelled = false;
  const response = new Response(new ReadableStream({
    pull(c) {
      if (offset === bytes.length) return c.close();
      c.enqueue(bytes.subarray(offset, offset + 127));
      offset = Math.min(bytes.length, offset + 127);
    },
    cancel() { cancelled = true; },
  }, { highWaterMark: 0 }), { status, headers: length ? { "Content-Length": length } : {} });
  return { response, cancelled: () => cancelled };
}
it("bounds actual streaming success and error bytes independently of Content-Length; never retries", async () => {
  for (const length of [undefined, "1", "999999"]) {
    for (const status of [200, 404, 503]) {
      for (const size of [8192, 8193]) {
        const bytes = Buffer.alloc(size, " ");
        bytes.set(Buffer.from(JSON.stringify(status === 200 ? admit : { error: "no" })));
        const s = stream(bytes, status, length);
        let calls = 0;
        const c = client(async () => { calls++; return s.response; });
        if (size > 8192) {
          await assert.rejects(c.checkCodexOperationAdmission(id, request), /exceeds 8192/);
          assert.equal(s.cancelled(), true);
        } else if (status === 200) assert.deepEqual(await c.checkCodexOperationAdmission(id, request), admit);
        else await assert.rejects(c.checkCodexOperationAdmission(id, request), CodexOperationAdmissionError);
        assert.equal(calls, 1);
      }
    }
  }
});
it("validates closed decisions, tags, UUIDs, generation, classes and timestamps", async () => {
  for (const good of [admit, wait, { ...wait, class: "overload" }]) {
    assert.deepEqual(await client(async () => Response.json(good)).checkCodexOperationAdmission(id, request), good);
  }
  for (const bad of [null, {}, [], "", { ...admit, decision: "unknown" }, { ...admit, class: "overload" },
    { ...admit, extra: true }, { ...admit, operation_id: id.toUpperCase() }, { ...admit, claim_generation: 3 },
    { ...admit, claim_generation: "2" }, { ...wait, class: "other" }, { ...wait, episode_id: "x" },
    { ...wait, server_now: "2026-02-30T01:00:00Z" }, { ...wait, eligible_at: "tomorrow" },
    { ...wait, server_now: "2026-10-27T24:00:00Z" }, { ...wait, eligible_at: wait.server_now }, { ...wait, eligible_at: undefined }]) {
    await assert.rejects(client(async () => Response.json(bad)).checkCodexOperationAdmission(id, request));
  }
  await assert.rejects(client(async () => new Response("")).checkCodexOperationAdmission(id, request));
});
it("distinguishes typed refusals from route uncertainty without text classification", async () => {
  for (const [status, body, reason] of [
    [404, { error: "anything", reason: "authority_lost" }, "authority_lost"],
    [409, { error: "anything", reason: "stale_source" }, "stale_source"],
    [404, { error: "operation authority lost" }, "route_uncertain"],
    [404, { error: "x", reason: "authority_lost", extra: true }, "route_uncertain"],
    [404, { error: "x", reason: "stale_source" }, "route_uncertain"],
  ] as const) {
    await assert.rejects(client(async () => Response.json(body, { status })).checkCodexOperationAdmission(id, request),
      (e: unknown) => e instanceof CodexOperationAdmissionError && e.reason === reason && e.status === status);
  }
});
it("rejects oversized or invalid requests before transport", async () => {
  const c = client(async () => { assert.fail("transport must not run"); });
  for (const bad of [{ ...request, operation_id: "x".repeat(4096) }, { ...request, claim_generation: 0 },
    { ...request, source: { gate_revision: 1, presentation_id: id, revision_input_id: 0 } }]) {
    await assert.rejects(c.checkCodexOperationAdmission(id, bad));
  }
});
it("honors caller abort and the three second deadline during body reads", async () => {
  for (const callerAbort of [true, false]) {
    const controller = new AbortController();
    let cancelled = false;
    let reading!: () => void;
    const started = new Promise<void>(resolve => { reading = resolve; });
    const c = client(async (_url, init) => {
      assert.ok(init?.signal);
      return new Response(new ReadableStream({ pull() { reading(); }, cancel() { cancelled = true; } }, { highWaterMark: 0 }));
    });
    const promise = c.checkCodexOperationAdmission(id, request, controller.signal);
    await started;
    if (callerAbort) controller.abort();
    // Keep the test process alive while AbortSignal.timeout's unref timer runs.
    const keepAlive = setTimeout(() => {}, 4000);
    try { await assert.rejects(promise); } finally { clearTimeout(keepAlive); }
    assert.equal(cancelled, true);
  }
});
it("aborts request transport and never retries", async () => {
  let calls = 0;
  const controller = new AbortController();
  const c = client(async (_url, init) => {
    calls++;
    return new Promise((_resolve, reject) => init?.signal?.addEventListener("abort", () => reject(init.signal?.reason)));
  });
  const promise = c.checkCodexOperationAdmission(id, request, controller.signal);
  controller.abort();
  await assert.rejects(promise);
  assert.equal(calls, 1);
});
it("times out before headers under the same three second deadline", async () => {
  let calls = 0;
  const c = client(async (_url, init) => {
    calls++;
    return new Promise((_resolve, reject) => init?.signal?.addEventListener("abort", () => reject(init.signal?.reason)));
  });
  const keepAlive = setTimeout(() => {}, 4000);
  try { await assert.rejects(c.checkCodexOperationAdmission(id, request)); }
  finally { clearTimeout(keepAlive); }
  assert.equal(calls, 1);
});
it("guards cooldown metadata by registration, generation and update tokens; copies and clears", () => {
  const reg = new ActiveRunRegistry();
  const operation = { operation_id: id, kind: "root" as const,
    source: { gate_revision: 1, presentation_id: id, revision_input_id: 2 } };
  assert.equal(reg.setAccountCooldown(id, 2, operation, Symbol()), undefined);
  const registration = reg.add(id, 2);
  assert.equal(reg.setAccountCooldown(id, 1, operation, registration), undefined);
  const first = reg.setAccountCooldown(id, 2, operation, registration)!;
  const second = reg.setAccountCooldown(id, 2, operation, registration)!;
  reg.clearAccountCooldown(id, 2, first);
  operation.source.gate_revision = 99;
  const built = reg.build();
  assert.equal(built.active[0]!.account_cooldown!.source!.gate_revision, 1);
  built.active[0]!.account_cooldown!.source!.gate_revision = 44;
  assert.equal(reg.build().active[0]!.account_cooldown!.source!.gate_revision, 1);
  reg.clearAccountCooldown(id, 1, second);
  assert.ok(reg.build().active[0]!.account_cooldown);
  reg.clearAccountCooldown(id, 2, second);
  assert.equal(reg.build().active[0]!.account_cooldown, undefined);
  reg.setAccountCooldown(id, 2, operation, registration);
  reg.setPhase(id, "awaiting_input");
  assert.equal(reg.build().active[0]!.account_cooldown, undefined);
  reg.remove(id);
  reg.add(id, 2);
  assert.equal(reg.setAccountCooldown(id, 2, operation, registration), undefined);
  reg.clearAccountCooldown(id, 2, second);
  reg.add(id, 0);
  assert.equal(reg.setAccountCooldown(id, 0, operation, reg.add(id, 0)), undefined);
});
it("pending terminal entries never carry cooldown", () => {
  const reg = new ActiveRunRegistry(() => [{ run_id: id, claim_generation: 2, phase: "running", blocked: false, since: 1 }], () => 1);
  const token = reg.add(id, 2);
  reg.setAccountCooldown(id, 2, operation, token);
  assert.equal(reg.build().active[0]!.account_cooldown, undefined);
});
it("serializes metadata only after negotiation on register, heartbeat and claim without mutation", async () => {
  for (const negotiated of [false, true]) {
    const sent: { path: string; body: { active_snapshot?: ActiveSnapshot } }[] = [];
    const features = ["active_run_snapshot", ...(negotiated ? ["codex_account_operation_admission_v1"] : [])];
    const c = client(async (url, init) => {
      const path = String(url);
      sent.push({ path, body: JSON.parse(String(init?.body)) });
      if (path.endsWith("/register")) return Response.json({ worker_id: id, register_nonce: "nonce", protocol_features: features });
      if (path.endsWith("/heartbeat")) return Response.json({ protocol_features: features });
      return new Response(null, { status: 204 });
    });
    const snapshot: ActiveSnapshot = { snapshot_epoch: 1, active: [{ run_id: id, claim_generation: 2,
      phase: "running", terminal_pending: false, account_cooldown: operation }], pending_overflow: false };
    const original = structuredClone(snapshot);
    await c.register("worker", undefined, undefined, undefined, undefined, snapshot);
    assert.equal(sent.at(-1)!.body.active_snapshot!.active[0]!.account_cooldown, undefined);
    await c.heartbeat(undefined, undefined, snapshot);
    await c.claimRun(snapshot);
    await c.register("worker", undefined, undefined, undefined, undefined, snapshot);
    for (const entry of sent.slice(1)) {
      assert.equal(Boolean(entry.body.active_snapshot!.active[0]!.account_cooldown), negotiated, entry.path);
      if (!entry.path.endsWith("/register")) assert.equal(entry.body.active_snapshot!.register_nonce, "nonce");
    }
    assert.deepEqual(snapshot, original);
  }
});
