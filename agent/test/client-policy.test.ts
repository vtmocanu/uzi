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
async function setup(response: () => Response | Promise<Response>, capabilities = caps, advertised = features, timeout = 100) {
  const requests: { url: string; init: RequestInit }[] = [];
  const client = new WorkerClient("http://api", "token", "test", nullLogger(), {
    httpTimeoutMs: timeout,
    fetch: async (url, init) => {
      if (String(url).endsWith("/register")) return Response.json({ protocol_features: advertised });
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
