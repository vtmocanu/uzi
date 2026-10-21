import type { CompletionPendingDTO, SemanticDriftDTO } from "./apiTypes";

// Owner transport mirrors api/internal/apitypes/semantic_{gate,plan,coverage}.go.
// Display validation never rewrites the approval object or grants authority.
export interface SemanticCitation { source_id: string; hash: string; start: number; end: number }
interface Binding { candidate_digest: string; source_digest: string; criteria_digest: string }
interface Maintainer { reason: string; conditions: string[]; verification_steps: string[] }
interface MaintainerApproval extends Maintainer { id: string }
interface SourceRef { id: string; kind: string; hash: string; provenance: string; completeness: string }
interface SourceCapture { id: string; kind: string; provenance: string; completeness: string; bytes: string }
interface Criterion {
  id: string; milestone_id: string; kind: string; text: string; done_when: string;
  citations: SemanticCitation[]; evidence_requirements: { description: string; path?: string }[];
  maintainer?: Maintainer;
}
export interface SemanticGateApproval extends Binding {
  presentation_revision: number; presentation_digest: string; candidate_identity_version: string;
  candidate_revision: number; claim_generation: number; review_id?: string; invocation_id?: string;
  report_digest?: string; maintainers: MaintainerApproval[];
}
export interface SemanticGateDTO {
  presentation: {
    candidate: {
      candidate_identity_version: string; revision: number; digest: string; sources: SourceRef[];
      plan_fields_digest?: string; plan_hash: string; source_outcome: string; criteria: Criterion[];
      coverage?: { outcome: string; candidate_digest: string; reviews: number; invocations: number; tokens: number; seconds: number };
    };
    binding: Binding; claim_generation: number;
    coverage: Binding & {
      outcome: string; reviews: number; invocations: number; charged_tokens: number;
      remaining_tokens: number; deadline: string;
      report?: Binding & {
        requirements: { requirement_id: string; status: string; explanation: string; citations: SemanticCitation[]; criterion_ids: string[] }[];
        criteria: { criterion_id: string; atomic: boolean; explanation: string }[];
      };
    };
    review_id?: string; invocation_id?: string; report_digest?: string;
    /** Why an autopilot run stopped for a human; unknown values are tolerated. */
    semantic_park?: string;
    consumed_seconds: number; remaining_seconds: number; remaining_reviews: number;
    maintainers: MaintainerApproval[];
  };
  approval: SemanticGateApproval; sources: SourceCapture[]; plan: string;
}

type Check = (v: unknown) => boolean;
const string: Check = v => typeof v === "string";
const integer: Check = v => Number.isSafeInteger(v) && (v as number) >= 0;
const positive: Check = v => integer(v) && (v as number) > 0;
const array = (check: Check): Check => v => Array.isArray(v) && v.every(check);
const object = (required: Record<string, Check>, optional: Record<string, Check> = {}): Check => v => {
  if (!v || typeof v !== "object" || Array.isArray(v)) return false;
  const value = v as Record<string, unknown>;
  return Object.entries(required).every(([k, check]) => Object.prototype.hasOwnProperty.call(value, k) && check(value[k])) &&
    Object.entries(optional).every(([k, check]) => !Object.prototype.hasOwnProperty.call(value, k) || check(value[k]));
};
const binding = { candidate_digest: string, source_digest: string, criteria_digest: string };
const identity = { review_id: string, invocation_id: string, report_digest: string };
const maintainer = { reason: string, conditions: array(string), verification_steps: array(string) };
const maintainers = array(object({ id: string, ...maintainer }));
const citation = object({ source_id: string, hash: string, start: integer, end: integer });
const schema = object({
  presentation: object({
    candidate: object({
      candidate_identity_version: v => v === "semantic_v1", revision: positive, digest: string,
      sources: array(object({ id: string, kind: string, hash: string, provenance: string, completeness: string })),
      plan_hash: string, source_outcome: string,
      criteria: v => Array.isArray(v) && v.length > 0 && v.length <= 256 && v.every(object({
        id: string, milestone_id: string, kind: string, text: string, done_when: string,
        citations: array(citation), evidence_requirements: array(object({ description: string }, { path: string })),
      }, { maintainer: object(maintainer) })),
    }, {
      plan_fields_digest: string,
      coverage: object({ outcome: string, candidate_digest: string, reviews: integer, invocations: integer, tokens: integer, seconds: integer }),
    }),
    binding: object(binding), claim_generation: integer,
    coverage: object({
      ...binding, outcome: string, reviews: integer, invocations: integer, charged_tokens: integer,
      remaining_tokens: integer, deadline: string,
    }, { report: object({
      ...binding,
      requirements: array(object({ requirement_id: string, status: string, explanation: string, citations: array(citation), criterion_ids: array(string) })),
      criteria: array(object({ criterion_id: string, atomic: v => typeof v === "boolean", explanation: string })),
    }) }),
    consumed_seconds: integer, remaining_seconds: integer, remaining_reviews: integer, maintainers,
  }, { ...identity, semantic_park: string }),
  approval: object({
    ...binding, presentation_revision: positive, presentation_digest: string,
    candidate_identity_version: v => v === "semantic_v1", candidate_revision: positive,
    claim_generation: integer, maintainers,
  }, identity),
  sources: array(object({ id: string, kind: string, provenance: string, completeness: string, bytes: string })),
  plan: string,
});

