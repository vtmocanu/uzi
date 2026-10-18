// Dormant M1 wire/capture helpers. No production enrollment, claim or tool caller.
import { createHash } from "node:crypto";
import { z } from "zod";
import { findSemanticPrdPaths, validatePrdPath } from "./prd-link.js";
import type { BarePrdReader } from "./prd-link.js";

const KiB = 1024;
const limits = { issue: 64 * KiB, prd: 256 * KiB, plan: 256 * KiB, total: 576 * KiB };
const validString = (s: string): boolean => Buffer.from(s).toString("utf8") === s;
const text = (max: number) => z.string().refine(
  s => validString(s) && /[^\p{White_Space}]/u.test(s) && Buffer.byteLength(s) <= max,
  { message: "Expected nonempty bounded UTF-8 text" },
);
const identifier = (max: number) => text(max).refine(
  s => !/[\p{Cc}\p{Cf}\u2028\u2029]/u.test(s),
  { message: "Unsafe identifier" },
);
const hash = z.string().regex(/^[0-9a-f]{64}$/);
const integer = z.number().int().safe();
const completeness = z.enum(["complete", "unknown"]);
const sourceFields = {
  id: identifier(512), kind: z.enum(["issue", "prd"]),
  provenance: identifier(2048), completeness,
};
const base64 = z.string().refine(s => Buffer.from(s, "base64").toString("base64") === s, "Invalid base64");

export const semanticSourceCaptureSchema = z.strictObject({ ...sourceFields, bytes: base64.nullable() })
  .superRefine((source, ctx) => {
    const bytes = Buffer.from(source.bytes ?? "", "base64");
    if (bytes.length > limits[source.kind] || !isUtf8(bytes)) {
      ctx.addIssue({ code: "custom", message: "Invalid source bytes" });
    }
  });
const sourceRef = z.strictObject({ ...sourceFields, hash });
const citation = z.strictObject({
  source_id: identifier(512), hash, start: integer.nonnegative(), end: integer.positive(),
}).refine(c => c.end > c.start, "Invalid citation range");
const evidence = z.strictObject({
  description: text(512), path: z.union([z.literal(""), identifier(512)]).optional(),
});
const maintainer = z.strictObject({
  reason: text(2048), conditions: z.array(text(512)).min(1).max(8),
  verification_steps: z.array(text(512)).min(1).max(8),
});
const criterionFields = {
  milestone_id: identifier(512), kind: z.enum(["code", "doc", "test", "maintainer"]),
  text: text(2048), done_when: text(2048),
  citations: z.array(citation).min(1).max(8),
  evidence_requirements: z.array(evidence).min(1).max(8),
  maintainer: maintainer.nullable().optional(),
};
function checkMaintainer(c: { kind: string; maintainer?: unknown }): boolean {
  return c.kind === "maintainer" ? c.maintainer != null : c.maintainer == null;
}
const proposedCriterion = z.strictObject(criterionFields).refine(checkMaintainer, "Invalid maintainer metadata");
const canonicalCriterion = z.strictObject({
  id: z.string().regex(/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/),
  ...criterionFields,
}).refine(checkMaintainer, "Invalid maintainer metadata");

function checkCounts(criteria: { milestone_id: string }[]): boolean {
  const counts = new Map<string, number>();
  for (const c of criteria) {
    const n = (counts.get(c.milestone_id) ?? 0) + 1;
    if (n > 32) return false;
    counts.set(c.milestone_id, n);
  }
  return true;
}
const semanticProposalSchema = z.strictObject({
  criteria: z.array(proposedCriterion).min(1).max(256).refine(checkCounts, "Too many criteria per milestone"),
});

