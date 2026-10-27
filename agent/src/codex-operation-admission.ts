import type { CodexOperationDecision, CodexOperationIdentity, CodexOperationRequest } from "./protocol.js";

function record(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}
function closed(value: Record<string, unknown>, keys: string[]): boolean {
  return Object.keys(value).every(key => keys.includes(key));
}
function positive(value: unknown): value is number {
  return Number.isSafeInteger(value) && (value as number) > 0;
}
function uuid(value: unknown): value is string {
  return typeof value === "string" && /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/.test(value) &&
    value !== "00000000-0000-0000-0000-000000000000";
}
export function validCodexOperation(value: unknown): value is CodexOperationIdentity {
  if (!record(value) || !closed(value, ["operation_id", "kind", "source"]) ||
      !uuid(value.operation_id) || !["root", "child", "advice", "checker"].includes(value.kind as string)) return false;
  if (value.source === undefined) return true;
  const s = value.source;
  return record(s) && closed(s, ["gate_revision", "presentation_id", "revision_input_id"]) &&
    positive(s.gate_revision) && positive(s.revision_input_id) && uuid(s.presentation_id);
}
export function validCodexOperationRequest(value: unknown): value is CodexOperationRequest {
  if (!record(value)) return false;
  const { claim_generation, ...operation } = value;
  return validCodexOperation(operation) && positive(claim_generation);
}
function timestamp(value: unknown): value is string {
  if (typeof value !== "string" || !/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,9})?(?:Z|[+-]\d{2}:\d{2})$/.test(value)) return false;
  if (Number(value.slice(11, 13)) > 23 || Number(value.slice(14, 16)) > 59 || Number(value.slice(17, 19)) > 59) return false;
  if (!value.endsWith("Z") && (Number(value.slice(-5, -3)) > 23 || Number(value.slice(-2)) > 59)) return false;
  const [year, month, day] = value.slice(0, 10).split("-").map(Number);
  return month! >= 1 && month! <= 12 && day! >= 1 &&
    day! <= new Date(Date.UTC(year!, month!, 0)).getUTCDate() && Number.isFinite(Date.parse(value));
}
// Date.parse truncates fractional milliseconds; PostgreSQL timestamps retain them.
function timestampNanos(value: string): bigint {
  const fraction = /\.(\d+)/.exec(value)?.[1] ?? "";
  return BigInt(Date.parse(value)) * 1_000_000n + BigInt(fraction.padEnd(9, "0").slice(3));
}
export function decodeCodexOperationDecision(value: unknown, request: CodexOperationRequest): CodexOperationDecision {
  const fail = (): never => { throw new Error("invalid Codex operation admission response"); };
  if (!record(value) || value.operation_id !== request.operation_id || !uuid(value.operation_id) ||
      value.claim_generation !== request.claim_generation || !positive(value.claim_generation)) return fail();
  const common = ["decision", "operation_id", "claim_generation"];
  if (value.decision === "admit" && closed(value, common)) return value as CodexOperationDecision;
  if (value.decision !== "wait" || !closed(value, [...common, "class", "episode_id", "server_now", "eligible_at"]) ||
      !["rate_limit", "overload"].includes(value.class as string) || !uuid(value.episode_id) ||
      !timestamp(value.server_now) || !timestamp(value.eligible_at) ||
      timestampNanos(value.eligible_at) <= timestampNanos(value.server_now)) return fail();
  return value as CodexOperationDecision;
}
