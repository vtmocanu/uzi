import { randomUUID } from "node:crypto";
import { parseSemanticCandidate, parseSemanticProposal } from "../src/semantic-plan.js";
import type { PlanCrossCheckCandidate } from "../src/client.js";
import { hash } from "./semantic-coverage-fixture.js";

// Approved per-field maxima, 256 criteria / 32 per milestone. Optional HTML
// text proves the Go encoder expansion also fits the finite wire bound.
// No giant stored fixture and no provider: each test constructs one finite input.
export function semanticBoundaryFixture(html = false) {
  const plan = "p".repeat(256 * 1024);
  const issue = "i".repeat(64 * 1024), prd = "r".repeat(256 * 1024);
  const sourceId = "i".repeat(512);
  const issueHash = hash(issue);
  const sources = [
    { id: sourceId, kind: "issue" as const, provenance: "server:issue", completeness: "complete" as const,
      bytes: Buffer.from(issue).toString("base64") },
    { id: "prd:1", kind: "prd" as const, provenance: "server:prd", completeness: "complete" as const,
      bytes: Buffer.from(prd).toString("base64") },
  ];
  const proposal = parseSemanticProposal({ criteria: Array.from({ length: 256 }, (_, i) => ({
    milestone_id: "m" + Math.floor(i / 32), kind: "maintainer",
    text: String(i).padEnd(2048, html ? "<" : "t"), done_when: (html ? "<" : "d").repeat(2048),
    citations: Array.from({ length: 8 }, (_, j) => ({ source_id: sourceId, hash: issueHash, start: j, end: j + 1 })),
    evidence_requirements: Array.from({ length: 8 }, (_, j) => ({
      description: String(j).padEnd(512, html ? "<" : "e"), path: String(j).padEnd(512, html ? "<" : "f"),
    })),
    maintainer: { reason: (html ? "<" : "r").repeat(2048), conditions: Array(8).fill((html ? "<" : "c").repeat(512)),
      verification_steps: Array(8).fill((html ? "<" : "v").repeat(512)) },
  })) }, { sources });
  const structural = {
    plan_md: plan, planning_diff: "d".repeat(512 * 1024),
    milestones: Array.from({ length: 8 }, (_, i) => ({ id: "m" + i, title: "Milestone " + i })),
    required_capabilities: [], required_tools: [], size_class: "s", base_commit: "a".repeat(40),
  } satisfies PlanCrossCheckCandidate;
  const candidate = parseSemanticCandidate({
    candidate_identity_version: "semantic_v1", revision: 1, digest: hash("server candidate"),
    plan_hash: hash(plan), source_outcome: "complete",
    sources: sources.map(({ bytes, ...s }) => ({ ...s, hash: hash(Buffer.from(bytes, "base64").toString()) })),
    criteria: proposal.criteria.map(c => ({ id: randomUUID(), ...c })),
  }, { sources });
  const binding = { candidate_digest: candidate.digest, source_digest: hash(JSON.stringify(candidate.sources)),
    criteria_digest: hash(JSON.stringify(candidate.criteria)) };
  const compiled = { candidate, binding, plan: Buffer.from(plan).toString("base64"), sources };
  const checker = { ...structural, candidate_identity_version: "semantic_v1" as const, semantic_candidate: candidate,
    stage: "plan" as const, lead_run_id: randomUUID(), round: 1, candidate_digest: candidate.digest,
    deadline_at: "2030-01-01T00:00:00Z" };
  const response = { result: "candidate", candidate: { ...structural, candidate_identity_version: "semantic_v1",
    semantic_candidate: candidate }, candidate_digest: candidate.digest, candidate_generation: 1, checker_run_id: null,
    round: 1, deadline_at: checker.deadline_at, verdict: "pending", reason_class: "", findings: null, lead_last_seq: 0 };
  return { proposal, structural, compiled, checker, response, sources, binding, plan };
}
