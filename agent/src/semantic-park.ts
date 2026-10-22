// PRD #1230 M4 (dormant): the autopilot coverage park. The SERVER owns the coverage verdict
// (evaluateSemanticCoverage); the worker only mirrors the park reason the server will validate
// against its own gate projection (validateSemanticPark), so a disagreement is refused, never
// believed.
import { RequestError, isTransientStatus } from "./client.js";

export type SemanticPark = "coverage_incomplete" | "coverage_uncertain" | "coverage_unavailable" | "maintainer_authorization";
/** The worker-wire status outcome; the gate projection's "exhausted" is "unavailable" here. */
export type SemanticCoverageOutcome = "complete" | "incomplete" | "uncertain" | "unavailable";

/** The reason the server requires on an auto_approve awaiting_approval report, or undefined when
 * the candidate is complete and maintainer-free (only the checker verdict, if any, gates it). */
export function expectedSemanticPark(outcome: SemanticCoverageOutcome, maintainerCount: number): SemanticPark | undefined {
  switch (outcome) {
    case "complete": return maintainerCount > 0 ? "maintainer_authorization" : undefined;
    case "incomplete": return "coverage_incomplete";
    case "uncertain": return "coverage_uncertain";
    default: return "coverage_unavailable";
  }
}

const NETWORK_CODE = /^E[A-Z0-9]+$|^UND_ERR/;
/** A transport or server-availability failure of a coverage call: network, timeout, 408/429/5xx.
 * An owner abort, a malformed or foreign response (ZodError, "Foreign semantic ...") and any 4xx
 * refusal are NOT transport failures and stay fail-closed. */
export function isCoverageTransportFailure(error: unknown, owner: AbortSignal): boolean {
  if (owner.aborted) return false;
  if (error instanceof RequestError) return isTransientStatus(error.status);
  if (!(error instanceof Error)) return false;
  if (error.name === "TimeoutError" || error.name === "AbortError") return true;
  for (let e: unknown = error, depth = 0; e instanceof Error && depth < 4; e = (e as { cause?: unknown }).cause, depth++) {
    const code = (e as { code?: unknown }).code;
    if (typeof code === "string" && NETWORK_CODE.test(code)) return true;
    if (e instanceof TypeError && /fetch failed|network/i.test(e.message)) return true;
  }
  return false;
}
