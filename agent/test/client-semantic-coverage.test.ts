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
  assert.ok(Buffer.byteLength(JSON.stringify(f.compiled).replace(/</g, "\\u003c")) > 32 * 1024 * 1024);
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
      if (path.endsWith("/claim")) return goResponse(claim, { headers: { "X-Uzi-Claim-Kind": "cross_check",
        "X-Uzi-Candidate-Identity-Version": "semantic_v1" } });
      if (init?.method === "POST") {
        const body = JSON.parse(String(init.body));
        assert.equal(body.semantic_candidate, undefined);
        assert.equal(body.candidate_identity_version, "semantic_v1");
        assert.ok(Buffer.byteLength(String(init.body)) < 8 * 1024 * 1024);
      }
      return goResponse(f.response, { headers: { "X-Uzi-Candidate-Identity-Version": "semantic_v1" } });
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

it("M2 high-density validated semantic checker prompt preserves every criterion and source identity", () => {
  const f = semanticBoundaryFixture();
  const checker = decodeCrossCheckClaim(f.checker);
  assert.equal(Buffer.byteLength(JSON.stringify(checker)), 7_954_175);
  const prompt = crossCheckPrompt(makeClaim({ kind: "cross_check", cross_check: checker }));
  assert.ok(prompt.includes(JSON.stringify(f.compiled.candidate)));
  for (const criterion of f.compiled.candidate.criteria) {
    assert.ok(prompt.includes(JSON.stringify(criterion)));
    assert.ok(prompt.includes(criterion.id));
  }
  for (const source of f.compiled.candidate.sources) {
    assert.ok(prompt.includes(JSON.stringify(source)));
    assert.ok(prompt.includes(source.id));
  }
});

it("M2 semantic transport rejects above 64 MiB and cannot widen structural replies with a worker flag", async () => {
  const f = semanticBoundaryFixture();
  let body = JSON.stringify({ ...f.response, candidate: f.structural }) + " ".repeat(6 * 1024 * 1024);
  let marked = false;
  const client = new WorkerClient("http://example.test", "fixture", "test", nullLogger(), {
    fetch: async () => new Response(body, { headers: marked ? {
      "X-Uzi-Candidate-Identity-Version": "semantic_v1", "X-Uzi-Claim-Kind": "cross_check",
    } : {} }),
  });
  await assert.rejects(client.submitPlanCrossCheck("run", 1, { ...f.structural,
    candidate_identity_version: "semantic_v1", semantic_candidate: f.compiled.candidate }), /exceeds 6291456/);
  body = JSON.stringify(f.compiled).padEnd(64 * 1024 * 1024 + 1, " ");
  await assert.rejects(client.submitSemanticCandidate("run", 1, f.structural, f.proposal, f.sources), /exceeds 67108864/);
  await assert.rejects(client.reserveSemanticCoverage("run", 1, { ...f.binding, request_id: randomUUID(), supported: true }), /exceeds 67108864/);
  marked = true;
  body = JSON.stringify(f.response).padEnd(64 * 1024 * 1024 + 1, " ");
  await assert.rejects(client.planCrossCheckStatus("run", 1), /exceeds 67108864/);
  await assert.rejects(client.claimCrossCheck(), /exceeds 67108864/);
});

it("M2 semantic markers reject unknown versions and ineligible endpoints before consuming bodies", async () => {
  for (const marker of ["future", "semantic_v1"]) {
    for (const bytes of [1, 7 * 1024 * 1024]) {
      let pulls = 0, cancelled = 0;
      const client = new WorkerClient("http://example.test", "fixture", "test", nullLogger(), {
        fetch: async () => new Response(new ReadableStream<Uint8Array>({
          pull(controller) { pulls++; controller.enqueue(new Uint8Array(bytes)); },
          cancel() { cancelled++; },
        }, { highWaterMark: 0 }), { headers: { "X-Uzi-Candidate-Identity-Version": marker } }),
      });
      await assert.rejects(client.codeCrossCheckStatus("run", 1), /identity marker/);
      assert.equal(pulls, 0);
      assert.equal(cancelled, 1);
      if (marker === "future") {
        await assert.rejects(client.planCrossCheckStatus("run", 1), /identity marker/);
        await assert.rejects(client.claimCrossCheck(), /identity marker/);
        assert.equal(pulls, 0);
        assert.equal(cancelled, 3);
      }
    }
  }
});

