// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { act, cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { Dashboard } from "./Dashboard";
import { api, type RunListItem, type Worker } from "../lib/api";
import { useAuth } from "../auth/AuthContext";

// The overview fetches six endpoints on first load and re-polls only listRuns +
// listWorkers every 10s. Mock the api (keep the real isTerminalRun) and useAuth so
// this stays offline; drive the poll with fake timers.
vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      listRuns: vi.fn(),
      listWorkers: vi.fn(),
      listRepos: vi.fn(),
      listAgentTemplates: vi.fn(),
      listSecrets: vi.fn(),
      listConnections: vi.fn(),
      getUsage: vi.fn(),
      getAdminUsage: vi.fn(),
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
  judge_enabled: false,
  judge_anthropic_secret_id: null,
  judge_anthropic_secret_label: null,
  created_at: "2026-01-01T00:00:00Z",
  last_login: null,
};

function aRun(over: Partial<RunListItem> = {}): RunListItem {
  return {
    id: "run-1",
    repo_id: "repo-1",
    forge_type: "gitlab",
    mr_web_url: null,
    kind: "issue",
    issue_iid: 7,
    issue_title: "Live run row",
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
    anthropic_secret_id: null,
    anthropic_secret_label: null,
    claimed_at: null,
    started_at: null,
    finished_at: null,
    created_at: "2026-07-05T12:00:00Z",
    updated_at: "2026-07-05T12:00:00Z",
    repo_path: "vtmocanu/uzi",
    worker_name: "laptop",
    // PRD #98 M4: unjudged by default, so existing assertions are unchanged.
    judge_verdict: null,
    judge_todo_count: 0,
    ...over,
  };
}

function aWorker(over: Partial<Worker> = {}): Worker {
  return {
    id: "w1",
    name: "laptop",
    status: "online",
    busy: false,
    kind: "external",
    hosted_size: null,
    active_runs: 0,
    max_concurrent_runs: null,
    template_declared: null,
    template_reported: null,
    version: null,
    upgrade_status: "unknown",
    upgrade_detail: null,
    upgrade_target: "",
    upgrade_blocking_container: null,
    upgrade_blocking_reason: null,
    upgrade_last_exit_code: null,
    last_heartbeat_at: null,
    created_at: "2026-07-05T12:00:00Z",
    stats_cpu_pct: null,
    stats_mem_bytes: null,
    stats_mem_limit_bytes: null,
    stats_source: null,
    anthropic_secret_id: null,
    anthropic_secret_label: null,
    anthropic_bind_mode: "default",
    ...over,
  };
}

function renderDashboard() {
  return render(
    <MemoryRouter>
      <Dashboard />
    </MemoryRouter>,
  );
}

beforeEach(() => {
  vi.mocked(useAuth).mockReturnValue({
    user,
    loading: false,
    prdLabel: "PRD",
    autopilotLabel: "autopilot",
    theme: "ember",
    themeOverride: null,
    defaultTheme: "ember",
    prdlessLabel: "PRDLESS",
    prdlessEnabled: false,
    vaultUnlocked: true,
    vaultExists: true,
    hasPassword: true,
    register: vi.fn(),
    login: vi.fn(),
    logout: vi.fn(),
    refresh: vi.fn(),
  });
  // Deterministic visibility so usePollWhileVisible ticks.
  Object.defineProperty(document, "hidden", { configurable: true, get: () => false });
  mockApi.listRuns.mockResolvedValue({ runs: [aRun()] });
  mockApi.listWorkers.mockResolvedValue({ workers: [aWorker()] });
  mockApi.listRepos.mockResolvedValue({ repos: [] });
  mockApi.listAgentTemplates.mockResolvedValue({ templates: [] });
  mockApi.listSecrets.mockResolvedValue({ secrets: [] });
  mockApi.listConnections.mockResolvedValue({ connections: [] });
  // Default: no usage yet (the run_count===0 "nothing yet" state).
  mockApi.getUsage.mockResolvedValue(emptySelf());
  mockApi.getAdminUsage.mockResolvedValue({ factory: emptySelf(), users: [], earliest_run: null });
});

const zeros = () => ({ input_tokens: 0, cache_read_tokens: 0, cache_creation_tokens: 0, output_tokens: 0, cost_usd: 0 });
function emptySelf() {
  return { lifetime: zeros(), last_7_days: zeros(), run_count: 0 };
}

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.clearAllMocks();
});

