import { z } from "zod";
import { parseSemanticCandidate, semanticSourceCaptureSchema } from "./semantic-plan.js";
const utf8 = (s: string) => Buffer.from(s).toString("utf8") === s;
type Candidate = ReturnType<typeof parseSemanticCandidate>;
type Source = z.infer<typeof semanticSourceCaptureSchema>;
const explanation = z.string().min(1).refine(s => utf8(s) && Buffer.byteLength(s) <= 2048);
const citation = z.strictObject({
  source_id: z.string(), hash: z.string(),
  start: z.number().int().safe().nonnegative(), end: z.number().int().safe().positive(),
});
export const coverageReportSchema = z.strictObject({
  candidate_digest: z.string(),
  source_digest: z.string().regex(/^[0-9a-f]{64}$/),
  criteria_digest: z.string().regex(/^[0-9a-f]{64}$/),
  requirements: z.array(z.strictObject({
    requirement_id: z.string().regex(/^r[1-9][0-9]{0,5}$/),
    status: z.enum(["represented", "missing", "uncertain"]),
    criterion_ids: z.array(z.string()).max(8),
    citations: z.array(citation).min(1).max(8),
    explanation,
  })).min(1).max(512),
  criteria: z.array(z.strictObject({
    criterion_id: z.string(), atomic: z.boolean(), explanation,
  })).min(1).max(256),
});
export type Report = z.infer<typeof coverageReportSchema>;

// Strict JSON: reject duplicate decoded keys and pathological nesting before schema parsing.
function decodeReport(raw: string): unknown {
  if (!utf8(raw) || Buffer.byteLength(raw) > 65536) throw new Error("Report size");
  const value: unknown = JSON.parse(raw);
  const stack: ({ keys: Set<string>; key: boolean } | null)[] = [];
  for (const token of raw.match(/"(?:[^"\\]|\\.)*"|[{}[\]:,]|[^\s{}[\]:,]+/g) ?? []) {
    const top = stack.at(-1);
    if (token === "{" || token === "[") {
      if (stack.length >= 32) throw new Error("Report depth");
      stack.push(token === "{" ? { keys: new Set(), key: true } : null);
    } else if (token === "}" || token === "]") stack.pop();
    else if (token === "," && top) top.key = true;
    else if (token.startsWith('"') && top?.key) {
      const key = JSON.parse(token) as string;
      if (top.keys.has(key)) throw new Error("Duplicate report key");
      top.keys.add(key);
      top.key = false;
    }
  }
  return value;
}
export function validateCoverageReport(raw: string, candidate: Candidate, sources: Source[], binding: { source_digest: string; criteria_digest: string }): Report {
  const report = coverageReportSchema.parse(decodeReport(raw));
  if (report.candidate_digest !== candidate.digest || report.source_digest !== binding.source_digest ||
      report.criteria_digest !== binding.criteria_digest) throw new Error("Foreign candidate");
  const criterionIds = new Set(candidate.criteria.map(c => c.id));
  const assessed = report.criteria.map(c => c.criterion_id);
  if (new Set(assessed).size !== assessed.length || assessed.length !== criterionIds.size ||
      assessed.some(id => !criterionIds.has(id))) throw new Error("Foreign or omitted criterion");
  const requirementIds = new Set<string>();
  const citedSources = new Set<string>();
  for (const requirement of report.requirements) {
    if (requirementIds.has(requirement.requirement_id)) throw new Error("Duplicate requirement");
    requirementIds.add(requirement.requirement_id);
    if (new Set(requirement.criterion_ids).size !== requirement.criterion_ids.length ||
        requirement.criterion_ids.some(id => !criterionIds.has(id)) ||
        (requirement.status === "represented" && !requirement.criterion_ids.length)) throw new Error("Invalid criterion link");
    for (const cite of requirement.citations) {
      const source = sources.find(s => s.id === cite.source_id);
      const ref = candidate.sources.find(s => s.id === cite.source_id);
      if (!source || !ref || cite.hash !== ref.hash) throw new Error("Foreign source");
      const bytes = Buffer.from(source.bytes!, "base64");
      if (cite.end <= cite.start || cite.end > bytes.length) throw new Error("Invalid range");
      const decoder = new TextDecoder("utf-8", { fatal: true });
      decoder.decode(bytes.subarray(0, cite.start));
      decoder.decode(bytes.subarray(0, cite.end));
      citedSources.add(source.id);
    }
  }
  if (sources.some(s => !citedSources.has(s.id))) throw new Error("Omitted source");
  return report;
}

