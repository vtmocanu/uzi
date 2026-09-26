// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { act, cleanup, render, screen, fireEvent, waitFor, within } from "@testing-library/react";
import { MemoryRouter, Route, Routes, useNavigate } from "react-router-dom";
import { IssueView } from "./IssueView";
import {
  api,
  ApiError,
  type Card,
  type IssueDetail,
  type Run,
  type RunListItem,
  type SecretMeta,
  type Worker,
} from "../lib/api";
import { useAuth } from "../auth/AuthContext";

// IssueView loads four endpoints and, for Promote (PRD #764), calls promoteIssue.
// Mock the api and useAuth so the test stays offline.
vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      getIssue: vi.fn(),
      listRuns: vi.fn(),
      listWorkers: vi.fn(),
      listSecrets: vi.fn(),
      getMySettings: vi.fn(),
      promoteIssue: vi.fn(),
      createRun: vi.fn(),
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

function anIssue(over: Partial<IssueDetail> = {}): IssueDetail {
  return {
    iid: 7,
    title: "A small typo fix",
    state: "opened",
    labels: ["uzi"],
    assignee_ids: [],
    web_url: "https://gitlab.example.com/grp/proj/-/issues/7",
    forge_type: "gitlab",
    author: "alice",
    has_prd_link: false,
    column: "",
    closed: false,
    conflict: false,
    description: "no PRD here",
    bot_forge_user_id: 4021,
    ...over,
  };
}

function aCard(labels: string[]): Card {
  return {
    iid: 7,
    title: "A small typo fix",
    state: "opened",
    labels,
    assignee_ids: [],
    web_url: "https://gitlab.example.com/grp/proj/-/issues/7",
    forge_type: "gitlab",
    author: "alice",
    has_prd_link: false,
    column: "",
    closed: false,
    conflict: false,
    forge_updated_at: "2026-01-01T00:00:00Z",
    latest_run: null,
    pipeline: null,
  };
}

function aRunItem(over: Partial<RunListItem> = {}): RunListItem {
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
    harness: "claude",
    title: null,
    resume_of_run_id: null,
    status: "completed",
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
    codex_secret_id: null,
    codex_secret_label: null,
    recovery_retry_not_before: null,
    forge_park_count: 0,
    forge_park_max: 0,
    claimed_at: null,
    started_at: "2026-07-05T12:00:00Z",
    finished_at: "2026-07-05T12:05:00Z",
    created_at: "2026-07-05T12:00:00Z",
    updated_at: "2026-07-05T12:05:00Z",
    repo_path: "ns/repo",
    worker_name: "w1",
    judge_verdict: null,
    judge_todo_count: 0,
    ...over,
  } as RunListItem;
}

function setAuth() {
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
}

function renderIssueView() {
  return render(
    <MemoryRouter initialEntries={["/repos/repo-1/issues/7"]}>
      <Routes>
        <Route path="/repos/:repoId/issues/:iid" element={<IssueView />} />
      </Routes>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  mockApi.listRuns.mockResolvedValue({ runs: [] });
  mockApi.listWorkers.mockResolvedValue({ workers: [] });
  mockApi.listSecrets.mockResolvedValue({ secrets: [] });
  // PRD #1429 M4a review Fix 1: the viewer's default_harness, no preference by default.
  mockApi.getMySettings.mockResolvedValue({
    settings: {
      default_harness: null,
      default_model: null,
      default_effort: null,
      judge_model: null,
      summary_model: null,
      appearance_mode: null,
      light_theme: null,
      dark_theme: null,
      typeface: null,
      theme: null,
    },
  });
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
  // clearAllMocks keeps queued mock*Once implementations, so a test that fails before
  // consuming its queued createRun outcomes would leak them into the next test.
  mockApi.createRun.mockReset();
});

// Issue #124, item 9. The issue TITLE and DESCRIPTION are both forge-supplied, and the
// issue's own audit list names descriptions alongside titles. The description renders
// through <Markdown>, which is hardened against raw HTML and dangerous URL schemes and,
// since issue #319, ALSO strips Cf/bidi control characters centrally — so that pipeline
// now closes the bidi hole by construction for the description (the page's own
// stripUnsafeChars wrap on it is redundant but harmless). The title is a non-Markdown
// escaped-JSX sink and still strips per-site.
describe("IssueView — the forge title and description carry no format characters (#124)", () => {
  it("strips bidi/zero-width characters from both", async () => {
    setAuth();
    mockApi.getIssue.mockResolvedValue({
      issue: anIssue({
        title: "Fix the \u202Eparser\u200B bug",
        description: "The \u202Eapproved fix is in `api/`.",
      }),
    });
    const { container } = renderIssueView();
    // Wait for the LOADED, sanitized title before asserting. The "#7" breadcrumb
    // renders from the URL param during the loading state, before getIssue resolves,
    // so anchoring on it let the synchronous assertions below race the async fetch.
    // The stripped title only appears once the issue has loaded and been sanitized,
    // so findByText on it is the deterministic gate for both assertions.
    expect(await screen.findByText("Fix the parser bug")).toBeTruthy();
    expect(container.textContent ?? "").not.toMatch(/[\p{Cf}]/u);
    expect(container.textContent).toContain("The approved fix is in");
  });
});

// issue #1486: a run-history row's MR/PR link is reconstructed from the issue's project
// URL when the run has no persisted mr_web_url, and that reconstruction must be
// forge-aware. A github issue + github run must produce a /pull/<n> link, not GitLab's
// hard-coded /-/merge_requests/<n> (a 404 on GitHub) and not an inert chip.
describe("IssueView run history — forge-aware MR/PR link when mr_web_url is null (#1486)", () => {
  it("builds a /pull/<n> RunHistoryRow link for a github issue+run", async () => {
    setAuth();
    mockApi.getIssue.mockResolvedValue({
      issue: anIssue({
        forge_type: "github",
        web_url: "https://github.com/ns/repo/issues/7",
      }),
    });
    mockApi.listRuns.mockResolvedValue({
      runs: [
        aRunItem({
          id: "run-1",
          status: "completed",
          forge_type: "github",
          mr_iid: 799,
          mr_state: "opened",
          mr_web_url: null,
        }),
      ],
    });

    const { container } = renderIssueView();

    await screen.findByText("A small typo fix");
    // A real anchor (not an inert span) carrying the GitHub /pull/ path.
    const chip = await waitFor(() => {
      const a = container.querySelector('a[href*="/pull/"]') as HTMLAnchorElement | null;
      expect(a).not.toBeNull();
      return a!;
    });
    expect(chip.getAttribute("href")).toBe("https://github.com/ns/repo/pull/799");
    // Non-vacuous: the old hard-coded path would have produced /-/merge_requests/799.
    expect(container.querySelector('a[href*="merge_requests"]')).toBeNull();
  });
});

