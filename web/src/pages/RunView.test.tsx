// @vitest-environment jsdom
import { afterEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import {
  PlanPanel,
  AgentRosterSummary,
  JudgePanel,
  RunHeading,
  RunCompletedLine,
  RunFailureReason,
  HealthFlag,
  derivePlanRevision,
} from "./RunView";
// ?raw rather than node:fs — the web tsconfig has no node types, and this repo
// already makes the same choice for the same reason in WorkerUpgradeBadge.test.tsx
// and workerSizes.test.ts. Vite inlines it at build time, so the assertion runs
// against the real file under both tsc and vitest.
import runViewSource from "./RunView.tsx?raw";
import questionPanelSource from "../components/QuestionPanel.tsx?raw";
import { api, type IssueDraft, type Repo, type RepoAgent, type Run, type RunMessage, type RunReview } from "../lib/api";

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
      // PRD #94 M3: triage mutations. Defaulted to resolve so a click + refetch settles.
      setDisposition: vi.fn().mockResolvedValue(null),
      deleteDisposition: vi.fn().mockResolvedValue(null),
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
    anthropic_secret_id: null,
    anthropic_secret_label: null,
    anthropic_select_reason: null,
    anthropic_headroom_pct: null,
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

// Issue #124 / item 7. The run page's own heading renders the forge issue title, and it
// was the one render site in this batch that no test reached — dropping its strip left the
// whole file green, which is why RunHeading is now extracted the way the panels beside it
// already are.
// Issue #124, the TEXT channel for the two fields 96ed275a fixed on the ATTRIBUTE channel.
// Both are asserted SEPARATELY and each is mutated on its own, because they render in
// DIFFERENT branches of RunView and no single fixture reaches both — a case planting a Cf in
// `failure_reason` passes with `health_reason` still raw, which is exactly the shape that let
// three earlier defects through. Each fixture gives its field a real non-empty value rather
// than relying on a default, since `RunView.test.tsx` setting both to `null` is why nothing
// caught these.
describe("RunFailureReason — the worker-supplied failure reason (#124, text channel)", () => {
  it("strips bidi/zero-width characters out of the rendered reason", () => {
    const { container } = render(
      <RunFailureReason run={run({ failure_reason: "the repo \u202Eapproved\u200B this" })} />,
    );
    // The rendered TEXT is the assertion — a `title` check here would be measuring what
    // 96ed275a already fixed, on a surface that carries no title at all.
    //
    // The POSITIVE assertion below is load-bearing, not belt-and-braces. Both renders sit
    // behind a truthiness guard, so a fixture that regressed to `null` would mount nothing
    // and leave `not.toMatch` matching an empty string — green, and proving nothing. Same
    // shape as this batch's first finding: `!fs.existsSync(...)` passing because the thing
    // it tested for was absent. Measured: nulling this fixture reds with
    // `expected '' to be 'the repo approved this'`.
    expect(container.textContent ?? "").not.toMatch(/[\p{Cf}]/u);
    expect(container.textContent).toBe("the repo approved this");
  });

  it("renders nothing when there is no reason", () => {
    const { container } = render(<RunFailureReason run={run({ failure_reason: null })} />);
    expect(container.textContent).toBe("");
  });
});

describe("HealthFlag — the health reason (#124, text channel)", () => {
  it("strips bidi/zero-width characters out of the rendered reason", () => {
    const { container } = render(
      <HealthFlag run={run({ status: "running", health: "stalled", health_reason: "no output for \u202E20m\u200B" })} />,
    );
    expect(container.textContent ?? "").not.toMatch(/[\p{Cf}]/u);
    // Sharper here than in the sibling case: the pill DOES mount without a reason (it
    // renders `⚠ stalled`), so `not.toMatch` passes on a null fixture and only this line
    // catches it. Measured: `expected '⚠ stalled' to contain 'no output for 20m'`.
    expect(container.textContent).toContain("no output for 20m");
  });
});

// Issue #124: `run.branch` is WORKER-supplied and ingest stores it as
// `stripNULParam(req.Branch)` — NUL only, no Cc/Cf. Extracted to be assertable at all;
// before that, dropping the strip left the whole file green.
describe("RunCompletedLine — the worker-supplied branch carries no format characters (#124)", () => {
  it("strips bidi/zero-width characters out of run.branch", () => {
    const { container } = render(
      <RunCompletedLine run={run({ branch: "agent/issue-\u202E7\u200B", mr_iid: null })} duration="2m" />,
    );
    // Anchored on the duration, which the mutation cannot move.
    expect(container.textContent).toContain("Ran for 2m");
    expect(container.textContent ?? "").not.toMatch(/[\p{Cf}]/u);
    expect(screen.getByText("agent/issue-7")).toBeTruthy();
  });
});

describe("RunHeading — the forge issue title carries no format characters (#124)", () => {
  it("strips bidi/zero-width characters, and keeps the iid beside them", () => {
    const { container } = render(
      <RunHeading run={run({ issue_title: "Fix the \u202Eparser\u200B bug", issue_iid: 7 })} />,
    );
    expect(container.textContent ?? "").not.toMatch(/[\p{Cf}]/u);
    expect(screen.getByText("Fix the parser bug")).toBeTruthy();
    expect(screen.getByText("#7")).toBeTruthy();
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
      // PRD #94: default to an untriaged review (every rec reads "To do"). The triage
      // bar reads these server counts directly — the panel never re-derives them.
      dispositions: [],
      triage: { total: 1, todo: 1, filed: 0, done: 0, dismissed: 0, false_positives: 0 },
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

  // Issue #124. React escaping (pinned above) does not touch Unicode format characters,
  // and the api's review-ingest scrub dropped Cc only — Cc and Cf are disjoint — so a bidi
  // override reached the browser intact and the browser's bidi algorithm honours it. Ingest
  // strips Cf now; rows stored before that still arrive carrying it. The rendered text is
  // what a human READS, so that is what gets asserted.
  it("strips bidi/zero-width characters out of judge free text before rendering (#124)", async () => {
    const RLO = "\u202E"; // RIGHT-TO-LEFT OVERRIDE — reorders the line that follows it
    const ZWSP = "\u200B"; // zero-width space — also defeats search over the rendered text
    // EVERY judge-derived free-text field in this fixture carries a hostile character, and
    // that is the point rather than thoroughness. The whole-subtree assertion below can only
    // see a field the FIXTURE makes hostile — so while `judge_model` sat here as the clean
    // literal "haiku", the case passed with that field unstripped and the comment's promise
    // ("a new judge field added to this panel without the strip fails here") was false. It
    // survived four #124 commits in that blind spot. A field added to the DTO must be added
    // here too, or this case silently stops covering it.
    mockApi.getRunReview.mockResolvedValue({
      review: review({
        summary_md: `The review ${RLO}approved this change`,
        judge_model: `hai${RLO}ku`,
        recommendations: [
          {
            id: "rc1",
            category: "install_worker_tool",
            target: `shell${ZWSP}check`,
            rationale_md: `hit ${RLO}command not found`,
            confidence: "high",
            created_at: "2026-01-01T00:00:00Z",
          },
        ],
      }),
    });
    const { container } = render(<JudgePanel run={run({ status: "completed" })} />);
    // The await must resolve whether or not the strip works. Awaiting the CLEANED text
    // here would make a mutation red at a 5s findByText timeout instead of at the
    // assertion below — a red that looks like proof and measures something else. The
    // verdict chip is a closed enum, so it is on screen in both states.
    await screen.findByText("Issues found");

    // Nothing in the panel's rendered text carries a character from either category,
    // asserted over the WHOLE subtree. The guarantee is exactly as wide as the fixture is
    // hostile — see the note above; it is not "any new field is covered automatically".
    // Asserted FIRST, so it is what a mutation reds on.
    const rendered = container.textContent ?? "";
    expect(rendered).not.toMatch(/[\p{Cf}]/u);
    expect(rendered).toContain("The review approved this change");
    expect(rendered).toContain("hit command not found");
    // The target renders as the searchable string, not one with an invisible seam in it.
    expect(screen.getByText("shellcheck")).toBeTruthy();
    expect(rendered).toContain("via haiku");
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

  // Issue #124 / LOW-1. The draft is templated server-side from `rec.target` +
  // `rationale_md`, so a review stored BEFORE the ingest strip can still seed a bidi
  // override into it — and this body is written to the user's forge. The strip belongs at
  // the SEED, not on `value=`: these are controlled components, so the state IS the POST
  // body, and filtering the value would silently rewrite what the user typed.
  it("strips format characters from the SEEDED draft, before the user can edit it (#124)", async () => {
    mockApi.getRunReview.mockResolvedValue({ review: review() });
    mockApi.listRepos.mockResolvedValue({ repos: [repoOpt("repo1", "vtmocanu/uzi")] });
    mockApi.getIssueDraft.mockResolvedValue({
      draft: draftFixture({
        title: "Improve the \u202Ereviewer",
        description: "## What the judge found\n\n\u200Bmalicious\u202E line",
      }),
    });
    render(<JudgePanel run={run({ status: "completed" })} />);
    fireEvent.click(await screen.findByText("File issue"));
    await screen.findByText("Draft issue");

    // Read the CONTROL VALUES, not the rendered text: these are what a Create click posts.
    const title = screen.getByDisplayValue(/Improve the/) as HTMLInputElement;
    const body = screen.getByDisplayValue(/What the judge found/) as HTMLTextAreaElement;
    expect(title.value).not.toMatch(/[\p{Cf}]/u);
    expect(body.value).not.toMatch(/[\p{Cf}]/u);
    expect(title.value).toBe("Improve the reviewer");
    // …and the markdown structure is intact: the strip spares \n, so the fenced template
    // the server built is still a template and not one run-on line.
    expect(body.value).toContain("## What the judge found");
    expect(body.value).toContain("\n");
  });

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

  // ── Triage: chips, controls, stale flag, triage bar, collapse (PRD #94 M3) ────────────
  function rec(
    id: string,
    category: string,
    target: string,
    rationale = `rationale ${id}`,
  ): RunReview["recommendations"][number] {
    return {
      id,
      category: category as RunReview["recommendations"][number]["category"],
      target,
      rationale_md: rationale,
      confidence: "",
      created_at: "2026-01-01T00:00:00Z",
    };
  }

  it("renders a status chip per row by the disposition→filed→to-do ladder", async () => {
    mockApi.getRunReview.mockResolvedValue({
      review: review({
        recommendations: [
          rec("rc1", "install_worker_tool", "shellcheck"),
          rec("rc2", "improve_agent", "reviewer"),
          rec("rc3", "improve_uzi", "poller"),
          rec("rc4", "add_agent", "deploy-agent"),
          rec("rc5", "improve_uzi", "timeout"),
        ],
        filed_issues: [
          {
            category: "add_agent",
            target: "deploy-agent",
            issue_iid: 72,
            issue_url: "https://gitlab.example/x/-/issues/72",
            filed_at: "2026-01-01T00:00:00Z",
          },
        ],
        dispositions: [
          { category: "install_worker_tool", target: "shellcheck", status: "done", reason: "", set_at: "2026-01-01T00:00:00Z", stale: false },
          { category: "improve_agent", target: "reviewer", status: "dismissed", reason: "wont_do", set_at: "2026-01-01T00:00:00Z", stale: false },
          { category: "improve_uzi", target: "timeout", status: "dismissed", reason: "not_an_issue", set_at: "2026-01-01T00:00:00Z", stale: false },
        ],
        triage: { total: 5, todo: 1, filed: 1, done: 1, dismissed: 2, false_positives: 1 },
      }),
    });
    render(<JudgePanel run={run({ status: "completed" })} />);

    // The ✓ is a decorative aria-hidden glyph, so the chip's own text is just "Done".
    expect(await screen.findByText("Done")).toBeTruthy(); // done > filed
    expect(screen.getByText("Dismissed · Won't do")).toBeTruthy();
    expect(screen.getByText("Dismissed · Not an issue")).toBeTruthy();
    expect(screen.getByText("To do")).toBeTruthy(); // no disposition, not filed
    expect(screen.getByText("Filed")).toBeTruthy(); // settled link, no disposition
  });

  it("Mark done sets a done disposition (no reason) and refetches the review", async () => {
    mockApi.getRunReview.mockResolvedValue({ review: review() });
    render(<JudgePanel run={run({ status: "completed" })} />);

    fireEvent.click(await screen.findByText("Mark done"));
    await waitFor(() => expect(mockApi.setDisposition).toHaveBeenCalledWith("r1", "rc1", "done"));
    // Refetch: getRunReview ran on mount AND again after the mutation.
    await waitFor(() => expect(mockApi.getRunReview.mock.calls.length).toBeGreaterThanOrEqual(2));
  });

  it("Dismiss → Not an issue sets a not_an_issue dismissal", async () => {
    mockApi.getRunReview.mockResolvedValue({ review: review() });
    render(<JudgePanel run={run({ status: "completed" })} />);

    fireEvent.click(await screen.findByText("Dismiss ▾"));
    fireEvent.click(await screen.findByText("Not an issue"));
    await waitFor(() =>
      expect(mockApi.setDisposition).toHaveBeenCalledWith("r1", "rc1", "dismissed", "not_an_issue"),
    );
  });

  it("Undo clears the disposition via deleteDisposition and refetches", async () => {
    mockApi.getRunReview.mockResolvedValue({
      review: review({
        dispositions: [
          { category: "install_worker_tool", target: "shellcheck", status: "done", reason: "", set_at: "2026-01-01T00:00:00Z", stale: false },
        ],
        triage: { total: 1, todo: 0, filed: 0, done: 1, dismissed: 0, false_positives: 0 },
      }),
    });
    render(<JudgePanel run={run({ status: "completed" })} />);

    fireEvent.click(await screen.findByText("Undo"));
    await waitFor(() => expect(mockApi.deleteDisposition).toHaveBeenCalledWith("r1", "rc1"));
    await waitFor(() => expect(mockApi.getRunReview.mock.calls.length).toBeGreaterThanOrEqual(2));
  });

  it("renders the stale note straight from the DTO's stale flag", async () => {
    mockApi.getRunReview.mockResolvedValue({
      review: review({
        dispositions: [
          { category: "install_worker_tool", target: "shellcheck", status: "done", reason: "", set_at: "2026-01-01T00:00:00Z", stale: true },
        ],
        triage: { total: 1, todo: 0, filed: 0, done: 1, dismissed: 0, false_positives: 0 },
      }),
    });
    render(<JudgePanel run={run({ status: "completed" })} />);
    expect(await screen.findByText(/recommendation changed since you resolved/i)).toBeTruthy();
  });

  it("renders the triage bar from server counts, never re-derived from the on-screen rows", async () => {
    // ONE recommendation is rendered, but the server triage totals 5 — so the bar's
    // numbers can only come from review.triage, proving no TS re-derivation.
    mockApi.getRunReview.mockResolvedValue({
      review: review({ triage: { total: 5, todo: 2, filed: 1, done: 1, dismissed: 1, false_positives: 1 } }),
    });
    render(<JudgePanel run={run({ status: "completed" })} />);

    expect(await screen.findByText("3 of 5 handled")).toBeTruthy(); // filed+done+dismissed of total
    expect(screen.getByText("2")).toBeTruthy(); // the server todo count, though 1 row shows
    expect(screen.getByText(/1 of 1 dismissed was a false positive/)).toBeTruthy();
  });

  it("collapse-dismissed toggle hides the dismissed rows", async () => {
    mockApi.getRunReview.mockResolvedValue({
      review: review({
        recommendations: [
          rec("rc1", "install_worker_tool", "shellcheck", "keep me"),
          rec("rc2", "improve_agent", "reviewer", "hide me"),
        ],
        dispositions: [
          { category: "improve_agent", target: "reviewer", status: "dismissed", reason: "wont_do", set_at: "2026-01-01T00:00:00Z", stale: false },
        ],
        triage: { total: 2, todo: 1, filed: 0, done: 0, dismissed: 1, false_positives: 0 },
      }),
    });
    render(<JudgePanel run={run({ status: "completed" })} />);

    expect(await screen.findByText("hide me")).toBeTruthy();
    expect(screen.getByText("keep me")).toBeTruthy();
    fireEvent.click(screen.getByText(/Hide dismissed \(1\)/));
    await waitFor(() => expect(screen.queryByText("hide me")).toBeNull());
    expect(screen.getByText("keep me")).toBeTruthy();
  });

  // A rec filed THEN marked done keeps both facts: the "Done" chip AND the clickable
  // issue link, but NOT the create-issue affordance (you can't file a second issue).
  it("a filed-then-done row keeps the issue link but drops the File-issue action", async () => {
    mockApi.getRunReview.mockResolvedValue({
      review: review({
        recommendations: [rec("rc1", "add_agent", "deploy-agent")],
        filed_issues: [
          {
            category: "add_agent",
            target: "deploy-agent",
            issue_iid: 72,
            issue_url: "https://gitlab.example/x/-/issues/72",
            filed_at: "2026-01-01T00:00:00Z",
          },
        ],
        dispositions: [
          { category: "add_agent", target: "deploy-agent", status: "done", reason: "", set_at: "2026-01-01T00:00:00Z", stale: false },
        ],
        triage: { total: 1, todo: 0, filed: 0, done: 1, dismissed: 0, false_positives: 0 },
      }),
    });
    render(<JudgePanel run={run({ status: "completed" })} />);

    // Done wins the chip ladder…
    expect(await screen.findByText("Done")).toBeTruthy();
    // …but the filed issue link survives the disposition (Resolved Q: file then done).
    const link = screen.getByRole("link", { name: /#72/ });
    expect(link.getAttribute("href")).toBe("https://gitlab.example/x/-/issues/72");
    // No way to file a second issue on a disposed row.
    expect(screen.queryByText("File issue")).toBeNull();
  });

  it("Escape closes the Dismiss menu and returns focus to the trigger (a11y)", async () => {
    mockApi.getRunReview.mockResolvedValue({ review: review() });
    render(<JudgePanel run={run({ status: "completed" })} />);

    const trigger = await screen.findByRole("button", { name: /Dismiss/ });
    fireEvent.click(trigger);
    expect(await screen.findByText("Won't do")).toBeTruthy();

    fireEvent.keyDown(document, { key: "Escape" });
    await waitFor(() => expect(screen.queryByText("Won't do")).toBeNull());
    expect(document.activeElement).toBe(trigger);
  });

  it("moves focus to the Undo control after a mutation, not to <body> (a11y)", async () => {
    // Mount reads untriaged; the post-mutation refetch reads the row as done, so the row
    // swaps to the Undo branch and focus must land there.
    mockApi.getRunReview
      .mockResolvedValueOnce({ review: review() })
      .mockResolvedValue({
        review: review({
          dispositions: [
            { category: "install_worker_tool", target: "shellcheck", status: "done", reason: "", set_at: "2026-01-01T00:00:00Z", stale: false },
          ],
          triage: { total: 1, todo: 0, filed: 0, done: 1, dismissed: 0, false_positives: 0 },
        }),
      });
    render(<JudgePanel run={run({ status: "completed" })} />);

    fireEvent.click(await screen.findByText("Mark done"));
    const undo = await screen.findByText("Undo");
    await waitFor(() => expect(document.activeElement).toBe(undo));
  });

  it("announces the mutation result via the polite live region (a11y)", async () => {
    mockApi.getRunReview.mockResolvedValue({ review: review() });
    render(<JudgePanel run={run({ status: "completed" })} />);

    fireEvent.click(await screen.findByText("Mark done"));
    expect(await screen.findByText("Marked done")).toBeTruthy();
  });
});

// ── Plan revision at the gate (PRD #41) ────────────────────────────────────────
function planMsg(seq: number, kind: string, payload: unknown): RunMessage {
  return { seq, kind, agent: "lead", agent_instance: null, agent_label: null, payload, created_at: "2026-07-04T00:00:00.000Z" };
}

describe("derivePlanRevision (PRD #41)", () => {
  it("counts versions from `plan` and rounds from `plan_feedback`; re-gated after a newer plan", () => {
    const rev = derivePlanRevision([
      planMsg(1, "plan", { plan_md: "v1" }),
      planMsg(2, "plan_feedback", { feedback: "drop step 1" }),
      planMsg(3, "plan_revising", { round: 1 }),
      planMsg(4, "plan", { plan_md: "v2" }),
    ]);
    expect(rev.versions).toBe(2);
    expect(rev.rounds).toBe(1);
    expect(rev.revising).toBe(false); // latest of {plan, plan_revising} by seq is the v2 plan
    expect(rev.latestFeedback).toBe("drop step 1");
    expect(rev.priorPlans).toEqual(["v1"]);
  });

  it("is `revising` when the latest of {plan, plan_revising} by seq is plan_revising", () => {
    const rev = derivePlanRevision([
      planMsg(1, "plan", { plan_md: "v1" }),
      planMsg(2, "plan_feedback", { feedback: "please rework" }),
      planMsg(3, "plan_revising", { round: 1 }),
    ]);
    expect(rev.revising).toBe(true);
    expect(rev.versions).toBe(1);
    expect(rev.rounds).toBe(1);
    expect(rev.latestFeedback).toBe("please rework");
  });

  it("uses the latest feedback bubble by seq", () => {
    const rev = derivePlanRevision([
      planMsg(1, "plan", { plan_md: "v1" }),
      planMsg(2, "plan_feedback", { feedback: "first" }),
      planMsg(3, "plan", { plan_md: "v2" }),
      planMsg(4, "plan_feedback", { feedback: "second" }),
    ]);
    expect(rev.latestFeedback).toBe("second");
    expect(rev.rounds).toBe(2);
  });
});

describe("PlanPanel — three-action gate + revision (PRD #41)", () => {
  const baseMessages = [planMsg(1, "plan", { plan_md: "# Plan\n- step one" })];

  function renderGate(
    over: {
      run?: Partial<Run>;
      messages?: RunMessage[];
      onRequestChanges?: (f: string) => void;
      onCancel?: () => void;
    } = {},
  ) {
    const onRequestChanges = over.onRequestChanges ?? vi.fn();
    const onCancel = over.onCancel ?? vi.fn();
    const utils = render(
      <PlanPanel
        run={run(over.run ?? { repo_agents: [], own_agents: [] })}
        messages={over.messages ?? baseMessages}
        busy={false}
        onApprove={() => {}}
        onReject={() => {}}
        onRequestChanges={onRequestChanges}
        onCancel={onCancel}
      />,
    );
    return { ...utils, onRequestChanges, onCancel };
  }

  it("shows Approve / Request changes / Reject with a v1 chip and 'revision 0 of 3'", () => {
    const { getByRole, container } = renderGate();
    expect(getByRole("button", { name: /approve plan/i })).toBeTruthy();
    expect(getByRole("button", { name: /request changes/i })).toBeTruthy();
    expect(getByRole("button", { name: /^reject$/i })).toBeTruthy();
    expect(container.textContent).toContain("v1");
    expect(container.textContent).toContain("revision 0 of 3");
  });

  it("Request changes reveals the composer and hides the header gate actions", () => {
    const { getByRole, queryByRole } = renderGate();
    fireEvent.click(getByRole("button", { name: /request changes/i }));
    expect(getByRole("button", { name: /send & revise/i })).toBeTruthy();
    expect(queryByRole("button", { name: /approve plan/i })).toBeNull();
    expect(queryByRole("button", { name: /^reject$/i })).toBeNull();
  });

  it("Cancel restores the header actions and closes the composer", () => {
    const { getByRole, queryByRole } = renderGate();
    fireEvent.click(getByRole("button", { name: /request changes/i }));
    fireEvent.click(getByRole("button", { name: /^cancel$/i }));
    expect(getByRole("button", { name: /approve plan/i })).toBeTruthy();
    expect(queryByRole("button", { name: /send & revise/i })).toBeNull();
  });

  it("Send & revise calls onRequestChanges with the typed feedback", () => {
    const onRequestChanges = vi.fn();
    const { getByRole, container } = renderGate({ onRequestChanges });
    fireEvent.click(getByRole("button", { name: /request changes/i }));
    const textarea = container.querySelector("textarea")!;
    fireEvent.change(textarea, { target: { value: "build it client-side" } });
    fireEvent.click(getByRole("button", { name: /send & revise/i }));
    expect(onRequestChanges).toHaveBeenCalledWith("build it client-side");
  });

  it("Send & revise is disabled until the feedback is non-empty", () => {
    const { getByRole } = renderGate();
    fireEvent.click(getByRole("button", { name: /request changes/i }));
    expect((getByRole("button", { name: /send & revise/i }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("renders the revising parked state when latest-by-seq is plan_revising", () => {
    const messages = [
      planMsg(1, "plan", { plan_md: "v1 body" }),
      planMsg(2, "plan_feedback", { feedback: "rework the endpoint" }),
      planMsg(3, "plan_revising", { round: 1 }),
    ];
    const onCancel = vi.fn();
    const { container, getByRole, queryByRole } = renderGate({ messages, onCancel });
    expect(container.textContent).toContain("Revising the plan");
    expect(container.textContent).toContain("rework the endpoint");
    expect(container.textContent).toContain("revision 1 of 3");
    // Parked: the approve gate is gone, replaced by a Cancel-run affordance.
    expect(queryByRole("button", { name: /approve plan/i })).toBeNull();
    fireEvent.click(getByRole("button", { name: /cancel run/i }));
    expect(onCancel).toHaveBeenCalled();
  });

  it("flips back to an open v2 gate with a history accordion when a newer plan arrives", () => {
    const messages = [
      planMsg(1, "plan", { plan_md: "v1 body" }),
      planMsg(2, "plan_feedback", { feedback: "rework the endpoint" }),
      planMsg(3, "plan_revising", { round: 1 }),
      planMsg(4, "plan", { plan_md: "v2 body" }),
    ];
    const { container, getByRole } = renderGate({
      run: { repo_agents: [], own_agents: [], plan_md: "# Plan v2\n- new step" },
      messages,
    });
    expect(getByRole("button", { name: /approve plan/i })).toBeTruthy();
    expect(container.textContent).toContain("Updated plan awaiting your approval");
    expect(container.textContent).toContain("v2");
    expect(container.textContent).toContain("revision 1 of 3");
    // The superseded v1 is preserved in the collapsed history accordion.
    expect(container.textContent).toContain("superseded");
    expect(container.textContent).toContain("v1 body");
  });
});

// PRD #111 M1: the credential chip must be WIRED INTO the page, not merely exist.
//
// Measured before this test was written: deleting `<RunCredential run={run} />` from
// RunView left the entire web suite green — 94 files, 1069 tests, exit 0. The
// component had its own unit tests and the page fixtures had gained two fields, but
// nothing anywhere asserted the two were connected, so M1's only user-visible web
// surface was effectively untested.
//
// A SOURCE assertion rather than a render one, and the limit is worth stating: the
// chip sits inside PageHeader's titleNode, deep in the page component, which this
// file does not render (it exports and tests the panels, not the page — rendering it
// needs a router, the auth context and a live WS stream). So this proves the wiring
// EXISTS; RunCredential.test.tsx proves what it renders. Together they cover what a
// single page-level render would, and this half reddens on exactly the mutation that
// went undetected.
describe("RunView ↔ RunCredential wiring (PRD #111 M1)", () => {
  // 🔴 A SOURCE-TEXT CONTROL IS SATISFIED BY DISABLED CODE, and the first version of
  // this guard was. `toContain` over raw file text does not know what a comment is,
  // so `{/* <RunCredential run={run} /> */}` passed while the chip rendered nowhere.
  // Measured: deleting the JSX reddened, commenting it out left all 42 green.
  //
  // The generalisation is worth more than the fix, because this is the SECOND
  // presence-over-source control on this branch (with M3-D's backfill assertion):
  // a control that asserts something EXISTS in source must strip comments first, or
  // it proves only that the text is present, not that it runs. Note the direction
  // matters — an ABSENCE assertion (see rateLimits.ts's "derives nothing" guard) is
  // correct to ignore commented-out code, because disabled code is not a second
  // implementation. Presence and absence have opposite relationships to comments.
  // (This said THIRD while naming two; the commit that introduced it argued in its
  // own message that it is two and not three, so the artefact a reader finds here
  // carried the very miscount the commit was written to correct.)
  //
  // 🔴 STRIPPING COMMENTS CLOSES ONE MEMBER OF THE CLASS, NOT THE CLASS. The class is
  // DISABLED CODE, and a comment is only one way to disable something. Measured
  // against RunView.tsx: `{/* … */}`, `/* … */` and `// …` all redden now, but
  // `{false && <RunCredential run={run} />}` still PASSES — and it is not a comment,
  // so no amount of comment-stripping reaches it. The honest ceiling of any
  // source-text presence guard is "the text is present", never "it runs", which is
  // what the block comment above already says and what stays true after this fix.
  // Closing the rest needs a real render, which this file cannot do (the chip sits in
  // PageHeader's titleNode and the page needs a router, the auth context and a live
  // WS stream). ACKNOWLEDGED GAP, covered by the web-ux browser pass — recorded here
  // so the next reader does not see three comment forms handled and conclude the
  // class is closed.
  const live = runViewSource
    .replace(/\{\/\*[\s\S]*?\*\/\}/g, "") // JSX comments
    .replace(/\/\*[\s\S]*?\*\//g, "") // block comments
    .replace(/^\s*\/\/.*$/gm, ""); // line comments

  it("renders the credential chip in the run header", () => {
    expect(live).toContain("<RunCredential run={run} />");
  });

  it("imports the component it renders", () => {
    expect(live).toContain('from "../components/RunCredential"');
  });
});

describe("RunView ↔ QuestionPanel wiring (PRD #88 M2)", () => {
  // Same instrument and the SAME acknowledged ceiling as the block above: this proves
  // the text is present and not commented out, never that it runs. `{false && …}` still
  // passes, and no comment-stripping reaches it. The panel's own behaviour — chips,
  // index alignment, the escaped sinks — is covered by QuestionPanel.test.tsx against a
  // real render; what cannot be reached from here is the PAGE, which needs a router, the
  // auth context and a live WS stream to mount. Recorded so the next reader does not
  // mistake four assertions for coverage of the journey.
  const live = runViewSource
    .replace(/\{\/\*[\s\S]*?\*\/\}/g, "")
    .replace(/\/\*[\s\S]*?\*\//g, "")
    .replace(/^\s*\/\/.*$/gm, "");

  it("gates the panel on BOTH the parked status and a derived open question", () => {
    // Either half alone is wrong in a real window: the status alone renders a composer
    // with nothing to answer while the question message is still replaying, and the
    // question alone keeps offering one after the run resumed (the `answer` message is
    // emitted BEFORE the `running` state report).
    expect(live).toContain('run.status === "awaiting_input" && openQuestion');
  });

  it("derives the question from the feed rather than reading a DTO field", () => {
    // PRD #88 D-L: no new column and no new DTO field. If this becomes `run.something`,
    // web, Slack and the CLI stop sharing one derivation rule.
    expect(live).toContain("deriveOpenQuestion(messages)");
    expect(live).toContain('from "../lib/runQuestion"');
  });

  it("submits the answer under the `answer` steering kind", () => {
    expect(live).toContain('submit("answer", body)');
  });

  it("announces the park from an ALWAYS-MOUNTED live region, outside the panel's gate", () => {
    // The structural property is the whole fix and it is not the obvious shape: a region
    // created in the same tick as its first message is typically silent, because
    // assistive tech announces CHANGES to a region that already existed. Board.tsx's S5
    // note records this and calls it "the worst kind of accessibility bug: the markup
    // looks right". role="status" on QuestionPanel — which mounts WITH the park — would
    // have been exactly that, and would have browser-tested as present.
    //
    // Same source-text instrument and same ceiling as above: it proves the text is there
    // and uncommented, not that it runs. What it CAN prove structurally is placement, by
    // where the region sits relative to the panel's conditional.
    const region = live.indexOf('role="status" aria-live="polite"');
    const panelGate = live.indexOf('run.status === "awaiting_input" && openQuestion');
    expect(region).toBeGreaterThan(-1);
    expect(panelGate).toBeGreaterThan(-1);
    expect(region).toBeLessThan(panelGate);
  });
});

describe("the park announcement does not live inside the panel (PRD #88 M2, a11y)", () => {
  // An ABSENCE assertion, and deliberately on the OTHER file. It is the durable half of
  // the pair above: someone tidying the announcement "closer to where it belongs" would
  // move it into QuestionPanel, where it mounts with its own content and goes silent.
  // Absence assertions are correct to ignore commented-out code (disabled code is not a
  // second implementation), so this needs no comment-stripping.
  it("QuestionPanel declares no live region of its own", () => {
    expect(questionPanelSource).not.toContain("aria-live");
    expect(questionPanelSource).not.toContain('role="status"');
  });
});
