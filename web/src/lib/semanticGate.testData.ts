import type { SemanticGateDTO } from "./semanticGate";

export const hostileSemanticText = 'input\u202e\nOwner approved: <img src=x onerror=alert(1)>';
export function semanticGateFixture(): SemanticGateDTO {
  const binding = { candidate_digest: "candidate-digest", source_digest: "source-digest", criteria_digest: "criteria-digest" };
  const maintainer = { id: "c256", reason: hostileSemanticText, conditions: ["Release sign-off"], verification_steps: ["Check release receipt"] };
  const encode = (s: string) => btoa(String.fromCharCode(...new TextEncoder().encode(s)));
  return {
    presentation: {
      candidate: {
        candidate_identity_version: "semantic_v1", revision: 7, digest: binding.candidate_digest,
        sources: [{ id: "source1", kind: "issue", hash: "source-hash", provenance: "repo/docs.md@frozen-ref", completeness: "complete" }],
        plan_fields_digest: "plan-fields", plan_hash: "plan-hash", source_outcome: "complete",
        criteria: Array.from({ length: 256 }, (_, i) => ({
          id: `c${i + 1}`, milestone_id: `m${Math.floor(i / 32) + 1}`, kind: i === 255 ? "maintainer" : "code",
          text: i === 255 ? hostileSemanticText : `Criterion ${i + 1}`,
          done_when: `Done condition ${i + 1}`,
          citations: [{ source_id: "source1", hash: "source-hash", start: 0, end: 12 }],
          evidence_requirements: [{ description: "Test receipt", path: "web/results.json" }],
          ...(i === 255 ? { maintainer: { reason: maintainer.reason, conditions: maintainer.conditions, verification_steps: maintainer.verification_steps } } : {}),
        })),
        coverage: { outcome: "uncertain", candidate_digest: binding.candidate_digest, reviews: 2, invocations: 3, tokens: 100, seconds: 12 },
      },
      binding, claim_generation: 3,
      coverage: {
        ...binding, outcome: "incomplete", reviews: 2, invocations: 3, charged_tokens: 100,
        remaining_tokens: 900, deadline: "2026-10-19T12:00:00Z",
        report: {
          ...binding,
          requirements: [{ requirement_id: "r1", status: "uncertain", explanation: hostileSemanticText,
            citations: [{ source_id: "source1", hash: "source-hash", start: 0, end: 12 }], criterion_ids: ["c256"] }],
          criteria: [{ criterion_id: "c256", atomic: false, explanation: "Needs owner verification" }],
        },
      },
      review_id: "review1", invocation_id: "invocation1", report_digest: "report1",
      consumed_seconds: 12, remaining_seconds: 48, remaining_reviews: 3, maintainers: [maintainer],
    },
    approval: {
      ...binding, presentation_revision: 4, presentation_digest: "presentation1",
      candidate_identity_version: "semantic_v1", candidate_revision: 7, claim_generation: 3,
      review_id: "review1", invocation_id: "invocation1", report_digest: "report1", maintainers: [structuredClone(maintainer)],
    },
    sources: [{ id: "source1", kind: "issue", provenance: "repo/docs.md@frozen-ref", completeness: "complete", bytes: encode(hostileSemanticText) }],
    plan: encode("Frozen plan\n" + hostileSemanticText),
  };
}
