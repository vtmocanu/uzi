// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { act, cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { Dashboard } from "./Dashboard";
import { api, type ForgeConnection, type Repo, type RunListItem, type SecretMeta, type Worker } from "../lib/api";
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
      getRecoveryHolds: vi.fn(),
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
  ci_autofix_enabled: false,
  attribution_enabled: true,
  ephemeral_workers_enabled: false,
  wait_on_limit: false,
  notify_early_limit_reset: false,
  judge_anthropic_secret_id: null,
  judge_anthropic_secret_label: null,
  judge_anthropic_bind_mode: "default" as const,
  created_at: "2026-01-01T00:00:00Z",
  last_login: null,
};

function aRun(over: Partial<RunListItem> = {}): RunListItem {
  return {
    id: "run-1",
    repo_id: "repo-1",
    forge_type: "gitlab",
    mr_web_url: null,
    issue_web_url: null,
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
    model: null,
    override_subagent_model: false,
    mr_iid: null,
    mr_state: null,
    failure_reason: null,
    stop_kind: null,
    stop_reason: null,
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
    anthropic_select_reason: null,
    anthropic_headroom_pct: null,
    wait_on_limit: false,
    limit_resets_at: null,
    retry_not_before: null,
    limit_wait_count: 0,
    rate_limit_type: null,
    recovery_wait_cause: null,
    recovery_retry_not_before: null,
    forge_park_count: 0,
    forge_park_max: 0,
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
    stats_disk_nix_bytes: null,
    stats_disk_nix_total_bytes: null,
    stats_disk_data_bytes: null,
    stats_disk_data_total_bytes: null,
    anthropic_secret_id: null,
    anthropic_secret_label: null,
    anthropic_bind_mode: "default",
    draining_since: null,
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
    uziLabel: "uzi",
    autopilotLabel: "autopilot",
    appearance: {
      mode: "dark",
      light_theme: "hall",
      dark_theme: "ember",
      typeface: "system",
      overrides: { mode: null, light_theme: null, dark_theme: null, typeface: null },
      defaults: { mode: "dark", light_theme: "hall", dark_theme: "ember", typeface: "system" },
    },
    vaultUnlocked: true,
    vaultExists: true,
    hasPassword: true,
    judgeEnforcedByAdmin: false,
    effectiveJudgeModel: "opus",
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
  // Default: no custody holds, so the board alert self-hides and existing assertions are
  // unaffected. Tests that exercise the alert override this.
  mockApi.getRecoveryHolds.mockResolvedValue({
    aggregate: { open_holds: 0, custody_hold_limit: 8, decision_needed: 0, blocked_runs: 0 },
    holds: [],
  });
});

const zeros = () => ({ input_tokens: 0, cache_read_tokens: 0, cache_creation_tokens: 0, output_tokens: 0, cost_usd: 0 });
// PRD #1293: zero outcomes keep the failed-runs block hidden (finished===0), so these
// usage fixtures leave the existing "nothing yet" / card assertions unchanged.
const zeroOutcomes = () => ({ finished: 0, completed: 0, cancelled: 0, plan_rejected: 0, failed: 0, fail_origins: {} });
const zeroOutcomeWindows = () => ({ lifetime: zeroOutcomes(), last_7_days: zeroOutcomes() });
function emptySelf() {
  return { lifetime: zeros(), last_7_days: zeros(), run_count: 0, outcomes: zeroOutcomeWindows() };
}

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.clearAllMocks();
});

