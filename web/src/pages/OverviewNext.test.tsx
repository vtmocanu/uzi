// @vitest-environment jsdom
// PRD #3878 M1: the /overview/next preview page.
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { act, cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { OverviewNext } from "./OverviewNext";
import { Dashboard } from "./Dashboard";
import { api } from "../lib/api";
import { useAuth } from "../auth/AuthContext";
import { healthySilentDoc, overviewNextDoc } from "../mocks/data";
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
      getAdminHealth: vi.fn(),
    },
  };
});
vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));

const mockApi = vi.mocked(api);
const admin = { ...aUser, is_admin: true };

// Every endpoint a non-admin must never reach.
const adminEndpoints = () => [mockApi.adminListRuns, mockApi.getAdminUsage, mockApi.getAdminHealth];

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
  mockApi.getAdminHealth.mockResolvedValue(healthySilentDoc());
  mockApi.getRecoveryHolds.mockResolvedValue(noHolds());
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.clearAllMocks();
});

function noHolds() {
  return {
    aggregate: { open_holds: 0, admission_counted_holds: 0, custody_hold_limit: 8, decision_needed: 0, blocked_runs: 0 },
    holds: [],
  };
}

async function settle(ms = 0) {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(ms);
  });
}

async function mount(page: "next" | "dashboard" = "next") {
  const view = render(<MemoryRouter>{page === "next" ? <OverviewNext /> : <Dashboard />}</MemoryRouter>);
  await settle();
  return view;
}

const sentence = () => screen.getByText(/run(s are| is) executing/).textContent ?? "";

describe("OverviewNext for a non-admin", () => {
  it("shows the greeting, the calm sentence and the run, and calls no admin endpoint", async () => {
    await mount();
    await settle(30_000);
    expect(screen.getByRole("heading", { name: "Welcome, Admin" })).toBeTruthy();
    expect(sentence()).toBe("Nothing needs you right now. 1 run is executing.");
    expect(screen.getByText("Live run row")).toBeTruthy();
    for (const call of adminEndpoints()) expect(call).not.toHaveBeenCalled();
    // The health chip is admin-only.
    expect(screen.queryByRole("link", { name: /System health/ })).toBeNull();
  });

  it("links back to the current Overview, and Dashboard links here", async () => {
    await mount();
    expect(screen.getByRole("link", { name: "Back to the current Overview" }).getAttribute("href")).toBe("/dashboard");
    cleanup();
    await mount("dashboard");
    expect(screen.getByRole("link", { name: "Try the new Overview" }).getAttribute("href")).toBe("/overview/next");
  });

  it("lists only the viewer's active runs, with the empty state when there are none", async () => {
    mockApi.listRuns.mockResolvedValue({
      runs: [aRun({ id: "a", issue_title: "Active one" }), aRun({ id: "b", issue_title: "Finished one", status: "completed" })],
    });
    await mount();
    expect(screen.getByText("Active one")).toBeTruthy();
    expect(screen.queryByText("Finished one")).toBeNull();
    cleanup();
    mockApi.listRuns.mockResolvedValue({ runs: [aRun({ status: "completed" })] });
    await mount();
    expect(screen.getByText("No active runs. Start one from a board card.")).toBeTruthy();
  });

  it("keeps the onboarding checklist while setup is incomplete and drops it once every step is done", async () => {
    await mount();
    expect(screen.getByText("Get the factory running")).toBeTruthy();
    expect(screen.getByText("1/4 done")).toBeTruthy();
    cleanup();
    mockApi.listRepos.mockResolvedValue({ repos: [{ enabled: true } as never] });
    mockApi.listConnections.mockResolvedValue({ connections: [{} as never] });
    mockApi.listSecrets.mockResolvedValue({ secrets: [{ kind: "anthropic_token" } as never] });
    mockApi.listWorkers.mockResolvedValue({ workers: [aWorker({ status: "online" })] });
    await mount();
    expect(screen.queryByText("Get the factory running")).toBeNull();
  });
});

