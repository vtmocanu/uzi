// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import {
  act,
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import {
  PlanPanel,
  SeededPlanPanel,
  AgentRosterSummary,
  JudgePanel,
  JUDGE_POLL_MAX_TRIES,
  MilestoneBadge,
  MilestoneChecklist,
  RunHeading,
  RunCompletedLine,
  RunFailureReason,
  HealthFlag,
  LimitWaitPanel,
  RunView,
  RunSummary,
  derivePlanRevision,
} from "./RunView";
import { useRunStream } from "../lib/useRunStream";
// ?raw rather than node:fs — the web tsconfig has no node types, and this repo
// already makes the same choice for the same reason in WorkerUpgradeBadge.test.tsx
// and workerSizes.test.ts. Vite inlines it at build time, so the assertion runs
// against the real file under both tsc and vitest.
import runViewSource from "./RunView.tsx?raw";
import questionPanelSource from "../components/QuestionPanel.tsx?raw";
import {
  api,
  type IssueDraft,
  type Repo,
  type RepoAgent,
  type Run,
  type RunMessage,
  type RunReview,
  type Worker,
} from "../lib/api";

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
      // PRD #320 M6: the Expedite/undo mutation. Defaulted to resolve so a click +
      // refreshRun settles; the priority cases assert the call args.
      expediteRun: vi.fn().mockResolvedValue({ run: null }),
    },
  };
});
const mockApi = vi.mocked(api);

// issue #279: the report-only surfaces (hero chip + Findings panel) live inline in the
// RunView PAGE, not in an exported sub-component, so those cases render the whole page.
// useRunStream is the page's only data source; mock it so a test can hand it a run
// directly (the real hook opens a websocket). useRunStream is used ONLY by RunView, so
// this mock does not touch the sub-component tests above.
vi.mock("../lib/useRunStream", () => ({ useRunStream: vi.fn() }));
const mockUseRunStream = vi.mocked(useRunStream);

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
    issue_web_url: null,
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
    model: null,
    override_subagent_model: false,
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
    // PRD #122: a non-milestone run by default, so every existing case exercises the
    // null-fallback path (no checklist, no M badge, iteration badge unchanged).
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
    const approve = await screen.findByRole("button", {
      name: /Approve plan · 2 repo agents/,
    });
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
    const approve = screen.getByRole("button", {
      name: /Approve plan · 2 repo agents/,
    });
    fireEvent.click(approve);
    expect(onApprove).toHaveBeenCalledWith({
      source: "repo",
      exclusions: ["tester"],
    });
  });

  it("State B: no repo agents → own templates from run.own_agents (lead already stripped server-side)", async () => {
    const onApprove = vi.fn();
    render(
      <PlanPanel
        run={run({
          repo_agents: [],
          own_agents: [agent("coder"), agent("reviewer")],
        })}
        busy={false}
        onApprove={onApprove}
        onReject={() => {}}
      />,
    );
    // Own is default: label counts the 2 own agents the server delivered.
    await screen.findByRole("button", {
      name: /Approve plan · 2 of your templates/,
    });
    // The own chips are the two the run carries — the lead never appears (the server
    // strips it before own_agents), and there was no global fetch.
    expect(screen.getByRole("button", { name: /^●?\s*coder/i })).toBeTruthy();
    expect(
      screen.getByRole("button", { name: /^●?\s*reviewer/i }),
    ).toBeTruthy();
    expect(screen.queryByRole("button", { name: /^●?\s*lead/i })).toBeNull();
    fireEvent.click(
      screen.getByRole("button", {
        name: /Approve plan · 2 of your templates/,
      }),
    );
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
    const approve = await screen.findByRole("button", {
      name: /^Approve plan$/,
    });
    fireEvent.click(approve);
    expect(onApprove).toHaveBeenCalledWith({ source: "own", exclusions: [] });
  });

  it("the plan markdown still renders", async () => {
    render(
      <PlanPanel
        run={run({ repo_agents: [], own_agents: [] })}
        busy={false}
        onApprove={() => {}}
        onReject={() => {}}
      />,
    );
    expect(await screen.findByText("step one")).toBeTruthy();
  });
});

// PRD #84 M4 4d: the plan-gate readiness summary — the run's inferred requirements checked
// against its ASSIGNED worker's capabilities, plus the false-positive "run without it"
// override. Rendered inside the awaiting_approval PlanPanel, so these drive the panel
// directly with the workers prop (the whole page needs a live stream + a dozen mocks).
describe("PlanPanel readiness summary (PRD #84 M4 4d)", () => {
  // The readiness code reads only w.id + w.capabilities; a cast keeps the fixture terse
  // without filling the whole Worker shape (the test file already casts elsewhere).
  function worker(id: string, capabilities: string[]): Worker {
    return { id, capabilities } as unknown as Worker;
  }

  it("marks a required capability UNMET when the assigned worker lacks it, with remediation", async () => {
    render(
      <PlanPanel
        run={run({ worker_id: "w1", required_capabilities: ["docker"], repo_agents: [], own_agents: [] })}
        workers={[worker("w1", [])]}
        busy={false}
        onApprove={() => {}}
        onReject={() => {}}
      />,
    );
    expect(await screen.findByText("Run requirements")).toBeTruthy();
    // The unmet badge is identified by its title (the warn-toned "does not advertise" copy).
    expect(screen.getByTitle(/does not advertise "docker"/)).toBeTruthy();
    // The remediation line names the missing cap and the escape hatch.
    expect(screen.getByText(/Provision or start a worker with/i)).toBeTruthy();
  });

  it("marks a required capability MET (ok-toned) and offers NO override when the worker advertises it", async () => {
    render(
      <PlanPanel
        run={run({ worker_id: "w1", required_capabilities: ["docker"], repo_agents: [], own_agents: [] })}
        workers={[worker("w1", ["docker"])]}
        busy={false}
        onApprove={() => {}}
        onReject={() => {}}
      />,
    );
    expect(await screen.findByTitle(/advertises "docker"/)).toBeTruthy();
    // Nothing unmet → no remediation, no override button.
    expect(screen.queryByText(/Provision or start a worker with/i)).toBeNull();
    expect(screen.queryByRole("button", { name: /Run without/i })).toBeNull();
  });

  it("marks docker MET via the docker_enabled fold even when capabilities[] omits it", async () => {
    // A provision-time docker worker whose self-report has not landed: docker=true but
    // capabilities=[]. The server folds docker_enabled into effective caps, so the panel must
    // too — otherwise it shows a false UNMET for a plan the approve gate accepts (no 409).
    const w = { id: "w1", capabilities: [], docker: true } as unknown as Worker;
    render(
      <PlanPanel
        run={run({ worker_id: "w1", required_capabilities: ["docker"], repo_agents: [], own_agents: [] })}
        workers={[w]}
        busy={false}
        onApprove={() => {}}
        onReject={() => {}}
      />,
    );
    expect(await screen.findByTitle(/advertises "docker"/)).toBeTruthy();
    expect(screen.queryByTitle(/does not advertise "docker"/)).toBeNull();
    expect(screen.queryByRole("button", { name: /Run without/i })).toBeNull();
  });

  it("renders required_tools ('will be provisioned') and the size_class label", async () => {
    render(
      <PlanPanel
        run={run({
          worker_id: "w1",
          required_capabilities: [],
          required_tools: ["node", "go"],
          size_class: "m",
          repo_agents: [],
          own_agents: [],
        })}
        workers={[worker("w1", [])]}
        busy={false}
        onApprove={() => {}}
        onReject={() => {}}
      />,
    );
    expect(await screen.findByText("Run requirements")).toBeTruthy();
    expect(screen.getByText("node")).toBeTruthy();
    expect(screen.getByText("go")).toBeTruthy();
    expect(screen.getByText("will be provisioned")).toBeTruthy();
    expect(screen.getByText("size: m")).toBeTruthy();
    // Tools never block, so even with no capability check there is no override affordance.
    expect(screen.queryByRole("button", { name: /Run without/i })).toBeNull();
  });

  it("renders NO requirements block when nothing was inferred (all three empty)", async () => {
    render(
      <PlanPanel
        run={run({ required_capabilities: [], required_tools: [], size_class: "", repo_agents: [], own_agents: [] })}
        workers={[worker("w1", [])]}
        busy={false}
        onApprove={() => {}}
        onReject={() => {}}
      />,
    );
    // Positive anchor first so the absence check is not vacuous.
    expect(await screen.findByText("step one")).toBeTruthy();
    expect(screen.queryByText("Run requirements")).toBeNull();
  });

  // Review fix: size_class alone must NOT open the panel. detectToolchain ALWAYS emits a
  // non-empty size_class (s/m/l), so with the old `sizeClass !== ""` term the panel rendered
  // for EVERY plan gate. size is advisory-only: with no capability and no tool the panel — and
  // the "size:" label with it — is suppressed entirely.
  it("does NOT render the panel when only size_class is set (no capability, no tool)", async () => {
    render(
      <PlanPanel
        run={run({ required_capabilities: [], required_tools: [], size_class: "l", repo_agents: [], own_agents: [] })}
        workers={[worker("w1", [])]}
        busy={false}
        onApprove={() => {}}
        onReject={() => {}}
      />,
    );
    // Positive anchor first so the absence checks are not vacuous.
    expect(await screen.findByText("step one")).toBeTruthy();
    expect(screen.queryByText("Run requirements")).toBeNull();
    expect(screen.queryByText("size: l")).toBeNull();
  });

  it("the override button approves with overrideCapabilities=true (the false-positive correction)", async () => {
    const onApprove = vi.fn();
    render(
      <PlanPanel
        run={run({ worker_id: "w1", required_capabilities: ["docker"], repo_agents: [], own_agents: [] })}
        workers={[worker("w1", [])]}
        busy={false}
        onApprove={onApprove}
        onReject={() => {}}
      />,
    );
    const override = await screen.findByRole("button", { name: /Run without docker/i });
    fireEvent.click(override);
    // Default selection for a lead-only run (no repo/own agents) is source "own", no exclusions,
    // and the override flag rides as the second arg.
    expect(onApprove).toHaveBeenCalledWith({ source: "own", exclusions: [] }, true);
  });

  it("hides the override from a NON-OWNER (canSteer=false) even when a cap is unmet", async () => {
    render(
      <PlanPanel
        run={run({ worker_id: "w1", required_capabilities: ["docker"], repo_agents: [], own_agents: [] })}
        workers={[worker("w1", [])]}
        busy={false}
        canSteer={false}
        onApprove={() => {}}
        onReject={() => {}}
      />,
    );
    // The unmet state still renders (read-only), but the action does not.
    expect(await screen.findByTitle(/does not advertise "docker"/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: /Run without/i })).toBeNull();
  });
});

describe("PlanPanel — files changed during planning (PRD #212)", () => {
  it("renders the section and each porcelain line when the list is non-empty", async () => {
    render(
      <PlanPanel
        run={run({ plan_changed_files: [" M src/app.ts", "?? notes.md"], repo_agents: [], own_agents: [] })}
        busy={false}
        onApprove={() => {}}
        onReject={() => {}}
      />,
    );
    expect(await screen.findByText("Files changed during planning")).toBeTruthy();
    // Each porcelain line is rendered verbatim, INCLUDING its leading XY status prefix
    // (" M " = modified-in-worktree, "?? " = untracked) — the renderer must not parse or
    // strip the status code. Assert on textContent, not getByText's string matcher, whose
    // default normalizer TRIMS the leading space and so cannot see it (the space is
    // meaningful; M1 preserves it server-side too).
    expect(screen.getByText(/src\/app\.ts/).textContent).toBe(" M src/app.ts");
    expect(screen.getByText(/notes\.md/).textContent).toBe("?? notes.md");
  });

  it("renders a synthetic '… (+K more)' truncation marker verbatim (Decision 5)", async () => {
    // The server caps the list and appends a NON-porcelain marker line that has no XY
    // status code. A renderer that keyed on the status code would choke on it; this locks
    // that lines are printed verbatim, not parsed.
    render(
      <PlanPanel
        run={run({ plan_changed_files: [" M src/app.ts", "… (+42 more)"], repo_agents: [], own_agents: [] })}
        busy={false}
        onApprove={() => {}}
        onReject={() => {}}
      />,
    );
    expect(await screen.findByText("Files changed during planning")).toBeTruthy();
    expect(screen.getByText("… (+42 more)")).toBeTruthy();
  });

  it("renders NOTHING when the list is empty", async () => {
    render(
      <PlanPanel
        run={run({ plan_changed_files: [], repo_agents: [], own_agents: [] })}
        busy={false}
        onApprove={() => {}}
        onReject={() => {}}
      />,
    );
    // The plan body still renders, so the panel mounted; the section must not.
    expect(await screen.findByText(/step one/)).toBeTruthy();
    expect(screen.queryByText("Files changed during planning")).toBeNull();
  });

  it("renders NOTHING when the field is absent (a pre-#212 run)", async () => {
    // The default run() fixture omits plan_changed_files entirely.
    render(
      <PlanPanel
        run={run({ repo_agents: [], own_agents: [] })}
        busy={false}
        onApprove={() => {}}
        onReject={() => {}}
      />,
    );
    expect(await screen.findByText(/step one/)).toBeTruthy();
    expect(screen.queryByText("Files changed during planning")).toBeNull();
  });

  it("sanitizes untrusted paths for display (strips a U+202E RTL override)", async () => {
    // Written as a \u escape, never a raw byte: a raw format rune trips the lint ratchet
    // AND would reorder this source line as a human reads it. The escape still produces
    // the real U+202E rune at runtime, so this exercises stripUnsafeChars for real — a
    // vacuous form (a literal "&#x202E;" string) would strip nothing. Matches the
    // safeText.test.ts / existing RunView #124 convention.
    const RLO = "\u202E"; // RIGHT-TO-LEFT OVERRIDE
    render(
      <PlanPanel
        run={run({ plan_changed_files: [` M src/a${RLO}b.ts`], repo_agents: [], own_agents: [] })}
        busy={false}
        onApprove={() => {}}
        onReject={() => {}}
      />,
    );
    const heading = await screen.findByText("Files changed during planning");
    const container = heading.parentElement as HTMLElement;
    // stripUnsafeChars ran: the rendered text keeps the path but drops the raw override rune.
    expect(container.textContent).toContain("src/ab.ts");
    expect(container.textContent).not.toContain(RLO);
  });
});

