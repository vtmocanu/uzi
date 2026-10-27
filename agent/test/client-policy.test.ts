import { it } from "node:test";
import assert from "node:assert/strict";
import { WorkerClient, PolicyHoldUnsupported } from "../src/client.js";
import type { PolicyHoldRequest } from "../src/protocol.js";
import { nullLogger } from "./helpers.js";

const body: PolicyHoldRequest = { outcome_id: "11111111-1111-4111-8111-111111111111",
  source_generation: 1, claim_generation: 2, detail: "resume_rejected", head: "", published: false };
const features = ["provider_policy_hold", "claim_generation_fence", "active_run_snapshot"];
const caps = ["provider_policy_hold_v1", "credential_switch_v1"];
const ack = (extra = {}) => ({ disposition: "applied", outcome_id: body.outcome_id,
  claim_generation: 2, status: "paused", hold_reason: "provider_policy_refusal", ...extra });
async function setup(response: () => Response | Promise<Response>, capabilities = caps, advertised = features, timeout = 100, workerId?: string) {
  const requests: { url: string; init: RequestInit }[] = [];
  const client = new WorkerClient("http://api", "token", "test", nullLogger(), {
    httpTimeoutMs: timeout,
    fetch: async (url, init) => {
      if (String(url).endsWith("/register")) return Response.json({ protocol_features: advertised, ...(workerId === undefined ? {} : { worker_id: workerId }) });
      requests.push({ url: String(url), init: init! });
      return response();
    },
  });
  await client.register("test", undefined, 1, undefined, capabilities);
  return { client, requests };
}

it("policy negotiation requires original capabilities and all trusted feature prerequisites", async () => {
  for (const missing of features) {
    const { client } = await setup(() => Response.json(ack()), caps, features.filter(f => f !== missing));
    assert.equal(client.supportsPolicyHold(), false);
  }
  for (const missing of caps) {
    const { client } = await setup(() => Response.json(ack()), caps.filter(c => c !== missing));
    assert.equal(client.supportsPolicyHold(), false);
  }
  const { client } = await setup(() => Response.json(ack()));
  assert.equal(client.supportsPolicyHold(), true);
  client.clearFeatures();
  assert.equal(client.supportsPolicyHold(), false);
});

it("policy sends exact body once and decodes flat ACKs including later owner execution and pending 409", async () => {
  for (const [extra, status] of [
    [{}, 200], [{ disposition: "already_applied", status: "running", hold_reason: undefined }, 200],
    [{ disposition: "resolved_newer_policy", status: "queued" }, 200],
    [{ disposition: "terminal", status: "cancelled" }, 200], [{ disposition: "pending", status: "claimed" }, 409],
    [{ disposition: "recorded", status: "running" }, 200],
    [{ disposition: "unauthenticated_historical_outcome", status: "queued" }, 409],
    [{ disposition: "unauthenticated_historical_outcome", status: "cancelled" }, 409],
  ] as const) {
    const { client, requests } = await setup(() => Response.json(ack(extra), { status }));
    assert.equal((await client.reportPolicyHold("r/1", body)).disposition, extra.disposition ?? "applied");
    assert.equal(requests.length, 1);
    assert.equal(requests[0]!.url, "http://api/api/worker/runs/r%2F1/policy-hold");
    assert.equal(requests[0]!.init.method, "POST");
    assert.deepEqual(JSON.parse(String(requests[0]!.init.body)), body);
  }
});

it("malformed, wrong identity/generation/status, oversize and transport errors have no authority or raw error content", async () => {
  for (const value of [null, [], { run: ack() }, ack({ disposition: "superseded" }), ack({ outcome_id: "other" }),
    ack({ claim_generation: 1 }), ack({ claim_generation: "2" }), ack({ status: "unknown" }),
    ack({ status: "running" }), ack({ disposition: "terminal", status: "paused" }),
    ack({ disposition: "recorded", status: "completed" }),
    ack({ disposition: "resolved_newer_policy", status: "failed" }), ack({ hold_reason: null }), ack({ unexpected: "must reject" })]) {
    const { client, requests } = await setup(() => Response.json(value));
    await assert.rejects(client.reportPolicyHold("r", body), { message: "policy hold request failed" });
    assert.equal(requests.length, 1);
  }
  for (const response of [() => new Response("sensitive".repeat(1000)), () => { throw new Error("sensitive"); },
    () => new Response("sensitive", { status: 503 })]) {
    const { client } = await setup(response);
    await assert.rejects(client.reportPolicyHold("r", body), { message: "policy hold request failed" });
  }
});