describe("IssueView PRD presence badge (PRD #764)", () => {
  // A linked prds/*.md is optional but still detected: an issue that has one shows a
  // neutral "PRD" badge; one that does not shows no badge (and no "no PRD link" warning,
  // which the old PRD-required model rendered).
  const BADGE_TITLE = "This issue links a prds/*.md file";

  it("shows the neutral PRD badge when the issue links a PRD", async () => {
    setAuth();
    mockApi.getIssue.mockResolvedValue({ issue: anIssue({ has_prd_link: true }) });
    renderIssueView();
    await screen.findByText("A small typo fix");
    expect(screen.getByTitle(BADGE_TITLE)).toBeTruthy();
  });

  it("shows no PRD badge — and no 'no PRD link' warning — when the issue has no PRD", async () => {
    setAuth();
    mockApi.getIssue.mockResolvedValue({ issue: anIssue({ has_prd_link: false }) });
    renderIssueView();
    await screen.findByText("A small typo fix");
    expect(screen.queryByTitle(BADGE_TITLE)).toBeNull();
    // The retired "no PRD link" warning must not render — a PRD is optional now.
    expect(screen.queryByText("no PRD link")).toBeNull();
  });
});

// PRD #102 M4 gave the board and the issue view one shared chip predicate. PRD #764:
// chipLabels still excludes only the autopilot marker and the column labels (so PRD and
// other content labels chip normally), but IssueView additionally drops the `uzi`
// runnable marker from its own chip row — it is surfaced as the brand "runnable" badge
// instead, so it never renders both as a chip and as a badge.
describe("IssueView label chips (PRD #102 M4, PRD #764)", () => {
  it("chips content labels (incl. PRD) and drops the autopilot marker + columns", async () => {
    setAuth();
    mockApi.getIssue.mockResolvedValue({
      // A non-uzi issue (so the runnable badge does not render a second "uzi" element),
      // and has_prd_link:false so the neutral PRD presence badge does not add a second
      // "PRD" element beside the PRD chip under test.
      issue: anIssue({
        labels: ["PRD", "autopilot", "In Progress", "bug"],
        column: "In Progress",
        has_prd_link: false,
      }),
    });
    renderIssueView();
    await screen.findByText("A small typo fix");
    expect(screen.getByText("bug")).toBeTruthy();
    // PRD #764: "PRD" is no longer special to chipLabels; it chips as an ordinary label.
    expect(screen.getByText("PRD")).toBeTruthy();
    // autopilot is excluded from the CHIPS and always was. Since review M-1 it renders
    // as a BADGE instead, which is a different element with a different meaning — so
    // the assertion is that exactly one element carries the name and it is the badge,
    // not that the name is absent. Asserting absence here would forbid the badge.
    const autopilotEls = screen.getAllByText("autopilot");
    expect(autopilotEls).toHaveLength(1);
    expect(autopilotEls[0].getAttribute("title")).toMatch(/Autopilot/);
    // The issue's own column already names the header badge; a second copy of it as
    // a chip is the duplication Decision 6's column exclusion exists to prevent.
    expect(screen.getAllByText("In Progress")).toHaveLength(1);
  });

  // m-2. The strip on this page had NO test: folding stripUnsafeChars(l) to a raw {l}
  // left nine tests green, because nothing here rendered a label carrying one.
  it("strips format characters out of a chip (#124)", async () => {
    setAuth();
    mockApi.getIssue.mockResolvedValue({
      issue: anIssue({ labels: ["PRD", "se\u202Ecurity"], has_prd_link: true }),
    });
    const { container } = renderIssueView();
    await screen.findByText("A small typo fix");
    expect(container.textContent ?? "").not.toMatch(/[\p{Cf}]/u);
    expect(screen.getByText("security")).toBeTruthy();
  });

  // m-3. The ATTRIBUTE channel, on this surface too. container.textContent cannot see
  // a title=, so the strip there was ungated on both pages at once.
  it("strips the chip's title ATTRIBUTE, not only its text (#124)", async () => {
    setAuth();
    mockApi.getIssue.mockResolvedValue({
      issue: anIssue({ labels: ["PRD", "se\u202Ecurity"], has_prd_link: true }),
    });
    renderIssueView();
    await screen.findByText("A small typo fix");
    const chip = screen.getByText("security");
    expect(chip.getAttribute("title")).toBe("security");
  });
});

// M-1. The autopilot label lost its only user-visible surface in web/ when M4 removed
// it from the chip list. Removing it from the CHIPS was right — it is a workflow
// marker, not content — but the consequence was that an issue armed for an unattended
// run showed nothing at all until a run existed to carry RunView's badge.
//
// A badge is not a chip, so Decision 6 is untouched: the chip row still excludes the
// label, and this says the distinct thing the chip never did.
describe("IssueView autopilot badge (PRD #102 review M-1)", () => {
  it("shows an autopilot badge when the label is applied", async () => {
    setAuth();
    mockApi.getIssue.mockResolvedValue({
      issue: anIssue({ labels: ["PRD", "autopilot"], has_prd_link: true }),
    });
    renderIssueView();
    await screen.findByText("A small typo fix");
    expect(screen.getByTitle(/Autopilot/)).toBeTruthy();
    expect(screen.getByText("autopilot")).toBeTruthy();
  });

  it("shows nothing when the label is absent", async () => {
    setAuth();
    mockApi.getIssue.mockResolvedValue({ issue: anIssue({ labels: ["PRD"], has_prd_link: true }) });
    renderIssueView();
    await screen.findByText("A small typo fix");
    expect(screen.queryByTitle(/Autopilot/)).toBeNull();
  });

  it("reads the CONFIGURED label name, never the literal 'autopilot'", async () => {
    // The label is operator-configurable, like the other three. A hardcoded name would
    // silently stop marking armed issues the moment an admin renames it.
    vi.mocked(useAuth).mockReturnValue({
      ...vi.mocked(useAuth)(),
      autopilotLabel: "robot",
    } as unknown as ReturnType<typeof useAuth>);
    mockApi.getIssue.mockResolvedValue({
      issue: anIssue({ labels: ["PRD", "robot"], has_prd_link: true }),
    });
    renderIssueView();
    await screen.findByText("A small typo fix");
    expect(screen.getByText("robot")).toBeTruthy();
  });

  it("does NOT put autopilot back in the chip row (Decision 6 stays intact)", async () => {
    // The badge must not become a second route for a label the chip predicate excludes:
    // exactly one element carries the name, and it is the badge.
    setAuth();
    mockApi.getIssue.mockResolvedValue({
      issue: anIssue({ labels: ["PRD", "autopilot"], has_prd_link: true }),
    });
    renderIssueView();
    await screen.findByText("A small typo fix");
    expect(screen.getAllByText("autopilot")).toHaveLength(1);
    expect(screen.getByText("autopilot").getAttribute("title")).toMatch(/Autopilot/);
  });
});

