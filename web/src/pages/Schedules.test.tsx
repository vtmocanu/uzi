// @vitest-environment jsdom
//
// Schedules list page (PRD #241 M5, mock §1): rows render per target/timing, and the
// per-row enable toggle PATCHes { enabled } and adopts the server's returned row.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { Schedules } from "./Schedules";
import { api, type LastFire, type Schedule } from "../lib/api";
import { useAuth } from "../auth/AuthContext";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      listSchedules: vi.fn(),
      listScheduleCatalog: vi.fn(),
      listRepos: vi.fn(),
      updateSchedule: vi.fn(),
      runScheduleNow: vi.fn(),
      cloneSchedule: vi.fn(),
      resetSchedule: vi.fn(),
      enableCatalogSchedule: vi.fn(),
      deleteSchedule: vi.fn(),
      addScheduleRepo: vi.fn(),
      // The clone flow opens the edit modal on the new row, which mounts ScheduleModal;
      // stub its preview + checkRepoLabels so the modal's effects don't hit undefined.
      previewSchedule: vi.fn(),
      checkRepoLabels: vi.fn(),
    },
  };
});

// The clone action opens ScheduleModal (which reads useAuth); stub it so the modal can
// mount outside an AuthProvider.
vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));

const mockApi = vi.mocked(api);

const EMPTY_CATALOG = { entries: [], enablements: [] };

function sched(over: Partial<Schedule>): Schedule {
  return {
    id: "s1",
    repo_id: "repo-uzi",
    repo_path: "vtmocanu/uzi",
    target: "sweep",
    issue_iid: null,
    labels: null,
    prompt: "",
    timing: "recurring",
    cron_expr: "0 2 * * 1-5",
    run_at: null,
    timezone: "Europe/Bucharest",
    next_fire_at: new Date(Date.now() + 3_600_000).toISOString(),
    last_fired_at: null,
    last_fire: null,
    auto_approve: true,
    wait_on_limit: true,
    max_issues: 10,
    guidance: null,
    model: null,
    override_subagent_model: false,
    enabled: true,
    status: "active",
    origin: "user",
    catalog_slug: null,
    customized: false,
    sibling_group_id: null,
    created_at: new Date().toISOString(),
    updated_at: new Date().toISOString(),
    next_fires: [],
    ...over,
  };
}

beforeEach(() => {
  mockApi.listSchedules.mockResolvedValue([
    sched({ id: "s1", target: "sweep", enabled: true }),
    sched({ id: "s2", target: "prompt", prompt: "hunt flaky tests", enabled: false, cron_expr: "0 9 * * 1" }),
  ]);
  mockApi.listScheduleCatalog.mockResolvedValue(EMPTY_CATALOG);
  mockApi.listRepos.mockResolvedValue({ repos: [] } as Awaited<ReturnType<typeof api.listRepos>>);
  mockApi.previewSchedule.mockResolvedValue({ fires: [] });
  mockApi.checkRepoLabels.mockResolvedValue({ missing: [] });
  vi.mocked(useAuth).mockReturnValue({ prdLabel: "PRD" } as unknown as ReturnType<typeof useAuth>);
});
afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

// The page opens on the Default jobs tab; the rows under test live in My schedules,
// so switch there. The tab bar renders synchronously (not gated on load), so a click
// before the fetch resolves just seeds tab state for when the table paints.
function renderPage() {
  const utils = render(
    <MemoryRouter>
      <Schedules />
    </MemoryRouter>,
  );
  fireEvent.click(screen.getByRole("tab", { name: /My schedules/ }));
  return utils;
}