describe("Dashboard first-load / re-poll error split", () => {
  it("keeps skeletons (renders no tiles) when the first load fails", async () => {
    mockApi.listRuns.mockRejectedValueOnce(new Error("api down"));
    renderDashboard();
    // Let the first-load Promise.all reject and the catch run.
    await act(async () => {
      await new Promise((r) => setTimeout(r, 0));
    });

    expect(screen.getByText(/Welcome/)).toBeTruthy(); // mounted (past the !user guard)
    expect(screen.queryByText("Active runs")).toBeNull(); // no stat tiles → still skeleton
    expect(screen.queryByText("Live run row")).toBeNull(); // no recent-runs rows
  });

  it("keeps the last-good data when a re-poll fails, never blanking back to skeletons", async () => {
    vi.useFakeTimers();
    renderDashboard();
    // Flush the successful first load.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(screen.getByText("Active runs")).toBeTruthy(); // tiles rendered
    expect(screen.getByText("Live run row")).toBeTruthy();

    // The next poll's listRuns fails; the poll must swallow it and keep last-good.
    mockApi.listRuns.mockRejectedValueOnce(new Error("poll blip"));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(10000);
    });

    expect(mockApi.listRuns).toHaveBeenCalledTimes(2); // first load + one poll
    expect(screen.getByText("Active runs")).toBeTruthy(); // still showing the tiles
    expect(screen.getByText("Live run row")).toBeTruthy(); // last-good data intact
  });
});

describe("Dashboard usage cards (PRD #40)", () => {
  const bundle = (inp: number, cr: number, out: number, cost: number) => ({
    input_tokens: inp,
    cache_read_tokens: cr,
    cache_creation_tokens: 0,
    output_tokens: out,
    cost_usd: cost,
  });
  const selfWithUsage = {
    lifetime: bundle(1_610_000, 16_100_000, 710_000, 26.4),
    last_7_days: bundle(200_000, 2_800_000, 100_000, 4.55),
    run_count: 23,
  };
  const adminUsage = {
    factory: { lifetime: bundle(5_400_000, 53_900_000, 2_400_000, 88.15), last_7_days: zeros(), run_count: 79 },
    users: [
      { user_id: "a", email: "vlad@example.com", usage: bundle(1_610_000, 16_100_000, 710_000, 26.4), run_count: 23 },
      { user_id: "b", email: "maria@example.com", usage: bundle(2_490_000, 21_400_000, 1_020_000, 37.83), run_count: 31 },
    ],
    earliest_run: "2026-05-12T09:00:00Z",
  };

  const settle = async () => {
    await act(async () => {
      await new Promise((r) => setTimeout(r, 0));
    });
  };

  it("shows the Your usage card for any user (lifetime total + last-7-days kicker)", async () => {
    mockApi.getUsage.mockResolvedValue(selfWithUsage);
    const { container } = renderDashboard();
    await settle();
    expect(screen.getByText("Your usage")).toBeTruthy();
    // lifetime total = 1.61M + 16.1M + 0.71M = 18.42M tokens.
    expect(container.textContent).toContain("18.42M");
    expect(screen.getByText(/Across/)).toBeTruthy();
    expect(screen.getByText(/in the last 7 days/)).toBeTruthy();
  });

  it("renders the empty 'nothing yet' state when the user has no usage", async () => {
    // Default getUsage mock returns run_count 0.
    renderDashboard();
    await settle();
    expect(screen.getByText("Your usage")).toBeTruthy();
    expect(screen.getByText(/No usage recorded yet/)).toBeTruthy();
  });

  it("an admin sees the Factory total card + per-user breakdown", async () => {
    vi.mocked(useAuth).mockReturnValue({ user: { ...user, is_admin: true } } as unknown as ReturnType<typeof useAuth>);
    mockApi.getUsage.mockResolvedValue(selfWithUsage);
    mockApi.getAdminUsage.mockResolvedValue(adminUsage);
    renderDashboard();
    await settle();

    expect(screen.getByText(/Factory total/)).toBeTruthy();
    expect(screen.getByText(/Per-user breakdown/)).toBeTruthy();
    expect(screen.getByText("vlad@example.com")).toBeTruthy();
    expect(screen.getByText("maria@example.com")).toBeTruthy();
    expect(screen.getByText("uzi total")).toBeTruthy();
  });

  it("a NON-admin never sees factory data and never calls the admin endpoint", async () => {
    // Default user has is_admin false.
    mockApi.getUsage.mockResolvedValue(selfWithUsage);
    renderDashboard();
    await settle();

    expect(screen.getByText("Your usage")).toBeTruthy();
    expect(screen.queryByText(/Factory total/)).toBeNull();
    expect(screen.queryByText(/Per-user breakdown/)).toBeNull();
    expect(mockApi.getAdminUsage).not.toHaveBeenCalled();
  });
});
