// @vitest-environment jsdom
// PRD #1189 M2: the budget-aware header elapsed, the conditional Extend action, the extended
// header, and the near-timeout panel. Whole-page cases mock useRunStream (RunView's only data
// source) so the controls are exercised where they live; NearTimeoutPanel is also rendered
// directly for the checkpoint-omission case.
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { NearTimeoutPanel, RunView } from "./RunView";
import { useRunStream } from "../lib/useRunStream";
import { api, type Run } from "../lib/api";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      getRunReview: vi.fn().mockResolvedValue({ review: null, pending_judge: null }),
      rerunJudge: vi.fn(),
      listRepos: vi.fn().mockResolvedValue({ repos: [] }),
      listWorkers: vi.fn().mockResolvedValue({ workers: [] }),
      getMySettings: vi.fn().mockResolvedValue({ settings: { mr_rework_enabled: null } }),
      setRunMrRework: vi.fn().mockResolvedValue({ run: null }),
    },
  };
});
const mockApi = vi.mocked(api);

vi.mock("../lib/useRunStream", () => ({ useRunStream: vi.fn() }));
const mockUseRunStream = vi.mocked(useRunStream);

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});
beforeEach(() => {
  mockApi.getRunReview.mockResolvedValue({ review: null, pending_judge: null });
});

function run(over: Partial<Run>): Run {
  return {
    id: "r1",
    repo_id: "repo1",
    forge_type: "gitlab",
    mr_web_url: null,
    issue_web_url: null,
    kind: "issue",
    issue_iid: 87,
    issue_title: "Add rate limiting",
    issue_description: "d",
    title: null,
    resume_of_run_id: null,
    status: "running",
    requeue_count: 0,
    iteration_count: 0,
    auto_approve: false,
    worker_id: "w1",
    branch: "agent/issue-87",
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
    milestones: null,
    milestones_completed: null,
    milestones_in_progress: null,
    milestones_candidate: null,
    budget_max_iterations: null,
    budget_wall_seconds: null,
    anthropic_secret_id: null,
    anthropic_secret_label: null,
    anthropic_select_reason: null,
    anthropic_headroom_pct: null,
    wait_on_limit: false,
    limit_resets_at: null,
    retry_not_before: null,
    limit_wait_count: 0,
    rate_limit_type: null,
    claimed_at: null,
    started_at: null,
    finished_at: null,
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    ...over,
  };
}

// A running run with an 8h budget whose deadline is `remainingMin` minutes out.
function budgeted(over: Partial<Run> = {}, remainingMin = 65): Partial<Run> {
  return {
    status: "running",
    started_at: new Date(Date.now() - (8 * 3600 - remainingMin * 60) * 1000).toISOString(),
    deadline_at: new Date(Date.now() + remainingMin * 60 * 1000).toISOString(),
    budget_wall_seconds: 28800,
    budget_total_seconds: 28800,
    budget_used_seconds: 28800 - remainingMin * 60,
    budget_extension_seconds: 0,
    budget_extension_cap_seconds: 57600,
    ...over,
  };
}

function renderPage(over: Partial<Run>, canSteer = true) {
  const submit = vi.fn();
  const refreshRun = vi.fn();
  mockUseRunStream.mockReturnValue({
    run: run(over),
    messages: [],
    connected: true,
    error: "",
    submit,
    refreshRun,
    inputs: [],
    canSteer,
  } as unknown as ReturnType<typeof useRunStream>);
  const utils = render(
    <MemoryRouter initialEntries={["/runs/r1"]}>
      <RunView />
    </MemoryRouter>,
  );
  return { ...utils, submit, refreshRun };
}

describe("RunView — budget-aware elapsed (PRD #1189)", () => {
  it("shows `/ 8h` for a running issue run that carries a budget", async () => {
    renderPage(budgeted());
    await screen.findByText("Add rate limiting");
    expect(screen.getByText(/\/\s*8h/)).toBeTruthy();
  });

  it("keeps today's plain elapsed for a null-budget kind (a chat run)", async () => {
    renderPage({ kind: "chat", status: "running", started_at: new Date(Date.now() - 300_000).toISOString() });
    await screen.findByText("Add rate limiting");
    // No budget "/ Nh" text (the breadcrumb's "/" is not followed by a duration).
    expect(screen.queryByText(/\/\s*\d+h/)).toBeNull();
  });

  it("reads `/ 8h+2h` on an extended run, never `/ 10h`", async () => {
    renderPage(
      budgeted({ budget_extension_seconds: 7200, budget_total_seconds: 36000, health: "ok" }),
    );
    await screen.findByText("Add rate limiting");
    expect(screen.getByText(/\/\s*8h\+2h/)).toBeTruthy();
    expect(screen.queryByText(/\/\s*10h/)).toBeNull();
    // The flag chip is gone once the run is healthy again.
    expect(screen.queryByText(/near timeout/i)).toBeNull();
  });
});

