/** Guidance only: capture uses ordinary message delivery, never an approval gate. */
export const DRAFT_PLAN_ACK = "Draft capture requested; this is not plan submission or approval.";

/** Decode only the named field; extras never enter signal reduction or replay hashing. */
export function parseDraftPlan(input: unknown): string | undefined {
  if (typeof input === "string") {
    // UTF-8 has at least one byte per UTF-16 unit; reject before scanning raw input.
    if (input.length > 256 * 1024 || Buffer.byteLength(input, "utf8") > 256 * 1024) return undefined;
    try { input = JSON.parse(input); } catch { return undefined; }
  }
  if (!input || typeof input !== "object" || Array.isArray(input)) return undefined;
  const plan = (input as Record<string, unknown>)["plan_md"];
  // Check decoded UTF-16 length before trimming, sanitizing, or redacting.
  if (typeof plan !== "string" || plan.length > 65_536 || !plan.trim()) return undefined;
  return plan;
}