// Issue #124 / item 7: the recent-runs list renders the same forge-supplied issue title.
describe("Dashboard — the recent-run title carries no format characters (#124)", () => {
  it("strips bidi/zero-width characters out of the forge-supplied title", async () => {
    mockApi.listRuns.mockResolvedValue({ runs: [aRun({ issue_title: "Fix the \u202Eparser\u200B bug" })] });
    const { container } = renderDashboard();
    // Same explicit first-load flush the neighbouring cases use, then anchor on a fixed
    // stat-tile label rather than on the title: a lookup for the CLEANED title cannot match
    // while the format character is present, so a mutation would red at the lookup instead
    // of at the assertion below.
    await act(async () => {
      await new Promise((r) => setTimeout(r, 0));
    });
    expect(screen.getByText("Active runs")).toBeTruthy();
    expect(container.textContent ?? "").not.toMatch(/[\p{Cf}]/u);
    expect(screen.getByText("Fix the parser bug")).toBeTruthy();
  });
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

describe("Dashboard onboarding checklist (PRD #60)", () => {
  it("keeps no decoration-* class on a done step, so the strike falls back to currentColor", async () => {
    // One online worker (step 4 done) and no token/forge/repos (steps 1-3 open), so
    // the checklist card renders at all (it hides once every step is done). The worker
    // restates the beforeEach default on purpose — it is the step this test asserts on.
    mockApi.listWorkers.mockResolvedValue({ workers: [aWorker({ status: "online" })] });
    renderDashboard();
    await act(async () => {
      await new Promise((r) => setTimeout(r, 0));
    });

    const done = screen.getByText("Bring a worker online");
    const classes = done.className.split(/\s+/).filter(Boolean);
    expect(classes).toContain("line-through");
    // The regression guard, deliberately wider than the defect: no decoration-* utility
    // at all. A decoration COLOR is what caused PRD #60 (a border token at ~1.9:1 against
    // the card, so the line was all but invisible); with none set, the line inherits the
    // muted text color. jsdom cannot observe the computed color, so the class list is the
    // proxy — a thickness or style utility would trip this too, which is the intent.
    expect(classes.filter((c) => c.startsWith("decoration-"))).toEqual([]);

    // Sanity: an open step is not struck at all.
    expect(screen.getByText("Add your Anthropic token").className).not.toContain("line-through");
  });
});

// Issue #1331: step 4 "Bring a worker online" must reflect a DURABLE fact — that a
// worker has ever registered (online right now, OR a non-null last_heartbeat_at) —
// not the momentary `status === "online"`. During a fleet roll every worker can sit
// offline for minutes while its container upgrades, yet the factory is plainly set
// up; the old predicate (workersOnline > 0) re-opened the step and dropped the
// counter to 0/4 on that exact screen. These pin the intended `workerJoined`
// behavior: cases 1 and 3 FAIL on the unfixed tree, case 2 is the boundary control.
describe("Dashboard onboarding step 4 is durable (PRD #1331)", () => {
  const flush = async () => {
    await act(async () => {
      await new Promise((r) => setTimeout(r, 0));
    });
  };

  it("counts a registered-but-offline fleet (mid-roll) as joined", async () => {
    // The bug's exact screen: both workers offline mid-upgrade, but each carries a
    // last_heartbeat_at, so the fleet has demonstrably registered before.
    mockApi.listWorkers.mockResolvedValue({
      workers: [
        aWorker({ id: "w1", status: "offline", upgrade_status: "upgrading", last_heartbeat_at: "2026-09-13T10:00:00Z" }),
        aWorker({ id: "w2", status: "offline", upgrade_status: "upgrading", last_heartbeat_at: "2026-09-13T10:00:05Z" }),
      ],
    });
    const { container } = renderDashboard();
    await flush();

    // Steps 1-3 are open (beforeEach defaults) so the card renders; step 4 must read
    // as done — struck through, and folded into the "N/4 done" counter.
    const step4 = screen.getByText("Bring a worker online");
    expect(step4.className.split(/\s+/).filter(Boolean)).toContain("line-through");
    expect(container.textContent).toContain("1/4 done");
  });

  it("leaves step 4 open for a worker that never registered", async () => {
    // The boundary control: a row exists but has never sent a heartbeat and is not
    // online, so it has not joined. The fix must not over-reach to "any worker row
    // exists" — this case passes on the unfixed tree too.
    mockApi.listWorkers.mockResolvedValue({
      workers: [aWorker({ id: "w1", status: "offline", last_heartbeat_at: null })],
    });
    const { container } = renderDashboard();
    await flush();

    expect(screen.getByText("Bring a worker online").className).not.toContain("line-through");
    expect(container.textContent).toContain("0/4 done");
  });

  it("hides the checklist card once steps 1-3 are done and the fleet has registered, while the Workers tile still shows 0/N", async () => {
    // Steps 1-3 done, plus a registered-but-offline fleet → all four steps done → the
    // "Get the factory running" card hides. A positive control against a vacuous
    // negative: the Workers-online tile lives outside the !ready gate, so it survives
    // the card hiding and still reads 0/2 for the two offline workers.
    const secret: SecretMeta = {
      id: "s1",
      kind: "anthropic_token",
      label: "tok",
      is_default: true,
      auto_eligible: false,
      created_at: "2026-09-13T00:00:00Z",
      updated_at: "2026-09-13T00:00:00Z",
    };
    const connection: ForgeConnection = {
      id: "c1",
      forge_type: "github",
      base_url: "https://github.com",
      bot_username: "uzi-bot",
      bot_forge_user_id: 1,
      human_username: null,
      created_at: "2026-09-13T00:00:00Z",
      last_verified_at: null,
      privilege_status: null,
      privilege_checked_at: null,
      privilege_report: null,
    };
    const repo: Repo = {
      id: "repo-1",
      connection_id: "c1",
      forge_project_id: 1,
      path_with_namespace: "vtmocanu/uzi",
      web_url: "https://github.com/vtmocanu/uzi",
      default_branch: "main",
      enabled: true,
      repo_skills_enabled: false,
      repo_claudemd_enabled: false,
      repo_devbox_opt_in: false,
      repo_fold_improve_uzi_backlog: false,
      pipeline: null,
      guardrail_blocked: false,
      docker_allowlisted: false,
      docker_blocked: false,
    };
    mockApi.listSecrets.mockResolvedValue({ secrets: [secret] });
    mockApi.listConnections.mockResolvedValue({ connections: [connection] });
    mockApi.listRepos.mockResolvedValue({ repos: [repo] });
    mockApi.listWorkers.mockResolvedValue({
      workers: [
        aWorker({ id: "w1", status: "offline", upgrade_status: "upgrading", last_heartbeat_at: "2026-09-13T10:00:00Z" }),
        aWorker({ id: "w2", status: "offline", upgrade_status: "upgrading", last_heartbeat_at: "2026-09-13T10:00:05Z" }),
      ],
    });
    const { container } = renderDashboard();
    await flush();

    expect(screen.queryByText("Get the factory running")).toBeNull();
    expect(container.textContent).toContain("0/2");
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
    outcomes: zeroOutcomeWindows(),
  };
  const adminUsage = {
    factory: { lifetime: bundle(5_400_000, 53_900_000, 2_400_000, 88.15), last_7_days: zeros(), run_count: 79, outcomes: zeroOutcomeWindows() },
    users: [
      { user_id: "a", email: "vlad@example.com", usage: bundle(1_610_000, 16_100_000, 710_000, 26.4), run_count: 23, outcomes: zeroOutcomes() },
      { user_id: "b", email: "maria@example.com", usage: bundle(2_490_000, 21_400_000, 1_020_000, 37.83), run_count: 31, outcomes: zeroOutcomes() },
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

// PRD #35 (web-ux F2). `!isTerminalRun` was doing duty as "actively working", which was
// harmless while every non-terminal status resolved in minutes. `limit_wait` is the
// first that lasts HOURS by design, so the hint became a claim that is simply false.
describe("Dashboard — parked runs are not 'agents at work' (PRD #35)", () => {
  const flush = async () => {
    await act(async () => {
      await new Promise((r) => setTimeout(r, 0));
    });
  };

  it("keeps the old copy exactly when nothing is parked", () => {
    // The regression guard. A factory with no parked run must read as it always did —
    // this change is meant to be invisible until it has something to say.
    return (async () => {
      mockApi.listRuns.mockResolvedValue({ runs: [aRun({ status: "running" }), aRun({ id: "r2", status: "queued" })] });
      renderDashboard();
      await flush();
      expect(screen.getByText("agents at work")).toBeTruthy();
    })();
  });

  it("🔴 splits the hint when a run is parked, rather than calling it work", async () => {
    mockApi.listRuns.mockResolvedValue({
      runs: [
        aRun({ status: "running" }),
        aRun({ id: "r2", status: "queued" }),
        aRun({ id: "r3", status: "limit_wait" }),
      ],
    });
    renderDashboard();
    await flush();
    expect(screen.queryByText("agents at work")).toBeNull();
    expect(screen.getByText("2 at work · 1 waiting to resume")).toBeTruthy();
  });

  // Issue #754: pool_wait is the same kind of self-resuming hold as limit_wait, so it
  // must NOT be counted as "at work" either. A pool hold is blocked on a pooled token,
  // not on a usage limit, which is why the generalized copy reads "waiting to resume"
  // rather than the limit-specific wording.
  it("🔴 counts a pool_wait run as waiting-to-resume, not as work", async () => {
    mockApi.listRuns.mockResolvedValue({
      runs: [
        aRun({ status: "running" }),
        aRun({ id: "r2", status: "queued" }),
        aRun({ id: "r3", status: "pool_wait" }),
      ],
    });
    renderDashboard();
    await flush();
    expect(screen.queryByText("agents at work")).toBeNull();
    expect(screen.getByText("2 at work · 1 waiting to resume")).toBeTruthy();
    // The pool hold is a resource wait, never a usage-limit wait — the old copy would
    // have been a lie about WHY it is parked.
    expect(screen.queryByText(/waiting on a usage limit/)).toBeNull();
  });

  // Issue #1197: recovery_wait is the same kind of self-resuming hold again — a
  // transient-recovery park that auto-resumes on a capped backoff — so it must NOT be
  // counted as "at work" either. Same generalized "waiting to resume" copy.
  it("🔴 counts a recovery_wait run as waiting-to-resume, not as work", async () => {
    mockApi.listRuns.mockResolvedValue({
      runs: [
        aRun({ status: "running" }),
        aRun({ id: "r2", status: "queued" }),
        aRun({ id: "r3", status: "recovery_wait" }),
      ],
    });
    renderDashboard();
    await flush();
    expect(screen.queryByText("agents at work")).toBeNull();
    expect(screen.getByText("2 at work · 1 waiting to resume")).toBeTruthy();
  });

  it("🔴 counts all three hold kinds together in the waiting bucket", async () => {
    // The three self-resuming holds share one honest bucket; the working count drops by all.
    mockApi.listRuns.mockResolvedValue({
      runs: [
        aRun({ status: "running" }),
        aRun({ id: "r2", status: "limit_wait" }),
        aRun({ id: "r3", status: "pool_wait" }),
        aRun({ id: "r4", status: "recovery_wait" }),
      ],
    });
    renderDashboard();
    await flush();
    expect(screen.getByText("1 at work · 3 waiting to resume")).toBeTruthy();
  });

  it("🔴 still COUNTS the parked run — splitting the hint must not hide it", async () => {
    // The opposite failure, and the reason this is a hint change rather than a filter:
    // excluding parked runs from the tile would make them vanish from the one surface
    // that says how much is in flight.
    mockApi.listRuns.mockResolvedValue({
      runs: [aRun({ status: "running" }), aRun({ id: "r3", status: "limit_wait" })],
    });
    const { container } = renderDashboard();
    await flush();
    expect(screen.getByText("Active runs")).toBeTruthy();
    expect(container.textContent).toContain("1 at work · 1 waiting to resume");
    // The tile's value is 2, not 1.
    const tile = screen.getByText("Active runs").closest("a") ?? screen.getByText("Active runs").parentElement;
    expect(tile?.textContent).toMatch(/2/);
  });

  it("reads 'nothing in flight' when every run is terminal", async () => {
    mockApi.listRuns.mockResolvedValue({ runs: [aRun({ status: "completed" })] });
    renderDashboard();
    await flush();
    expect(screen.getByText("nothing in flight")).toBeTruthy();
  });
});

describe("Dashboard milestone badge (PRD #122)", () => {
  const flush = () => act(async () => { await new Promise((r) => setTimeout(r, 0)); });

  it("shows a compact M{done}/{total} badge on a milestone-structured run", async () => {
    mockApi.listRuns.mockResolvedValue({
      runs: [
        aRun({
          issue_title: "Milestone run",
          milestones: [
            { id: "a", title: "A" },
            { id: "b", title: "B" },
            { id: "c", title: "C" },
            { id: "d", title: "D" },
            { id: "e", title: "E" },
          ],
          milestones_completed: ["a", "b"],
        }),
      ],
    });
    renderDashboard();
    await flush();
    expect(screen.getByText("Milestone run")).toBeTruthy();
    expect(screen.getByText("M2/5")).toBeTruthy();
  });

  it("adds no milestone badge for a run with no milestones", async () => {
    mockApi.listRuns.mockResolvedValue({ runs: [aRun({ issue_title: "Plain run", milestones: null })] });
    renderDashboard();
    await flush();
    expect(screen.getByText("Plain run")).toBeTruthy();
    expect(screen.queryByText(/^M\d+\/\d+$/)).toBeNull();
  });
});

// Non-terminal holds may retain current_activity from their last tool-use frame. None
// should present that stale activity as live work (PRD #1190 and issue #1197).
describe("Dashboard live 'now' strip excludes parked runs", () => {
  const flush = () => act(async () => { await new Promise((r) => setTimeout(r, 0)); });
  const activity = {
    agent: "coder",
    agent_label: "Wiring the collector",
    tool: "Write",
    detail: "api/collector.go",
    at: "2026-07-05T12:00:00Z",
    seq: 5,
  };

  it("shows the live now strip for a RUNNING run (positive control)", async () => {
    mockApi.listRuns.mockResolvedValue({
      runs: [aRun({ issue_title: "Running now", status: "running", current_activity: activity })],
    });
    renderDashboard();
    await flush();
    expect(screen.getByText("Running now")).toBeTruthy();
    expect(screen.getByText("Wiring the collector")).toBeTruthy();
    expect(screen.getByText("Running now").closest("li")?.querySelector(".bg-ok.animate-pulse")).toBeTruthy();
  });

  it.each(["paused", "limit_wait", "pool_wait", "recovery_wait"] as const)(
    "hides stale live activity on %s even when current_activity is populated",
    async (status) => {
      const title = `Held ${status}`;
      mockApi.listRuns.mockResolvedValue({
        runs: [aRun({ issue_title: title, status, current_activity: activity })],
      });
      renderDashboard();
      await flush();
      expect(screen.getByText(title)).toBeTruthy();
      expect(screen.queryByText("Wiring the collector")).toBeNull();
      expect(screen.getByText(title).closest("li")?.querySelector(".bg-ok.animate-pulse")).toBeNull();
    },
  );
});

describe("Dashboard Worker-load card surfaces the cordon badge (PRD #496)", () => {
  const flush = async () => {
    await act(async () => {
      await new Promise((r) => setTimeout(r, 0));
    });
  };
  // The Worker-load card only renders workers passing hasStats (stats_source and
  // stats_mem_bytes both non-null — see WorkerStats.hasStats), so every fixture here
  // sets those so the worker appears in the card at all.
  const withStats = (over: Partial<Worker>): Worker =>
    aWorker({ stats_source: "cgroup", stats_mem_bytes: 1_000_000, ...over });

  it("shows the cordoned pill for a drained worker holding no runs", async () => {
    mockApi.listWorkers.mockResolvedValue({
      workers: [withStats({ draining_since: "2026-07-05T12:00:00Z", active_runs: 0 })],
    });
    renderDashboard();
    await flush();
    // Anchor on the card title so a mutation that drops the worker (not just the badge)
    // still fails at the badge lookup below.
    expect(screen.getByText("Worker load")).toBeTruthy();
    expect(screen.getByText("cordoned")).toBeTruthy();
    expect(screen.queryByText("draining")).toBeNull();
  });

  it("shows the draining pill for a cordoned worker still holding a run", async () => {
    mockApi.listWorkers.mockResolvedValue({
      workers: [withStats({ draining_since: "2026-07-05T12:00:00Z", active_runs: 1 })],
    });
    renderDashboard();
    await flush();
    expect(screen.getByText("Worker load")).toBeTruthy();
    expect(screen.getByText("draining")).toBeTruthy();
    expect(screen.queryByText("cordoned")).toBeNull();
  });
});
