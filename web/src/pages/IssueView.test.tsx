// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, render, screen, fireEvent, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { IssueView } from "./IssueView";
import { api, type Card, type IssueDetail, type SecretMeta, type Worker } from "../lib/api";
import { useAuth } from "../auth/AuthContext";

// IssueView loads four endpoints and, for the PRDLESS toggle (PRD #22 M4), calls
// setIssuePrdless. Mock the api and useAuth so the test stays offline.
vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      getIssue: vi.fn(),
      listRuns: vi.fn(),
      listWorkers: vi.fn(),
      listSecrets: vi.fn(),
      setIssuePrdless: vi.fn(),
      promoteIssue: vi.fn(),
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
    labels: ["PRD"],
    web_url: "https://gitlab.example.com/grp/proj/-/issues/7",
    forge_type: "gitlab",
    author: "alice",
    has_prd_link: false,
    column: "",
    closed: false,
    conflict: false,
    description: "no PRD here",
    ...over,
  };
}

function aCard(labels: string[]): Card {
  return {
    iid: 7,
    title: "A small typo fix",
    state: "opened",
    labels,
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

function setAuth(
  prdlessEnabled: boolean,
  opts: { runEligibleLabels?: string[]; eligibleLabelWaivesPrdLink?: boolean } = {},
) {
  vi.mocked(useAuth).mockReturnValue({
    user,
    loading: false,
    prdLabel: "PRD",
    autopilotLabel: "autopilot",
    theme: "ember",
    themeOverride: null,
    defaultTheme: "ember",
    prdlessLabel: "PRDLESS",
    prdlessEnabled,
    // PRD #196 M4 ships `bug` as run-eligible; tests that need a non-eligible label use
    // `documentation`. The waiver defaults on, like the shipped default.
    runEligibleLabels: opts.runEligibleLabels ?? ["PRD", "bug"],
    eligibleLabelWaivesPrdLink: opts.eligibleLabelWaivesPrdLink ?? true,
    vaultUnlocked: true,
    vaultExists: true,
    hasPassword: true,
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
// through <Markdown>, which is hardened against raw HTML and dangerous URL schemes —
// neither of which is a bidi override, so that pipeline does not close this.
describe("IssueView — the forge title and description carry no format characters (#124)", () => {
  it("strips bidi/zero-width characters from both", async () => {
    setAuth(false);
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

describe("IssueView PRDLESS toggle (PRD #22 M4)", () => {
  it("hides the toggle when the feature is disabled", async () => {
    setAuth(false);
    mockApi.getIssue.mockResolvedValue({ issue: anIssue() });
    renderIssueView();
    await screen.findByText("A small typo fix");
    expect(screen.queryByText(/PRDLESS/)).toBeNull();
  });

  it("applies the label and adopts the returned card's labels", async () => {
    setAuth(true);
    mockApi.getIssue.mockResolvedValue({ issue: anIssue() });
    mockApi.setIssuePrdless.mockResolvedValue({ card: aCard(["PRD", "PRDLESS"]) });
    renderIssueView();

    const applyBtn = await screen.findByText("Mark PRDLESS");
    fireEvent.click(applyBtn);

    await waitFor(() =>
      expect(mockApi.setIssuePrdless).toHaveBeenCalledWith("repo-1", 7, true),
    );
    // After the 200, the label is present, so the affordance flips to "Remove".
    await screen.findByText("Remove PRDLESS");
  });

  it("removes the label when it is already applied", async () => {
    setAuth(true);
    mockApi.getIssue.mockResolvedValue({ issue: anIssue({ labels: ["PRD", "PRDLESS"] }) });
    mockApi.setIssuePrdless.mockResolvedValue({ card: aCard(["PRD"]) });
    renderIssueView();

    const removeBtn = await screen.findByText("Remove PRDLESS");
    fireEvent.click(removeBtn);

    await waitFor(() =>
      expect(mockApi.setIssuePrdless).toHaveBeenCalledWith("repo-1", 7, false),
    );
    await screen.findByText("Mark PRDLESS");
  });
});

describe("IssueView PRDLESS badge (PRD #22 M3)", () => {
  // The badge is queried by its distinctive title rather than its text. That used to
  // be necessary because the issue ALSO rendered a label chip named "PRDLESS" and the
  // title was the only thing telling the two apart; since PRD #102 M4 the shared
  // chipLabels predicate excludes PRDLESS unconditionally, so there is no chip to
  // collide with. Kept as the query anyway: it is the assertion that does not move
  // when the label is renamed in settings.
  const BADGE_TITLE = "PRD-link gate bypassed by label";

  it("shows the PRDLESS badge, not 'no PRD link', when enabled and labeled", async () => {
    setAuth(true);
    mockApi.getIssue.mockResolvedValue({ issue: anIssue({ labels: ["PRD", "PRDLESS"] }) });
    renderIssueView();
    await screen.findByText("A small typo fix");
    expect(screen.getByTitle(BADGE_TITLE)).toBeTruthy();
    expect(screen.queryByText("no PRD link")).toBeNull();
  });

  it("shows 'no PRD link' when enabled but the label is absent", async () => {
    setAuth(true);
    mockApi.getIssue.mockResolvedValue({ issue: anIssue({ labels: ["PRD"] }) });
    renderIssueView();
    await screen.findByText("A small typo fix");
    expect(screen.getByText("no PRD link")).toBeTruthy();
    expect(screen.queryByTitle(BADGE_TITLE)).toBeNull();
  });

  it("shows 'no PRD link' when the feature is disabled even if labeled", async () => {
    setAuth(false);
    mockApi.getIssue.mockResolvedValue({ issue: anIssue({ labels: ["PRD", "PRDLESS"] }) });
    renderIssueView();
    await screen.findByText("A small typo fix");
    expect(screen.getByText("no PRD link")).toBeTruthy();
    expect(screen.queryByTitle(BADGE_TITLE)).toBeNull();
  });
});

// PRD #102 M4. The issue view had its own ad-hoc chip filter (column, plus PRDLESS only
// while its badge showed), so PRD and autopilot chips rendered here. Not "and not on the
// board", which is how this read until the fact-check: before a02e3184 the board
// rendered no chips AT ALL, so there was no predicate divergence to describe — M4 gave
// the board chips and gave both surfaces one predicate in the same change.
describe("IssueView label chips (PRD #102 M4)", () => {
  it("chips content labels and drops every workflow marker", async () => {
    setAuth(true);
    mockApi.getIssue.mockResolvedValue({
      issue: anIssue({
        labels: ["PRD", "autopilot", "PRDLESS", "In Progress", "bug"],
        column: "In Progress",
        has_prd_link: true,
      }),
    });
    renderIssueView();
    await screen.findByText("A small typo fix");
    expect(screen.getByText("bug")).toBeTruthy();
    expect(screen.queryByText("PRD")).toBeNull();
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
    setAuth(true);
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
    setAuth(true);
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
    setAuth(true);
    mockApi.getIssue.mockResolvedValue({
      issue: anIssue({ labels: ["PRD", "autopilot"], has_prd_link: true }),
    });
    renderIssueView();
    await screen.findByText("A small typo fix");
    expect(screen.getByTitle(/Autopilot/)).toBeTruthy();
    expect(screen.getByText("autopilot")).toBeTruthy();
  });

  it("shows nothing when the label is absent", async () => {
    setAuth(true);
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
    setAuth(true);
    mockApi.getIssue.mockResolvedValue({
      issue: anIssue({ labels: ["PRD", "autopilot"], has_prd_link: true }),
    });
    renderIssueView();
    await screen.findByText("A small typo fix");
    expect(screen.getAllByText("autopilot")).toHaveLength(1);
    expect(screen.getByText("autopilot").getAttribute("title")).toMatch(/Autopilot/);
  });
});

describe("IssueView Start gate honors the PRDLESS bypass (PRD #22 B1)", () => {
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

  it("enables Start once the PRDLESS label is applied on a no-PRD-link issue", async () => {
    setAuth(true);
    // Make the missing PRD link the ONLY blocker: give the user a worker + token.
    mockApi.listWorkers.mockResolvedValue({ workers: [aWorker()] });
    mockApi.listSecrets.mockResolvedValue({ secrets: [aToken()] });
    mockApi.getIssue.mockResolvedValue({ issue: anIssue({ labels: ["PRD"] }) }); // no link, no label
    mockApi.setIssuePrdless.mockResolvedValue({ card: aCard(["PRD", "PRDLESS"]) });
    renderIssueView();

    const startBtn = () => screen.getByRole("button", { name: /start run/i }) as HTMLButtonElement;
    await screen.findByText("A small typo fix");
    // Gated on the missing PRD link, despite the worker + token being present.
    expect(startBtn().disabled).toBe(true);

    fireEvent.click(screen.getByText("Mark PRDLESS"));
    // Once the label lands, the bypass enables Start.
    await waitFor(() => expect(startBtn().disabled).toBe(false));
  });
});

// PRD #102 M6. M4 removed the PRD chip from this page — correctly, since every
// cached issue carried the label by construction and the chip carried no
// information. M6 is what gives it information again: without an indicator here, a
// non-PRD issue opened from the board is indistinguishable from a PRD one on the
// page where a user decides what to do with it, and which offers the run controls
// the server-side gate now refuses.
describe("IssueView — eligibility affordances (PRD #102 M6, widened by PRD #196 M4)", () => {
  // Minimal worker + token so the missing PRD link is the ONLY thing a gate blocks on.
  const withWorkerAndToken = () => {
    mockApi.listWorkers.mockResolvedValue({ workers: [{ id: "w1" } as unknown as Worker] });
    mockApi.listSecrets.mockResolvedValue({ secrets: [{ kind: "anthropic_token" } as unknown as SecretMeta] });
  };

  it("marks a NON-ELIGIBLE issue and offers Promote in place of Start run", async () => {
    // `documentation` is not in the eligible set, so the issue is not runnable.
    setAuth(false);
    mockApi.getIssue.mockResolvedValue({ issue: anIssue({ labels: ["documentation"] }) });
    renderIssueView();
    await screen.findByText("A small typo fix");

    expect(screen.getByText("not PRD")).toBeTruthy();
    expect(screen.getByRole("button", { name: /Promote to PRD/ })).toBeTruthy();
    // Hidden, not disabled: the server refuses the run and Promote is the one-click
    // answer, so a gated button explaining a resolvable rule would be noise.
    expect(screen.queryByRole("button", { name: /start run/i })).toBeNull();
  });

  it("offers Start run on an ELIGIBLE bug issue, not Promote or 'not PRD' (mock §4)", async () => {
    // `bug` is in the shipped eligible set, so the issue is runnable: Start run shows,
    // the non-eligible marker and Promote do not.
    setAuth(false);
    mockApi.getIssue.mockResolvedValue({ issue: anIssue({ labels: ["bug"] }) });
    renderIssueView();
    await screen.findByText("A small typo fix");

    expect(screen.queryByText("not PRD")).toBeNull();
    expect(screen.queryByRole("button", { name: /Promote to PRD/ })).toBeNull();
    expect(screen.getByRole("button", { name: /start run/i })).toBeTruthy();
  });

  it("shows neither marker nor Promote on an ordinary PRD issue", async () => {
    setAuth(false);
    mockApi.getIssue.mockResolvedValue({ issue: anIssue({ labels: ["PRD"] }) });
    renderIssueView();
    await screen.findByText("A small typo fix");

    expect(screen.queryByText("not PRD")).toBeNull();
    expect(screen.queryByRole("button", { name: /Promote to PRD/ })).toBeNull();
    expect(screen.getByRole("button", { name: /start run/i })).toBeTruthy();
  });

  it("enables Start on an eligible bug issue with no PRD link when the waiver is on (PRD #196 M4)", async () => {
    // The waiver mirrors the server: an issue eligible via a NON-PRIMARY label does not
    // need a prds/*.md link. With worker + token, the missing link is the only blocker.
    setAuth(false);
    withWorkerAndToken();
    mockApi.getIssue.mockResolvedValue({ issue: anIssue({ labels: ["bug"] }) }); // has_prd_link:false
    renderIssueView();
    const startBtn = () => screen.getByRole("button", { name: /start run/i }) as HTMLButtonElement;
    await screen.findByText("A small typo fix");
    await waitFor(() => expect(startBtn().disabled).toBe(false));
  });

  it("GATES the same bug issue when eligibleLabelWaivesPrdLink is false (PRD #196 M4)", async () => {
    // The scope proof: with the waiver off, the no-link bug issue is blocked on the link
    // again, even though it is still eligible (Start run is shown, just disabled).
    setAuth(false, { eligibleLabelWaivesPrdLink: false });
    withWorkerAndToken();
    mockApi.getIssue.mockResolvedValue({ issue: anIssue({ labels: ["bug"] }) });
    renderIssueView();
    const startBtn = () => screen.getByRole("button", { name: /start run/i }) as HTMLButtonElement;
    await screen.findByText("A small typo fix");
    await waitFor(() => expect(startBtn().disabled).toBe(true));
  });

  it("offers the PRDLESS toggle on an eligible bug issue, not on a non-eligible one (Decision 16, PRD #196 M4)", async () => {
    // prdlessEnabled AND has_prd_link false. On an ELIGIBLE bug issue the toggle shows
    // (the run gate lets it through, so PRDLESS can waive the link)...
    setAuth(true);
    mockApi.getIssue.mockResolvedValue({ issue: anIssue({ labels: ["bug"] }) });
    const { unmount } = renderIssueView();
    await screen.findByText("A small typo fix");
    expect(screen.getByText("Mark PRDLESS")).toBeTruthy();
    unmount();

    // ...but on a NON-eligible documentation issue it grants nothing and is hidden.
    setAuth(true);
    mockApi.getIssue.mockResolvedValue({ issue: anIssue({ labels: ["documentation"] }) });
    renderIssueView();
    await screen.findByText("A small typo fix");
    expect(screen.queryByText("Mark PRDLESS")).toBeNull();
  });

  it("promotes forge-first and adopts the returned labels", async () => {
    setAuth(false);
    mockApi.getIssue.mockResolvedValue({ issue: anIssue({ labels: ["documentation"] }) });
    mockApi.promoteIssue.mockResolvedValue({ card: aCard(["PRD", "documentation"]) });
    renderIssueView();
    await screen.findByText("A small typo fix");

    fireEvent.click(screen.getByRole("button", { name: /Promote to PRD/ }));
    await waitFor(() => expect(mockApi.promoteIssue).toHaveBeenCalledWith("repo-1", 7));
    // The page re-reads as an ordinary PRD issue: the marker goes, Start run appears.
    await waitFor(() => expect(screen.queryByText("not PRD")).toBeNull());
    expect(screen.getByRole("button", { name: /start run/i })).toBeTruthy();
  });

  it("does not offer Promote on the self-improve tracker (Decision 13a)", async () => {
    setAuth(false);
    mockApi.getIssue.mockResolvedValue({ issue: anIssue({ labels: ["uzi-self-improve"] }) });
    renderIssueView();
    await screen.findByText("A small typo fix");

    // Still MARKED as not-PRD (it is not uzi's board work), but not promotable — the
    // server refuses it too, so an offered button would be a 422 waiting to happen.
    expect(screen.getByText("not PRD")).toBeTruthy();
    expect(screen.queryByRole("button", { name: /Promote to PRD/ })).toBeNull();
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
    setAuth(false);
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
    setAuth(false);
    mockApi.getIssue.mockResolvedValue({ issue: anIssue({ column: "In Progress" }) });
    renderIssueView();
    await screen.findByText("A small typo fix");
    expect(screen.getByText("In Progress")).toBeTruthy();
  });
});
