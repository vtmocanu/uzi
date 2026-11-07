// @vitest-environment jsdom
// PRD #3878 M1: the /overview/next preview page.
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
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
    expect(screen.getByRole("region", { name: "Your usage" })).toBeTruthy();
    expect(screen.queryByRole("group", { name: "Usage scope" })).toBeNull();
    expect(screen.queryByRole("region", { name: "By user" })).toBeNull();
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
    expect(screen.getByRole("group", { name: "Usage scope" })).toBeTruthy();
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
    expect(within(card).queryByRole("link", { name: /^Review held work/ })).toBeNull();
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
    expect(within(needs).getByText("Held work needs 2 decisions")).toBeTruthy();
    expect(within(needs).getByText("2 decisions")).toBeTruthy();
    expect(within(needs).getByRole("link", { name: "Review held work" }).getAttribute("href")).toBe(
      "/workers?tab=workers#recovery-holds",
    );
    // The card header carries its own link, beside the row's.
    expect(within(heldCard()).getByRole("link", { name: "Review held work →" }).getAttribute("href")).toBe(
      "/workers?tab=workers#recovery-holds",
    );
    expect(screen.getAllByRole("link", { name: /^Review held work/ })).toHaveLength(2);
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
    expect(screen.queryByRole("link", { name: /^Review held work/ })).toBeNull();
    expect(screen.queryByText("Needs your decision")).toBeNull();
    expect(sentence()).toBe("Nothing needs you right now. 1 run is executing.");
  });

  it("says held work is unavailable when the holds request fails, and keeps the rest of the page", async () => {
    mockApi.getRecoveryHolds.mockRejectedValue(new Error("down"));
    await mount();
    expect(within(heldCard()).getByText("Held work is unavailable right now.")).toBeTruthy();
    expect(screen.getByText("Live run row")).toBeTruthy();
  });

  it("says it is loading, not unavailable, until the first holds response", async () => {
    mockApi.getRecoveryHolds.mockImplementation(() => new Promise(() => {}));
    await mount();
    expect(within(heldCard()).getByText("Loading held work…")).toBeTruthy();
    expect(screen.queryByText(/Held work is unavailable/)).toBeNull();
  });

  it("still shows the danger headline and the held row when a first-load endpoint rejects", async () => {
    mockApi.listRepos.mockRejectedValue(new Error("repos down"));
    mockApi.getRecoveryHolds.mockResolvedValue({
      aggregate: heldAggregate({ open_holds: 3, admission_counted_holds: 3, decision_needed: 1, blocked_runs: 2 }),
      holds: [decisionHold("a83c51f0-aaaa", "2026-07-05T10:00:00Z")],
    });
    await mount();
    const card = screen.getByRole("alert", { name: "Held work" });
    expect(within(card).getByRole("heading", { level: 3 }).textContent).toBe("Held work is blocking new runs");
    expect(within(card).getByRole("link", { name: "Review held work →" })).toBeTruthy();
    expect(within(screen.getByRole("region", { name: "Needs your decision" })).getByText(/Held work from run/)).toBeTruthy();
  });

  it("shows the Held work card with its figures while the runs request never resolves", async () => {
    mockApi.listRuns.mockImplementation(() => new Promise(() => {}));
    mockApi.getRecoveryHolds.mockResolvedValue({
      aggregate: heldAggregate({ open_holds: 2, admission_counted_holds: 2 }),
      holds: [],
    });
    await mount();
    expect([...heldCard().querySelectorAll("strong")].map((e) => e.textContent)).toEqual(["2 / 8", "2", "0"]);
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

describe("Issue work line", () => {
  const line = () => screen.getByRole("region", { name: "Issue work" });
  const stationOf = (stage: string) => line().querySelector(`[data-stage="${stage}"]`) as HTMLElement;
  const countOf = (stage: string) => Number(stationOf(stage).getAttribute("data-count"));
  const activeStages = ["queued", "planning", "review", "building", "asking", "parked", "other"];
  const widgetRows = () => {
    const rows = (name: string) => screen.queryAllByRole("list", { name }).flatMap((l) => within(l).getAllByRole("listitem"));
    return rows("Your runs").length + rows("Other users' runs").length;
  };

  it("sits right after the decision card, only once the data has loaded", async () => {
    mockApi.listRuns.mockResolvedValue({ runs: [aRun({ id: "plan-1", status: "awaiting_approval" })] });
    await mount();
    const needs = screen.getByRole("region", { name: "Needs your decision" });
    expect(needs.nextElementSibling).toBe(line());
    cleanup();
    mockApi.listRuns.mockImplementation(() => new Promise(() => {}));
    await mount();
    expect(screen.queryByRole("region", { name: "Issue work" })).toBeNull();
  });

  it("counts another user's queued run for an admin, naming the owner in the badge tooltip", async () => {
    vi.mocked(useAuth).mockReturnValue(authValue(admin));
    mockApi.listRuns.mockResolvedValue({ runs: [aRun({ id: "own-1", status: "running" })] });
    mockApi.adminListRuns.mockResolvedValue({
      runs: [aRun({ id: "x-1", status: "queued", issue_iid: 55, owner_email: "mira@uzi.local" })],
    });
    await mount();
    expect(countOf("queued")).toBe(1);
    expect(within(stationOf("queued")).getByText("#55").getAttribute("title")).toBe("mira@uzi.local");
    expect(within(line()).getByText("2 runs on the line, 0 other, all users")).toBeTruthy();
  });

  it("never counts another user's run on a non-admin's line, and never asks for the admin list", async () => {
    mockApi.adminListRuns.mockResolvedValue({
      runs: [aRun({ id: "x-1", status: "queued", owner_email: "mira@uzi.local" })],
    });
    await mount();
    expect(countOf("queued")).toBe(0);
    expect(countOf("building")).toBe(1);
    for (const call of adminEndpoints()) expect(call).not.toHaveBeenCalled();
    expect(within(line()).queryByText(/all users/)).toBeNull();
  });

  it("counts only the viewer's runs when the admin list is unreadable", async () => {
    vi.mocked(useAuth).mockReturnValue(authValue(admin));
    mockApi.adminListRuns.mockRejectedValue(new Error("down"));
    await mount();
    expect(activeStages.reduce((n, s) => n + countOf(s), 0)).toBe(1);
    expect(within(line()).queryByText(/all users/)).toBeNull();
  });

  const mixed = () => [
    aRun({ id: "o1", status: "queued" }),
    aRun({ id: "o2", status: "awaiting_approval", is_revising: true }),
    aRun({ id: "o3", status: "awaiting_approval" }),
    aRun({ id: "o4", status: "awaiting_input" }),
    aRun({ id: "o5", status: "paused" }),
    aRun({ id: "o6", status: "running", health: "waiting_account_cooldown" }),
    aRun({ id: "o7", kind: "ci_fix", status: "running" }),
    aRun({ id: "o8", status: "running" }),
    aRun({ id: "o9", status: "completed", mr_iid: 3, mr_state: "opened", finished_at: "2026-07-05T12:00:00Z" }),
    aRun({ id: "o10", status: "failed" }),
  ];

  it("totals the stations and bays to the Runs widget's rows for a non-admin, PRs apart", async () => {
    mockApi.listRuns.mockResolvedValue({ runs: mixed() });
    await mount();
    expect(widgetRows()).toBe(8);
    expect(activeStages.reduce((n, s) => n + countOf(s), 0)).toBe(widgetRows());
    expect(countOf("pr")).toBe(1);
  });

  it("totals the stations and bays to Yours plus Other users for an admin", async () => {
    vi.mocked(useAuth).mockReturnValue(authValue(admin));
    mockApi.listRuns.mockResolvedValue({ runs: mixed() });
    mockApi.adminListRuns.mockResolvedValue({
      runs: [
        ...mixed().map((r) => ({ ...r, owner_email: admin.email })),
        aRun({ id: "x1", status: "queued", owner_email: "mira@uzi.local" }),
        aRun({ id: "x2", status: "awaiting_input", owner_email: "mira@uzi.local" }),
        aRun({ id: "x3", status: "limit_wait", owner_email: "andrei@uzi.local" }),
        aRun({ id: "x4", status: "completed", owner_email: "andrei@uzi.local" }),
        aRun({ id: "x5", kind: "job", status: "running", owner_email: "andrei@uzi.local" }),
      ],
    });
    await mount();
    expect(widgetRows()).toBe(8 + 4);
    expect(activeStages.reduce((n, s) => n + countOf(s), 0)).toBe(widgetRows());
    expect(countOf("pr")).toBe(1);
  });

  it("does not count an admin's own run that only the admin list carries as another user's", async () => {
    vi.mocked(useAuth).mockReturnValue(authValue(admin));
    mockApi.listRuns.mockResolvedValue({ runs: [aRun({ id: "own-1", status: "running" })] });
    mockApi.adminListRuns.mockResolvedValue({
      runs: [
        aRun({ id: "own-1", status: "running", owner_email: admin.email }),
        aRun({ id: "own-lag", status: "queued", owner_email: admin.email }),
        aRun({ id: "x-1", status: "queued", owner_email: "mira@uzi.local" }),
      ],
    });
    await mount();
    expect(countOf("queued")).toBe(1);
    expect(countOf("building")).toBe(1);
    expect(activeStages.reduce((n, s) => n + countOf(s), 0)).toBe(widgetRows());
    expect(widgetRows()).toBe(2);
  });

  describe("filtering the Runs widget by a stage", () => {
    const click = (stage: string) =>
      act(() => {
        fireEvent.click(stationOf(stage));
      });
    const rows = (name: string) => {
      const list = screen.queryByRole("list", { name });
      return list ? within(list).getAllByRole("listitem").map((li) => li.textContent ?? "") : [];
    };
    const pressed = () =>
      Array.from(line().querySelectorAll('[aria-pressed="true"]')).map((b) => b.getAttribute("data-stage"));
    const adminMixed = () => {
      vi.mocked(useAuth).mockReturnValue(authValue(admin));
      mockApi.listRuns.mockResolvedValue({
        runs: [
          aRun({ id: "o-plan", status: "awaiting_approval", issue_title: "Own plan" }),
          aRun({ id: "o-build", status: "running", issue_title: "Own build" }),
        ],
      });
      mockApi.adminListRuns.mockResolvedValue({
        runs: [
          aRun({ id: "x-plan", status: "awaiting_approval", issue_title: "Their plan", owner_email: "mira@uzi.local" }),
          aRun({ id: "x-build", status: "running", issue_title: "Their build", owner_email: "mira@uzi.local" }),
        ],
      });
    };

    it("shows only plan-review runs in both groups, and restores all on a second click", async () => {
      adminMixed();
      await mount();
      expect(rows("Your runs")).toHaveLength(2);
      expect(rows("Other users' runs")).toHaveLength(2);
      await click("review");
      expect(rows("Your runs")).toEqual([expect.stringContaining("Own plan")]);
      expect(rows("Other users' runs")).toEqual([expect.stringContaining("Their plan")]);
      expect(pressed()).toEqual(["review"]);
      await click("review");
      expect(pressed()).toEqual([]);
      expect(rows("Your runs")).toHaveLength(2);
      expect(rows("Other users' runs")).toHaveLength(2);
    });

    it("moves the pressed state with the selection, and Show all clears it", async () => {
      adminMixed();
      await mount();
      await click("building");
      expect(pressed()).toEqual(["building"]);
      await click("review");
      expect(pressed()).toEqual(["review"]);
      expect(rows("Your runs")).toEqual([expect.stringContaining("Own plan")]);
      await act(async () => {
        fireEvent.click(within(line()).getByRole("button", { name: "Show all" }));
      });
      expect(pressed()).toEqual([]);
      expect(rows("Your runs")).toHaveLength(2);
    });

    it("lists a pull request once when two runs share it, and counts the same", async () => {
      const pr = { status: "completed" as const, repo_id: "repo-1", mr_iid: 5, mr_state: "opened" as const };
      mockApi.listRuns.mockResolvedValue({
        runs: [
          aRun({ ...pr, id: "pr-old", issue_title: "PR older", finished_at: "2026-07-05T10:00:00Z" }),
          aRun({ ...pr, id: "pr-new", issue_title: "PR newer", finished_at: "2026-07-05T12:00:00Z" }),
        ],
      });
      await mount();
      expect(countOf("pr")).toBe(1);
      await click("pr");
      expect(rows("Your runs")).toEqual([expect.stringContaining("PR newer")]);
      expect(screen.getByRole("link", { name: /All runs/ }).previousElementSibling?.textContent).toBe("Open pull requests");
    });

    it("hides an empty group, says so when both are empty, and keeps the unavailable notice", async () => {
      adminMixed();
      await mount();
      await click("queued");
      expect(screen.getByText("No runs at this stage.")).toBeTruthy();
      expect(screen.queryByText("Yours")).toBeNull();
      expect(screen.queryByText("Other users (admin)")).toBeNull();
      await click("review");
      expect(screen.queryByText("No runs at this stage.")).toBeNull();
      cleanup();
      mockApi.adminListRuns.mockRejectedValue(new Error("down"));
      await mount();
      await click("review");
      expect(rows("Your runs")).toHaveLength(1);
      expect(screen.getByRole("status").textContent).toBe("Other users' runs are unavailable");
      await click("review");
      await click("queued");
      expect(screen.getByRole("status").textContent).toBe("Other users' runs are unavailable");
      expect(screen.getByText("No runs at this stage.")).toBeTruthy();
    });
  });
});
