// PRD #1230 D5: source drift observation. A detector only: it reads each frozen PRD source at an
// exact commit through the trusted bare, reports a hash or an explicit unknown reason, and records
// the server's stored comparison. Nothing here holds a run, revises a contract, gates a permit or
// marks a criterion satisfied; a caller treats every failure as "no observation".
import { createHash } from "node:crypto";
import { z } from "zod";
import { validatePrdPath, type BarePrdReader } from "./prd-link.js";
import { SEMANTIC_PRD_MAX_BYTES } from "./semantic-plan.js";

const hex64 = z.string().regex(/^[0-9a-f]{64}$/);
const outcome = z.enum(["unchanged", "changed", "unknown"]);
const reason = z.enum(["missing", "truncated", "unreadable", "denied", "invalid", "timeout"]);
export type DriftReason = z.infer<typeof reason>;
const id = z.string().min(1).max(512);
const path = z.string().min(1).max(1024);

const driftSource = z.strictObject({
  source_id: id, frozen_hash: hex64, observed_hash: hex64.optional(),
  ref: z.string().regex(/^(?:[0-9a-f]{40}|[0-9a-f]{64})$/).optional(),
  outcome, reason: reason.optional(), checkbox_only: z.boolean().optional(),
});
/** The server's stored record (apitypes.SemanticDriftObservation), strict and bounded. */
export const semanticObservationRecordSchema = z.strictObject({
  phase: z.enum(["presentation", "publication"]),
  candidate_digest: hex64.optional(),
  gate_revision: z.number().int().safe().positive().optional(),
  pushed_sha: z.string().regex(/^(?:[0-9a-f]{40}|[0-9a-f]{64})$/).optional(),
  observed_at: z.string().datetime({ offset: true }),
  issue: driftSource,
  prds: z.array(driftSource).max(8),
  links_added: z.array(path).max(8),
  links_removed: z.array(path).max(8),
  links_truncated: z.boolean().optional(),
});
export type SemanticObservationRecord = z.infer<typeof semanticObservationRecordSchema>;
export const SEMANTIC_OBSERVATION_RESPONSE_MAX_BYTES = 64 * 1024;

export interface ObservedPrdSource {
  source_id: string; observed_hash?: string; ref: string;
  outcome: "unchanged" | "changed" | "unknown"; reason?: DriftReason;
}
export type SemanticObservationRequest =
  | { phase: "presentation"; candidate_digest: string; gate_revision: number; prd_sources: ObservedPrdSource[] }
  | { phase: "publication"; pushed_sha: string; prd_sources: ObservedPrdSource[] };

/** The slice of a frozen candidate source the observation needs. */
export interface FrozenSource { id: string; kind: string; hash: string; completeness: string }

const TIMEOUT_RE = /\bexceeded \d+ms\b|timed out/i;
const MISSING_RE = /does not exist in|exists on disk, but not in|unknown revision or path|not in the working tree|Path '.*' does not exist/i;

function readFailure(error: unknown): DriftReason {
  const message = error instanceof Error ? error.message : String(error);
  if (TIMEOUT_RE.test(message)) return "timeout";
  if (MISSING_RE.test(message)) return "missing";
  return "unreadable";
}

/**
 * Report every frozen PRD source of the candidate, read at `sha` from the bare (never the writable
 * worktree, a branch tip or claim-time text). Sequential reads share one deadline; a failed read
 * marks only its own source unknown. An incomplete frozen baseline is always unknown: the read only
 * refines the reason. The result never carries source text.
 */
export async function observePrdSources(
  reader: BarePrdReader,
  input: { barePath: string; sha: string; sources: readonly FrozenSource[]; timeoutMs: number; signal?: AbortSignal },
): Promise<ObservedPrdSource[]> {
  const deadline = Date.now() + input.timeoutMs;
  const out: ObservedPrdSource[] = [];
  for (const source of input.sources) {
    if (source.kind !== "prd") continue;
    const unknown = (r: DriftReason): ObservedPrdSource => ({ source_id: source.id, ref: input.sha, outcome: "unknown", reason: r });
    const file = source.id.startsWith("prd:") && !source.id.startsWith("prd:unknown:") ? source.id.slice(4) : "";
    if (!validatePrdPath(file)) { out.push(unknown("invalid")); continue; }
    const remaining = deadline - Date.now();
    if (remaining <= 0 || input.signal?.aborted) { out.push(unknown(remaining <= 0 ? "timeout" : "unreadable")); continue; }
    try {
      const read = await reader.readBare(input.barePath, ["show", input.sha + ":" + file], {
        maxBytes: SEMANTIC_PRD_MAX_BYTES, timeoutMs: remaining, signal: input.signal, includeBytes: true,
      });
      if (read.truncated) out.push(unknown("truncated"));
      else if (read.bytes === undefined || read.bytes.length > SEMANTIC_PRD_MAX_BYTES) out.push(unknown("unreadable"));
      else if (source.completeness !== "complete") out.push(unknown("invalid"));
      else {
        const observed = createHash("sha256").update(read.bytes).digest("hex");
        out.push({ source_id: source.id, observed_hash: observed, ref: input.sha,
          outcome: observed === source.hash ? "unchanged" : "changed" });
      }
    } catch (error) { out.push(unknown(readFailure(error))); }
  }
  return out;
}

/** True when an observation reports anything but a clean match: a changed or unknown source, or a link
 * added to or removed from the issue. This only selects a disclosure; it never satisfies or fails anything. */
export function hasDriftContradiction(record: SemanticObservationRecord): boolean {
  return [record.issue, ...record.prds].some(s => s.outcome !== "unchanged") ||
    record.links_added.length > 0 || record.links_removed.length > 0;
}
