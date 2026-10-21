import type { Run, RunMessage } from "./apiTypes";
import { isTerminalRun } from "./runStatus";
import { stripUnsafeChars } from "./safeText";

// Cross-check activity evidence (PRD #3166): the three `status` events a checker emits.
// The wording mirrors the CLI (api/cmd/uzi/check_activity.go) and the agent's
// formatActivityLine / formatActivitySummary so web, `run logs` and `run get` read the same.
// Every payload is untrusted worker output: fields are probed as typed values, op / outcome /
// category are matched against closed sets, and an unrecognised shape yields undefined so the
// caller keeps its generic `status: <event>` fallback.

const ACTIVITY_EVENT_START = "check_activity_start";
const ACTIVITY_EVENT = "check_activity";
const ACTIVITY_EVENT_SUMMARY = "check_activity_summary";

const OPS: ReadonlySet<string> = new Set(["read", "search"]);
const OUTCOMES = ["ok", "denied", "oversize", "limit", "error"] as const;
const OUTCOME_SET: ReadonlySet<string> = new Set(OUTCOMES);
const CATEGORY_LABELS: ReadonlyMap<string, string> = new Map([
  ["outside_checkout", "outside checkout"],
  ["tool_output", "tool output"],
  ["policy", "policy"],
]);

function hasOwn(rec: object, key: string): boolean {
  return Object.prototype.hasOwnProperty.call(rec, key);
}

function asRecord(v: unknown): Record<string, unknown> | undefined {
  return v && typeof v === "object" && !Array.isArray(v) ? (v as Record<string, unknown>) : undefined;
}

/** A whole, non-negative, exactly representable integer; anything else is undefined. */
function nonNegInt(v: unknown): number | undefined {
  return typeof v === "number" && Number.isInteger(v) && v >= 0 && v <= 2 ** 53 ? v : undefined;
}

function own(rec: Record<string, unknown>, key: string): unknown {
  return hasOwn(rec, key) ? rec[key] : undefined;
}

// stripUnsafeChars keeps \n and \t (legitimate in prose); a repository path never needs them.
function cleanPath(s: string): string {
  return stripUnsafeChars(s).replace(/[\n\t]/g, "");
}

function plural(n: number, one: string, many: string): string {
  return n === 1 ? one : many;
}

function activityLine(rec: Record<string, unknown>): string | undefined {
  const op = own(rec, "op");
  const outcome = own(rec, "outcome");
  if (typeof op !== "string" || !OPS.has(op)) return undefined;
  if (typeof outcome !== "string" || !OUTCOME_SET.has(outcome)) return undefined;
  const rawCategory = own(rec, "category");
  const category = typeof rawCategory === "string" ? CATEGORY_LABELS.get(rawCategory) ?? "" : "";
  const rawPath = own(rec, "path");
  const path = typeof rawPath === "string" ? cleanPath(rawPath) : "";
  if (outcome === "denied" && path === "") return category ? `${op} denied (${category})` : `${op} denied`;
  let subject: string = op;
  if (path !== "") subject += ` ${path}`;
  else if (category) subject += ` (${category})`;
  const lines = own(rec, "lines");
  if (Array.isArray(lines) && lines.length === 2) {
    const start = nonNegInt(lines[0]);
    const end = nonNegInt(lines[1]);
    if (start !== undefined && end !== undefined) subject += ` L${start}-${end}`;
  }
  const parts = [subject];
  const matches = nonNegInt(own(rec, "matches"));
  if (matches !== undefined) {
    const truncated = own(rec, "matches_truncated") === true;
    const word = truncated ? "matches" : matches === 1 ? "match" : "matches";
    parts.push(`${matches}${truncated ? "+" : ""} ${word}`);
  }
  parts.push(outcome);
  return parts.join(" · ");
}

function outcomeTotal(v: unknown): number {
  const counts = asRecord(v);
  if (!counts) return 0;
  let sum = 0;
  for (const key of OUTCOMES) sum += nonNegInt(own(counts, key)) ?? 0;
  return sum;
}

/** `4 reads, 0 searches, 0 dropped, complete`; undefined when `reads` / `searches` are absent. */
function summaryText(rec: Record<string, unknown>): string | undefined {
  if (!hasOwn(rec, "reads") || !hasOwn(rec, "searches")) return undefined;
  const reads = outcomeTotal(rec["reads"]);
  const searches = outcomeTotal(rec["searches"]);
  const dropped = nonNegInt(own(rec, "dropped")) ?? 0;
  const state = own(rec, "complete") === true ? "complete" : "incomplete";
  return `${reads} ${plural(reads, "read", "reads")}, ${searches} ${plural(searches, "search", "searches")}, ${dropped} dropped, ${state}`;
}

/**
 * The human line for a `status` payload carrying one of the activity events, or undefined for
 * every other payload and for an activity payload that does not validate.
 */
export function activityStatusLine(payload: unknown): string | undefined {
  const rec = asRecord(payload);
  if (!rec) return undefined;
  switch (own(rec, "event")) {
    case ACTIVITY_EVENT_START:
      return "activity capture started";
    case ACTIVITY_EVENT:
      return activityLine(rec);
    case ACTIVITY_EVENT_SUMMARY: {
      const text = summaryText(rec);
      return text === undefined ? undefined : `activity summary: ${text}`;
    }
    default:
      return undefined;
  }
}

/**
 * The single capture-state line for a cross-check run, or undefined when there is nothing to
 * say. G is the run's CURRENT claim generation: a marker or summary counts as current only when
 * its generation equals G, and an older (lower-generation) attempt's summary is shown only
 * labelled as that attempt's, and a higher generation (a re-claim between reads) is ignored. An absent G (older API) labels the newest summary historical.
 */
export function checkActivityState(
  run: Pick<Run, "status" | "claim_generation" | "first_started_at">,
  messages: readonly Pick<RunMessage, "kind" | "payload">[],
): string | undefined {
  const markers = new Set<number>();
  const summaries: { gen: number | undefined; text: string }[] = [];
  for (const m of messages) {
    if (m.kind !== "status") continue;
    const rec = asRecord(m.payload);
    if (!rec) continue;
    const event = own(rec, "event");
    const gen = nonNegInt(own(rec, "generation"));
    if (event === ACTIVITY_EVENT_START) {
      if (gen !== undefined) markers.add(gen);
    } else if (event === ACTIVITY_EVENT_SUMMARY) {
      const text = summaryText(rec);
      if (text !== undefined) summaries.push({ gen, text });
    }
  }
  const g = run.claim_generation;
  if (g === null || g === undefined) {
    const last = summaries[summaries.length - 1];
    return last ? `historical; current capture unknown: ${last.text}` : undefined;
  }
  for (let i = summaries.length - 1; i >= 0; i--) {
    if (summaries[i].gen === g) return summaries[i].text;
  }
  const terminal = isTerminalRun(run.status);
  const claimed = run.first_started_at != null;
  let current = "";
  if (markers.has(g)) current = terminal ? "activity capture incomplete" : "activity capture in progress";
  else if (terminal && claimed) current = "activity capture unavailable";
  let earlier = "";
  for (let i = summaries.length - 1; i >= 0; i--) {
    const s = summaries[i];
    if (s.gen !== undefined && s.gen < g) {
      earlier = `earlier attempt (generation ${s.gen}): ${s.text}`;
      break;
    }
  }
  return current && earlier ? `${current}; ${earlier}` : current || earlier || undefined;
}
