import { it } from "node:test";
import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import { WorkerClient } from "../src/client.js";
import type { PlanCrossCheckCandidate } from "../src/client.js";
import { nullLogger, makeClaim } from "./helpers.js";
import { decodeCrossCheckClaim } from "../src/code-cross-check-contract.js";
import { crossCheckPrompt } from "../src/codex/cross-check.js";
import { semanticServer } from "./semantic-server-fixture.js";
import { proposal } from "./semantic-coverage-fixture.js";
import { semanticBoundaryFixture } from "./semantic-boundary-fixture.js";
import { parseSemanticCandidate } from "../src/semantic-plan.js";

it("M2 high-density approved fields survive semantic HTTP, canonical decoding, checker response and child claim", async () => {
  const f = semanticBoundaryFixture(true);
  const raw = JSON.stringify(f.compiled.candidate);
  assert.ok(Buffer.byteLength(raw) > 6 * 1024 * 1024);
  assert.deepEqual(parseSemanticCandidate(raw), f.compiled.candidate);
  assert.deepEqual(parseSemanticCandidate(raw.padEnd(16 * 1024 * 1024 + 1, " ")), f.compiled.candidate);
  assert.throws(() => parseSemanticCandidate(raw.padEnd(64 * 1024 * 1024 + 1, " ")), /Invalid wire JSON/);
  const reservation = { ...f.binding, outcome: "reserved", review_id: randomUUID(), invocation_id: randomUUID(),
    model: "sonnet", max_tokens: 40000, deadline: "2030-01-01T00:00:00Z",
    reviews: 1, invocations: 1, charged_tokens: 40000, ...f.compiled, binding: undefined };
  const claim = makeClaim({ kind: "cross_check", cross_check: f.checker });
  // Match Go encoding/json escaping on every server response; no passing prefix.
  const goResponse = (value: unknown, init?: ResponseInit) =>
    new Response(JSON.stringify(value).replace(/</g, "\\u003c"), init);
  assert.ok(Buffer.byteLength(JSON.stringify(f.compiled).replace(/</g, "\\u003c")) > 20 * 1024 * 1024);
  const client = new WorkerClient("http://example.test", "fixture", "test", nullLogger(), {
    fetch: async (input, init) => {
      const path = new URL(String(input)).pathname;
      if (path.endsWith("/candidate")) {
        const body = JSON.parse(String(init?.body));
        assert.deepEqual(body.proposal, f.proposal);
        assert.ok(Buffer.byteLength(String(init?.body)) > 4 * 1024 * 1024);
        return goResponse(f.compiled);
      }
      if (path.endsWith("/reserve")) return goResponse(reservation);
      if (path.endsWith("/claim")) return goResponse(claim, { headers: { "X-Uzi-Claim-Kind": "cross_check" } });
      if (init?.method === "POST") {
        const body = JSON.parse(String(init.body));
        assert.equal(body.semantic_candidate, undefined);
        assert.equal(body.candidate_identity_version, "semantic_v1");
        assert.ok(Buffer.byteLength(String(init.body)) < 8 * 1024 * 1024);
      }
      return goResponse(f.response);
    },
  });
  assert.deepEqual((await client.submitSemanticCandidate("run", 1, f.structural, f.proposal, f.sources)).candidate, f.compiled.candidate);
  assert.deepEqual((await client.reserveSemanticCoverage("run", 1, { ...f.binding, request_id: randomUUID(), supported: true })).candidate, f.compiled.candidate);
  const submitted = { ...f.structural, candidate_identity_version: "semantic_v1" as const, semantic_candidate: f.compiled.candidate };
  const posted = await client.submitPlanCrossCheck("run", 1, submitted);
  assert.equal(posted.result, "candidate");
  if (posted.result === "candidate") assert.deepEqual(posted.candidate, submitted);
  const status = await client.planCrossCheckStatus("run", 1);
  assert.equal(status.result, "candidate");
  if (status.result === "candidate") assert.deepEqual(status.candidate, submitted);
  assert.deepEqual((await client.claimCrossCheck())?.cross_check, f.checker);
});