it("only a definitive missing route is unsupported; run-not-owned JSON remains pending error", async () => {
  for (const response of [new Response("404 page not found\n", { status: 404 }), new Response(null, { status: 405 })]) {
    const { client } = await setup(() => response);
    await assert.rejects(client.reportPolicyHold("r", body), PolicyHoldUnsupported);
  }
  const { client } = await setup(() => Response.json({ error: "run not found for this worker" }, { status: 404 }));
  await assert.rejects(client.reportPolicyHold("r", body), e => !(e instanceof PolicyHoldUnsupported));
});

it("durable replay still attempts its fenced endpoint after a feature-cache rollback", async () => {
  const { client, requests } = await setup(() => Response.json(ack()));
  client.clearFeatures();
  assert.equal(client.supportsPolicyHold(), false);
  assert.equal((await client.reportPolicyHold("r", body)).disposition, "applied");
  assert.equal(requests.length, 1);
});

it("deadline bounds a stalled response body and caller cancellation propagates", async () => {
  const { client } = await setup(() => new Response(new ReadableStream({ start(c) { c.enqueue(new TextEncoder().encode("{")); } })), caps, features, 20);
  await assert.rejects(client.reportPolicyHold("r", body));
  const controller = new AbortController(); controller.abort(new Error("cancelled"));
  await assert.rejects(client.reportPolicyHold("r", body, controller.signal), { message: "cancelled" });
});

const revisionRun = "12345678-1234-5678-9abc-123456789abc";
const revisionWorker = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee";
const revisionGate = { revision: "7", presentation_id: revisionRun, payload_digest: "a".repeat(64) };
const revisionClaim = { run_id: revisionRun, claim_generation: 2, kind: "issue", issue_iid: 1,
  interactive: false, secrets: { codex: {} } };
const revisionRunDTO = { id: revisionRun, worker_id: revisionWorker, status: "running" };
const reconciliation = { claim_generation: 2, lead_last_seq: 0, plan_cross_check_settled: true,
  gate_revision: 0, current_plan_sha256: "b".repeat(64) };

it("claim gate decoding preserves unknown/null/object, closes malformed values and copies primitives", async () => {
  for (const value of [undefined, null, revisionGate, {}, { ...revisionGate, extra: true },
    { ...revisionGate, revision: 7 }, { ...revisionGate, payload_digest: "A".repeat(64) }]) {
    const wire = { ...revisionClaim, ...(value === undefined ? {} : { policy_revision_gate: value }) };
    const { client } = await setup(() => Response.json(wire), caps, features, 100, revisionWorker);
    const claim = await client.claimRun();
    assert.deepEqual(claim?.policy_revision_gate, value === null || value === revisionGate ? value : undefined);
    if (value === revisionGate) {
      const snapshot = claim!.policy_revision_gate!;
      wire.policy_revision_gate = null;
      assert.deepEqual(snapshot, revisionGate);
      assert.notEqual(snapshot, revisionGate);
    }
  }
  for (const extra of [{ claim_generation: 0 }, { claim_generation: "2" }, { run_id: "wrong" },
    { interactive: true }, { kind: "review" }, { target_run_id: revisionWorker }, { secrets: {} }, { kind: undefined }]) {
    const wire = { ...revisionClaim, ...extra, policy_revision_gate: null };
    const { client } = await setup(() => Response.json(wire), caps, features, 100, revisionWorker);
    assert.equal((await client.claimRun())?.policy_revision_gate, undefined);
  }
  const { client } = await setup(() => Response.json({ ...revisionClaim, policy_revision_gate: null }), caps, []);
  assert.equal((await client.claimRun())?.policy_revision_gate, undefined);
});

