import type { AdminUsage, HarnessRuns, RunUsage } from "./api";
import { aggregateDisclosure } from "./costStatus";
import type { UsageWindow } from "../components/UsageCards";

// The per-user rows and the factory total row of the admin usage breakdown, for one window.
// Shared by the current Overview's PerUserUsageTable and the preview's By user table, so the
// two cannot disagree about sorting, the window's source fields or the PR-count rule.

// A RunUsage bundle split the way the run view splits it: fresh = fresh input +
// cache creation, cached = cache reads, out = output; total = all three.
export function breakdown(u: RunUsage): { fresh: number; cached: number; out: number; total: number; cost: number } {
  const fresh = u.input_tokens + u.cache_creation_tokens;
  const cached = u.cache_read_tokens;
  const out = u.output_tokens;
  return { fresh, cached, out, total: fresh + cached + out, cost: u.cost_usd };
}

// Issue #3876: a user's Claude / Codex run shares as whole percentages summing to 100 (Codex
// takes the rounding remainder); null when the split is absent or the window has no runs.
export function harnessShare(h: HarnessRuns | undefined): { claude: number; codex: number } | null {
  if (!h) return null;
  const total = h.claude + h.codex;
  if (total === 0) return null;
  const claude = Math.round((100 * h.claude) / total);
  return { claude, codex: 100 - claude };
}

export function perUserRows(admin: AdminUsage, window: UsageWindow) {
  const recent = window === "last_7_days";
  const available = admin.users.every((u) =>
    u.last_7_days != null && u.last7_run_count != null && u.last7_outcomes != null &&
    u.last7_subscription_run_count != null && u.last7_unreported_run_count != null,
  );
  const factory = breakdown(admin.factory[window]);
  const factoryOutcomes = admin.factory.outcomes[window];
  const factoryDisclosure = aggregateDisclosure(
    recent ? admin.factory.last7_subscription_run_count : admin.factory.lifetime_subscription_run_count,
    recent ? admin.factory.last7_unreported_run_count : admin.factory.lifetime_unreported_run_count,
  );
  // An older API without the seven-day fields: the caller shows its unavailable message.
  // No row may dereference the missing fields.
  const usable = !recent || available;
  const rows = (usable ? admin.users : []).map((u) => ({
    ...u,
    b: breakdown(recent ? u.last_7_days! : u.usage),
    selectedRuns: recent ? u.last7_run_count! : u.run_count,
    selectedOutcomes: recent ? u.last7_outcomes! : u.outcomes,
    subscription: recent ? u.last7_subscription_run_count! : u.subscription_run_count,
    unreported: recent ? u.last7_unreported_run_count! : u.unreported_run_count,
    selectedPRs: recent ? u.last7_pr_count : u.pr_count,
    selectedHarness: recent ? u.last7_harness_runs : u.harness_runs,
  })).sort((a, b) => b.b.cost - a.b.cost || b.b.total - a.b.total || b.b.out - a.b.out ||
    (a.user_id < b.user_id ? -1 : a.user_id > b.user_id ? 1 : 0));
  // Issue #3686: the PRs column and total exist only when every row reports the selected window's count.
  const showPRs = rows.every((r) => r.selectedPRs != null);
  // null (never a partial sum) unless every row reports the count.
  const factoryPRs: number | null = showPRs ? rows.reduce((sum, r) => sum + r.selectedPRs!, 0) : null;
  // Issue #3876: the Claude / Codex column exists only when every row reports the selected
  // window's split, the same rule as the PRs.
  const showHarness = rows.every((r) => r.selectedHarness != null);
  const factoryRuns = recent ? rows.reduce((sum, u) => sum + u.selectedRuns, 0) : admin.factory.run_count;
  return { available, rows, showPRs, showHarness, factoryRuns, factoryPRs, factoryOutcomes, factory, factoryDisclosure };
}