// Fatal UTF-8 decoding: replacement characters must never disguise invalid frozen bytes.
export function decodeSemanticText(raw: string): string {
  if (!/^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/.test(raw)) throw new Error("Invalid base64");
  const bytes = Uint8Array.from(atob(raw), c => c.charCodeAt(0));
  return new TextDecoder("utf-8", { fatal: true }).decode(bytes);
}

export function isSemanticGate(value: unknown): value is SemanticGateDTO {
  if (!schema(value)) return false;
  const gate = value as SemanticGateDTO;
  const p = gate.presentation;
  const a = gate.approval;
  const expected = p.candidate.criteria.filter(c => c.kind === "maintainer")
    .map(c => ({ id: c.id, ...c.maintainer }));
  const sameMaintainers = (items: MaintainerApproval[]) => items.length === expected.length &&
    items.every((m, i) => m.id === expected[i].id && m.reason === expected[i].reason &&
      JSON.stringify(m.conditions) === JSON.stringify(expected[i].conditions) &&
      JSON.stringify(m.verification_steps) === JSON.stringify(expected[i].verification_steps));
  if (p.candidate.digest !== p.binding.candidate_digest || a.candidate_revision !== p.candidate.revision ||
      a.claim_generation !== p.claim_generation || a.review_id !== p.review_id ||
      a.invocation_id !== p.invocation_id || a.report_digest !== p.report_digest ||
      !sameMaintainers(p.maintainers) || !sameMaintainers(a.maintainers) ||
      (["candidate_digest", "source_digest", "criteria_digest"] as const).some(key =>
        p.binding[key] !== a[key] || p.binding[key] !== p.coverage[key] ||
        (p.coverage.report !== undefined && p.binding[key] !== p.coverage.report[key]))) return false;
  try {
    decodeSemanticText(gate.plan);
    for (const source of gate.sources) decodeSemanticText(source.bytes);
    // Every frozen reference must have its complete transport capture.
    return gate.presentation.candidate.sources.every(ref => gate.sources.some(source =>
      source.id === ref.id && source.kind === ref.kind && source.provenance === ref.provenance &&
      source.completeness === ref.completeness));
  } catch { return false; }
}

const PARK_REASONS: Record<string, string> = {
  coverage_incomplete: "Autopilot stopped for a human: semantic coverage of the plan is incomplete.",
  coverage_uncertain: "Autopilot stopped for a human: semantic coverage of the plan is uncertain.",
  coverage_unavailable: "Autopilot stopped for a human: semantic coverage could not be checked.",
  maintainer_authorization: "Autopilot stopped for a human: the plan has maintainer criteria that need an owner's authorization.",
};
/** Human wording for a semantic_park value; an unknown value falls back to a neutral line. */
export function semanticParkReason(value: string): string {
  return Object.prototype.hasOwnProperty.call(PARK_REASONS, value)
    ? PARK_REASONS[value]
    : "Autopilot stopped for a human to review this plan.";
}

// completion_pending is publication data: unverified maintainer criteria, never evidence of completion.
const pendingCriterion = object({
  criterion_id: string, milestone_id: string, kind: v => v === "maintainer", text: string, reason: string,
  conditions: array(string), verification_steps: array(string), authorization_revision: integer, status: v => v === "unverified",
});
const pendingSchema = object({ version: integer, contract_revision: integer, criteria: array(pendingCriterion) });
export function isCompletionPending(value: unknown): value is CompletionPendingDTO {
  return pendingSchema(value);
}

// semantic_drift is a detector's record (PRD #1230 D5): hashes, refs and enums, never source text and never
// evidence that a criterion is met. Anything outside the shape is "could not be read", not partially shown.
const oneOf = (...values: string[]): Check => v => typeof v === "string" && values.includes(v);
const driftSource = object({
  source_id: string, frozen_hash: string, outcome: oneOf("unchanged", "changed", "unknown"),
}, {
  observed_hash: string, ref: string, checkbox_only: v => typeof v === "boolean",
  reason: oneOf("missing", "truncated", "unreadable", "denied", "invalid", "timeout"),
});
const driftObservation = object({
  phase: oneOf("presentation", "publication"), observed_at: string, issue: driftSource,
  prds: array(driftSource), links_added: array(string), links_removed: array(string),
}, { candidate_digest: string, gate_revision: integer, pushed_sha: string, links_truncated: v => typeof v === "boolean" });
const driftSchema = object({
  observations: array(driftObservation), dropped: integer, contradiction: v => typeof v === "boolean",
});
export function isSemanticDrift(value: unknown): value is SemanticDriftDTO {
  return driftSchema(value);
}
