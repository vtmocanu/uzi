// @vitest-environment jsdom
import { afterEach, describe, it, expect } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { RunRow } from "./RunsList";
import type { RunActivity, RunListItem } from "../lib/api";
import { setDemoMode } from "../lib/demoMode";

// PRD #1064 M3: the runs-list row's "now" line (read from the current_activity DTO), its
// terminal-hiding, the ◐ badge suffix, and the D5 byte-compat guard. RunRow is rendered
// directly inside a Router (no layout fetch). `now` is passed in, so the age token is
// deterministic without mocking a clock.

const AT = "2026-07-05T12:00:00Z";
const NOW = Date.parse(AT) + 40_000; // 40s after the activity instant → "40s"

function anActivity(over: Partial<RunActivity> = {}): RunActivity {
  return {
    agent: "coder",
    agent_label: "Expose heartbeat freshness gauge",
    tool: "Edit",
    detail: "api/internal/handler/metrics.go",
    at: AT,
    seq: 12,
    ...over,
  };
}

function aRun(over: Partial<RunListItem> = {}): RunListItem {
  return {
    id: "run-1",
    repo_id: "repo-1",
    forge_type: "gitlab",
    mr_web_url: null,
    issue_web_url: null,
    kind: "issue",
    issue_iid: 7,
    issue_title: "A run",
    issue_description: "",
    harness: "claude", // PRD #1429 M1: harness joined RunDTO.
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
    codex_account_action: null,
    codex_secret_label: null,
    recovery_retry_not_before: null,
    forge_park_count: 0,
    forge_park_max: 0,
    claimed_at: null,
    started_at: null,
    finished_at: null,
    created_at: AT,
    updated_at: AT,
    repo_path: "grp/proj",
    worker_name: "w1",
    judge_verdict: null,
    judge_todo_count: 0,
    ...over,
  };
}

const milestones = [
  { id: "m1", title: "First" },
  { id: "m2", title: "Second" },
  { id: "m3", title: "Third" },
];

function renderRow(run: RunListItem) {
  return render(
    <MemoryRouter>
      <RunRow run={run} now={NOW} />
    </MemoryRouter>,
  );
}

afterEach(() => {
  cleanup();
  setDemoMode(false);
});

