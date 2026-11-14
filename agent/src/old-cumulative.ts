import type { Logger } from "./log.js";

/** The per-model counters of a result frame's `modelUsage` that the in-place credential switch
 *  records as the old credential's last known cumulative reading. */
const FIELDS = ["inputTokens", "outputTokens", "cacheReadInputTokens", "cacheCreationInputTokens"] as const;

/** Mirror of the api's validateOldCumulative limits (credential_switch_apply.go): at most 64
 *  entries, keys of at most 200 BYTES. The shared fixture old-cumulative-boundaries.json is
 *  checked against the server on both sides. */
const MAX_ENTRIES = 64;
const MAX_KEY_BYTES = 200;

/**
 * Derives the `old_cumulative` map sent with a credential-switch apply from a result frame's
 * `modelUsage` ("<model>.<field>" -> token count). Informational only: it decides nothing.
 * A value survives only as a non-negative safe integer; a key only within 200 UTF-8 bytes and
 * free of C0 control characters and DEL (a longer or control-bearing key is skipped, never truncated). Keys are sorted and the first 64 kept. Anything
 * dropped is warned about by count, never by value. An empty result is undefined.
 */
export function oldCumulativeFromModelUsage(modelUsage: unknown, log?: Logger): Record<string, number> | undefined {
  if (typeof modelUsage !== "object" || modelUsage === null || Array.isArray(modelUsage)) return undefined;
  const entries = new Map<string, number>();
  let badValues = 0;
  let longKeys = 0; // keys skipped for length or a control character
  for (const [model, perModel] of Object.entries(modelUsage as Record<string, unknown>)) {
    if (typeof perModel !== "object" || perModel === null || Array.isArray(perModel)) continue;
    for (const field of FIELDS) {
      const v = (perModel as Record<string, unknown>)[field];
      if (v === undefined) continue;
      if (typeof v !== "number" || !Number.isSafeInteger(v) || v < 0) {
        badValues += 1;
        continue;
      }
      const key = `${model}.${field}`;
      // A C0 control character or DEL: NUL cannot be stored in jsonb, and the others expand to \u00XX on the wire.
      // eslint-disable-next-line no-control-regex
      if (Buffer.byteLength(key, "utf8") > MAX_KEY_BYTES || /[\u0000-\u001f\u007f]/.test(key)) {
        longKeys += 1;
        continue;
      }
      entries.set(key, v);
    }
  }
  const sorted = [...entries.keys()].sort();
  const overCap = Math.max(0, sorted.length - MAX_ENTRIES);
  if (badValues + longKeys + overCap > 0) {
    log?.warn("old_cumulative: some modelUsage readings were dropped", {
      invalid_values: badValues,
      oversize_keys: longKeys,
      over_entry_cap: overCap,
    });
  }
  if (sorted.length === 0) return undefined;
  const out: Record<string, number> = {};
  for (const k of sorted.slice(0, MAX_ENTRIES)) out[k] = entries.get(k) as number;
  return out;
}
