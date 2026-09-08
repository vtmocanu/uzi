// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { Findings } from "./Findings";
import { AppShell } from "../components/AppShell";
import { useAuth } from "../auth/AuthContext";
import {
  api,
  type IncidentalFinding,
  type IncidentalFindingBacklog,
  type TriageCounts,
} from "../lib/api";

// importOriginal is spread so the REAL isHttpsUrl (TriageStateChip's filed-link guard), ApiError and
// the DTO types run for real; only the `api` object is replaced. This inner `api:{}` is NOT spread
// from actual, so EVERY api.* the page or the AppShell chrome reaches must be listed here — the M4
// additions (getFindingsStats / dismissFindings / undoDismissFinding) and the AppShell nav polls.
vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    MOCK_MODE: false,
    api: {
      // Findings page
      listFindings: vi.fn(),
      getFindingsStats: vi.fn(),
      dismissFindings: vi.fn(),
      undoDismissFinding: vi.fn(),
      fileFinding: vi.fn(),
      dismissFinding: vi.fn(),
      findingIssueDraft: vi.fn(),
      listRepos: vi.fn().mockResolvedValue({ repos: [] }),
      // AppShell chrome, for the together-mount BLK-BADGE test. Zero/empty so the nav renders with
      // no other badge in the way of the Findings badge assertions.
      getJudgeStats: vi.fn().mockResolvedValue({ total: 0, todo: 0, filed: 0, done: 0, dismissed: 0, false_positives: 0 }),
      listConnections: vi.fn().mockResolvedValue({ connections: [] }),
      unreadNotificationCount: vi.fn().mockResolvedValue({ count: 0 }),
      workerUpgradeSummary: vi.fn().mockResolvedValue({ attention: 0, target_release: "0.6.0" }),
      runsInProgressCount: vi.fn().mockResolvedValue({ count: 0 }),
      listSchedules: vi.fn().mockResolvedValue([]),
      listRuns: vi.fn().mockResolvedValue({ runs: [] }),
      getMyRateLimits: vi.fn().mockResolvedValue({ status: "no_token" }),
      getMySettings: vi.fn().mockResolvedValue({
        settings: { default_model: null, default_effort: null, judge_model: null, summary_model: null, theme: null },
      }),
      version: vi.fn().mockResolvedValue({ version: "9.9.9-test" }),
    },
  };
});
vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));

const mockApi = vi.mocked(api);

const user = {
  id: "u1",
  email: "admin@uzi.local",
  display_name: "Admin",
  is_admin: false,
  is_active: true,
  autopilot_enabled: false,
  judge_enabled: true,
  created_at: "2026-01-01T00:00:00Z",
  last_login: null,
};

function finding(over: Partial<IncidentalFinding> = {}): IncidentalFinding {
  return {
    disposition_id: "disp-1",
    finding_id: "find-1",
    location: "api/internal/sweeper.go#sweepLoop",
    repo_id: "repo-uzi",
    repo_path: "vtmocanu/uzi",
    status: "open",
    last_title: "Leaked ticker in sweepLoop",
    seen_in_runs: 2,
    ...over,
  };
}

function backlog(over: Partial<IncidentalFindingBacklog> = {}): IncidentalFindingBacklog {
  return {
    bucket: "to_file",
    repo: "",
    run: "",
    open_count: 3,
    findings: [finding()],
    ...over,
  };
}

function triage(over: Partial<TriageCounts> = {}): TriageCounts {
  return { total: 5, todo: 3, filed: 1, done: 1, dismissed: 0, false_positives: 0, ...over };
}

beforeEach(() => {
  vi.mocked(useAuth).mockReturnValue({ user, logout: vi.fn() } as unknown as ReturnType<typeof useAuth>);
  mockApi.listFindings.mockResolvedValue(backlog());
  mockApi.getFindingsStats.mockResolvedValue(triage());
  mockApi.listRepos.mockResolvedValue({ repos: [] });
});
afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

function renderFindings(entries: string[] = ["/findings"]) {
  return render(
    <MemoryRouter initialEntries={entries}>
      <Findings />
    </MemoryRouter>,
  );
}

function renderFindingsInShell(entries: string[] = ["/findings"]) {
  return render(
    <MemoryRouter initialEntries={entries}>
      <AppShell>
        <Findings />
      </AppShell>
    </MemoryRouter>,
  );
}

