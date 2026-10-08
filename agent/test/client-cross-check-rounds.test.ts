import assert from "node:assert/strict";
import { it } from "node:test";
import { WorkerClient, type PlanCrossCheckCandidate } from "../src/client.js";
import { nullLogger } from "./helpers.js";

const candidate: PlanCrossCheckCandidate = { plan_md: "plan", planning_diff: "", milestones: [],
  required_capabilities: [], required_tools: [], size_class: "s", base_commit: "a".repeat(40) };
const client = () => new WorkerClient("http://example.com", "fixture-join", "test", nullLogger());
const wire = (round = 2) => ({ result: "candidate", round, automatic_revision_limit: 4, automatic_rounds_enabled: true,
  checker_run_id: null, candidate_digest: "b".repeat(64), candidate_generation: 3, candidate,
  verdict: "pending", reason_class: "", findings: null, deadline_at: "2030-01-01T00:00:00Z", lead_last_seq: 4 });
const metadata = () => ({ result: "latest", round: 2, candidate_generation: 2,
  automatic_revision_limit: 4, automatic_rounds_enabled: true, next_round: 3,
  next_round_eligible: true, fallback_reason: "" });
const empty = () => ({ result: "no_row", round: 0, candidate_generation: 0,
  automatic_revision_limit: 0, automatic_rounds_enabled: false, next_round: null,
  next_round_eligible: false, fallback_reason: "" });

it("submit sends explicit round with signal fourth; status requires the requested candidate round", async (t) => {
  const requests: { url: string; init?: RequestInit }[] = [];
  let response: unknown = wire();
  t.mock.method(globalThis, "fetch", async (url: unknown, init?: RequestInit) => {
    requests.push({ url: String(url), init });
    return Response.json(response);
  });
  const c = client(), signal = new AbortController().signal;
  assert.equal((await c.submitPlanCrossCheck("run", 3, candidate, signal, 2)).result, "candidate");
  assert.deepEqual(JSON.parse(requests[0]!.init!.body as string), { ...candidate, stage: "plan", claim_generation: 3, round: 2 });
  assert.ok(requests[0]!.init!.signal);
  assert.equal((await c.planCrossCheckStatus("run", 3, 2, signal)).result, "candidate");
  assert.equal(requests[1]!.url, "http://example.com/api/worker/runs/run/cross-checks/plan/2?claim_generation=3");
  response = wire(1);
  await c.submitPlanCrossCheck("run", 3, candidate, signal);
  assert.equal(JSON.parse(requests[2]!.init!.body as string).round, 1);
  await assert.rejects(c.planCrossCheckStatus("run", 3, 2), /invalid.*cross-check/);
  response = wire(5);
  assert.equal((await c.planCrossCheckStatus("run", 3, 5)).result, "candidate");
  for (const round of [0, 6, 1.5, NaN]) {
    const before = requests.length;
    await assert.rejects(c.planCrossCheckStatus("run", 3, round), /invalid.*round/);
    await assert.rejects(c.submitPlanCrossCheck("run", 3, candidate, signal, round), /invalid.*round/);
    assert.equal(requests.length, before);
  }
});

it("candidate snapshot is bounded and legacy round one has no automatic provenance", async (t) => {
  const legacy = wire(1) as Record<string, unknown>;
  delete legacy.automatic_revision_limit;
  delete legacy.automatic_rounds_enabled;
  t.mock.method(globalThis, "fetch", async () => Response.json(legacy));
  const decoded = await client().planCrossCheckStatus("run", 3);
  assert.equal(decoded.result, "candidate");
  if (decoded.result !== "candidate") throw new Error("missing candidate");
  assert.equal(decoded.automatic_rounds_enabled, false);
  assert.equal(decoded.automatic_revision_limit, 0);
  for (const value of [
    { ...legacy, round: 2 }, { ...legacy, automatic_rounds_enabled: true },
    { ...legacy, automatic_revision_limit: 0 }, { ...wire(), automatic_revision_limit: -1 },
    { ...wire(), automatic_revision_limit: 5 }, { ...wire(), automatic_revision_limit: 1.5 },
    { ...wire(), automatic_revision_limit: "4" }, { ...wire(), automatic_rounds_enabled: 1 },
    { ...wire(), automatic_rounds_enabled: false }, { ...wire(), automatic_revision_limit: 0 },
  ]) {
    t.mock.method(globalThis, "fetch", async () => Response.json(value));
    await assert.rejects(client().planCrossCheckStatus("run", 3, (value as Record<string, unknown>).round as number), /invalid.*cross-check/);
  }
  t.mock.method(globalThis, "fetch", async () => Response.json({ ...wire(), verdict: "failed", reason_class: "approved_not_stored" }));
  assert.equal((await client().planCrossCheckStatus("run", 3, 2)).result, "candidate");
  for (const reason_class of ["revisions_exhausted", "approve", "unknown"]) {
    t.mock.method(globalThis, "fetch", async () => Response.json({ ...wire(), verdict: "failed", reason_class }));
    await assert.rejects(client().planCrossCheckStatus("run", 3, 2), /invalid.*cross-check/);
  }
});

