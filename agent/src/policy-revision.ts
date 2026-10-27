import type { PolicyRevisionCarrier, PolicyStopGateContext } from "./protocol.js";
import type { PlanVerdict } from "./steering.js";

/** Human persistence receipts keep numeric IDs only when conversion is exact. */
export function safeRevisionInputId(id: unknown): number | undefined {
  return typeof id === "number" && Number.isSafeInteger(id) && id > 0 ? id : undefined;
}

function canonicalUUID(value: unknown): value is string {
  return typeof value === "string" && value.length === 36
    && /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/u.test(value);
}

function positiveInt64(value: unknown): value is string {
  return typeof value === "string" && value.length <= 19
    && /^[1-9][0-9]*$/u.test(value) && !/[^0-9]/u.test(value)
    && (value.length < 19 || value <= "9223372036854775807");
}

/** Copy the closed Go gate shape. Unknown or malformed coverage never becomes null. */
function copyGate(value: unknown): PolicyStopGateContext | null | undefined {
  if (value === null) return null;
  if (typeof value !== "object" || Array.isArray(value)) return undefined;
  const keys = Reflect.ownKeys(value);
  if (keys.length !== 3 || !keys.includes("revision")
    || !keys.includes("presentation_id") || !keys.includes("payload_digest")) return undefined;
  const { revision, presentation_id, payload_digest } = value as Record<string, unknown>;
  if (!positiveInt64(revision) || !canonicalUUID(presentation_id)
    || typeof payload_digest !== "string" || payload_digest.length !== 64
    || !/^[0-9a-f]{64}$/u.test(payload_digest)) return undefined;
  return { revision, presentation_id, payload_digest };
}

/** Build a closed carrier from caller-supplied provenance; the caller owns authentication.
 * No lookup, inference, authority decision, or retained mutable gate reference. */
export function buildPolicyRevision(
  gate: unknown,
  verdict: Extract<PlanVerdict, { kind: "revise" }>,
): PolicyRevisionCarrier | undefined {
  const copied = copyGate(gate);
  if (copied === undefined) return undefined;
  if (verdict.automatic === true) {
    if (!canonicalUUID(verdict.crossCheckId)) return undefined;
    return { gate: copied, revision: { source: "plan_cross_check", cross_check_id: verdict.crossCheckId } };
  }
  const id = safeRevisionInputId(verdict.inputId);
  if (id === undefined) return undefined;
  return { gate: copied, revision: { source: "run_user_input", input_id: String(id) } };
}