// A worker and an Anthropic token, the facts a runnable Start needs (shared by the
// Start gate suite and the #1727 navigation suite).
const aWorker = (): Worker => ({
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
  created_at: "2026-01-01T00:00:00Z",
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
});
const aToken = (): SecretMeta => ({
  id: "sec-1",
  label: "default",
  is_default: true,
  auto_eligible: false,
  kind: "anthropic_token",
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-01T00:00:00Z",
});

describe("IssueView Start gate (PRD #764)", () => {

  it("enables Start on a uzi issue with NO PRD link once a worker + token exist (PRD #764)", async () => {
    setAuth();
    // A run no longer requires a PRD link (PRD #764), so a worker + token is all it takes.
    mockApi.listWorkers.mockResolvedValue({ workers: [aWorker()] });
    mockApi.listSecrets.mockResolvedValue({ secrets: [aToken()] });
    mockApi.getIssue.mockResolvedValue({ issue: anIssue({ labels: ["uzi"], has_prd_link: false }) });
    renderIssueView();

    const startBtn = () => screen.getByRole("button", { name: /start run/i }) as HTMLButtonElement;
    await screen.findByText("A small typo fix");
    await waitFor(() => expect(startBtn().disabled).toBe(false));
  });

  // PRD #1429 M4a, D2: the harness picker appears ONLY when the user has a usable
  // credential for BOTH harnesses — a single-harness (Claude-only) user's flow stays
  // byte-identical to today, with no redundant picker.
  it("hides the harness picker for a Claude-only user (today's flow, unchanged)", async () => {
    setAuth();
    mockApi.listWorkers.mockResolvedValue({ workers: [aWorker()] });
    mockApi.listSecrets.mockResolvedValue({ secrets: [aToken()] });
    mockApi.getIssue.mockResolvedValue({ issue: anIssue({ labels: ["uzi"], has_prd_link: false }) });
    renderIssueView();

    await screen.findByText("A small typo fix");
    await screen.findByRole("button", { name: /start run/i });
    expect(screen.queryByLabelText("Harness for this run")).toBeNull();
    // The Anthropic picker is still there — unaffected by the harness feature.
    expect(screen.getByLabelText("Anthropic token for this run")).toBeTruthy();
  });

  it("shows the harness picker once BOTH harnesses are usable, defaulting to inherit", async () => {
    setAuth();
    mockApi.listWorkers.mockResolvedValue({ workers: [aWorker()] });
    mockApi.listSecrets.mockResolvedValue({
      secrets: [aToken(), { ...aToken(), id: "sec-codex", kind: "openai_api_key" }],
    });
    mockApi.getIssue.mockResolvedValue({ issue: anIssue({ labels: ["uzi"], has_prd_link: false }) });
    renderIssueView();

    await screen.findByText("A small typo fix");
    const picker = (await screen.findByLabelText("Harness for this run")) as HTMLSelectElement;
    expect(picker.value).toBe("inherit");
  });

  it("sends the picked harness on Start and hides the Anthropic picker once Codex is chosen", async () => {
    setAuth();
    mockApi.listWorkers.mockResolvedValue({ workers: [aWorker()] });
    mockApi.listSecrets.mockResolvedValue({
      secrets: [aToken(), { ...aToken(), id: "sec-codex", kind: "openai_api_key" }],
    });
    mockApi.getIssue.mockResolvedValue({ issue: anIssue({ labels: ["uzi"], has_prd_link: false }) });
    mockApi.createRun.mockResolvedValue({ run: { id: "run-codex-1" } as unknown as Run });
    renderIssueView();

    await screen.findByText("A small typo fix");
    const picker = (await screen.findByLabelText("Harness for this run")) as HTMLSelectElement;
    fireEvent.change(picker, { target: { value: "codex" } });
    // Once harness=codex is chosen, the Anthropic picker (irrelevant to a Codex run) hides.
    expect(screen.queryByLabelText("Anthropic token for this run")).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: /start run/i }));
    await waitFor(() => expect(mockApi.createRun).toHaveBeenCalled());
    expect(mockApi.createRun).toHaveBeenCalledWith("repo-1", 7, undefined, undefined, "codex");
  });

  // Fix 2 (M4a review): a Codex-only user never sees the harness picker (only one
  // harness is usable — D2), so `harness` stays "inherit". Gating the Anthropic
  // TokenPicker on the raw picker value alone left it visible for this user even though
  // the run WILL resolve to Codex — using it 422s. Paired with the positive control
  // below (a Claude-only / both-usable user DOES see it) so this is not a vacuous
  // "never renders" assertion.
  it("hides the Anthropic token picker for a Codex-only user, even with no explicit harness pick", async () => {
    setAuth();
    mockApi.listWorkers.mockResolvedValue({ workers: [aWorker()] });
    mockApi.listSecrets.mockResolvedValue({
      secrets: [{ ...aToken(), id: "sec-codex", kind: "openai_api_key" }],
    });
    mockApi.getIssue.mockResolvedValue({ issue: anIssue({ labels: ["uzi"], has_prd_link: false }) });
    renderIssueView();

    await screen.findByText("A small typo fix");
    await screen.findByRole("button", { name: /start run/i });
    // Single-harness user: no redundant harness picker either (D2).
    expect(screen.queryByLabelText("Harness for this run")).toBeNull();
    expect(screen.queryByLabelText("Anthropic token for this run")).toBeNull();
  });

  // Positive control for the case above: a Claude-only user (the pre-existing,
  // single-harness flow) still sees the Anthropic picker — the fix must not hide it
  // universally.
  it("still shows the Anthropic token picker for a Claude-only user", async () => {
    setAuth();
    mockApi.listWorkers.mockResolvedValue({ workers: [aWorker()] });
    mockApi.listSecrets.mockResolvedValue({ secrets: [aToken()] });
    mockApi.getIssue.mockResolvedValue({ issue: anIssue({ labels: ["uzi"], has_prd_link: false }) });
    renderIssueView();

    await screen.findByText("A small typo fix");
    expect(await screen.findByLabelText("Anthropic token for this run")).toBeTruthy();
  });

  // Positive control: a both-usable user with the harness picker left on inherit (the
  // untouched default) also still sees the Anthropic picker — D11 rule 4 resolves
  // inherit to Claude when both are usable and no default is set, so hiding it here
  // would be wrong.
  it("still shows the Anthropic token picker for a both-usable user on the untouched inherit default", async () => {
    setAuth();
    mockApi.listWorkers.mockResolvedValue({ workers: [aWorker()] });
    mockApi.listSecrets.mockResolvedValue({
      secrets: [aToken(), { ...aToken(), id: "sec-codex", kind: "openai_api_key" }],
    });
    mockApi.getIssue.mockResolvedValue({ issue: anIssue({ labels: ["uzi"], has_prd_link: false }) });
    renderIssueView();

    await screen.findByText("A small typo fix");
    const picker = (await screen.findByLabelText("Harness for this run")) as HTMLSelectElement;
    expect(picker.value).toBe("inherit");
    expect(screen.getByLabelText("Anthropic token for this run")).toBeTruthy();
  });

  // Fix 1 (M4a review follow-up, D11 rule 2): a both-usable user who set a Codex
  // default_harness (Run Defaults) and leaves the picker on the untouched "inherit"
  // default must ALSO have the Anthropic picker hidden — the run resolves to Codex via
  // D11 rule 2 (a usable default wins), and touching the Anthropic picker would 422.
  // Paired with the positive control directly above (both-usable, no default, still
  // shows the picker) so this is not a vacuous "never renders" assertion.
  it("hides the Anthropic token picker for a both-usable user whose default_harness is codex", async () => {
    setAuth();
    mockApi.listWorkers.mockResolvedValue({ workers: [aWorker()] });
    mockApi.listSecrets.mockResolvedValue({
      secrets: [aToken(), { ...aToken(), id: "sec-codex", kind: "openai_api_key" }],
    });
    mockApi.getMySettings.mockResolvedValue({
      settings: {
        default_harness: "codex",
        default_model: null,
        default_effort: null,
        judge_model: null,
        summary_model: null,
        appearance_mode: null,
        light_theme: null,
        dark_theme: null,
        typeface: null,
        theme: null,
      },
    });
    mockApi.getIssue.mockResolvedValue({ issue: anIssue({ labels: ["uzi"], has_prd_link: false }) });
    renderIssueView();

    await screen.findByText("A small typo fix");
    // The harness picker still shows (both are usable — D2) and stays on the untouched
    // "inherit" default...
    const picker = (await screen.findByLabelText("Harness for this run")) as HTMLSelectElement;
    expect(picker.value).toBe("inherit");
    // ...but the Anthropic picker is hidden: D11 rule 2 resolves this inherit pick to Codex.
    expect(screen.queryByLabelText("Anthropic token for this run")).toBeNull();
  });

  // Issue #856 M3: a completed prior run that still owns an open MR makes the
  // server refuse a fresh run with a coded 409 (issue_has_open_mr). Start catches
  // it, confirms (the message names the MR), and retries with force on confirm.
  const openMRError = () =>
    new ApiError(
      409,
      "issue #7 already has open MR !42 — merge or close it, or leave review comments on the MR to iterate, before starting a new run (pass --force to re-run anyway)",
      { code: "issue_has_open_mr", mr_iid: 42 },
    );

  const renderWithRunRoute = () =>
    render(
      <MemoryRouter initialEntries={["/repos/repo-1/issues/7"]}>
        <Routes>
          <Route path="/repos/:repoId/issues/:iid" element={<IssueView />} />
          <Route path="/runs/:runId" element={<div>run page</div>} />
        </Routes>
      </MemoryRouter>,
    );

  const runnable = () => {
    mockApi.listWorkers.mockResolvedValue({ workers: [aWorker()] });
    mockApi.listSecrets.mockResolvedValue({ secrets: [aToken()] });
    mockApi.getIssue.mockResolvedValue({ issue: anIssue({ labels: ["uzi"], has_prd_link: false }) });
  };

  it("on the open-MR 409, confirms and retries Start with force, then navigates (#856)", async () => {
    setAuth();
    runnable();
    const confirmSpy = vi.spyOn(window, "confirm").mockReturnValue(true);
    mockApi.createRun
      .mockRejectedValueOnce(openMRError())
      .mockResolvedValueOnce({ run: { id: "run-9" } as unknown as Run });
    renderWithRunRoute();

    const startBtn = () => screen.getByRole("button", { name: /start run/i }) as HTMLButtonElement;
    await screen.findByText("A small typo fix");
    await waitFor(() => expect(startBtn().disabled).toBe(false));
    fireEvent.click(startBtn());

    // The confirm is web-composed: names the MR, states the action, no --force jargon.
    await waitFor(() => expect(confirmSpy).toHaveBeenCalledTimes(1));
    const confirmMsg = confirmSpy.mock.calls[0][0] as string;
    expect(confirmMsg).toContain("!42");
    expect(confirmMsg).toContain("Start a new run anyway?");
    expect(confirmMsg).not.toContain("--force");
    await waitFor(() => expect(mockApi.createRun).toHaveBeenCalledTimes(2));
    expect(mockApi.createRun.mock.calls[0]).toEqual(["repo-1", 7, undefined]);
    expect(mockApi.createRun.mock.calls[1]).toEqual(["repo-1", 7, true]);
    await screen.findByText("run page");
    confirmSpy.mockRestore();
  });

  it("on the open-MR 409, declining Start does not retry and shows no error (#856)", async () => {
    setAuth();
    runnable();
    const confirmSpy = vi.spyOn(window, "confirm").mockReturnValue(false);
    mockApi.createRun.mockRejectedValueOnce(openMRError());
    renderIssueView();

    const startBtn = () => screen.getByRole("button", { name: /start run/i }) as HTMLButtonElement;
    await screen.findByText("A small typo fix");
    await waitFor(() => expect(startBtn().disabled).toBe(false));
    fireEvent.click(startBtn());

    await waitFor(() => expect(confirmSpy).toHaveBeenCalledTimes(1));
    expect(mockApi.createRun).toHaveBeenCalledTimes(1);
    // The coded-conflict message is not shown as an error alert on decline.
    expect(screen.queryByText(/already has open MR/)).toBeNull();
    // The Start control is re-enabled after decline (starting state cleared): a
    // stuck-in-"Starting…" regression would leave it disabled and fail here.
    await waitFor(() => expect(startBtn().disabled).toBe(false));
    expect(screen.queryByText("Starting…")).toBeNull();
    confirmSpy.mockRestore();
  });

  it("on the open-MR 409, a confirmed forced retry that fails clears starting (#856)", async () => {
    setAuth();
    runnable();
    const confirmSpy = vi.spyOn(window, "confirm").mockReturnValue(true);
    mockApi.createRun
      .mockRejectedValueOnce(openMRError())
      .mockRejectedValueOnce(new ApiError(500, "boom while forcing"));
    renderIssueView();

    const startBtn = () => screen.getByRole("button", { name: /start run/i }) as HTMLButtonElement;
    await screen.findByText("A small typo fix");
    await waitFor(() => expect(startBtn().disabled).toBe(false));
    fireEvent.click(startBtn());

    await waitFor(() => expect(confirmSpy).toHaveBeenCalledTimes(1));
    // Retried with force === true; that retry failed, and the starting state is
    // cleared (button re-enabled). Like Board, the forced-retry failure now stays on
    // screen (issue #1727): settlement and its reload no longer wipe it.
    await waitFor(() => expect(mockApi.createRun).toHaveBeenCalledTimes(2));
    expect(mockApi.createRun.mock.calls[1]).toEqual(["repo-1", 7, true]);
    await waitFor(() => expect(startBtn().disabled).toBe(false));
    expect(screen.getByText("boom while forcing")).toBeTruthy();
    confirmSpy.mockRestore();
  });

  // Issue #1727 item 1. A failed start called onError then onSettled, and onSettled
  // cleared the error it had just been handed; the reload it fires then ran the hook's
  // onFetchStart, which cleared it a second time. Either wipe alone hides the failure.
  //
  // Mutation-checked: restoring setError("") in onSettled, or restoring the
  // onFetchStart clear, each leaves no alert (observed red: "worker pool is full" not
  // found).
  it("keeps a failed start's error on screen through settlement and the reload (#1727)", async () => {
    setAuth();
    runnable();
    mockApi.createRun.mockRejectedValueOnce(new ApiError(500, "worker pool is full"));
    renderIssueView();

    const startBtn = () => screen.getByRole("button", { name: /start run/i }) as HTMLButtonElement;
    await screen.findByText("A small typo fix");
    await waitFor(() => expect(startBtn().disabled).toBe(false));
    const loadsBefore = mockApi.listRuns.mock.calls.length;
    fireEvent.click(startBtn());

    // Settled (button re-enabled) and the reload has run and resolved...
    await waitFor(() => expect(startBtn().disabled).toBe(false));
    await waitFor(() => expect(mockApi.listRuns.mock.calls.length).toBe(loadsBefore + 1));
    await act(async () => {});
    // ...and the error is still shown.
    expect(screen.getByText("worker pool is full")).toBeTruthy();
  });

  it("clears a failed start's error when the next attempt starts (#1727)", async () => {
    setAuth();
    runnable();
    const confirmSpy = vi.spyOn(window, "confirm").mockReturnValue(false);
    mockApi.createRun
      .mockRejectedValueOnce(new ApiError(500, "worker pool is full"))
      .mockRejectedValueOnce(openMRError());
    renderIssueView();

    const startBtn = () => screen.getByRole("button", { name: /start run/i }) as HTMLButtonElement;
    await screen.findByText("A small typo fix");
    await waitFor(() => expect(startBtn().disabled).toBe(false));
    fireEvent.click(startBtn());
    await screen.findByText("worker pool is full");

    // The second attempt is declined at the open-MR confirm (no error of its own), so
    // the only thing that can remove the first error is the attempt-start clear.
    fireEvent.click(startBtn());
    await waitFor(() => expect(confirmSpy).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(startBtn().disabled).toBe(false));
    expect(screen.queryByText("worker pool is full")).toBeNull();
    confirmSpy.mockRestore();
  });

  it("a successful start still navigates to the run with no error shown (#1727)", async () => {
    setAuth();
    runnable();
    mockApi.createRun.mockResolvedValueOnce({ run: { id: "run-9" } as unknown as Run });
    renderWithRunRoute();

    const startBtn = () => screen.getByRole("button", { name: /start run/i }) as HTMLButtonElement;
    await screen.findByText("A small typo fix");
    await waitFor(() => expect(startBtn().disabled).toBe(false));
    fireEvent.click(startBtn());
    await screen.findByText("run page");
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("a settled (declined) start still reloads the run history (#1727)", async () => {
    setAuth();
    runnable();
    const confirmSpy = vi.spyOn(window, "confirm").mockReturnValue(false);
    mockApi.createRun.mockRejectedValueOnce(openMRError());
    renderIssueView();

    const startBtn = () => screen.getByRole("button", { name: /start run/i }) as HTMLButtonElement;
    await screen.findByText("A small typo fix");
    await waitFor(() => expect(startBtn().disabled).toBe(false));
    const loadsBefore = mockApi.listRuns.mock.calls.length;
    fireEvent.click(startBtn());
    await waitFor(() => expect(mockApi.listRuns.mock.calls.length).toBe(loadsBefore + 1));
    confirmSpy.mockRestore();
  });
});

