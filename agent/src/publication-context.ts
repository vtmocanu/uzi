import type { PublicationContextRequest, PublicationContextResponse, PublicationContextBinding } from "./protocol.js";

const reasons = new Set([
  "claim_changed", "ancestry_unknown", "association_unknown", "ambiguous_source",
  "source_unknown", "authority_unknown", "publication_unproven", "invalid_request",
  "forge_unknown", "forge_mismatch", "authority_changed",
]);
const positive = (v: unknown): v is number => Number.isSafeInteger(v) && (v as number) > 0;
const sha = (v: unknown): v is string => typeof v === "string" && /^[a-fA-F0-9]{40}$/.test(v);
const uuid = (v: unknown): v is string => typeof v === "string" &&
  /^[a-fA-F0-9]{8}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{12}$/.test(v) &&
  v !== "00000000-0000-0000-0000-000000000000";
const branch = (v: unknown): v is string => typeof v === "string" && v.length > 0 &&
  Buffer.byteLength(v) <= 1024 && !/[\p{Cc}\p{Cf}]/u.test(v);
const object = (v: unknown): v is Record<string, unknown> => !!v && typeof v === "object" && !Array.isArray(v);
const keys = (v: Record<string, unknown>, names: string[]): boolean =>
  Object.keys(v).length === names.length && names.every(n => Object.hasOwn(v, n));
const invalid = (): never => { throw new Error("invalid publication context"); };

export function validatePublicationContextRequest(runId: string, request: PublicationContextRequest): void {
  if (!uuid(runId) || !object(request) || !keys(request, ["claim_generation", "branch", "candidate_head"]) ||
      !positive(request.claim_generation) || !branch(request.branch) || !sha(request.candidate_head) ||
      Buffer.byteLength(JSON.stringify(request)) > 4096) invalid();
}

/** Decode the entire envelope before accepting any authority; errors never include server prose. */
export function decodePublicationContext(raw: unknown, runId: string, request: PublicationContextRequest): PublicationContextResponse {
  if (!object(raw) || raw.version !== 1) return invalid();
  if (raw.status === "unknown") {
    if (!keys(raw, ["version", "status", "reason"]) || typeof raw.reason !== "string" || !reasons.has(raw.reason)) return invalid();
    return { version: 1, status: "unknown", reason: raw.reason };
  }
  if ((raw.status !== "resolved" && raw.status !== "not_applicable") ||
      !keys(raw, ["version", "status", "binding"]) || !object(raw.binding)) return invalid();
  const b = raw.binding;
  if (!keys(b, ["run_id", "claim_generation", "branch", "candidate_head", "mr_iid", "source_run_id", "source_publication", "issue_iid", "intent"]) ||
      !uuid(b.run_id) || b.run_id !== runId || !positive(b.claim_generation) || b.claim_generation !== request.claim_generation ||
      !branch(b.branch) || b.branch !== request.branch || !sha(b.candidate_head) || b.candidate_head !== request.candidate_head ||
      !positive(b.mr_iid) || !uuid(b.source_run_id) || b.source_run_id === runId) return invalid();
  const p = b.source_publication;
  if (p !== null) {
    if (!object(p) || !keys(p, ["version", "claim_generation", "branch", "mr_iid", "published_head", "intent", "reported_non_closing"]) ||
        p.version !== 1 || !positive(p.claim_generation) || !branch(p.branch) || p.branch !== b.branch ||
        !positive(p.mr_iid) || p.mr_iid !== b.mr_iid || !sha(p.published_head) ||
        (p.intent !== "closing" && p.intent !== "non_closing") || typeof p.reported_non_closing !== "boolean" ||
        (p.reported_non_closing && p.intent !== "non_closing")) return invalid();
  }
  if (raw.status === "not_applicable") {
    if (b.intent !== "not_applicable" || b.issue_iid !== null || p !== null) return invalid();
  } else {
    if (!positive(b.issue_iid) || (b.intent !== "closing" && b.intent !== "non_closing") ||
        (b.intent === "closing" && (p === null || p.intent !== "closing"))) return invalid();
  }
  return { version: 1, status: raw.status, binding: b as unknown as PublicationContextBinding };
}

/** Identity is historical and immutable; the outgoing candidate is deliberately excluded.
 * A newly observed restriction is latched for all later candidates in this publication. */
export class PublicationContextLatch {
  private identity?: string;
  private restrictedIssue?: number;

  observe(context: PublicationContextResponse): number | undefined {
    if (context.status === "unknown") throw new Error("publication context unavailable");
    const b = context.binding;
    const p = b.source_publication;
    const identity = JSON.stringify([b.source_run_id, b.mr_iid, b.issue_iid, p === null ? null :
      [p.version, p.claim_generation, p.branch, p.mr_iid, p.published_head, p.intent, p.reported_non_closing]]);
    if (this.identity !== undefined && this.identity !== identity) throw new Error("publication source identity changed");
    this.identity = identity;
    if (b.intent === "non_closing") this.restrictedIssue = b.issue_iid!;
    return this.restrictedIssue;
  }
}
