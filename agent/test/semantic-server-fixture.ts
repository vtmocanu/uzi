import assert from "node:assert/strict";
import { randomUUID } from "node:crypto";
import type { WorkerClient } from "../src/client.js";
import { parseSemanticCandidate, parseSemanticProposal, semanticSourceCaptureSchema } from "../src/semantic-plan.js";
import type { Report } from "../src/plan-contract-report.js";
import type { SemanticBinding, SemanticCoverageStatus, SemanticReservation } from "../src/semantic-coverage-wire.js";
import { criterionId, hash } from "./semantic-coverage-fixture.js";

/** In-memory server fixture only: fake canonicalization is not production digest authority. */
export function semanticServer(client: WorkerClient, events: string[]) {
  let compiled: { candidate: ReturnType<typeof parseSemanticCandidate>; binding: SemanticBinding;
    plan: string; sources: ReturnType<typeof semanticSourceCaptureSchema.parse>[] } | undefined;
  let sources: ReturnType<typeof semanticSourceCaptureSchema.parse>[] = [];
  let plan = "";
  let persisted: Report | undefined;
  let reviews = 0;
  let calls = 0;
  let charged = 0;
  let reviewCharged = 0;
  let denied = false;
  let replay = false;
  let reviewId: string | undefined;
  const requests: unknown[] = [];
  const status = (): SemanticCoverageStatus => ({
    ...compiled!.binding, outcome: persisted ? "complete" : "unavailable",
    reviews, invocations: calls, charged_tokens: charged, remaining_tokens: 120000 - charged,
    deadline: new Date(Date.now() + 900000).toISOString(), ...(persisted ? { report: persisted } : {}),
  });
  const report = (): Report => ({
    ...compiled!.binding,
    requirements: [{ requirement_id: "r1", status: "represented", criterion_ids: [criterionId],
      citations: compiled!.candidate.criteria[0]!.citations, explanation: "Source represented" }],
    criteria: [{ criterion_id: criterionId, atomic: true, explanation: "One behavior" }],
  });
  client.submitSemanticCandidate = async (_id, _gen, structural, proposal, captures) => {
    events.push("compile");
    assert.equal(structural.candidate_identity_version, undefined);
    assert.equal(structural.semantic_candidate, undefined);
    const parsed = parseSemanticProposal(proposal, { sources: captures });
    sources = captures.map(s => semanticSourceCaptureSchema.parse(s));
    plan = structural.plan_md;
    const digest = hash(JSON.stringify({ structural, parsed, sources }));
    if (compiled?.candidate.digest !== digest) persisted = undefined;
    const candidate = parseSemanticCandidate({
      candidate_identity_version: "semantic_v1", revision: 1, digest,
      plan_hash: hash(plan), source_outcome: "complete",
      sources: sources.map(({ bytes, ...s }) => ({ ...s, hash: hash(Buffer.from(bytes!, "base64").toString()) })),
      criteria: parsed.criteria.map(c => ({ id: criterionId, ...c })),
    }, { sources });
    compiled = { candidate, plan: Buffer.from(plan).toString("base64"), sources, binding: { candidate_digest: digest, source_digest: hash(JSON.stringify(candidate.sources)),
      criteria_digest: hash(JSON.stringify(candidate.criteria)) } };
    return compiled;
  };
  client.semanticCoverageStatus = async () => { events.push("status"); return status(); };
  client.reserveSemanticCoverage = async (_id, _gen, request): Promise<SemanticReservation> => {
    events.push("reserve"); requests.push(request);
    assert.deepEqual({ candidate_digest: request.candidate_digest, source_digest: request.source_digest, criteria_digest: request.criteria_digest }, compiled!.binding);
    const base = { ...compiled!.binding, deadline: new Date(Date.now() + 299000).toISOString(),
      reviews, invocations: calls, charged_tokens: charged, max_tokens: 40000 - (request.review_id ? reviewCharged : 0) };
    if (denied || !request.supported) return { ...base, outcome: "unavailable" };
    if (replay) return { ...base, outcome: "replay", review_id: reviewId ?? randomUUID(), invocation_id: randomUUID() };
    if (request.review_id) assert.equal(request.review_id, reviewId);
    else { reviewId = randomUUID(); reviews++; reviewCharged = 0; }
    calls++;
    return { ...base, outcome: "reserved", review_id: reviewId, invocation_id: randomUUID(),
      model: "sonnet", candidate: compiled!.candidate, plan: Buffer.from(plan).toString("base64"), sources };
  };
  client.settleSemanticCoverage = async (_id, _gen, request) => {
    events.push("settle:" + request.status);
    assert.equal(request.review_id, reviewId);
    const charge = request.tokens ?? 40000;
    charged += charge;
    reviewCharged += charge;
    return status();
  };
  client.publishSemanticCoverage = async (_id, _gen, request) => {
    events.push("publish"); persisted = request.report; return status();
  };
  return { report, status, requests, compiled: () => compiled!, sources: () => sources,
    deny: () => { denied = true; }, replay: () => { replay = true; },
    persisted: () => persisted };
}
