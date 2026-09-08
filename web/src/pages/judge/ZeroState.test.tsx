// @vitest-environment jsdom
//
// ZeroState had no test of its own. PRD #1183 retires the per-surface rollup vocabulary and
// moves its settled-example chips onto the shared TriageStateChip, so this pins the two
// observable consequences: the settled examples read the ONE vocabulary ("✓ Done" / "Filed")
// and the retired rollup word "To do" appears nowhere, while the kept helpers (verdictTrend,
// seenInRunsLabel) still render.
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { ZeroState } from "./ZeroState";
import { api, type JudgeBacklog, type JudgeRecommendationGroup } from "../../lib/api";

vi.mock("../../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../lib/api")>();
  return { ...actual, api: { getJudgeBacklog: vi.fn() } };
});

const mockApi = vi.mocked(api);

function occ(over: Partial<JudgeRecommendationGroup["occurrences"][number]> = {}) {
  return {
    run_id: "run-1",
    run_title: "A run",
    review_id: "rev-1",
    rec_id: "rec-1",
    verdict: "issues" as const,
    confidence: "" as const,
    bucket: "done" as const,
    ...over,
  };
}

function group(over: Partial<JudgeRecommendationGroup> = {}): JudgeRecommendationGroup {
  return {
    category: "improve_uzi",
    target: "api/internal/poller",
    bucket: "done",
    open_count: 0,
    run_count: 3,
    rationale_preview: "",
    occurrences: [occ()],
    ...over,
  };
}

// The zero-state fetches the bucket=all snapshot for the verdict trend and the settled
// examples. One done group and one filed group so both TriageStateChip rungs render.
function backlog(over: Partial<JudgeBacklog> = {}): JudgeBacklog {
  return {
    bucket: "all",
    run: "",
    groups: [
      group({ target: "done-target", bucket: "done", run_count: 4 }),
      group({ category: "install_worker_tool", target: "filed-target", bucket: "filed", run_count: 2 }),
    ],
    truncated: false,
    triage: { total: 2, todo: 0, filed: 1, done: 1, dismissed: 0, false_positives: 0 },
    ...over,
  };
}

beforeEach(() => {
  mockApi.getJudgeBacklog.mockResolvedValue(backlog());
});
afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

function renderZero(judgeEnabled = true) {
  return render(
    <MemoryRouter>
      <ZeroState judgeEnabled={judgeEnabled} />
    </MemoryRouter>,
  );
}

describe("ZeroState — inbox-zero vocabulary (PRD #1183)", () => {
  it("renders the headline in the 'to triage' vocabulary and its settled chips via TriageStateChip, with no old 'To do'", async () => {
    renderZero();

    // The inbox-zero headline carries the open-state vocabulary ("nothing to triage").
    expect(await screen.findByText(/nothing to triage/i)).toBeTruthy();

    // The bucket=all snapshot lands and the settled examples render...
    await waitFor(() => expect(screen.getByText("done-target")).toBeTruthy());
    // ...through the ONE shared chip: "✓ Done" for a done group, "Filed" for a filed one.
    expect(screen.getAllByText((_, el) => el?.textContent?.trim() === "✓ Done").length).toBeGreaterThan(0);
    expect(screen.getByText("Filed")).toBeTruthy();

    // The retired rollup word never appears — it read "To do" before this PRD.
    expect(screen.queryByText("To do")).toBeNull();
  });

  it("keeps the frequency chip and the verdict trend", async () => {
    renderZero();
    await waitFor(() => expect(screen.getByText("done-target")).toBeTruthy());

    // seenInRunsLabel is kept (the done group's run_count is 4).
    expect(screen.getByText("seen in 4 runs")).toBeTruthy();
    // verdictTrend is kept: the snapshot's one judged run tallies once.
    expect(screen.getByText("Verdicts across your judged runs")).toBeTruthy();
  });

  it("shows the opt-in card when the judge is off", async () => {
    renderZero(false);
    expect(await screen.findByText(/judge is off for your account/i)).toBeTruthy();
  });
});
