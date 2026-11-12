// @vitest-environment jsdom
// PRD #3878 M1: the /overview/next preview page.
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { OverviewNext } from "./OverviewNext";
import { CommandMenuContext } from "../components/CommandMenuContext";
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
      listUsers: vi.fn(),
    },
  };
});
vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));

const mockApi = vi.mocked(api);
const admin = { ...aUser, is_admin: true };

// Every endpoint a non-admin must never reach.
const adminEndpoints = () => [mockApi.adminListRuns, mockApi.getAdminUsage, mockApi.getAdminHealth, mockApi.listUsers];

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
  mockApi.listUsers.mockResolvedValue({ users: [] });
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
    expect(sentence()).toBe("Nothing needs you right now. 2 runs are executing across the factory for 2 people and 1 queued.");
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
    // The PR sits in its group: its ref links to the forge, and so does its action once expanded.
    expect(screen.getByRole("link", { name: "vtmocanu/uzi #4" }).getAttribute("href")).toBe("https://github.com/o/r/pull/4");
    fireEvent.click(screen.getByRole("button", { name: "Review PRs" }));
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

  const scopeGroup = () => within(screen.getByRole("group", { name: "Issue work scope" }));
  const chooseScope = async (name: "You" | "All users") => {
    await act(async () => {
      fireEvent.click(scopeGroup().getByRole("button", { name }));
    });
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
    await chooseScope("All users");
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
    await chooseScope("All users");
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
    await chooseScope("All users");
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
      Array.from(line().querySelectorAll('[data-stage][aria-pressed="true"]')).map((b) => b.getAttribute("data-stage"));
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
      await chooseScope("All users");
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

    it("moves the pressed state with the selection, and pressing the same station again clears it", async () => {
      adminMixed();
      await mount();
      await click("building");
      expect(pressed()).toEqual(["building"]);
      await click("review");
      expect(pressed()).toEqual(["review"]);
      expect(rows("Your runs")).toEqual([expect.stringContaining("Own plan")]);
      await click("review");
      expect(pressed()).toEqual([]);
      expect(rows("Your runs")).toHaveLength(2);
      expect(within(line()).queryByRole("button", { name: "Show all" })).toBeNull();
    });

    it("with You chosen, shows only the viewer's runs at the stage, and clears on a second press", async () => {
      adminMixed();
      await mount();
      await click("building");
      expect(rows("Your runs")).toEqual([expect.stringContaining("Own build")]);
      expect(screen.queryByRole("list", { name: "Other users' runs" })).toBeNull();
      expect(screen.queryByText("Their build")).toBeNull();
      await click("building");
      expect(pressed()).toEqual([]);
      expect(rows("Other users' runs")).toHaveLength(2);
    });

    it("keeps the pressed stage when the scope changes", async () => {
      adminMixed();
      await mount();
      await click("review");
      await chooseScope("All users");
      expect(pressed()).toEqual(["review"]);
      expect(rows("Other users' runs")).toEqual([expect.stringContaining("Their plan")]);
      await chooseScope("You");
      expect(pressed()).toEqual(["review"]);
      expect(screen.queryByRole("list", { name: "Other users' runs" })).toBeNull();
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

describe("Issue work scope (#4052)", () => {
  const SCOPE_KEY = "uzi.overviewIssueWorkScope";
  const line = () => screen.getByRole("region", { name: "Issue work" });
  const group = () => within(screen.getByRole("group", { name: "Issue work scope" }));
  const countOf = (stage: string) => Number(line().querySelector(`[data-stage="${stage}"]`)!.getAttribute("data-count"));
  const pick = async (name: "You" | "All users") => {
    await act(async () => {
      fireEvent.click(group().getByRole("button", { name }));
    });
  };
  const seed = () => {
    vi.mocked(useAuth).mockReturnValue(authValue(admin));
    mockApi.listRuns.mockResolvedValue({
      runs: [aRun({ id: "o-plan", status: "awaiting_approval" }), aRun({ id: "o-build", status: "running" })],
    });
    mockApi.adminListRuns.mockResolvedValue({
      runs: [
        aRun({ id: "x-plan", status: "awaiting_approval", owner_email: "mira@uzi.local" }),
        aRun({ id: "x-queued", status: "queued", owner_email: "mira@uzi.local" }),
      ],
    });
  };

  it("defaults an admin to You: own-only counts, own-only captions, and no all-users note", async () => {
    seed();
    await mount();
    expect(group().getByRole("button", { name: "You" }).getAttribute("aria-pressed")).toBe("true");
    expect(countOf("queued")).toBe(0);
    expect(countOf("review")).toBe(1);
    expect(line().querySelector('[data-stage="review"]')!.textContent).toContain("Waiting on you");
    expect(within(line()).getByText("2 runs on the line, 0 other")).toBeTruthy();
    expect(within(line()).queryByText(/all users/)).toBeNull();
    expect(localStorage.getItem(SCOPE_KEY)).toBeNull();
  });

  it("shows today's all-user counts after choosing All users, and remembers the choice", async () => {
    seed();
    await mount();
    await pick("All users");
    expect(countOf("queued")).toBe(1);
    expect(countOf("review")).toBe(2);
    expect(line().querySelector('[data-stage="review"]')!.textContent).toContain("1 on you, 1 on others");
    expect(within(line()).getByText("4 runs on the line, 0 other, all users")).toBeTruthy();
    expect(JSON.parse(localStorage.getItem(SCOPE_KEY)!)).toBe("all");
    cleanup();
    await mount();
    expect(group().getByRole("button", { name: "All users" }).getAttribute("aria-pressed")).toBe("true");
    expect(countOf("review")).toBe(2);
    await pick("You");
    expect(JSON.parse(localStorage.getItem(SCOPE_KEY)!)).toBe("you");
  });

  it("reads an unknown stored value, or an unreadable store, as You", async () => {
    seed();
    localStorage.setItem(SCOPE_KEY, JSON.stringify("factory"));
    await mount();
    expect(countOf("review")).toBe(1);
    cleanup();
    localStorage.setItem(SCOPE_KEY, JSON.stringify("all"));
    vi.spyOn(localStorage, "getItem").mockImplementation(() => {
      throw new Error("denied");
    });
    await mount();
    expect(countOf("review")).toBe(1);
    expect(group().getByRole("button", { name: "You" }).getAttribute("aria-pressed")).toBe("true");
    vi.restoreAllMocks();
  });

  it("disables All users and draws You when the admin list failed", async () => {
    seed();
    mockApi.adminListRuns.mockRejectedValue(new Error("down"));
    localStorage.setItem(SCOPE_KEY, JSON.stringify("all"));
    await mount();
    const all = group().getByRole("button", { name: "All users" }) as HTMLButtonElement;
    expect(all.disabled).toBe(true);
    expect(group().getByRole("button", { name: "You" }).getAttribute("aria-pressed")).toBe("true");
    expect(countOf("review")).toBe(1);
    expect(within(line()).queryByText(/all users/)).toBeNull();
    expect(localStorage.getItem(SCOPE_KEY)).toBe(JSON.stringify("all"));
  });

  it("shows You while a saved all waits for the admin list, then All users once it resolves, leaving storage alone", async () => {
    seed();
    let resolve!: (v: { runs: ReturnType<typeof aRun>[] }) => void;
    mockApi.adminListRuns.mockImplementation(() => new Promise((r) => (resolve = r as typeof resolve)));
    localStorage.setItem(SCOPE_KEY, JSON.stringify("all"));
    await mount();
    expect((group().getByRole("button", { name: "All users" }) as HTMLButtonElement).disabled).toBe(true);
    expect(countOf("review")).toBe(1);
    expect(localStorage.getItem(SCOPE_KEY)).toBe(JSON.stringify("all"));
    await act(async () => {
      resolve({ runs: [aRun({ id: "x-plan", status: "awaiting_approval", owner_email: "mira@uzi.local" })] });
    });
    await settle();
    expect(group().getByRole("button", { name: "All users" }).getAttribute("aria-pressed")).toBe("true");
    expect(countOf("review")).toBe(2);
    expect(localStorage.getItem(SCOPE_KEY)).toBe(JSON.stringify("all"));
  });

  it("gives a non-admin no scope switch on either card and requests nothing from the admin endpoints", async () => {
    localStorage.setItem(SCOPE_KEY, JSON.stringify("all"));
    await mount();
    expect(screen.queryByRole("group", { name: "Issue work scope" })).toBeNull();
    expect(screen.queryByRole("group", { name: "Usage scope" })).toBeNull();
    for (const call of adminEndpoints()) expect(call).not.toHaveBeenCalled();
    expect(within(line()).queryByText(/all users/)).toBeNull();
  });
});

describe("People chip (issue #4042)", () => {
  const SCOPE_KEY = "uzi.overviewUsageScope";
  const user = (name: string, over: { last_login?: string | null; is_active?: boolean } = {}) => ({
    ...aUser,
    id: `u-${name}`,
    email: `${name}@uzi.local`,
    display_name: name[0].toUpperCase() + name.slice(1),
    ...over,
  });
  const ago = (ms: number) => new Date(Date.now() - ms).toISOString();
  const H = 3_600_000;
  const chip = () => screen.getByRole("button", { name: /^People:/ });
  const dialog = () => within(screen.getByRole("dialog", { name: "People using uzi" }));
  const openChip = async () => {
    await act(async () => {
      fireEvent.click(chip());
    });
  };

  // Nine active accounts: four own listed active work, one signed in recently, four are idle.
  function seedRoster() {
    vi.mocked(useAuth).mockReturnValue(authValue(admin));
    mockApi.listRuns.mockResolvedValue({ runs: [aRun({ id: "own-1", status: "running" })] });
    mockApi.adminListRuns.mockResolvedValue({
      runs: [
        aRun({ id: "own-1", status: "running", owner_email: admin.email }),
        aRun({ id: "m-1", status: "running", owner_email: "mira@uzi.local" }),
        aRun({ id: "a-1", status: "queued", owner_email: "andrei@uzi.local" }),
        aRun({ id: "i-1", status: "paused", owner_email: "ioana@uzi.local" }),
        aRun({ id: "done", status: "completed", owner_email: "bogdan@uzi.local" }),
      ],
    });
    mockApi.listUsers.mockResolvedValue({
      users: [
        { ...admin, last_login: ago(H) },
        user("mira"),
        user("andrei"),
        user("ioana"),
        user("bogdan", { last_login: ago(2 * H) }),
        user("c1", { last_login: ago(10 * 24 * H) }),
        user("c2", { last_login: ago(10 * 24 * H) }),
        user("c3", { last_login: ago(10 * 24 * H) }),
        user("c4", { last_login: ago(10 * 24 * H) }),
        user("gone", { is_active: false }),
      ],
    });
  }

  it("counts four of nine people for an admin, with the right pills and one signed-in person", async () => {
    seedRoster();
    await mount();
    expect(chip().getAttribute("aria-label")).toBe("People: 4 active now of 9");
    expect(sentence()).toContain("2 runs are executing across the factory for 2 people");
    await openChip();
    const rows = dialog().getAllByRole("listitem").map((li) => li.textContent ?? "");
    expect(rows).toHaveLength(5);
    expect(rows[0]).toBe("AAdmin (you)1 running");
    expect(rows).toContain("AAndrei1 queued");
    expect(rows).toContain("MMira1 running");
    expect(rows).toContain("IIoana" + "waits on them");
    expect(rows[4]).toContain("Bogdan");
    expect(rows[4]).toContain("2h ago");
    expect(dialog().getByText("4 people, last seen 10 days ago")).toBeTruthy();
    expect(dialog().queryByText(/online/i)).toBeNull();
  });

  it("keeps the sentence's people count within its executing count for a viewer run only the admin list has", async () => {
    vi.mocked(useAuth).mockReturnValue(authValue(admin));
    mockApi.listRuns.mockResolvedValue({ runs: [] });
    mockApi.adminListRuns.mockResolvedValue({
      runs: [
        aRun({ id: "v-only", status: "running", owner_email: admin.email }),
        aRun({ id: "m-1", status: "running", owner_email: "mira@uzi.local" }),
      ],
    });
    await mount();
    const m = sentence().match(/(\d+) runs? (?:is|are) executing across the factory for (\d+) (?:person|people)/);
    expect(m).not.toBeNull();
    expect(Number(m![2])).toBeLessThanOrEqual(Number(m![1]));
    await openChip();
    expect(dialog().getAllByRole("listitem").some((li) => li.textContent?.includes("(you)"))).toBe(true);
  });

  it("says active now is unavailable when the first admin run list fails", async () => {
    vi.mocked(useAuth).mockReturnValue(authValue(admin));
    mockApi.adminListRuns.mockRejectedValue(new Error("down"));
    await mount();
    expect(chip().getAttribute("aria-label")).toBe("People: Active now unavailable");
  });

  it("shows no total, and no signed-in or idle group, when the user list fails", async () => {
    seedRoster();
    mockApi.listUsers.mockRejectedValue(new Error("403"));
    await mount();
    expect(chip().getAttribute("aria-label")).toBe("People: 4 active now");
    await openChip();
    expect(dialog().queryByText(/Signed in within 24 hours/)).toBeNull();
    expect(dialog().queryByText("Idle")).toBeNull();
    expect(dialog().getAllByRole("listitem")).toHaveLength(4);
    expect(screen.getByRole("region", { name: "Your usage" })).toBeTruthy();
    expect(screen.getAllByText("Live run row").length).toBeGreaterThan(0);
  });

  it("disables the usage action when the viewer's own usage could not be read", async () => {
    seedRoster();
    mockApi.getUsage.mockRejectedValue(new Error("down"));
    await mount();
    await openChip();
    const btn = dialog().getByRole("button", { name: "Usage by user unavailable" }) as HTMLButtonElement;
    expect(btn.disabled).toBe(true);
  });

  it("switches the Usage card to All users and focuses By user when Usage by user is activated", async () => {
    seedRoster();
    await mount();
    expect(localStorage.getItem(SCOPE_KEY)).toBeNull();
    expect(screen.queryByRole("region", { name: "By user" })).toBeNull();
    await openChip();
    await act(async () => {
      fireEvent.click(dialog().getByRole("button", { name: "Usage by user →" }));
    });
    expect(chip().getAttribute("aria-expanded")).toBe("false");
    const byUser = screen.getByRole("region", { name: "By user" });
    expect(within(screen.getByRole("group", { name: "Usage scope" })).getByRole("button", { name: "All users" }).getAttribute("aria-pressed")).toBe("true");
    expect(document.activeElement).toBe(byUser);
    expect(byUser.id).toBe("overview-usage-by-user");
    expect(JSON.parse(localStorage.getItem(SCOPE_KEY)!)).toBe("all");
  });

  it("gives a non-admin no chip, no 'for' in the sentence and no admin or user request", async () => {
    await mount();
    await settle(30_000);
    expect(screen.queryByRole("button", { name: /^People:/ })).toBeNull();
    expect(sentence()).not.toContain(" for ");
    for (const call of adminEndpoints()) expect(call).not.toHaveBeenCalled();
  });
});

// PRD #3875 M1: the Search button in the header opens the shell's command menu.
describe("OverviewNext Search button", () => {
  it("calls the shell's open() on click, with the shortcut shown on the button", async () => {
    const open = vi.fn();
    render(
      <MemoryRouter>
        <CommandMenuContext.Provider value={{ open }}>
          <OverviewNext />
        </CommandMenuContext.Provider>
      </MemoryRouter>,
    );
    await settle();
    const button = screen.getByRole("button", { name: /^Search/ });
    expect(button.textContent).toMatch(/Ctrl K|\u2318K/);
    fireEvent.click(button);
    expect(open).toHaveBeenCalledTimes(1);
  });

  it("names the shortcut in aria-keyshortcuts: Control+K by default, Meta+K on an Apple platform", async () => {
    const mountWithMenu = () =>
      render(
        <MemoryRouter>
          <CommandMenuContext.Provider value={{ open: vi.fn() }}>
            <OverviewNext />
          </CommandMenuContext.Provider>
        </MemoryRouter>,
      );
    const view = mountWithMenu();
    await settle();
    expect(screen.getByRole("button", { name: /^Search/ }).getAttribute("aria-keyshortcuts")).toBe("Control+K");
    view.unmount();
    Object.defineProperty(navigator, "platform", { configurable: true, value: "MacIntel" });
    try {
      mountWithMenu();
      await settle();
      expect(screen.getByRole("button", { name: /^Search/ }).getAttribute("aria-keyshortcuts")).toBe("Meta+K");
    } finally {
      delete (navigator as unknown as Record<string, unknown>).platform;
    }
  });

  it("renders no Search button without a command menu (page mounted outside the shell)", async () => {
    await mount();
    expect(screen.queryByRole("button", { name: /^Search/ })).toBeNull();
  });
});
