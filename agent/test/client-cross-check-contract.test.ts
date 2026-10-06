import assert from "node:assert/strict";
import { it } from "node:test";
import { WorkerClient, RequestError, isStrictDecodeError, type PlanCrossCheckCandidate } from "../src/client.js";
import { nullLogger } from "./helpers.js";

const candidate: PlanCrossCheckCandidate = { plan_md: "plan", planning_diff: "", milestones: [],
  required_capabilities: [], required_tools: [], size_class: "s", base_commit: "a".repeat(40) };
const wire = () => ({ result: "candidate", round: 1, checker_run_id: null,
  candidate_digest: "b".repeat(64), candidate_generation: 3, candidate,
  verdict: "approve", reason_class: "approve", findings: null,
  deadline_at: "2030-01-01T00:00:00Z", lead_last_seq: 4 });
const client = () => new WorkerClient("http://example.com", "fixture-join", "test", nullLogger(),
  { terminalRetrySchedule: [] });

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

it("strict checked state rejects missing negotiation or generation without sending", async (t) => {
  const fetch = t.mock.method(globalThis, "fetch", async () => { throw new Error("must not send"); });
  const c = client();
  await assert.rejects(c.reportPlanCrossCheckGateState("run", { status: "running", claim_generation: 3 }));
  c.protocolFeatures = ["gate_revision_v1"];
  for (const claim_generation of [undefined, 0, -1, 1.5, NaN, Infinity])
    await assert.rejects(c.reportPlanCrossCheckGateState("run", { status: "running", claim_generation }));
  assert.equal(fetch.mock.callCount(), 0);
});
