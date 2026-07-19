// @vitest-environment jsdom
import { afterEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { PlanPanel, AgentRosterSummary, JudgePanel } from "./RunView";
import { api, type IssueDraft, type Repo, type RepoAgent, type Run, type RunReview } from "../lib/api";

// The picker no longer fetches the template list (PRD #37 M4-fix — it reads the
// run's own_agents instead). listAgentTemplates is mocked only so a test can assert
// it is NEVER called. getRunReview/rerunJudge back the PRD #46 M4 judge panel.
vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      listAgentTemplates: vi.fn(),
      getRunReview: vi.fn(),
      rerunJudge: vi.fn(),
      // PRD #68 M4: the file-issue draft/write + the picker's repo list. Defaulted to an
      // empty picker so the panel's best-effort repos fetch resolves cleanly.
      listRepos: vi.fn().mockResolvedValue({ repos: [] }),
      getIssueDraft: vi.fn(),
      fileIssue: vi.fn(),
    },
  };
});
const mockApi = vi.mocked(api);

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

function agent(name: string): RepoAgent {
  return { name, description: `desc ${name}` };
}

function run(over: Partial<Run>): Run {
  return {
    id: "r1",
    repo_id: "repo1",
    forge_type: "gitlab",
    mr_web_url: null,
    kind: "issue",
    issue_iid: 87,
    issue_title: "Add rate limiting",
    issue_description: "d",
    title: null,
    resume_of_run_id: null,
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
    health: "ok",
    health_reason: null,
    health_since: null,
    pipeline_ref: null,
    pipeline_web_url: null,
    fix_verdict: null,
    plan_md: "# Plan\n- step one",
    repo_agents: null,
    agent_source: null,
    agent_exclusions: null,
    own_agents: null,
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
    const onApprove = vi.fn();
    render(
      <PlanPanel
        run={run({
          repo_agents: [agent("coder"), agent("reviewer")],
          own_agents: [agent("coder")],
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
    const onApprove = vi.fn();
    render(
      <PlanPanel
        run={run({
          repo_agents: [agent("coder"), agent("reviewer"), agent("tester")],
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

  it("State B: no repo agents → own templates from run.own_agents (lead already stripped server-side)", async () => {
    const onApprove = vi.fn();
    render(
      <PlanPanel
        run={run({ repo_agents: [], own_agents: [agent("coder"), agent("reviewer")] })}
        busy={false}
        onApprove={onApprove}
        onReject={() => {}}
      />,
    );
    // Own is default: label counts the 2 own agents the server delivered.
    await screen.findByRole("button", { name: /Approve plan · 2 of your templates/ });
    // The own chips are the two the run carries — the lead never appears (the server
    // strips it before own_agents), and there was no global fetch.
    expect(screen.getByRole("button", { name: /^●?\s*coder/i })).toBeTruthy();
    expect(screen.getByRole("button", { name: /^●?\s*reviewer/i })).toBeTruthy();
    expect(screen.queryByRole("button", { name: /^●?\s*lead/i })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: /Approve plan · 2 of your templates/ }));
    expect(onApprove).toHaveBeenCalledWith({ source: "own", exclusions: [] });
    // M4-fix: the picker must NOT reach for the global template list anymore.
    expect(mockApi.listAgentTemplates).not.toHaveBeenCalled();
  });

  it("empty own roster (lead-only owner) still approves as a lead-only run", async () => {
    const onApprove = vi.fn();
    render(
      <PlanPanel
        run={run({ repo_agents: [], own_agents: [] })}
        busy={false}
        onApprove={onApprove}
        onReject={() => {}}
      />,
    );
    // No selectable agents on either source → the button drops the count suffix.
    const approve = await screen.findByRole("button", { name: /^Approve plan$/ });
    fireEvent.click(approve);
    expect(onApprove).toHaveBeenCalledWith({ source: "own", exclusions: [] });
  });

  it("the plan markdown still renders", async () => {
    render(<PlanPanel run={run({ repo_agents: [], own_agents: [] })} busy={false} onApprove={() => {}} onReject={() => {}} />);
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

  it("an own-source run says so and lists the actual own agent names (M4-fix)", () => {
    render(
      <AgentRosterSummary
        run={run({
          status: "completed",
          agent_source: "own",
          agent_exclusions: ["reviewer"],
          own_agents: [agent("coder"), agent("reviewer"), agent("tester")],
        })}
      />,
    );
    expect(screen.getByText(/your uzi agent templates/i)).toBeTruthy();
    expect(screen.queryByText(/repository's own agents/i)).toBeNull();
    // Own names now render (they were carried on the run row); the excluded one is
    // pulled out into the "Excluded" line.
    expect(screen.getByText("coder")).toBeTruthy();
    expect(screen.getByText("tester")).toBeTruthy();
    expect(screen.getByText(/Excluded: reviewer/)).toBeTruthy();
  });
});

describe("JudgePanel (PRD #46 M4)", () => {
  function review(over: Partial<RunReview> = {}): RunReview {
    return {
      id: "rev1",
      target_run_id: "r1",
      verdict: "issues",
      summary_md: "Lost time to a missing tool.",
      judge_model: "haiku",
      status: "complete",
      created_at: "2026-01-01T00:00:00Z",
      updated_at: "2026-01-01T00:00:00Z",
      recommendations: [
        {
          id: "rc1",
          category: "install_worker_tool",
          target: "shellcheck",
          rationale_md: "hit command not found twice",
          confidence: "high",
          created_at: "2026-01-01T00:00:00Z",
        },
      ],
      filed_issues: [],
      ...over,
    };
  }

  it("renders the verdict chip + recommendations for a judged run", async () => {
    mockApi.getRunReview.mockResolvedValue({ review: review() });
    render(<JudgePanel run={run({ status: "completed" })} />);

    expect(await screen.findByText("Issues found")).toBeTruthy();
    expect(screen.getByText("Lost time to a missing tool.")).toBeTruthy();
    expect(screen.getByText("Install a worker tool")).toBeTruthy();
    expect(screen.getByText("shellcheck")).toBeTruthy();
    expect(screen.getByText("hit command not found twice")).toBeTruthy();
    expect(mockApi.getRunReview).toHaveBeenCalledWith("r1");
  });

  it("renders review free text as escaped text, never HTML", async () => {
    // A rationale containing markup must appear as literal characters (React escapes
    // it) — proving no dangerouslySetInnerHTML / markdown parsing on judge output.
    mockApi.getRunReview.mockResolvedValue({
      review: review({
        summary_md: "<img src=x onerror=alert(1)> **not bold**",
        recommendations: [],
      }),
    });
    const { container } = render(<JudgePanel run={run({ status: "completed" })} />);

    expect(await screen.findByText(/<img src=x onerror=alert\(1\)> \*\*not bold\*\*/)).toBeTruthy();
    // The markup did not become a real element.
    expect(container.querySelector("img")).toBeNull();
  });

  it("offers Run judge for an unjudged run and enqueues a re-run", async () => {
    mockApi.getRunReview.mockResolvedValue({ review: null });
    mockApi.rerunJudge.mockResolvedValue({ run: run({ id: "judge1", kind: "judge", status: "queued" }) });
    render(<JudgePanel run={run({ status: "failed" })} />);

    const btn = await screen.findByText("Run judge");
    expect(screen.getByText(/hasn't been judged yet/i)).toBeTruthy();
    fireEvent.click(btn);
    expect(mockApi.rerunJudge).toHaveBeenCalledWith("r1");
    expect(await screen.findByText(/re-queued/i)).toBeTruthy();
  });

  it("surfaces a re-run error", async () => {
    const { ApiError } = await import("../lib/api");
    mockApi.getRunReview.mockResolvedValue({ review: review() });
    mockApi.rerunJudge.mockRejectedValue(new ApiError(409, "a judge run is already in progress for this run"));
    render(<JudgePanel run={run({ status: "completed" })} />);

    fireEvent.click(await screen.findByText("Re-run judge"));
    expect(await screen.findByText(/already in progress/i)).toBeTruthy();
  });

  it("renders nothing for an ineligible kind (chat) and never fetches a review", async () => {
    const { container } = render(<JudgePanel run={run({ kind: "chat", status: "completed" })} />);
    expect(container.textContent).toBe("");
    expect(mockApi.getRunReview).not.toHaveBeenCalled();
  });

  // Regression for the coordinate-key mismatch (web-ux blocking): a recommendation with a
  // PERSISTED filed link (from ReviewDTO.filed_issues on reload) must render the filed ROW
  // with the issue link, NOT the idle "File issue" button. Same-session smoke missed this
  // because the just-filed LOCAL state masked it; only a persisted link exercises coordKey.
  it("renders a persisted filed link as the filed row, not the idle button", async () => {
    mockApi.getRunReview.mockResolvedValue({
      review: review({
        filed_issues: [
          {
            category: "install_worker_tool",
            target: "shellcheck",
            issue_iid: 71,
            issue_url: "https://gitlab.example/vtmocanu/uzi/-/issues/71",
            filed_at: "2026-01-02T00:00:00Z",
          },
        ],
      }),
    });
    render(<JudgePanel run={run({ status: "completed" })} />);

    const link = await screen.findByRole("link", { name: /#71/ });
    expect(link.getAttribute("href")).toBe("https://gitlab.example/vtmocanu/uzi/-/issues/71");
    // The idle affordance for this (now-filed) recommendation is gone.
    expect(screen.queryByText("File issue")).toBeNull();
  });

  // ── File-issue draft flow (PRD #68 M4 states A–E) ──────────────────────────────────────
  function repoOpt(id: string, path: string): Repo {
    return {
      id,
      connection_id: "c1",
      forge_project_id: 1,
      path_with_namespace: path,
      web_url: `https://gitlab.example/${path}`,
      default_branch: "main",
      enabled: true,
      repo_skills_enabled: false,
      repo_devbox_opt_in: false,
      pipeline: null,
    };
  }
  function draftFixture(over: Partial<IssueDraft> = {}): IssueDraft {
    return {
      default_repo_id: "repo1",
      title: "Improve the reviewer: reviewer",
      description: "## What the judge found\n\n````\nrationale\n````",
      labels: ["PRD", "PRDLESS"],
      provenance: "from vlad's worker, run 8f2c1d04",
      default_note: "Defaulted to the judged run's repo.",
      ...over,
    };
  }

  it("opens the draft with templated fields on File issue click (state B)", async () => {
    mockApi.getRunReview.mockResolvedValue({ review: review() });
    mockApi.listRepos.mockResolvedValue({ repos: [repoOpt("repo1", "vtmocanu/uzi")] });
    mockApi.getIssueDraft.mockResolvedValue({ draft: draftFixture() });
    render(<JudgePanel run={run({ status: "completed" })} />);

    fireEvent.click(await screen.findByText("File issue"));
    expect(mockApi.getIssueDraft).toHaveBeenCalledWith("r1", "rc1");
    expect(await screen.findByText("Draft issue")).toBeTruthy();
    // Provenance is prominent (Decision 8), labels are the server-assembled pair, and the
    // title is an editable field seeded from the draft.
    expect(screen.getByText(/from vlad's worker, run 8f2c1d04/)).toBeTruthy();
    expect(screen.getByText("PRD")).toBeTruthy();
    expect(screen.getByText("PRDLESS")).toBeTruthy();
    expect((screen.getByDisplayValue("Improve the reviewer: reviewer") as HTMLInputElement).tagName).toBe("INPUT");
  });

  it("files the issue and shows the created link (state C)", async () => {
    mockApi.getRunReview.mockResolvedValue({ review: review() });
    mockApi.listRepos.mockResolvedValue({ repos: [repoOpt("repo1", "vtmocanu/uzi")] });
    mockApi.getIssueDraft.mockResolvedValue({ draft: draftFixture() });
    mockApi.fileIssue.mockResolvedValue({
      issue: { iid: 71, web_url: "https://gitlab.example/vtmocanu/uzi/-/issues/71", title: "t" },
    });
    render(<JudgePanel run={run({ status: "completed" })} />);

    fireEvent.click(await screen.findByText("File issue"));
    fireEvent.click(await screen.findByText("Create issue"));
    expect(mockApi.fileIssue).toHaveBeenCalledWith("r1", "rc1", {
      repo_id: "repo1",
      title: "Improve the reviewer: reviewer",
      description: draftFixture().description,
    });
    const link = await screen.findByRole("link", { name: /#71/ });
    expect(link.getAttribute("href")).toBe("https://gitlab.example/vtmocanu/uzi/-/issues/71");
  });

  it("disables Create until a repo is picked when no default resolves (state D)", async () => {
    mockApi.getRunReview.mockResolvedValue({ review: review() });
    mockApi.listRepos.mockResolvedValue({ repos: [repoOpt("repo1", "vtmocanu/uzi")] });
    mockApi.getIssueDraft.mockResolvedValue({
      draft: draftFixture({ default_repo_id: "", default_note: "No uzi repo is configured." }),
    });
    render(<JudgePanel run={run({ status: "completed" })} />);

    fireEvent.click(await screen.findByText("File issue"));
    const create = (await screen.findByText("Create issue")) as HTMLButtonElement;
    expect(create.disabled).toBe(true);
    expect(screen.getByText("No uzi repo is configured.")).toBeTruthy();
    // Picking a repo enables Create.
    fireEvent.change(screen.getByRole("combobox"), { target: { value: "repo1" } });
    expect((screen.getByText("Create issue") as HTMLButtonElement).disabled).toBe(false);
  });

  it("keeps the draft open and shows the error when the forge rejects (state E)", async () => {
    const { ApiError } = await import("../lib/api");
    mockApi.getRunReview.mockResolvedValue({ review: review() });
    mockApi.listRepos.mockResolvedValue({ repos: [repoOpt("repo1", "vtmocanu/uzi")] });
    mockApi.getIssueDraft.mockResolvedValue({ draft: draftFixture() });
    mockApi.fileIssue.mockRejectedValue(new ApiError(502, "the forge rejected the request (403)"));
    render(<JudgePanel run={run({ status: "completed" })} />);

    fireEvent.click(await screen.findByText("File issue"));
    fireEvent.click(await screen.findByText("Create issue"));
    expect(await screen.findByText(/forge rejected the request/i)).toBeTruthy();
    // The draft stays open with its fields intact (not collapsed to the filed row).
    expect(screen.getByDisplayValue("Improve the reviewer: reviewer")).toBeTruthy();
  });

  it("flags a stale filed link (filed before the current review revision)", async () => {
    mockApi.getRunReview.mockResolvedValue({
      review: review({
        updated_at: "2026-02-01T00:00:00Z",
        filed_issues: [
          {
            category: "install_worker_tool",
            target: "shellcheck",
            issue_iid: 71,
            issue_url: "https://gitlab.example/vtmocanu/uzi/-/issues/71",
            filed_at: "2026-01-01T00:00:00Z", // older than updated_at → stale
          },
        ],
      }),
    });
    render(<JudgePanel run={run({ status: "completed" })} />);
    expect(await screen.findByText(/filed for an earlier version/i)).toBeTruthy();
  });

  it("recovers from a draft-load failure with Retry and Cancel (no dead end)", async () => {
    const { ApiError } = await import("../lib/api");
    mockApi.getRunReview.mockResolvedValue({ review: review() });
    mockApi.listRepos.mockResolvedValue({ repos: [] });
    mockApi.getIssueDraft.mockRejectedValue(new ApiError(500, "boom"));
    render(<JudgePanel run={run({ status: "completed" })} />);

    fireEvent.click(await screen.findByText("File issue"));
    expect(await screen.findByText("Retry")).toBeTruthy();
    // Cancel dismisses the failed card, restoring the File-issue button.
    fireEvent.click(screen.getByText("Cancel"));
    expect(await screen.findByText("File issue")).toBeTruthy();
  });
});