describe("OverviewNext for an admin", () => {
  beforeEach(() => {
    vi.mocked(useAuth).mockReturnValue(authValue(admin));
  });

  it("loads the admin run list independently and counts every user's runs across the factory", async () => {
    const own = aRun({ id: "own-1" });
    mockApi.listRuns.mockResolvedValue({ runs: [own] });
    mockApi.adminListRuns.mockResolvedValue({
      runs: [
        // The admin list also carries the admin's own run: counted once, as the viewer's row.
        { ...own, owner_email: admin.email },
        aRun({ id: "x-1", status: "running", owner_email: "mira@uzi.local" }),
        aRun({ id: "x-2", status: "queued", owner_email: "andrei@uzi.local" }),
      ],
    });
    await mount();
    expect(mockApi.adminListRuns).toHaveBeenCalledWith({ view: "summary" });
    expect(mockApi.adminListRuns).toHaveBeenCalledTimes(1);
    expect(sentence()).toBe("Nothing needs you right now. 2 runs are executing across the factory and 1 queued.");
    // Other users' rows are not in the temporary viewer-only Runs card.
    expect(screen.getAllByText("Live run row")).toHaveLength(1);
    await settle(10_000);
    expect(mockApi.adminListRuns).toHaveBeenCalledTimes(2);
  });

  it("keeps every personal section and counts only the viewer's runs when the admin fetch fails", async () => {
    mockApi.adminListRuns.mockRejectedValue(new Error("admin api down"));
    await mount();
    expect(screen.getByText("Live run row")).toBeTruthy();
    expect(sentence()).toBe("Nothing needs you right now. 1 run is executing. Counting only your runs.");
    expect(sentence()).not.toContain("across the factory");
    expect(screen.getByRole("heading", { name: "Welcome, Admin" })).toBeTruthy();
  });

  it("falls back to the viewer's runs when a later admin read fails, and recovers on the next success", async () => {
    mockApi.adminListRuns.mockResolvedValue({
      runs: [aRun({ id: "x-1", status: "running", owner_email: "mira@uzi.local" })],
    });
    await mount();
    expect(sentence()).toContain("2 runs are executing across the factory");
    mockApi.adminListRuns.mockRejectedValueOnce(new Error("blip"));
    await settle(10_000);
    expect(sentence()).toBe("Nothing needs you right now. 1 run is executing. Counting only your runs.");
    await settle(10_000);
    expect(sentence()).toContain("2 runs are executing across the factory");
  });

  it("does not fail the personal load when the admin request is the only failure", async () => {
    mockApi.adminListRuns.mockRejectedValue(new Error("403"));
    await mount();
    expect(screen.queryByText("Get the factory running")).not.toBeNull();
    expect(screen.getByText("Live run row")).toBeTruthy();
  });

  it("shows the health chip with the verdict for a non-passing document and a pass count for a clean one", async () => {
    mockApi.getAdminHealth.mockResolvedValue(overviewNextDoc());
    await mount();
    const chip = screen.getByRole("link", { name: /System health/ });
    expect(chip.getAttribute("href")).toBe("/admin/health");
    expect(chip.textContent).toContain("1 check needs attention");
    cleanup();
    const clean = healthySilentDoc();
    mockApi.getAdminHealth.mockResolvedValue(clean);
    await mount();
    const applicable = clean.checks.length - clean.counts.na;
    expect(screen.getByRole("link", { name: /System health/ }).textContent).toContain(
      `${clean.counts.ok} of ${applicable} checks passing`,
    );
  });
});

