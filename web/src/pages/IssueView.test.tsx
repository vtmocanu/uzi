// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, render, screen, fireEvent, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { IssueView } from "./IssueView";
import { api, ApiError, type Card, type IssueDetail, type Run, type SecretMeta, type Worker } from "../lib/api";
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
  judge_anthropic_secret_id: null,
  judge_anthropic_secret_label: null,
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

function setAuth() {
  vi.mocked(useAuth).mockReturnValue({
    user,
    loading: false,
    uziLabel: "uzi",
    autopilotLabel: "autopilot",
    theme: "ember",
    themeOverride: null,
    defaultTheme: "ember",
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
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
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

describe("IssueView Start gate (PRD #764)", () => {
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
    // cleared (button re-enabled). NOTE: unlike Board, IssueView's forced-retry
    // failure toast does NOT persist — startRun's catch calls load(), whose first
    // line is setError(""), which wipes the just-set message. This is existing
    // control flow (the spec said keep it as-is), so this test asserts the real
    // behavior; the Board/IssueView divergence is flagged back to the lead.
    await waitFor(() => expect(mockApi.createRun).toHaveBeenCalledTimes(2));
    expect(mockApi.createRun.mock.calls[1]).toEqual(["repo-1", 7, true]);
    await waitFor(() => expect(startBtn().disabled).toBe(false));
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