it("M2 semantic marker and full candidate must agree at every body size", async () => {
  const f = await fixture();
  const candidate = { ...f.structural, candidate_identity_version: "semantic_v1", semantic_candidate: f.compiled.candidate };
  const response = { result: "candidate", candidate, candidate_digest: f.compiled.candidate.digest,
    candidate_generation: 1, checker_run_id: null, round: 1, deadline_at: "2030-01-01T00:00:00Z",
    verdict: "pending", reason_class: "", findings: null, lead_last_seq: 0 };
  const checker = { ...candidate, stage: "plan", lead_run_id: randomUUID(), round: 1,
    candidate_digest: response.candidate_digest, deadline_at: response.deadline_at };
  let wire: unknown, marker: string | undefined;
  let padding = 0;
  const client = new WorkerClient("http://example.test", "fixture", "test", nullLogger(), {
    fetch: async () => new Response(JSON.stringify(wire) + " ".repeat(padding), { headers: {
      "X-Uzi-Claim-Kind": "cross_check", ...(marker ? { "X-Uzi-Candidate-Identity-Version": marker } : {}),
    } }),
  });
  for (padding of [0, 7 * 1024 * 1024]) {
    marker = undefined;
    wire = response;
    await assert.rejects(client.submitPlanCrossCheck("run", 1, f.structural),
      padding ? /exceeds 6291456/ : /missing.*identity marker/);
    await assert.rejects(client.planCrossCheckStatus("run", 1),
      padding ? /exceeds 6291456/ : /missing.*identity marker/);
    wire = { kind: "cross_check", cross_check: checker };
    await assert.rejects(client.claimCrossCheck(), padding ? /exceeds 2097152/ : /missing.*identity marker/);
    marker = "semantic_v1";
    for (wire of [
      null, { result: "no_row", reason_class: "no_candidate", lead_last_seq: 0 },
      { result: "parked", lead_last_seq: 0 }, { ...response, candidate: f.structural },
      { ...response, candidate: { ...candidate, semantic_candidate: undefined } },
      { ...response, candidate_digest: "0".repeat(64) },
    ]) {
      await assert.rejects(client.submitPlanCrossCheck("run", 1, f.structural));
      await assert.rejects(client.planCrossCheckStatus("run", 1));
    }
    for (wire of [
      null, { kind: "cross_check" }, { kind: "cross_check", cross_check: { ...checker, ...f.structural,
        candidate_identity_version: undefined, semantic_candidate: undefined } },
      { kind: "cross_check", cross_check: { ...checker, stage: "code" } },
      { kind: "cross_check", cross_check: { ...checker, semantic_candidate: undefined } },
      { kind: "cross_check", cross_check: { ...checker, candidate_digest: "0".repeat(64) } },
    ]) await assert.rejects(client.claimCrossCheck());
  }
});

it("M2 semantic 64 MiB streaming bound cancels overflow before EOF and accepts exact-cap status", async () => {
  const f = semanticBoundaryFixture();
  let cancelled = 0, sent = 0;
  const client = new WorkerClient("http://example.test", "fixture", "test", nullLogger(), {
    fetch: async () => new Response(new ReadableStream<Uint8Array>({
      pull(controller) {
        const size = Math.min(1024 * 1024, 64 * 1024 * 1024 + 1 - sent);
        assert.ok(size > 0, "overflow must reject without waiting for EOF");
        sent += size; controller.enqueue(new Uint8Array(size));
      },
      cancel() { cancelled++; },
    }, { highWaterMark: 0 }), { headers: { "X-Uzi-Candidate-Identity-Version": "semantic_v1" } }),
  });
  await assert.rejects(client.planCrossCheckStatus("run", 1), /exceeds 67108864/);
  assert.equal(cancelled, 1);
  sent = 0;
  await assert.rejects(client.claimCrossCheck(), /exceeds 67108864/);
  assert.equal(cancelled, 2);

  const raw = JSON.stringify(f.response).padEnd(64 * 1024 * 1024, " ");
  const exact = new WorkerClient("http://example.test", "fixture", "test", nullLogger(), {
    fetch: async () => new Response(new ReadableStream<Uint8Array>({
      start(controller) { controller.enqueue(Buffer.from(raw)); controller.close(); },
      cancel() { assert.fail("exact-cap source must reach EOF"); },
    }), { headers: { "X-Uzi-Candidate-Identity-Version": "semantic_v1" } }),
  });
  assert.equal((await exact.planCrossCheckStatus("run", 1)).result, "candidate");
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
