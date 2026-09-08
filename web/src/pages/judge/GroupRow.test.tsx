// @vitest-environment jsdom
//
// PRD #1183 M2 regression: the expander caches the newest-open run's FULL rationale keyed to
// that run. A backlog reload can change which run is newest-open while the row keeps its
// coordinate key (same category/target) — so the cache MUST drop the previous run's rationale
// and refetch, never leave the old run's text on screen or skip the new fetch.
import { afterEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { GroupRow } from "./GroupRow";
import type { JudgeRecommendationGroup, RunReview } from "../../lib/api";

afterEach(cleanup);

function occ(over: Partial<JudgeRecommendationGroup["occurrences"][number]> = {}) {
  return {
    run_id: "run-1",
    run_title: "A run",
    review_id: "rev-1",
    rec_id: "rec-1",
    verdict: "issues" as const,
    confidence: "" as const,
    bucket: "todo" as const,
    ...over,
  };
}

function group(over: Partial<JudgeRecommendationGroup> = {}): JudgeRecommendationGroup {
  return {
    category: "improve_uzi",
    target: "api/internal/poller",
    bucket: "todo",
    open_count: 1,
    run_count: 1,
    rationale_preview: "Clamped preview.",
    occurrences: [occ()],
    ...over,
  };
}

function reviewFor(runId: string, rationale: string): { review: RunReview; pending_judge: null } {
  return {
    review: {
      id: "rev-x",
      target_run_id: runId,
      verdict: "issues",
      summary_md: "",
      judge_model: "m",
      status: "complete",
      created_at: "2026-07-20T10:00:00Z",
      updated_at: "2026-07-20T10:00:00Z",
      recommendations: [
        {
          id: "rec-x",
          category: "improve_uzi",
          target: "api/internal/poller",
          rationale_md: rationale,
          confidence: "high",
          created_at: "2026-07-20T10:00:00Z",
        },
      ],
      filed_issues: [],
      dispositions: [],
      triage: { total: 0, todo: 0, filed: 0, done: 0, dismissed: 0, false_positives: 0 },
    },
    pending_judge: null,
  };
}

function row(g: JudgeRecommendationGroup, fetchReview: (runId: string) => Promise<ReturnType<typeof reviewFor>>) {
  return (
    <MemoryRouter>
      <GroupRow
        group={g}
        selected={false}
        onToggleSelect={() => {}}
        onDispose={() => {}}
        repos={[]}
        onFiled={() => {}}
        fetchReview={fetchReview}
      />
    </MemoryRouter>
  );
}

describe("GroupRow — the expander's rationale cache is keyed to the newest-open run", () => {
  it("drops the previous run's rationale and refetches when the newest-open run changes", async () => {
    const fetchReview = vi
      .fn()
      .mockResolvedValueOnce(reviewFor("run-A", "RATIONALE FROM RUN A"))
      .mockResolvedValueOnce(reviewFor("run-B", "RATIONALE FROM RUN B"));

    const gA = group({ occurrences: [occ({ run_id: "run-A", rec_id: "rec-A" })] });
    const { rerender } = render(row(gA, fetchReview));

    // First expand fetches run-A and shows its full rationale.
    fireEvent.click(screen.getByRole("button", { name: /Expand occurrences/ }));
    await waitFor(() => expect(fetchReview).toHaveBeenCalledWith("run-A"));
    expect(await screen.findByText("RATIONALE FROM RUN A")).toBeTruthy();

    // Backlog reload: SAME coordinate (category/target → same row key), but the newest-open run
    // is now run-B. The row stays expanded and mounted, so its cache/refs survive the rerender.
    const gB = group({ occurrences: [occ({ run_id: "run-B", rec_id: "rec-B" })] });
    rerender(row(gB, fetchReview));

    await waitFor(() => expect(fetchReview).toHaveBeenCalledWith("run-B"));
    expect(await screen.findByText("RATIONALE FROM RUN B")).toBeTruthy();
    // The stale run-A rationale is gone (not left on screen, not re-shown).
    expect(screen.queryByText("RATIONALE FROM RUN A")).toBeNull();
  });

  it("does not refetch when the newest-open run is unchanged across a reload", async () => {
    const fetchReview = vi.fn().mockResolvedValue(reviewFor("run-A", "RATIONALE FROM RUN A"));

    const gA = group({ occurrences: [occ({ run_id: "run-A", rec_id: "rec-A" })] });
    const { rerender } = render(row(gA, fetchReview));

    fireEvent.click(screen.getByRole("button", { name: /Expand occurrences/ }));
    await waitFor(() => expect(fetchReview).toHaveBeenCalledTimes(1));
    expect(await screen.findByText("RATIONALE FROM RUN A")).toBeTruthy();

    // A reload that keeps run-A as the newest open must not refetch — the cache stands.
    rerender(row(group({ occurrences: [occ({ run_id: "run-A", rec_id: "rec-A" })] }), fetchReview));
    await waitFor(() => expect(screen.getByText("RATIONALE FROM RUN A")).toBeTruthy());
    expect(fetchReview).toHaveBeenCalledTimes(1);
  });
});
