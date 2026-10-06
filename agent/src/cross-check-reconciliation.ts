import type { PlanCrossCheckReconciliation } from "./protocol.js";

/** Decode the flat server snapshot as one group; absence never implies settlement. */
export function readPlanCrossCheckReconciliation(value: unknown): PlanCrossCheckReconciliation | undefined {
  if (!value || typeof value !== "object" || Array.isArray(value)) return undefined;
  const wire = value as Record<string, unknown>;
  const integer = (v: unknown, minimum: number): v is number =>
    typeof v === "number" && Number.isSafeInteger(v) && v >= minimum;
  const sha = (v: unknown): v is string => typeof v === "string" && /^[a-fA-F0-9]{64}$/.test(v);
  const id = wire.gate_presentation_id;
  const digest = wire.gate_payload_digest;
  if (!integer(wire.lead_last_seq, 0) || wire.lead_last_seq > 0x7fffffff || !integer(wire.claim_generation, 1) ||
      typeof wire.plan_cross_check_settled !== "boolean" || !integer(wire.gate_revision, 0) ||
      !sha(wire.current_plan_sha256)) return undefined;
  if (wire.gate_revision === 0) {
    if (id !== undefined || digest !== undefined) return undefined;
  } else if (typeof id !== "string" ||
      !/^[a-fA-F0-9]{8}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{12}$/.test(id) ||
      !sha(digest)) return undefined;
  return {
    leadLastSeq: wire.lead_last_seq,
    claimGeneration: wire.claim_generation,
    planCrossCheckSettled: wire.plan_cross_check_settled,
    gateRevision: wire.gate_revision,
    currentPlanSHA256: wire.current_plan_sha256,
    ...(id === undefined ? {} : { gatePresentationId: id as string, gatePayloadDigest: digest as string }),
  };
}
