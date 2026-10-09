import assert from "node:assert/strict";
import { it } from "node:test";
import { WorkerClient, RequestError, isStrictDecodeError, isTransient, type PlanCrossCheckCandidate } from "../src/client.js";
import { nullLogger, makeClaim } from "./helpers.js";

it("cleanup metadata rejects ordinary ownership and nonexact identities", async (t) => {
  const lead = "11111111-1111-4111-8111-111111111111";
  const valid = { protocol: "code_snapshot_cleanup_v1", lead_run_id: lead,
    head_commit: "a".repeat(40), outcome: "completed", lead_status: "running",
    owned_by_worker: true, checker_run_id: "22222222-2222-4222-8222-222222222222",
    checker_claim_generation: 1, checker_status: "completed" };
  t.mock.method(globalThis, "fetch", async (url: Parameters<typeof fetch>[0]) => {
    assert.match(String(url), /ownership\?purpose=code_snapshot/);
    return Response.json(valid);
  });
  assert.deepEqual(await client().getCodeSnapshotCleanup(lead), valid);
  for (const body of [{ status: "completed", claim_generation: 1 },
    { ...valid, extra: true }, { ...valid, head_commit: valid.head_commit + "\\n" },
    { ...valid, checker_claim_generation: 0 }]) {
    t.mock.method(globalThis, "fetch", async () => Response.json(body));
    await assert.rejects(client().getCodeSnapshotCleanup(lead), /invalid code snapshot cleanup/);
  }
});

it("legacy plan claims and statuses decode without code-stage fields", async (t) => {
  const legacy = makeClaim({ kind: "cross_check", cross_check: { stage: "plan", lead_run_id: "lead", round: 1,
    candidate_digest: "b".repeat(64), deadline_at: "2030-01-01T00:00:00Z",
    plan_md: "legacy plan", milestones: [], required_capabilities: [], required_tools: [],
    size_class: "s", base_commit: "a".repeat(40), planning_diff: "" } });
  t.mock.method(globalThis, "fetch", async () => Response.json(legacy, { headers: { "X-Uzi-Claim-Kind": "cross_check" } }));
  const decoded = await client().claimCrossCheck();
  assert.equal(decoded?.cross_check?.stage, "plan");
  assert.equal(decoded?.code_cross_check_required, undefined);
  assert.equal(decoded?.cross_check && "head_commit" in decoded.cross_check, false);
  t.mock.method(globalThis, "fetch", async () => Response.json(wire()));
  const status = await client().planCrossCheckStatus("lead", 3);
  assert.equal(status.result, "candidate");
  assert.equal("head_commit" in status, false);
});

it("production dedicated claim client discriminates stages and retains plan requirements", async (t) => {
  const plan = makeClaim({ kind: "cross_check", cross_check: { stage: "plan", lead_run_id: "lead", round: 1,
    candidate_digest: "b".repeat(64), deadline_at: "2030-01-01T00:00:00Z",
    plan_md: "plan", milestones: [], required_capabilities: [], required_tools: [],
    size_class: "s", base_commit: "a".repeat(40), planning_diff: "" } });
  for (const cross_check of [
    { ...plan.cross_check, stage: "unknown" },
    { ...plan.cross_check, planning_diff: undefined },
    { ...plan.cross_check, milestones: undefined },
    { ...plan.cross_check, stage: "code", head_commit: "bad", code_context: {} },
  ]) {
    t.mock.method(globalThis, "fetch", async () => Response.json({ ...plan, cross_check },
      { headers: { "X-Uzi-Claim-Kind": "cross_check" } }));
    await assert.rejects(client().claimCrossCheck(), /invalid cross-check claim/);
  }
  t.mock.method(globalThis, "fetch", async () => Response.json({ ...plan, cross_check: {
    ...plan.cross_check, stage: "code", head_commit: "d".repeat(40), code_context: {},
    planning_diff: undefined, guidance_snapshot: undefined,
  } }, { headers: { "X-Uzi-Claim-Kind": "cross_check" } }));
  assert.equal((await client().claimCrossCheck())?.cross_check?.stage, "code");
});

