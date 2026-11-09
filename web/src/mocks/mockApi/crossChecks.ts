import type { CrossCheckHistoryAttemptDTO as Attempt, CrossCheckHistoryDTO, CrossCheckHistoryGroupDTO as Group, CrossCheckHistoryParams as Params } from "../../lib/apiTypes";
import { ApiError } from "../../lib/apiError";
import { delay, mockScenario, requireSession } from "./shared";

function attempt(stage: string, round: number, active = false): Attempt {
  return {
    stage, round, active, result: active ? "pending" : "failed", review: "none",
    execution: active ? "running" : "couldnt_start", reason: active ? "Cause not recorded for this attempt" : "Worker does not support this check",
    findings_count: null, created_at: "2026-10-30T10:00:00Z", decided_at: null,
    interrupted_at: null, deadline_at: "2026-10-30T11:00:00Z", child_id: null, child_status: null,
    child_context: "No checker run available", model: null, effort: null, historical: false,
    effect: "Effect not recorded", next_action: "No action recorded",
  };
}
function executed(a: Attempt, parent: number, review = "none", findings: number | null = null): Attempt {
  return { ...a, execution: a.active ? "running" : "finished",
    result: a.active ? "pending" : a.stage === "code" ? "completed" : review === "approved" ? "approve" : "revise",
    review, findings_count: findings,
    reason: review === "approved" ? "Review approved" : review === "changes_requested" ? "Changes requested" : "Cause not recorded for this attempt",
    child_id: `10000000-0000-4000-8000-${String(parent * 100 + (a.stage === "code" ? 50 : 0) + a.round).padStart(12, "0")}`,
    child_status: a.active ? "running" : "completed", child_context: "Checker run available",
    model: "gpt-5.4", effort: "high", decided_at: a.active ? null : "2026-10-30T10:30:00Z" };
}
function groups(): Group[] {
  if (!mockScenario().startsWith("cross-check")) return [];
  // Enough groups and rounds to exercise real bounded pagination, both stages,
  // and an older active round outside the first attempt window.
  return Array.from({ length: 29 }, (_, i) => {
    const active = i < 2;
    // Retained legacy rounds exercise history windows, not the current round policy.
    const retainedPlan = 31;
    const attempts = Array.from({ length: retainedPlan }, (_, n) => {
      const a = attempt("plan", n + 1, active && (n === 0 || n === retainedPlan - 1));
      return a.active ? executed(a, i + 1) : a;
    });
    // An unstored approval keeps its review but is replaced, as in the API.
    attempts[2] = { ...attempts[2], execution: "replaced", review: "approved", reason: "Approval was not stored", historical: true };
    // A Plan verdict recorded before custody loss keeps its verdict and stays historical (#4037).
    attempts[3] = { ...executed(attempts[3], i + 1, "approved"), interrupted_at: "2026-10-30T10:45:00Z", historical: true };
    attempts[4] = executed(attempts[4], i + 1, "changes_requested");
    attempts[5] = { ...attempts[5], execution: "unknown", review: "unknown" };
    attempts.push(executed(attempt("code", 1, active), i + 1,
      active ? "none" : i % 2 === 0 ? "approved" : "changes_requested", active ? null : i % 2 === 0 ? 0 : 2));
    return {
      parent_id: `00000000-0000-4000-8000-${String(i + 1).padStart(12, "0")}`,
      parent_title: `Synthetic cross-check work run ${i + 1}`, parent_status: active ? "running" : "completed",
      latest_at: "2026-10-30T10:00:00Z", active, total_attempts: attempts.length,
      matching_attempts: attempts.length, active_attempts: attempts.filter(a => a.active).length,
      active_hidden: 0, attempts, refresh_needed: false,
    };
  }).sort((a, b) => b.latest_at.localeCompare(a.latest_at) || b.parent_id.localeCompare(a.parent_id));
}
function matches(a: Attempt, p: Params) {
  if (p.stage && p.stage !== "all" && p.stage !== a.stage) return false;
  if (!p.outcome || p.outcome === "any") return true;
  if (p.outcome === "didnt_finish") return ["didnt_finish", "couldnt_start"].includes(a.execution);
  // Replacement takes precedence over a recorded review, mirroring the API's classification.
  if (["replaced", "interrupted"].includes(a.execution)) return p.outcome === "replaced";
  if (p.outcome === "replaced") return false;
  if (p.outcome === "passed") return a.review === "approved";
  return ["changes_requested", "blocked"].includes(a.review);
}
function slice(g: Group, p: Params, end?: number): Group {
  const filtered = g.attempts.filter(a => matches(a, p));
  const boundary = end ?? filtered.length;
  const start = Math.max(0, boundary - Math.min(20, Math.max(2, p.attempt_limit ?? 10)));
  const attempts = filtered.slice(start, boundary);
  const hidden = g.active_attempts - attempts.filter(a => a.active).length;
  return { ...g, attempts, matching_attempts: filtered.length, active_hidden: hidden,
    active_context: hidden ? "Additional active attempts are filtered or outside the loaded window." : undefined,
    older_cursor: start ? `${g.parent_id}:${start}` : undefined,
    omission: start ? "Earlier rounds omitted: load older attempts" : undefined };
}
export const crossChecksApi = {
  crossCheckHistoryAvailability: async () => {
    requireSession();
    return delay({ available: groups().length > 0 });
  },
  listCrossCheckHistory: async (p: Params = {}): Promise<CrossCheckHistoryDTO> => {
    requireSession();
    const all = groups().filter(g => g.active === (p.lifecycle !== "past"));
    if (p.attempt_cursor?.length) {
      if (p.cursor || p.attempt_cursor.length > 8) throw new ApiError(400, "Invalid cursor");
      return delay({ groups: p.attempt_cursor.map(cursor => {
        const [id, end] = cursor.split(":");
        const g = all.find(g => g.parent_id === id);
        if (!g || !/^\d+$/.test(end)) throw new ApiError(400, "Invalid cursor");
        return slice(g, p, Number(end));
      }) });
    }
    const filtered = all.filter(g => g.attempts.some(a => matches(a, p)));
    const start = Number(p.cursor ?? 0);
    const limit = Math.min(25, Math.max(1, p.limit ?? 25));
    return delay({
      groups: filtered.slice(start, start + limit).map(g => slice(g, p)),
      ...(start + limit < filtered.length ? { next_cursor: String(start + limit) } : {}),
    });
  },
};