// Policy/coverage are DTO placeholders; M1 does not invent M2 policy ceilings or outcomes.
const semanticEnrollmentSchema = z.strictObject({
  profile: z.literal("semantic"),
  policy: z.strictObject({
    version: z.literal(1), model: z.string(), max_reviews: integer,
    max_invocations: integer, max_tokens: integer, max_seconds: integer,
  }),
});
const coverage = z.strictObject({
  outcome: z.string(), candidate_digest: z.string(), reviews: integer,
  invocations: integer, tokens: integer, seconds: integer,
});
const semanticCandidateSchema = z.strictObject({
  candidate_identity_version: z.literal("semantic_v1"), revision: integer.positive(),
  digest: hash, sources: z.array(sourceRef).min(1).max(9), plan_hash: hash,
  source_outcome: completeness,
  criteria: z.array(canonicalCriterion).min(1).max(256).refine(checkCounts, "Too many criteria per milestone"),
  coverage: coverage.nullable().optional(),
}).superRefine((candidate, ctx) => {
  const ids = candidate.criteria.map(c => c.id);
  const sourceIds = candidate.sources.map(s => s.id);
  if (new Set(ids).size !== ids.length || new Set(sourceIds).size !== sourceIds.length ||
      candidate.sources.filter(s => s.kind === "issue").length !== 1 ||
      candidate.sources.filter(s => s.kind === "prd").length > 8 ||
      candidate.source_outcome !== (candidate.sources.some(s => s.completeness === "unknown") ? "unknown" : "complete")) {
    ctx.addIssue({ code: "custom", message: "Invalid candidate source/criterion identities" });
  }
  const sources = new Map(candidate.sources.map(s => [s.id, s]));
  for (const c of candidate.criteria) {
    for (const cite of c.citations) {
      const source = sources.get(cite.source_id);
      if (!source || source.hash !== cite.hash) {
        ctx.addIssue({ code: "custom", message: "Foreign citation" });
      }
    }
  }
});

/** Decode JSON without JSON.parse's silent duplicate-key overwrite. JSON.parse
 * validates syntax; the token walk checks decoded keys, including escaped aliases.
 * Input byte size and nesting bound both work and stack depth.
 */
function wireValue(input: unknown): unknown {
  if (typeof input !== "string" && !(input instanceof Uint8Array)) return input;
  const raw = typeof input === "string" ? input : new TextDecoder("utf-8", { fatal: true }).decode(input);
  if (!validString(raw) || Buffer.byteLength(raw) > 16 * 1024 * KiB) throw new Error("Invalid wire JSON");
  const value: unknown = JSON.parse(raw);
  const stack: ({ keys: Set<string>; expectingKey: boolean } | null)[] = [];
  // A syntax-aware walk below uses JSON tokens and separators, not object values.
  const lexical = raw.match(/"(?:[^"\\]|\\.)*"|[{}[\]:,]|[^\s{}[\]:,]+/g) ?? [];
  for (const token of lexical) {
    const top = stack.at(-1);
    if (token === "{" || token === "[") {
      if (stack.length >= 64) throw new Error("Wire nesting limit");
      stack.push(token === "{" ? { keys: new Set(), expectingKey: true } : null);
    } else if (token === "}" || token === "]") stack.pop();
    else if (token === "," && top) top.expectingKey = true;
    else if (token.startsWith('"') && top?.expectingKey) {
      const key = JSON.parse(token) as string;
      if (top.keys.has(key)) throw new Error("Duplicate JSON key");
      top.keys.add(key);
      top.expectingKey = false;
    }
  }
  return value;
}

// Optional trusted context validates what the wire alone cannot establish.
// This is validation only: the API remains the compiler and identity authority.
type SourceContext = { sources: readonly unknown[]; milestoneIds?: readonly string[] };
function checkContext(
  criteria: z.infer<typeof proposedCriterion>[], context: SourceContext,
) {
  const captures = context.sources.map(s => semanticSourceCaptureSchema.parse(s));
  const sources = new Map(captures.map(s => [s.id, s]));
  if (sources.size !== captures.length) throw new Error("Duplicate source identity");
  for (const criterion of criteria) {
    if (context.milestoneIds && !context.milestoneIds.includes(criterion.milestone_id)) {
      throw new Error("Foreign milestone");
    }
    for (const cite of criterion.citations) {
      const source = sources.get(cite.source_id);
      if (!source || source.completeness !== "complete") throw new Error("Incomplete or foreign source");
      const bytes = Buffer.from(source.bytes ?? "", "base64");
      if (cite.hash !== createHash("sha256").update(bytes).digest("hex") ||
          cite.end > bytes.length || !isUtf8(bytes.subarray(0, cite.start)) ||
          !isUtf8(bytes.subarray(0, cite.end))) {
        throw new Error("Citation does not match complete UTF-8 bytes");
      }
    }
  }
}
export function parseSemanticProposal(input: unknown, context?: SourceContext) {
  const proposal = semanticProposalSchema.parse(wireValue(input));
  if (context) checkContext(proposal.criteria, context);
  return proposal;
}
export function parseSemanticCandidate(input: unknown, context?: SourceContext) {
  const candidate = semanticCandidateSchema.parse(wireValue(input));
  if (context) checkContext(candidate.criteria, context);
  return candidate;
}
export function parseSemanticEnrollment(input: unknown) {
  return semanticEnrollmentSchema.parse(wireValue(input));
}