describe("Findings page — counted tabs (PRD #1183 M4)", () => {
  it("reads every tab count from the canonical stats, NOT a tally of the visible rows", async () => {
    // Canonical stats far exceed the single visible row; the tabs must show the stats, not 1.
    mockApi.getFindingsStats.mockResolvedValue(
      triage({ total: 12, todo: 7, filed: 2, done: 1, dismissed: 2, false_positives: 1 }),
    );
    mockApi.listFindings.mockResolvedValue(backlog({ findings: [finding()] }));
    renderFindings();
    await waitFor(() => expect(screen.getByText("Leaked ticker in sweepLoop")).toBeTruthy());

    // Five tabs, each labelled + counted from stats.
    expect(screen.getByRole("tab", { name: /To triage/ }).textContent).toContain("7");
    expect(screen.getByRole("tab", { name: /Filed/ }).textContent).toContain("2");
    expect(screen.getByRole("tab", { name: /Done/ }).textContent).toContain("1");
    expect(screen.getByRole("tab", { name: /Dismissed/ }).textContent).toContain("2");
    expect(screen.getByRole("tab", { name: /^All/ }).textContent).toContain("12");
    // Only one row is on screen, so the To-triage tab's 7 is provably not a row tally.
    expect(screen.getAllByText("Leaked ticker in sweepLoop").length).toBe(1);
    // The old open-state word is gone everywhere.
    expect(screen.queryByRole("tab", { name: /To file/ })).toBeNull();
  });

  it("switches the fetched bucket when the Done tab is clicked", async () => {
    renderFindings();
    await waitFor(() => expect(screen.getByText("Leaked ticker in sweepLoop")).toBeTruthy());
    fireEvent.click(screen.getByRole("tab", { name: /^Done/ }));
    await waitFor(() => expect(mockApi.listFindings).toHaveBeenCalledWith("done", undefined, undefined));
  });
});

describe("Findings page — seen-in-runs", () => {
  it("renders 'seen in 3 runs' but never 'seen in 1 run'", async () => {
    mockApi.listFindings.mockResolvedValue(
      backlog({
        repo: "repo-uzi",
        findings: [
          finding({ disposition_id: "d-a", finding_id: "f-a", last_title: "three runs", seen_in_runs: 3 }),
          finding({ disposition_id: "d-b", finding_id: "f-b", last_title: "one run", seen_in_runs: 1 }),
        ],
      }),
    );
    renderFindings(["/findings?repo=repo-uzi"]);
    await waitFor(() => expect(screen.getByText("three runs")).toBeTruthy());
    expect(screen.getByText("seen in 3 runs")).toBeTruthy();
    expect(screen.queryByText(/seen in 1 run/)).toBeNull();
  });
});