describe("Schedules list", () => {
  it("renders a row per schedule with its target badge and cron", async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText("Sweep eligible PRD issues")).toBeTruthy());
    expect(screen.getByText("Prompt: hunt flaky tests")).toBeTruthy();
    // The cron string is shown verbatim (canonical source of truth).
    expect(screen.getByText("0 2 * * 1-5")).toBeTruthy();
    // A paused (disabled) schedule shows the paused pill in Next run.
    expect(screen.getByText("paused")).toBeTruthy();
  });

  it("suppresses the auto-approve chip for a self_improve row (PRD #590 follow-up 2)", async () => {
    // A self_improve run is always server-forced to auto_approve, so the chip is not a
    // user option: with wait_on_limit off, the row falls back to "defaults", not a chip.
    only1({ target: "self_improve", origin: "user", auto_approve: true, wait_on_limit: false });
    renderPage();
    await waitFor(() => expect(screen.getByText("Self-improvement")).toBeTruthy());
    expect(screen.queryByText("auto-approve")).toBeNull();
    expect(screen.getByText("defaults")).toBeTruthy();
  });

  it("still shows the auto-approve chip for a non-self_improve row (control)", async () => {
    only1({ target: "sweep", origin: "user", auto_approve: true, wait_on_limit: false });
    renderPage();
    await waitFor(() => expect(screen.getByText("Sweep eligible PRD issues")).toBeTruthy());
    expect(screen.getByText("auto-approve")).toBeTruthy();
  });

  it("the enable toggle PATCHes { enabled } and adopts the returned row", async () => {
    mockApi.updateSchedule.mockImplementation(async (id: string, input) =>
      sched({ id, enabled: input.enabled ?? true }),
    );
    renderPage();
    await waitFor(() => expect(screen.getByText("Sweep eligible PRD issues")).toBeTruthy());

    // s1 is enabled; disabling it sends { enabled: false }.
    const toggle = screen.getByRole("switch", { name: "Disable schedule" });
    fireEvent.click(toggle);
    await waitFor(() => expect(mockApi.updateSchedule).toHaveBeenCalledWith("s1", { enabled: false }));
  });
});

// ── PRD #589 M6: the two-tab split (Default jobs vs My schedules) ──────────────
describe("Schedules — two tabs (PRD #589 M6)", () => {
  const CATALOG = {
    entries: [
      {
        slug: "bug-triage",
        name: "Bug triage sweep",
        description: "Daily bug sweep",
        target: "sweep" as const,
        cron: "0 2 * * *",
        timezone: "UTC",
        model: "",
        prompt: "",
        labels: ["bug"],
        guidance: "Triage the bug.",
        max_issues: 3,
        auto_approve: true,
        wait_on_limit: true,
      },
    ],
    enablements: [],
  };

  it("opens on Default jobs (catalog entry shown, user rows hidden) and switches to My schedules", async () => {
    mockApi.listScheduleCatalog.mockResolvedValue(CATALOG);
    mockApi.listSchedules.mockResolvedValue([
      sched({ id: "u1", target: "prompt", prompt: "my own flaky hunt", origin: "user" }),
    ]);
    render(
      <MemoryRouter>
        <Schedules />
      </MemoryRouter>,
    );

    // Default jobs is the initial tab: the catalog entry renders, the user row does NOT.
    await waitFor(() => expect(screen.getByText("Bug triage sweep")).toBeTruthy());
    expect(screen.queryByText("Prompt: my own flaky hunt")).toBeNull();

    // Switch to My schedules: the user row renders, the catalog entry is gone.
    fireEvent.click(screen.getByRole("tab", { name: /My schedules/ }));
    await waitFor(() => expect(screen.getByText("Prompt: my own flaky hunt")).toBeTruthy());
    expect(screen.queryByText("Bug triage sweep")).toBeNull();
  });

  it("a user row exposes a Clone action that calls cloneSchedule", async () => {
    mockApi.listSchedules.mockResolvedValue([sched({ id: "u1", target: "prompt", prompt: "mine", origin: "user" })]);
    mockApi.cloneSchedule.mockResolvedValue(
      sched({ id: "u2", target: "prompt", prompt: "mine", origin: "user" }),
    );
    renderPage();
    await waitFor(() => expect(screen.getByText("Prompt: mine")).toBeTruthy());

    fireEvent.click(screen.getByRole("button", { name: "Clone schedule" }));
    await waitFor(() => expect(mockApi.cloneSchedule).toHaveBeenCalledWith("u1"));
  });
});

// ── PRD #308 M4: the enriched "Last run" cell + "Last fire" detail row ─────────
const NOW = new Date().toISOString();

function fire(over: Partial<LastFire>): LastFire {
  return { fired_at: NOW, matched: 0, capped: false, started: [], skips: [], ...over };
}

// only1 renders a single schedule so the "Last fire" disclosure and its panel are
// unambiguous (one button, one detail panel).
function only1(over: Partial<Schedule>) {
  mockApi.listSchedules.mockResolvedValue([sched({ id: "s1", ...over })]);
}

