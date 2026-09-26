// @vitest-environment jsdom
import { afterEach, describe, it, expect } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { RunRow } from "./RunsList";
import type { RunActivity, RunListItem } from "../lib/api";
import { setDemoMode } from "../lib/demoMode";

// PRD #1064 M3: the runs-list row's "now" line (read from the current_activity DTO), its
// terminal-hiding, the ◐ badge suffix, and the D5 no-now-line guard. RunRow is rendered
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
  it.each(["claude", "codex"] as const)("places one %s logo beside the mobile title and gives lower lines full width", (harness) => {
    const title = "A long title ".repeat(12);
    const { container } = renderRow(aRun({
      harness,
      issue_title: title,
      summary_intent: "A summary",
      current_activity: anActivity(),
    }));
    const leading = container.querySelector("li > div")?.firstElementChild as HTMLElement;
    const logo = leading.querySelector(`[role="img"][aria-label="${harness === "claude" ? "Claude" : "Codex"}"]`) as HTMLElement;
    expect(leading.className).toContain("grid-cols-[1rem_minmax(0,1fr)] gap-x-1.5");
    expect(leading.className).toContain("sm:grid-cols-[1.75rem_minmax(0,1fr)] sm:gap-x-2");
    expect(leading.querySelectorAll('[role="img"]')).toHaveLength(1);
    expect(logo.querySelectorAll("svg")).toHaveLength(1);
    expect(logo.className).toContain("h-4 w-4");
    const titleLine = leading.querySelector("p")!;
    expect(titleLine.textContent).toBe(title);
    expect(titleLine.previousElementSibling).toBe(logo);
    expect(titleLine.className).toContain("col-start-2");
    expect(titleLine.className).toContain("truncate");
    for (const line of [screen.getByText("A summary"), screen.getByText("coder").closest("p")!, leading.lastElementChild!]) {
      expect(line.className).toContain("col-span-2 sm:col-span-1 sm:col-start-2");
    }
  });

  it.each([null, undefined, "future-harness"])("removes the logo track for harness %s", (harness) => {
    const { container } = renderRow(aRun({ harness: harness as RunListItem["harness"] }));
    const leading = container.querySelector("li > div")?.firstElementChild as HTMLElement;
    expect(leading.className).toContain("grid-cols-1");
    expect(leading.className).not.toContain("gap-x");
    expect(leading.querySelector('[role="img"]')).toBeNull();
    expect(screen.getByText("A run").className).not.toContain("col-start-2");
  });

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
      // No positioned item: a relative/absolute item would paint above the stretched run
      // link and make the meta text a dead click zone (review of #1708).
      expect(item.className).toContain("inline-flex");
      expect(item.className).not.toMatch(/\b(relative|absolute)\b/);
      expect(item.className).not.toContain("gap-2");
      expect(item.firstElementChild?.textContent).toBe("·");
      expect(item.firstElementChild?.className).toContain("-ml-4 w-4 shrink-0 text-center");
      expect(item.firstElementChild?.getAttribute("aria-hidden")).toBe("true");
    }
    expect(meta.children[0].className).not.toMatch(/\b(relative|absolute)\b/);
    // A space precedes every hidden dot, so textContent and screen readers keep items apart.
    expect(meta.textContent).toContain(" ·worker A");
    expect(meta.textContent).toContain("subscription");
  });

  it("lets a long repo path truncate before the issue reference clips", () => {
    const path = "grp/" + "a".repeat(80);
    renderRow(aRun({ repo_path: path }));
    const repo = screen.getByText(path);
    expect(repo.className).toContain("min-w-0 truncate");
    expect(repo.parentElement?.className).toContain("min-w-0 max-w-full");
    expect(repo.nextElementSibling?.className).toContain("shrink-0");
    expect(repo.nextElementSibling?.textContent).toContain("#7");
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

  // D5: a null-milestone, null-activity run carries none of the now-line markers.
  // The snapshot pins that no now line / ◐ crept in as the row layout evolves.
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