describe("Findings page — state chips (PRD #1183 vocabulary)", () => {
  it("shows the To triage chip on an open row (not 'To file')", async () => {
    renderFindings();
    await waitFor(() => expect(screen.getByText("Leaked ticker in sweepLoop")).toBeTruthy());
    // "To triage" is now both the open-bucket tab and the row's chip — at least one, never "To file".
    expect(screen.getAllByText("To triage").length).toBeGreaterThan(0);
    expect(screen.queryByText("To file")).toBeNull();
  });

  it("shows the reasoned Dismissed chip", async () => {
    mockApi.listFindings.mockResolvedValue(
      backlog({
        bucket: "dismissed",
        repo: "repo-uzi",
        findings: [
          finding({ disposition_id: "d-x", finding_id: "f-x", status: "dismissed", dismiss_reason: "wont_do", last_title: "parked" }),
        ],
      }),
    );
    renderFindings(["/findings?bucket=dismissed&repo=repo-uzi"]);
    await waitFor(() => expect(screen.getByText("parked")).toBeTruthy());
    expect(screen.getByText("Dismissed · Won't do")).toBeTruthy();
  });

  it("shows the Done via #N chip on an issue-closed row", async () => {
    mockApi.listFindings.mockResolvedValue(
      backlog({
        bucket: "done",
        repo: "repo-uzi",
        findings: [
          finding({
            disposition_id: "d-done",
            finding_id: "f-done",
            status: "done",
            set_via: "issue_close",
            filed_issue_iid: 344,
            filed_issue_url: "https://gitlab.example.com/vtmocanu/uzi/-/issues/344",
            last_title: "auto-done",
          }),
        ],
      }),
    );
    renderFindings(["/findings?bucket=done&repo=repo-uzi"]);
    await waitFor(() => expect(screen.getByText("auto-done")).toBeTruthy());
    expect(screen.getByText(/Done via #344/)).toBeTruthy();
  });
});

describe("Findings page — File issue via the shared draft card", () => {
  it("opens the fixed-repo draft and posts the edits to fileFinding(finding_id)", async () => {
    mockApi.findingIssueDraft.mockResolvedValue({
      title: "File race in sweepLoop",
      description: "The ticker leaks on early return.",
      location: "api/internal/sweeper.go#sweepLoop",
      labels: ["bug"],
      provenance: "from the coder worker",
    });
    mockApi.fileFinding.mockResolvedValue({
      issue: { iid: 512, web_url: "https://gitlab.example.com/vtmocanu/uzi/-/issues/512", title: "File race" },
    });
    renderFindings();
    await waitFor(() => expect(screen.getByText("Leaked ticker in sweepLoop")).toBeTruthy());

    fireEvent.click(screen.getByRole("button", { name: "File issue" }));
    await screen.findByText("Draft issue");
    await screen.findByRole("button", { name: "Create issue" });
    expect(mockApi.findingIssueDraft).toHaveBeenCalledWith("find-1");
    // Fixed-repo mode — the draft card carries NO repo selector (only the page's repo filter, one
    // level up, is a combobox). Scope the assertion to the draft card, and confirm the fixed repo
    // shows read-only.
    const draft = screen.getByText("Draft issue").closest("div.rounded-xl") as HTMLElement;
    expect(within(draft).queryByRole("combobox")).toBeNull();
    expect(within(draft).getByText("vtmocanu/uzi")).toBeTruthy();

    fireEvent.change(screen.getByDisplayValue(/File race in sweepLoop/), { target: { value: "edited title" } });
    fireEvent.click(screen.getByRole("button", { name: "Create issue" }));

    await waitFor(() =>
      expect(mockApi.fileFinding).toHaveBeenCalledWith("find-1", {
        title: "edited title",
        description: "The ticker leaks on early return.",
        labels: ["bug"],
      }),
    );
    // Filed state is the shared "Filed #N" link chip.
    const link = await screen.findByRole("link", { name: /Filed #512/ });
    expect(link.getAttribute("href")).toBe("https://gitlab.example.com/vtmocanu/uzi/-/issues/512");
  });
});

describe("Findings page — Dismiss ▾ menu", () => {
  it("dismisses a single row through the BULK endpoint keyed on disposition_id", async () => {
    mockApi.dismissFindings.mockResolvedValue({
      updated: 1,
      findings: [
        finding({ disposition_id: "disp-1", status: "dismissed", dismiss_reason: "wont_do", last_title: "Leaked ticker in sweepLoop" }),
      ],
    });
    renderFindings();
    await waitFor(() => expect(screen.getByText("Leaked ticker in sweepLoop")).toBeTruthy());

    fireEvent.click(screen.getByRole("button", { name: "Dismiss ▾" }));
    const menu = screen.getByRole("menu");
    expect(within(menu).getByText("False positive, the worker got it wrong")).toBeTruthy();
    fireEvent.click(within(menu).getByText("Won't do"));

    // Single-row dismiss routes through the bulk endpoint on disposition_id (undo stays symmetric).
    await waitFor(() => expect(mockApi.dismissFindings).toHaveBeenCalledWith(["disp-1"], "wont_do"));
    expect(mockApi.dismissFinding).not.toHaveBeenCalled();
    // The row re-renders at its new rollup with the reasoned chip.
    expect(await screen.findByText("Dismissed · Won't do")).toBeTruthy();
  });

  it("closes the menu on Escape and on an outside click", async () => {
    renderFindings();
    await waitFor(() => expect(screen.getByText("Leaked ticker in sweepLoop")).toBeTruthy());

    fireEvent.click(screen.getByRole("button", { name: "Dismiss ▾" }));
    expect(screen.getByRole("menu")).toBeTruthy();
    fireEvent.keyDown(document.body, { key: "Escape" });
    expect(screen.queryByRole("menu")).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "Dismiss ▾" }));
    expect(screen.getByRole("menu")).toBeTruthy();
    fireEvent.pointerDown(document.body);
    expect(screen.queryByRole("menu")).toBeNull();
  });
});

describe("Findings page — select-all, bulk dismiss + bounded Undo", () => {
  it("select-all dismisses every open row, and Undo reopens each at a bounded concurrency", async () => {
    const rows = Array.from({ length: 8 }, (_, i) =>
      finding({ disposition_id: `disp-${i + 1}`, finding_id: `find-${i + 1}`, last_title: `bug ${i + 1}`, location: `f${i + 1}.go#x` }),
    );
    mockApi.listFindings.mockResolvedValue(backlog({ repo: "repo-uzi", findings: rows, open_count: 8 }));
    mockApi.getFindingsStats.mockResolvedValue(triage({ total: 8, todo: 8, filed: 0, done: 0, dismissed: 0 }));
    mockApi.dismissFindings.mockResolvedValue({
      updated: 8,
      findings: rows.map((r) => ({ ...r, status: "dismissed", dismiss_reason: "wont_do" as const })),
    });

    // Gate every undo DELETE so the in-flight count can be measured against the pool bound.
    let inFlight = 0;
    let maxInFlight = 0;
    const releases: Array<() => void> = [];
    mockApi.undoDismissFinding.mockImplementation((id: string) => {
      inFlight += 1;
      maxInFlight = Math.max(maxInFlight, inFlight);
      return new Promise<IncidentalFinding>((resolve) => {
        releases.push(() => {
          inFlight -= 1;
          resolve(finding({ disposition_id: id, status: "open" }));
        });
      });
    });

    renderFindings(["/findings?repo=repo-uzi"]);
    await waitFor(() => expect(screen.getByText("bug 1")).toBeTruthy());

    // Select every open row via the header checkbox, then bulk-dismiss from the sticky bar.
    fireEvent.click(screen.getByRole("checkbox", { name: /Select all 8 shown/ }));
    const barLabel = await screen.findByText(/8 findings selected/);
    const bar = barLabel.parentElement as HTMLElement;
    fireEvent.click(within(bar).getByRole("button", { name: "Dismiss ▾" }));
    fireEvent.click(within(screen.getByRole("menu")).getByText("Won't do"));

    await waitFor(() => expect(mockApi.dismissFindings).toHaveBeenCalledTimes(1));
    expect(mockApi.dismissFindings.mock.calls[0][0]).toHaveLength(8);
    expect(mockApi.dismissFindings.mock.calls[0][1]).toBe("wont_do");

    // The toast offers Undo over all 8 settled ids.
    const undoBtn = await screen.findByRole("button", { name: "Undo" });
    fireEvent.click(undoBtn);

    // BOUNDED: exactly UNDO_CONCURRENCY (6) DELETEs are in flight before any resolves, never all 8.
    await waitFor(() => expect(mockApi.undoDismissFinding).toHaveBeenCalledTimes(6));
    expect(maxInFlight).toBe(6);

    // Drain the gate; the remaining two fire as workers free up, reaching 8 total.
    await act(async () => {
      for (let i = 0; i < 30 && releases.length; i++) {
        releases.shift()?.();
        await Promise.resolve();
      }
    });
    await waitFor(() => expect(mockApi.undoDismissFinding).toHaveBeenCalledTimes(8));
    expect(maxInFlight).toBe(6);
  });
});

describe("Findings page — evidence expander (PRD #1183 M3/M4)", () => {
  it("shows evidence_preview and one link per occurrence, with run_title stripped in the title attribute", async () => {
    // \u202E RIGHT-TO-LEFT OVERRIDE, built from an escape (never pasted raw), in the agent-authored
    // run_title / last_title.
    mockApi.listFindings.mockResolvedValue(
      backlog({
        repo: "repo-uzi",
        findings: [
          finding({
            disposition_id: "disp-1",
            finding_id: "find-1",
            last_title: "Leak\u202Ey ticker",
            evidence_preview: "The ticker leaks on early return.",
            occurrences: [
              { run_id: "run-live", run_title: "Wire\u202E the store", reported_at: "2026-09-08T11:30:00Z", confidence: "high" },
              { run_id: "run-done", run_title: "Sweeper refactor", reported_at: "2026-09-08T02:00:00Z", confidence: "medium" },
            ],
          }),
        ],
      }),
    );
    renderFindings(["/findings?repo=repo-uzi"]);
    await waitFor(() => expect(screen.getByText("Leaky ticker")).toBeTruthy());

    // The row checkbox's aria-label carries last_title — stripped on the ATTRIBUTE.
    const checkbox = screen.getByRole("checkbox", { name: /Select Leak/ });
    expect(checkbox.getAttribute("aria-label")).not.toMatch(/[\p{Cf}]/u);

    // Collapsed by default.
    expect(screen.queryByText("The ticker leaks on early return.")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Expand evidence" }));

    expect(screen.getByText("The ticker leaks on early return.")).toBeTruthy();
    // One run link per occurrence, to /runs/{run_id}, with run_title stripped in the title attribute.
    const link = screen.getByRole("link", { name: /Wire the store/ });
    expect(link.getAttribute("href")).toBe("/runs/run-live");
    expect(link.getAttribute("title")).not.toMatch(/[\p{Cf}]/u);
    expect(link.getAttribute("title")).toBe("Wire the store");
    expect(screen.getByRole("link", { name: /Sweeper refactor/ }).getAttribute("href")).toBe("/runs/run-done");
  });
});

describe("Findings page — repo grouping (PRD #333 M7, D3)", () => {
  it("groups rows under a repo_path header in the All-repos view", async () => {
    mockApi.listFindings.mockResolvedValue(
      backlog({
        findings: [
          finding({ disposition_id: "d-a", finding_id: "f-a", repo_id: "repo-uzi", repo_path: "vtmocanu/uzi", last_title: "uzi bug" }),
          finding({ disposition_id: "d-b", finding_id: "f-b", repo_id: "repo-atlas", repo_path: "vtmocanu/atlas-api", last_title: "atlas bug" }),
        ],
      }),
    );
    renderFindings();
    await waitFor(() => expect(screen.getByText("uzi bug")).toBeTruthy());
    expect(screen.getByRole("heading", { name: "vtmocanu/uzi" })).toBeTruthy();
    expect(screen.getByRole("heading", { name: "vtmocanu/atlas-api" })).toBeTruthy();
    expect(mockApi.listFindings).toHaveBeenCalledWith("to_file", undefined, undefined);
  });

  it("renders a null finding_id row display-only, with no File/Dismiss actions", async () => {
    mockApi.listFindings.mockResolvedValue(
      backlog({
        bucket: "filed",
        repo: "repo-uzi",
        findings: [
          finding({ disposition_id: "d-orphan", finding_id: undefined, status: "filed", filed_issue_iid: 488, last_title: "orphaned filed" }),
        ],
      }),
    );
    renderFindings(["/findings?bucket=filed&repo=repo-uzi"]);
    await waitFor(() => expect(screen.getByText("orphaned filed")).toBeTruthy());
    expect(screen.queryByRole("button", { name: "File issue" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Dismiss ▾" })).toBeNull();
    expect(screen.queryByRole("checkbox")).toBeNull();
  });
});

// The BLK-BADGE regression, adapted to Findings (PRD #1183 M4): mounting the page INSIDE the real
// AppShell is the only configuration in which the nav badge's PROPAGATION is observable — apart,
// the badge and the page are always both correct. After a dismiss (no navigation), the page
// publishes the fresh stats.todo through FindingsOpenContext and the nav badge must move.
describe("Findings nav badge moves after a dismiss without navigation (PRD #1183 M4)", () => {
  function navBadgeText() {
    return screen.getByRole("link", { name: /^Findings/ }).textContent ?? "";
  }

  it("agrees on load, then follows a dismiss with no navigation", async () => {
    let todo = 3;
    mockApi.getFindingsStats.mockImplementation(async () => triage({ total: 5, todo, filed: 1, done: 1, dismissed: 5 - todo - 2 }));
    mockApi.listFindings.mockResolvedValue(backlog({ open_count: 3, findings: [finding()] }));
    mockApi.dismissFindings.mockImplementation(async () => {
      todo = 2; // the dismiss took one open coordinate out
      return {
        updated: 1,
        findings: [finding({ disposition_id: "disp-1", status: "dismissed", dismiss_reason: "wont_do", last_title: "Leaked ticker in sweepLoop" })],
      };
    });

    renderFindingsInShell();
    // The badge reads the canonical open count.
    await waitFor(() => expect(navBadgeText()).toContain("3"));
    expect(screen.getByRole("tab", { name: /To triage/ }).textContent).toContain("3");

    // Dismiss the one open row — no route change.
    fireEvent.click(screen.getByRole("button", { name: "Dismiss ▾" }));
    fireEvent.click(within(screen.getByRole("menu")).getByText("Won't do"));

    // The badge follows the fresh stats.todo, published through the context (no navigation).
    await waitFor(() => expect(navBadgeText()).toContain("2"));
    expect(navBadgeText()).not.toContain("3");
  });
});
