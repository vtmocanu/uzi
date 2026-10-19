import { it } from "node:test";
import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import { WorkerClient, readRunAck } from "../src/client.js";
import { nullLogger } from "./helpers.js";
import { semanticServer } from "./semantic-server-fixture.js";
import { SEMANTIC_GATE_ACK_MAX_BYTES } from "../src/semantic-gate-wire.js";
import { proposal } from "./semantic-coverage-fixture.js";

async function fixture() {
  let response: unknown;
  let responseStatus = 200;
  let wireResponse: (() => Response) | undefined;
  const requests: unknown[] = [];
  const client = new WorkerClient("http://example.test", "fixture", "test", nullLogger(), {
    fetch: async (_url, init) => {
      requests.push(JSON.parse(String(init?.body)));
      return wireResponse ? wireResponse() : new Response(JSON.stringify(response), { status: responseStatus });
    },
  });
  client.protocolFeatures = ["claim_generation_fence", "gate_revision_v1"];
  const server = semanticServer(client, []);
  await client.submitSemanticCandidate("run", 7, {
    plan_md: "Implement A", milestones: [{ id: "m1", title: "A" }],
    required_capabilities: [], required_tools: [], size_class: "s",
    base_commit: "a".repeat(40), planning_diff: "",
  }, proposal(), [{ id: "issue:1", kind: "issue", provenance: "server:issue",
    completeness: "complete", bytes: Buffer.from("Implement A").toString("base64") }]);
  const body = { status: "awaiting_approval" as const, plan_md: "Implement A",
    presentation_id: randomUUID(), candidate_digest: server.compiled().candidate.digest, claim_generation: 7 };
  const ack = { run: { id: "run", status: "awaiting_approval" }, ...server.gateAck(7, 4) };
  response = ack;
  return { client, requests, body, ack, respondWire: (value: () => Response) => { wireResponse = value; }, respond: (value: unknown, status = 200) => {
    response = value; responseStatus = status;
  } };
}


it("semantic hard state accepts exhausted coverage for a revised candidate without downgrade", async () => {
  const f = await fixture();
  f.ack.semantic_presentation.coverage.outcome = "exhausted" as typeof f.ack.semantic_presentation.coverage.outcome;
  f.ack.semantic_presentation.remaining_reviews = 0;
  const ack = await f.client.reportSemanticPlanGateState("run", f.body);
  assert.equal(ack.semanticGate?.semantic_presentation.coverage.outcome, "exhausted");
  assert.deepEqual(f.requests, [f.body]);
});

it("semantic hard ACK preserves all 256 criteria across eight milestones with Go HTML escaping", async () => {
  const f = await fixture();
  const p = f.ack.semantic_presentation;
  const fill = (prefix: string, bytes: number) => prefix + "<".repeat(bytes - prefix.length);
  const sourceId = fill("source", 512);
  const source = { ...p.candidate.sources[0]!, id: sourceId, provenance: fill("origin", 2048) };
  p.candidate.sources = [source];
  p.candidate.criteria = Array.from({ length: 256 }, (_, i) => ({
    id: randomUUID(), milestone_id: ("m" + Math.floor(i / 32)).padEnd(64, "x"), kind: "maintainer" as const,
    text: fill("text" + i, 2048), done_when: fill("done" + i, 2048),
    citations: Array.from({ length: 8 }, (_, j) => ({ source_id: sourceId, hash: source.hash, start: j, end: j + 1 })),
    evidence_requirements: Array.from({ length: 8 }, (_, j) => ({
      description: fill("evidence" + i + ":" + j, 512), path: fill("path" + i + ":" + j, 512),
    })),
    maintainer: {
      reason: fill("reason" + i, 2048),
      conditions: Array.from({ length: 8 }, (_, j) => fill("condition" + i + ":" + j, 512)),
      verification_steps: Array.from({ length: 8 }, (_, j) => fill("verify" + i + ":" + j, 512)),
    },
  }));
  const maintainers = p.candidate.criteria.map(c => ({ id: c.id, ...c.maintainer! }));
  p.maintainers = maintainers;
  f.ack.semantic_approval.maintainers = maintainers;
  // encoding/json escapes HTML characters to six bytes; JSON.stringify does not.
  const raw = JSON.stringify(f.ack).replaceAll("<", "\\u003c");
  assert.ok(Buffer.byteLength(raw) > 64 * 1024 * 1024);
  assert.ok(Buffer.byteLength(raw) < 80 * 1024 * 1024);
  f.respondWire(() => new Response(raw, { headers: { "X-Uzi-Candidate-Identity-Version": "semantic_v1" } }));
  const ack = await f.client.reportSemanticPlanGateState("run", f.body);
  assert.deepEqual(ack.semanticGate, {
    semantic_presentation: p, semantic_approval: f.ack.semantic_approval,
  });
  assert.deepEqual(f.requests, [f.body]);
});

it("semantic hard ACK rejects streamed overflow without retry or downgrade", async () => {
  const f = await fixture();
  const chunk = new Uint8Array(1024 * 1024).fill(32);
  let cancelled = false;
  f.respondWire(() => {
    let remaining = SEMANTIC_GATE_ACK_MAX_BYTES + 1;
    return new Response(new ReadableStream({
      pull(controller) {
        if (remaining === 0) return;
        const count = Math.min(remaining, chunk.length);
        controller.enqueue(chunk.subarray(0, count));
        remaining -= count;
        // Leave EOF pending so the overflow reader must cancel this stream.
      },
      cancel() { cancelled = true; },
    }));
  });
  await assert.rejects(f.client.reportSemanticPlanGateState("run", f.body), /exceeds/);
  assert.deepEqual(f.requests, [f.body]);
  assert.equal(cancelled, true);
});

