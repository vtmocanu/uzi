// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { RunsList } from "./RunsList";
import { api, type RunListItem } from "../lib/api";
import { useAuth } from "../auth/AuthContext";

// Keep the real module (isTerminalRun etc.) and mock only the network + auth so the
// list renders offline. A non-admin viewer skips the admin fetches.
vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      listRuns: vi.fn(),
      adminListRuns: vi.fn(),
      adminListWorkers: vi.fn(),
      // PRD #94: the global strip fetches this on mount. Default to an empty backlog so
      // the pre-existing tests keep the strip hidden; the strip tests override it.
      getJudgeStats: vi
        .fn()
        .mockResolvedValue({ total: 0, todo: 0, filed: 0, done: 0, dismissed: 0, false_positives: 0 }),
    },
  };
});
vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));

const mockApi = vi.mocked(api);

function aRun(over: Partial<RunListItem> = {}): RunListItem {
  return {
    id: "run-1",
    repo_id: "repo-1",
    forge_type: "gitlab",
    mr_web_url: null,
    kind: "issue",
    issue_iid: 7,
    issue_title: "A run",
    issue_description: "",
    title: null,
    resume_of_run_id: null,
    status: "running",
    requeue_count: 0,
    iteration_count: 0,
    auto_approve: false,
    worker_id: "w1",
    branch: null,
    mr_iid: null,
    mr_state: null,
    failure_reason: null,
    stop_kind: null,
    health: "ok",
    health_reason: null,
    health_since: null,
    pipeline_ref: null,
    pipeline_web_url: null,
    fix_verdict: null,
    plan_md: null,
    repo_agents: null,
    agent_source: null,
    agent_exclusions: null,
    own_agents: null,
    claimed_at: null,
    started_at: null,
    finished_at: null,
    created_at: "2026-07-05T12:00:00Z",
    updated_at: "2026-07-05T12:00:00Z",
    repo_path: "grp/proj",
    worker_name: "w1",
    // PRD #98 M4: unjudged by default, so existing assertions are unchanged.
    judge_verdict: null,
    judge_todo_count: 0,
    ...over,
  };
}

beforeEach(() => {
  vi.mocked(useAuth).mockReturnValue({ user: { is_admin: false } } as unknown as ReturnType<typeof useAuth>);
});
afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("RunsList — waiting for vault unlock (PRD #32)", () => {
  it("renders own queued runs as waiting for vault unlock while locked", async () => {
    vi.mocked(useAuth).mockReturnValue({
      user: { is_admin: false },
      vaultUnlocked: false,
    } as unknown as ReturnType<typeof useAuth>);
    mockApi.listRuns.mockResolvedValue({
      runs: [aRun({ id: "q", issue_title: "Queued run", status: "queued" })],
    });

    render(
      <MemoryRouter>
        <RunsList />
      </MemoryRouter>,
    );

    await waitFor(() => expect(screen.getByText("Queued run")).toBeTruthy());
    expect(screen.getByText(/waiting for vault unlock/)).toBeTruthy();
    // The bare "queued" pill must not also render for that run.
    expect(screen.queryByText("queued")).toBeNull();
  });

  it("admin all-users list: only the admin's OWN queued row shows waiting-for-unlock", async () => {
    vi.mocked(useAuth).mockReturnValue({
      user: { is_admin: true, email: "me@uzi.test" },
      vaultUnlocked: false,
    } as unknown as ReturnType<typeof useAuth>);
    mockApi.listRuns.mockResolvedValue({ runs: [] });
    mockApi.adminListWorkers.mockResolvedValue({ workers: [] });
    mockApi.adminListRuns.mockResolvedValue({
      runs: [
        aRun({ id: "mine", issue_title: "My queued", status: "queued", owner_email: "me@uzi.test" }),
        aRun({ id: "theirs", issue_title: "Their queued", status: "queued", owner_email: "other@uzi.test" }),
      ],
    });

    render(
      <MemoryRouter>
        <RunsList />
      </MemoryRouter>,
    );

    await waitFor(() => expect(screen.getByText("My queued")).toBeTruthy());
    // Exactly one waiting badge — the admin's own row; the other owner's row stays
    // a plain "queued" pill (their vault state is unknown here).
    expect(screen.getAllByText(/waiting for vault unlock/)).toHaveLength(1);
    expect(screen.getByText("queued")).toBeTruthy();
  });

  it("renders a plain queued pill when the vault is unlocked", async () => {
    vi.mocked(useAuth).mockReturnValue({
      user: { is_admin: false },
      vaultUnlocked: true,
    } as unknown as ReturnType<typeof useAuth>);
    mockApi.listRuns.mockResolvedValue({
      runs: [aRun({ id: "q", issue_title: "Queued run", status: "queued" })],
    });

    render(
      <MemoryRouter>
        <RunsList />
      </MemoryRouter>,
    );

    await waitFor(() => expect(screen.getByText("Queued run")).toBeTruthy());
    expect(screen.getByText("queued")).toBeTruthy();
    expect(screen.queryByText(/waiting for vault unlock/)).toBeNull();
  });
});