it("production code verdict client rejects duplicate IDs, non-ASCII IDs and actual UTF-8 bounds before HTTP", async (t) => {
  const fetch = t.mock.method(globalThis, "fetch", async () => Response.json({}));
  const finding = { id: "F1", severity: "major" as const, path: "source.ts", line: 1, title: "defect", detail: "" };
  const valid = { ...finding, detail: "é".repeat(900) };
  const c = client();
  await c.reportCodeCrossCheckVerdict("child", 1, { outcome: "completed", findings: [valid] });
  assert.equal(fetch.mock.callCount(), 1);
  for (const findings of [
    [finding, finding],
    [{ ...finding, id: "é" }],
    [{ ...finding, id: "valid_ID\n" }],
    [{ ...finding, id: "valid_ID\r" }],
    [{ ...finding, id: "control\u0000" }],
    [{ ...finding, id: "ansi\u001b[31m" }],
    [{ ...finding, id: "bidi\u202e" }],
    [{ ...finding, detail: "é".repeat(1024) }],
    Array.from({ length: 21 }, (_, i) => ({ ...finding, id: String(i) })),
    Array.from({ length: 20 }, (_, i) => ({ ...valid, id: String(i) })),
  ]) await assert.rejects(c.reportCodeCrossCheckVerdict("child", 1, { outcome: "completed", findings }), /invalid code/);
  assert.equal(fetch.mock.callCount(), 1, "invalid output never reaches HTTP");
});

const candidate: PlanCrossCheckCandidate = { plan_md: "plan", planning_diff: "", milestones: [],
  required_capabilities: [], required_tools: [], size_class: "s", base_commit: "a".repeat(40) };
const wire = () => ({ result: "candidate", round: 1, checker_run_id: null,
  candidate_digest: "b".repeat(64), candidate_generation: 3, candidate,
  verdict: "approve", reason_class: "approve", findings: null,
  deadline_at: "2030-01-01T00:00:00Z", lead_last_seq: 4 });
const client = () => new WorkerClient("http://example.com", "fixture-join", "test", nullLogger(),
  { sleep: async () => {} });

it("submit and status validate stored variants before exposing approve", async (t) => {
  const malformed = [null, {}, { ...wire(), round: 2 }, { ...wire(), candidate_digest: "g".repeat(64) },
    { ...wire(), candidate_generation: 0 }, { ...wire(), checker_run_id: "not-uuid" },
    { ...wire(), deadline_at: "tomorrow" }, { ...wire(), deadline_at: "2030-02-30T00:00:00Z" },
    { ...wire(), lead_last_seq: 0x80000000 },
    { ...wire(), reason_class: "revise" }, { ...wire(), verdict: "unknown" },
    { ...wire(), candidate: { ...candidate, milestones: null } },
    { ...wire(), candidate: { ...candidate, base_commit: "g".repeat(40) } },
    { ...wire(), candidate: { ...candidate, required_tools: [1] } },
    { ...wire(), candidate: { ...candidate, required_capabilities: ["x".repeat(257)] } },
    { ...wire(), candidate: { ...candidate, plan_md: "é".repeat(128 * 1024 + 1) } },
    { ...wire(), candidate: { ...candidate, planning_diff: "x".repeat(512 * 1024 + 1) } },
    { ...wire(), candidate: { ...candidate, milestones: Array(65).fill({ id: "m", title: "title" }) } },
    { ...wire(), findings: { summary: "x".repeat(4097), items: [] } },
    { ...wire(), findings: { summary: "", items: [{ file: "", severity: "critical", summary: "", rationale: "" }] } },
    { result: "no_row", reason_class: "unknown", lead_last_seq: 0 },
    { result: "no_row", reason_class: "no_candidate", lead_last_seq: -1 }];
  for (const call of [
    (c: WorkerClient) => c.submitPlanCrossCheck("run", 3, candidate),
    (c: WorkerClient) => c.planCrossCheckStatus("run", 3),
  ]) {
    for (const value of malformed) {
      t.mock.method(globalThis, "fetch", async () => Response.json(value));
      await assert.rejects(call(client()), /invalid.*cross-check/);
    }
    t.mock.method(globalThis, "fetch", async () => Response.json({ ...wire(),
      candidate: { ...candidate, required_capabilities: null, required_tools: null },
      findings: { summary: "", items: null } }));
    const decoded = await call(client());
    assert.equal(decoded.result, "candidate");
    if (decoded.result !== "candidate") throw new Error("missing candidate");
    assert.deepEqual(decoded.candidate.required_capabilities, []);
    assert.deepEqual(decoded.candidate.required_tools, []);
    assert.deepEqual(decoded.findings?.items, []);
  }
});

