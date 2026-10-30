// PRD #3247 M2: bounded parser for the Codex `model/list` response. The response is untrusted
// app-server output, so every field is size-checked and anything malformed collapses to
// `undefined` (the caller then emits no note). Nothing here logs or echoes the response.

/** What uzi consumes from one catalog entry. */
export interface CodexCatalogEntry {
  readonly defaultEffort?: string;
  readonly supportedEfforts: readonly string[];
}

/** Request params for the single, unpaginated probe call. */
export const CATALOG_PROBE_LIMIT = 200;
/** A `data` array longer than this is treated as malformed (no pagination, no partial scan). */
const CATALOG_MAX_ENTRIES = 200;
const CATALOG_MAX_MODEL_BYTES = 100;
const CATALOG_MAX_EFFORTS = 16;
const CATALOG_MAX_EFFORT_BYTES = 16;

function boundedString(v: unknown, maxBytes: number): string | undefined {
  return typeof v === "string" && Buffer.byteLength(v, "utf8") <= maxBytes ? v : undefined;
}

/** Pinned 0.163.0 `ReasoningEffortOption` is `{ reasoningEffort, description }`; a bare string is
 *  tolerated too. Returns undefined for any other shape. */
function effortName(v: unknown): string | undefined {
  if (typeof v === "string") return boundedString(v, CATALOG_MAX_EFFORT_BYTES);
  if (v !== null && typeof v === "object" && !Array.isArray(v)) {
    return boundedString((v as Record<string, unknown>).reasoningEffort, CATALOG_MAX_EFFORT_BYTES);
  }
  return undefined;
}

/** Find the entry for `requested` in a `model/list` result, or undefined when the response is
 *  malformed, oversized, has no match, or the matching entry is malformed. */
export function parseCatalogEntry(response: unknown, requested: string): CodexCatalogEntry | undefined {
  if (response === null || typeof response !== "object" || Array.isArray(response)) return undefined;
  const data = (response as Record<string, unknown>).data;
  if (!Array.isArray(data) || data.length > CATALOG_MAX_ENTRIES) return undefined;
  for (const raw of data) {
    if (raw === null || typeof raw !== "object" || Array.isArray(raw)) continue;
    const rec = raw as Record<string, unknown>;
    const model = boundedString(rec.model, CATALOG_MAX_MODEL_BYTES);
    const id = boundedString(rec.id, CATALOG_MAX_MODEL_BYTES);
    if (model !== requested && id !== requested) continue;
    const list = rec.supportedReasoningEfforts;
    if (!Array.isArray(list) || list.length > CATALOG_MAX_EFFORTS) return undefined;
    const supportedEfforts: string[] = [];
    for (const item of list) {
      const name = effortName(item);
      if (name === undefined) return undefined;
      supportedEfforts.push(name);
    }
    const rawDefault = rec.defaultReasoningEffort;
    if (rawDefault === undefined || rawDefault === null) return { supportedEfforts };
    const defaultEffort = boundedString(rawDefault, CATALOG_MAX_EFFORT_BYTES);
    if (defaultEffort === undefined) return undefined;
    return { defaultEffort, supportedEfforts };
  }
  return undefined;
}