// PRD #764. The runnable marker + Promote affordance key off the single `uzi` label: an
// issue carrying it is uzi's to run (Start run + a runnable badge); one without it offers
// Promote (which adds `uzi`).
describe("IssueView — runnable marker + Promote (PRD #764)", () => {
  // The runnable badge is queried by its distinctive title (its text "uzi" also appears
  // as a chip), so this assertion does not move when the label is renamed in settings.
  const RUNNABLE_TITLE = /uzi will run it/;

  it("marks a NON-uzi issue and offers Promote in place of Start run", async () => {
    setAuth();
    mockApi.getIssue.mockResolvedValue({ issue: anIssue({ labels: ["documentation"] }) });
    renderIssueView();
    await screen.findByText("A small typo fix");

    // No runnable badge; Promote is offered instead of Start run.
    expect(screen.queryByTitle(RUNNABLE_TITLE)).toBeNull();
    expect(screen.getByRole("button", { name: /Promote to uzi/ })).toBeTruthy();
    expect(screen.queryByRole("button", { name: /start run/i })).toBeNull();
  });

  it("shows the runnable marker + Start run on a uzi issue, not Promote", async () => {
    setAuth();
    mockApi.getIssue.mockResolvedValue({ issue: anIssue({ labels: ["uzi"] }) });
    renderIssueView();
    await screen.findByText("A small typo fix");

    // The runnable marker renders (the POSITIVE assertion for the new model)...
    expect(screen.getByTitle(RUNNABLE_TITLE)).toBeTruthy();
    // ...and Start run shows, not Promote.
    expect(screen.queryByRole("button", { name: /Promote to uzi/ })).toBeNull();
    expect(screen.getByRole("button", { name: /start run/i })).toBeTruthy();
  });

  it("marks an ASSIGNED-but-unlabelled issue runnable with honest copy, not the uzi pill (PRD #767 M5)", async () => {
    setAuth();
    // No `uzi` label, but assigned to the repo's bot (bot_forge_user_id 4021).
    mockApi.getIssue.mockResolvedValue({
      issue: anIssue({ labels: ["documentation"], assignee_ids: [4021] }),
    });
    renderIssueView();
    await screen.findByText("A small typo fix");

    // Runnable + Start run, not Promote.
    expect(screen.getByRole("button", { name: /start run/i })).toBeTruthy();
    expect(screen.queryByRole("button", { name: /Promote to uzi/ })).toBeNull();
    // The marker is the assignment badge with honest copy...
    const marker = screen.getByTitle(/assigned to the uzi bot, so it's eligible for a uzi run/i);
    expect(marker.textContent).toBe("assigned");
    // ...and it must NOT claim the uzi label (the false-copy bug this fixes).
    expect(screen.queryByTitle(/carries the uzi label/i)).toBeNull();
  });

  it("promotes forge-first and adopts the returned labels", async () => {
    setAuth();
    mockApi.getIssue.mockResolvedValue({ issue: anIssue({ labels: ["documentation"] }) });
    mockApi.promoteIssue.mockResolvedValue({ card: aCard(["uzi", "documentation"]) });
    renderIssueView();
    await screen.findByText("A small typo fix");

    fireEvent.click(screen.getByRole("button", { name: /Promote to uzi/ }));
    await waitFor(() => expect(mockApi.promoteIssue).toHaveBeenCalledWith("repo-1", 7));
    // The page re-reads as a runnable issue: the runnable marker appears, Start run shows.
    await waitFor(() => expect(screen.getByTitle(RUNNABLE_TITLE)).toBeTruthy());
    expect(screen.getByRole("button", { name: /start run/i })).toBeTruthy();
  });

  it("does not offer Promote on the self-improve tracker (Decision 13a)", async () => {
    setAuth();
    mockApi.getIssue.mockResolvedValue({ issue: anIssue({ labels: ["uzi-self-improve"] }) });
    renderIssueView();
    await screen.findByText("A small typo fix");

    // Not runnable (no `uzi` label) and not promotable — the server refuses it too, so an
    // offered button would be a 422 waiting to happen.
    expect(screen.queryByTitle(RUNNABLE_TITLE)).toBeNull();
    expect(screen.queryByRole("button", { name: /Promote to uzi/ })).toBeNull();
  });
});

// web-ux S3. A column name is user-supplied and effectively unbounded: the Columns
// editor applies no maxlength and GitLab allows 255 characters. Measured at 375x812,
// a 105-char name rendered a 594px badge in a 375px viewport and pushed
// document.scrollWidth to 610 — the whole page scrolled sideways.
//
// jsdom has no layout, so this CANNOT assert the width. It asserts the mechanism
// instead: the badge must not be whitespace-nowrap, which is what forced the
// overflow. Stated plainly because a passing test here is weaker evidence than the
// browser measurement that found it.
describe("IssueView — a long column name does not overflow the page (web-ux S3)", () => {
  it("renders the column badge wrapping rather than nowrap", async () => {
    setAuth();
    const long = "Waiting on the upstream vendor to confirm the migration window and sign off";
    mockApi.getIssue.mockResolvedValue({ issue: anIssue({ column: long }) });
    renderIssueView();
    await screen.findByText("A small typo fix");

    const badge = screen.getByText(long).closest("span[class*='rounded-md']") as HTMLElement;
    expect(badge).toBeTruthy();
    expect(badge.classList.contains("whitespace-nowrap")).toBe(false);
    expect(badge.classList.contains("whitespace-normal")).toBe(true);
  });

  it("still renders an ordinary column name", async () => {
    setAuth();
    mockApi.getIssue.mockResolvedValue({ issue: anIssue({ column: "In Progress" }) });
    renderIssueView();
    await screen.findByText("A small typo fix");
    expect(screen.getByText("In Progress")).toBeTruthy();
  });
});

// Issue #961 item 3 (m2). Navigating A→B on the same route element refetches on the
// [repoId, iidNum] deps change. If A's in-flight fetch resolves AFTER B's, its
// `setIssue(A)` must be dropped: the fetcher guards `setIssue` with `if (isCurrent())`,
// and the hook's own generation guard already protects `data` (the runs). Without the
// guard the header flips to A's title while the runs stay B's — the exact "A's header
// beside B's runs" mix this pins against.
//
// Mutation-checked: dropping `if (isCurrent())` around setIssue makes A's late resolve
// call setIssue(A), so "Issue A" renders (observed red: Unable to satisfy — "Issue A"
// found where the test expects null, "Issue B" header replaced).
describe("IssueView — a superseded fetch cannot seed a stale issue header (item 3)", () => {
  function NavToB() {
    const navigate = useNavigate();
    return (
      <button type="button" onClick={() => navigate("/repos/repo-1/issues/8")}>
        go to B
      </button>
    );
  }

  it("keeps B's header when A's fetch resolves last", async () => {
    setAuth();

    let resolveA!: (v: { issue: IssueDetail }) => void;
    let resolveB!: (v: { issue: IssueDetail }) => void;
    const pA = new Promise<{ issue: IssueDetail }>((r) => (resolveA = r));
    const pB = new Promise<{ issue: IssueDetail }>((r) => (resolveB = r));
    mockApi.getIssue.mockImplementation((_repo: string, iid: number) => (iid === 7 ? pA : pB));

    render(
      <MemoryRouter initialEntries={["/repos/repo-1/issues/7"]}>
        <Routes>
          <Route
            path="/repos/:repoId/issues/:iid"
            element={
              <>
                <NavToB />
                <IssueView />
              </>
            }
          />
        </Routes>
      </MemoryRouter>,
    );

    // A is in flight (getIssue(7) pending); navigate to B before A resolves.
    await waitFor(() => expect(mockApi.getIssue).toHaveBeenCalledWith("repo-1", 7));
    fireEvent.click(screen.getByRole("button", { name: "go to B" }));
    await waitFor(() => expect(mockApi.getIssue).toHaveBeenCalledWith("repo-1", 8));

    // B resolves first and lands its header...
    await act(async () => {
      resolveB({ issue: anIssue({ iid: 8, title: "Issue B" }) });
    });
    await screen.findByText("Issue B");

    // ...then A resolves LAST and must be dropped as stale — no A/B header mix.
    await act(async () => {
      resolveA({ issue: anIssue({ iid: 7, title: "Issue A" }) });
    });
    expect(screen.getByText("Issue B")).toBeTruthy();
    expect(screen.queryByText("Issue A")).toBeNull();
  });
});

// Issue #961 item 4a (m2). A promote failure sets the shared `error` slot. Navigating to
// another issue is a route-identity change; the reset effect keyed on [repoId, iidNum]
// wipes that error so a stale promote toast does not bleed onto the next issue. (Issue
// #1727 moved this clear off the hook's onFetchStart, which also fired on the post-start
// reload and wiped a fresh start error.)
//
// Mutation-checked: removing setActionError("") from the reset effect leaves the promote
// error on screen after navigation (observed red: "forge said no" still present).
describe("IssueView — a promote error clears on issue navigation (item 4a)", () => {
  function NavToB() {
    const navigate = useNavigate();
    return (
      <button type="button" onClick={() => navigate("/repos/repo-1/issues/8")}>
        go to B
      </button>
    );
  }

  it("wipes the promote error when navigating to another issue", async () => {
    setAuth();
    mockApi.getIssue.mockImplementation(async (_repo: string, iid: number) => ({
      issue:
        iid === 7
          ? anIssue({ iid: 7, title: "Issue A", labels: ["documentation"] })
          : anIssue({ iid: 8, title: "Issue B", labels: ["documentation"] }),
    }));
    mockApi.promoteIssue.mockRejectedValue(new ApiError(500, "forge said no"));

    render(
      <MemoryRouter initialEntries={["/repos/repo-1/issues/7"]}>
        <Routes>
          <Route
            path="/repos/:repoId/issues/:iid"
            element={
              <>
                <NavToB />
                <IssueView />
              </>
            }
          />
        </Routes>
      </MemoryRouter>,
    );

    await screen.findByText("Issue A");
    fireEvent.click(screen.getByRole("button", { name: /Promote to uzi/ }));
    await waitFor(() => expect(screen.getByText("forge said no")).toBeTruthy());

    // Navigate to B: the route-identity reset effect wipes the promote error.
    fireEvent.click(screen.getByRole("button", { name: "go to B" }));
    await screen.findByText("Issue B");
    expect(screen.queryByText("forge said no")).toBeNull();
  });
});

// Issue #1727 review. The action error no longer clears on settle, so an outcome that
// arrives AFTER the user navigated A -> B (same IssueView instance; the route reset effect
// has already run) would land on B. The handlers drop an outcome whose route identity
// changed since the attempt began.
//
// Mutation-checked: removing the route-identity check from the start/promote handlers
// shows A's error on B (observed red: "worker pool is full" / "forge said no" found), and
// removing it from the promote success path replaces B's header with A's ("Issue A").
describe("IssueView — an action outcome cannot land on the next issue (#1727)", () => {
  function NavToB() {
    const navigate = useNavigate();
    return (
      <button type="button" onClick={() => navigate("/repos/repo-1/issues/8")}>
        go to B
      </button>
    );
  }

  function renderAB(labels: string[]) {
    mockApi.listWorkers.mockResolvedValue({ workers: [aWorker()] });
    mockApi.listSecrets.mockResolvedValue({ secrets: [aToken()] });
    mockApi.getIssue.mockImplementation(async (_repo: string, iid: number) => ({
      issue:
        iid === 7
          ? anIssue({ iid: 7, title: "Issue A", labels, has_prd_link: false })
          : anIssue({ iid: 8, title: "Issue B", labels, has_prd_link: false }),
    }));
    render(
      <MemoryRouter initialEntries={["/repos/repo-1/issues/7"]}>
        <Routes>
          <Route
            path="/repos/:repoId/issues/:iid"
            element={
              <>
                <NavToB />
                <IssueView />
              </>
            }
          />
        </Routes>
      </MemoryRouter>,
    );
  }

  it("drops a start failure for A that arrives after navigating to B", async () => {
    setAuth();
    let rejectA!: (e: unknown) => void;
    mockApi.createRun.mockReturnValueOnce(new Promise((_, rej) => (rejectA = rej)));
    renderAB(["uzi"]);

    const startBtn = () => screen.getByRole("button", { name: /start run|starting/i }) as HTMLButtonElement;
    await screen.findByText("Issue A");
    await waitFor(() => expect(startBtn().disabled).toBe(false));
    fireEvent.click(startBtn());
    await waitFor(() => expect(mockApi.createRun).toHaveBeenCalledWith("repo-1", 7, undefined));

    fireEvent.click(screen.getByRole("button", { name: "go to B" }));
    await screen.findByText("Issue B");
    await act(async () => {
      rejectA(new ApiError(500, "worker pool is full"));
    });
    // A's attempt still settles (B's Start is usable again), but its error is dropped.
    await waitFor(() => expect(startBtn().disabled).toBe(false));
    expect(screen.queryByText("worker pool is full")).toBeNull();
    expect(screen.getByText("Issue B")).toBeTruthy();
  });

  it("drops a promote failure for A that arrives after navigating to B", async () => {
    setAuth();
    let rejectA!: (e: unknown) => void;
    mockApi.promoteIssue.mockReturnValueOnce(new Promise((_, rej) => (rejectA = rej)));
    renderAB(["documentation"]);

    await screen.findByText("Issue A");
    fireEvent.click(screen.getByRole("button", { name: /Promote to uzi/ }));
    await waitFor(() => expect(mockApi.promoteIssue).toHaveBeenCalledWith("repo-1", 7));

    fireEvent.click(screen.getByRole("button", { name: "go to B" }));
    await screen.findByText("Issue B");
    await act(async () => {
      rejectA(new ApiError(500, "forge said no"));
    });
    expect(screen.queryByText("forge said no")).toBeNull();
    expect(screen.getByText("Issue B")).toBeTruthy();
  });

  it("drops a promote success for A that arrives after navigating to B", async () => {
    setAuth();
    let resolveA!: (v: { card: ReturnType<typeof aCard> }) => void;
    mockApi.promoteIssue.mockReturnValueOnce(new Promise((res) => (resolveA = res)));
    renderAB(["documentation"]);

    await screen.findByText("Issue A");
    fireEvent.click(screen.getByRole("button", { name: /Promote to uzi/ }));
    await waitFor(() => expect(mockApi.promoteIssue).toHaveBeenCalledWith("repo-1", 7));

    fireEvent.click(screen.getByRole("button", { name: "go to B" }));
    await screen.findByText("Issue B");
    await act(async () => {
      resolveA({ card: aCard(["uzi", "documentation"]) });
    });
    // A's captured issue must not overwrite B's header, nor mark B runnable.
    expect(screen.getByText("Issue B")).toBeTruthy();
    expect(screen.queryByText("Issue A")).toBeNull();
    expect(screen.queryByTitle(/uzi will run it/)).toBeNull();
  });

  // #1727 review (blocking). The reset effect used to leave A's `issue` in place until
  // B's getIssue resolved, so A's header and its Promote/Start buttons stayed live while
  // the route (and routeKeyRef) already read B: a click there acted on A, and a late
  // promote success adopted A's header over B's. The reset now clears `issue` so nothing
  // of A renders while B loads.
  //
  // Fail-before (HEAD 70aa9575): "Issue A" and "Promote to uzi" were still on screen after
  // navigating to B with B's getIssue pending.
  it("shows none of A's header or actions while B loads, and keeps B's header after A's late promote", async () => {
    setAuth();
    let resolveB!: (v: { issue: IssueDetail }) => void;
    const pB = new Promise<{ issue: IssueDetail }>((r) => (resolveB = r));
    mockApi.getIssue.mockImplementation((_repo: string, iid: number) =>
      iid === 7
        ? Promise.resolve({ issue: anIssue({ iid: 7, title: "Issue A", labels: ["documentation"] }) })
        : pB,
    );
    let resolvePromoteA!: (v: { card: ReturnType<typeof aCard> }) => void;
    mockApi.promoteIssue.mockReturnValueOnce(new Promise((res) => (resolvePromoteA = res)));
    render(
      <MemoryRouter initialEntries={["/repos/repo-1/issues/7"]}>
        <Routes>
          <Route
            path="/repos/:repoId/issues/:iid"
            element={
              <>
                <NavToB />
                <IssueView />
              </>
            }
          />
        </Routes>
      </MemoryRouter>,
    );

    await screen.findByText("Issue A");
    fireEvent.click(screen.getByRole("button", { name: /Promote to uzi/ }));
    await waitFor(() => expect(mockApi.promoteIssue).toHaveBeenCalledWith("repo-1", 7));

    fireEvent.click(screen.getByRole("button", { name: "go to B" }));
    await waitFor(() => expect(mockApi.getIssue).toHaveBeenCalledWith("repo-1", 8));
    // B is still loading: nothing of A is left to read or click.
    expect(screen.queryByText("Issue A")).toBeNull();
    expect(screen.queryByRole("button", { name: /Promote to uzi|…/ })).toBeNull();
    expect(screen.queryByRole("button", { name: /start run|starting/i })).toBeNull();
    expect(screen.getByText("Loading issue…")).toBeTruthy();

    await act(async () => {
      resolveB({ issue: anIssue({ iid: 8, title: "Issue B", labels: ["documentation"] }) });
    });
    await screen.findByText("Issue B");
    // B's own Promote is idle, not A's in-flight spinner.
    expect(screen.getByRole("button", { name: /Promote to uzi/ })).toBeTruthy();

    await act(async () => {
      resolvePromoteA({ card: aCard(["uzi", "documentation"]) });
    });
    expect(screen.getByText("Issue B")).toBeTruthy();
    expect(screen.queryByText("Issue A")).toBeNull();
    expect(screen.queryByTitle(/uzi will run it/)).toBeNull();
    expect(mockApi.promoteIssue).toHaveBeenCalledTimes(1);
  });

  // #1727 review (blocking), the Start half. `starting` was not reset on navigation, so
  // B's Start button rendered "Starting…" (and disabled) for as long as A's attempt was
  // in flight.
  //
  // Fail-before (HEAD 70aa9575): B's button read "Starting…" after B loaded.
  it("does not leave B's Start button in A's Starting state", async () => {
    setAuth();
    mockApi.createRun.mockReturnValueOnce(new Promise(() => {}));
    renderAB(["uzi"]);

    const startBtn = () => screen.getByRole("button", { name: /start run|starting/i }) as HTMLButtonElement;
    await screen.findByText("Issue A");
    await waitFor(() => expect(startBtn().disabled).toBe(false));
    fireEvent.click(startBtn());
    await waitFor(() => expect(startBtn().textContent).toBe("Starting…"));

    fireEvent.click(screen.getByRole("button", { name: "go to B" }));
    await screen.findByText("Issue B");
    await waitFor(() => expect(startBtn().disabled).toBe(false));
    expect(startBtn().textContent).toBe("Start run");
    expect(mockApi.createRun).toHaveBeenCalledTimes(1);
  });
});

// PRD #1247 (CodeRabbit finding [5]). IssueView is REUSED across a route-param change
// without remounting (the data effect keys on [repoId, iidNum] and refetches), so the
// picked credential must reset to inherit when the route identity changes — otherwise a
// token pinned for issue A silently carries to issue B's Start run.
describe("IssueView — the picked credential resets on issue navigation (PRD #1247)", () => {
  const aWorker = (): Worker => ({
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
    created_at: "2026-01-01T00:00:00Z",
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
  });
  const aToken = (): SecretMeta => ({
    id: "sec-1",
    label: "console-key",
    is_default: false,
    auto_eligible: true,
    kind: "anthropic_token",
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
  });

  function NavToB() {
    const navigate = useNavigate();
    return (
      <button type="button" onClick={() => navigate("/repos/repo-1/issues/8")}>
        go to B
      </button>
    );
  }

  it("returns the token picker to inherit after navigating to another issue", async () => {
    setAuth();
    mockApi.listWorkers.mockResolvedValue({ workers: [aWorker()] });
    mockApi.listSecrets.mockResolvedValue({ secrets: [aToken()] });
    // Both issues are runnable uzi issues, so the token picker renders for each.
    mockApi.getIssue.mockImplementation(async (_repo: string, iid: number) => ({
      issue:
        iid === 7
          ? anIssue({ iid: 7, title: "Issue A", labels: ["uzi"] })
          : anIssue({ iid: 8, title: "Issue B", labels: ["uzi"] }),
    }));

    render(
      <MemoryRouter initialEntries={["/repos/repo-1/issues/7"]}>
        <Routes>
          <Route
            path="/repos/:repoId/issues/:iid"
            element={
              <>
                <NavToB />
                <IssueView />
              </>
            }
          />
        </Routes>
      </MemoryRouter>,
    );

    await screen.findByText("Issue A");
    // Pin the token for issue A (the user CHANGING the selection).
    const pickerA = screen.getByLabelText("Anthropic token for this run") as HTMLSelectElement;
    const opt = (await within(pickerA).findByRole("option", { name: /console-key/ })) as HTMLOptionElement;
    fireEvent.change(pickerA, { target: { value: opt.value } });
    expect(pickerA.value).toBe("sec-1");

    // Navigate to issue B (same route element, no remount).
    fireEvent.click(screen.getByRole("button", { name: "go to B" }));
    await screen.findByText("Issue B");

    // The pin must not carry over: issue B's picker seeds back to inherit. On the old code
    // the `credential` state survives the route change and this reads "sec-1".
    const pickerB = screen.getByLabelText("Anthropic token for this run") as HTMLSelectElement;
    await waitFor(() => expect(pickerB.value).toBe("mode:inherit"));
  });
});
