// @vitest-environment jsdom
// Issue #1727: a paused run's "Paused by you at" time and its header "· clock stopped" elapsed
// are measured to when the pause LANDED, status_since, not updated_at (which moves on unrelated
// writes such as an extend). The fixtures carry NO budget fields, so extendBudgetView returns
// null and PausedElapsed takes its plain-elapsed fallback branch, the one that reads the
// instant. updated_at sits hours after status_since, so an updated_at read differs visibly.
import { afterEach, describe, it, expect, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { RunView } from "./RunView";
import { useRunStream } from "../lib/useRunStream";
import { type Run } from "../lib/api";
import { extendBudgetView } from "../lib/budget";
import { formatDuration } from "../components/RunEvent";
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
      // PRD #1296 M5: the run page's Recovery archives section fetches its own summary on
      // mount. Defaulted to a supported-but-empty aggregate so a full-page render settles
      // and the section renders nothing on these non-recovery budget fixtures.
      getRunArchives: vi.fn().mockResolvedValue({
        supported: false,
        legacy: true,
        has_open_hold: false,
        counts: {
          preparing: 0,
          uploading: 0,
          available: 0,
          needs_action: 0,
          expired: 0,
          discarded: 0,
        },
        archives: [],
      }),
    },
  };
});

vi.mock("../lib/useRunStream", () => ({ useRunStream: vi.fn() }));
const mockUseRunStream = vi.mocked(useRunStream);

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
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
    harness: "claude", // PRD #1429 M1: harness joined RunDTO.
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
    recovery_wait_cause: null,
    codex_account_action: null,
    codex_secret_label: null,
    recovery_retry_not_before: null,
    forge_park_count: 0,
    forge_park_max: 0,
    claimed_at: null,
    started_at: null,
    finished_at: null,
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    ...over,
  };
}

function renderPage(over: Partial<Run>) {
  mockUseRunStream.mockReturnValue({
    run: run(over),
    messages: [],
    connected: true,
    error: "",
    submit: vi.fn(),
    refreshRun: vi.fn(),
    inputs: [],
    canSteer: true,
  } as unknown as ReturnType<typeof useRunStream>);
  return render(
    <MemoryRouter initialEntries={["/runs/r1"]}>
      <RunView />
    </MemoryRouter>,
  );
}

// started_at < status_since < updated_at. status_since and updated_at are 4h25m apart on the
// same day, so they render as different local wall-clock times in any timezone.
const STARTED = "2026-01-01T08:00:00Z";
const STATUS_SINCE = "2026-01-01T09:17:00Z";
const UPDATED = "2026-01-01T13:42:00Z";

// A paused run with no budget: budget_wall_seconds/total/used/extension all null.
const PAUSED: Partial<Run> = {
  status: "paused",
  started_at: STARTED,
  updated_at: UPDATED,
  branch: "agent/issue-87",
  budget_wall_seconds: null,
  budget_total_seconds: null,
  budget_used_seconds: null,
  budget_extension_seconds: undefined,
};

// The PausedPanel heading's own formatting (RunView.tsx PausedPanel).
function localTime(iso: string): string {
  return new Date(Date.parse(iso)).toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" });
}

function elapsedText(toIso: string): string {
  return `${formatDuration(Date.parse(toIso) - Date.parse(STARTED))} · clock stopped`;
}

async function expectAnchoredOn(expectedIso: string, otherIso: string) {
  await screen.findByText("Add rate limiting");
  const heading = screen.getByText(/Paused by you/);
  expect(heading.textContent).toContain(`Paused by you at ${localTime(expectedIso)}`);
  expect(heading.textContent).not.toContain(localTime(otherIso));
  expect(screen.getByText(elapsedText(expectedIso))).toBeTruthy();
  expect(screen.queryByText(elapsedText(otherIso))).toBeNull();
}

describe("RunView — a paused run reads status_since, falling back to updated_at (issue #1727)", () => {
  it("the fixture takes PausedElapsed's no-budget fallback branch", () => {
    expect(extendBudgetView(run(PAUSED), Date.now())).toBeNull();
  });

  it("status_since present: the pause time and the elapsed follow status_since, not updated_at", async () => {
    renderPage({ ...PAUSED, status_since: STATUS_SINCE });
    await expectAnchoredOn(STATUS_SINCE, UPDATED);
  });

  it("status_since omitted (an older server): both follow updated_at", async () => {
    const over: Partial<Run> = { ...PAUSED };
    delete over.status_since;
    renderPage(over);
    await expectAnchoredOn(UPDATED, STATUS_SINCE);
  });

  it("status_since null: both follow updated_at", async () => {
    renderPage({ ...PAUSED, status_since: null });
    await expectAnchoredOn(UPDATED, STATUS_SINCE);
  });

  it("status_since unparseable: both follow updated_at", async () => {
    renderPage({ ...PAUSED, status_since: "not-a-time" });
    await expectAnchoredOn(UPDATED, STATUS_SINCE);
  });
});