function isUtf8(bytes: Uint8Array): boolean {
  try { new TextDecoder("utf-8", { fatal: true }).decode(bytes); return true; }
  catch { return false; }
}
function capture(id: string, kind: "issue" | "prd", provenance: string, raw?: Uint8Array, fallbackId = kind + ":unknown") {
  const safe = identifier(512).safeParse(id).success && identifier(2048).safeParse(provenance).success;
  const complete = safe && raw !== undefined && raw.length <= limits[kind] && isUtf8(raw);
  const source = {
    id: safe ? id : fallbackId, kind,
    provenance: safe ? provenance : "worker:unknown",
    completeness: complete ? "complete" as const : "unknown" as const,
    bytes: complete ? Buffer.from(raw).toString("base64") : "",
  };
  semanticSourceCaptureSchema.parse(source);
  return source;
}

/** Trusted worker input only. raw issue/plan bytes must come from pinned server
 * input, never lead prose or an agent worktree. Hashes are computed by the server.
 * At most eight sequential reads, each with a finite timeout and one shared
 * deadline. A read failure marks that source unknown and does not block siblings.
 *
 * BarePrdReader's legacy text result cannot prove byte fidelity (StringDecoder
 * can discard invalid trailing bytes). Only a reader supplying raw bytes can
 * produce complete PRD captures; text-only readers fail closed.
 */
export async function captureSemanticSources(
  reader: BarePrdReader,
  input: {
    barePath: string; repo: string; headSha: string;
    issue: { id: string; bytes?: Uint8Array };
    planBytes: Uint8Array;
    timeoutMs: number; signal?: AbortSignal;
  },
) {
  const issue = capture(input.issue.id, "issue", input.repo + ":" + input.headSha + ":issue:" + input.issue.id, input.issue.bytes);
  const sources = [issue];
  const paths = issue.completeness === "complete"
    ? findSemanticPrdPaths(Buffer.from(issue.bytes, "base64").toString("utf8")) : [];
  let total = Buffer.from(issue.bytes, "base64").length + input.planBytes.length;
  const validPlan = input.planBytes.length <= limits.plan && isUtf8(input.planBytes);
  const safeRef = /^(?:[0-9a-f]{40}|[0-9a-f]{64})$/i.test(input.headSha);
  const safeRepo = identifier(1024).safeParse(input.repo).success;
  const safeTimeout = Number.isFinite(input.timeoutMs) && input.timeoutMs > 0;
  const deadline = Date.now() + (safeTimeout ? input.timeoutMs : 0);
  let outcome: "complete" | "unknown" = issue.completeness;
  if (!validPlan || !safeRef || !safeRepo || !safeTimeout || paths.length > 8 || total > limits.total) outcome = "unknown";
  // Overflow cannot return a passing prefix: retain bounded identities but clear
  // every captured source when the requested source set cannot fit.
  for (const path of paths.slice(0, 8)) {
    let raw: Uint8Array | undefined;
    const remaining = deadline - Date.now();
    if (paths.length <= 8 && validPlan && safeRef && safeRepo && safeTimeout && remaining > 0 && !input.signal?.aborted && validatePrdPath(path)) {
      try {
        const read = await reader.readBare(input.barePath, ["show", input.headSha + ":" + path], {
          maxBytes: limits.prd, timeoutMs: remaining, signal: input.signal, includeBytes: true,
        });
        if (!read.truncated && read.bytes !== undefined && isUtf8(read.bytes) &&
            read.bytes.length <= limits.prd && total + read.bytes.length <= limits.total) {
          raw = read.bytes;
          total += raw.length;
        }
      } catch { /* Unknown is explicit; no retry and later sources still run. */ }
    }
    const source = capture("prd:" + path, "prd", input.repo + ":" + input.headSha + ":" + path, raw, "prd:unknown:" + sources.length);
    if (source.completeness === "unknown") outcome = "unknown";
    sources.push(source);
  }
  if (paths.length > 8 || !validPlan || total > limits.total || !safeRef || !safeRepo || !safeTimeout) {
    for (const source of sources) { source.completeness = "unknown"; source.bytes = ""; }
  }
  return { sources, source_outcome: outcome };
}
