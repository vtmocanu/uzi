// Dormant worker protocol. Digests are supplied by the server, never rehashed JSON.
import { z } from "zod";
import { parseSemanticCandidate, semanticSourceCaptureSchema } from "./semantic-plan.js";
import { coverageReportSchema } from "./plan-contract-report.js";

const digest = z.string().regex(/^[0-9a-f]{64}$/);
export const semanticBindingSchema = z.strictObject({
  candidate_digest: digest, source_digest: digest, criteria_digest: digest,
});
export type SemanticBinding = z.infer<typeof semanticBindingSchema>;
const candidate = z.unknown().transform(value => parseSemanticCandidate(value));
const natural = z.number().int().safe().nonnegative();
const base64 = z.string().refine(s => Buffer.from(s, "base64").toString("base64") === s);
export const semanticCandidateResponseSchema = z.strictObject({
  candidate, binding: semanticBindingSchema,
  plan: base64, sources: z.array(semanticSourceCaptureSchema).max(9),
}).refine(v => v.candidate.digest === v.binding.candidate_digest);
export const semanticCoverageStatusSchema = z.strictObject({
  outcome: z.enum(["unavailable", "complete", "incomplete", "uncertain"]),
  // Unavailable before enrollment can carry empty bindings and a zero Go timestamp.
  candidate_digest: z.union([digest, z.literal("")]),
  source_digest: z.union([digest, z.literal("")]),
  criteria_digest: z.union([digest, z.literal("")]),
  reviews: natural.max(3), invocations: natural.max(6),
  charged_tokens: natural, remaining_tokens: natural,
  deadline: z.string().datetime({ offset: true }),
  report: coverageReportSchema.optional(),
});
export const semanticReservationSchema = z.strictObject({
  outcome: z.enum(["reserved", "replay", "unavailable"]),
  review_id: z.string().uuid().optional(), invocation_id: z.string().uuid().optional(),
  candidate_digest: z.union([digest, z.literal("")]),
  source_digest: z.union([digest, z.literal("")]),
  criteria_digest: z.union([digest, z.literal("")]),
  model: z.literal("sonnet").optional(),
  deadline: z.string().datetime({ offset: true }),
  max_tokens: natural.max(40000), reviews: natural.max(3), invocations: natural.max(2),
  charged_tokens: natural,
  candidate: candidate.optional(), plan: base64.optional(),
  sources: z.array(semanticSourceCaptureSchema).max(9).optional(),
}).superRefine((v, ctx) => {
  if (v.outcome === "reserved" && (!v.review_id || !v.invocation_id || !v.model ||
      !v.candidate || !v.plan || !v.sources || v.candidate.digest !== v.candidate_digest ||
      !digest.safeParse(v.source_digest).success || !digest.safeParse(v.criteria_digest).success)) {
    ctx.addIssue({ code: "custom", message: "Incomplete coverage reservation" });
  }
});
export type SemanticReservation = z.infer<typeof semanticReservationSchema>;
export type SemanticCoverageStatus = z.infer<typeof semanticCoverageStatusSchema>;
export interface SemanticReserveRequest extends SemanticBinding {
  request_id: string; review_id?: string; supported: boolean;
}
export interface SemanticSettleRequest {
  review_id: string; invocation_id: string;
  status: "success" | "schema_error" | "transport_error" | "unknown";
  tokens?: number;
}
export interface SemanticPublishRequest {
  review_id: string; invocation_id: string; report: z.infer<typeof coverageReportSchema>;
}