describe("Schedules — fire outcomes (PRD #308 M4)", () => {
  it("started: a green badge, expandable to the runs it started with their run ids", async () => {
    only1({
      target: "issue",
      issue_iid: 142,
      last_fire: fire({
        matched: 1,
        started: [{ issue_iid: 142, run_id: "3f1a2b7c-dead-beef-0000-000000000000", title: "Fix the thing" }],
      }),
    });
    renderPage();
    // The list cell shows the green outcome badge before expansion.
    await waitFor(() => expect(screen.getByText("1 started")).toBeTruthy());

    fireEvent.click(screen.getByRole("button", { name: "Last fire" }));
    // Expanded: the started run is listed with its issue ref and a run chip (truncated id).
    expect(screen.getByText("Fix the thing")).toBeTruthy();
    const chip = screen.getByText(/run 3f1a/);
    expect(chip.getAttribute("href")).toBe("/runs/3f1a2b7c-dead-beef-0000-000000000000");
  });

  it("backfill (issue #416): a fire that backfilled past a skip shows the started runs AND the flagged skip, and the tally is relabeled 'examined' (may exceed max_issues)", async () => {
    only1({
      target: "sweep",
      max_issues: 3,
      // examined 4 = started 3 (10, 30, 40 — 30/40 backfilled past the skip) + skipped 1 (20).
      // matched is the WIRE field name (unchanged); it now carries the examined count.
      last_fire: fire({
        matched: 4,
        capped: false,
        started: [
          { issue_iid: 10, run_id: "10101010-0000-0000-0000-000000000000", title: "oldest eligible" },
          { issue_iid: 30, run_id: "30303030-0000-0000-0000-000000000000", title: "backfilled one" },
          { issue_iid: 40, run_id: "40404040-0000-0000-0000-000000000000", title: "backfilled two" },
        ],
        skips: [{ issue_iid: 20, title: "no prd here", reason: "not_eligible" }],
      }),
    });
    renderPage();
    // Collapsed cell: three runs started even though the 2nd candidate was skipped.
    await waitFor(() => expect(screen.getByText("3 started")).toBeTruthy());

    fireEvent.click(screen.getByRole("button", { name: "Last fire" }));
    // Both a started run row and the skipped candidate row render together.
    expect(screen.getByText("backfilled two")).toBeTruthy();
    expect(screen.getByText("no prd here")).toBeTruthy();
    expect(screen.getByText("not eligible")).toBeTruthy();
    // The tally label is "examined", not "matched" (and its value 4 exceeds max_issues 3).
    expect(screen.getByText("examined")).toBeTruthy();
    expect(screen.queryByText("matched")).toBeNull();
    // The started-nothing cap hint must NOT show — the fire started runs.
    expect(screen.queryByText("Nothing newer was reached.")).toBeNull();
  });

  it("started-nothing: an amber '0 started · 1 skipped' badge, expandable to the skip + its reason", async () => {
    only1({
      target: "sweep",
      max_issues: 5,
      last_fire: fire({
        matched: 1,
        capped: false,
        skips: [{ issue_iid: 96, title: "Worker restart drops commits", reason: "not_eligible" }],
      }),
    });
    renderPage();
    await waitFor(() => expect(screen.getByText("0 started · 1 skipped")).toBeTruthy());

    fireEvent.click(screen.getByRole("button", { name: "Last fire" }));
    expect(screen.getByText("Worker restart drops commits")).toBeTruthy();
    // The typed reason renders as its human label, never the raw sentinel.
    expect(screen.getByText("not eligible")).toBeTruthy();
    expect(screen.queryByText("not_eligible")).toBeNull();
  });

  it("empty-label: a matched-0 sweep reads as a neutral 'matched 0', not an error", async () => {
    only1({ target: "sweep", last_fire: fire({ matched: 0 }) });
    renderPage();
    await waitFor(() => expect(screen.getByText("matched 0")).toBeTruthy());
  });

  it("never-fired: shows the dash and offers no disclosure", async () => {
    only1({ last_fired_at: null, last_fire: null });
    renderPage();
    await waitFor(() => expect(screen.getByText("— never fired")).toBeTruthy());
    expect(screen.queryByRole("button", { name: "Last fire" })).toBeNull();
  });

  it("parked: an error schedule with a prior last_fire still shows that fire", async () => {
    only1({
      status: "error",
      last_fire: fire({
        matched: 1,
        started: [{ issue_iid: 173, run_id: "aaaa1111-0000-0000-0000-000000000000", title: "prior run" }],
      }),
    });
    renderPage();
    await waitFor(() => expect(screen.getByText("1 started")).toBeTruthy());
  });

  it("parked: an error schedule with no fire at all shows the dash", async () => {
    only1({ status: "error", last_fired_at: null, last_fire: null });
    renderPage();
    await waitFor(() => expect(screen.getByText("— never fired")).toBeTruthy());
  });
});

