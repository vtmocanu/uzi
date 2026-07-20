import { describe, it, expect } from "vitest";
import {
  bucketTabCount,
  bucketTabLabel,
  JUDGE_BUCKETS,
  recentVerdictTrend,
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

describe("judgeBacklog — recentVerdictTrend", () => {
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
    const trend = recentVerdictTrend(groups);
    expect(trend).toEqual({ ideal: 1, ok: 0, issues: 1, total: 2 });
  });

  it("is empty for no occurrences", () => {
    expect(recentVerdictTrend([])).toEqual({ ideal: 0, ok: 0, issues: 0, total: 0 });
  });
});