it("strict checked state rejects skew once with its exact generation and presentation identity", async (t) => {
  const c = client();
  c.protocolFeatures = ["gate_revision_v1", "claim_generation_fence"];
  const body = { status: "awaiting_approval" as const, claim_generation: 3,
    presentation_id: "8b0c1a2e-3f4d-4e5f-8a6b-7c8d9e0f1a2b", adopt_gate_revision: 2,
    plan_cross_check_gate_reason: "interrupted" as const };
  const sent: unknown[] = [];
  const fetch = t.mock.method(globalThis, "fetch", async (_url: unknown, init?: RequestInit) => {
    sent.push(JSON.parse(init!.body as string));
    return Response.json({ error: "invalid request body" }, { status: 400 });
  });
  await assert.rejects(c.reportPlanCrossCheckGateState("run", body), (err: unknown) => {
    assert.ok(err instanceof RequestError);
    assert.equal(isStrictDecodeError(err), true, "the response exercises the exact downgrade discriminator");
    return true;
  });
  assert.equal(fetch.mock.callCount(), 1);
  assert.deepEqual(sent, [body]);
});

const ackMaxBytes = 6 * 1024 * 1024;
const checkedState = { status: "awaiting_approval" as const, claim_generation: 3 };

function streamedAck(byteLength: number, contentLength?: string) {
  const prefix = Buffer.from(JSON.stringify({ run: { status: "awaiting_approval" }, gate_revision: 2 }));
  let emitted = 0;
  let cancellations = 0;
  const stream = new ReadableStream<Uint8Array>({
    pull(controller) {
      if (emitted === byteLength) {
        controller.close();
        return;
      }
      const chunk = Buffer.alloc(Math.min(64 * 1024, byteLength - emitted), 32);
      if (emitted === 0) prefix.copy(chunk);
      emitted += chunk.byteLength;
      controller.enqueue(chunk);
    },
    cancel() { cancellations++; },
  }, { highWaterMark: 0 });
  return {
    response: new Response(stream, { headers: contentLength === undefined ? {} : { "Content-Length": contentLength } }),
    cancellations: () => cancellations,
    emitted: () => emitted,
  };
}

it("strict checked state rejects actual streamed ACK overflow and cancels without trusting Content-Length", async (t) => {
  for (const contentLength of [undefined, "64"]) {
    const streamed = streamedAck(8_388_659, contentLength);
    let posts = 0;
    const fetch = t.mock.method(globalThis, "fetch", async (_url: unknown, init?: RequestInit) => {
      assert.equal(init?.method, "POST");
      return ++posts === 1 ? streamed.response : Response.json({ run: { status: "awaiting_approval" }, gate_revision: 2 });
    });
    const c = client();
    c.protocolFeatures = ["gate_revision_v1"];
    await assert.rejects(c.reportPlanCrossCheckGateState("run", checkedState), (err: unknown) => {
      assert.ok(err instanceof Error);
      assert.equal(err.name, "ResponseBodyOverflowError");
      assert.match(err.message, /response body exceeds 6291456 bytes/);
      assert.equal(isTransient(err), false);
      return true;
    });
    assert.equal(streamed.cancellations(), 1);
    assert.equal(streamed.emitted(), ackMaxBytes + 64 * 1024, "stop at the first overflowing chunk");
    assert.equal(fetch.mock.callCount(), 1);
  }
});

