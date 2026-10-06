import assert from "node:assert/strict";
import { it } from "node:test";
import { WorkerClient, readRunAck, RequestError, type PlanCrossCheckCandidate } from "../src/client.js";
import { nullLogger } from "./helpers.js";

const ID = "8b0c1a2e-3f4d-4e5f-8a6b-7c8d9e0f1a2b";
const HASH = "a".repeat(64);
const running = { lead_last_seq: 0, claim_generation: 3, plan_cross_check_settled: false,
  gate_revision: 0, current_plan_sha256: HASH };
const parked = { ...running, lead_last_seq: 12, plan_cross_check_settled: true,
  gate_revision: 2, gate_presentation_id: ID, gate_payload_digest: "b".repeat(64) };
const expectedRunning = { leadLastSeq: 0, claimGeneration: 3, planCrossCheckSettled: false,
  gateRevision: 0, currentPlanSHA256: HASH };
const expectedParked = { ...expectedRunning, leadLastSeq: 12, planCrossCheckSettled: true,
  gateRevision: 2, gatePresentationId: ID, gatePayloadDigest: "b".repeat(64) };
const candidate: PlanCrossCheckCandidate = { plan_md: "plan", milestones: [], required_capabilities: [],
  required_tools: [], size_class: "s", base_commit: "c".repeat(40), planning_diff: "" };
const client = () => new WorkerClient("http://example.com", "fixture-join", "test", nullLogger(),
  { terminalRetrySchedule: [] });
const calls = [
  (c: WorkerClient, signal?: AbortSignal) => c.submitPlanCrossCheck("run", 3, candidate, signal),
  (c: WorkerClient, signal?: AbortSignal) => c.planCrossCheckStatus("run", 3, 1, signal),
];
const invalid = [
  {}, { ...parked, plan_cross_check_settled: undefined }, { ...parked, plan_cross_check_settled: "true" },
  { ...parked, claim_generation: 0 }, { ...parked, claim_generation: Number.MAX_SAFE_INTEGER + 1 },
  { ...parked, lead_last_seq: -1 }, { ...parked, lead_last_seq: 0.5 },
  { ...parked, lead_last_seq: 0x80000000 },
  { ...parked, lead_last_seq: Number.MAX_SAFE_INTEGER + 1 }, { ...parked, lead_last_seq: "12" },
  { ...parked, current_plan_sha256: "g".repeat(64) }, { ...parked, current_plan_sha256: HASH.slice(1) },
  { ...parked, gate_presentation_id: "bad" }, { ...parked, gate_presentation_id: undefined },
  { ...parked, gate_payload_digest: undefined }, { ...parked, gate_payload_digest: "bad" },
  { ...parked, gate_revision: -1 }, { ...parked, gate_revision: 1.5 },
  { ...parked, gate_revision: Number.MAX_SAFE_INTEGER + 1 }, { ...parked, gate_revision: "2" },
  { ...parked, gate_revision: 0 }, { ...running, gate_presentation_id: ID },
  { ...running, gate_payload_digest: HASH }, { ...running, gate_revision: 2 },
];

it("applied running and forced-AA ACKs carry an atomic group, including false and revision zero", async (t) => {
  for (const [status, proof, expected] of [
    ["running", running, expectedRunning],
    ["running", { ...running, plan_cross_check_settled: true }, { ...expectedRunning, planCrossCheckSettled: true }],
    ["awaiting_approval", parked, expectedParked],
  ] as const) {
    const answer = { run: { status, budget_max_iterations: 4 }, ...proof };
    assert.deepEqual((await readRunAck(Response.json(answer))).reconciliation, expected);
    const c = client();
    t.mock.method(globalThis, "fetch", async (url: string | URL | Request) => String(url).endsWith("/register")
      ? Response.json({ worker_id: "w", protocol_features: ["claim_generation_fence"] })
      : Response.json(answer));
    await c.register("w");
    const ack = await c.reportState("run", { status: "running", plan_md: "plan", claim_generation: 3 });
    assert.deepEqual(ack.reconciliation, expected);
    assert.equal(ack.budgetMaxIterations, 4);
    assert.equal(ack.gateRevision, status === "running" ? undefined : 2);
  }
});

