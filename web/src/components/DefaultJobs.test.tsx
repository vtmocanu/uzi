// @vitest-environment jsdom
//
// DefaultJobs (PRD #589 M6): the Default-jobs tab renders the catalog in the shared
// table shape, gates Enable behind a repo choice (success criterion 1), and collapses a
// default enabled on several repos into one summary row that expands to per-repo sub-rows
// (Layout A). It calls checkRepoLabels via SweepLabelWarn, so the api is mocked.
//
// PRD #636 M5 polish: the redundant per-row DEFAULT chip was dropped (the tab header
// already says "Default jobs"), and the Target cell keeps name + lock + type pill in one
// flex-wrap container ahead of the description so the pill never dangles under it. The
// lock marker is retained (it encodes a baked/read-only prompt).
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { DefaultJobs } from "./DefaultJobs";
import { api, type CatalogEntry, type LastFire, type Repo, type Schedule, type ScheduleCatalog } from "../lib/api";

// An expanded LastFireDetail sub-row reads useAuth().uziLabel (PRD #764 follow-up); this
// tree is not otherwise auth-aware, so a minimal stub keeps it out of an AuthProvider.
vi.mock("../auth/AuthContext", () => ({ useAuth: () => ({ uziLabel: "uzi" }) }));

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      checkRepoLabels: vi.fn(),
      ensureRepoLabels: vi.fn(),
    },
  };
});
const mockApi = vi.mocked(api);

beforeEach(() => {
  mockApi.checkRepoLabels.mockResolvedValue({ missing: [] });
  mockApi.ensureRepoLabels.mockResolvedValue({ ensured: [] });
});
afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

const REPOS: Repo[] = [
  { id: "repo-uzi", path_with_namespace: "vtmocanu/uzi" } as Repo,
  { id: "repo-atlas", path_with_namespace: "vtmocanu/atlas-api" } as Repo,
];

function entry(over: Partial<CatalogEntry> = {}): CatalogEntry {
  return {
    slug: "bug-triage",
    name: "Bug triage sweep",
    description: "Daily bug sweep",
    target: "sweep",
    cron: "0 2 * * *",
    timezone: "UTC",
    model: "",
    prompt: "",
    labels: ["bug"],
    guidance: "Triage the bug.",
    max_issues: 3,
    auto_approve: true,
    wait_on_limit: true,
    ...over,
  };
}

// A materialized default row (origin='default') for a slug on a repo.
function defRow(over: Partial<Schedule>): Schedule {
  return {
    id: "sch-x",
    repo_id: "repo-uzi",
    repo_path: "vtmocanu/uzi",
    target: "sweep",
    issue_iid: null,
    labels: ["bug"],
    prompt: "",
    timing: "recurring",
    cron_expr: "0 2 * * *",
    run_at: null,
    timezone: "UTC",
    next_fire_at: null,
    last_fired_at: null,
    last_fire: null,
    auto_approve: true,
    wait_on_limit: true,
    max_issues: 3,
    // A sweep default's baked catalog guidance is the read-only baked_guidance (issue #675);
    // guidance is the owner overlay (null until set).
    guidance: null,
    baked_guidance: "Triage the bug.",
    model: null,
    override_subagent_model: false,
    enabled: true,
    status: "active",
    origin: "default",
    catalog_slug: "bug-triage",
    customized: false,
    sibling_group_id: null,
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    next_fires: [],
    ...over,
  };
}

function catalog(entries: CatalogEntry[], enablements: ScheduleCatalog["enablements"] = []): ScheduleCatalog {
  return { entries, enablements };
}

function noop() {}
const asyncNoop = async () => {};

function renderTab(props: Partial<Parameters<typeof DefaultJobs>[0]> = {}) {
  // Wrapped in a router: a sub-row's expanded LastFireDetail renders <Link> run chips.
  return render(
    <MemoryRouter>
      <DefaultJobs
        catalog={props.catalog ?? catalog([entry()])}
        schedules={props.schedules ?? []}
        repos={props.repos ?? REPOS}
        busyId=""
        onEnable={props.onEnable ?? asyncNoop}
        onTogglePause={noop}
        onRunNow={noop}
        onReset={noop}
        onClone={noop}
        onRemove={noop}
        onEdit={noop}
        notice=""
        error=""
      />
    </MemoryRouter>,
  );
}

