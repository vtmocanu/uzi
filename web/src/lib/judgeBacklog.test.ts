import { describe, it, expect } from "vitest";
import {
  bucketTabCount,
  bucketTabLabel,
  JUDGE_BUCKETS,
  verdictTrend,
  rollupLabel,
  rollupTone,
  seenInRunsLabel,
} from "./judgeBacklog";
import type { JudgeRecommendationGroup, TriageCounts } from "./api";

const triage: TriageCounts = { total: 11, todo: 5, filed: 2, done: 2, dismissed: 2, false_positives: 1 };

describe("judgeBacklog — bucket tabs", () => {
  it("reads each tab count STRAIGHT from the canonical triage aggregate (never re-tallied)", () => {
    // This is the load-bearing property (PRD #98 auditor #3): the To-triage tab must
    // equal triage.todo — the same number the nav badge and the notification read — not a
    // count of the group rows on screen. `all` is the recommendation-row total.
    expect(bucketTabCount(triage, "todo")).toBe(5);
    expect(bucketTabCount(triage, "filed")).toBe(2);
    expect(bucketTabCount(triage, "done")).toBe(2);
    expect(bucketTabCount(triage, "dismissed")).toBe(2);
    expect(bucketTabCount(triage, "all")).toBe(11);
  });

  it("labels every bucket and covers the whole enum", () => {
    expect(JUDGE_BUCKETS).toEqual(["todo", "filed", "done", "dismissed", "all"]);
    expect(JUDGE_BUCKETS.map(bucketTabLabel)).toEqual(["To triage", "Filed", "Done", "Dismissed", "All"]);
  });

  it("maps the rollup badge tone/label for each rung", () => {
    expect(rollupLabel("filed")).toBe("Filed");
    expect(rollupLabel("done")).toBe("Done");
    expect(rollupLabel("dismissed")).toBe("Dismissed");
    expect(rollupTone("done")).toBe("ok");
    expect(rollupTone("filed")).toBe("info");
  });
});

describe("judgeBacklog — evidence copy", () => {
  it("singularises 'seen in N runs'", () => {
    expect(seenInRunsLabel(1)).toBe("seen in 1 run");
    expect(seenInRunsLabel(3)).toBe("seen in 3 runs");
  });
});

describe("judgeBacklog — verdictTrend (all-time, not a recency window)", () => {
  function group(occ: { run_id: string; verdict: "ideal" | "ok" | "issues" }[]): JudgeRecommendationGroup {
    return {
      category: "improve_uzi",
      target: "t",
      bucket: "all",
      open_count: 0,
      run_count: occ.length,
      rationale_preview: "",
      occurrences: occ.map((o) => ({
        run_id: o.run_id,
        run_title: "",
        review_id: "rev",
        rec_id: "rec",
        verdict: o.verdict,
        confidence: "",
        bucket: "todo",
      })),
    };
  }

  it("tallies each judged run's verdict ONCE, even when it recurs across groups", () => {
    // run-A appears in two groups; its verdict must count once, not twice — the trend is
    // per-run, not per-occurrence.
    const groups = [
      group([
        { run_id: "run-A", verdict: "issues" },
        { run_id: "run-B", verdict: "ideal" },
      ]),
      group([{ run_id: "run-A", verdict: "issues" }]),
    ];
    const trend = verdictTrend(groups);
    expect(trend).toEqual({ ideal: 1, ok: 0, issues: 1, total: 2 });
  });

  it("is empty for no occurrences", () => {
    expect(verdictTrend([])).toEqual({ ideal: 0, ok: 0, issues: 0, total: 0 });
  });
});

// PRD #98 review N7. isBucket lives in Judge.tsx because it validates a URL param, but the
// property that makes it safe belongs here, next to the array it now derives from: the
// exported JUDGE_BUCKETS must cover the whole JudgeBacklogBucket union.
//
// This is the one client-side bucket site tsc cannot guard — its input is `string | null`,
// so every comparison is legal against any string and a drift renders an empty list rather
// than erroring. Deriving the validator from JUDGE_BUCKETS moves the guard to the array,
// where the compiler DOES check the element type; this pins the remaining half, that the
// array is exhaustive rather than merely well-typed.
describe("judgeBacklog — JUDGE_BUCKETS is the whole union (PRD #98 review N7)", () => {
  it("covers every rung plus the unfiltered view, with no duplicates", () => {
    expect([...JUDGE_BUCKETS].sort()).toEqual(["all", "dismissed", "done", "filed", "todo"]);
    expect(new Set(JUDGE_BUCKETS).size).toBe(JUDGE_BUCKETS.length);
  });

  it("has a tab label and a rollup label for every member — exhaustive by construction", () => {
    for (const b of JUDGE_BUCKETS) {
      expect(bucketTabLabel(b)).toBeTruthy();
      expect(rollupLabel(b)).toBeTruthy();
    }
  });
});