// ── PRD #411 M3: forge issue links on fire rows ────────────────────────────────
const ISSUE_URL = "https://gitlab.example.com/vtmocanu/uzi/-/issues/26";

describe("Schedules — issue links on fire rows (PRD #411)", () => {
  it("started row with a valid https web_url renders an external forge anchor for #26", async () => {
    only1({
      target: "issue",
      issue_iid: 26,
      last_fire: fire({
        matched: 1,
        started: [
          {
            issue_iid: 26,
            run_id: "26262626-0000-0000-0000-000000000000",
            title: "Clickable issue links",
            web_url: ISSUE_URL,
          },
        ],
      }),
    });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Last fire" }));
    // The forge anchor is distinct from the row's "run <id>" link (matched by name).
    const link = screen.getByRole("link", { name: /Open issue #26/ });
    expect(link.getAttribute("href")).toBe(ISSUE_URL);
    expect(link.getAttribute("target")).toBe("_blank");
    expect(link.getAttribute("rel")).toBe("noreferrer");
  });

  it("paired negative on the SAME #26 wording: a started row with no web_url renders plain #26, no forge anchor", async () => {
    only1({
      target: "issue",
      issue_iid: 26,
      last_fire: fire({
        matched: 1,
        started: [
          {
            issue_iid: 26,
            run_id: "26262626-0000-0000-0000-000000000000",
            title: "Clickable issue links",
            web_url: null,
          },
        ],
      }),
    });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Last fire" }));
    // Scope to the fire row (the schedule's target cell also shows a #26).
    const row = screen.getByText("Clickable issue links").closest<HTMLElement>("div.rounded-lg")!;
    // No forge anchor for the issue...
    expect(within(row).queryByRole("link", { name: /Open issue/ })).toBeNull();
    // ...but the SAME #26 wording renders as plain text in the fire row.
    expect(within(row).getByText("#26")).toBeTruthy();
  });

  it("a fire row with a null issue_iid renders the 'prompt' marker, not an anchor", async () => {
    only1({
      target: "sweep",
      max_issues: 5,
      last_fire: fire({
        matched: 1,
        skips: [{ issue_iid: null, title: "pinned-issue candidate", reason: "not_eligible", web_url: null }],
      }),
    });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Last fire" }));
    // Scope to the skip row (the target legend also shows a "prompt" badge).
    const row = screen.getByText("pinned-issue candidate").closest<HTMLElement>("div.rounded-lg")!;
    expect(within(row).getByText("prompt")).toBeTruthy();
    expect(within(row).queryByRole("link")).toBeNull();
  });
});

describe("Schedules — the cap hint (PRD #308 M4, Goal 2)", () => {
  const hintText = "Nothing newer was reached.";

  it("renders when capped && skipped>0 && started===0", async () => {
    only1({
      target: "sweep",
      max_issues: 1,
      last_fire: fire({
        matched: 1,
        capped: true,
        skips: [{ issue_iid: 96, title: "candidate", reason: "not_eligible" }],
      }),
    });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Last fire" }));
    expect(screen.getByText(hintText)).toBeTruthy();
  });

  it("does NOT render when capped but something started", async () => {
    only1({
      target: "sweep",
      max_issues: 1,
      last_fire: fire({
        matched: 2,
        capped: true,
        started: [{ issue_iid: 90, run_id: "bbbb2222-0000-0000-0000-000000000000", title: "started one" }],
        skips: [{ issue_iid: 96, title: "candidate", reason: "not_eligible" }],
      }),
    });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Last fire" }));
    expect(screen.queryByText(hintText)).toBeNull();
  });

  it("does NOT render when skips exist but the fire was not capped", async () => {
    only1({
      target: "sweep",
      max_issues: 10,
      last_fire: fire({
        matched: 1,
        capped: false,
        skips: [{ issue_iid: 96, title: "candidate", reason: "not_eligible" }],
      }),
    });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Last fire" }));
    expect(screen.queryByText(hintText)).toBeNull();
  });
});

// ── PRD #636 M3: grouped/expandable sibling view + "add another repo" ──────────
describe("Schedules — sibling groups (PRD #636 M3)", () => {
  // Two siblings sharing a non-null group id on distinct repos.
  const twoSiblings = () => [
    sched({ id: "g1", target: "prompt", prompt: "grouped job", repo_id: "repo-uzi", repo_path: "vtmocanu/uzi", sibling_group_id: "grp-1" }),
    sched({ id: "g2", target: "prompt", prompt: "grouped job", repo_id: "repo-atlas", repo_path: "vtmocanu/atlas", sibling_group_id: "grp-1" }),
  ];
  // The expand toggle's accessible name is derived from the head member's target title.
  const groupToggle = /Show repos for Prompt: grouped job/;

  it("two rows sharing a group id render as ONE expandable summary over 2 sub-rows", async () => {
    mockApi.listSchedules.mockResolvedValue(twoSiblings());
    renderPage();

    // One group summary with an expand toggle — not two standalone rows.
    await waitFor(() => expect(screen.getByRole("button", { name: groupToggle })).toBeTruthy());
    // Collapsed: the per-repo sub-row pause toggles are not rendered yet (non-vacuous
    // against the expanded state asserted below).
    expect(screen.queryByRole("switch", { name: "Pause on vtmocanu/uzi" })).toBeNull();
    expect(screen.queryByRole("switch", { name: "Pause on vtmocanu/atlas" })).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: groupToggle }));

    // Expanded: exactly one sub-row per sibling repo.
    await waitFor(() => expect(screen.getByRole("switch", { name: "Pause on vtmocanu/uzi" })).toBeTruthy());
    expect(screen.getByRole("switch", { name: "Pause on vtmocanu/atlas" })).toBeTruthy();
  });

  it("the group summary carries name + repo-count only — no type pill (PRD #636 M3)", async () => {
    mockApi.listSchedules.mockResolvedValue(twoSiblings());
    renderPage();

    const toggle = await screen.findByRole("button", { name: groupToggle });
    const summaryRow = toggle.closest("tr")!;
    // Positive half: the summary DOES carry the schedule name and the repo-count/expand
    // toggle (so the negative below is discriminating, not vacuous).
    expect(within(summaryRow).getByText("Prompt: grouped job")).toBeTruthy();
    expect(within(summaryRow).getByText(/2 repos/)).toBeTruthy();
    // Discriminating negative: NO sweep/prompt type pill in the summary. The type is
    // already in the name ("Prompt:"/"Sweep ·") and siblings may have diverged, so the
    // per-repo target lives in the sub-rows, not the collapsed summary.
    expect(within(summaryRow).queryByText("prompt")).toBeNull();
    expect(within(summaryRow).queryByText("sweep")).toBeNull();
  });

  it("each expanded sub-row surfaces its own per-repo target badge (PRD #636 M3)", async () => {
    mockApi.listSchedules.mockResolvedValue(twoSiblings());
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: groupToggle }));
    await waitFor(() => expect(screen.getByRole("switch", { name: "Pause on vtmocanu/uzi" })).toBeTruthy());

    // The summary dropped the type pill, so each sub-row must carry its own target — a
    // diverged sibling's target is surfaced per-repo, never silently dropped.
    const uziRow = screen.getByText("vtmocanu/uzi").closest<HTMLElement>("div.rounded-lg")!;
    expect(within(uziRow).getByText("prompt")).toBeTruthy();
    const atlasRow = screen.getByText("vtmocanu/atlas").closest<HTMLElement>("div.rounded-lg")!;
    expect(within(atlasRow).getByText("prompt")).toBeTruthy();
  });

  it("two NULL-group rows render as TWO standalone rows with NO summary/expand control", async () => {
    mockApi.listSchedules.mockResolvedValue([
      sched({ id: "n1", target: "prompt", prompt: "first solo", sibling_group_id: null }),
      sched({ id: "n2", target: "prompt", prompt: "second solo", sibling_group_id: null }),
    ]);
    renderPage();

    // Both rows render as their own standalone ScheduleRow…
    await waitFor(() => expect(screen.getByText("Prompt: first solo")).toBeTruthy());
    expect(screen.getByText("Prompt: second solo")).toBeTruthy();
    // …and CRUCIALLY there is NO group summary/expand control. The naive
    // groupBy(sibling_group_id) bug would collapse both nulls into one bogus group with a
    // "Show repos for" toggle, so this absence is the discriminating assertion (a bare
    // "2 rows appear" check would pass on that bug).
    expect(screen.queryByRole("button", { name: /Show repos for/ })).toBeNull();
    // Positive control: each standalone row exposes its own row-level enable toggle.
    expect(screen.getAllByRole("switch", { name: /able schedule/ })).toHaveLength(2);
  });

  it("a non-null group with exactly ONE live member renders standalone (no summary)", async () => {
    mockApi.listSchedules.mockResolvedValue([
      sched({ id: "solo", target: "prompt", prompt: "lonely sibling", sibling_group_id: "grp-lonely" }),
    ]);
    renderPage();

    await waitFor(() => expect(screen.getByText("Prompt: lonely sibling")).toBeTruthy());
    // A one-member non-null group is a standalone row, never a one-child group.
    expect(screen.queryByRole("button", { name: /Show repos for/ })).toBeNull();
    // It IS a standalone ScheduleRow (row-level enable toggle present; the fixture row is
    // enabled, so the toggle reads "Disable schedule").
    expect(screen.getByRole("switch", { name: "Disable schedule" })).toBeTruthy();
  });

  it("'add another repo' calls addScheduleRepo with the source id + repo, and the new sibling appears after refresh", async () => {
    const repoList = {
      repos: [
        { id: "repo-uzi", path_with_namespace: "vtmocanu/uzi" },
        { id: "repo-atlas", path_with_namespace: "vtmocanu/atlas" },
        { id: "repo-new", path_with_namespace: "vtmocanu/newrepo" },
      ],
    } as Awaited<ReturnType<typeof api.listRepos>>;
    mockApi.listRepos.mockResolvedValue(repoList);
    mockApi.listSchedules
      .mockResolvedValueOnce(twoSiblings())
      .mockResolvedValueOnce([
        ...twoSiblings(),
        sched({ id: "g3", target: "prompt", prompt: "grouped job", repo_id: "repo-new", repo_path: "vtmocanu/newrepo", sibling_group_id: "grp-1" }),
      ]);
    mockApi.addScheduleRepo.mockResolvedValue(
      sched({ id: "g3", target: "prompt", prompt: "grouped job", repo_id: "repo-new", repo_path: "vtmocanu/newrepo", sibling_group_id: "grp-1" }),
    );
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: groupToggle }));
    // The picker offers only repos not already in the group (repo-new).
    const picker = await screen.findByRole("combobox", { name: /Add Prompt: grouped job on another repo/ });
    expect(within(picker).getByRole("option", { name: "vtmocanu/newrepo" })).toBeTruthy();
    expect(within(picker).queryByRole("option", { name: "vtmocanu/uzi" })).toBeNull();

    fireEvent.change(picker, { target: { value: "repo-new" } });
    fireEvent.click(screen.getByRole("button", { name: "Add" }));

    // Called with the head sibling's id and the chosen repo (not a fixed uuid).
    await waitFor(() => expect(mockApi.addScheduleRepo).toHaveBeenCalledWith("g1", "repo-new"));
    // The refreshed group auto-expands and shows the new sibling's sub-row.
    await waitFor(() => expect(screen.getByRole("switch", { name: "Pause on vtmocanu/newrepo" })).toBeTruthy());
  });

  it("a duplicate add (409) is friendly and non-fatal, not an error", async () => {
    mockApi.listRepos.mockResolvedValue({
      repos: [
        { id: "repo-uzi", path_with_namespace: "vtmocanu/uzi" },
        { id: "repo-atlas", path_with_namespace: "vtmocanu/atlas" },
        { id: "repo-new", path_with_namespace: "vtmocanu/newrepo" },
      ],
    } as Awaited<ReturnType<typeof api.listRepos>>);
    mockApi.listSchedules.mockResolvedValue(twoSiblings());
    const { ApiError } = await vi.importActual<typeof import("../lib/api")>("../lib/api");
    mockApi.addScheduleRepo.mockRejectedValue(new ApiError(409, "already on that repo"));
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: groupToggle }));
    const picker = await screen.findByRole("combobox", { name: /Add Prompt: grouped job on another repo/ });
    fireEvent.change(picker, { target: { value: "repo-new" } });
    fireEvent.click(screen.getByRole("button", { name: "Add" }));

    // Friendly notice, not a red error banner (role=alert).
    await waitFor(() => expect(screen.getByText(/already on that repo/i)).toBeTruthy());
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("per-sub-row pause and remove target ONLY that sibling's id", async () => {
    mockApi.listSchedules.mockResolvedValue(twoSiblings());
    mockApi.updateSchedule.mockImplementation(async (id: string, input) => sched({ id, enabled: input.enabled ?? true }));
    mockApi.deleteSchedule.mockResolvedValue(null);
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: groupToggle }));
    await waitFor(() => expect(screen.getByRole("switch", { name: "Pause on vtmocanu/atlas" })).toBeTruthy());

    // Pause the atlas sibling — only g2 is PATCHed.
    fireEvent.click(screen.getByRole("switch", { name: "Pause on vtmocanu/atlas" }));
    await waitFor(() => expect(mockApi.updateSchedule).toHaveBeenCalledWith("g2", { enabled: false }));
    expect(mockApi.updateSchedule).not.toHaveBeenCalledWith("g1", { enabled: false });

    // Remove the uzi sibling — only g1 is deleted.
    fireEvent.click(screen.getByRole("button", { name: "Remove on vtmocanu/uzi" }));
    await waitFor(() => expect(mockApi.deleteSchedule).toHaveBeenCalledWith("g1"));
    expect(mockApi.deleteSchedule).not.toHaveBeenCalledWith("g2");
  });
});

