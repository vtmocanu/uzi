import type { JudgeVerdict } from "./api";

/** The subset of a run the judge badge needs — so the helper is testable without a
 *  whole RunListItem and works for any surface that carries these two fields. */
export type JudgeBadgeable = {
  judge_verdict: JudgeVerdict | null;
  judge_todo_count: number;
};

export type JudgeBadgeSpec = {
  label: string;
  tone: "ok" | "info" | "warning";
  title: string;
};

// Verdict → tone. `issues` is a warning rather than a danger: the judge found things
// worth doing, not a broken run — the run's own status pill owns failure.
const VERDICT_TONE: Record<JudgeVerdict, JudgeBadgeSpec["tone"]> = {
  ideal: "ok",
  ok: "info",
  issues: "warning",
};

const VERDICT_TITLE: Record<JudgeVerdict, string> = {
  ideal: "The judge found nothing to improve in this run.",
  ok: "The judge rated this run acceptable.",
  issues: "The judge flagged issues in this run.",
};

/**
 * judgeBadge builds the /runs per-row judge badge (PRD #98 M4, Decision 7).
 *
 * ONE grammar, deliberately: verdict first, and the to-triage count appended only when
 * it is > 0 — `⚖ issues · 2`, `⚖ ideal`. The concept mock had two grammars (a
 * verdict badge AND a separate count badge) and that was called out as a bug: two
 * badges for one run read as two facts, and the reader has to work out that they
 * describe the same review.
 *
 * Returns null for an unjudged run, so the row renders NOTHING rather than a neutral
 * pill. "Not judged" and "judged and fine" are genuinely different, and a placeholder
 * would assert the second when only the first is true — the same reason judge_verdict
 * is nullable rather than defaulted server-side.
 *
 * The count is NOT recomputed here: it arrives already bucketed by the one shared
 * BucketOf ladder, so this badge cannot disagree with the Judge page or the nav badge.
 */
export function judgeBadge(run: JudgeBadgeable): JudgeBadgeSpec | null {
  const verdict = run.judge_verdict;
  if (!verdict) return null;
  const count = run.judge_todo_count > 0 ? run.judge_todo_count : 0;
  return {
    label: count > 0 ? `⚖ ${verdict} · ${count}` : `⚖ ${verdict}`,
    tone: VERDICT_TONE[verdict],
    title: count > 0 ? `${VERDICT_TITLE[verdict]} ${count} still to triage.` : VERDICT_TITLE[verdict],
  };
}
