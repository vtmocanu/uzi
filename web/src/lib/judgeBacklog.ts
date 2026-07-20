// Judge menu display helpers (PRD #98 M3). Pure, so the bucket-tab wiring, the group
// rollup labels, and the zero-state verdict tally are unit-tested without the DOM. None
// of these render untrusted text — they map closed enums (buckets, verdicts) to fixed UI
// copy; the untrusted free text (rationale_preview, target, run_title) is rendered
// separately as escaped React text on the page.

import type {
  BadgeTone,
} from "../components/ui";
import type {
  JudgeBacklogBucket,
  JudgeRecommendationGroup,
  ReviewVerdict,
  TriageCounts,
} from "./api";

// The bucket tabs, in ladder order. "To triage" is the landing tab (the backlog's
// reason to exist); "All" is the unfiltered view. The order matches the #94 ladder plus
// the catch-all last.
export const JUDGE_BUCKETS: JudgeBacklogBucket[] = ["todo", "filed", "done", "dismissed", "all"];

const BUCKET_LABELS: Record<JudgeBacklogBucket, string> = {
  todo: "To triage",
  filed: "Filed",
  done: "Done",
  dismissed: "Dismissed",
  all: "All",
};

export function bucketTabLabel(bucket: JudgeBacklogBucket): string {
  return BUCKET_LABELS[bucket];
}

// bucketTabCount reads a tab's number STRAIGHT from the canonical triage aggregate —
// never from the groups on screen (PRD #98: the To-triage tab must agree with the nav
// badge and the notification to the digit, which only holds if it reads triage.todo
// rather than re-tallying a possibly-truncated, possibly-filtered group list). "all" is
// the recommendation-row denominator (triage.total), matching #94's strip.
export function bucketTabCount(triage: TriageCounts, bucket: JudgeBacklogBucket): number {
  switch (bucket) {
    case "todo":
      return triage.todo;
    case "filed":
      return triage.filed;
    case "done":
      return triage.done;
    case "dismissed":
      return triage.dismissed;
    case "all":
      return triage.total;
  }
}

// rollupTone tints a group's rollup badge by the #94 ladder tone: todo is a plain
// neutral "to do", filed is info, done is ok, dismissed is muted/neutral. A group
// rollup is never "all" (that is a filter, not a member state), but the map is total so
// the type stays exhaustive.
const ROLLUP_TONE: Record<JudgeBacklogBucket, BadgeTone> = {
  todo: "neutral",
  filed: "info",
  done: "ok",
  dismissed: "neutral",
  all: "neutral",
};

export function rollupTone(bucket: JudgeBacklogBucket): BadgeTone {
  return ROLLUP_TONE[bucket];
}

const ROLLUP_LABEL: Record<JudgeBacklogBucket, string> = {
  todo: "To do",
  filed: "Filed",
  done: "Done",
  dismissed: "Dismissed",
  all: "All",
};

export function rollupLabel(bucket: JudgeBacklogBucket): string {
  return ROLLUP_LABEL[bucket];
}

// seenInRunsLabel is the frequency evidence chip. Singular/plural so "seen in 1 run"
// never reads wrong — a group can legitimately be a single run (it just is not deduped
// across any yet).
export function seenInRunsLabel(runCount: number): string {
  return `seen in ${runCount} ${runCount === 1 ? "run" : "runs"}`;
}

// VerdictTrend is the zero-state's "recent verdicts" summary: a per-verdict count over
// the DISTINCT runs the caller has judged. Distinct-by-run because a run carries one
// verdict (its review's), while a group lists it once per occurrence — tallying raw
// occurrences would over-count a verdict for every recommendation it recurred in.
export interface VerdictTrend {
  ideal: number;
  ok: number;
  issues: number;
  total: number;
}

// recentVerdictTrend tallies each judged run's verdict once, from the occurrences a
// backlog carries. It reads a closed enum (verdict), never free text. Fed the bucket=all
// snapshot on the zero-state so it reflects the whole history, not the (empty) todo view.
export function recentVerdictTrend(groups: JudgeRecommendationGroup[]): VerdictTrend {
  const byRun = new Map<string, ReviewVerdict>();
  for (const g of groups) {
    for (const occ of g.occurrences) {
      // First writer wins is fine: a run's verdict is the same on every occurrence of
      // it, since they all come from that run's one review.
      if (!byRun.has(occ.run_id)) byRun.set(occ.run_id, occ.verdict);
    }
  }
  const trend: VerdictTrend = { ideal: 0, ok: 0, issues: 0, total: 0 };
  for (const v of byRun.values()) {
    trend[v] += 1;
    trend.total += 1;
  }
  return trend;
}
