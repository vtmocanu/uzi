// Neutral decode helpers for the runtime-reported lead model and effort (PRD #3244 M2).
//
// Both adapters build a RuntimeConfigObservation through these helpers, and the executors turn it
// into the reporter's stored vocabulary with toLeadObservation. Nothing here reads usage frames.

import type { ObservedEffort, RuntimeConfigObservation } from "./harness.js";
import type { LeadObservation } from "./lead-config-report.js";

/** Cap on a model token, in lockstep with the API's `agenttmpl.MaxModelLen` (bytes). */
const MAX_OBSERVED_MODEL_BYTES = 100;

/** Control (Cc), format (Cf: bidi overrides, zero-width), replacement and whitespace characters,
 *  the same rejection set as the API's `agenttmpl.ValidateModel`. */
const MODEL_REJECT_RE = /[\p{Cc}\p{Cf}\p{Cs}\uFFFD\s]/u;

const KNOWN_LEVELS: ReadonlySet<string> = new Set([
  "none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra",
]);

/** A runtime-reported model token that passes the stored-model rules, else undefined (the caller
 *  then reports `unknown`). The value is returned as given, never trimmed or rewritten. */
export function decodeObservedModel(raw: unknown): string | undefined {
  if (typeof raw !== "string" || raw.length === 0) return undefined;
  if (Buffer.byteLength(raw, "utf8") > MAX_OBSERVED_MODEL_BYTES) return undefined;
  if (MODEL_REJECT_RE.test(raw)) return undefined;
  return raw;
}

/** Map a runtime effort string to the stored set: a known level maps to itself, anything else
 *  (Codex `persistent`, a custom string) to `other`. Non-strings are not a level at all. */
export function mapObservedEffort(raw: unknown): ObservedEffort | undefined {
  if (typeof raw !== "string" || raw.length === 0) return undefined;
  return KNOWN_LEVELS.has(raw) ? (raw as ObservedEffort) : "other";
}

/** Claude effort: a known level maps to itself, any other non-empty string to `other` (the value is
 *  never echoed); non-strings are a shape mismatch. */
export function decodeClaudeEffort(raw: unknown): ObservedEffort | undefined {
  return mapObservedEffort(raw);
}

/** The reporter's wire form of an observation. `effort: null` with real evidence is "not sent". */
export function toLeadObservation(obs: RuntimeConfigObservation): LeadObservation {
  const model = decodeObservedModel(obs.model);
  return {
    model: model ?? null,
    modelEvidence: model === undefined ? "unknown" : obs.modelEvidence,
    effort: obs.effort ?? null,
    effortEvidence: obs.effortEvidence,
  };
}