// ── issue #690: per-repo last-run parity on grouped sub-rows ───────────────────
describe("Schedules — last-run parity on grouped sub-rows (issue #690)", () => {
  it("a grouped sub-row with a last_fire shows the outcome badge and expands to the fire detail", async () => {
    // Only the uzi sibling carries a fire; the atlas sibling never fired. That keeps the
    // "Last fire" disclosure unambiguous (one sub-row has it) while proving both the
    // outcome-badge and never-fired branches render per-repo.
    mockApi.listSchedules.mockResolvedValue([
      sched({
        id: "g1",
        target: "prompt",
        prompt: "grouped job",
        repo_id: "repo-uzi",
        repo_path: "vtmocanu/uzi",
        sibling_group_id: "grp-1",
        last_fire: fire({
          matched: 1,
          started: [{ issue_iid: 8, run_id: "88888888-0000-0000-0000-000000000000", title: "grouped started" }],
        }),
      }),
      sched({
        id: "g2",
        target: "prompt",
        prompt: "grouped job",
        repo_id: "repo-atlas",
        repo_path: "vtmocanu/atlas",
        sibling_group_id: "grp-1",
        last_fire: null,
        last_fired_at: null,
      }),
    ]);
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: /Show repos for Prompt: grouped job/ }));
    await waitFor(() => expect(screen.getByRole("switch", { name: "Pause on vtmocanu/uzi" })).toBeTruthy());

    // The uzi sub-row shows the enriched green outcome badge; the atlas sub-row never fired.
    // Scope each outcome to its own sub-row (anchored on the unique per-repo pause switch)
    // so a Uzi/Atlas swap fails the test — a document-level getByText would pass either way,
    // and "— never fired" also appears in the group summary cell (issue #690 CR).
    const uziRow = screen
      .getByRole("switch", { name: "Pause on vtmocanu/uzi" })
      .closest<HTMLElement>("div.rounded-lg")!;
    const atlasRow = screen
      .getByRole("switch", { name: "Pause on vtmocanu/atlas" })
      .closest<HTMLElement>("div.rounded-lg")!;
    expect(within(uziRow).getByText("1 started")).toBeTruthy();
    expect(within(uziRow).queryByText("— never fired")).toBeNull();
    expect(within(atlasRow).getByText("— never fired")).toBeTruthy();
    expect(within(atlasRow).queryByText("1 started")).toBeNull();

    // Only one sub-row carries a fire, so the disclosure is unambiguous. Expanding reveals
    // the started run (LastFireDetail) below that sub-row's flex row.
    expect(screen.queryByText("grouped started")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Last fire" }));
    expect(screen.getByText("grouped started")).toBeTruthy();
  });
});

