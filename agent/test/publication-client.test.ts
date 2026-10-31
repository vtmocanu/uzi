import { describe, it } from "node:test";
import assert from "node:assert/strict";

import { WorkerClient } from "../src/client.js";
import type { StateRequest } from "../src/protocol.js";
import { publicationParkRefusal } from "../src/publication-intent.js";
import { intent, RUN } from "./publication-intent-fixture.js";
import { makeClaim, nullLogger } from "./helpers.js";

// Issue #1324: the client wire shapes. There is no shared wire fixture for these types, so each
// shape below is compared by hand with the Go json tags: api/internal/workersvc/publication_resume.go
// (PublicationIntentRequest / PublicationIntentResponse), publication_park.go (validatePublicationPark),
// forge_origin_success.go (ForgeCloneSuccessRequest) and apitypes/publication.go.

interface Sent { url: string; body: Record<string, unknown> }

function clientWith(respond: (sent: Sent) => Response | Promise<Response>, features: string[]) {
  const sent: Sent[] = [];
  const client = new WorkerClient("https://api.example", "fixture-worker-token", "test", nullLogger(), {
    fetch: async (url, init) => {
      const record = { url: String(url), body: JSON.parse(String(init?.body ?? "{}")) as Record<string, unknown> };
      sent.push(record);
      return respond(record);
    },
  });
  client.protocolFeatures = features;
  return { client, sent };
}

const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const request = () => ({ claim_generation: 4, intent: intent() });

describe("client.registerPublicationIntent", () => {
  it("sends nothing without the negotiated feature", async () => {
    const { client, sent } = clientWith(() => json(200, {}), []);
    assert.deepEqual(await client.registerPublicationIntent(RUN, request()), { kind: "unsupported" });
    assert.equal(sent.length, 0);
  });

  it("gates only a NEW enrollment on the feature: an update for an existing enrollment is sent regardless", async () => {
    const { client, sent } = clientWith(() => json(200, { enrollment_seq: 5, candidate_seq: 3, state: "active" }), []);
    assert.deepEqual(await client.registerPublicationIntent(RUN, request()), { kind: "unsupported" });
    assert.equal(sent.length, 0);
    const result = await client.registerPublicationIntent(RUN, { claim_generation: 6, enrollment_seq: 5, candidate_seq: 3, intent: intent() });
    assert.equal(result.kind, "acked");
    assert.equal(sent.length, 1);
  });

  it("POSTs {claim_generation, enrollment_seq?, candidate_seq?, intent} and reads the ack", async () => {
    const { client, sent } = clientWith(() => json(200, { enrollment_seq: 5, candidate_seq: 2, state: "active" }), ["publication_resume_v1"]);
    const result = await client.registerPublicationIntent(RUN, { claim_generation: 4, enrollment_seq: 5, candidate_seq: 2, intent: intent() });
    assert.deepEqual(result, { kind: "acked", enrollment_seq: 5, candidate_seq: 2, state: "active" });
    assert.equal(sent[0]!.url, `https://api.example/api/worker/runs/${RUN}/publication-intent`);
    assert.deepEqual(Object.keys(sent[0]!.body).sort(), ["candidate_seq", "claim_generation", "enrollment_seq", "intent"]);
    assert.deepEqual(sent[0]!.body.intent, JSON.parse(JSON.stringify(intent())));
  });

  it("reads each definite refusal with its machine-readable reason", async () => {
    const cases: [number, unknown, { status: number; reason: string }][] = [
      [409, { error: "publication_intent_mismatch", reason: "publication_intent_mismatch" }, { status: 409, reason: "publication_intent_mismatch" }],
      [409, { error: "publication_enrollment_retired", reason: "publication_enrollment_retired" }, { status: 409, reason: "publication_enrollment_retired" }],
      [409, { disposition: "stale_claim" }, { status: 409, reason: "stale_claim" }],
      [400, { error: "publication_resume_unsupported", reason: "publication_resume_unsupported" }, { status: 400, reason: "publication_resume_unsupported" }],
      [400, { error: "publication_kind_unsupported", reason: "publication_kind_unsupported" }, { status: 400, reason: "publication_kind_unsupported" }],
      [400, { error: "invalid request body" }, { status: 400, reason: "http_400" }],
      [404, { error: "run not owned" }, { status: 404, reason: "http_404" }],
      // The api's disabled answer is httpx.Error with the token as the message and no reason member.
      [503, { error: "publication_resume_disabled" }, { status: 503, reason: "publication_resume_disabled" }],
    ];
    for (const [status, body, want] of cases) {
      const { client } = clientWith(() => json(status, body), ["publication_resume_v1"]);
      assert.deepEqual(await client.registerPublicationIntent(RUN, request()), { kind: "refused", ...want }, `${status} ${JSON.stringify(body)}`);
    }
  });

  it("never reads an ambiguous answer as a definite one", async () => {
    const answers: (() => Response | Promise<Response>)[] = [
      () => json(503, { error: "publication intent unavailable" }),
      () => json(500, { error: "boom" }),
      () => json(429, {}),
      () => json(200, { enrollment_seq: 0, candidate_seq: 1, state: "active" }),
      () => new Response("not json", { status: 200 }),
      () => { throw new Error("socket hang up"); },
    ];
    for (const answer of answers) {
      const { client } = clientWith(answer, ["publication_resume_v1"]);
      assert.equal((await client.registerPublicationIntent(RUN, request())).kind, "unknown");
    }
  });
});

