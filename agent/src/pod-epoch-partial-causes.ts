/** Report-only source map: discoverEpochRetainedSources owns census entry/bare/seen/path
 * caps and unreadable/deadline/interruption. retainedNames/retainedBares and
 * checkEpochMetadata (including exec output clipping) attach scoped error details;
 * only an inventory catch commits them, so evaluateRetained blockers do not leak.
 * inspectRetainedSources owns budget/deadline/interruption, bare/descriptor/path
 * and binding failures. measureAllocated owns entry/pending/depth/deadline and
 * filesystem failures, including read and finally-close as independent events.
 * runPodEpochShadow owns grouping/selector and ownership/holds read failures.
 * Measurement EACCES/EPERM (opendir or entry lstat below the parent) and the entry, depth and
 * pending caps are tree-local in epoch inspection: their counters may appear on an eligible pass.
 * Counters stay diagnostic and grant nothing.
 */
const helperSuffixes = ["entry_cap", "bare_cap", "metadata_byte_cap", "metadata_record_cap", "source_record_cap"] as const;
const readSuffixes = ["http_1xx", "http_2xx", "http_3xx", "http_4xx", "http_5xx", "timeout", "transport", "other"] as const;
const keys = [
  ...helperSuffixes.map(k => `census_${k}` as const),
  ...helperSuffixes.map(k => `inspection_${k}` as const),
  ...readSuffixes.map(k => `ownership_read_${k}` as const),
  ...readSuffixes.map(k => `holds_read_${k}` as const),
  "census_seen_path_cap", "census_unreadable", "census_deadline", "census_interrupted",
  "census_physical_path_cap", "census_unclassified", "inspection_unclassified",
  "group_cap", "group_physical_path_cap", "selector_refused",
  "inspection_budget", "inspection_deadline", "inspection_interrupted", "inspection_unreadable_bare",
  "inspection_descriptor_cap", "inspection_physical_path_cap", "tree_binding_failed",
  "measurement_entry_cap", "measurement_pending_cap", "measurement_depth_cap",
  "measurement_deadline", "measurement_filesystem_error",
] as const;
export type PartialCause = typeof keys[number];
export type PartialCauses = Partial<Record<PartialCause, number>>;
const allowed = new Set<string>(keys);
function get(value: unknown, key: string): unknown {
  try { return value !== null && (typeof value === "object" || typeof value === "function") ? Reflect.get(value, key) : undefined; }
  catch { return undefined; }
}
export function countPartialCause(target: PartialCauses, key: PartialCause, amount = 1): void {
  try {
    if (!allowed.has(key) || !Number.isSafeInteger(amount) || amount <= 0) return;
    const old = get(target, key);
    target[key] = Math.min(65535, (typeof old === "number" && Number.isSafeInteger(old) && old > 0 ? old : 0) + amount);
  } catch { /* Diagnostics never change the observed operation. */ }
}
/** Return whether the source supplied a usable explanation, including for legacy adapters. */
export function mergePartialCauses(target: PartialCauses, source: PartialCauses | undefined): boolean {
  let found = false;
  for (const key of keys) {
    const amount = get(source, key);
    if (typeof amount === "number" && Number.isSafeInteger(amount) && amount > 0) {
      found = true;
      countPartialCause(target, key, amount);
    }
  }
  return found;
}
/** Inspect at most four cause objects; never inspect message, request or body. */
export function readFailureCause(stage: "ownership" | "holds", error: unknown): PartialCause {
  const seen = new Set<unknown>();
  let timeout = false, transport = false;
  for (let i = 0; i < 4 && error !== null && (typeof error === "object" || typeof error === "function") && !seen.has(error); i++) {
    seen.add(error);
    const status = get(error, "status");
    if (typeof status === "number" && Number.isInteger(status) && status >= 100 && status <= 599)
      return `${stage}_read_http_${Math.floor(status / 100)}xx` as PartialCause;
    const name = get(error, "name"), code = get(error, "code");
    const n = typeof name === "string" ? name.slice(0, 128) : "";
    const c = typeof code === "string" ? code.slice(0, 128) : "";
    timeout ||= n === "TimeoutError" || ["ETIMEDOUT", "ESOCKETTIMEDOUT", "UND_ERR_CONNECT_TIMEOUT", "UND_ERR_HEADERS_TIMEOUT", "UND_ERR_BODY_TIMEOUT"].includes(c);
    transport ||= ["TypeError", "FetchError", "NetworkError"].includes(n) ||
      ["ECONNRESET", "ECONNREFUSED", "EPIPE", "ENOTFOUND", "EAI_AGAIN", "ENETUNREACH", "EHOSTUNREACH", "UND_ERR_SOCKET", "UND_ERR_CONNECT"].includes(c);
    error = get(error, "cause");
  }
  return `${stage}_read_${timeout ? "timeout" : transport ? "transport" : "other"}`;
}