describe("RunsList row now line (PRD #1064 M3)", () => {
  it("renders the now line for a non-terminal run carrying current_activity", () => {
    renderRow(
      aRun({
        status: "running",
        current_activity: anActivity(),
        milestones,
        milestones_in_progress: ["m2"],
      }),
    );
    expect(screen.getByText("coder")).toBeTruthy();
    // The first in-progress milestone id (D4) is shown on the compact line.
    expect(screen.getByText("m2")).toBeTruthy();
    expect(screen.getByText("Expose heartbeat freshness gauge")).toBeTruthy();
    const age = screen.getByText("· 40s ago");
    expect(age.previousElementSibling?.textContent).toBe("Expose heartbeat freshness gauge");
    expect(age.className).not.toContain("ml-auto");
  });

  it("omits the age and its separator for an invalid activity timestamp", () => {
    const { container } = renderRow(aRun({ current_activity: anActivity({ at: "invalid" }) }));
    expect(screen.getByText("Expose heartbeat freshness gauge")).toBeTruthy();
    expect(container.textContent).not.toContain("ago");
    expect(container.querySelectorAll(".italic + span")).toHaveLength(0);
  });

  it("centres the logo group and keeps meta separators attached to their items", () => {
    const { container } = renderRow(aRun({
      worker_name: "worker A",
      usage: { input_tokens: 1000, cache_read_tokens: 0, cache_creation_tokens: 0, output_tokens: 200, cost_usd: 0, cost_status: "subscription" },
    }));
    const title = screen.getByText("A run");
    expect(title.parentElement?.parentElement?.className).toContain("items-center");
    const meta = container.querySelector("p.flex.flex-wrap")!;
    expect(meta.className).toContain("gap-x-4");
    expect(meta.className).toContain("overflow-x-clip");
    for (const item of Array.from(meta.children).slice(1)) {
      expect(item.className).not.toContain("font-mono");
      expect(item.className).toContain("relative inline-flex");
      expect(item.className).not.toContain("gap-2");
      expect(item.firstElementChild?.textContent).toBe("·");
      expect(item.firstElementChild?.className).toContain("absolute -left-4 w-4 text-center");
    }
    expect(meta.textContent).toContain("subscription");
  });

  it("keeps the MR separator outside its chip and masks repo and owner in demo mode", () => {
    setDemoMode(true);
    const { container } = render(
      <MemoryRouter>
        <RunRow run={aRun({ mr_iid: 12, repo_path: "private/project", owner_email: "alice.smith@example.test" })} now={NOW} showOwner />
      </MemoryRouter>,
    );
    const meta = container.querySelector("p.flex.flex-wrap")!;
    expect(meta.textContent).toContain("demo/project");
    expect(meta.textContent).toContain("Alice");
    expect(meta.textContent).not.toContain("private/");
    expect(meta.textContent).not.toContain("alice.smith@");
    const mrItem = Array.from(meta.children).find((item) => item.textContent?.includes("MR !12"))!;
    expect(mrItem.firstElementChild?.textContent).toBe("·");
    expect(mrItem.lastElementChild?.textContent).toBe("MR !12");
  });

  it("HIDES the now line for a terminal run even when current_activity is populated", () => {
    renderRow(
      aRun({ status: "completed", finished_at: AT, current_activity: anActivity() }),
    );
    expect(screen.queryByText("coder")).toBeNull();
    expect(screen.queryByText("Expose heartbeat freshness gauge")).toBeNull();
  });

  it("renders NO now line for a run with a null current_activity", () => {
    renderRow(aRun({ status: "running", current_activity: null }));
    expect(screen.queryByText("Expose heartbeat freshness gauge")).toBeNull();
  });

  it("adds a ◐ suffix to the milestone badge only while a milestone is in progress", () => {
    const { rerender } = renderRow(
      aRun({ milestones, milestones_completed: ["m1"], milestones_in_progress: ["m2"] }),
    );
    expect(screen.getByText("M1/3 ◐")).toBeTruthy();

    rerender(
      <MemoryRouter>
        <RunRow
          run={aRun({ milestones, milestones_completed: ["m1"], milestones_in_progress: [] })}
          now={NOW}
        />
      </MemoryRouter>,
    );
    expect(screen.getByText("M1/3")).toBeTruthy();
    expect(screen.queryByText("M1/3 ◐")).toBeNull();
  });

  it("folds untrusted activity fields through stripUnsafeChars", () => {
    renderRow(
      aRun({
        status: "running",
        // U+202E RIGHT-TO-LEFT OVERRIDE in the role, a zero-width space (U+200B) in the
        // label — both written as escapes, never pasted raw into source.
        current_activity: anActivity({ agent: "co\u202Eder", agent_label: "task\u200Bone" }),
      }),
    );
    expect(screen.getByText("coder")).toBeTruthy();
    expect(screen.getByText("taskone")).toBeTruthy();
  });

  // D5 byte-compat: a null-milestone, null-activity run carries none of the new markers,
  // so it renders exactly as a pre-#1064 row. The snapshot pins that no now line / ◐ crept in.
  it("D5: a null-milestone null-activity run renders with no now line and no ◐ (snapshot)", () => {
    const { container } = renderRow(
      aRun({ status: "running", milestones: null, milestones_in_progress: null, current_activity: null }),
    );
    expect(container.textContent).not.toContain("◐");
    // The now-line dot is the only bg-ok element this row would add; the pre-existing
    // running-status pill pulses with bg-current, so target bg-ok specifically.
    expect(container.querySelector(".bg-ok")).toBeNull();
    expect(container).toMatchSnapshot();
  });
});
