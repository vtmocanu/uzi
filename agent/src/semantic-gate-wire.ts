import { z } from "zod";
import { isDeepStrictEqual } from "node:util";
import { parseSemanticCandidate } from "./semantic-plan.js";
import { semanticBindingSchema, semanticCoverageBaseSchema } from "./semantic-coverage-wire.js";

// Match api/internal/apitypes/semantic_transport.go: 80 MiB for candidate,
// presentation and approval (including three maintainer copies with sixfold Go
// JSON escaping), plus the existing 32 MiB RunDTO remainder. Only the hard
// semantic state path receives this cap; ordinary worker reads keep their caps.
export const SEMANTIC_GATE_ACK_MAX_BYTES = (80 + 32) * 1024 * 1024;

const natural = z.number().int().safe().nonnegative();
const positive = natural.positive();
const digest = z.string().regex(/^[0-9a-f]{64}$/);
const maintainer = z.strictObject({
  id: z.string().uuid(), reason: z.string(), conditions: z.array(z.string()),
  verification_steps: z.array(z.string()),
});
const reviewFields = {
  review_id: z.string().uuid().optional(), invocation_id: z.string().uuid().optional(),
  report_digest: digest.optional(),
};
const presentation = z.strictObject({
  candidate: z.unknown().transform(value => parseSemanticCandidate(value)),
  binding: semanticBindingSchema, claim_generation: positive,
  coverage: semanticCoverageBaseSchema.extend({
    // Gate projections include budget exhaustion for a newly revised candidate.
    outcome: z.enum(["unavailable", "complete", "incomplete", "uncertain", "exhausted"]),
  }), ...reviewFields,
  consumed_seconds: natural, remaining_seconds: natural, remaining_reviews: natural.max(3),
  maintainers: z.array(maintainer).max(256),
  // Present only on a gate parked by the autopilot fallback; bounded to the server's vocabulary.
  semantic_park: z.enum(["coverage_incomplete", "coverage_uncertain", "coverage_unavailable",
    "maintainer_authorization"]).optional(),
});
const approval = z.strictObject({
  presentation_revision: positive, presentation_digest: digest,
  candidate_identity_version: z.literal("semantic_v1"), candidate_revision: positive,
  ...semanticBindingSchema.shape, claim_generation: positive, ...reviewFields,
  maintainers: z.array(maintainer).max(256),
});

/** The API owns presentation hashes; validate correspondence without rehashing its JSON. */
export const semanticGateAckSchema = z.strictObject({
  semantic_presentation: presentation, semantic_approval: approval,
}).refine(({ semantic_presentation: p, semantic_approval: a }) => {
  const expected = p.candidate.criteria.filter(c => c.kind === "maintainer")
    .map(c => ({ id: c.id, ...c.maintainer! }));
  return p.candidate.digest === p.binding.candidate_digest &&
    a.candidate_revision === p.candidate.revision && a.claim_generation === p.claim_generation &&
    Object.entries(p.binding).every(([key, value]) =>
      a[key as keyof typeof p.binding] === value &&
      p.coverage[key as keyof typeof p.binding] === value &&
      (!p.coverage.report || p.coverage.report[key as keyof typeof p.binding] === value)) &&
    a.review_id === p.review_id && a.invocation_id === p.invocation_id &&
    a.report_digest === p.report_digest &&
    isDeepStrictEqual(p.maintainers, expected) && isDeepStrictEqual(a.maintainers, expected);
});
export type SemanticGateAck = z.infer<typeof semanticGateAckSchema>;