it("M2 semantic transport rejects above 64 MiB and cannot widen structural replies with a worker flag", async () => {
  const f = semanticBoundaryFixture();
  let body = JSON.stringify({ ...f.response, candidate: f.structural }) + " ".repeat(6 * 1024 * 1024);
  const client = new WorkerClient("http://example.test", "fixture", "test", nullLogger(), {
    fetch: async () => new Response(body),
  });
  await assert.rejects(client.submitPlanCrossCheck("run", 1, { ...f.structural,
    candidate_identity_version: "semantic_v1", semantic_candidate: f.compiled.candidate }), /exceeds 6291456/);
  body = JSON.stringify(f.compiled).padEnd(64 * 1024 * 1024 + 1, " ");
  await assert.rejects(client.submitSemanticCandidate("run", 1, f.structural, f.proposal, f.sources), /exceeds 67108864/);
  await assert.rejects(client.reserveSemanticCoverage("run", 1, { ...f.binding, request_id: randomUUID(), supported: true }), /exceeds 67108864/);
  body = JSON.stringify(f.response).padEnd(64 * 1024 * 1024 + 1, " ");
  await assert.rejects(client.planCrossCheckStatus("run", 1), /exceeds 67108864/);
  await assert.rejects(client.claimCrossCheck(), /exceeds 67108864/);
});

async function fixture() {
  const fake = {} as WorkerClient;
  const server = semanticServer(fake, []);
  const structural: PlanCrossCheckCandidate = { plan_md: "Implement A", milestones: [{ id: "m1", title: "A" }],
    required_capabilities: [], required_tools: [], size_class: "s", base_commit: "a".repeat(40), planning_diff: "" };
  const sources = [{ id: "issue:1", kind: "issue", provenance: "server:1", completeness: "complete",
    bytes: Buffer.from("Implement A").toString("base64") }];
  const compiled = await fake.submitSemanticCandidate("run", 1, structural, proposal(), sources);
  return { server, structural, sources, compiled };
}

it("M2 HTTP client sends authenticated fenced semantic routes and preserves explicit invalid proposals", async () => {
  const f = await fixture();
  const paths: string[] = [];
  const bodies: Record<string, unknown>[] = [];
  const requestId = randomUUID();
  const reviewId = randomUUID(), invocationId = randomUUID();
  const reservation = { ...f.compiled.binding, outcome: "reserved", review_id: reviewId, invocation_id: invocationId,
    model: "sonnet", max_tokens: 40000, deadline: new Date(Date.now() + 300000).toISOString(),
    reviews: 1, invocations: 1, charged_tokens: 40000, candidate: f.compiled.candidate,
    sources: f.sources, plan: Buffer.from("Implement A").toString("base64") };
  const client = new WorkerClient("http://example.test", "fixture-token", "test", nullLogger(), {
    fetch: async (input, init) => {
      const path = new URL(String(input)).pathname; paths.push(path);
      assert.equal(new Headers(init?.headers).get("Authorization"), "Bearer fixture-token");
      const body = init?.body ? JSON.parse(String(init.body)) : undefined;
      if (body) { bodies.push(body); assert.equal(body.claim_generation, 7); }
      const value = path.endsWith("/candidate") ? f.compiled :
        path.endsWith("/reserve") ? reservation : f.server.status();
      return new Response(JSON.stringify(value));
    },
  });
  await client.submitSemanticCandidate("run", 7, f.structural, { invalid: true }, f.sources);
  await client.semanticCoverageStatus("run", 7);
  const reserveBody = { ...f.compiled.binding, request_id: requestId, supported: true };
  await client.reserveSemanticCoverage("run", 7, reserveBody);
  await client.reserveSemanticCoverage("run", 7, reserveBody); // caller retry retains exact request UUID
  await client.settleSemanticCoverage("run", 7, { review_id: reviewId, invocation_id: invocationId, status: "schema_error", tokens: 110 });
  await client.publishSemanticCoverage("run", 7, { review_id: reviewId, invocation_id: invocationId, report: f.server.report() });
  assert.deepEqual(bodies[0]!.proposal, { invalid: true });
  assert.equal(bodies[1]!.request_id, requestId);
  assert.equal(bodies[2]!.request_id, requestId);
  assert.deepEqual(paths, ["candidate", "coverage", "reserve", "reserve", "settle", "publish"].map(s =>
    "/api/worker/runs/run/semantic/" + (s === "coverage" || s === "candidate" ? s : "coverage/" + s)));
});

