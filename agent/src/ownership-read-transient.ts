/**
 * Positive classification of a transient failure of the run-ownership probe
 * (`WorkerClient.getRunOwnership`), e.g. while the API restarts.
 *
 * `isTransient` in client.ts is not used here: it is true for any non-HTTP failure, which
 * would also retry deadline, abort and decode failures. This classifier answers only when
 * there is positive evidence, and returns a bounded token (never message text).
 *
 * The transport-phase marker (`isFetchRejection`) is required because a truncated 200 body
 * surfaces from the body read as `TypeError("terminated")` with cause `UND_ERR_SOCKET`,
 * the same shape as a socket failure while connecting; only the marker tells them apart.
 */
import { isFetchRejection, RequestError } from "./client.js";
import { WORKER_API_PREFIX } from "./protocol.js";

const TRANSIENT_HTTP_STATUSES: ReadonlySet<number> = new Set([502, 503, 504]);
const TRANSIENT_CAUSE_CODES: ReadonlySet<string> = new Set(["ECONNREFUSED", "ECONNRESET", "UND_ERR_SOCKET"]);

export function transientOwnershipReadCause(error: unknown, runId: string): string | undefined {
  try {
    if (error instanceof RequestError) {
      if (error.method !== "GET" || error.path !== `${WORKER_API_PREFIX}/runs/${runId}/ownership`) return undefined;
      const status = error.status;
      return TRANSIENT_HTTP_STATUSES.has(status) ? `http_${status}` : undefined;
    }
    if (!isFetchRejection(error) || !(error instanceof TypeError)) return undefined;
    if (!Object.hasOwn(error, "cause")) return undefined;
    const cause: unknown = (error as { cause?: unknown }).cause;
    if (typeof cause !== "object" || cause === null) return undefined;
    const code: unknown = (cause as { code?: unknown }).code;
    return typeof code === "string" && TRANSIENT_CAUSE_CODES.has(code) ? code.toLowerCase() : undefined;
  } catch {
    return undefined;
  }
}