it("strict checked state accepts large ACKs through EOF at and below the byte cap", async (t) => {
  for (const byteLength of [ackMaxBytes - 1, ackMaxBytes]) {
    const streamed = streamedAck(byteLength);
    t.mock.method(globalThis, "fetch", async () => streamed.response);
    const c = client();
    c.protocolFeatures = ["gate_revision_v1"];
    const ack = await c.reportPlanCrossCheckGateState("run", checkedState);
    assert.equal(ack.applied, true);
    assert.equal(ack.status, "awaiting_approval");
    assert.equal(ack.gateRevision, 2);
    assert.equal(streamed.emitted(), byteLength);
  }
});

it("strict checked state propagates ACK stream failures while ordinary state retains compatibility", async (t) => {
  const response = () => new Response(new ReadableStream<Uint8Array>({
    pull() { throw new Error("fixture ACK stream failure"); },
  }));
  t.mock.method(globalThis, "fetch", async () => response());
  const c = client();
  c.protocolFeatures = ["gate_revision_v1"];
  await assert.rejects(c.reportPlanCrossCheckGateState("run", checkedState), /fixture ACK stream failure/);
  assert.deepEqual(await c.reportState("run", { status: "running" }), { applied: true, status: undefined });
});

it("strict checked state retries identical requests after transient transport or ACK stream failures", async (t) => {
  for (const failure of ["transport", "stream"]) {
    const sent: unknown[] = [];
    const fetch = t.mock.method(globalThis, "fetch", async (_url: unknown, init?: RequestInit) => {
      assert.equal(init?.method, "POST");
      sent.push(JSON.parse(init!.body as string));
      if (sent.length === 1) {
        if (failure === "transport") throw new TypeError("fixture fetch failure");
        return new Response(new ReadableStream<Uint8Array>({
          pull() { throw new Error("fixture ACK stream failure"); },
        }));
      }
      return Response.json({ run: { status: "awaiting_approval" }, gate_revision: 2 });
    });
    const c = client();
    c.protocolFeatures = ["gate_revision_v1"];
    const ack = await c.reportPlanCrossCheckGateState("run", checkedState);
    assert.equal(ack.applied, true);
    assert.equal(ack.status, "awaiting_approval");
    assert.equal(ack.gateRevision, 2);
    assert.equal(fetch.mock.callCount(), 2);
    assert.deepEqual(sent, [checkedState, checkedState]);
  }
});

it("ordinary state still accepts an ACK larger than the strict byte cap", async (t) => {
  const streamed = streamedAck(ackMaxBytes + 1);
  t.mock.method(globalThis, "fetch", async () => streamed.response);
  const ack = await client().reportState("run", { status: "running" });
  assert.equal(ack.applied, true);
  assert.equal(ack.status, "awaiting_approval");
  assert.equal(streamed.emitted(), ackMaxBytes + 1);
});

it("strict checked state rejects missing negotiation or generation without sending", async (t) => {
  const fetch = t.mock.method(globalThis, "fetch", async () => { throw new Error("must not send"); });
  const c = client();
  await assert.rejects(c.reportPlanCrossCheckGateState("run", { status: "running", claim_generation: 3 }));
  c.protocolFeatures = ["gate_revision_v1"];
  for (const claim_generation of [undefined, 0, -1, 1.5, NaN, Infinity])
    await assert.rejects(c.reportPlanCrossCheckGateState("run", { status: "running", claim_generation }));
  assert.equal(fetch.mock.callCount(), 0);
});