it("M2 HTTP client rejects unknown fields, malformed reservations and oversized bodies", async () => {
  const f = await fixture();
  let response: unknown = { ...f.server.status(), authority: true };
  let oversized = false;
  let sends = 0;
  const client = new WorkerClient("http://example.test", "fixture", "test", nullLogger(), {
    fetch: async () => { sends++; return new Response(oversized ? "x".repeat(128 * 1024 + 1) : JSON.stringify(response)); },
  });
  await assert.rejects(client.semanticCoverageStatus("run", 7));
  response = { ...f.server.status(), outcome: "reserved", max_tokens: 40000 };
  await assert.rejects(client.reserveSemanticCoverage("run", 7, { ...f.compiled.binding, request_id: randomUUID(), supported: true }));
  oversized = true;
  await assert.rejects(client.semanticCoverageStatus("run", 7));
  await assert.rejects(client.semanticCoverageStatus("run", 0), /generation/);
  assert.equal(sends, 3);
});

it("M2 API semantic checker shape retains canonical criteria and sources without structural fallback", async () => {
  const f = await fixture();
  // Baseline of PlanCrossCheckCandidate's embedded JSON in ClaimPlanCrossCheck.
  const candidate = { ...f.structural, candidate_identity_version: "semantic_v1" as const,
    semantic_candidate: f.compiled.candidate };
  const checker = { ...candidate, stage: "plan" as const, lead_run_id: "lead", round: 1,
    candidate_digest: f.compiled.candidate.digest, deadline_at: "2030-01-01T00:00:00Z" };
  assert.deepEqual(decodeCrossCheckClaim(checker), checker);
  const prompt = crossCheckPrompt(makeClaim({ kind: "cross_check", cross_check: {
    ...checker, milestones: [{ id: "m1", title: "A" }],
  } }));
  assert.ok(prompt.includes(JSON.stringify(f.compiled.candidate)));
  for (const change of [
    { candidate_identity_version: undefined }, { semantic_candidate: undefined },
    { candidate_identity_version: "structural" }, { candidate_digest: "0".repeat(64) },
    { semantic_candidate: { ...f.compiled.candidate, unknown: true } },
    { semantic_candidate: { ...f.compiled.candidate, criteria: [] } },
  ]) assert.throws(() => decodeCrossCheckClaim({ ...checker, ...change }), /invalid cross-check claim/);
  const client = new WorkerClient("http://example.test", "fixture", "test", nullLogger(), {
    fetch: async () => Response.json({ ...f.server.status(), outcome: "incomplete" }),
  });
  assert.equal((await client.semanticCoverageStatus("run", 1)).outcome, "incomplete");
});

it("M2 HTTP client bounds a response body that never completes and preserves owner cancellation", async () => {
  let cancelled = 0;
  const client = new WorkerClient("http://example.test", "fixture", "test", nullLogger(), {
    httpTimeoutMs: 20, fetch: async () => new Response(new ReadableStream({ cancel() { cancelled++; } })),
  });
  await assert.rejects(client.semanticCoverageStatus("run", 1));
  const owner = new AbortController(); owner.abort();
  await assert.rejects(client.semanticCoverageStatus("run", 1, owner.signal));
  assert.equal(cancelled, 1);
});
