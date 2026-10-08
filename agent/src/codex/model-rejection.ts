// Codex 0.159.3 synthetic local Responses HTTP 400 capture (#2151): the
// entire TurnError.message contains this envelope. This is protocol evidence,
// not proof of hosted subscription entitlement. No prose or RPC data is trusted.
export type ModelRejectionTag = "model_not_found";

export function modelRejectionTag(message: unknown): ModelRejectionTag | undefined {
 if (typeof message !== "string" || Buffer.byteLength(message) > 8 * 1024) return undefined;
 let value: unknown;
 try { value = JSON.parse(message); } catch { return undefined; }
 if (!value || typeof value !== "object" || Array.isArray(value)) return undefined;
 const error = (value as Record<string, unknown>).error;
 if (!error || typeof error !== "object" || Array.isArray(error)) return undefined;
 const e = error as Record<string, unknown>;
 return e.code === "model_not_found" && e.param === "model" && e.type === "invalid_request_error"
  ? "model_not_found" : undefined;
}

export class CrossCheckCheckerUnavailableError extends Error {
 constructor() {
  super("cross-check selected pin unavailable");
  this.name = "CrossCheckCheckerUnavailableError";
 }
}
