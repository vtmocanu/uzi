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
  created_at: "2026-01-01T00:00:00Z",
  last_login: null,
};

function aRun(over: Partial<RunListItem> = {}): RunListItem {
  return {
    id: "run-1",
    repo_id: "repo-1",
    kind: "issue",
    issue_iid: 7,
    issue_title: "Live run row",
    issue_description: "",
    status: "running",
    requeue_count: 0,
    iteration_count: 0,
    auto_approve: false,
    worker_id: "w1",
    branch: null,
    mr_iid: null,
    failure_reason: null,
    pipeline_ref: null,
    pipeline_web_url: null,
    fix_verdict: null,
    plan_md: null,
    claimed_at: null,
    started_at: null,
    finished_at: null,
    created_at: "2026-07-05T12:00:00Z",
    updated_at: "2026-07-05T12:00:00Z",
    repo_path: "vtmocanu/uzi",
    worker_name: "laptop",
    ...over,
  };
}

function aWorker(over: Partial<Worker> = {}): Worker {
  return {
    id: "w1",
    name: "laptop",
    status: "online",
    busy: false,
    template_declared: null,
    template_reported: null,
    version: null,
    last_heartbeat_at: null,
    created_at: "2026-07-05T12:00:00Z",
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
});

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