describe("client typed publication park", () => {
  const park = (over: Partial<StateRequest> = {}): StateRequest => ({
    status: "recovery_wait", recovery_cause: "forge_unreachable", recovery_phase: "publication",
    publication_surface: "git", publication_step: "push", claim_generation: 4, ...over,
  });
  const ok = () => json(200, { run: { id: RUN, status: "recovery_wait", recovery_cause: "forge_unreachable", recovery_retry_not_before: "2026-01-01T00:00:00Z" } });

  it("sends the typed fields exactly once", async () => {
    const { client, sent } = clientWith(ok, ["publication_resume_v1"]);
    const ack = await client.reportState(RUN, park({ handoff_checkpoint_tip: "5".repeat(40) }));
    assert.equal(ack.status, "recovery_wait");
    assert.equal(sent.length, 1);
    assert.deepEqual(sent[0]!.body, {
      status: "recovery_wait", recovery_cause: "forge_unreachable", recovery_phase: "publication",
      publication_surface: "git", publication_step: "push", claim_generation: 4, handoff_checkpoint_tip: "5".repeat(40),
    });
  });

  it("sends the typed park whether or not the api advertises the feature", async () => {
    const { client, sent } = clientWith(ok, []);
    const ack = await client.reportState(RUN, park());
    assert.equal(ack.status, "recovery_wait");
    assert.equal(sent.length, 1);
  });

  it("refuses to send an incomplete typed park, rather than degrade to an untyped one", async () => {
    const bad: [Partial<StateRequest>, string[]][] = [
      [{ publication_surface: undefined }, ["publication_resume_v1"]],
      [{ publication_step: "commit" as never }, ["publication_resume_v1"]],
      [{ recovery_cause: "empty_turn" }, ["publication_resume_v1"]],
      [{ claim_generation: undefined }, ["publication_resume_v1"]],
      [{ handoff_checkpoint_tip: "abc" }, ["publication_resume_v1"]],
    ];
    for (const [over, features] of bad) {
      const { client, sent } = clientWith(ok, features);
      await assert.rejects(client.reportState(RUN, park(over)), /publication park requires/);
      assert.equal(sent.length, 0);
    }
  });

  it("hands the refusal reasons to the caller on the ack", async () => {
    for (const reason of ["publication_enrolled", "publication_not_enrolled"] as const) {
      const { client } = clientWith(() => json(409, { run: { id: RUN, status: "running" }, reason }), ["publication_resume_v1"]);
      const ack = await client.reportState(RUN, park());
      assert.equal(ack.applied, false);
      assert.equal(publicationParkRefusal(ack), reason);
    }
  });

  it("does not retry a strict-decode refusal into an untyped park", async () => {
    const { client, sent } = clientWith(() => json(400, { error: "invalid request body" }), ["publication_resume_v1"]);
    await assert.rejects(client.reportState(RUN, park()));
    assert.ok(sent.every((s) => s.body.recovery_phase === "publication"), "every attempt stays typed");
  });
});

describe("client.reportForgeApiSuccess", () => {
  const reservation = { incarnation: "22222222-2222-4222-8222-222222222222", version: 7, observation_version: 3 };
  const claim = (over = {}) => makeClaim({ run_id: "11111111-1111-4111-8111-111111111111", claim_generation: 9, forge_api_origin_reservation: reservation, ...over });
  const settle = () => new Promise((resolve) => setImmediate(resolve));

  it("echoes the API reservation with surface api, and the Git report stays byte-identical", async () => {
    const { client, sent } = clientWith(() => new Response(null, { status: 200 }), ["forge_clone_success_v1"]);
    const flight = new AbortController();
    client.reportForgeApiSuccess(claim(), flight.signal);
    await settle();
    assert.equal(sent[0]!.url, "https://api.example/api/worker/runs/11111111-1111-4111-8111-111111111111/forge-clone-success");
    assert.deepEqual(sent[0]!.body, { claim_generation: 9, ...reservation, surface: "api" });
    client.reportForgeCloneSuccess(claim({ forge_origin_reservation: reservation }), flight.signal);
    await settle();
    assert.deepEqual(sent[1]!.body, { claim_generation: 9, ...reservation }, "no surface member on the Git report");
  });

  it("sends nothing without a reservation, a negotiated feature, or a live flight, and a refusal never closes the Git feature", async () => {
    const none = clientWith(() => new Response(null, { status: 200 }), ["forge_clone_success_v1"]);
    const noToken = claim();
    delete (noToken as { forge_api_origin_reservation?: unknown }).forge_api_origin_reservation;
    none.client.reportForgeApiSuccess(noToken, new AbortController().signal);
    const cancelled = new AbortController();
    cancelled.abort();
    none.client.reportForgeApiSuccess(claim(), cancelled.signal);
    const off = clientWith(() => new Response(null, { status: 200 }), []);
    off.client.reportForgeApiSuccess(claim(), new AbortController().signal);
    await settle();
    assert.deepEqual([none.sent.length, off.sent.length], [0, 0]);

    const refusing = clientWith(() => new Response(null, { status: 400 }), ["forge_clone_success_v1"]);
    refusing.client.reportForgeApiSuccess(claim(), new AbortController().signal);
    await settle();
    assert.equal(refusing.sent.length, 1);
    assert.equal(refusing.client.hasFeature("forge_clone_success_v1"), true);
  });
});