describe("DefaultJobs — catalog row", () => {
  it("drops the redundant DEFAULT chip but keeps the baked-prompt lock marker (PRD #636 M5)", () => {
    renderTab();
    // Positive: the row actually rendered (so the null check below is discriminating, not
    // vacuously passing because nothing mounted).
    expect(screen.getByText("Bug triage sweep")).toBeTruthy();
    // The 🔒 baked-prompt marker is retained — it encodes a baked/read-only prompt.
    expect(screen.getByLabelText("Baked prompt, read-only")).toBeTruthy();
    // The per-row DEFAULT chip is gone (redundant with the tab header).
    expect(screen.queryByText("DEFAULT")).toBeNull();
  });

  it("keeps the type pill and lock inside the name's flex container, ahead of the description (PRD #636 M5)", () => {
    renderTab();
    // The name span's parent is the flex-wrap container holding name + lock + type pill.
    const nameContainer = screen.getByText("Bug triage sweep").parentElement;
    expect(nameContainer).not.toBeNull();
    // The lock marker and the type pill are siblings inside that container.
    expect(within(nameContainer as HTMLElement).getByLabelText("Baked prompt, read-only")).toBeTruthy();
    expect(within(nameContainer as HTMLElement).getByText("sweep")).toBeTruthy();
    // The description is NOT inside the name container...
    expect(within(nameContainer as HTMLElement).queryByText("Daily bug sweep")).toBeNull();
    // ...and follows the name container in document order (jsdom has no layout engine, so
    // this asserts DOM structure/order, not visual non-wrap).
    const description = screen.getByText("Daily bug sweep");
    expect(
      (nameContainer as HTMLElement).compareDocumentPosition(description) & Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
  });

  it("row Enable is ABSENT with no repo picked and APPEARS once an actionable repo is selected", async () => {
    renderTab();
    // No repo picked and the row is collapsed: the row Enable button does not render (it
    // only appears when clicking it would do something). EnableAnotherRepo's "Enable" lives
    // in the expanded children, and the disclosure is named "Show repos for …", so neither
    // matches /Enable/ here.
    expect(screen.queryByRole("button", { name: /Enable/ })).toBeNull();

    // Pick an actionable repo via the multi-select checkbox (labelled by its path).
    fireEvent.click(screen.getByLabelText("vtmocanu/uzi"));

    // The row Enable button now appears and is not disabled.
    const enable = await screen.findByRole("button", { name: /Enable/ });
    expect(enable.hasAttribute("disabled")).toBe(false);
    // The "N repos → N schedules" hint reflects the pick.
    expect(screen.getByText(/1 repo → 1 schedule/)).toBeTruthy();
  });

  it("no row Enable when the only selected repo is already enabled on the job", () => {
    // The job is already enabled on repo-uzi; picking only repo-uzi leaves the actionable
    // set empty, so the row Enable button must not render.
    renderTab({
      schedules: [defRow({ id: "sch-uzi", repo_id: "repo-uzi", repo_path: "vtmocanu/uzi" })],
    });
    fireEvent.click(screen.getByLabelText("vtmocanu/uzi"));

    // Collapsed row: the disclosure toggle is named "Show repos for …", so it won't match
    // /Enable/. No actionable repo → no row Enable button.
    expect(screen.queryByRole("button", { name: /Enable/ })).toBeNull();
  });

  it("the disclosure toggle shows no count text in the Default jobs tab (the green pill carries it)", () => {
    // An enabled default row renders the disclosure toggle (repoCount > 0). The Default-jobs
    // variant passes showDisclosureCount={false}, so the toggle degrades to a bare chevron —
    // the "N repos" count lives only in the green Next-run pill.
    renderTab({
      schedules: [defRow({ id: "sch-uzi", repo_id: "repo-uzi", repo_path: "vtmocanu/uzi" })],
    });

    const toggle = screen.getByRole("button", { name: /Show repos for/ });
    // No visible count text inside the toggle button. (Scoped to the button: the green Badge
    // in the Next-run cell also matches /repo/, so a bare queryByText would find the pill.)
    expect(within(toggle).queryByText(/repo/)).toBeNull();
    // ...but the count is still shown — in the green pill, not the toggle. Scope the
    // assertion to the Next-run table cell so it fails if the badge (or its count) is
    // removed and the text merely reappears elsewhere in the row.
    const nextRunCell = screen.getByRole("cell", { name: /1 repo/ });
    expect(within(nextRunCell).getByText("1 repo")).toBeTruthy();
  });

  it("renders a labelless assigned sweep without crashing", () => {
    // Regression for the /schedules black-screen: the shipped assigned-sweep default carries
    // labels: null, and the options cell read entry.labels.map() unguarded, so the tab threw
    // on first render. Rendering the row must not throw, and with no labels no "label …" chip
    // is emitted.
    renderTab({
      catalog: catalog([entry({ slug: "assigned-sweep", name: "Assigned sweep", labels: null })]),
    });
    // The row rendered (so the chip assertion below is discriminating, not vacuous).
    expect(screen.getByText("Assigned sweep")).toBeTruthy();
    // No selector-label chip is rendered for a labelless sweep.
    expect(screen.queryByText(/^label /)).toBeNull();
  });

  it("row Enable fans out only the actionable subset of the selection", async () => {
    // The job is already enabled on repo-uzi. Picking BOTH repos should enable only the
    // actionable one (repo-atlas), never re-enable the already-materialized repo-uzi.
    const onEnable = vi.fn(async () => {});
    renderTab({
      schedules: [defRow({ id: "sch-uzi", repo_id: "repo-uzi", repo_path: "vtmocanu/uzi" })],
      onEnable,
    });

    // Select both repos before clicking Enable (the top picker re-renders per pick).
    fireEvent.click(screen.getByLabelText("vtmocanu/uzi"));
    fireEvent.click(screen.getByLabelText("vtmocanu/atlas-api"));

    fireEvent.click(screen.getByRole("button", { name: /Enable/ }));
    await waitFor(() =>
      expect(onEnable).toHaveBeenCalledWith(expect.anything(), ["repo-atlas"]),
    );
  });
});

describe("DefaultJobs — Layout A (one summary row, per-repo sub-rows)", () => {
  it("a default enabled on 2 repos renders one summary row that expands to 2 sub-rows", async () => {
    const schedules = [
      defRow({ id: "sch-uzi", repo_id: "repo-uzi", repo_path: "vtmocanu/uzi" }),
      defRow({ id: "sch-atlas", repo_id: "repo-atlas", repo_path: "vtmocanu/atlas-api", enabled: false }),
    ];
    renderTab({ schedules });

    // Collapsed: the per-repo sub-rows are not rendered yet. The repo paths also appear in
    // the guardrail RepoMultiSelect, so key the collapsed check on the sub-row-only pause/
    // resume toggles instead — non-vacuous against the expanded state below.
    expect(screen.queryByRole("switch", { name: "Resume on vtmocanu/atlas-api" })).toBeNull();
    expect(screen.queryByRole("switch", { name: "Pause on vtmocanu/uzi" })).toBeNull();

    // Expand via the "2 repos" disclosure.
    fireEvent.click(screen.getByRole("button", { name: /Show repos for Bug triage sweep/ }));

    // Two per-repo sub-rows appear, one per enabled repo — the paused repo shows a resume
    // toggle (never a fresh enable), the active one a pause toggle.
    await waitFor(() =>
      expect(screen.getByRole("switch", { name: "Resume on vtmocanu/atlas-api" })).toBeTruthy(),
    );
    expect(screen.getByRole("switch", { name: "Pause on vtmocanu/uzi" })).toBeTruthy();
  });

  it("the 'enable on another repo' picker offers only repos not yet materialized for the slug", async () => {
    const schedules = [defRow({ id: "sch-uzi", repo_id: "repo-uzi", repo_path: "vtmocanu/uzi" })];
    renderTab({ schedules });
    fireEvent.click(screen.getByRole("button", { name: /Show repos for Bug triage sweep/ }));

    const picker = await screen.findByRole("combobox", { name: /Enable Bug triage sweep on another repo/ });
    // repo-atlas (not materialized) is offered; repo-uzi (materialized) is not.
    expect(within(picker).getByRole("option", { name: "vtmocanu/atlas-api" })).toBeTruthy();
    expect(within(picker).queryByRole("option", { name: "vtmocanu/uzi" })).toBeNull();
  });
});

// ── issue #690: per-repo last-run parity on default sub-rows ────────────────────
const NOW = new Date().toISOString();
function fire(over: Partial<LastFire>): LastFire {
  return { fired_at: NOW, matched: 0, capped: false, started: [], skips: [], ...over };
}

describe("DefaultJobs — last-run parity on sub-rows (issue #690)", () => {
  it("an enabled default sub-row with a last_fire shows the outcome badge and expands to the fire detail", async () => {
    const schedules = [
      defRow({
        id: "sch-uzi",
        repo_id: "repo-uzi",
        repo_path: "vtmocanu/uzi",
        last_fire: fire({
          matched: 1,
          started: [{ issue_iid: 7, run_id: "77777777-0000-0000-0000-000000000000", title: "started thing" }],
        }),
      }),
    ];
    renderTab({ schedules });

    // Expand the summary to reveal the single per-repo sub-row.
    fireEvent.click(screen.getByRole("button", { name: /Show repos for Bug triage sweep/ }));
    await waitFor(() => expect(screen.getByRole("switch", { name: "Pause on vtmocanu/uzi" })).toBeTruthy());

    // Collapsed sub-row: the enriched green outcome badge, no detail panel yet (non-vacuous
    // against the expanded assertion below).
    expect(screen.getByText("1 started")).toBeTruthy();
    expect(screen.queryByText("started thing")).toBeNull();

    // One sub-row, so the "Last fire" disclosure is unambiguous. Expanding reveals the
    // started run (LastFireDetail) below the flex row.
    fireEvent.click(screen.getByRole("button", { name: "Last fire" }));
    expect(screen.getByText("started thing")).toBeTruthy();
  });

  it("an enabled default sub-row that never fired shows the '— never fired' fallback and offers no disclosure", async () => {
    const schedules = [
      defRow({ id: "sch-uzi", repo_id: "repo-uzi", repo_path: "vtmocanu/uzi", last_fire: null, last_fired_at: null }),
    ];
    renderTab({ schedules });

    fireEvent.click(screen.getByRole("button", { name: /Show repos for Bug triage sweep/ }));
    await waitFor(() => expect(screen.getByRole("switch", { name: "Pause on vtmocanu/uzi" })).toBeTruthy());

    // Scope to the sub-row (the group summary's Last-run cell also shows "— never fired"
    // when no member has fired). The never-fired fallback is present and offers no disclosure.
    const subRow = screen.getByRole("switch", { name: "Pause on vtmocanu/uzi" }).closest<HTMLElement>("div.rounded-lg")!;
    expect(within(subRow).getByText("— never fired")).toBeTruthy();
    expect(within(subRow).queryByRole("button", { name: "Last fire" })).toBeNull();
  });
});

describe("DefaultJobs — sweep-warn after enabling", () => {
  it("arms the sweep-warn for a repo missing the selector label, and Create calls ensureRepoLabels", async () => {
    mockApi.checkRepoLabels.mockResolvedValue({ missing: ["bug"] });
    const onEnable = vi.fn(async () => {});
    renderTab({ onEnable });

    // Pick a repo and enable the sweep default.
    fireEvent.click(screen.getByLabelText("vtmocanu/atlas-api"));
    fireEvent.click(screen.getByRole("button", { name: /Enable/ }));
    await waitFor(() => expect(onEnable).toHaveBeenCalled());

    // The warn surfaces for the enabled repo's missing "bug" label...
    const create = await screen.findByRole("button", { name: /Create label “bug”/ });
    fireEvent.click(create);
    await waitFor(() =>
      expect(mockApi.ensureRepoLabels).toHaveBeenCalledWith("repo-atlas", ["bug"]),
    );
  });
});