it("invalid ACK groups leave readable ordinary fields and positive gateRevision semantics intact", async (t) => {
  for (const proof of invalid) {
    const answer = { run: { status: "running", budget_wall_seconds: 90 }, ...proof };
    const read = await readRunAck(Response.json(answer));
    assert.equal(read.status, "running");
    assert.equal(read.budgetWallSeconds, 90);
    assert.ok(!("reconciliation" in read));
    t.mock.method(globalThis, "fetch", async () => Response.json(answer));
    assert.ok(!("reconciliation" in await client().reportState("run", { status: "running", claim_generation: 3 })));
  }
  assert.deepEqual(await readRunAck(new Response("{")), {});
  t.mock.method(globalThis, "fetch", async () => new Response("{"));
  assert.deepEqual(await client().reportState("run", { status: "running", claim_generation: 3 }),
    { applied: true, status: undefined });
  assert.ok(!("reconciliation" in await readRunAck(Response.json({ run: { status: "awaiting_approval" }, ...running }))));
});

it("refused/stale 409, wrong generation, and 404 never expose state proof", async (t) => {
  for (const code of [200, 409, 404]) {
    const c = client();
    t.mock.method(globalThis, "fetch", async (url: string | URL | Request) => String(url).endsWith("/register")
      ? Response.json({ worker_id: "w", protocol_features: ["claim_generation_fence"] })
      : Response.json({ run: { status: "running" }, disposition: "stale_claim",
        ...parked, claim_generation: code === 200 ? 4 : 3 }, { status: code }));
    await c.register("w");
    if (code === 404) await assert.rejects(c.reportState("run", { status: "running", claim_generation: 3 }), RequestError);
    else assert.ok(!("reconciliation" in await c.reportState("run", { status: "running", claim_generation: 3 })));
  }
  assert.ok(!("reconciliation" in await readRunAck(Response.json({ run: { status: "running" }, ...parked }, { status: 409 }))));
});

it("lost forced ACK can be recovered by parked status/submit with identity, current hash, cursor and generation", async (t) => {
  for (const call of calls) {
    t.mock.method(globalThis, "fetch", async () => Response.json({ result: "parked", verdict: "", reason_class: "", ...parked }));
    const recovered = await call(client());
    assert.equal(recovered.result, "parked");
    assert.deepEqual(recovered.reconciliation, expectedParked);
    t.mock.method(globalThis, "fetch", async () => Response.json({ result: "parked", verdict: "pending",
      reason_class: "", ...parked, plan_cross_check_settled: false, candidate, findings: { summary: "hidden" } }));
    await assert.rejects(call(client()), /invalid parked/);
    for (const proof of [...invalid, running, { ...parked, claim_generation: 4 }]) {
      t.mock.method(globalThis, "fetch", async () => Response.json({ result: "parked", verdict: "", reason_class: "", ...proof }));
      await assert.rejects(call(client()), /invalid parked/);
    }
    t.mock.method(globalThis, "fetch", async () => new Response("{"));
    await assert.rejects(call(client()), SyntaxError);
    t.mock.method(globalThis, "fetch", async () => Response.json({ result: "parked", ...parked }, { status: 409 }));
    await assert.rejects(call(client()), RequestError);
  }
});

it("legacy active/no-row and ordinary ACK shapes remain unchanged; incomplete proof is not invented", async (t) => {
  for (const call of calls) {
    for (const value of [
      { result: "no_row", reason_class: "no_candidate", lead_last_seq: 7 },
      { result: "candidate", round: 1, candidate, candidate_digest: HASH, candidate_generation: 3, lead_last_seq: 7,
        verdict: "pending", reason_class: "", checker_run_id: null, findings: null, deadline_at: "2030-01-01T00:00:00Z" },
    ]) {
      t.mock.method(globalThis, "fetch", async () => Response.json(value));
      assert.deepEqual(await call(client()), value);
      t.mock.method(globalThis, "fetch", async () => Response.json({ ...value, plan_cross_check_settled: true }));
      assert.equal((await call(client())).reconciliation, undefined);
      t.mock.method(globalThis, "fetch", async () => Response.json({ ...value, ...running }));
      assert.deepEqual((await call(client())).reconciliation, expectedRunning);
    }
  }
  t.mock.method(globalThis, "fetch", async () => Response.json({ run: { status: "running" } }));
  assert.deepEqual(await client().reportState("run", { status: "running" }),
    { applied: true, status: "running", completedCount: 0 });
});

