// @vitest-environment jsdom
// PRD #3878 M1: Dashboard reads its data through useOverviewData WITHOUT the admin-runs
// option, so it sends exactly the requests it always has and never the admin run list.
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { act, cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { Dashboard } from "./Dashboard";
import { api } from "../lib/api";
import { useAuth } from "../auth/AuthContext";
import { aRun, aUser, aWorker, authValue, emptySelf } from "./overviewFixtures";

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
      adminListRuns: vi.fn(),
      getRecoveryHolds: vi.fn(),
    },
  };
});
vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));

const mockApi = vi.mocked(api);

beforeEach(() => {
  vi.useFakeTimers();
  localStorage.clear();
  Object.defineProperty(document, "hidden", { configurable: true, get: () => false });
  vi.mocked(useAuth).mockReturnValue(authValue(aUser));
  mockApi.listRuns.mockResolvedValue({ runs: [aRun()] });
  mockApi.listWorkers.mockResolvedValue({ workers: [aWorker()] });
  mockApi.listRepos.mockResolvedValue({ repos: [] });
  mockApi.listAgentTemplates.mockResolvedValue({ templates: [] });
  mockApi.listSecrets.mockResolvedValue({ secrets: [] });
  mockApi.listConnections.mockResolvedValue({ connections: [] });
  mockApi.getUsage.mockResolvedValue(emptySelf());
  mockApi.getAdminUsage.mockResolvedValue({ factory: emptySelf(), users: [], earliest_run: null });
  mockApi.adminListRuns.mockResolvedValue({ runs: [] });
  mockApi.getRecoveryHolds.mockResolvedValue({
    aggregate: { open_holds: 0, admission_counted_holds: 0, custody_hold_limit: 8, decision_needed: 0, blocked_runs: 0 },
    holds: [],
  });
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.clearAllMocks();
});

async function mountDashboard() {
  const view = render(
    <MemoryRouter>
      <Dashboard />
    </MemoryRouter>,
  );
  await act(async () => {
    await vi.advanceTimersByTimeAsync(0);
  });
  return view;
}

describe("Dashboard on the shared overview data hook", () => {
  it("sends today's first-load requests for a non-admin, with no admin request at all", async () => {
    await mountDashboard();
    expect(mockApi.listRuns).toHaveBeenCalledTimes(1);
    expect(mockApi.listRuns).toHaveBeenCalledWith({ view: "summary" });
    for (const call of [
      mockApi.listWorkers,
      mockApi.listRepos,
      mockApi.listAgentTemplates,
      mockApi.listSecrets,
      mockApi.listConnections,
      mockApi.getUsage,
    ]) {
      expect(call).toHaveBeenCalledTimes(1);
    }
    expect(mockApi.getAdminUsage).not.toHaveBeenCalled();
    expect(mockApi.adminListRuns).not.toHaveBeenCalled();
  });

  it("never calls adminListRuns for an admin, on first load or on the 10 s poll, and still loads admin usage", async () => {
    vi.mocked(useAuth).mockReturnValue(authValue({ ...aUser, is_admin: true }));
    await mountDashboard();
    expect(mockApi.getAdminUsage).toHaveBeenCalledTimes(1);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(10_000);
    });
    // The poll re-fetched runs and workers (first load + one poll) and nothing admin-run related.
    expect(mockApi.listRuns).toHaveBeenCalledTimes(2);
    expect(mockApi.listWorkers).toHaveBeenCalledTimes(2);
    expect(mockApi.listRepos).toHaveBeenCalledTimes(1);
    expect(mockApi.adminListRuns).not.toHaveBeenCalled();
  });

  it("links to the preview Overview and renders the same Recent runs", async () => {
    await mountDashboard();
    expect(screen.getByRole("link", { name: "Try the new Overview" }).getAttribute("href")).toBe("/overview/next");
    expect(screen.getByText("Recent runs")).toBeTruthy();
    expect(screen.getByText("Live run row")).toBeTruthy();
  });
});