it("latest is authenticated, preserves historical generation and exposes recommendations only", async (t) => {
  let response: unknown = { ...metadata(), candidate, planning_diff: "secret", findings: {}, reconciliation: { claim_generation: 3 }, verdict: "approve" };
  t.mock.method(globalThis, "fetch", async (url: unknown, init?: RequestInit) => {
    assert.equal(String(url), "http://example.com/api/worker/runs/run/cross-checks/plan/latest?claim_generation=3");
    assert.equal(new Headers(init?.headers).get("Authorization"), "Bearer fixture-join");
    assert.ok(init?.signal);
    return Response.json(response);
  });
  const c = client(), signal = new AbortController().signal;
  assert.deepEqual(await c.planCrossCheckLatest("run", 3, signal), metadata());
  for (const value of [empty(), { ...empty(), next_round: 1, next_round_eligible: true },
    { ...metadata(), round: 5, next_round: null, next_round_eligible: false, fallback_reason: "revisions_exhausted" },
    ...["", "approve", "interrupted", "approved_not_stored", "timed_out", "revise", "block"].map((fallback_reason) =>
      ({ ...metadata(), next_round: null, next_round_eligible: false, fallback_reason }))]) {
    response = value;
    assert.deepEqual(await c.planCrossCheckLatest("run", 3, signal), value);
  }
});

it("latest fails closed on malformed snapshots, identities and contradictory recommendations", async (t) => {
  const missing = metadata() as Record<string, unknown>;
  delete missing.automatic_revision_limit;
  for (const value of [null, {}, missing,
    { ...metadata(), result: "candidate" }, { ...metadata(), round: 0 }, { ...metadata(), round: 6 },
    { ...metadata(), candidate_generation: 0 }, { ...metadata(), candidate_generation: 1.5 },
    { ...metadata(), candidate_generation: Number.MAX_SAFE_INTEGER + 1 },
    { ...metadata(), automatic_revision_limit: 5 }, { ...metadata(), automatic_revision_limit: 1 },
    { ...metadata(), automatic_rounds_enabled: false }, { ...metadata(), automatic_rounds_enabled: "true" },
    { ...metadata(), next_round: 4 }, { ...metadata(), next_round: 2 },
    { ...metadata(), next_round: null }, { ...metadata(), next_round_eligible: false },
    { ...metadata(), next_round_eligible: 1 }, { ...metadata(), next_round: 6 },
    { ...metadata(), fallback_reason: "approve" }, { ...metadata(), fallback_reason: "unknown" },
    { ...metadata(), next_round: null, next_round_eligible: false, fallback_reason: "revisions_exhausted" },
    { ...empty(), round: 1 }, { ...empty(), candidate_generation: 1 },
    { ...empty(), automatic_revision_limit: 1 }, { ...empty(), automatic_rounds_enabled: true },
    { ...empty(), fallback_reason: "approve" }, { ...empty(), next_round: 2, next_round_eligible: true }]) {
    t.mock.method(globalThis, "fetch", async () => Response.json(value));
    await assert.rejects(client().planCrossCheckLatest("run", 3), /invalid cross-check latest metadata/);
  }
});
