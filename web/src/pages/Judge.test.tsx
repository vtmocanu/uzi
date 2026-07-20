// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { Judge } from "./Judge";
import { api, type JudgeBacklog, type JudgeRecommendationGroup, type JudgeDispositionResult } from "../lib/api";
import { useAuth } from "../auth/AuthContext";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      getJudgeBacklog: vi.fn(),
      bulkSetJudgeDisposition: vi.fn(),
      deleteDisposition: vi.fn(),
      listRepos: vi.fn().mockResolvedValue({ repos: [] }),
    },
  };
});
vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));

const mockApi = vi.mocked(api);

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
    open_count: 3,
    run_count: 3,
    rationale_preview: "Queue-to-claim latency dominated the run.",
    occurrences: [
      occ({ run_id: "run-1", rec_id: "rec-1" }),
      occ({ run_id: "run-2", rec_id: "rec-2" }),
      occ({ run_id: "run-3", rec_id: "rec-3" }),
    ],
    ...over,
  };
}

// triage.todo (5) is DELIBERATELY larger than the number of open group rows (they dedup):
// the tab must show the canonical 5, not the 2 rows on screen.
function backlog(over: Partial<JudgeBacklog> = {}): JudgeBacklog {
  return {
    bucket: "todo",
    run: "",
    groups: [group(), group({ target: "shellcheck", category: "install_worker_tool", open_count: 1, run_count: 2 })],
    truncated: false,
    triage: { total: 11, todo: 5, filed: 2, done: 2, dismissed: 2, false_positives: 1 },
    ...over,
  };
}

beforeEach(() => {
  vi.mocked(useAuth).mockReturnValue({ user: { judge_enabled: true } } as unknown as ReturnType<typeof useAuth>);
});
afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

function renderJudge(entries: string[] = ["/judge"]) {
  return render(
    <MemoryRouter initialEntries={entries}>
      <Judge />
    </MemoryRouter>,
  );
}

describe("Judge — bucket tabs read the canonical triage, not the groups on screen", () => {
  it("shows triage.todo on the To-triage tab even though fewer group rows are visible", async () => {
    mockApi.getJudgeBacklog.mockResolvedValue(backlog());
    renderJudge();

    await waitFor(() => expect(screen.getByText("shellcheck")).toBeTruthy());
    // Two deduped group rows…
    expect(screen.getAllByRole("checkbox")).toHaveLength(2);
    // …but the To-triage tab shows the canonical per-recommendation count (5), NOT 2.
    const tab = screen.getByRole("tab", { name: /To triage/ });
    expect(tab.textContent).toContain("5");
    // The default fetch is the todo bucket.
    expect(mockApi.getJudgeBacklog).toHaveBeenCalledWith("todo", undefined);
  });
});

describe("Judge — rationale_preview is escaped text, never HTML (PRD #98 auditor #1)", () => {
  it("renders markup in the preview as literal characters", async () => {
    mockApi.getJudgeBacklog.mockResolvedValue(
      backlog({
        groups: [group({ rationale_preview: "<img src=x onerror=alert(1)> **not bold**" })],
        triage: { total: 1, todo: 1, filed: 0, done: 0, dismissed: 0, false_positives: 0 },
      }),
    );
    const { container } = renderJudge();

    expect(await screen.findByText(/<img src=x onerror=alert\(1\)> \*\*not bold\*\*/)).toBeTruthy();
    // The markup never became a real element (no dangerouslySetInnerHTML / markdown).
    expect(container.querySelector("img")).toBeNull();
  });
});