// ── issue #638: issue schedules can't span repos + per-repo target badges ───────
describe("Schedules — issue-target add-repo gating + sub-row badges (issue #638)", () => {
  const reason = "Issue schedules can't span repos - issue numbers are repo-relative";

  it("P1c: an issue-target standalone row disables the 'Add another repo' control (with the reason as its tooltip)", async () => {
    mockApi.listRepos.mockResolvedValue({
      repos: [
        { id: "repo-uzi", path_with_namespace: "vtmocanu/uzi" },
        { id: "repo-atlas", path_with_namespace: "vtmocanu/atlas" },
      ],
    } as Awaited<ReturnType<typeof api.listRepos>>);
    mockApi.listSchedules.mockResolvedValue([
      sched({ id: "iss1", target: "issue", issue_iid: 42, origin: "user", sibling_group_id: null }),
    ]);
    renderPage();
    await waitFor(() => expect(screen.getByText("#42")).toBeTruthy());

    // The affordance is still present (discoverable), but disabled. Its accessible
    // name carries the blocked reason (the title tooltip is unreachable on a disabled
    // button for keyboard/SR/touch), and its title tooltip repeats the fuller reason.
    const addBtn = screen.getByRole("button", { name: "Add another repo (unavailable for issue schedules)" });
    expect((addBtn as HTMLButtonElement).disabled).toBe(true);
    expect(addBtn.getAttribute("title")).toBe(reason);
  });

  it("P1c control-negative: a non-issue (sweep) standalone row keeps 'Add another repo' enabled", async () => {
    mockApi.listRepos.mockResolvedValue({
      repos: [
        { id: "repo-uzi", path_with_namespace: "vtmocanu/uzi" },
        { id: "repo-atlas", path_with_namespace: "vtmocanu/atlas" },
      ],
    } as Awaited<ReturnType<typeof api.listRepos>>);
    mockApi.listSchedules.mockResolvedValue([
      sched({ id: "sw1", target: "sweep", origin: "user", sibling_group_id: null }),
    ]);
    renderPage();
    await waitFor(() => expect(screen.getByText("Sweep eligible PRD issues")).toBeTruthy());

    // Discriminates the P1c gate: a sweep row's add-repo control is NOT disabled, so the
    // disabled assertion above is driven by target === "issue", not an always-off control.
    const addBtn = screen.getByRole("button", { name: "Add another repo" });
    expect((addBtn as HTMLButtonElement).disabled).toBe(false);
    expect(addBtn.getAttribute("title")).toBe("Add another repo");
  });

  it("P2b: grouped issue siblings surface a per-repo 'issue' target badge", async () => {
    mockApi.listSchedules.mockResolvedValue([
      sched({ id: "gi1", target: "issue", issue_iid: 7, repo_id: "repo-uzi", repo_path: "vtmocanu/uzi", origin: "user", sibling_group_id: "grp-iss" }),
      sched({ id: "gi2", target: "issue", issue_iid: 7, repo_id: "repo-atlas", repo_path: "vtmocanu/atlas", origin: "user", sibling_group_id: "grp-iss" }),
    ]);
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: /Show repos for #7/ }));
    await waitFor(() => expect(screen.getByRole("switch", { name: "Pause on vtmocanu/uzi" })).toBeTruthy());

    // The group summary drops the type pill, so each sub-row must carry its own target
    // badge — previously absent for issue targets (only sweep/prompt were rendered).
    const uziRow = screen.getByText("vtmocanu/uzi").closest<HTMLElement>("div.rounded-lg")!;
    expect(within(uziRow).getByText("issue")).toBeTruthy();
    const atlasRow = screen.getByText("vtmocanu/atlas").closest<HTMLElement>("div.rounded-lg")!;
    expect(within(atlasRow).getByText("issue")).toBeTruthy();
  });

  it("P2b: grouped self_improve siblings surface a per-repo 'self-improve' target badge", async () => {
    mockApi.listSchedules.mockResolvedValue([
      sched({ id: "si1", target: "self_improve", repo_id: "repo-uzi", repo_path: "vtmocanu/uzi", origin: "user", sibling_group_id: "grp-si" }),
      sched({ id: "si2", target: "self_improve", repo_id: "repo-atlas", repo_path: "vtmocanu/atlas", origin: "user", sibling_group_id: "grp-si" }),
    ]);
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: /Show repos for Self-improvement/ }));
    await waitFor(() => expect(screen.getByRole("switch", { name: "Pause on vtmocanu/uzi" })).toBeTruthy());

    const uziRow = screen.getByText("vtmocanu/uzi").closest<HTMLElement>("div.rounded-lg")!;
    expect(within(uziRow).getByText("self-improve")).toBeTruthy();
    const atlasRow = screen.getByText("vtmocanu/atlas").closest<HTMLElement>("div.rounded-lg")!;
    expect(within(atlasRow).getByText("self-improve")).toBeTruthy();
  });
});