describe("RunView — conditional Extend action (PRD #1189)", () => {
  it("renders Extend only when the near-timeout flag is up, and only for the owner", async () => {
    renderPage(budgeted({ health: "slow", health_since: "2026-01-01T00:00:00Z" }), true);
    await screen.findByText("Add rate limiting");
    expect(screen.getAllByRole("button", { name: /extend time/i }).length).toBeGreaterThan(0);
  });

  it("shows inert text (never a button) to a non-owner on a flagged run", async () => {
    renderPage(budgeted({ health: "slow", health_since: "2026-01-01T00:00:00Z" }), false);
    await screen.findByText("Add rate limiting");
    expect(screen.queryByRole("button", { name: /extend time/i })).toBeNull();
    expect(screen.getAllByText(/only the run's owner can extend/i).length).toBeGreaterThan(0);
  });

  it("does NOT render Extend on a healthy running run", async () => {
    renderPage(budgeted({ health: "ok" }), true);
    await screen.findByText("Add rate limiting");
    expect(screen.queryByRole("button", { name: /extend time/i })).toBeNull();
  });

  it("hides Extend when the cap is 0, even on a flagged run", async () => {
    renderPage(
      budgeted({ health: "slow", health_since: "2026-01-01T00:00:00Z", budget_extension_cap_seconds: 0 }),
      true,
    );
    await screen.findByText("Add rate limiting");
    expect(screen.queryByRole("button", { name: /extend time/i })).toBeNull();
  });

  it("Confirm posts an `extend` input with the chosen seconds body", async () => {
    const { submit } = renderPage(
      budgeted({ health: "slow", health_since: "2026-01-01T00:00:00Z" }),
      true,
    );
    await screen.findByText("Add rate limiting");
    // Open the first Extend chooser (the header one) and confirm the preselected +2h.
    fireEvent.click(screen.getAllByRole("button", { name: /extend time/i })[0]);
    const dialog = screen.getAllByRole("dialog", { name: /extend this run/i })[0];
    await act(async () => {
      fireEvent.click(within(dialog).getByRole("button", { name: /Extend by 2h/ }));
    });
    await waitFor(() => expect(submit).toHaveBeenCalledWith("extend", "7200"));
  });
});

// The Extend header action AND the near-timeout panel are BOTH gated on
// `shouldShowHealthFlag(...) && run.health === "slow"`. The other flaggable health values
// (stalled/looping/waiting_worker/approval_idle) render a warn chip but must NOT surface
// Extend or the near-timeout panel — that is a near-TIMEOUT affordance, not a general
// unhealthy-run one. Folding away the `=== "slow"` conjunct leaves the happy-path tests green,
// so these pin it: each redden if the conjunct is dropped from either gate.
describe("RunView — Extend/near-timeout are gated to `slow`, not any flag (PRD #1189)", () => {
  const flaggableNonSlow = ["stalled", "looping", "waiting_worker", "approval_idle"] as const;

  it.each(flaggableNonSlow)(
    "renders NO Extend button and NO near-timeout panel for health=%s",
    async (health) => {
      renderPage(budgeted({ health, health_since: "2026-01-01T00:00:00Z" }), true);
      await screen.findByText("Add rate limiting");
      // The flag chip itself still shows (it is flaggable) — but neither near-timeout surface.
      expect(screen.queryByRole("button", { name: /extend time/i })).toBeNull();
      expect(screen.queryByText(/close to its time limit/i)).toBeNull();
      expect(screen.queryByText(/unless you extend it/i)).toBeNull();
    },
  );
});

describe("NearTimeoutPanel (PRD #1189)", () => {
  const slow = run(budgeted({ health: "slow", health_since: "2026-01-01T00:00:00Z" }));

  it("states used / budget / deadline and offers Extend + Stop for the owner", () => {
    render(<NearTimeoutPanel run={slow} busy={false} canSteer onExtend={() => {}} onStop={() => {}} />);
    expect(screen.getByText(/close to its time limit/i)).toBeTruthy();
    expect(screen.getByText(/unless you extend it/i)).toBeTruthy();
    expect(screen.getByRole("button", { name: /extend time/i })).toBeTruthy();
    expect(screen.getByRole("button", { name: /stop run/i })).toBeTruthy();
  });

  it("includes the checkpoint sentence when checkpoint_tip_at is set", () => {
    render(
      <NearTimeoutPanel
        run={run(budgeted({ health: "slow", checkpoint_tip_at: new Date(Date.now() - 9 * 60_000).toISOString() }))}
        busy={false}
        canSteer
        onExtend={() => {}}
        onStop={() => {}}
      />,
    );
    expect(screen.getByText(/the last checkpoint was pushed/i)).toBeTruthy();
  });

  it("OMITS the checkpoint sentence when checkpoint_tip_at is null", () => {
    render(
      <NearTimeoutPanel
        run={run(budgeted({ health: "slow", checkpoint_tip_at: null }))}
        busy={false}
        canSteer
        onExtend={() => {}}
        onStop={() => {}}
      />,
    );
    expect(screen.queryByText(/the last checkpoint was pushed/i)).toBeNull();
  });

  it("shows inert owner-only text (no buttons) to a non-owner", () => {
    render(<NearTimeoutPanel run={slow} busy={false} canSteer={false} onExtend={() => {}} onStop={() => {}} />);
    expect(screen.queryByRole("button", { name: /extend time/i })).toBeNull();
    expect(screen.queryByRole("button", { name: /stop run/i })).toBeNull();
    expect(screen.getByText(/only the run's owner can extend or stop it/i)).toBeTruthy();
  });

  it("self-hides on a healthy run", () => {
    const { container } = render(
      <NearTimeoutPanel run={run(budgeted({ health: "ok" }))} busy={false} canSteer onExtend={() => {}} onStop={() => {}} />,
    );
    expect(container.textContent).toBe("");
  });
});