describe("RunsList — autopilot badge", () => {
  it("shows the autopilot badge only for an auto_approve run", async () => {
    mockApi.listRuns.mockResolvedValue({
      runs: [
        aRun({ id: "auto", issue_title: "Autopilot run", auto_approve: true }),
        aRun({ id: "manual", issue_title: "Manual run", auto_approve: false }),
      ],
    });

    render(
      <MemoryRouter>
        <RunsList />
      </MemoryRouter>,
    );

    await waitFor(() => expect(screen.getByText("Autopilot run")).toBeTruthy());
    expect(screen.getByText("Manual run")).toBeTruthy();
    // Exactly one badge — the manual run must not carry it.
    expect(screen.getAllByText("autopilot")).toHaveLength(1);
  });
});

describe("RunsList — usage meta line (PRD #40)", () => {
  it("adds tokens + cost to a run with usage (running → 'so far'), nothing to a run without", async () => {
    mockApi.listRuns.mockResolvedValue({
      runs: [
        aRun({
          id: "with",
          issue_title: "Has usage",
          status: "running",
          usage: { input_tokens: 114_400, cache_read_tokens: 1_170_000, cache_creation_tokens: 0, output_tokens: 48_200, cost_usd: 1.87 },
        }),
        aRun({ id: "without", issue_title: "No usage", status: "running" }),
      ],
    });

    render(
      <MemoryRouter>
        <RunsList />
      </MemoryRouter>,
    );

    await waitFor(() => expect(screen.getByText("Has usage")).toBeTruthy());
    // total = 114.4k + 1.17M + 0 + 48.2k = 1,332,600 → "1.33M tok so far" (running).
    expect(screen.getByText(/1\.33M tok so far/)).toBeTruthy();
    expect(screen.getByText(/\$1\.87/)).toBeTruthy();
    // The no-usage run contributes no "tok" figure — exactly one run shows usage.
    expect(screen.queryAllByText(/tok/).length).toBe(1);
  });
})

describe("RunsList — global judge-triage strip (PRD #94)", () => {
  it("renders the strip with counts from getJudgeStats (ignores the runs list)", async () => {
    mockApi.listRuns.mockResolvedValue({ runs: [] });
    mockApi.getJudgeStats.mockResolvedValue({
      total: 47,
      todo: 18,
      filed: 9,
      done: 12,
      dismissed: 8,
      false_positives: 3,
    });

    render(
      <MemoryRouter>
        <RunsList />
      </MemoryRouter>,
    );

    await waitFor(() =>
      expect(screen.getByText("Judge recommendations · all your runs")).toBeTruthy(),
    );
    // Counts come straight from the global stats, not the (empty) runs list.
    expect(screen.getByText("18")).toBeTruthy();
    expect(screen.getByText("9")).toBeTruthy();
    expect(screen.getByText("12")).toBeTruthy();
    expect(screen.getByText("8")).toBeTruthy();
    expect(screen.getByText(/3 of 8 dismissed were false positives/)).toBeTruthy();
  });

  it("hides the strip when the backlog is empty (total 0)", async () => {
    mockApi.listRuns.mockResolvedValue({ runs: [] });
    mockApi.getJudgeStats.mockResolvedValue({
      total: 0,
      todo: 0,
      filed: 0,
      done: 0,
      dismissed: 0,
      false_positives: 0,
    });

    render(
      <MemoryRouter>
        <RunsList />
      </MemoryRouter>,
    );

    await waitFor(() => expect(screen.getByText("No runs yet")).toBeTruthy());
    expect(screen.queryByText("Judge recommendations · all your runs")).toBeNull();
  });
})