it("actual state transport decodes the same top-level gate in plain and reconciled ACKs with atomic receipts", async () => {
  for (const envelope of [{}, { gate_revision: 7 }, reconciliation]) for (const gate of [undefined, null, revisionGate,
    { ...revisionGate, extra: true }, { ...revisionGate, revision: "01" }]) {
    const wire = { run: revisionRunDTO, ...envelope,
      ...(gate === undefined ? {} : { policy_revision_gate: gate }),
      revision_input_id: 3, superseded_revision_input_ids: [1, 2] };
    const { client, requests } = await setup(() => Response.json(wire), caps, features, 100, revisionWorker);
    const ack = await client.reportState(revisionRun, { status: "running", claim_generation: 2 });
    assert.deepEqual(ack.policyRevisionGate, gate === null || gate === revisionGate ? gate : undefined);
    assert.equal(ack.sentClaimGeneration, 2);
    assert.equal(ack.workerIdentityMatches, true);
    assert.equal(ack.revisionInputId, 3);
    assert.deepEqual(ack.supersededRevisionInputIds, [1, 2]);
    assert.equal(requests.length, 1);
    if (gate === revisionGate) {
      assert.notEqual(ack.policyRevisionGate, gate);
      wire.policy_revision_gate = null;
      assert.deepEqual(ack.policyRevisionGate, revisionGate);
    }
  }
  for (const ids of [[1, 1], [1, Number.MAX_SAFE_INTEGER + 1], [3], null]) {
    const { client } = await setup(() => Response.json({ run: revisionRunDTO, policy_revision_gate: null,
      revision_input_id: 3, superseded_revision_input_ids: ids }), caps, features, 100, revisionWorker);
    const ack = await client.reportState(revisionRun, { status: "running", claim_generation: 2 });
    assert.equal(ack.policyRevisionGate, null);
    assert.equal(ack.revisionInputId, undefined);
    assert.equal(ack.supersededRevisionInputIds, undefined);
  }
});

it("partial reconciliation never falls back to plain policy gate coverage", async () => {
  const response = async (proof: Record<string, unknown>) => {
    const { client } = await setup(() => Response.json({ run: revisionRunDTO,
      policy_revision_gate: null, ...proof }), caps, features, 100, revisionWorker);
    return client.reportState(revisionRun, { status: "running", claim_generation: 2 });
  };
  for (const missing of Object.keys(reconciliation)) {
    const proof: Record<string, unknown> = { ...reconciliation };
    delete proof[missing];
    assert.equal((await response(proof)).policyRevisionGate, undefined, missing);
  }
  // Each proof-specific marker, even malformed/null, prevents plain-ACK fallback.
  // gate_revision alone belongs to both encodings and has positive controls above.
  for (const key of ["claim_generation", "lead_last_seq", "plan_cross_check_settled",
    "gate_presentation_id", "gate_payload_digest", "current_plan_sha256"]) {
    for (const value of [null, 0, "malformed"])
      assert.equal((await response({ [key]: value })).policyRevisionGate, undefined, key);
  }
});

it("state gate never escapes declined/stale/wrong identity/generation or unnegotiated transport", async () => {
  const cases = [
    { status: 409 }, { extra: { applied: false } }, { extra: { disposition: "stale_claim" } },
    { extra: { disposition: "released" } }, { run: { ...revisionRunDTO, id: revisionWorker } },
    { run: { ...revisionRunDTO, worker_id: revisionRun } }, { run: { id: revisionRun, status: "running" } },
    { generation: 0 }, { generation: undefined },
    { extra: { ...reconciliation, claim_generation: 1 } },
    { extra: { claim_generation: "2" } }, { advertised: [] }, { capabilities: [] },
    { registered: false }, { extra: { run: { ...revisionRunDTO, policy_revision_gate: null }, policy_revision_gate: undefined } },
  ];
  for (const fixture of cases) {
    const wire = { run: revisionRunDTO, policy_revision_gate: null, ...fixture.extra,
      ...(fixture.run ? { run: fixture.run } : {}) };
    const { client } = await setup(() => Response.json(wire, { status: fixture.status ?? 200 }),
      fixture.capabilities ?? caps, fixture.advertised ?? features, 100,
      fixture.registered === false ? undefined : revisionWorker);
    const ack = await client.reportState(revisionRun, { status: "running",
      claim_generation: Object.hasOwn(fixture, "generation") ? fixture.generation : 2 });
    assert.equal(ack.policyRevisionGate, undefined, JSON.stringify(fixture));
  }
});

it("strict-decode generation stripping and negotiation loss cannot expose a policy revision gate", async () => {
  let attempts = 0;
  const { client, requests } = await setup(() => ++attempts === 1
    ? Response.json({ error: "invalid request body" }, { status: 400 })
    : Response.json({ run: revisionRunDTO, policy_revision_gate: null }), caps, features, 100, revisionWorker);
  const ack = await client.reportState(revisionRun, { status: "running", claim_generation: 2 });
  assert.equal(requests.length, 2);
  assert.equal(JSON.parse(String(requests[1]!.init.body)).claim_generation, undefined);
  assert.equal(ack.sentClaimGeneration, undefined);
  assert.equal(ack.policyRevisionGate, undefined);

  const live = await setup(() => {
    live.client.clearFeatures();
    return Response.json({ run: revisionRunDTO, policy_revision_gate: null });
  }, caps, features, 100, revisionWorker);
  assert.equal((await live.client.reportState(revisionRun, { status: "running", claim_generation: 2 })).policyRevisionGate, undefined);
});
