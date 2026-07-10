// @vitest-environment jsdom
import { afterEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { PlanPanel, AgentRosterSummary } from "./RunView";
import { api, type AgentTemplate, type Run } from "../lib/api";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return { ...actual, api: { listAgentTemplates: vi.fn() } };
});
const mockApi = vi.mocked(api);

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

function tmpl(name: string, scope: AgentTemplate["scope"] = "builtin"): AgentTemplate {
  return {
    id: name,
    name,
    description: `desc ${name}`,
    model: null,
    tools: null,
    prompt_body: "b",
    is_builtin: scope === "builtin",
    scope,
    user_id: null,
    updated_by: null,
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
  };
}

function run(over: Partial<Run>): Run {
  return {
    id: "r1",
    repo_id: "repo1",
    kind: "issue",
    issue_iid: 87,
    issue_title: "Add rate limiting",
    issue_description: "d",
    status: "awaiting_approval",
    requeue_count: 0,
    iteration_count: 0,
    auto_approve: false,
    worker_id: "w1",
    branch: null,
    mr_iid: null,
    mr_state: null,
    failure_reason: null,
    stop_kind: null,
    pipeline_ref: null,
    pipeline_web_url: null,
    fix_verdict: null,
    plan_md: "# Plan\n- step one",
    repo_agents: null,
    agent_source: null,
    agent_exclusions: null,
    claimed_at: null,
    started_at: null,
    finished_at: null,
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    ...over,
  };
}

describe("PlanPanel agent picker (PRD #37 M4)", () => {
  it("State A: repo detected → approve button labels the repo-agent default", async () => {
    mockApi.listAgentTemplates.mockResolvedValue({ templates: [tmpl("lead"), tmpl("coder")] });
    const onApprove = vi.fn();
    render(
      <PlanPanel
        run={run({
          repo_agents: [
            { name: "coder", description: "impl" },
            { name: "reviewer", description: "rev" },
          ],
        })}
        busy={false}
        onApprove={onApprove}
        onReject={() => {}}
      />,
    );
    // The button label reflects the default repo selection.
    const approve = await screen.findByRole("button", { name: /Approve plan · 2 repo agents/ });
    fireEvent.click(approve);
    expect(onApprove).toHaveBeenCalledWith({ source: "repo", exclusions: [] });
  });

  it("excluding a repo agent updates the approve label and the submitted selection", async () => {
    mockApi.listAgentTemplates.mockResolvedValue({ templates: [tmpl("lead")] });
    const onApprove = vi.fn();
    render(
      <PlanPanel
        run={run({
          repo_agents: [
            { name: "coder", description: "impl" },
            { name: "reviewer", description: "rev" },
            { name: "tester", description: "t" },
          ],
        })}
        busy={false}
        onApprove={onApprove}
        onReject={() => {}}
      />,
    );
    await screen.findByRole("button", { name: /Approve plan · 3 repo agents/ });
    fireEvent.click(screen.getByRole("button", { name: /^●?\s*tester/i }));
    const approve = screen.getByRole("button", { name: /Approve plan · 2 repo agents/ });
    fireEvent.click(approve);
    expect(onApprove).toHaveBeenCalledWith({ source: "repo", exclusions: ["tester"] });
  });

  it("State B: no repo agents → own templates default, lead filtered out of the chips", async () => {
    mockApi.listAgentTemplates.mockResolvedValue({
      templates: [tmpl("lead"), tmpl("coder"), tmpl("reviewer")],
    });
    const onApprove = vi.fn();
    render(<PlanPanel run={run({ repo_agents: [] })} busy={false} onApprove={onApprove} onReject={() => {}} />);
    // Own is default: label counts the 2 non-lead templates.
    await screen.findByRole("button", { name: /Approve plan · 2 of your templates/ });
    // The lead is NOT a selectable chip (pinned, not in the roster).
    expect(screen.queryByRole("button", { name: /^●?\s*lead/i })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: /Approve plan · 2 of your templates/ }));
    expect(onApprove).toHaveBeenCalledWith({ source: "own", exclusions: [] });
  });

  it("the plan markdown still renders", async () => {
    mockApi.listAgentTemplates.mockResolvedValue({ templates: [] });
    render(<PlanPanel run={run({ repo_agents: [] })} busy={false} onApprove={() => {}} onReject={() => {}} />);
    expect(await screen.findByText("step one")).toBeTruthy();
  });
});

describe("AgentRosterSummary (read-only, post-approval)", () => {
  it("a repo-source run states the internal review was repo-authored, inertly", () => {
    const { container } = render(
      <AgentRosterSummary
        run={run({
          status: "running",
          agent_source: "repo",
          agent_exclusions: ["tester"],
          repo_agents: [
            { name: "coder", description: "See http://evil.example — click" },
            { name: "reviewer", description: "rev" },
            { name: "tester", description: "t" },
          ],
        })}
      />,
    );
    expect(screen.getByText(/repository's own agents/i)).toBeTruthy();
    // Included = roster minus exclusions; the excluded one is listed separately.
    expect(screen.getByText("coder")).toBeTruthy();
    expect(screen.getByText("reviewer")).toBeTruthy();
    expect(screen.getByText(/Excluded: tester/)).toBeTruthy();
    // No linkification of any repo-supplied text.
    expect(container.querySelector("a")).toBeNull();
  });

  it("an own-source run says so and shows no repo marker", () => {
    render(<AgentRosterSummary run={run({ status: "completed", agent_source: "own", agent_exclusions: [] })} />);
    expect(screen.getByText(/your uzi agent templates/i)).toBeTruthy();
    expect(screen.queryByText(/repository's own agents/i)).toBeNull();
  });
});