// PRD #362 M4: the run-summary cards (intent / proposed-approved plan / deltas) and their
// per-run collapse. Rendered directly (RunView needs a live stream + a dozen mocks); a
// Map-backed localStorage stub backs the collapse pref, since this jsdom build ships none.
describe("RunSummary (PRD #362 M4)", () => {
  function makeStorage(): Storage {
    const m = new Map<string, string>();
    return {
      getItem: (k: string) => (m.has(k) ? m.get(k)! : null),
      setItem: (k: string, v: string) => void m.set(k, String(v)),
      removeItem: (k: string) => void m.delete(k),
      clear: () => m.clear(),
      key: (i: number) => [...m.keys()][i] ?? null,
      get length() {
        return m.size;
      },
    } as Storage;
  }
  beforeEach(() => {
    Object.defineProperty(window, "localStorage", { configurable: true, value: makeStorage() });
  });

  it("renders nothing when the run carries no summary (issue-title header stands)", () => {
    const { container } = render(<RunSummary run={run({})} />);
    expect(container.innerHTML).toBe("");
  });

  it("shows the intent card when summary_intent is present", () => {
    render(<RunSummary run={run({ summary_intent: "Add a token-bucket rate limiter to the API." })} />);
    expect(screen.getByText("What this run will implement")).toBeTruthy();
    expect(screen.getByText("Add a token-bucket rate limiter to the API.")).toBeTruthy();
  });

  it("labels the plan 'Proposed plan' while awaiting_approval", () => {
    render(
      <RunSummary
        run={run({ status: "awaiting_approval", summary_plan: "Introduce middleware and a store.", summary_deltas: [] })}
      />,
    );
    expect(screen.getByText("Proposed plan")).toBeTruthy();
    expect(screen.queryByText("Approved plan")).toBeNull();
  });

  it("labels the plan 'Approved plan' once past the gate (e.g. running/completed)", () => {
    render(<RunSummary run={run({ status: "running", summary_plan: "Introduce middleware and a store." })} />);
    expect(screen.getByText("Approved plan")).toBeTruthy();
    expect(screen.queryByText("Proposed plan")).toBeNull();
  });

  it("renders deltas tagged by kind", () => {
    render(
      <RunSummary
        run={run({
          status: "awaiting_approval",
          summary_plan: "A plan.",
          summary_deltas: [
            { kind: "added", text: "Add a metrics endpoint." },
            { kind: "changed", text: "Use Redis instead of in-memory." },
            { kind: "dropped", text: "Skip the admin dashboard." },
          ],
        })}
      />,
    );
    expect(screen.getByText("added")).toBeTruthy();
    expect(screen.getByText("changed")).toBeTruthy();
    expect(screen.getByText("dropped")).toBeTruthy();
    expect(screen.getByText("Add a metrics endpoint.")).toBeTruthy();
    expect(screen.getByText("Use Redis instead of in-memory.")).toBeTruthy();
    expect(screen.getByText("Skip the admin dashboard.")).toBeTruthy();
  });

  it("shows the no-deviations line for an empty deltas array", () => {
    render(<RunSummary run={run({ status: "awaiting_approval", summary_plan: "A plan.", summary_deltas: [] })} />);
    expect(screen.getByText("No deviations — the plan matches the original ask")).toBeTruthy();
  });

  it("shows the no-deviations line for null deltas", () => {
    render(<RunSummary run={run({ status: "running", summary_plan: "A plan.", summary_deltas: null })} />);
    expect(screen.getByText("No deviations — the plan matches the original ask")).toBeTruthy();
  });

  it("tolerates a non-array summary_deltas without crashing (renders no-deviations)", () => {
    // Server coerces malformed jsonb to null (M1); this is the defensive second line for a
    // value that somehow reaches the renderer as a non-array.
    const bad = { kind: "added", text: "not a list" } as unknown as Run["summary_deltas"];
    render(<RunSummary run={run({ status: "running", summary_plan: "A plan.", summary_deltas: bad })} />);
    expect(screen.getByText("No deviations — the plan matches the original ask")).toBeTruthy();
  });

  it("renders intent markdown as elements while keeping raw HTML inert and stripping bidi (issue #423)", () => {
    // Intent/plan now render through the shared hardened <Markdown> (issue #423, revising the
    // "rendered as text (web)" clause of Decision 10): markdown syntax becomes real elements,
    // but raw HTML stays INERT text (the pipeline carries NO rehype-raw) and bidi/format chars
    // are still stripped BEFORE the parse. RLO is written as an escape sequence, never a raw byte.
    const RLO = String.fromCodePoint(0x202e);
    const { container } = render(
      <RunSummary
        run={run({ summary_intent: `Call \`fireSweep\` with **max_issues** <script>alert(1)</script> ${RLO}rate limiting` })}
      />,
    );
    // Markdown parsed to real elements.
    expect(container.querySelector("code")?.textContent).toBe("fireSweep");
    expect(container.querySelector("strong")?.textContent).toBe("max_issues");
    // Raw HTML did NOT become a live node (no rehype-raw)…
    expect(container.querySelector("script")).toBeNull();
    // …but survives as inert text on the page.
    expect(container.textContent ?? "").toContain("<script>alert(1)</script>");
    // Bidi/format characters are still stripped.
    expect(container.textContent ?? "").not.toMatch(/[\p{Cf}]/u);
  });

  it("collapses on toggle and remembers the choice per run across a remount", () => {
    const r = run({ id: "run-collapse", summary_intent: "Some intent." });
    const { unmount } = render(<RunSummary run={r} />);
    // Expanded by default: the intent text is visible.
    expect(screen.getByText("Some intent.")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Collapse" }));
    expect(screen.queryByText("Some intent.")).toBeNull();
    unmount();
    // A fresh mount reads the persisted per-run choice → still collapsed.
    render(<RunSummary run={r} />);
    expect(screen.queryByText("Some intent.")).toBeNull();
    expect(screen.getByRole("button", { name: "Expand" })).toBeTruthy();
  });
});

// PRD #209 M5. A seeded run supplies its plan at create time and never enters
// awaiting_approval, so PlanPanel (the approval UI) never renders for it and run.plan_md
// would be unreachable on the run page. SeededPlanPanel is the read-only surface that
// makes it reachable in any state. It must NOT carry the gate's actions — an Approve/Reject
// on a run that has no approval gate would be a lie — which is what keeps the
// awaiting_approval PlanPanel the ONLY approval surface (Success Criterion 2).
describe("SeededPlanPanel (PRD #209 M5)", () => {
  it("renders the seeded plan body in a read-only collapsible", () => {
    const { container } = render(
      <SeededPlanPanel run={run({ status: "running", plan_md: "# Plan\n- seeded step" })} />,
    );
    // The plan markdown reaches the page…
    expect(screen.getByText("seeded step")).toBeTruthy();
    // …inside a native <details> collapsible whose summary names it "Seeded plan".
    expect(container.querySelector("details")).toBeTruthy();
    expect(container.querySelector("summary")?.textContent).toContain("Seeded plan");
  });

  it("carries NONE of the gate's actions — it is not the approval UI", () => {
    render(<SeededPlanPanel run={run({ status: "running", plan_md: "# Plan\n- seeded step" })} />);
    // Positive anchor first, so the absence checks below are not vacuous: a component that
    // rendered nothing would fail here.
    expect(screen.getByText("seeded step")).toBeTruthy();
    // No approve/reject/revise controls. These strings DO render on the awaiting_approval
    // PlanPanel (asserted in the PlanPanel block above), so a regression that copied the
    // gate's actions onto this surface would redden these lines — they are not vacuous.
    expect(screen.queryByRole("button", { name: /Approve plan/i })).toBeNull();
    expect(screen.queryByRole("button", { name: /^Reject$/i })).toBeNull();
    expect(screen.queryByRole("button", { name: /Request changes/i })).toBeNull();
  });

  it("renders nothing when the run carries no plan body", () => {
    const { container } = render(
      <SeededPlanPanel run={run({ status: "running", plan_md: null })} />,
    );
    expect(container.textContent).toBe("");
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

  // PRD #209 M5. A seeded run carries agent_source="repo" from creation but has no
  // repo_agents until the worker's post-checkout report. The card used to render the
  // definitive "internal review was performed by repo-authored agents" claim over an
  // EMPTY chip list — asserting a roster it does not have. The pending state states the
  // source without the roster claim.
  it("a seeded repo-source run with no roster yet shows a pending state, not an asserted empty roster (PRD #209 M5)", () => {
    render(
      <AgentRosterSummary
        run={run({
          status: "running",
          plan_source: "seeded",
          agent_source: "repo",
          repo_agents: null,
        })}
      />,
    );
    // Positive: the pending copy names the source (repo agents) and explains the gap.
    expect(screen.getByText(/roster appears here once the worker checks out/i)).toBeTruthy();
    expect(screen.getByText(".claude/agents/")).toBeTruthy();
    // Non-vacuous negative: the past-tense "internal review was performed by" claim —
    // the misleading assertion this fix removes — must NOT render here. It CAN render
    // (the adjacent "repo-source run" test above asserts it does for a populated roster),
    // so reverting the fix reddens this line. Measured: with the fix reverted this run
    // renders "its internal review was performed by repo-authored agents" and this fails.
    expect(screen.queryByText(/internal review was performed by/i)).toBeNull();
    // The past-tense "used the repository's own agents" wording is likewise gone.
    expect(screen.queryByText(/used the repository's own agents/i)).toBeNull();
    // And no chip is rendered claiming a specific agent.
    expect(screen.queryByText(/^●?\s*coder/i)).toBeNull();
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
      <RunFailureReason
        run={run({ failure_reason: "the repo \u202Eapproved\u200B this" })}
      />,
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
    const { container } = render(
      <RunFailureReason run={run({ failure_reason: null })} />,
    );
    expect(container.textContent).toBe("");
  });
});

describe("HealthFlag — the health reason (#124, text channel)", () => {
  it("strips bidi/zero-width characters out of the rendered reason", () => {
    const { container } = render(
      <HealthFlag
        run={run({
          status: "running",
          health: "stalled",
          health_reason: "no output for \u202E20m\u200B",
        })}
      />,
    );
    expect(container.textContent ?? "").not.toMatch(/[\p{Cf}]/u);
    // Sharper here than in the sibling case: the pill DOES mount without a reason (it
    // renders `⚠ stalled`), so `not.toMatch` passes on a null fixture and only this line
    // catches it. Measured: `expected '⚠ stalled' to contain 'no output for 20m'`.
    expect(container.textContent).toContain("no output for 20m");
  });
});

// Issue #185: the live region must be ALWAYS MOUNTED and the visible pill must NOT be a
// live region. A region created in the same tick as its first message is silent (assistive
// tech announces CHANGES to a region that already existed — Board.tsx S5), so role="status"
// on the pill itself never announced the flag despite the comment claiming it did.
describe("HealthFlag live region (#185)", () => {
  it("keeps the sr-only status region mounted and EMPTY for a healthy run", () => {
    const { container } = render(<HealthFlag run={run({ status: "running", health: "ok" })} />);
    // No visible pill for a healthy run…
    const pill = [...container.querySelectorAll("span")].find((s) => (s.textContent ?? "").includes("⚠"));
    expect(pill).toBeUndefined();
    // …but the live region exists so a flag arriving later is an announced content change,
    // not a silent mount-with-first-message.
    const live = container.querySelector('span.sr-only[role="status"]') as HTMLElement;
    expect(live).toBeTruthy();
    expect(live.getAttribute("aria-live")).toBe("polite");
    expect(live.textContent).toBe("");
  });

  it("announces via the sr-only region, and the visible pill is NOT a live region", () => {
    const { container } = render(
      <HealthFlag run={run({ status: "running", health: "stalled", health_reason: "no output" })} />,
    );
    // The sr-only region carries the flag label — the thing a screen reader hears.
    const live = container.querySelector('span.sr-only[role="status"]') as HTMLElement;
    expect(live).toBeTruthy();
    expect(live.getAttribute("aria-live")).toBe("polite");
    expect(live.textContent?.trim()).not.toBe("");
    // The visible pill exists but carries NO role="status"/aria-live — that was the bug.
    const pill = [...container.querySelectorAll("span")].find((s) => (s.textContent ?? "").includes("⚠"))!;
    expect(pill).toBeTruthy();
    expect(pill.getAttribute("role")).toBeNull();
    expect(pill.hasAttribute("aria-live")).toBe(false);
    // Exactly ONE status region on the whole flag — the sr-only one.
    expect(container.querySelectorAll('[role="status"]').length).toBe(1);
  });
});

// Issue #124: `run.branch` is WORKER-supplied and ingest stores it as
// `stripNULParam(req.Branch)` — NUL only, no Cc/Cf. Extracted to be assertable at all;
// before that, dropping the strip left the whole file green.
describe("RunCompletedLine — the worker-supplied branch carries no format characters (#124)", () => {
  it("strips bidi/zero-width characters out of run.branch", () => {
    const { container } = render(
      <RunCompletedLine
        run={run({ branch: "agent/issue-\u202E7\u200B", mr_iid: null })}
        duration="2m"
      />,
    );
    // Anchored on the duration, which the mutation cannot move.
    expect(container.textContent).toContain("Ran for 2m");
    expect(container.textContent ?? "").not.toMatch(/[\p{Cf}]/u);
    expect(screen.getByText("agent/issue-7")).toBeTruthy();
  });
});

describe("RunCompletedLine — report-only deliverable (issue #279)", () => {
  it("names the report-only deliverable and points to findings when report_md is present", () => {
    const { container } = render(
      <RunCompletedLine
        run={run({ report_only: true, branch: null, mr_iid: null, report_md: "All checks passed." })}
        duration="3m"
      />,
    );
    expect(container.textContent).toContain("Ran for 3m");
    expect(container.textContent).toContain("Report only — no merge request; findings below.");
  });

  it("does not promise findings below when report_md is empty (no dangling pointer)", () => {
    const { container } = render(
      <RunCompletedLine run={run({ report_only: true, branch: null, mr_iid: null })} duration="3m" />,
    );
    expect(container.textContent).toContain("Report only — no merge request.");
    expect(container.textContent).not.toContain("findings below");
  });

  it("suppresses the branch clause on a report-only run (mutually exclusive)", () => {
    // Defensive: a report_only completion pushes no branch, but the clause must not render a
    // contradictory "Branch … Report only" line even if a branch value somehow rode along.
    const { container } = render(
      <RunCompletedLine run={run({ report_only: true, branch: "agent/issue-9", mr_iid: null })} duration="3m" />,
    );
    expect(container.textContent).toContain("Report only — no merge request");
    expect(container.textContent).not.toContain("Branch");
  });

  it("does not name a report-only deliverable for a normal completion", () => {
    const { container } = render(
      <RunCompletedLine run={run({ branch: "agent/issue-87", mr_iid: 42 })} duration="3m" mrState="merged" />,
    );
    expect(container.textContent).not.toContain("Report only");
  });
});

describe("RunCompletedLine — the declared PRD-completion move (issue #150)", () => {
  it("names the path prd_done_path was set to on a completed run", () => {
    const { container } = render(
      <RunCompletedLine
        run={run({ branch: "agent/issue-72", mr_iid: 5, prd_done_path: "prds/done/72-x.md" })}
        duration="4m"
        mrState="opened"
      />,
    );
    expect(container.textContent).toContain("PRD moved to");
    expect(screen.getByText("prds/done/72-x.md")).toBeTruthy();
  });

  it("omits the PRD clause when prd_done_path is absent", () => {
    const { container } = render(
      <RunCompletedLine run={run({ branch: "agent/issue-72", mr_iid: 5 })} duration="4m" mrState="opened" />,
    );
    expect(container.textContent).toContain("Branch");
    expect(container.textContent).not.toContain("PRD moved to");
  });

  it("strips bidi/control characters out of a hostile prd_done_path", () => {
    const { container } = render(
      <RunCompletedLine
        run={run({ branch: "agent/issue-72", mr_iid: null, prd_done_path: "prds/done/‮72-x​.md" })}
        duration="4m"
      />,
    );
    expect(container.textContent).toContain("PRD moved to");
    expect(container.textContent ?? "").not.toMatch(/[\p{Cf}]/u);
    expect(screen.getByText("prds/done/72-x.md")).toBeTruthy();
  });
});

// PRD #300: the frozen per-schedule model is shown in the STATUS-INDEPENDENT header
// metadata row (a Badge), not in the completed-hero line — a wrong/typo'd model must be
// visible on a FAILED or stopped run too (Risks / SC6). These render the whole page
// (useRunStream mocked) so the header badge is exercised where it actually lives.
describe("RunView header — the frozen per-schedule model badge (PRD #300)", () => {
  const MODEL_BADGE_TITLE = "Model this run was fired with (frozen by its schedule)";

  function renderPage(over: Partial<Run>) {
    mockUseRunStream.mockReturnValue({
      run: run(over),
      messages: [],
      connected: true,
      error: "",
      submit: vi.fn(),
      refreshRun: vi.fn(),
      inputs: [],
      canSteer: false,
    } as unknown as ReturnType<typeof useRunStream>);
    mockApi.getRunReview.mockResolvedValue({ review: null, pending_judge: null });
    return render(
      <MemoryRouter initialEntries={["/runs/r1"]}>
        <RunView />
      </MemoryRouter>,
    );
  }

  it("shows the model badge on a COMPLETED run", async () => {
    const { container } = renderPage({
      status: "completed",
      model: "fable",
      branch: "agent/issue-87",
      mr_iid: 42,
      mr_state: "merged",
    });
    expect(await screen.findByTitle(MODEL_BADGE_TITLE)).toBeTruthy();
    expect(container.textContent).toContain("model fable");
    // The old completed-hero "Model <code>…</code>." clause is gone — no double render.
    expect(container.textContent).not.toContain("Model fable");
  });

  it("shows the model badge on a FAILED run — the whole point of the header placement (SC6)", async () => {
    const { container } = renderPage({ status: "failed", model: "fable", failure_reason: "boom" });
    expect(await screen.findByTitle(MODEL_BADGE_TITLE)).toBeTruthy();
    expect(container.textContent).toContain("model fable");
  });

  it("strips format characters from the model value, like the other header metadata", async () => {
    const injected = `cl${String.fromCodePoint(0x202e)}aude`;
    const { container } = renderPage({ status: "failed", model: injected });
    expect(await screen.findByTitle(MODEL_BADGE_TITLE)).toBeTruthy();
    expect(container.textContent ?? "").not.toMatch(/[\p{Cf}]/u);
  });

  it("shows no model badge when the run inherited (model null)", async () => {
    renderPage({ status: "failed", model: null });
    // Wait for the page to settle (RunHeading renders the issue title synchronously).
    await screen.findByText("Add rate limiting");
    expect(screen.queryByTitle(MODEL_BADGE_TITLE)).toBeNull();
  });

  // PRD #305: the frozen "apply model also to agents" flag, shown on every status.
  const OVERRIDE_BADGE_TITLE =
    "This run's model was applied to every subagent, overriding their own model pins";

  it("shows the override-subagent-model badge when the run applied its model fleet-wide", async () => {
    const { container } = renderPage({
      status: "completed",
      model: "fable",
      override_subagent_model: true,
    });
    expect(await screen.findByTitle(OVERRIDE_BADGE_TITLE)).toBeTruthy();
    expect(container.textContent).toContain("model on all agents");
  });

  it("shows no override-subagent-model badge when the flag is off (today's default)", async () => {
    renderPage({ status: "failed", model: "fable", override_subagent_model: false });
    await screen.findByText("Add rate limiting");
    expect(screen.queryByTitle(OVERRIDE_BADGE_TITLE)).toBeNull();
  });
});

// PRD #320 M6: the queue-priority pill + the owner's Expedite/undo action in the run
// header. These render the whole page (useRunStream mocked) so the control is exercised
// where it lives — gated on run.status === "queued" and, for the action, on canSteer.
describe("RunView — queue priority pill + Expedite action (PRD #320 M6)", () => {
  function renderPage(over: Partial<Run>, canSteer: boolean) {
    const refreshRun = vi.fn();
    mockUseRunStream.mockReturnValue({
      run: run(over),
      messages: [],
      connected: true,
      error: "",
      submit: vi.fn(),
      refreshRun,
      inputs: [],
      canSteer,
    } as unknown as ReturnType<typeof useRunStream>);
    mockApi.getRunReview.mockResolvedValue({ review: null, pending_judge: null });
    const utils = render(
      <MemoryRouter initialEntries={["/runs/r1"]}>
        <RunView />
      </MemoryRouter>,
    );
    return { ...utils, refreshRun };
  }

  it("shows the Deprioritized pill and an Expedite button for an owner's queued demoted run", async () => {
    renderPage({ status: "queued", priority: "background" }, true);
    await screen.findByText("Add rate limiting");
    expect(screen.getByText("Deprioritized")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Expedite" })).toBeTruthy();
  });

  it("clicking Expedite calls api.expediteRun(id, true) then refreshRun", async () => {
    mockApi.expediteRun.mockResolvedValue({ run: run({ status: "queued", priority: "expedited" }) });
    const { refreshRun } = renderPage({ status: "queued", priority: "background" }, true);
    await screen.findByText("Add rate limiting");
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Expedite" }));
    });
    await waitFor(() => expect(mockApi.expediteRun).toHaveBeenCalledWith("r1", true));
    await waitFor(() => expect(refreshRun).toHaveBeenCalled());
  });

  it("an expedited run offers Undo expedite, which calls api.expediteRun(id, false)", async () => {
    mockApi.expediteRun.mockResolvedValue({ run: run({ status: "queued", priority: "normal" }) });
    renderPage({ status: "queued", priority: "expedited" }, true);
    await screen.findByText("Add rate limiting");
    expect(screen.getByText("Expedited")).toBeTruthy();
    await act(async () => {
      fireEvent.click(screen.getByRole("button", { name: "Undo expedite" }));
    });
    await waitFor(() => expect(mockApi.expediteRun).toHaveBeenCalledWith("r1", false));
  });

  it("a NON-OWNER (canSteer=false) sees the pill and inert text, never a button", async () => {
    renderPage({ status: "queued", priority: "background" }, false);
    await screen.findByText("Add rate limiting");
    // The pill is not owner-gated…
    expect(screen.getByText("Deprioritized")).toBeTruthy();
    // …but the action is inert text, not a button that would 404 (LimitWaitPanel's rule).
    expect(screen.getByText(/only the run's owner can expedite it/i)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Expedite" })).toBeNull();
  });

  it("renders no pill and no action on a non-queued run carrying a stale priority", async () => {
    renderPage({ status: "running", priority: "expedited" }, true);
    await screen.findByText("Add rate limiting");
    expect(screen.queryByText("Expedited")).toBeNull();
    expect(screen.queryByRole("button", { name: "Expedite" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Undo expedite" })).toBeNull();
  });
});

// issue #279: the completed-hero "report only" chip and the Findings panel are inline in
// the RunView page. These render the whole page (useRunStream mocked) and assert the
// UNTRUSTED report_md is escaped plain text, never a live markup sink.
describe("RunView report-only surfaces (issue #279)", () => {
  function renderPage(over: Partial<Run>) {
    mockUseRunStream.mockReturnValue({
      run: run(over),
      messages: [],
      connected: true,
      error: "",
      submit: vi.fn(),
      refreshRun: vi.fn(),
      inputs: [],
      canSteer: false,
    } as unknown as ReturnType<typeof useRunStream>);
    // JudgePanel fetches its own review on mount; a null review settles it into the
    // never-judged empty state, which is irrelevant to these assertions.
    mockApi.getRunReview.mockResolvedValue({ review: null, pending_judge: null });
    return render(
      <MemoryRouter initialEntries={["/runs/r1"]}>
        <RunView />
      </MemoryRouter>,
    );
  }

  it("renders the 'report only' chip, the deliverable line, and the escaped findings", async () => {
    const { container } = renderPage({
      status: "completed",
      report_only: true,
      report_md: "No unscoped queries found. Two admin reports read cross-tenant by design.",
      branch: null,
      mr_iid: null,
      mr_web_url: null,
      started_at: "2026-01-01T00:00:00Z",
      finished_at: "2026-01-01T00:03:00Z",
    });
    expect(await screen.findByText("report only")).toBeTruthy();
    expect(container.textContent).toContain("Report only — no merge request; findings below.");
    expect(screen.getByText("Findings")).toBeTruthy();
    expect(
      screen.getByText("No unscoped queries found. Two admin reports read cross-tenant by design."),
    ).toBeTruthy();
  });

  it("renders an HTML/markdown payload in report_md as literal text, never as markup", async () => {
    const payload = '<img src=x onerror="alert(1)"> [pwn](javascript:alert(1))';
    const { container } = renderPage({
      status: "completed",
      report_only: true,
      report_md: payload,
      branch: null,
      mr_iid: null,
      mr_web_url: null,
    });
    // The payload is present verbatim as text …
    expect(await screen.findByText(payload)).toBeTruthy();
    // … and nothing in the rendered tree became a live sink from it.
    expect(container.querySelector("img")).toBeNull();
    expect(container.querySelector('a[href^="javascript"]')).toBeNull();
  });

  it("shows the MR button and no report-only chip for a normal completion", async () => {
    const mrUrl = "https://gitlab.example.com/g/p/-/merge_requests/42";
    const { container } = renderPage({
      status: "completed",
      report_only: false,
      branch: "agent/issue-87",
      mr_iid: 42,
      mr_state: "merged",
      mr_web_url: mrUrl,
    });
    // The MR link is the run's deliverable; its label is split across nodes + an icon,
    // so anchor on the href and the concatenated text.
    const link = await waitFor(() => {
      const a = container.querySelector(`a[href="${mrUrl}"]`);
      if (!a) throw new Error("MR link not rendered yet");
      return a;
    });
    expect(link.textContent).toContain("Open merge request");
    expect(screen.queryByText("report only")).toBeNull();
    expect(screen.queryByText("Findings")).toBeNull();
  });
});

// PRD #377 M2: the failed card surfaces the worker-preserved diff for a GitHub run whose
// branch touched .github/workflows/** (the bot's repo-only PAT can't push it). The block is
// gated on run.preserved_patch on a terminal, non-completed run — the same gate as
// failure_reason — and must read as "valid work preserved", not a crash.
describe("RunView preserved-patch surface (PRD #377)", () => {
  const FRAMING = "Here’s the diff to land as a human PR:";
  const PATCH =
    "diff --git a/.github/workflows/main-guard.yml b/.github/workflows/main-guard.yml\n" +
    "+name: main-guard\n";

  function renderPage(over: Partial<Run>) {
    mockUseRunStream.mockReturnValue({
      run: run(over),
      messages: [],
      connected: true,
      error: "",
      submit: vi.fn(),
      refreshRun: vi.fn(),
      inputs: [],
      canSteer: false,
    } as unknown as ReturnType<typeof useRunStream>);
    mockApi.getRunReview.mockResolvedValue({ review: null, pending_judge: null });
    return render(
      <MemoryRouter initialEntries={["/runs/r1"]}>
        <RunView />
      </MemoryRouter>,
    );
  }

  it("renders the framing label and the diff on a failed run with a preserved patch", async () => {
    const { container } = renderPage({
      status: "failed",
      forge_type: "github",
      failure_reason: "Land the preserved diff as a human PR; see docs/github-bot-setup.md.",
      preserved_patch: PATCH,
    });
    // The framing reads as "valid work preserved", not an error.
    expect(await screen.findByText(FRAMING, { exact: false })).toBeTruthy();
    // The diff renders in a monospace <pre> block, verbatim.
    const pre = container.querySelector("pre");
    expect(pre).not.toBeNull();
    expect(pre?.textContent).toContain("diff --git a/.github/workflows/main-guard.yml");
    expect(pre?.textContent).toContain("+name: main-guard");
  });

  it("does not render the diff block on a failed run without a preserved patch", async () => {
    const { container } = renderPage({
      status: "failed",
      forge_type: "github",
      failure_reason: "run timed out after 2h0m0s (RUN_TIMEOUT)",
      preserved_patch: null,
    });
    // Wait for the failure card (the reason) to mount before asserting absence.
    expect(await screen.findByText("run timed out after 2h0m0s (RUN_TIMEOUT)")).toBeTruthy();
    expect(screen.queryByText(FRAMING, { exact: false })).toBeNull();
    expect(container.querySelector("pre")).toBeNull();
  });

  it("does not render the diff block on a completed run even if a patch is present", async () => {
    const { container } = renderPage({
      status: "completed",
      forge_type: "github",
      branch: "agent/issue-87",
      preserved_patch: PATCH,
    });
    expect(await screen.findByText("Run completed")).toBeTruthy();
    expect(screen.queryByText(FRAMING, { exact: false })).toBeNull();
    expect(container.querySelector("pre")).toBeNull();
  });
});

describe("RunHeading — the forge issue title carries no format characters (#124)", () => {
  it("strips bidi/zero-width characters, and keeps the iid beside them", () => {
    const { container } = render(
      <RunHeading
        run={run({
          issue_title: "Fix the \u202Eparser\u200B bug",
          issue_iid: 7,
        })}
      />,
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
      triage: {
        total: 1,
        todo: 1,
        filed: 0,
        done: 0,
        dismissed: 0,
        false_positives: 0,
      },
      ...over,
    };
  }

  it("renders the verdict chip + recommendations for a judged run", async () => {
    mockApi.getRunReview.mockResolvedValue({ review: review(), pending_judge: null });
    render(<JudgePanel run={run({ status: "completed" })} />);

    expect(await screen.findByText("Issues found")).toBeTruthy();
    expect(screen.getByText("Lost time to a missing tool.")).toBeTruthy();
    expect(screen.getByText("Install a worker tool")).toBeTruthy();
    expect(screen.getByText("shellcheck")).toBeTruthy();
    expect(screen.getByText("hit command not found twice")).toBeTruthy();
    expect(mockApi.getRunReview).toHaveBeenCalledWith("r1");
  });

  it("renders the judge run cost/time strip when the judge posted usage (PRD #69 M6)", async () => {
    mockApi.getRunReview.mockResolvedValue({
      pending_judge: null,
      review: review({
        judge_run: {
          judge_run_id: "judge-1",
          claimed_at: "2026-01-01T00:00:00Z",
          started_at: "2026-01-01T00:00:05Z",
          finished_at: "2026-01-01T00:00:19Z", // 14s
          usage: {
            input_tokens: 48200,
            cache_read_tokens: 12800,
            cache_creation_tokens: 3100,
            output_tokens: 1840,
            cost_usd: 0.42,
          },
        },
      }),
    });
    const { container } = render(<JudgePanel run={run({ status: "completed" })} />);

    const strip = await waitFor(() => {
      const el = container.querySelector('[aria-label="Judge run cost and time"]');
      expect(el).not.toBeNull();
      return el as HTMLElement;
    });
    const text = strip.textContent ?? "";
    // The four tiles: tokens in/out (formatTokens), duration (14s), cost (formatCost).
    expect(text).toContain("Tokens in");
    expect(text).toContain("48.2k"); // formatTokens(48200)
    expect(text).toContain("Tokens out");
    expect(text).toContain("1.8k"); // formatTokens(1840) → "1.8k"
    expect(text).toContain("Duration");
    expect(text).toContain("Cost");
    expect(text).toContain("$0.42");
  });

  it("omits the judge cost/time strip for a pre-feature judge with null usage (PRD #69 M6)", async () => {
    mockApi.getRunReview.mockResolvedValue({
      pending_judge: null,
      review: review({
        judge_run: {
          judge_run_id: "judge-1",
          claimed_at: "2026-01-01T00:00:00Z",
          started_at: "2026-01-01T00:00:05Z",
          finished_at: "2026-01-01T00:00:19Z",
          usage: null, // no run_usage row ⇒ no strip, never a fabricated 0
        },
      }),
    });
    const { container } = render(<JudgePanel run={run({ status: "completed" })} />);

    // Wait for the panel to settle (the verdict chip renders), THEN assert no strip.
    await screen.findByText("Issues found");
    expect(container.querySelector('[aria-label="Judge run cost and time"]')).toBeNull();
  });

  it("renders review markdown as elements while keeping raw HTML inert", async () => {
    // summary_md now renders through the shared hardened <Markdown> (same as plan_md):
    // markdown syntax becomes real elements, but raw HTML stays INERT text because the
    // pipeline carries NO rehype-raw. So `**not bold**` is a <strong>, while the
    // <img onerror> string never becomes an <img> node (nothing to fire onerror on).
    mockApi.getRunReview.mockResolvedValue({
      pending_judge: null,
      review: review({
        summary_md: "<img src=x onerror=alert(1)> **not bold**",
        recommendations: [],
      }),
    });
    const { container } = render(
      <JudgePanel run={run({ status: "completed" })} />,
    );

    // The bold markdown became a real <strong>.
    const strong = await waitFor(() => {
      const el = container.querySelector("strong");
      expect(el).not.toBeNull();
      return el!;
    });
    expect(strong.textContent).toBe("not bold");
    // The raw HTML did NOT become a real element / active script.
    expect(container.querySelector("img")).toBeNull();
    expect(container.querySelector("script")).toBeNull();
    // The <img ...> markup survives as inert text on the page.
    expect(container.textContent).toContain("<img src=x onerror=alert(1)>");
  });

  it("renders summary_md and rationale_md markdown as elements (bold/list/code/link)", async () => {
    mockApi.getRunReview.mockResolvedValue({
      pending_judge: null,
      review: review({
        summary_md: "**bold** and `code`\n\n- bullet\n\n[label](https://example.com)",
        recommendations: [
          {
            id: "rc1",
            category: "install_worker_tool",
            target: "shellcheck",
            rationale_md: "**why** and `cmd`\n\n- reason\n\n[docs](https://example.com)",
            confidence: "high",
            created_at: "2026-01-01T00:00:00Z",
          },
        ],
      }),
    });
    const { container } = render(<JudgePanel run={run({ status: "completed" })} />);

    await screen.findByText("Issues found");

    // Both surfaces (summary + the one rationale) render markdown elements.
    const strongs = [...container.querySelectorAll("strong")].map((s) => s.textContent);
    expect(strongs).toContain("bold");
    expect(strongs).toContain("why");
    const codes = [...container.querySelectorAll("code")].map((c) => c.textContent);
    expect(codes).toContain("code");
    expect(codes).toContain("cmd");
    const items = [...container.querySelectorAll("li")].map((li) => (li.textContent ?? "").trim());
    expect(items).toContain("bullet");
    expect(items).toContain("reason");
    // Links are real anchors, forced external.
    const links = [...container.querySelectorAll("a")].filter(
      (a) => a.getAttribute("href") === "https://example.com",
    );
    expect(links.length).toBe(2);
    for (const a of links) {
      expect(a.getAttribute("target")).toBe("_blank");
      expect(a.getAttribute("rel")).toBe("noopener noreferrer");
    }
  });

  it("neutralizes dangerous URL schemes in judge markdown links", async () => {
    mockApi.getRunReview.mockResolvedValue({
      pending_judge: null,
      review: review({
        summary_md: "[x](javascript:alert(1)) and [y](data:text/html,evil)",
        recommendations: [],
      }),
    });
    const { container } = render(<JudgePanel run={run({ status: "completed" })} />);

    await screen.findByText("Issues found");
    // No active link carries a dangerous scheme — urlTransform + schemeIsDangerous strip it.
    expect(container.querySelector('a[href^="javascript:"]')).toBeNull();
    expect(container.querySelector('a[href^="data:"]')).toBeNull();
  });

  it("keeps raw <script>/<img onerror> in summary_md AND rationale_md inert", async () => {
    mockApi.getRunReview.mockResolvedValue({
      pending_judge: null,
      review: review({
        summary_md: "<script>alert(1)</script> and <img src=x onerror=alert(1)>",
        recommendations: [
          {
            id: "rc1",
            category: "install_worker_tool",
            target: "shellcheck",
            rationale_md: "<script>alert(2)</script> and <img src=y onerror=alert(2)>",
            confidence: "high",
            created_at: "2026-01-01T00:00:00Z",
          },
        ],
      }),
    });
    const { container } = render(<JudgePanel run={run({ status: "completed" })} />);

    await screen.findByText("Issues found");
    // No rehype-raw: neither surface produces a live script or image node.
    expect(container.querySelector("script")).toBeNull();
    expect(container.querySelector("img")).toBeNull();
  });

  it("renders nothing for empty/whitespace summary_md and rationale_md", async () => {
    mockApi.getRunReview.mockResolvedValue({
      pending_judge: null,
      review: review({
        summary_md: "   ",
        recommendations: [
          {
            id: "rc1",
            category: "install_worker_tool",
            target: "shellcheck",
            rationale_md: "",
            confidence: "high",
            created_at: "2026-01-01T00:00:00Z",
          },
        ],
      }),
    });
    const { container } = render(<JudgePanel run={run({ status: "completed" })} />);

    // The panel still renders (positive anchor) but the .trim() guards suppress the
    // empty markdown surfaces entirely — no empty prose box on either field.
    await screen.findByText("Issues found");
    expect(container.querySelector(".judge-prose")).toBeNull();
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
      pending_judge: null,
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
    const { container } = render(
      <JudgePanel run={run({ status: "completed" })} />,
    );
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
    mockApi.getRunReview.mockResolvedValue({ review: null, pending_judge: null });
    mockApi.rerunJudge.mockResolvedValue({
      run: run({ id: "judge1", kind: "judge", status: "queued" }),
    });
    render(<JudgePanel run={run({ status: "failed" })} />);

    const btn = await screen.findByText("Run judge");
    expect(screen.getByText(/hasn't been judged yet/i)).toBeTruthy();
    fireEvent.click(btn);
    expect(mockApi.rerunJudge).toHaveBeenCalledWith("r1");
    // "Judge re-queued" verbatim, and ONLY on this arm: the note is armed here by the
    // local optimistic flag, set right after THIS tab's POST resolved and before any
    // fetch has returned — the one place the panel actually knows the viewer re-queued
    // it. Two matches because the sr-only live region carries the same sentence.
    const requeued = await screen.findAllByText(
      "Judge re-queued — the new verdict will appear here when it finishes.",
    );
    expect(requeued).toHaveLength(2);
  });

  // ── Pending judge (PRD #119) ──────────────────────────────────────────────────────────
  // The panel's two "no verdict" causes are opposite affordances: nobody is judging this
  // (offer the button) vs a judge is already coming (say so, and take the button away —
  // its only possible outcome is the 409 from the one-active-judge-per-target index).
  const pending = (state: "scheduled" | "running") => ({
    state,
    enqueued_at: "2026-03-01T00:00:00Z",
  });

  it("shows the scheduled copy and a disabled button for an unjudged run with a judge queued (#119)", async () => {
    mockApi.getRunReview.mockResolvedValue({ review: null, pending_judge: pending("scheduled") });
    render(<JudgePanel run={run({ status: "failed" })} />);

    const btn = await screen.findByRole("button", { name: "Judge scheduled" });
    expect((btn as HTMLButtonElement).disabled).toBe(true);
    // Twice: the visible paragraph, and the sr-only live region that is the only thing
    // announcing this state to a screen reader (the button carrying it is disabled, so
    // it is out of the tab order and cannot be reached to hear the label).
    expect(
      screen.getAllByText("Judge scheduled — the verdict will appear here when it finishes."),
    ).toHaveLength(2);
    // The never-judged copy is the OTHER cause and must not appear for this one.
    expect(screen.queryByText(/hasn't been judged yet/i)).toBeNull();
    // The redundant click is gone: pressing the disabled button fires no POST.
    fireEvent.click(btn);
    expect(mockApi.rerunJudge).not.toHaveBeenCalled();
  });

  it("shows the in-progress copy and a disabled button once a worker has the judge (#119)", async () => {
    mockApi.getRunReview.mockResolvedValue({ review: null, pending_judge: pending("running") });
    render(<JudgePanel run={run({ status: "failed" })} />);

    const btn = await screen.findByRole("button", { name: "Judge running…" });
    expect((btn as HTMLButtonElement).disabled).toBe(true);
    expect(screen.getAllByText("Judge in progress…")).toHaveLength(2);
    fireEvent.click(btn);
    expect(mockApi.rerunJudge).not.toHaveBeenCalled();
  });

  // The live region itself, asserted by NAME rather than only through the two-match
  // counts above: those pass for any second copy of the string, including a visible one.
  // What a screen reader needs is specifically an sr-only polite status region that was
  // ALREADY MOUNTED before the text arrived — a region created in the same render as its
  // first message is typically silent, and looks perfectly correct in the DOM.
  it("announces the pending state through an sr-only region mounted before its text (#119)", async () => {
    mockApi.getRunReview.mockResolvedValue({ review: null, pending_judge: null });
    const { rerender } = render(<JudgePanel run={run({ status: "failed" })} />);
    await screen.findByText(/hasn't been judged yet/i);

    const live = document.querySelector('span.sr-only[role="status"]') as HTMLElement;
    expect(live).toBeTruthy();
    expect(live.getAttribute("aria-live")).toBe("polite");
    // Mounted and EMPTY while nothing is in flight.
    expect(live.textContent).toBe("");

    // A judge appears on the next fetch: the SAME node gains the text, which is the
    // change assistive tech announces.
    mockApi.getRunReview.mockResolvedValue({ review: null, pending_judge: pending("running") });
    rerender(<JudgePanel run={run({ id: "r2", status: "failed" })} />);
    await screen.findByRole("button", { name: "Judge running…" });
    expect(document.querySelector('span.sr-only[role="status"]')!.textContent).toBe(
      "Judge in progress…",
    );
  });

  // Exactly ONE region carries the sentence: two live regions on the same string make a
  // screen reader say it twice, so the visible paragraph must stay purely visual.
  it("does not double-announce the pending copy (#119)", async () => {
    mockApi.getRunReview.mockResolvedValue({ review: review(), pending_judge: pending("scheduled") });
    render(<JudgePanel run={run({ status: "completed" })} />);
    await screen.findByRole("button", { name: "Judge scheduled" });

    const regions = [...document.querySelectorAll('[role="status"][aria-live="polite"]')];
    const carrying = regions.filter((r) => /A judge is scheduled/.test(r.textContent ?? ""));
    expect(carrying).toHaveLength(1);
    expect((carrying[0] as HTMLElement).classList.contains("sr-only")).toBe(true);
  });

  // The survives-a-reload case, and the reason it is asserted on a FRESH mount with no
  // click in the test: before #119 the re-queued note and the disabled button came from a
  // local flag that only existed in the tab that pressed the button, so any other viewer —
  // or the same viewer after a reload — was re-offered an action that would only 409.
  it("shows the re-judge-in-flight note over an existing verdict on a fresh mount (#119)", async () => {
    mockApi.getRunReview.mockResolvedValue({ review: review(), pending_judge: pending("running") });
    render(<JudgePanel run={run({ status: "completed" })} />);

    const btn = await screen.findByRole("button", { name: "Judge running…" });
    expect((btn as HTMLButtonElement).disabled).toBe(true);
    // NEUTRAL wording on this arm, and that is the point of asserting it on a fresh
    // mount with no click: all the panel knows here is that the SERVER reports an active
    // judge. It may have been auto-enqueued at the terminal transition, or started by
    // another admin. "Judge re-queued" would tell this viewer they did something they
    // did not do — and it is in the wrong tense besides, sitting next to a button that
    // reads "Judge running…".
    expect(
      screen.getAllByText("A judge is running for this run — the new verdict will appear here when it finishes."),
    ).toHaveLength(2);
    expect(screen.queryByText(/re-queued/i)).toBeNull();
    // The existing verdict keeps rendering underneath it.
    expect(screen.getByText("Issues found")).toBeTruthy();
  });

  // The scheduled sibling of the arm above: the note tracks pendingJudge.state, so it
  // agrees with the button rather than describing a judge that is already running.
  it("says SCHEDULED, not running, while the server-truth judge is still queued (#119)", async () => {
    mockApi.getRunReview.mockResolvedValue({ review: review(), pending_judge: pending("scheduled") });
    render(<JudgePanel run={run({ status: "completed" })} />);

    await screen.findByRole("button", { name: "Judge scheduled" });
    expect(
      screen.getAllByText("A judge is scheduled for this run — the new verdict will appear here when it finishes."),
    ).toHaveLength(2);
    expect(screen.queryByText(/re-queued/i)).toBeNull();
  });

  // The other arm, on the same panel state: once THIS tab has fired the POST — and only
  // until the next fetch answers — "Judge re-queued" is an accurate claim and is kept
  // verbatim. Same note, different sentence, because the two arms know different facts.
  it("keeps the re-queued wording for this tab's own optimistic click (#119)", async () => {
    mockApi.getRunReview.mockResolvedValue({ review: review(), pending_judge: null });
    mockApi.rerunJudge.mockResolvedValue({
      run: run({ id: "judge1", kind: "judge", status: "queued" }),
    });
    render(<JudgePanel run={run({ status: "completed" })} />);

    fireEvent.click(await screen.findByText("Re-run judge"));
    expect(
      await screen.findAllByText("Judge re-queued — the new verdict will appear here when it finishes."),
    ).toHaveLength(2);
    // Not the neutral server-truth sentence: nothing on the wire said a judge is active.
    expect(screen.queryByText(/A judge is (scheduled|running) for this run/)).toBeNull();
  });

  // The TOCTOU backstop. The button is disabled whenever the last fetch saw a pending
  // judge, but that answer is point-in-time: an auto-judge can enqueue between the fetch
  // and the click. That 409 is absorbed — re-fetch, converge to pending — because the user
  // asked for a judge and a judge is running; that is not an error to show them.
  it("absorbs the already-active 409 into a re-fetch that converges to the pending state (#119)", async () => {
    const { ApiError } = await import("../lib/api");
    mockApi.getRunReview
      .mockResolvedValueOnce({ review: review(), pending_judge: null })
      .mockResolvedValue({ review: review(), pending_judge: pending("running") });
    mockApi.rerunJudge.mockRejectedValue(
      new ApiError(409, "a judge run is already in progress for this run"),
    );
    render(<JudgePanel run={run({ status: "completed" })} />);

    fireEvent.click(await screen.findByText("Re-run judge"));

    // The panel converged to the server's answer…
    const btn = await screen.findByRole("button", { name: "Judge running…" });
    expect((btn as HTMLButtonElement).disabled).toBe(true);
    // …and the raw 409 never reached the user as an error.
    expect(screen.queryByText(/already in progress/i)).toBeNull();
  });

  // The route answers 409 for a SECOND reason — ErrJudgeDisabled — and that one must still
  // surface: a user who turned the judge off has to see why nothing happened. Absorbing
  // every 409 would swallow it into a silent, pointless re-fetch.
  it("still surfaces the judge-disabled 409 rather than absorbing it (#119)", async () => {
    const { ApiError } = await import("../lib/api");
    mockApi.getRunReview.mockResolvedValue({ review: review(), pending_judge: null });
    mockApi.rerunJudge.mockRejectedValue(new ApiError(409, "run judging is disabled"));
    render(<JudgePanel run={run({ status: "completed" })} />);

    fireEvent.click(await screen.findByText("Re-run judge"));
    expect(await screen.findByText(/run judging is disabled/i)).toBeTruthy();
  });

  it("surfaces a non-409 re-run error", async () => {
    const { ApiError } = await import("../lib/api");
    mockApi.getRunReview.mockResolvedValue({ review: review(), pending_judge: null });
    mockApi.rerunJudge.mockRejectedValue(new ApiError(500, "internal error"));
    render(<JudgePanel run={run({ status: "completed" })} />);

    fireEvent.click(await screen.findByText("Re-run judge"));
    expect(await screen.findByText(/internal error/i)).toBeTruthy();
    // A failed enqueue leaves the action available — nothing is in flight.
    const btn = await screen.findByRole("button", { name: "Re-run judge" });
    expect((btn as HTMLButtonElement).disabled).toBe(false);
  });

  // The poll, on fake timers SCOPED to this test (every JudgePanel test that uses them
  // restores real timers in a finally). This is the happy path where the swap and the stop
  // coincide: one response carries both the new verdict and a cleared pending_judge. The
  // two are independent, though — a stop with no swap is the next case, and a swap with no
  // stop is "keeps polling while the judge is still active" below — so read this one as
  // "the verdict reaches the panel", not as "a landed verdict is what ends the poll" (it
  // is not: only a cleared pending_judge or the cap stops it).
  it("polls while a judge is pending and swaps to the verdict when it lands (#119)", async () => {
    vi.useFakeTimers();
    try {
      mockApi.getRunReview
        .mockResolvedValueOnce({ review: null, pending_judge: pending("running") })
        .mockResolvedValue({
          review: review({ updated_at: "2026-03-02T00:00:00Z" }),
          pending_judge: null,
        });
      render(<JudgePanel run={run({ status: "failed" })} />);
      await act(async () => {
        await vi.advanceTimersByTimeAsync(0);
      });
      expect(screen.getAllByText("Judge in progress…")).toHaveLength(2);

      await act(async () => {
        await vi.advanceTimersByTimeAsync(4000);
      });
      expect(screen.getByText("Issues found")).toBeTruthy();
      expect(screen.getByText("Lost time to a missing tool.")).toBeTruthy();
      // Pending cleared, so the action is offered again.
      const btn = screen.getByRole("button", { name: "Re-run judge" });
      expect((btn as HTMLButtonElement).disabled).toBe(false);
    } finally {
      vi.useRealTimers();
    }
  });

  it("stops polling when a judge leaves the active set without producing a review (#119)", async () => {
    vi.useFakeTimers();
    try {
      // A judge that fails/cancels: pending_judge clears and updated_at never moves,
      // because there is no review at all. Only the cleared pending can end this poll.
      mockApi.getRunReview
        .mockResolvedValueOnce({ review: null, pending_judge: pending("scheduled") })
        .mockResolvedValue({ review: null, pending_judge: null });
      render(<JudgePanel run={run({ status: "failed" })} />);
      await act(async () => {
        await vi.advanceTimersByTimeAsync(0);
      });
      expect(
        screen.getAllByText("Judge scheduled — the verdict will appear here when it finishes."),
      ).toHaveLength(2);

      await act(async () => {
        await vi.advanceTimersByTimeAsync(4000);
      });
      // Back to the genuine never-judged state, with a live button.
      expect(screen.getByText(/hasn't been judged yet/i)).toBeTruthy();
      expect((screen.getByRole("button", { name: "Run judge" }) as HTMLButtonElement).disabled).toBe(
        false,
      );

      // And the poll really stopped: no further fetches after the stop condition.
      const callsAfterStop = mockApi.getRunReview.mock.calls.length;
      await act(async () => {
        await vi.advanceTimersByTimeAsync(20_000);
      });
      expect(mockApi.getRunReview.mock.calls.length).toBe(callsAfterStop);
    } finally {
      vi.useRealTimers();
    }
  });

  // The reload-mid-re-judge journey, which is the headline case #119 exists for: the poll
  // now starts from SERVER truth, so it can begin on a mount that already has a verdict on
  // screen. The baseline the poll compares against therefore has to be seeded by the FETCH,
  // not only by a click — otherwise tick 1 compares the old, already-displayed verdict
  // against a null baseline, calls it "landed", and kills the interval on its first tick
  // while the judge that will actually replace it is still running.
  it("keeps polling across ticks that re-serve the SAME old verdict on a fresh mount (#119)", async () => {
    vi.useFakeTimers();
    try {
      const old = review({ updated_at: "2026-03-01T00:00:00Z", summary_md: "The old verdict." });
      mockApi.getRunReview.mockResolvedValue({ review: old, pending_judge: pending("running") });
      render(<JudgePanel run={run({ status: "completed" })} />);
      await act(async () => {
        await vi.advanceTimersByTimeAsync(0);
      });
      expect(screen.getByText("The old verdict.")).toBeTruthy();
      expect(
        (screen.getByRole("button", { name: "Judge running…" }) as HTMLButtonElement).disabled,
      ).toBe(true);

      // Three ticks that all re-serve the SAME verdict with the judge still active. The
      // poll must survive every one of them: nothing has changed since the mount fetch.
      await act(async () => {
        await vi.advanceTimersByTimeAsync(12_000);
      });
      expect(mockApi.getRunReview).toHaveBeenCalledTimes(4);
      expect(screen.getByText("The old verdict.")).toBeTruthy();

      // Now the judge finishes and the new verdict lands.
      mockApi.getRunReview.mockResolvedValue({
        review: review({ updated_at: "2026-03-02T00:00:00Z", summary_md: "The NEW verdict." }),
        pending_judge: null,
      });
      await act(async () => {
        await vi.advanceTimersByTimeAsync(4000);
      });
      expect(mockApi.getRunReview).toHaveBeenCalledTimes(5);
      expect(screen.getByText("The NEW verdict.")).toBeTruthy();
      expect(screen.queryByText("The old verdict.")).toBeNull();
      const btn = screen.getByRole("button", { name: "Re-run judge" });
      expect((btn as HTMLButtonElement).disabled).toBe(false);
      // Pending cleared, so the poll is done.
      await act(async () => {
        await vi.advanceTimersByTimeAsync(20_000);
      });
      expect(mockApi.getRunReview).toHaveBeenCalledTimes(5);
    } finally {
      vi.useRealTimers();
    }
  });

  // The ordering the API actually has: PostReview authorizes against the caller's STILL
  // ACTIVE judge run (workersvc/judge_review.go authorizeJudgeTrace), so the review row is
  // written BEFORE the judge leaves the active set — the worker reports completion later.
  // A tick landing in that window legitimately sees (fresh review, pending_judge non-null),
  // and it must swap the panel WITHOUT stopping: stopping there freezes a disabled
  // "Judge running…" button over the verdict that already arrived.
  it("swaps to a landed verdict but keeps polling while the judge is still active (#119)", async () => {
    vi.useFakeTimers();
    try {
      const fresh = review({ updated_at: "2026-03-02T00:00:00Z", summary_md: "The NEW verdict." });
      mockApi.getRunReview
        .mockResolvedValueOnce({ review: null, pending_judge: pending("running") })
        // Tick 1: the verdict is written, the judge run has not gone terminal yet.
        .mockResolvedValueOnce({ review: fresh, pending_judge: pending("running") })
        // Tick 2+: the worker's completion report landed.
        .mockResolvedValue({ review: fresh, pending_judge: null });
      render(<JudgePanel run={run({ status: "failed" })} />);
      await act(async () => {
        await vi.advanceTimersByTimeAsync(0);
      });
      expect(screen.getAllByText("Judge in progress…")).toHaveLength(2);

      await act(async () => {
        await vi.advanceTimersByTimeAsync(4000);
      });
      // The verdict swapped in…
      expect(mockApi.getRunReview).toHaveBeenCalledTimes(2);
      expect(screen.getByText("The NEW verdict.")).toBeTruthy();
      // …and the panel still reports the judge as active, because the server does.
      expect(
        (screen.getByRole("button", { name: "Judge running…" }) as HTMLButtonElement).disabled,
      ).toBe(true);

      // The poll did NOT stop on the landed verdict: tick 2 runs and sees pending clear.
      await act(async () => {
        await vi.advanceTimersByTimeAsync(4000);
      });
      expect(mockApi.getRunReview).toHaveBeenCalledTimes(3);
      const btn = screen.getByRole("button", { name: "Re-run judge" });
      expect((btn as HTMLButtonElement).disabled).toBe(false);

      // …and once pending cleared, it stopped.
      await act(async () => {
        await vi.advanceTimersByTimeAsync(20_000);
      });
      expect(mockApi.getRunReview).toHaveBeenCalledTimes(3);
    } finally {
      vi.useRealTimers();
    }
  });

  // ── The poll-try cap (#119) ───────────────────────────────────────────────────────────
  // The cap is the LAST stop condition: a judge that neither produces a verdict nor leaves
  // the active set would otherwise be polled every 4s for the life of the tab. In
  // production it is JUDGE_POLL_MAX_TRIES = 150 (× 4s ≈ 10 minutes, chosen because the old
  // 15-try (~1 min) cap gave up on judges that were still running). The two tests below
  // assert the EXACT stop boundary — 1 mount fetch + N ticks, then one more tick trips the
  // cap and no later tick fires — rather than "eventually stops", because an off-by-a-lot
  // cap still "stops".
  //
  // 🔴 THEY DRIVE THE CAP THROUGH THE `pollMaxTries` PROP AT 5, NOT THE PRODUCTION 150
  // (issue #227). The stop MECHANISM is identical at any cap, and exercising it at 5 costs
  // ~5 fake-timer turns instead of 149. That 149-turn chain — one `advanceTimersByTimeAsync(149
  // * 4000)` inside a single `it()`, each turn flushing a mocked promise + a React `act` —
  // was these two tests' whole cost: ~4228 ms mean in the full suite, >49000 ms under CPU
  // contention (PRD #103 M5 MR-C, `probes/prd-103-mrc-tester-tt/`), and it forced a per-test
  // 120000 timeout while the other 1658 tests kept the 20000 default. Splitting the file did
  // not help (solo was SLOWER); reducing the ITERATION COUNT is the fix, so with it gone the
  // per-test caps come off and these inherit the suite-wide 20000. The production value 150 is
  // no longer asserted through timers here — it is pinned directly by the timer-free test
  // below, so a change to the default still reddens the suite.
  it("pins the production poll-try cap default", () => {
    // Guards the value the mechanism tests deliberately do NOT drive (they use pollMaxTries=5).
    expect(JUDGE_POLL_MAX_TRIES).toBe(150);
  });

  it("stops after the poll-try cap when a pending judge never clears (#119)", async () => {
    vi.useFakeTimers();
    try {
      // ONE object, reused for every response: setPendingJudge with an identical reference
      // lets React bail out of the re-render, so this is ticks of poll, not re-renders.
      const stuck = { review: null, pending_judge: pending("running") };
      mockApi.getRunReview.mockResolvedValue(stuck);
      render(<JudgePanel run={run({ status: "failed" })} pollMaxTries={5} />);
      await act(async () => {
        await vi.advanceTimersByTimeAsync(0);
      });
      // The mount fetch, and nothing else yet.
      expect(mockApi.getRunReview).toHaveBeenCalledTimes(1);

      // One tick short of the cap (4 ticks → tries=4 < 5): still polling.
      await act(async () => {
        await vi.advanceTimersByTimeAsync(4 * 4000);
      });
      expect(mockApi.getRunReview).toHaveBeenCalledTimes(5);

      // The 5th tick (tries=5 >= 5) trips the cap.
      await act(async () => {
        await vi.advanceTimersByTimeAsync(4000);
      });
      expect(mockApi.getRunReview).toHaveBeenCalledTimes(6);

      // And it is really over — half an hour of ticks adds nothing. Without the cap the
      // interval would keep firing, because `polling` stays true while pendingJudge holds
      // its last non-null value: the effect never re-runs, so its cleanup never runs.
      await act(async () => {
        await vi.advanceTimersByTimeAsync(30 * 60_000);
      });
      expect(mockApi.getRunReview).toHaveBeenCalledTimes(6);
    } finally {
      vi.useRealTimers();
    }
  });

  // The same bound on the FETCH-FAILURE path, which is a separate `clearInterval` in the
  // `!next` branch — and one that could not have existed before #119. The effect used to
  // key on `queued` alone, so `setQueued(false)` re-ran the effect and the cleanup stopped
  // the timer. Now it keys on `polling = queued || pendingJudge !== null`, which stays TRUE
  // over a permanently-failing endpoint (pendingJudge keeps its last non-null value and no
  // response ever arrives to change it). The explicit clearInterval there is the ONLY thing
  // that ever stops a /review that fails forever.
  it("stops after the same poll-try cap when every poll tick FAILS (#119)", async () => {
    vi.useFakeTimers();
    try {
      // The mount fetch must SUCCEED and report a pending judge, or the poll never arms.
      mockApi.getRunReview
        .mockResolvedValueOnce({ review: null, pending_judge: pending("running") })
        .mockRejectedValue(new Error("network down"));
      render(<JudgePanel run={run({ status: "failed" })} pollMaxTries={5} />);
      await act(async () => {
        await vi.advanceTimersByTimeAsync(0);
      });
      expect(mockApi.getRunReview).toHaveBeenCalledTimes(1);
      // The in-flight state the panel is showing while the endpoint is dead — the reason
      // the failure is swallowed rather than banner-ed.
      expect(screen.getAllByText("Judge in progress…")).toHaveLength(2);

      await act(async () => {
        await vi.advanceTimersByTimeAsync(4 * 4000);
      });
      expect(mockApi.getRunReview).toHaveBeenCalledTimes(5);

      await act(async () => {
        await vi.advanceTimersByTimeAsync(4000);
      });
      expect(mockApi.getRunReview).toHaveBeenCalledTimes(6);

      // 🔴 This is the assertion that reddens if the `clearInterval` in the `!next` branch
      // is dropped: setQueued(false) alone does not stop this timer.
      await act(async () => {
        await vi.advanceTimersByTimeAsync(30 * 60_000);
      });
      expect(mockApi.getRunReview).toHaveBeenCalledTimes(6);
    } finally {
      vi.useRealTimers();
    }
  });

  // The rollout-skew normalization has TWO sites, and this is the POLL one. The mount-fetch
  // site is covered by "treats an absent pending_judge…" below; this covers the case where
  // the panel mounts against an api that HAS the key (so the poll arms) and a later tick is
  // served by a pod that does NOT — exactly what a rolling api deploy produces mid-poll.
  // Without `next.pending_judge ?? null` the poll writes `undefined` into state, and
  // `pendingJudge !== null` is true for undefined, so the button label walks into
  // `pendingJudge.state` and throws during render — AND the poll never stops, because
  // `nextPending === null` is false too.
  it("normalizes an absent pending_judge on a POLL tick, not just on the mount fetch (#119)", async () => {
    vi.useFakeTimers();
    try {
      mockApi.getRunReview.mockResolvedValueOnce({
        review: review({ updated_at: "2026-03-01T00:00:00Z", summary_md: "The old verdict." }),
        pending_judge: pending("running"),
      });
      render(<JudgePanel run={run({ status: "completed" })} />);
      await act(async () => {
        await vi.advanceTimersByTimeAsync(0);
      });
      expect(screen.getByText("The old verdict.")).toBeTruthy();
      expect(
        (screen.getByRole("button", { name: "Judge running…" }) as HTMLButtonElement).disabled,
      ).toBe(true);

      // The mid-poll response from the OLDER api: the key is absent entirely.
      mockApi.getRunReview.mockResolvedValue({
        review: review({ updated_at: "2026-03-02T00:00:00Z", summary_md: "The NEW verdict." }),
      } as unknown as Awaited<ReturnType<typeof api.getRunReview>>);
      await act(async () => {
        await vi.advanceTimersByTimeAsync(4000);
      });

      // It converged instead of throwing: verdict swapped, pending treated as cleared.
      expect(mockApi.getRunReview).toHaveBeenCalledTimes(2);
      expect(screen.getByText("The NEW verdict.")).toBeTruthy();
      expect(screen.queryByText("The old verdict.")).toBeNull();
      const btn = screen.getByRole("button", { name: "Re-run judge" });
      expect((btn as HTMLButtonElement).disabled).toBe(false);

      // …and an absent key is a CLEARED pending, so the poll stopped on it.
      await act(async () => {
        await vi.advanceTimersByTimeAsync(20_000);
      });
      expect(mockApi.getRunReview).toHaveBeenCalledTimes(2);
    } finally {
      vi.useRealTimers();
    }
  });

  // Rollout skew: api and web are separate Deployments, so a web pod can talk to an api
  // that predates the pending_judge key. The key is then ABSENT, destructuring to
  // undefined — and `undefined !== null` is true, so every `pendingJudge !== null` guard
  // walks straight into `pendingJudge.state` and throws during render. There is no
  // ErrorBoundary in web/src, so that TypeError unmounts the whole app over a missing
  // optional field. api/internal/uzicli/client.go handles the same skew on the CLI side.
  it("treats an absent pending_judge as no pending judge rather than throwing (#119)", async () => {
    mockApi.getRunReview.mockResolvedValue({ review: review() } as unknown as Awaited<
      ReturnType<typeof api.getRunReview>
    >);
    render(<JudgePanel run={run({ status: "completed" })} />);

    const btn = await screen.findByRole("button", { name: "Re-run judge" });
    expect((btn as HTMLButtonElement).disabled).toBe(false);
    expect(screen.getByText("Issues found")).toBeTruthy();
  });

  it("renders nothing for an ineligible kind (chat) and never fetches a review", async () => {
    const { container } = render(
      <JudgePanel run={run({ kind: "chat", status: "completed" })} />,
    );
    expect(container.textContent).toBe("");
    expect(mockApi.getRunReview).not.toHaveBeenCalled();
  });

  // Regression for the coordinate-key mismatch (web-ux blocking): a recommendation with a
  // PERSISTED filed link (from ReviewDTO.filed_issues on reload) must render the filed ROW
  // with the issue link, NOT the idle "File issue" button. Same-session smoke missed this
  // because the just-filed LOCAL state masked it; only a persisted link exercises coordKey.
  it("renders a persisted filed link as the filed row, not the idle button", async () => {
    mockApi.getRunReview.mockResolvedValue({
      pending_judge: null,
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
    expect(link.getAttribute("href")).toBe(
      "https://gitlab.example/vtmocanu/uzi/-/issues/71",
    );
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
      repo_claudemd_enabled: false,
      repo_devbox_opt_in: false,
      repo_fold_improve_uzi_backlog: false,
      pipeline: null,
      guardrail_blocked: false,
      docker_allowlisted: false,
      docker_blocked: false,
    };
  }
  function draftFixture(over: Partial<IssueDraft> = {}): IssueDraft {
    return {
      default_repo_id: "repo1",
      title: "Improve the reviewer: reviewer",
      description: "## What the judge found\n\n````\nrationale\n````",
      labels: ["uzi"],
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
    mockApi.getRunReview.mockResolvedValue({ review: review(), pending_judge: null });
    mockApi.listRepos.mockResolvedValue({
      repos: [repoOpt("repo1", "vtmocanu/uzi")],
    });
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
    const body = screen.getByDisplayValue(
      /What the judge found/,
    ) as HTMLTextAreaElement;
    expect(title.value).not.toMatch(/[\p{Cf}]/u);
    expect(body.value).not.toMatch(/[\p{Cf}]/u);
    expect(title.value).toBe("Improve the reviewer");
    // …and the markdown structure is intact: the strip spares \n, so the fenced template
    // the server built is still a template and not one run-on line.
    expect(body.value).toContain("## What the judge found");
    expect(body.value).toContain("\n");
  });

  it("opens the draft with templated fields on File issue click (state B)", async () => {
    mockApi.getRunReview.mockResolvedValue({ review: review(), pending_judge: null });
    mockApi.listRepos.mockResolvedValue({
      repos: [repoOpt("repo1", "vtmocanu/uzi")],
    });
    mockApi.getIssueDraft.mockResolvedValue({ draft: draftFixture() });
    render(<JudgePanel run={run({ status: "completed" })} />);

    fireEvent.click(await screen.findByText("File issue"));
    expect(mockApi.getIssueDraft).toHaveBeenCalledWith("r1", "rc1");
    expect(await screen.findByText("Draft issue")).toBeTruthy();
    // Provenance is prominent (Decision 8), the label is the server-assembled `uzi`
    // label (PRD #764), and the title is an editable field seeded from the draft.
    expect(screen.getByText(/from vlad's worker, run 8f2c1d04/)).toBeTruthy();
    expect(screen.getByText("uzi")).toBeTruthy();
    expect(
      (
        screen.getByDisplayValue(
          "Improve the reviewer: reviewer",
        ) as HTMLInputElement
      ).tagName,
    ).toBe("INPUT");
  });

  it("files the issue and shows the created link (state C)", async () => {
    mockApi.getRunReview.mockResolvedValue({ review: review(), pending_judge: null });
    mockApi.listRepos.mockResolvedValue({
      repos: [repoOpt("repo1", "vtmocanu/uzi")],
    });
    mockApi.getIssueDraft.mockResolvedValue({ draft: draftFixture() });
    mockApi.fileIssue.mockResolvedValue({
      issue: {
        iid: 71,
        web_url: "https://gitlab.example/vtmocanu/uzi/-/issues/71",
        title: "t",
      },
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
    expect(link.getAttribute("href")).toBe(
      "https://gitlab.example/vtmocanu/uzi/-/issues/71",
    );
  });

  it("disables Create until a repo is picked when no default resolves (state D)", async () => {
    mockApi.getRunReview.mockResolvedValue({ review: review(), pending_judge: null });
    mockApi.listRepos.mockResolvedValue({
      repos: [repoOpt("repo1", "vtmocanu/uzi")],
    });
    mockApi.getIssueDraft.mockResolvedValue({
      draft: draftFixture({
        default_repo_id: "",
        default_note: "No uzi repo is configured.",
      }),
    });
    render(<JudgePanel run={run({ status: "completed" })} />);

    fireEvent.click(await screen.findByText("File issue"));
    const create = (await screen.findByText(
      "Create issue",
    )) as HTMLButtonElement;
    expect(create.disabled).toBe(true);
    expect(screen.getByText("No uzi repo is configured.")).toBeTruthy();
    // Picking a repo enables Create.
    fireEvent.change(screen.getByRole("combobox"), {
      target: { value: "repo1" },
    });
    expect(
      (screen.getByText("Create issue") as HTMLButtonElement).disabled,
    ).toBe(false);
  });

  it("keeps the draft open and shows the error when the forge rejects (state E)", async () => {
    const { ApiError } = await import("../lib/api");
    mockApi.getRunReview.mockResolvedValue({ review: review(), pending_judge: null });
    mockApi.listRepos.mockResolvedValue({
      repos: [repoOpt("repo1", "vtmocanu/uzi")],
    });
    mockApi.getIssueDraft.mockResolvedValue({ draft: draftFixture() });
    mockApi.fileIssue.mockRejectedValue(
      new ApiError(502, "the forge rejected the request (403)"),
    );
    render(<JudgePanel run={run({ status: "completed" })} />);

    fireEvent.click(await screen.findByText("File issue"));
    fireEvent.click(await screen.findByText("Create issue"));
    expect(await screen.findByText(/forge rejected the request/i)).toBeTruthy();
    // The draft stays open with its fields intact (not collapsed to the filed row).
    expect(
      screen.getByDisplayValue("Improve the reviewer: reviewer"),
    ).toBeTruthy();
  });

  it("flags a stale filed link (filed before the current review revision)", async () => {
    mockApi.getRunReview.mockResolvedValue({
      pending_judge: null,
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
    expect(
      await screen.findByText(/filed for an earlier version/i),
    ).toBeTruthy();
  });

  it("recovers from a draft-load failure with Retry and Cancel (no dead end)", async () => {
    const { ApiError } = await import("../lib/api");
    mockApi.getRunReview.mockResolvedValue({ review: review(), pending_judge: null });
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
      pending_judge: null,
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
          {
            category: "install_worker_tool",
            target: "shellcheck",
            status: "done",
            reason: "",
            set_at: "2026-01-01T00:00:00Z",
            stale: false,
          },
          {
            category: "improve_agent",
            target: "reviewer",
            status: "dismissed",
            reason: "wont_do",
            set_at: "2026-01-01T00:00:00Z",
            stale: false,
          },
          {
            category: "improve_uzi",
            target: "timeout",
            status: "dismissed",
            reason: "not_an_issue",
            set_at: "2026-01-01T00:00:00Z",
            stale: false,
          },
        ],
        triage: {
          total: 5,
          todo: 1,
          filed: 1,
          done: 1,
          dismissed: 2,
          false_positives: 1,
        },
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
    mockApi.getRunReview.mockResolvedValue({ review: review(), pending_judge: null });
    render(<JudgePanel run={run({ status: "completed" })} />);

    fireEvent.click(await screen.findByText("Mark done"));
    await waitFor(() =>
      expect(mockApi.setDisposition).toHaveBeenCalledWith("r1", "rc1", "done"),
    );
    // Refetch: getRunReview ran on mount AND again after the mutation.
    await waitFor(() =>
      expect(mockApi.getRunReview.mock.calls.length).toBeGreaterThanOrEqual(2),
    );
  });

  it("Dismiss → Not an issue sets a not_an_issue dismissal", async () => {
    mockApi.getRunReview.mockResolvedValue({ review: review(), pending_judge: null });
    render(<JudgePanel run={run({ status: "completed" })} />);

    fireEvent.click(await screen.findByText("Dismiss ▾"));
    fireEvent.click(await screen.findByText("Not an issue"));
    await waitFor(() =>
      expect(mockApi.setDisposition).toHaveBeenCalledWith(
        "r1",
        "rc1",
        "dismissed",
        "not_an_issue",
      ),
    );
  });

  it("Undo clears the disposition via deleteDisposition and refetches", async () => {
    mockApi.getRunReview.mockResolvedValue({
      pending_judge: null,
      review: review({
        dispositions: [
          {
            category: "install_worker_tool",
            target: "shellcheck",
            status: "done",
            reason: "",
            set_at: "2026-01-01T00:00:00Z",
            stale: false,
          },
        ],
        triage: {
          total: 1,
          todo: 0,
          filed: 0,
          done: 1,
          dismissed: 0,
          false_positives: 0,
        },
      }),
    });
    render(<JudgePanel run={run({ status: "completed" })} />);

    fireEvent.click(await screen.findByText("Undo"));
    await waitFor(() =>
      expect(mockApi.deleteDisposition).toHaveBeenCalledWith("r1", "rc1"),
    );
    await waitFor(() =>
      expect(mockApi.getRunReview.mock.calls.length).toBeGreaterThanOrEqual(2),
    );
  });

  it("renders the stale note straight from the DTO's stale flag", async () => {
    mockApi.getRunReview.mockResolvedValue({
      pending_judge: null,
      review: review({
        dispositions: [
          {
            category: "install_worker_tool",
            target: "shellcheck",
            status: "done",
            reason: "",
            set_at: "2026-01-01T00:00:00Z",
            stale: true,
          },
        ],
        triage: {
          total: 1,
          todo: 0,
          filed: 0,
          done: 1,
          dismissed: 0,
          false_positives: 0,
        },
      }),
    });
    render(<JudgePanel run={run({ status: "completed" })} />);
    expect(
      await screen.findByText(/recommendation changed since you resolved/i),
    ).toBeTruthy();
  });

  it("renders the triage bar from server counts, never re-derived from the on-screen rows", async () => {
    // ONE recommendation is rendered, but the server triage totals 5 — so the bar's
    // numbers can only come from review.triage, proving no TS re-derivation.
    mockApi.getRunReview.mockResolvedValue({
      pending_judge: null,
      review: review({
        triage: {
          total: 5,
          todo: 2,
          filed: 1,
          done: 1,
          dismissed: 1,
          false_positives: 1,
        },
      }),
    });
    render(<JudgePanel run={run({ status: "completed" })} />);

    expect(await screen.findByText("3 of 5 handled")).toBeTruthy(); // filed+done+dismissed of total
    expect(screen.getByText("2")).toBeTruthy(); // the server todo count, though 1 row shows
    expect(
      screen.getByText(/1 of 1 dismissed was a false positive/),
    ).toBeTruthy();
  });

  it("collapse-dismissed toggle hides the dismissed rows", async () => {
    mockApi.getRunReview.mockResolvedValue({
      pending_judge: null,
      review: review({
        recommendations: [
          rec("rc1", "install_worker_tool", "shellcheck", "keep me"),
          rec("rc2", "improve_agent", "reviewer", "hide me"),
        ],
        dispositions: [
          {
            category: "improve_agent",
            target: "reviewer",
            status: "dismissed",
            reason: "wont_do",
            set_at: "2026-01-01T00:00:00Z",
            stale: false,
          },
        ],
        triage: {
          total: 2,
          todo: 1,
          filed: 0,
          done: 0,
          dismissed: 1,
          false_positives: 0,
        },
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
      pending_judge: null,
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
          {
            category: "add_agent",
            target: "deploy-agent",
            status: "done",
            reason: "",
            set_at: "2026-01-01T00:00:00Z",
            stale: false,
          },
        ],
        triage: {
          total: 1,
          todo: 0,
          filed: 0,
          done: 1,
          dismissed: 0,
          false_positives: 0,
        },
      }),
    });
    render(<JudgePanel run={run({ status: "completed" })} />);

    // Done wins the chip ladder…
    expect(await screen.findByText("Done")).toBeTruthy();
    // …but the filed issue link survives the disposition (Resolved Q: file then done).
    const link = screen.getByRole("link", { name: /#72/ });
    expect(link.getAttribute("href")).toBe(
      "https://gitlab.example/x/-/issues/72",
    );
    // No way to file a second issue on a disposed row.
    expect(screen.queryByText("File issue")).toBeNull();
  });

  it("Escape closes the Dismiss menu and returns focus to the trigger (a11y)", async () => {
    mockApi.getRunReview.mockResolvedValue({ review: review(), pending_judge: null });
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
      .mockResolvedValueOnce({ review: review(), pending_judge: null })
      .mockResolvedValue({
        pending_judge: null,
        review: review({
          dispositions: [
            {
              category: "install_worker_tool",
              target: "shellcheck",
              status: "done",
              reason: "",
              set_at: "2026-01-01T00:00:00Z",
              stale: false,
            },
          ],
          triage: {
            total: 1,
            todo: 0,
            filed: 0,
            done: 1,
            dismissed: 0,
            false_positives: 0,
          },
        }),
      });
    render(<JudgePanel run={run({ status: "completed" })} />);

    fireEvent.click(await screen.findByText("Mark done"));
    const undo = await screen.findByText("Undo");
    await waitFor(() => expect(document.activeElement).toBe(undo));
  });

  it("announces the mutation result via the polite live region (a11y)", async () => {
    mockApi.getRunReview.mockResolvedValue({ review: review(), pending_judge: null });
    render(<JudgePanel run={run({ status: "completed" })} />);

    fireEvent.click(await screen.findByText("Mark done"));
    expect(await screen.findByText("Marked done")).toBeTruthy();
  });
});

// ── Plan revision at the gate (PRD #41) ────────────────────────────────────────
function planMsg(seq: number, kind: string, payload: unknown): RunMessage {
  return {
    seq,
    kind,
    agent: "lead",
    agent_instance: null,
    agent_label: null,
    payload,
    created_at: "2026-07-04T00:00:00.000Z",
  };
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
    expect(
      (getByRole("button", { name: /send & revise/i }) as HTMLButtonElement)
        .disabled,
    ).toBe(true);
  });

  it("renders the revising parked state when latest-by-seq is plan_revising", () => {
    const messages = [
      planMsg(1, "plan", { plan_md: "v1 body" }),
      planMsg(2, "plan_feedback", { feedback: "rework the endpoint" }),
      planMsg(3, "plan_revising", { round: 1 }),
    ];
    const onCancel = vi.fn();
    const { container, getByRole, queryByRole } = renderGate({
      messages,
      onCancel,
    });
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
      run: {
        repo_agents: [],
        own_agents: [],
        plan_md: "# Plan v2\n- new step",
      },
      messages,
    });
    expect(getByRole("button", { name: /approve plan/i })).toBeTruthy();
    expect(container.textContent).toContain(
      "Updated plan awaiting your approval",
    );
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
describe("PlanPanel canSteer (PRE-EXISTING hole, fixed alongside PRD #88)", () => {
  // POST /inputs is user-scoped, so a non-owner admin — who can legitimately OPEN this
  // owner-or-admin run view — had an Approve button that 404s. useRunStream states the
  // rule outright ("never a broken Send that 404s") and SteerQueueCard already obeyed it;
  // this panel never did. Unrelated to #88 and fixed with it, because fixing only the
  // newer question composer would have left the identical hole one panel over.
  const gated = () =>
    render(
      <PlanPanel
        run={run({ repo_agents: [agent("coder")], own_agents: [agent("coder")] })}
        busy={false}
        canSteer={false}
        onApprove={() => {}}
        onReject={() => {}}
        onRequestChanges={() => {}}
        onCancel={() => {}}
      />,
    );

  it("hides every verdict control from a non-owner", () => {
    gated();
    expect(screen.queryByRole("button", { name: /Approve plan/ })).toBeNull();
    expect(screen.queryByRole("button", { name: "Request changes" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Reject" })).toBeNull();
  });

  it("does not contradict itself: the HEADING follows canSteer like the subtitle", () => {
    // Measured in the browser: a non-owner saw "Plan awaiting your approval" directly
    // above "Only they can approve or reject it" — the subtitle was conditional and the
    // heading was not. QuestionPanel's non-owner branch changes both; this is the older
    // panel's half of the same fix, left incomplete.
    gated();
    expect(screen.getByText(/Plan awaiting the owner's approval/)).toBeTruthy();
    expect(screen.queryByText(/awaiting your approval/)).toBeNull();
  });

  it("keeps the REVISED heading viewer-aware too", () => {
    // A v2+ re-gate has its own heading string, so gating only the first would put the
    // contradiction straight back for any run that was revised once.
    render(
      <PlanPanel
        run={run({ repo_agents: [], own_agents: [] })}
        messages={[
          { seq: 1, kind: "plan", agent: "lead", agent_instance: null, agent_label: null, payload: { plan_md: "a" }, created_at: "2026-07-28T00:00:00Z" },
          { seq: 2, kind: "plan", agent: "lead", agent_instance: null, agent_label: null, payload: { plan_md: "b" }, created_at: "2026-07-28T00:00:02Z" },
        ]}
        busy={false}
        canSteer={false}
        onApprove={() => {}}
        onReject={() => {}}
      />,
    );
    expect(screen.getByText(/Updated plan awaiting the owner's approval/)).toBeTruthy();
    expect(screen.queryByText(/awaiting your approval/)).toBeNull();
  });

  it("keeps the plan READABLE — the reason a non-owner admin opens this page", () => {
    // Hiding the panel outright would turn a permissions boundary into a blank page.
    gated();
    expect(screen.getByText("step one")).toBeTruthy();
    expect(screen.getByText(/Only they can approve or reject it/)).toBeTruthy();
  });

  it("hides the agent picker, which exists only to shape a verdict they cannot cast", () => {
    gated();
    expect(screen.queryByRole("button", { name: /^●?\s*coder/i })).toBeNull();
  });

  it("defaults to steerable, so an owner is never gated by an absent prop", async () => {
    render(
      <PlanPanel
        run={run({ repo_agents: [agent("coder")], own_agents: [agent("coder")] })}
        busy={false}
        onApprove={() => {}}
        onReject={() => {}}
      />,
    );
    expect(await screen.findByRole("button", { name: /Approve plan/ })).toBeTruthy();
  });

  it("hides Cancel run from a non-owner in the REVISING state too", () => {
    // A second branch with its own controls; gating only the open gate would leave the
    // revising panel offering a Cancel that 404s.
    render(
      <PlanPanel
        run={run({ repo_agents: [], own_agents: [] })}
        messages={[
          { seq: 1, kind: "plan", agent: "lead", agent_instance: null, agent_label: null, payload: { plan_md: "p" }, created_at: "2026-07-28T00:00:00Z" },
          { seq: 2, kind: "plan_revising", agent: "lead", agent_instance: null, agent_label: null, payload: { round: 1 }, created_at: "2026-07-28T00:00:01Z" },
        ]}
        busy={false}
        canSteer={false}
        onApprove={() => {}}
        onReject={() => {}}
        onCancel={() => {}}
      />,
    );
    expect(screen.getByText("Revising the plan")).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Cancel run" })).toBeNull();
  });
});

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

// PRD #35: the usage-limit strip on the run view.
describe("LimitWaitPanel (PRD #35)", () => {
  // A fixed clock: the countdown's whole job is arithmetic against now, and a test
  // that reads the real clock either tolerates a range (and stops discriminating) or
  // flakes on a second boundary.
  const NOW = Date.UTC(2026, 6, 27, 12, 0, 0);
  const ahead = (ms: number) => new Date(NOW + ms).toISOString();

  beforeEach(() => {
    vi.useFakeTimers();
    vi.setSystemTime(NOW);
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  // The parked shape, with the two instants DELIBERATELY different — see the
  // countdown case below for why every field here is load-bearing.
  const parked = (over: Partial<Run> = {}) =>
    run({
      status: "limit_wait",
      wait_on_limit: true,
      retry_not_before: ahead(2 * 3_600_000 + 14 * 60_000), // 2h 14m
      limit_resets_at: ahead(5 * 3_600_000), // 5h — LATER, and not what counts down
      rate_limit_type: "five_hour",
      limit_wait_count: 2,
      ...over,
    });

  it("renders NOTHING for a terminal run — there is no future limit to opt into", () => {
    for (const status of ["completed", "failed", "cancelled"] as const) {
      cleanup();
      const { container } = render(
        <LimitWaitPanel
          run={run({ status })}
          busy={false}
          onToggle={() => {}}
          onStop={() => {}}
        />,
      );
      // Not "a disabled checkbox": a control that cannot do anything only invites the
      // question of why it is there.
      expect(container.textContent).toBe("");
      expect(container.querySelector("input")).toBeNull();
    }
  });

  it("shows only the quiet toggle for a live, never-parked run", () => {
    const { container } = render(
      <LimitWaitPanel
        run={run({ status: "running" })}
        busy={false}
        onToggle={() => {}}
        onStop={() => {}}
      />,
    );
    expect(container.querySelector("input[type=checkbox]")).not.toBeNull();
    expect(container.textContent).toContain(
      "Wait out future Anthropic usage limits",
    );
    // No park has happened, so nothing may claim one has.
    expect(container.textContent).not.toContain("Paused");
    expect(container.textContent).not.toContain("Resumes in");
  });

  it("🔴 counts down to retry_not_before, NOT to limit_resets_at", () => {
    // These are different instants and the gap is not an offset: retry_not_before
    // carries jitter, is clamped, is cross-checked against the owner's gauge, and is
    // POOL-AWARE — an owner whose second credential still has headroom is promoted
    // before the dead credential's window rolls over, so the stamp is routinely
    // EARLIER. A countdown wired to limit_resets_at would tell the user to wait 5h
    // for a run that comes back in 2h 14m, and would look entirely plausible.
    const { container } = render(
      <LimitWaitPanel
        run={parked()}
        busy={false}
        onToggle={() => {}}
        onStop={() => {}}
      />,
    );
    expect(container.textContent).toContain("2h 14m");
    expect(container.textContent).not.toContain("5h 0m");
  });

  it("renders limit_resets_at as CONTEXT, named as its window", () => {
    const { container } = render(
      <LimitWaitPanel
        run={parked()}
        busy={false}
        onToggle={() => {}}
        onStop={() => {}}
      />,
    );
    expect(container.textContent).toContain("5-hour window reopens");
    // Formatted WITHOUT seconds (web-ux nit): toLocaleString() renders "7:00:00 PM"
    // for an instant known only to within a poll interval, so the precision is a
    // lie as well as noise.
    expect(container.textContent).toContain(
      new Date(ahead(5 * 3_600_000)).toLocaleString(undefined, {
        dateStyle: "medium",
        timeStyle: "short",
      }),
    );
    expect(container.textContent).not.toMatch(/\d:\d\d:\d\d/);
  });

  it("🔴 explains the gap when the run resumes BEFORE its window reopens", () => {
    // web-ux F4. Rendered plainly the two numbers read as a contradiction: "resumes in
    // 2h 14m" beside a window reopening ~3h later. The code was right and nothing said
    // why, so a user who notices concludes one number is wrong.
    //
    // The claim is the RULE, not a cause: retry_not_before means "the earliest moment
    // this user could spend anything". Naming a second credential would be a guess —
    // no DTO field says which leg of the computation won.
    const { container } = render(
      <LimitWaitPanel
        run={parked()}
        busy={false}
        onToggle={() => {}}
        onStop={() => {}}
      />,
    );
    expect(container.textContent).toMatch(
      /sooner than the 5-hour window reopens/,
    );
    expect(container.textContent).toMatch(
      /as soon as any of your tokens can pay for it/,
    );
  });

  it("does NOT explain a gap that does not exist — the ordering can go either way", () => {
    // The stamp starts at max(worker reset, gauge reset), so jitter and the
    // cross-check push it LATER; only an alternative credential pulls it earlier. A
    // panel that always printed "sooner than…" would be wrong half the time, so this
    // is derived from the values rather than assumed from the feature.
    const { container } = render(
      <LimitWaitPanel
        run={parked({ retry_not_before: ahead(6 * 3_600_000) })}
        busy={false}
        onToggle={() => {}}
        onStop={() => {}}
      />,
    );
    expect(container.textContent).not.toMatch(/sooner than/);
    expect(container.textContent).toContain("The 5-hour window reopens");
  });

  it("keeps the attempt clause OUT of the explanatory sentence", () => {
    // The first version joined window and attempt with " · " before composing, which
    // put "attempt 2" between "reopens <date>" and "because uzi resumes…" — one
    // sentence split around an unrelated fact.
    const { container } = render(
      <LimitWaitPanel
        run={parked()}
        busy={false}
        onToggle={() => {}}
        onStop={() => {}}
      />,
    );
    expect(container.textContent).toMatch(/pay for it\.\s*Attempt 2\./);
  });

  it("🔴 offers Stop IN the panel, not 600px below it", () => {
    // web-ux F1: the only pointer to the real control was "Stop it if you would rather
    // not wait" at 4.56 contrast, and `Stop run` lived past the whole activity card.
    // Meanwhile the page's one FILLED primary button was `Send follow-up`, which on a
    // parked run promises the opposite of what it does.
    const onStop = vi.fn();
    render(
      <LimitWaitPanel
        run={parked()}
        busy={false}
        onToggle={() => {}}
        onStop={onStop}
      />,
    );
    const stop = screen.getByRole("button", { name: /stop run/i });
    fireEvent.click(stop);
    expect(onStop).toHaveBeenCalledTimes(1);
  });

  it("does not offer Stop on a live, never-parked run — the steer card owns that", () => {
    render(
      <LimitWaitPanel
        run={run({ status: "running" })}
        busy={false}
        onToggle={() => {}}
        onStop={() => {}}
      />,
    );
    expect(screen.queryByRole("button", { name: /stop run/i })).toBeNull();
  });

  it("says 'Resuming shortly' once the clock has passed, never a negative countdown", () => {
    // The promotion pass runs on a ticker, so an expired stamp means "waiting on the
    // next sweep", not "late". Counting up would read as a fault where there is none.
    const { container } = render(
      <LimitWaitPanel
        run={parked({ retry_not_before: ahead(-90_000) })}
        busy={false}
        onToggle={() => {}}
        onStop={() => {}}
      />,
    );
    expect(container.textContent).toContain("Resuming shortly");
    // Targeted at a NEGATIVE DURATION, not at the character: "5-hour window" and the
    // ISO-ish date both carry hyphens, so a bare not.toContain("-") passes and fails
    // for reasons that have nothing to do with the countdown.
    expect(container.textContent).not.toMatch(/-\d+[smhd]\b/);
    expect(container.textContent).not.toContain("Resumes in");
    // Still a park, so the heading and the reassurance stay.
    expect(container.textContent).toContain(
      "Paused on an Anthropic usage limit",
    );
  });

  it("ticks the countdown down as time passes", () => {
    const { container } = render(
      <LimitWaitPanel
        run={parked({ retry_not_before: ahead(65_000) })}
        busy={false}
        onToggle={() => {}}
        onStop={() => {}}
      />,
    );
    expect(container.textContent).toContain("1m 05s");
    act(() => {
      vi.advanceTimersByTime(10_000);
    });
    expect(container.textContent).toContain("55s");
  });

  it("shows 'attempt N' only from the second park", () => {
    const { container } = render(
      <LimitWaitPanel
        run={parked()}
        busy={false}
        onToggle={() => {}}
        onStop={() => {}}
      />,
    );
    // Case-insensitive on the word, exact on the number: it is capitalised now that it
    // stands as its own sentence rather than riding a " · " join.
    expect(container.textContent).toMatch(/attempt 2/i);
    cleanup();
    const first = render(
      <LimitWaitPanel
        run={parked({ limit_wait_count: 1 })}
        busy={false}
        onToggle={() => {}}
        onStop={() => {}}
      />,
    );
    expect(first.container.textContent).not.toContain("attempt");
  });

  it("🔴 keeps the toggle LIVE on a parked run, and flipping it off reports only the flag", () => {
    // The semantic trap this whole control has: the name reads like a cancel. It is
    // not. Flipping it off changes what happens at the NEXT limit; this park keeps its
    // clock and the run still resumes. So the checkbox must be present, enabled, and
    // must hand the caller nothing but the new boolean.
    const onToggle = vi.fn();
    const { container } = render(
      <LimitWaitPanel
        run={parked()}
        busy={false}
        onToggle={onToggle}
        onStop={() => {}}
      />,
    );
    const box = container.querySelector(
      "input[type=checkbox]",
    ) as HTMLInputElement;
    expect(box).not.toBeNull();
    expect(box.disabled).toBe(false);
    expect(box.checked).toBe(true);
    fireEvent.click(box);
    expect(onToggle).toHaveBeenCalledTimes(1);
    expect(onToggle).toHaveBeenCalledWith(false);
    // The panel does not decide anything itself — the run is still parked, still
    // counting down, and nothing in the surface suggests it was cancelled or stopped.
    expect(container.textContent).toContain("2h 14m");
    expect(container.textContent).not.toMatch(/cancel|stopp?ed|abort/i);
  });

  it("reflects wait_on_limit=false as an unchecked box on a run that parked anyway", () => {
    // Reachable: the owner turned it off mid-park. The box must follow the flag, not
    // the status, or the user cannot tell that their change landed.
    const { container } = render(
      <LimitWaitPanel
        run={parked({ wait_on_limit: false })}
        busy={false}
        onToggle={() => {}}
        onStop={() => {}}
      />,
    );
    const box = container.querySelector(
      "input[type=checkbox]",
    ) as HTMLInputElement;
    expect(box.checked).toBe(false);
    expect(container.textContent).toContain(
      "Paused on an Anthropic usage limit",
    );
  });

  it("disables the box while a write is in flight", () => {
    const { container } = render(
      <LimitWaitPanel
        run={parked()}
        busy
        onToggle={() => {}}
        onStop={() => {}}
      />,
    );
    expect(
      (container.querySelector("input[type=checkbox]") as HTMLInputElement)
        .disabled,
    ).toBe(true);
  });

  it("announces the park politely rather than silently", () => {
    const { container } = render(
      <LimitWaitPanel
        run={parked()}
        busy={false}
        onToggle={() => {}}
        onStop={() => {}}
      />,
    );
    expect(container.querySelector('[role="status"]')?.textContent).toContain(
      "Paused on an Anthropic usage limit",
    );
  });

  it("degrades honestly when the server sent no timestamps at all", () => {
    // A pre-feature row, or a park whose stamps did not survive. It must not render
    // "NaN", "Invalid Date", or a fabricated time.
    const { container } = render(
      <LimitWaitPanel
        run={parked({
          retry_not_before: null,
          limit_resets_at: null,
          rate_limit_type: null,
        })}
        busy={false}
        onToggle={() => {}}
        onStop={() => {}}
      />,
    );
    expect(container.textContent).toContain("Resuming shortly");
    expect(container.textContent).not.toContain("NaN");
    expect(container.textContent).not.toContain("Invalid");
  });

  // Issue #183: a NON-OWNER (canSteer=false) can open this owner-or-admin run view, but
  // POST /inputs is user-scoped, so a Stop or a wait-on-limit write would 404. Both
  // controls are HIDDEN and replaced by inert text — never a greyed button that lies —
  // mirroring PlanPanel/QuestionPanel's non-owner branches.
  it("🔴 shows inert text and NO clickable controls for a non-owner on a parked run (#183)", () => {
    const onStop = vi.fn();
    const onToggle = vi.fn();
    const { container } = render(
      <LimitWaitPanel
        run={parked()}
        busy={false}
        canSteer={false}
        onToggle={onToggle}
        onStop={onStop}
      />,
    );
    // No live Stop button and no wait-on-limit checkbox.
    expect(screen.queryByRole("button", { name: /stop run/i })).toBeNull();
    expect(container.querySelector("input[type=checkbox]")).toBeNull();
    // The park still reads for everyone — heading and countdown are unchanged…
    expect(container.textContent).toContain("Paused on an Anthropic usage limit");
    expect(container.textContent).toContain("2h 14m");
    // …and the two controls become inert statements of who can act.
    expect(container.textContent).toMatch(/only its owner can change this/i);
    expect(container.textContent).toMatch(/only the run's owner can stop it/i);
  });

  it("hides the non-owner toggle on a live, never-parked run too (#183)", () => {
    // The faint-toggle (non-parked) branch is gated by the SAME shared `toggle`, so it
    // must not leave a clickable checkbox for a non-owner either.
    const { container } = render(
      <LimitWaitPanel
        run={run({ status: "running" })}
        busy={false}
        canSteer={false}
        onToggle={() => {}}
        onStop={() => {}}
      />,
    );
    expect(container.querySelector("input[type=checkbox]")).toBeNull();
    expect(screen.queryByRole("button", { name: /stop run/i })).toBeNull();
    expect(container.textContent).toMatch(/only its owner can change this/i);
  });

  it("keeps the LIVE toggle and Stop for an owner (canSteer default true)", () => {
    // The positive control for the two absence checks above: with canSteer defaulting to
    // true, an owner still gets the real checkbox and Stop button.
    const { container } = render(
      <LimitWaitPanel
        run={parked()}
        busy={false}
        onToggle={() => {}}
        onStop={() => {}}
      />,
    );
    expect(container.querySelector("input[type=checkbox]")).not.toBeNull();
    expect(screen.getByRole("button", { name: /stop run/i })).toBeTruthy();
    expect(container.textContent).not.toMatch(/only the run's owner can stop it/i);
  });
});

describe("RunView ↔ LimitWaitPanel wiring (PRD #35)", () => {
  // Same comment-stripping control as the RunCredential wiring above, and the same
  // acknowledged ceiling: this proves the text is present and runs, not that it
  // renders — `{false && …}` would still pass. The panel's own behaviour is covered
  // by the describe above, which renders it for real.
  const live = runViewSource
    .replace(/\{\/\*[\s\S]*?\*\/\}/g, "")
    .replace(/\/\*[\s\S]*?\*\//g, "")
    .replace(/^\s*\/\/.*$/gm, "");

  it("renders the panel on the run page", () => {
    expect(live).toContain("<LimitWaitPanel");
  });

  it("wires the toggle to the run-scoped endpoint, not to a cancel", () => {
    expect(live).toContain("api.setRunWaitOnLimit(run.id, enabled)");
  });

  it("refetches the run after the write", () => {
    // The flag is not a status change, so no WS frame announces it. Without the
    // refetch the checkbox snaps back to the stale run on the next render, which
    // reads exactly like the write having failed.
    expect(live).toMatch(/setRunWaitOnLimit[\s\S]{0,200}refreshRun\(\)/);
  });

  it("threads canSteer to the panel, so a non-owner is not offered a Stop that 404s (#183)", () => {
    // Same pattern as PlanPanel/QuestionPanel (MR !149): canSteer is destructured from
    // useRunStream at the call site and must reach LimitWaitPanel, or the non-owner
    // inert-text branch is unreachable from the page.
    const panel = live.indexOf("<LimitWaitPanel");
    expect(panel).toBeGreaterThan(-1);
    expect(live.slice(panel, panel + 400)).toContain("canSteer={canSteer}");
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
    const statusGate = live.indexOf('run.status === "awaiting_input" &&');
    const questionGate = live.indexOf("openQuestion ? (");
    const panel = live.indexOf("<QuestionPanel");
    expect(statusGate).toBeGreaterThan(-1);
    expect(questionGate).toBeGreaterThan(statusGate);
    expect(panel).toBeGreaterThan(questionGate);
  });

  it("renders the UNREADABLE branch when the status is parked but the payload is not", () => {
    // The else arm of that same conditional. Without it a parked run with an unusable
    // payload renders nothing at all — no panel, no explanation — until the deadline.
    const questionGate = live.indexOf("openQuestion ? (");
    const fallback = live.indexOf("<UnreadableQuestion");
    expect(fallback).toBeGreaterThan(questionGate);
    expect(live).toContain('from "../components/QuestionPanel"');
  });

  it("passes canSteer to BOTH gate panels, so neither offers a Send that 404s", () => {
    // PlanPanel's hole is PRE-EXISTING and unrelated to #88; fixing only the question
    // composer would have left the identical hole one panel over.
    const planPanel = live.indexOf("<PlanPanel");
    const questionPanel = live.indexOf("<QuestionPanel");
    expect(live.slice(planPanel, planPanel + 400)).toContain("canSteer={canSteer}");
    expect(live.slice(questionPanel, questionPanel + 400)).toContain("canSteer={canSteer}");
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
    const panelGate = live.indexOf('run.status === "awaiting_input" &&');
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

// PRD #517 (a11y): a run parking into awaiting_followup is a needs-you state classified
// identically by needsHumanAttention, so it must announce too — not only awaiting_input.
// These render the whole page (useRunStream mocked) and read the ALWAYS-MOUNTED sr-only
// region's CONTENT, which is the actual fix (a region born holding text is silent; the
// content change is what assistive tech narrates). The parkAnnounce region is a <div>;
// HealthFlag's is a <span>, so `div.sr-only[role="status"]` targets parkAnnounce uniquely.
describe("RunView park announcement — awaiting_followup (PRD #517, a11y)", () => {
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
    mockApi.getRunReview.mockResolvedValue({ review: null, pending_judge: null });
    return render(
      <MemoryRouter initialEntries={["/runs/r1"]}>
        <RunView />
      </MemoryRouter>,
    );
  }

  it("announces the follow-up park through the always-mounted sr-only region", async () => {
    renderPage({ status: "awaiting_followup" });
    const region = await waitFor(() => {
      const el = document.querySelector('div.sr-only[role="status"]') as HTMLElement | null;
      if (!el || el.textContent === "") throw new Error("not announced yet");
      return el;
    });
    expect(region.getAttribute("aria-live")).toBe("polite");
    expect(region.textContent).toBe("The run is waiting for your next follow-up.");
    // Mutation guard: it is the follow-up copy, NOT the awaiting_input question copy.
    expect(region.textContent).not.toContain("asking you a question");
  });

  it("leaves the region empty on a plain running run (nothing to announce)", async () => {
    renderPage({ status: "running" });
    await screen.findByText("Add rate limiting");
    const region = document.querySelector('div.sr-only[role="status"]') as HTMLElement;
    expect(region.textContent).toBe("");
  });
});

// PRD #122: the milestone header badge, the checklist, and the plan-gate candidate list.
describe("MilestoneBadge (compact M{done}/{total}, PRD #122)", () => {
  const ms = (n: number) =>
    Array.from({ length: n }, (_, i) => ({ id: `m${i + 1}`, title: `Milestone ${i + 1}` }));

  it("renders M{done}/{total} for a milestone-structured run", () => {
    render(<MilestoneBadge run={run({ milestones: ms(7), milestones_completed: ["m1", "m2", "m3"] })} />);
    expect(screen.getByText("M3/7")).toBeTruthy();
  });

  it("renders NOTHING for a run with no milestones — the iteration badge stands alone", () => {
    const { container } = render(<MilestoneBadge run={run({ milestones: null })} />);
    expect(container.textContent).toBe("");
  });

  it("renders M–/N (not a 0/N) when the tracker was never reported (PRD #265 M2)", () => {
    render(<MilestoneBadge run={run({ milestones: ms(4), milestones_completed: null })} />);
    expect(screen.getByText("M–/4")).toBeTruthy();
    expect(screen.queryByText("M0/4")).toBeNull();
  });

  it("still renders M0/N when an empty completion set WAS reported (genuine zero)", () => {
    render(<MilestoneBadge run={run({ milestones: ms(4), milestones_completed: [] })} />);
    expect(screen.getByText("M0/4")).toBeTruthy();
  });

  // PRD #390 D5 / M4. The two cases above assert the visible LABEL; the tooltip that tells the
  // states apart on hover is a `title` ATTRIBUTE, invisible to a text query — so a regression
  // that handed the neutral state the "reported complete" wording (or the reverse) would slip
  // past them. Assert the attribute on both states directly.
  it("carries the 'not reported' tooltip on the neutral state and 'reported complete' on a genuine zero (PRD #390 D5)", () => {
    const { rerender } = render(<MilestoneBadge run={run({ milestones: ms(4), milestones_completed: null })} />);
    const neutral = screen.getByText("M–/4");
    expect(neutral.getAttribute("title")).toBe("No milestone completion reported for this run");
    rerender(<MilestoneBadge run={run({ milestones: ms(4), milestones_completed: [] })} />);
    const zero = screen.getByText("M0/4");
    expect(zero.getAttribute("title")).toBe("Milestones reported complete of the approved plan");
  });
});

describe("MilestoneChecklist (done / in-progress / left, PRD #122)", () => {
  const milestones = [
    { id: "a", title: "First milestone" },
    { id: "b", title: "Second milestone" },
    { id: "c", title: "Third milestone" },
  ];

  it("renders every title with a done / in-progress / left indicator", () => {
    render(
      <MilestoneChecklist run={run({ milestones, milestones_completed: ["a"], milestones_in_progress: ["b"] })} />,
    );
    expect(screen.getByText("First milestone")).toBeTruthy();
    expect(screen.getByText("Second milestone")).toBeTruthy();
    expect(screen.getByText("Third milestone")).toBeTruthy();
    expect(screen.getByLabelText("done")).toBeTruthy();
    expect(screen.getByLabelText("in progress")).toBeTruthy();
    expect(screen.getByLabelText("not started")).toBeTruthy();
  });

  // PRD Decision 6: the worker REPORTS completion; nothing in uzi verified it, so the
  // copy must not say "verified". This is the header-wording guard.
  it("says 'reported complete' and NEVER implies verification", () => {
    const { container } = render(<MilestoneChecklist run={run({ milestones, milestones_completed: ["a"] })} />);
    expect(screen.getByText(/reported complete/i)).toBeTruthy();
    expect(container.textContent?.toLowerCase()).not.toContain("verified");
  });

  it("shows a done/total count that is clamped to frozen membership", () => {
    render(<MilestoneChecklist run={run({ milestones, milestones_completed: ["a", "b", "gone"] })} />);
    // "gone" is not a frozen member, so the count clamps to 2/3, never 3/3.
    expect(screen.getByText("2/3")).toBeTruthy();
  });

  it("renders NOTHING for a run with no milestones", () => {
    const { container } = render(<MilestoneChecklist run={run({ milestones: null })} />);
    expect(container.textContent).toBe("");
  });

  it("shows '–/N' (not '0/N') in the header when completion was never reported (PRD #265 M2)", () => {
    render(<MilestoneChecklist run={run({ milestones, milestones_completed: null })} />);
    expect(screen.getByText("–/3")).toBeTruthy();
    expect(screen.queryByText("0/3")).toBeNull();
    // Every milestone still renders as not-started — honest, just no longer a failure-look.
    expect(screen.getAllByLabelText("not started")).toHaveLength(3);
  });

  it("shows a genuine '0/3' when an empty completion set WAS reported", () => {
    render(<MilestoneChecklist run={run({ milestones, milestones_completed: [] })} />);
    expect(screen.getByText("0/3")).toBeTruthy();
  });

  it("renders an untrusted title as PLAIN text, never a Markdown link", () => {
    const { container } = render(
      <MilestoneChecklist
        run={run({ milestones: [{ id: "x", title: "[pwn](http://evil.test)" }], milestones_completed: [] })}
      />,
    );
    const title = screen.getByText("[pwn](http://evil.test)");
    expect(title).toBeTruthy();
    expect(title.closest("a")).toBeNull();
    expect(container.querySelector("a")).toBeNull();
  });
});

describe("PlanPanel proposed milestones (candidate list, PRD #122)", () => {
  it("renders the candidate titles as plain JSX at the gate, never through Markdown", async () => {
    render(
      <PlanPanel
        run={run({
          milestones_candidate: [
            { id: "c1", title: "Ship the thing" },
            { id: "c2", title: "[pwn](http://evil.test)" },
          ],
        })}
        busy={false}
        onApprove={() => {}}
        onReject={() => {}}
      />,
    );
    expect(await screen.findByText("Proposed milestones")).toBeTruthy();
    expect(screen.getByText("Ship the thing")).toBeTruthy();
    // The untrusted candidate title stays literal text — an approval dialog must never
    // mint a clickable link from an agent-authored title.
    const hostile = screen.getByText("[pwn](http://evil.test)");
    expect(hostile.closest("a")).toBeNull();
  });

  it("renders no proposed-milestones block when the candidate list is null", async () => {
    render(
      <PlanPanel run={run({ milestones_candidate: null })} busy={false} onApprove={() => {}} onReject={() => {}} />,
    );
    // Wait for the panel to settle (its best-effort repos fetch resolves), then assert absence.
    await screen.findByRole("button", { name: /Approve plan/ });
    expect(screen.queryByText("Proposed milestones")).toBeNull();
  });
});
