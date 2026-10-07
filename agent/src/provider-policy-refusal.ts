import { randomBytes } from "node:crypto";

export type PolicyRefusalTag = "cyberPolicy" | "misalignmentPolicyViolation";
type Admission = {
  readonly correlation_id: string;
  readonly phase: "planning" | "implementation";
};
export type PolicyRefusalPayload = {
  readonly event: "provider_policy_refusal";
  readonly provider: "codex";
  readonly category: "policy_refusal";
  readonly policy_tag: PolicyRefusalTag;
} & Admission & (
  | { readonly origin: "root" }
  | { readonly origin: "child"; readonly role: string; readonly parent_correlation_id: string }
);

// Worker-local nonce and monotonic counter: independent of provider and display identifiers.
const nonce = randomBytes(16).toString("hex");
let counter = 0;
export function admitPolicyTurn(phase: "plan" | "implement"): Admission {
  if (counter >= Number.MAX_SAFE_INTEGER) throw new Error("policy correlation exhausted");
  return Object.freeze({
    phase: phase === "plan" ? "planning" : "implementation",
    correlation_id: `pr-${nonce}-${++counter}`,
  });
}

export function isPolicyRefusalTag(tag: unknown): tag is PolicyRefusalTag {
  return tag === "cyberPolicy" || tag === "misalignmentPolicyViolation";
}

function validCorrelation(value: unknown): value is string {
  return typeof value === "string" && value.length >= 37 && value.length <= 52
    && /^pr-[0-9a-f]{32}-[1-9][0-9]{0,15}(?![\s\S])/.test(value)
    && Number.isSafeInteger(Number(value.slice(36)));
}

/** Closed validation at every injection boundary; malformed identifiers are rejected whole. */
export function validatePolicyRefusal(value: unknown): PolicyRefusalPayload | undefined {
  if (!value || typeof value !== "object" || Array.isArray(value)) return undefined;
  const p = value as Record<string, unknown>;
  const required = ["event", "provider", "category", "policy_tag", "phase", "correlation_id", "origin"];
  if (!required.every(key => Object.hasOwn(p, key))) return undefined;
  if (p.event !== "provider_policy_refusal" || p.provider !== "codex" || p.category !== "policy_refusal"
    || !isPolicyRefusalTag(p.policy_tag) || (p.phase !== "planning" && p.phase !== "implementation")
    || !validCorrelation(p.correlation_id)) return undefined;
  const common = {
    event: p.event, provider: p.provider, category: p.category, policy_tag: p.policy_tag,
    phase: p.phase, correlation_id: p.correlation_id,
  } as const;
  if (p.origin === "root" && Object.keys(p).length === 7) {
    return Object.freeze({ ...common, origin: "root" });
  }
  if (p.origin === "child" && Object.keys(p).length === 9
    && Object.hasOwn(p, "role") && Object.hasOwn(p, "parent_correlation_id") && validCorrelation(p.parent_correlation_id)
    && p.parent_correlation_id !== p.correlation_id && typeof p.role === "string"
    && p.role.length >= 1 && p.role.length <= 64 && !/[^a-zA-Z0-9_.-]/.test(p.role)) {
    return Object.freeze({
      ...common, origin: "child", role: p.role, parent_correlation_id: p.parent_correlation_id,
    });
  }
  return undefined;
}

export function sanitizePolicyRole(role: string, scrub: (role: string) => string = value => value): string {
  return scrub(role).replace(/[^a-zA-Z0-9_.-]/g, "_").slice(0, 64) || "child";
}

export function policyRefusalMessage(tag: PolicyRefusalTag): string {
  return `Codex provider safety-policy refusal (${tag})`;
}

/** Neutral typed failure: only validated worker metadata, never provider message fields. */
export class ProviderPolicyRefusal extends Error {
  readonly policyRefusal: PolicyRefusalPayload;
  constructor(payload: PolicyRefusalPayload) {
    const validated = validatePolicyRefusal(payload);
    if (!validated) throw new Error("invalid policy refusal metadata");
    super(policyRefusalMessage(validated.policy_tag));
    this.name = "ProviderPolicyRefusal";
    this.policyRefusal = validated;
  }
}