it("semantic hard ACK rejects orphan and unknown markers and sibling orphans", async () => {
  const f = await fixture();
  for (const [marker, body] of [
    ["unknown", f.ack],
    ["semantic_v1", { run: f.ack.run, gate_revision: 4 }],
    ["semantic_v1", { ...f.ack, semantic_approval: undefined }],
    ["semantic_v1", { ...f.ack, semantic_presentation: undefined }],
  ] as const) {
    f.respondWire(() => new Response(JSON.stringify(body), { headers: { "X-Uzi-Candidate-Identity-Version": marker } }));
    await assert.rejects(f.client.reportSemanticPlanGateState("run", f.body), /ACK/);
  }
  assert.equal(f.requests.length, 4);
});

it("semantic state preserves candidate and generation and reads exact server presentation", async () => {
  const f = await fixture();
  const ack = await f.client.reportSemanticPlanGateState("run", f.body);
  assert.deepEqual(f.requests, [f.body]);
  assert.deepEqual(ack.semanticGate, {
    semantic_presentation: f.ack.semantic_presentation, semantic_approval: f.ack.semantic_approval,
  });
  assert.equal(ack.gateRevision, 4);
  assert.equal(ack.sentClaimGeneration, 7);
});

it("throwing semantic candidate decoding remains malformed for running ACKs", async () => {
  const f = await fixture();
  const wire = { run: { id: "run", status: "running" },
    semantic_presentation: { candidate: {} }, semantic_approval: {} };
  const decoded = await readRunAck(new Response(JSON.stringify(wire), {
    headers: { "X-Uzi-Candidate-Identity-Version": "semantic_v1" },
  }));
  assert.equal(decoded.semanticGateMalformed, true);
  f.respond(wire);
  await assert.rejects(f.client.reportSemanticPlanGateState("run", {
    ...f.body, status: "running",
  }), /malformed semantic presentation ACK/);
});

it("semantic strict decoder refusal makes one fenced attempt with no downgrade", async () => {
  const f = await fixture();
  f.respond({ error: "invalid request body" }, 400);
  await assert.rejects(f.client.reportSemanticPlanGateState("run", f.body));
  assert.deepEqual(f.requests, [f.body]);
});

it("semantic state requires both existing negotiated fences before any request", async () => {
  for (const features of [[], ["gate_revision_v1"], ["claim_generation_fence"]]) {
    const f = await fixture();
    f.client.protocolFeatures = features;
    await assert.rejects(f.client.reportSemanticPlanGateState("run", f.body), /negotiation/);
    assert.deepEqual(f.requests, []);
  }
});


it("semantic hard ACK rejects every divergent maintainer field and binding", async () => {
  const f = await fixture();
  const criterion = f.ack.semantic_presentation.candidate.criteria[0]!;
  criterion.kind = "maintainer";
  criterion.maintainer = { reason: "Needs owner access", conditions: ["Owner condition"], verification_steps: ["Observe rollout"] };
  const maintainers = [{ id: criterion.id, ...criterion.maintainer }];
  f.ack.semantic_presentation.maintainers = maintainers;
  f.ack.semantic_approval.maintainers = maintainers;
  for (const sibling of ["semantic_presentation", "semantic_approval"] as const) {
    for (const patch of [
      { id: randomUUID() }, { reason: "Changed reason" }, { conditions: ["Changed condition"] },
      { verification_steps: ["Changed verification"] },
    ]) {
      f.respond({ ...f.ack, [sibling]: { ...f.ack[sibling], maintainers: [{ ...maintainers[0], ...patch }] } });
      await assert.rejects(f.client.reportSemanticPlanGateState("run", f.body), /ACK/);
    }
  }
  for (const field of ["candidate_digest", "source_digest", "criteria_digest"] as const) {
    f.respond({ ...f.ack, semantic_approval: { ...f.ack.semantic_approval, [field]: "f".repeat(64) } });
    await assert.rejects(f.client.reportSemanticPlanGateState("run", f.body), /ACK/);
    f.respond({ ...f.ack, semantic_presentation: { ...f.ack.semantic_presentation,
      coverage: { ...f.ack.semantic_presentation.coverage, [field]: "f".repeat(64) },
    } });
    await assert.rejects(f.client.reportSemanticPlanGateState("run", f.body), /ACK/);
  }
});

it("semantic human storage rejects absent, malformed and foreign ACK bindings", async () => {
  const f = await fixture();
  const variants = [
    { run: f.ack.run, gate_revision: 4 },
    { ...f.ack, semantic_presentation: null },
    { ...f.ack, semantic_approval: { ...f.ack.semantic_approval, candidate_digest: "f".repeat(64) } },
    { ...f.ack, semantic_approval: { ...f.ack.semantic_approval, claim_generation: 8 } },
    { ...f.ack, gate_revision: 5 },
    { ...f.ack, semantic_approval: { ...f.ack.semantic_approval, presentation_digest: "invalid" } },
  ];
  for (const value of variants) {
    f.respond(value);
    await assert.rejects(f.client.reportSemanticPlanGateState("run", f.body), /ACK/);
  }
  const malformed = await readRunAck(new Response(JSON.stringify({ ...f.ack, semantic_approval: null })));
  assert.equal(malformed.semanticGateMalformed, true);
  assert.equal(malformed.semanticGate, undefined);
});