describe("Judge — a dismiss RE-RENDERS the row from the response, never a client-side filter", () => {
  it("keeps the acted-on row (now Dismissed) and reports the response's Updated count", async () => {
    mockApi.getJudgeBacklog.mockResolvedValue(
      backlog({
        groups: [group()], // one group, open_count 3, visibly spanning 3 recommendations
        triage: { total: 3, todo: 3, filed: 0, done: 0, dismissed: 0, false_positives: 0 },
      }),
    );
    // The response RE-READS at bucket=all: the just-dismissed group comes back with its new
    // rollup (dismissed, open 0). Updated is 2 — a (review, category, target) TRIPLE count,
    // deliberately LOWER than the 3 recommendations the group visibly spanned.
    const disposed: JudgeDispositionResult = {
      updated: 2,
      groups: [
        group({
          bucket: "dismissed",
          open_count: 0,
          occurrences: [
            occ({ run_id: "run-1", rec_id: "rec-1", bucket: "dismissed" }),
            occ({ run_id: "run-2", rec_id: "rec-2", bucket: "dismissed" }),
            occ({ run_id: "run-3", rec_id: "rec-3", bucket: "dismissed" }),
          ],
        }),
      ],
      truncated: false,
      triage: { total: 3, todo: 0, filed: 0, done: 0, dismissed: 3, false_positives: 0 },
    };
    mockApi.bulkSetJudgeDisposition.mockResolvedValue(disposed);

    renderJudge();
    await waitFor(() => expect(screen.getByText("api/internal/poller")).toBeTruthy());

    fireEvent.click(screen.getByRole("button", { name: /Dismiss/ }));
    fireEvent.click(screen.getByRole("menuitem", { name: /Won't do/ }));

    // The toast reports the RESPONSE's Updated (2), not the group's visible span (3).
    await waitFor(() => expect(screen.getByText(/2 recommendations dismissed/)).toBeTruthy());
    // The row did NOT vanish — it re-rendered in place at its new rollup.
    const row = screen.getByText("api/internal/poller").closest("li")!;
    expect(within(row).getByText("Dismissed")).toBeTruthy();

    // scope defaults to open; the coordinate is the group's (category, target).
    expect(mockApi.bulkSetJudgeDisposition).toHaveBeenCalledWith(
      [{ category: "improve_uzi", target: "api/internal/poller" }],
      "dismissed",
      "wont_do",
      "open",
    );
  });

  it("Undo clears exactly the members the action settled", async () => {
    mockApi.getJudgeBacklog.mockResolvedValue(
      backlog({
        groups: [group()],
        triage: { total: 3, todo: 3, filed: 0, done: 0, dismissed: 0, false_positives: 0 },
      }),
    );
    mockApi.bulkSetJudgeDisposition.mockResolvedValue({
      updated: 3,
      groups: [group({ bucket: "dismissed", open_count: 0 })],
      truncated: false,
      triage: { total: 3, todo: 0, filed: 0, done: 0, dismissed: 3, false_positives: 0 },
    });
    mockApi.deleteDisposition.mockResolvedValue(null);

    renderJudge();
    await waitFor(() => expect(screen.getByText("api/internal/poller")).toBeTruthy());

    fireEvent.click(screen.getByRole("button", { name: /Mark done/ }));
    const toast = await screen.findByRole("status");
    fireEvent.click(within(toast).getByText("Undo"));

    // One delete per OPEN member snapshotted before the action (3 todo occurrences).
    await waitFor(() => expect(mockApi.deleteDisposition).toHaveBeenCalledTimes(3));
    expect(mockApi.deleteDisposition).toHaveBeenCalledWith("run-1", "rec-1");
    expect(mockApi.deleteDisposition).toHaveBeenCalledWith("run-2", "rec-2");
    expect(mockApi.deleteDisposition).toHaveBeenCalledWith("run-3", "rec-3");
  });
});

describe("Judge — truncation is surfaced, never rendered as authoritative (PRD #98 auditor #2)", () => {
  it("shows the truncated banner when the backlog hit the cap", async () => {
    mockApi.getJudgeBacklog.mockResolvedValue(backlog({ truncated: true }));
    renderJudge();
    expect(await screen.findByText(/backlog is large and was truncated/i)).toBeTruthy();
  });
});

describe("Judge — inbox-zero is a first-class view (PRD #98 Decision 8)", () => {
  it("renders the zero-state (and the opt-in card when the judge is off) when triage.todo is 0", async () => {
    vi.mocked(useAuth).mockReturnValue({ user: { judge_enabled: false } } as unknown as ReturnType<typeof useAuth>);
    // The todo view is empty and triage.todo is 0 → the zero state, which fetches an
    // all-bucket snapshot for the recent-verdict trend + recently-handled groups.
    mockApi.getJudgeBacklog.mockImplementation(async (bucket) => {
      if (bucket === "all") {
        return backlog({
          bucket: "all",
          groups: [group({ bucket: "done", open_count: 0 })],
          triage: { total: 2, todo: 0, filed: 1, done: 1, dismissed: 0, false_positives: 0 },
        });
      }
      return backlog({
        groups: [],
        triage: { total: 2, todo: 0, filed: 1, done: 1, dismissed: 0, false_positives: 0 },
      });
    });

    renderJudge();

    expect(await screen.findByText(/Inbox zero/i)).toBeTruthy();
    expect(screen.getByText(/judge is off for your account/i)).toBeTruthy();
    // Recent-verdict trend rendered from the all-bucket snapshot.
    await waitFor(() => expect(screen.getByText("Recent verdicts")).toBeTruthy());
  });
});

describe("Judge — the ?run= deep-link anchor (PRD #98 Decision 4)", () => {
  it("passes the run anchor to the backlog read and shows a clearable filter", async () => {
    mockApi.getJudgeBacklog.mockResolvedValue(backlog({ bucket: "all", run: "run-1" }));
    renderJudge(["/judge?run=run-1"]);

    await waitFor(() => expect(screen.getByText("api/internal/poller")).toBeTruthy());
    // An anchored deep-link defaults to the `all` bucket so the run's recs always show.
    expect(mockApi.getJudgeBacklog).toHaveBeenCalledWith("all", "run-1");
    expect(screen.getByText(/Filtered to one run's recommendations/i)).toBeTruthy();
  });
});