it("historical candidates keep the current ownership proof; forged groups cannot rebind a claim", async (t) => {
  for (const call of calls) {
    const historical = { result: "candidate", round: 1, candidate, candidate_digest: HASH,
      candidate_generation: 1, verdict: "failed", reason_class: "interrupted",
      checker_run_id: null, findings: null, deadline_at: "2030-01-01T00:00:00Z", ...running };
    t.mock.method(globalThis, "fetch", async () => Response.json(historical));
    const recovered = await call(client());
    assert.equal(recovered.result, "candidate");
    if (recovered.result !== "candidate") throw new Error("historical candidate lost");
    assert.equal(recovered.candidate_generation, 1);
    assert.deepEqual(recovered.reconciliation, expectedRunning);
    for (const value of [
      { ...historical, claim_generation: 4, reconciliation: expectedParked },
      { result: "no_row", reason_class: "no_candidate", lead_last_seq: 0, reconciliation: expectedParked },
    ]) {
      t.mock.method(globalThis, "fetch", async () => Response.json(value));
      const refused = await call(client());
      assert.ok(!("reconciliation" in refused));
    }
  }
});

it("optional caller abort reaches status and submit fetch without adding retries", async (t) => {
  for (const call of calls) {
    const abort = new AbortController();
    let seen: AbortSignal | undefined;
    const mock = t.mock.method(globalThis, "fetch", async (_url: string | URL | Request, init?: RequestInit) => {
      seen = init?.signal as AbortSignal;
      return await new Promise<Response>((_resolve, reject) => {
        seen!.addEventListener("abort", () => reject(seen!.reason), { once: true });
        abort.abort(new Error("poll stopped"));
      });
    });
    await assert.rejects(call(client(), abort.signal), /poll stopped/);
    assert.equal(seen?.aborted, true);
    assert.equal(mock.mock.callCount(), 1);
  }
});

it("streamed status keeps the exact 6 MiB cap, cancels overflow/errors and preserves status", async (t) => {
  const cap = 6 * 1024 * 1024;
  for (const [size, code] of [[cap, 200], [cap + 1, 200], [cap + 1, 429]]) {
    const bytes = Buffer.alloc(size!, " ");
    bytes.set(Buffer.from(JSON.stringify({ result: "parked", verdict: "", reason_class: "", ...parked })));
    let consumed = 0;
    let cancelled = false;
    t.mock.method(globalThis, "fetch", async () => new Response(new ReadableStream<Uint8Array>({
      pull(controller) {
        if (consumed === bytes.length) { controller.close(); return; }
        const end = Math.min(consumed + 4096, bytes.length);
        controller.enqueue(bytes.subarray(consumed, end)); consumed = end;
      },
      cancel() { cancelled = true; },
    }, { highWaterMark: 0 }), { status: code, headers: { "Content-Length": "1" } }));
    if (code === 429) {
      await assert.rejects(client().planCrossCheckStatus("run", 3), (error: unknown) =>
        error instanceof RequestError && error.status === 429);
      assert.equal(consumed, 4096);
      assert.equal(cancelled, true);
    } else if (size === cap) {
      assert.deepEqual((await client().planCrossCheckStatus("run", 3)).reconciliation, expectedParked);
      assert.equal(cancelled, false);
    } else {
      await assert.rejects(client().planCrossCheckStatus("run", 3), /response body exceeds 6291456 bytes/);
      assert.equal(cancelled, true);
    }
  }
});