describe("carried-over sections take the same inputs as Dashboard", () => {
  it("feeds the custody alert the viewer's own recovery_wait count only, never the admin rows", async () => {
    vi.mocked(useAuth).mockReturnValue(authValue(admin));
    mockApi.listRuns.mockResolvedValue({
      runs: [aRun({ id: "own-1", status: "recovery_wait" }), aRun({ id: "own-2", status: "recovery_wait" })],
    });
    mockApi.adminListRuns.mockResolvedValue({
      runs: Array.from({ length: 5 }, (_, i) =>
        aRun({ id: `other-${i}`, status: "recovery_wait", owner_email: "mira@uzi.local" }),
      ),
    });
    mockApi.getRecoveryHolds.mockResolvedValue({
      aggregate: { open_holds: 1, admission_counted_holds: 1, custody_hold_limit: 8, decision_needed: 1, blocked_runs: 0 },
      holds: [],
    });
    await mount("dashboard");
    const onDashboard = screen.getByRole("status", { name: "Held work" }).textContent;
    expect(onDashboard).toContain("2 runs waiting to recover");
    cleanup();
    await mount("next");
    const onPreview = screen.getByRole("status", { name: "Held work" }).textContent;
    expect(onPreview).toContain("2 runs waiting to recover");
    expect(onPreview).toBe(onDashboard);
  });

  it("shows a non-admin the same hosted-workers platform line as Dashboard", async () => {
    mockApi.listRuns.mockResolvedValue({ runs: [aRun({ status: "queued" })] });
    mockApi.listWorkers.mockResolvedValue({
      workers: [aWorker({ id: "h1", kind: "hosted", status: "offline", upgrade_status: "capacity_blocked" })],
    });
    await mount("dashboard");
    const onDashboard = screen.getByRole("status", { name: "Your hosted workers" }).textContent;
    expect(onDashboard).toContain("Your hosted workers cannot start right now.");
    cleanup();
    await mount("next");
    expect(screen.getByRole("status", { name: "Your hosted workers" }).textContent).toBe(onDashboard);
  });

  it("shows an admin no platform line on the preview, as on Dashboard", async () => {
    vi.mocked(useAuth).mockReturnValue(authValue(admin));
    mockApi.listRuns.mockResolvedValue({ runs: [aRun({ status: "queued" })] });
    mockApi.listWorkers.mockResolvedValue({
      workers: [aWorker({ id: "h1", kind: "hosted", status: "offline", upgrade_status: "capacity_blocked" })],
    });
    await mount("next");
    expect(screen.queryByRole("status", { name: "Your hosted workers" })).toBeNull();
  });
});

describe("Needs your decision", () => {
  const prRun = (over = {}) =>
    aRun({ id: "pr-1", status: "completed", kind: "issue", mr_iid: 4, mr_state: "opened", mr_web_url: "https://github.com/o/r/pull/4", forge_type: "github", issue_title: "Open PR run", finished_at: "2026-07-05T12:00:00Z", ...over });

  it("lists a plan to review, then drops the card once the run moves on", async () => {
    mockApi.listRuns.mockResolvedValue({ runs: [aRun({ id: "plan-1", status: "awaiting_approval", issue_title: "Plan me" })] });
    await mount();
    expect(screen.getByText("Needs your decision")).toBeTruthy();
    expect(screen.getByRole("link", { name: "Review plan" }).getAttribute("href")).toBe("/runs/plan-1");
    expect(screen.getByText(/^1 decision is waiting on you\./)).toBeTruthy();
    mockApi.listRuns.mockResolvedValue({ runs: [aRun({ id: "plan-1", status: "running", issue_title: "Plan me" })] });
    await settle(10_000);
    expect(screen.queryByText("Needs your decision")).toBeNull();
    expect(screen.getByText(/^Nothing needs you right now\./)).toBeTruthy();
  });

  it("counts a plan and an open pull request in the sentence", async () => {
    mockApi.listRuns.mockResolvedValue({ runs: [aRun({ id: "plan-1", status: "awaiting_approval" }), prRun()] });
    await mount();
    expect(screen.getByText(/^1 decision and 1 pull request are waiting on you\./)).toBeTruthy();
    expect(screen.getByRole("link", { name: "Review PR" }).getAttribute("href")).toBe("https://github.com/o/r/pull/4");
  });

  it("never lists another user's item for an admin, and counts none", async () => {
    vi.mocked(useAuth).mockReturnValue(authValue(admin));
    mockApi.listRuns.mockResolvedValue({ runs: [aRun()] });
    mockApi.adminListRuns.mockResolvedValue({
      runs: [aRun({ id: "x-1", status: "awaiting_approval", owner_email: "mira@uzi.local", issue_title: "Theirs" })],
    });
    await mount();
    expect(screen.queryByText("Needs your decision")).toBeNull();
    expect(screen.queryByText("Theirs")).toBeNull();
    expect(screen.getByText(/^Nothing needs you right now\./)).toBeTruthy();
  });
});
