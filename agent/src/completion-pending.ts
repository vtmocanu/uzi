// PRD #1230 M4 (dormant): the server's `completion_pending` projection of frozen, human-authorized
// maintainer criteria. It is publication data, never evidence of completion: nothing here marks a
// criterion satisfied, and a decoded value only selects non-closing delivery and a rendered
// checklist. The bounds mirror api/internal/workersvc/semantic_completion_bounds.go; a violation is
// malformed (fail closed), never silently trimmed or treated as absent.
import { z } from "zod";

const utf8 = (s: string): boolean => Buffer.from(s).toString("utf8") === s;
const bounded = (max: number) => z.string().refine(
  s => utf8(s) && !s.includes("\u0000") && s.trim() !== "" && Buffer.byteLength(s) <= max);
const natural = z.number().int().safe().positive();

const MAX_PENDING_CRITERIA = 256;

const criterion = z.strictObject({
  criterion_id: z.string().uuid(),
  milestone_id: z.string().regex(/^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/),
  kind: z.literal("maintainer"),
  text: bounded(2048),
  reason: bounded(2048),
  conditions: z.array(bounded(512)).min(1).max(8),
  verification_steps: z.array(bounded(512)).min(1).max(8),
  authorization_revision: natural,
  status: z.literal("unverified"),
});

export const completionPendingSchema = z.strictObject({
  version: z.literal(1),
  contract_revision: natural,
  criteria: z.array(criterion).max(MAX_PENDING_CRITERIA),
}).refine(v => new Set(v.criteria.map(c => c.criterion_id)).size === v.criteria.length);

export type CompletionPending = z.infer<typeof completionPendingSchema>;

/** Decode one wire value. `undefined` is legacy absence; any other invalid shape is malformed. */
export function decodeCompletionPending(value: unknown):
  { kind: "absent" } | { kind: "ok"; pending: CompletionPending } | { kind: "malformed" } {
  if (value === undefined) return { kind: "absent" };
  const parsed = completionPendingSchema.safeParse(value);
  return parsed.success ? { kind: "ok", pending: parsed.data } : { kind: "malformed" };
}

const canonical = (p: CompletionPending): string => JSON.stringify([
  p.version, p.contract_revision,
  [...p.criteria].sort((a, b) => a.criterion_id < b.criterion_id ? -1 : a.criterion_id > b.criterion_id ? 1 : 0)
    .map(c => [c.criterion_id, c.milestone_id, c.kind, c.text, c.reason, c.conditions, c.verification_steps,
      c.authorization_revision, c.status]),
]);

/** Presence on one side only is a mismatch. Criteria compare as a set (sorted by id) so server
 * ordering cannot matter, but every identity, revision and displayed field must agree. */
export function sameCompletionPending(a: CompletionPending | undefined, b: CompletionPending | undefined): boolean {
  if (a === undefined || b === undefined) return a === b;
  return canonical(a) === canonical(b);
}
