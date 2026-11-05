// @vitest-environment jsdom
// PRD #3878 M1: the /overview/next preview page.
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { act, cleanup, render, screen, within } from "@testing-library/react";
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
    expect(screen.queryByText(/Other users/)).toBeNull();
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
    // The admin's own row is shown once, under Yours; the other users' rows under Other users.
    expect(within(screen.getByRole("list", { name: "Your runs" })).getAllByText("Live run row")).toHaveLength(1);
    expect(within(screen.getByRole("list", { name: "Other users' runs" })).getAllByText("Live run row")).toHaveLength(2);
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

  it("shows both run groups, and says so when the admin list becomes unavailable", async () => {
    mockApi.listRuns.mockResolvedValue({ runs: [aRun({ id: "own-1", issue_title: "Mine" })] });
    mockApi.adminListRuns.mockResolvedValue({
      runs: [aRun({ id: "x-1", owner_email: "mira@uzi.local", issue_title: "Theirs" })],
    });
    await mount();
    expect(screen.getByText("Yours")).toBeTruthy();
    expect(screen.getByText("Other users (admin)")).toBeTruthy();
    expect(within(screen.getByRole("list", { name: "Other users' runs" })).getByText("Theirs")).toBeTruthy();
    mockApi.adminListRuns.mockRejectedValueOnce(new Error("blip"));
    await settle(10_000);
    expect(screen.getByRole("status").textContent).toBe("Other users' runs are unavailable");
    expect(screen.getByRole("status").parentElement?.textContent).toMatch(/unavailable \(last read/);
    expect(screen.queryByText("Theirs")).toBeNull();
    expect(screen.getByText("Mine")).toBeTruthy();
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

describe("Capacity rail", () => {
  it("lists every owner worker on the rail, including one with no stats, and drops Worker load", async () => {
    mockApi.listWorkers.mockResolvedValue({
      workers: [
        aWorker({ id: "w1", name: "statless-worker", active_runs: 1, max_concurrent_runs: 2 }),
        aWorker({ id: "w2", name: "gone-worker", status: "offline" }),
        // Stats-bearing (hasStats) so WorkerLoadCard would render if it were still mounted.
        aWorker({
          id: "w3",
          name: "loaded-worker",
          stats_cpu_pct: 10,
          stats_mem_bytes: 1024,
          stats_source: "cgroup",
        }),
      ],
    });
    await mount();
    expect(screen.queryByText("Worker load")).toBeNull();
    const rail = screen.getByRole("heading", { name: /Capacity/ }).closest("section") as HTMLElement;
    expect(within(rail).getByText("1 of 2")).toBeTruthy();
    expect(within(rail).getByText("statless-worker")).toBeTruthy();
    expect(within(rail).getByText("gone-worker")).toBeTruthy();
    expect(within(rail).getAllByText("No stats reported")).toHaveLength(2);
  });

  it("keeps Worker load on /dashboard", async () => {
    mockApi.listWorkers.mockResolvedValue({
      workers: [aWorker({ stats_cpu_pct: 10, stats_mem_bytes: 1024, stats_source: "cgroup" })],
    });
    await mount("dashboard");
    expect(screen.getByText("Worker load")).toBeTruthy();
  });
});

describe("carried-over sections take the same inputs as Dashboard", () => {
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

function heldAggregate(over: Partial<ReturnType<typeof noHolds>["aggregate"]>) {
  return { ...noHolds().aggregate, ...over };
}

function decisionHold(runId: string, createdAt: string) {
  return {
    id: `hold-${runId}`,
    run_id: runId,
    generation: 1,
    state: "open",
    attention: "source_only",
    worker_id: "w1",
    inventory_guarded: false,
    has_available_capture: false,
    created_at: createdAt,
    updated_at: createdAt,
  };
}

const heldCard = () => screen.getByRole("heading", { name: /Held work/, level: 2 }).closest("section") as HTMLElement;

describe("Held work card", () => {
  it("shows the Dashboard alert's headline and the own-runs-only recovery diagnostic, with no alert copy of its own", async () => {
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
      aggregate: heldAggregate({ open_holds: 1, admission_counted_holds: 1, decision_needed: 1 }),
      holds: [],
    });
    await mount("dashboard");
    const alert = screen.getByRole("status", { name: "Held work" });
    const dashboardHeadline = within(alert).getByRole("heading").textContent;
    expect(alert.textContent).toContain("2 runs waiting to recover");
    cleanup();
    mockApi.getRecoveryHolds.mockClear();

    await mount("next");
    const card = screen.getByRole("status", { name: "Held work" });
    expect(within(card).getByRole("heading", { level: 3 }).textContent).toBe(dashboardHeadline);
    expect(within(card).getByText("2 runs waiting to recover")).toBeTruthy();
    // CustodyBoardAlert's own copy is not on the preview.
    expect(screen.queryByText(/Review the holds that need a decision and choose how to preserve their work\./)).toBeNull();
    expect(screen.getAllByRole("status", { name: "Held work" })).toHaveLength(1);
    // One request per tick: the card takes holds from the hook, it does not fetch its own.
    expect(mockApi.getRecoveryHolds).toHaveBeenCalledTimes(1);
    await settle(10_000);
    expect(mockApi.getRecoveryHolds).toHaveBeenCalledTimes(2);
  });

  it("shows healthy holds as figures with no headline and no decision", async () => {
    mockApi.getRecoveryHolds.mockResolvedValue({
      aggregate: heldAggregate({ open_holds: 2, admission_counted_holds: 2 }),
      holds: [],
    });
    await mount();
    const card = heldCard();
    expect([...card.querySelectorAll("strong")].map((e) => e.textContent)).toEqual(["2 / 8", "2", "0"]);
    expect(within(card).getByText("open custody holds")).toBeTruthy();
    expect(within(card).getByText("holds need a decision")).toBeTruthy();
    expect(within(card).queryByRole("link", { name: "Review held work" })).toBeNull();
    expect(screen.queryByText("Needs your decision")).toBeNull();
    expect(sentence()).toBe("Nothing needs you right now. 1 run is executing.");
  });

  it("counts two decision holds as 2 decisions and lists one held row", async () => {
    mockApi.getRecoveryHolds.mockResolvedValue({
      aggregate: heldAggregate({ open_holds: 2, admission_counted_holds: 2, decision_needed: 2 }),
      holds: [decisionHold("run-a", "2026-07-05T10:00:00Z"), decisionHold("run-b", "2026-07-05T11:00:00Z")],
    });
    await mount();
    expect(sentence()).toBe("2 decisions are waiting on you. 1 run is executing.");
    const needs = screen.getByRole("region", { name: "Needs your decision" });
    expect(within(needs).getAllByRole("listitem")).toHaveLength(1);
    expect(within(needs).getByText("2 held-work holds need a decision")).toBeTruthy();
    expect(within(needs).getByText("2 decisions")).toBeTruthy();
    expect(within(needs).getByRole("link", { name: "Review held work" }).getAttribute("href")).toBe(
      "/workers?tab=workers#recovery-holds",
    );
    // The card's own link is the second one on the page.
    expect(screen.getAllByRole("link", { name: "Review held work" })).toHaveLength(2);
  });

  it("names the run of a single decision hold", async () => {
    mockApi.getRecoveryHolds.mockResolvedValue({
      aggregate: heldAggregate({ open_holds: 1, admission_counted_holds: 1, decision_needed: 1 }),
      holds: [decisionHold("a83c51f0-aaaa", "2026-07-05T10:00:00Z")],
    });
    await mount();
    expect(screen.getByText(/Held work from run/).textContent).toBe("Held work from run a83c51f0 needs a decision");
    expect(sentence()).toBe("1 decision is waiting on you. 1 run is executing.");
  });

  it("keeps blocked runs a danger headline with no review link and no held row", async () => {
    mockApi.getRecoveryHolds.mockResolvedValue({
      aggregate: heldAggregate({ open_holds: 3, admission_counted_holds: 3, blocked_runs: 2 }),
      holds: [],
    });
    await mount();
    const card = screen.getByRole("alert", { name: "Held work" });
    expect(within(card).getByRole("heading", { level: 3 }).textContent).toBe("Held work is blocking new runs");
    expect(within(card).getByRole("heading", { level: 3 }).className).toContain("text-danger");
    expect(screen.queryByRole("link", { name: "Review held work" })).toBeNull();
    expect(screen.queryByText("Needs your decision")).toBeNull();
    expect(sentence()).toBe("Nothing needs you right now. 1 run is executing.");
  });

  it("says held work is unavailable when the holds request fails, and keeps the rest of the page", async () => {
    mockApi.getRecoveryHolds.mockRejectedValue(new Error("down"));
    await mount();
    expect(within(heldCard()).getByText("Held work is unavailable right now.")).toBeTruthy();
    expect(screen.getByText("Live run row")).toBeTruthy();
  });

  it("does not fetch holds on Dashboard beyond its own alert", async () => {
    await mount("dashboard");
    expect(mockApi.getRecoveryHolds).toHaveBeenCalledTimes(1);
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
    // They appear only as a read-only row under Other users, with no action.
    expect(screen.getByText("Theirs")).toBeTruthy();
    expect(screen.queryByRole("link", { name: "Review plan" })).toBeNull();
    expect(screen.getByText(/^Nothing needs you right now\./)).toBeTruthy();
  });
});
