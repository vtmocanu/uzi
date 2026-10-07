import type { RunOutcomes } from "./apiTypes";

// Lifetime failure recency uses elapsed whole units, never calendar-day boundaries.
export function failureRecency(outcomes: RunOutcomes, now: number): string {
  if (outcomes.last_failed_at == null) {
    if (outcomes.failed > 0) return "Unavailable";
    return outcomes.finished === 0 ? "–" : "no failures";
  }
  const ended = Date.parse(outcomes.last_failed_at);
  if (!Number.isFinite(ended)) return "Unavailable";
  const minutes = Math.floor(Math.max(0, now - ended) / 60_000);
  if (minutes < 1) return "<1m";
  if (minutes < 60) return `${minutes}m`;
  if (minutes < 1440) return `${Math.floor(minutes / 60)}h`;
  return `${Math.floor(minutes / 1440)}d`;
}
