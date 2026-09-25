// @vitest-environment jsdom
//
// Schedules list page (PRD #241 M5, mock §1): rows render per target/timing, and the
// per-row enable toggle PATCHes { enabled } and adopts the server's returned row.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, useLocation, useNavigate } from "react-router-dom";
import { Schedules } from "./Schedules";
import { api, type LastFire, type Schedule } from "../lib/api";
import { useAuth } from "../auth/AuthContext";
import { formatStamp } from "../components/LastRun";
import { relativeFromNow } from "../components/ScheduleModal";
import { resolvePreset } from "../lib/schedulePausePresets";
import { schedulesApi } from "../mocks/mockApi/schedules";

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
      // PRD #1093: the page fetches the user-level pause state alongside the list, and the
      // picker/banner drive PUT/DELETE.
      getSchedulePause: vi.fn(),
      putSchedulePause: vi.fn(),
      deleteSchedulePause: vi.fn(),
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

// One shipped sweep entry. Its catalog cadence (UTC, 02:00 daily, inherit model) is
// deliberately DIFFERENT from what the default-row fixtures below carry, so a row that
// rendered catalog values instead of its own would fail.
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
      output_mode: "",
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
    baked_guidance: null,
    model: null,
    output_mode: null,
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
  // Default: not paused. Individual tests override to drive the paused / picker states.
  mockApi.getSchedulePause.mockResolvedValue({ paused: false, until: null });
  vi.mocked(useAuth).mockReturnValue({ prdLabel: "PRD", uziLabel: "uzi" } as unknown as ReturnType<typeof useAuth>);
});
afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

// With no ?tab the page lands on the Schedules tab whenever the owner has a schedule
// (PRD #1645 D1), which is where every row under test lives.
function renderPage() {
  return render(
    <MemoryRouter>
      <Schedules />
    </MemoryRouter>,
  );
}

describe("Schedules list", () => {
  it("renders a row per schedule with its target badge and cron", async () => {
    renderPage();
    await waitFor(() => expect(screen.getByText("Sweep eligible issues")).toBeTruthy());
    expect(screen.getByText("Prompt: hunt flaky tests")).toBeTruthy();
    // The cron string is shown verbatim (canonical source of truth).
    expect(screen.getByText("0 2 * * 1-5")).toBeTruthy();
    // A paused (disabled) schedule shows the paused pill in Next run.
    expect(screen.getByText("paused")).toBeTruthy();
  });

  it("renders sibling rows whose next_fires is null without crashing (issue #1003)", async () => {
    // A once/invalid-cron schedule ships `next_fires: null` on the wire. The row's next
    // fire (nextFireOf) reads `s.next_fires?.[0]` and must tolerate it, for siblings too.
    mockApi.listSchedules.mockResolvedValue([
      sched({ id: "g1", sibling_group_id: "grp-1003", repo_id: "repo-uzi", repo_path: "vtmocanu/uzi", target: "prompt", prompt: "grouped null next_fires", next_fires: null }),
      sched({ id: "g2", sibling_group_id: "grp-1003", repo_id: "repo-atlas", repo_path: "vtmocanu/atlas", target: "prompt", prompt: "grouped null next_fires", next_fires: null }),
    ]);
    renderPage();
    await waitFor(() => expect(screen.getAllByText("Prompt: grouped null next_fires")).toHaveLength(2));
    // Both rows rendered (their repo labels appear), so the null next_fires read did not throw.
    // Scoped to the table: the D5 Repo select lists the same paths as options.
    const table = screen.getByRole("table");
    expect(within(table).getByText(/^vtmocanu\/uzi/)).toBeTruthy();
    expect(within(table).getByText(/^vtmocanu\/atlas/)).toBeTruthy();
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
    await waitFor(() => expect(screen.getByText("Sweep eligible issues")).toBeTruthy());
    expect(screen.getByText("auto-approve")).toBeTruthy();
  });

  it("the enable toggle PATCHes { enabled } and adopts the returned row", async () => {
    mockApi.updateSchedule.mockImplementation(async (id: string, input) =>
      sched({ id, enabled: input.enabled ?? true }),
    );
    renderPage();
    await waitFor(() => expect(screen.getByText("Sweep eligible issues")).toBeTruthy());

    // s1 is enabled; pausing it sends { enabled: false }.
    const toggle = screen.getByRole("switch", { name: "Pause Sweep eligible issues on vtmocanu/uzi" });
    fireEvent.click(toggle);
    await waitFor(() => expect(mockApi.updateSchedule).toHaveBeenCalledWith("s1", { enabled: false }));
  });
});

// ── PRD #1645 D2: one table for both origins (replaces the PRD #589 two-tab split) ──
describe("Schedules — one list for both origins (PRD #1645 D2)", () => {
  it("a default row and a user row render in the same table, with no expand control", async () => {
    mockApi.listScheduleCatalog.mockResolvedValue(CATALOG);
    mockApi.listSchedules.mockResolvedValue([
      sched({ id: "u1", target: "prompt", prompt: "my own flaky hunt", origin: "user" }),
      sched({ id: "d1", origin: "default", catalog_slug: "bug-triage", labels: ["bug"] }),
    ]);
    renderPage();

    const user = await screen.findByText("Prompt: my own flaky hunt");
    const def = screen.getByText("Bug triage sweep");
    expect(user.closest("table")).toBe(def.closest("table"));
    expect(user.closest("table")).not.toBeNull();
    // Flat rows: nothing to expand before acting.
    expect(screen.queryByRole("button", { name: /Show repos for/ })).toBeNull();
  });

  it("a user row's More actions menu offers Clone, which calls cloneSchedule", async () => {
    mockApi.listSchedules.mockResolvedValue([sched({ id: "u1", target: "prompt", prompt: "mine", origin: "user" })]);
    mockApi.cloneSchedule.mockResolvedValue(
      sched({ id: "u2", target: "prompt", prompt: "mine", origin: "user" }),
    );
    renderPage();
    await waitFor(() => expect(screen.getByText("Prompt: mine")).toBeTruthy());

    fireEvent.click(screen.getByRole("button", { name: "More actions for Prompt: mine on vtmocanu/uzi" }));
    fireEvent.click(screen.getByRole("menuitem", { name: "Clone to an editable copy" }));
    await waitFor(() => expect(mockApi.cloneSchedule).toHaveBeenCalledWith("u1"));
  });
});

// ── issue #660: enabling a default sends the browser-detected timezone ─────────
describe("Schedules — enable-default sends the browser timezone (issue #660)", () => {
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
        output_mode: "",
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

  it("the fan-out passes the detected zone as enableCatalogSchedule's third arg", async () => {
    // Pin the detected browser zone so the expected third arg is deterministic (the util
    // reads Intl.DateTimeFormat().resolvedOptions().timeZone). Restore in finally so the
    // spy never leaks into a sibling test.
    const dtfSpy = vi.spyOn(Intl, "DateTimeFormat").mockImplementation(
      () => ({ resolvedOptions: () => ({ timeZone: "Europe/Bucharest" }) }) as unknown as Intl.DateTimeFormat,
    );
    try {
      mockApi.listScheduleCatalog.mockResolvedValue(CATALOG);
      mockApi.listSchedules.mockResolvedValue([]);
      mockApi.listRepos.mockResolvedValue({
        repos: [{ id: "repo-uzi", path_with_namespace: "vtmocanu/uzi" }],
      } as Awaited<ReturnType<typeof api.listRepos>>);
      mockApi.enableCatalogSchedule.mockResolvedValue(
        sched({ id: "new1", origin: "default", catalog_slug: "bug-triage", timezone: "Europe/Bucharest" }),
      );

      // With no schedules the page lands on the Job catalog tab, where the enable fan-out lives.
      render(
        <MemoryRouter>
          <Schedules />
        </MemoryRouter>,
      );
      await waitFor(() => expect(screen.getByText("Bug triage sweep")).toBeTruthy());

      // Pick the repo in the card's enable dialog; Enable is offered once its label check settles.
      fireEvent.click(screen.getByRole("button", { name: "Enable Bug triage sweep on…" }));
      fireEvent.click(screen.getByRole("checkbox", { name: /vtmocanu\/uzi/ }));
      const enable = await screen.findByRole("button", { name: "Enable 1" });
      await waitFor(() => expect((enable as HTMLButtonElement).disabled).toBe(false));
      fireEvent.click(enable);

      // The single real call site fans out one call per repo, now carrying the detected zone.
      await waitFor(() =>
        expect(mockApi.enableCatalogSchedule).toHaveBeenCalledWith("repo-uzi", "bug-triage", "Europe/Bucharest"),
      );
    } finally {
      dtfSpy.mockRestore();
    }
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

// ── PRD #636 siblings, now flat rows (PRD #1645 D2) + "add another repo" ──────────
const REPOS3 = {
  repos: [
    { id: "repo-uzi", path_with_namespace: "vtmocanu/uzi" },
    { id: "repo-atlas", path_with_namespace: "vtmocanu/atlas" },
    { id: "repo-new", path_with_namespace: "vtmocanu/newrepo" },
  ],
} as Awaited<ReturnType<typeof api.listRepos>>;

describe("Schedules — siblings render as independent rows (PRD #1645 D2)", () => {
  // Two siblings sharing a non-null group id on distinct repos.
  const twoSiblings = () => [
    sched({ id: "g1", target: "prompt", prompt: "grouped job", repo_id: "repo-uzi", repo_path: "vtmocanu/uzi", sibling_group_id: "grp-1" }),
    sched({ id: "g2", target: "prompt", prompt: "grouped job", repo_id: "repo-atlas", repo_path: "vtmocanu/atlas", sibling_group_id: "grp-1" }),
  ];

  it("two rows sharing a group id render as TWO rows, each with its own toggle, and no group summary", async () => {
    mockApi.listSchedules.mockResolvedValue(twoSiblings());
    renderPage();

    await waitFor(() => expect(screen.getByRole("switch", { name: "Pause Prompt: grouped job on vtmocanu/uzi" })).toBeTruthy());
    expect(screen.getByRole("switch", { name: "Pause Prompt: grouped job on vtmocanu/atlas" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: /Show repos for/ })).toBeNull();
    // Each row carries its own target pill.
    const uziRow = screen.getByRole("switch", { name: /on vtmocanu\/uzi$/ }).closest("tr")!;
    expect(within(uziRow).getByText("prompt")).toBeTruthy();
  });

  it("'Add to another repo' calls addScheduleRepo, then reveals and focuses the new row (D12)", async () => {
    mockApi.listRepos.mockResolvedValue(REPOS3);
    const added = sched({ id: "g3", target: "prompt", prompt: "grouped job", repo_id: "repo-new", repo_path: "vtmocanu/newrepo", sibling_group_id: "grp-1" });
    mockApi.listSchedules.mockResolvedValueOnce(twoSiblings()).mockResolvedValueOnce([...twoSiblings(), added]);
    mockApi.addScheduleRepo.mockResolvedValue(added);
    const scrolled: Element[] = [];
    const proto = Element.prototype as unknown as { scrollIntoView?: () => void };
    const had = proto.scrollIntoView;
    proto.scrollIntoView = function (this: Element) {
      scrolled.push(this);
    };
    try {
      renderPage();
      fireEvent.click(await screen.findByRole("button", { name: "More actions for Prompt: grouped job on vtmocanu/uzi" }));
      fireEvent.click(screen.getByRole("menuitem", { name: "Add to another repo" }));

      // The picker offers only repos other than this row's own, and takes focus.
      const picker = await screen.findByRole("combobox", { name: /Add Prompt: grouped job on another repo/ });
      await waitFor(() => expect(document.activeElement).toBe(picker));
      expect(within(picker).getByRole("option", { name: "vtmocanu/newrepo" })).toBeTruthy();
      expect(within(picker).queryByRole("option", { name: "vtmocanu/uzi" })).toBeNull();

      fireEvent.change(picker, { target: { value: "repo-new" } });
      fireEvent.click(screen.getByRole("button", { name: "Add" }));

      await waitFor(() => expect(mockApi.addScheduleRepo).toHaveBeenCalledWith("g1", "repo-new"));
      // The new row is rendered, scrolled into view and focused (its name cell).
      await waitFor(() => expect(screen.getByRole("switch", { name: "Pause Prompt: grouped job on vtmocanu/newrepo" })).toBeTruthy());
      const nameCell = document.getElementById("schedule-name-g3");
      expect(nameCell).not.toBeNull();
      await waitFor(() => expect(document.activeElement).toBe(nameCell));
      expect(scrolled).toContain(nameCell);
    } finally {
      proto.scrollIntoView = had;
    }
  });

  it("a duplicate add (409) is friendly and non-fatal, not an error", async () => {
    mockApi.listRepos.mockResolvedValue(REPOS3);
    mockApi.listSchedules.mockResolvedValue(twoSiblings());
    const { ApiError } = await vi.importActual<typeof import("../lib/api")>("../lib/api");
    mockApi.addScheduleRepo.mockRejectedValue(new ApiError(409, "already on that repo"));
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "More actions for Prompt: grouped job on vtmocanu/uzi" }));
    fireEvent.click(screen.getByRole("menuitem", { name: "Add to another repo" }));
    const picker = await screen.findByRole("combobox", { name: /Add Prompt: grouped job on another repo/ });
    fireEvent.change(picker, { target: { value: "repo-new" } });
    fireEvent.click(screen.getByRole("button", { name: "Add" }));

    // Friendly notice, not a red error banner (role=alert).
    await waitFor(() => expect(screen.getByText(/already on that repo/i)).toBeTruthy());
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("per-row pause and Remove target ONLY that sibling's id", async () => {
    mockApi.listSchedules.mockResolvedValue(twoSiblings());
    mockApi.updateSchedule.mockImplementation(async (id: string, input) => sched({ id, enabled: input.enabled ?? true }));
    mockApi.deleteSchedule.mockResolvedValue(null);
    renderPage();

    // Pause the atlas sibling — only g2 is PATCHed.
    fireEvent.click(await screen.findByRole("switch", { name: "Pause Prompt: grouped job on vtmocanu/atlas" }));
    await waitFor(() => expect(mockApi.updateSchedule).toHaveBeenCalledWith("g2", { enabled: false }));
    expect(mockApi.updateSchedule).not.toHaveBeenCalledWith("g1", { enabled: false });

    // Remove the uzi sibling — only g1 is deleted (no confirmation, D4).
    fireEvent.click(screen.getByRole("button", { name: "More actions for Prompt: grouped job on vtmocanu/uzi" }));
    fireEvent.click(screen.getByRole("menuitem", { name: "Remove" }));
    await waitFor(() => expect(mockApi.deleteSchedule).toHaveBeenCalledWith("g1"));
    expect(mockApi.deleteSchedule).not.toHaveBeenCalledWith("g2");
  });
});

// ── issue #690: per-repo last-run parity, now on each flat row ─────────────────
describe("Schedules — per-repo last run on sibling rows (issue #690)", () => {
  it("each sibling row shows its own outcome, and the fired one expands to its detail", async () => {
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

    const uziRow = (await screen.findByRole("switch", { name: /on vtmocanu\/uzi$/ })).closest("tr")!;
    const atlasRow = screen.getByRole("switch", { name: /on vtmocanu\/atlas$/ }).closest("tr")!;
    expect(within(uziRow).getByText("1 started")).toBeTruthy();
    expect(within(uziRow).queryByText("— never fired")).toBeNull();
    expect(within(atlasRow).getByText("— never fired")).toBeTruthy();
    expect(within(atlasRow).queryByText("1 started")).toBeNull();

    expect(screen.queryByText("grouped started")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Last fire" }));
    expect(screen.getByText("grouped started")).toBeTruthy();
  });
});

// ── issue #638: issue schedules can't span repos ───────────────────────────────
describe("Schedules — issue-target add-repo gating (issue #638)", () => {
  const reason = "Issue schedules can't span repos - issue numbers are repo-relative";

  it("P1c: an issue-target row's 'Add to another repo' item is disabled, with the reason as its description", async () => {
    mockApi.listRepos.mockResolvedValue(REPOS3);
    mockApi.listSchedules.mockResolvedValue([
      sched({ id: "iss1", target: "issue", issue_iid: 42, origin: "user", sibling_group_id: null }),
    ]);
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "More actions for #42 on vtmocanu/uzi" }));

    const item = screen.getByRole("menuitem", { name: "Add to another repo" });
    expect(item.getAttribute("aria-disabled")).toBe("true");
    expect(document.getElementById(item.getAttribute("aria-describedby")!)?.textContent).toBe(reason);
    // Activating it does nothing: no picker opens.
    fireEvent.click(item);
    expect(screen.queryByRole("combobox", { name: /on another repo/ })).toBeNull();
  });

  it("P1c control-negative: a sweep row's 'Add to another repo' item is enabled", async () => {
    mockApi.listRepos.mockResolvedValue(REPOS3);
    mockApi.listSchedules.mockResolvedValue([
      sched({ id: "sw1", target: "sweep", origin: "user", sibling_group_id: null }),
    ]);
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "More actions for Sweep eligible issues on vtmocanu/uzi" }));

    const item = screen.getByRole("menuitem", { name: "Add to another repo" });
    expect(item.getAttribute("aria-disabled")).toBeNull();
    expect(item.getAttribute("aria-describedby")).toBeNull();
  });

  it("self_improve sibling rows each carry the 'self-improve' target pill", async () => {
    mockApi.listSchedules.mockResolvedValue([
      sched({ id: "si1", target: "self_improve", repo_id: "repo-uzi", repo_path: "vtmocanu/uzi", origin: "user", sibling_group_id: "grp-si" }),
      sched({ id: "si2", target: "self_improve", repo_id: "repo-atlas", repo_path: "vtmocanu/atlas", origin: "user", sibling_group_id: "grp-si" }),
    ]);
    renderPage();

    const uziRow = (await screen.findByRole("switch", { name: /on vtmocanu\/uzi$/ })).closest("tr")!;
    const atlasRow = screen.getByRole("switch", { name: /on vtmocanu\/atlas$/ }).closest("tr")!;
    expect(within(uziRow).getByText("self-improve")).toBeTruthy();
    expect(within(atlasRow).getByText("self-improve")).toBeTruthy();
  });
});

// ── PRD #1645 M1: the unified list, tabs, row anatomy, reveal ─────────────────
function LocationProbe() {
  const loc = useLocation();
  return <output data-testid="loc">{loc.search}</output>;
}

function renderAt(url: string) {
  return render(
    <MemoryRouter initialEntries={[url]}>
      <Schedules />
      <LocationProbe />
    </MemoryRouter>,
  );
}

// A default row whose own cadence, zone and model all differ from CATALOG's entry.
const customizedDefault = (over: Partial<Schedule> = {}) =>
  sched({
    id: "d1",
    origin: "default",
    catalog_slug: "bug-triage",
    target: "sweep",
    labels: ["bug"],
    max_issues: 3,
    cron_expr: "30 7 * * 1-5",
    timezone: "Europe/Bucharest",
    model: "claude-sonnet-4-5",
    customized: true,
    ...over,
  });

const tabNamed = (re: RegExp) => screen.getByRole("tab", { name: re });

describe("Schedules — tabs, landing and ?tab deep link (PRD #1645 D1)", () => {
  it("lands on Schedules when the owner has a schedule", async () => {
    renderAt("/schedules");
    await waitFor(() => expect(tabNamed(/^Schedules · 2/).getAttribute("aria-selected")).toBe("true"));
    expect(tabNamed(/^Job catalog/).getAttribute("aria-selected")).toBe("false");
  });

  it("lands on the Job catalog when the owner has no schedules", async () => {
    mockApi.listSchedules.mockResolvedValue([]);
    mockApi.listScheduleCatalog.mockResolvedValue(CATALOG);
    renderAt("/schedules");
    await waitFor(() => expect(tabNamed(/^Job catalog · 1/).getAttribute("aria-selected")).toBe("true"));
    expect(screen.getByText("Bug triage sweep")).toBeTruthy();
  });

  it("?tab=catalog selects the catalog even when schedules exist; an invalid ?tab falls back to the landing rule", async () => {
    mockApi.listScheduleCatalog.mockResolvedValue(CATALOG);
    renderAt("/schedules?tab=catalog");
    await waitFor(() => expect(screen.getByText("Bug triage sweep")).toBeTruthy());
    expect(tabNamed(/^Job catalog/).getAttribute("aria-selected")).toBe("true");
    cleanup();

    renderAt("/schedules?tab=bogus");
    await waitFor(() => expect(screen.getByText("Sweep eligible issues")).toBeTruthy());
    expect(tabNamed(/^Schedules/).getAttribute("aria-selected")).toBe("true");
  });

  it("the URL follows tab changes, by click and by the arrow-key tablist", async () => {
    renderAt("/schedules?keep=1");
    await waitFor(() => expect(screen.getByText("Sweep eligible issues")).toBeTruthy());
    fireEvent.click(tabNamed(/^Job catalog/));
    expect(screen.getByTestId("loc").textContent).toBe("?keep=1&tab=catalog");
    expect(tabNamed(/^Job catalog/).getAttribute("aria-selected")).toBe("true");

    // APG roving tablist: ArrowRight wraps back to Schedules and moves focus with it.
    fireEvent.keyDown(tabNamed(/^Job catalog/), { key: "ArrowRight" });
    expect(screen.getByTestId("loc").textContent).toBe("?keep=1&tab=schedules");
    expect(document.activeElement).toBe(tabNamed(/^Schedules/));
  });

  it("counts every schedule of both origins, and pills catalog entries enabled nowhere", async () => {
    mockApi.listScheduleCatalog.mockResolvedValue({
      entries: [CATALOG.entries[0], { ...CATALOG.entries[0], slug: "refactor-scout", name: "Refactor scout" }],
      enablements: [],
    });
    mockApi.listSchedules.mockResolvedValue([
      customizedDefault(),
      sched({ id: "u1", target: "prompt", prompt: "mine" }),
    ]);
    renderAt("/schedules");
    await waitFor(() => expect(tabNamed(/^Schedules · 2$/)).toBeTruthy());
    expect(tabNamed(/^Job catalog · 2/).textContent).toContain("1 not enabled");
  });

  it("hides the not-enabled pill when every entry runs somewhere", async () => {
    mockApi.listScheduleCatalog.mockResolvedValue(CATALOG);
    mockApi.listSchedules.mockResolvedValue([customizedDefault({ enabled: false })]);
    renderAt("/schedules");
    await waitFor(() => expect(tabNamed(/^Schedules · 1$/)).toBeTruthy());
    // Positive control: the catalog tab rendered its count.
    expect(tabNamed(/^Job catalog · 1$/)).toBeTruthy();
    expect(screen.queryByText(/not enabled$/)).toBeNull();
  });

  it("the empty Schedules tab shows the D8 copy and links to the catalog", async () => {
    mockApi.listSchedules.mockResolvedValue([]);
    mockApi.listScheduleCatalog.mockResolvedValue(CATALOG);
    renderAt("/schedules?tab=schedules");
    await waitFor(() =>
      expect(
        screen.getByText("Nothing runs on a clock yet. Enable a shipped job from the Job catalog, or create your own."),
      ).toBeTruthy(),
    );
    // Both New schedule buttons (header + empty state) are offered.
    expect(screen.getAllByRole("button", { name: /New schedule/ })).toHaveLength(2);
    fireEvent.click(screen.getByRole("button", { name: "Open the Job catalog" }));
    expect(tabNamed(/^Job catalog/).getAttribute("aria-selected")).toBe("true");
  });
});

describe("Schedules — default rows on the unified list (PRD #1645 D4)", () => {
  it("a default row shows its OWN cron, timezone and model, never the catalog's", async () => {
    mockApi.listScheduleCatalog.mockResolvedValue(CATALOG);
    mockApi.listSchedules.mockResolvedValue([customizedDefault()]);
    renderPage();

    const name = await screen.findByText("Bug triage sweep");
    const row = name.closest("tr")!;
    expect(within(row).getByText("30 7 * * 1-5")).toBeTruthy();
    expect(within(row).getByText(/Europe\/Bucharest/)).toBeTruthy();
    expect(within(row).getByText("model claude-sonnet-4-5")).toBeTruthy();
    // The catalog's values (0 2 * * *, UTC, inherit model) appear nowhere on the row.
    expect(within(row).queryByText("0 2 * * *")).toBeNull();
    expect(within(row).queryByText(/UTC/)).toBeNull();
    expect(within(row).queryByText("inherit model")).toBeNull();
    // Sealed labels and the max-issues cap are chips from the row too.
    expect(within(row).getByText("label bug")).toBeTruthy();
    expect(within(row).getByText("max 3")).toBeTruthy();
    // Provenance: lock marker, kind pill, customized badge.
    expect(within(row).getByLabelText("Baked prompt, read-only")).toBeTruthy();
    expect(within(row).getByText("sweep")).toBeTruthy();
    expect(within(row).getByText("customized")).toBeTruthy();
  });

  it("a customized default shows Reset inline (scope in its tooltip); a non-customized one offers none", async () => {
    mockApi.listScheduleCatalog.mockResolvedValue(CATALOG);
    mockApi.listSchedules.mockResolvedValue([
      customizedDefault(),
      customizedDefault({ id: "d2", repo_id: "repo-atlas", repo_path: "vtmocanu/atlas", customized: false }),
    ]);
    mockApi.resetSchedule.mockResolvedValue(customizedDefault({ customized: false }));
    renderPage();

    const reset = await screen.findByRole("button", { name: "Reset Bug triage sweep on vtmocanu/uzi to catalog defaults" });
    expect(reset.getAttribute("title")).toBe(
      "Restores all editable settings to the catalog defaults and clears your guidance, harness and token overrides. The shipped prompt and labels are unchanged.",
    );
    // Exactly one Reset: the non-customized atlas row has none inline...
    expect(screen.getAllByRole("button", { name: /^Reset / })).toHaveLength(1);
    expect(screen.queryByRole("button", { name: /Reset Bug triage sweep on vtmocanu\/atlas/ })).toBeNull();
    // ...nor in its menu.
    fireEvent.click(screen.getByRole("button", { name: "More actions for Bug triage sweep on vtmocanu/atlas" }));
    expect(screen.queryByRole("menuitem", { name: /Reset/ })).toBeNull();
    fireEvent.keyDown(screen.getByRole("menu"), { key: "Escape" });

    fireEvent.click(reset);
    await waitFor(() => expect(mockApi.resetSchedule).toHaveBeenCalledWith("d1"));
  });

  it("toggle, Run now and Edit are on the row with no expand step", async () => {
    mockApi.listScheduleCatalog.mockResolvedValue(CATALOG);
    mockApi.listSchedules.mockResolvedValue([customizedDefault({ customized: false })]);
    mockApi.runScheduleNow.mockResolvedValue({ created: 1, run_ids: ["r"], matched: 1, capped: false, started: [], skips: [] });
    mockApi.updateSchedule.mockImplementation(async (id: string, input) => customizedDefault({ id, enabled: input.enabled ?? true }));
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "Run now: Bug triage sweep on vtmocanu/uzi" }));
    await waitFor(() => expect(mockApi.runScheduleNow).toHaveBeenCalledWith("d1"));
    fireEvent.click(screen.getByRole("switch", { name: "Pause Bug triage sweep on vtmocanu/uzi" }));
    await waitFor(() => expect(mockApi.updateSchedule).toHaveBeenCalledWith("d1", { enabled: false }));
    fireEvent.click(screen.getByRole("button", { name: "Edit Bug triage sweep on vtmocanu/uzi" }));
    expect(await screen.findByRole("dialog")).toBeTruthy();
    expect(screen.queryByRole("button", { name: /Show repos for/ })).toBeNull();
  });

  it("a paused default reads 'Resume …' on its toggle", async () => {
    mockApi.listScheduleCatalog.mockResolvedValue(CATALOG);
    mockApi.listSchedules.mockResolvedValue([customizedDefault({ enabled: false })]);
    renderPage();
    expect(await screen.findByRole("switch", { name: "Resume Bug triage sweep on vtmocanu/uzi" })).toBeTruthy();
  });

  it("'from catalog' switches to the Job catalog and focuses that entry", async () => {
    mockApi.listScheduleCatalog.mockResolvedValue(CATALOG);
    mockApi.listSchedules.mockResolvedValue([customizedDefault()]);
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "from catalog: Bug triage sweep" }));
    expect(tabNamed(/^Job catalog/).getAttribute("aria-selected")).toBe("true");
    const card = document.getElementById("catalog-entry-bug-triage");
    expect(card).not.toBeNull();
    await waitFor(() => expect(document.activeElement).toBe(card));
    // It only focuses the card: no enable dialog opens.
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("the menu's 'Enable on another repo' opens that entry's enable dialog on the Job catalog (D4)", async () => {
    mockApi.listScheduleCatalog.mockResolvedValue(CATALOG);
    mockApi.listSchedules.mockResolvedValue([customizedDefault()]);
    mockApi.listRepos.mockResolvedValue(REPOS3);
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "More actions for Bug triage sweep on vtmocanu/uzi" }));
    // Default rows offer Enable-on-another-repo, never Add-to-another-repo.
    expect(screen.queryByRole("menuitem", { name: "Add to another repo" })).toBeNull();
    fireEvent.click(screen.getByRole("menuitem", { name: "Enable on another repo" }));

    expect(tabNamed(/^Job catalog/).getAttribute("aria-selected")).toBe("true");
    const dialog = await screen.findByRole("dialog", { name: "Enable Bug triage sweep on" });
    expect(document.getElementById("catalog-entry-bug-triage")!.contains(dialog)).toBe(true);
    await waitFor(() => expect(document.activeElement).toBe(dialog));
    // The row's own repo is the one already enabled.
    const uzi = within(dialog).getByRole("checkbox", { name: /vtmocanu\/uzi/ }) as HTMLInputElement;
    expect(uzi.checked && uzi.disabled).toBe(true);
    // Escape returns focus to the card's Enable on… button, not <body>.
    fireEvent.keyDown(document, { key: "Escape" });
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Enable Bug triage sweep on…" }));
  });
});

describe("Schedules — D3 sort on the page", () => {
  it("orders parked first, then enabled by next fire, then enabled with none, then paused", async () => {
    mockApi.listScheduleCatalog.mockResolvedValue(CATALOG);
    const soon = new Date(Date.now() + 1 * 3_600_000).toISOString();
    const later = new Date(Date.now() + 5 * 3_600_000).toISOString();
    mockApi.listSchedules.mockResolvedValue([
      sched({ id: "paused", target: "prompt", prompt: "a paused one", enabled: false }),
      sched({ id: "later", target: "prompt", prompt: "b later", next_fire_at: later }),
      sched({ id: "none", target: "prompt", prompt: "c no next fire", next_fire_at: null }),
      customizedDefault({ id: "soon", next_fire_at: soon, next_fires: [soon] }),
      sched({ id: "parked", target: "prompt", prompt: "z parked", status: "error" }),
    ]);
    renderPage();
    await waitFor(() => expect(screen.getByText("Prompt: z parked")).toBeTruthy());
    const order = [...document.querySelectorAll<HTMLElement>("[id^='schedule-name-']")].map((el) =>
      el.id.replace("schedule-name-", ""),
    );
    expect(order).toEqual(["parked", "soon", "later", "none", "paused"]);
  });
});

describe("Schedules — clone reveals the copy (PRD #1645 D12)", () => {
  it("cloning keeps the edit modal over the Schedules tab and focuses the clone when it closes", async () => {
    const clone = sched({ id: "c1", target: "sweep", labels: ["bug"], origin: "user" });
    mockApi.listScheduleCatalog.mockResolvedValue(CATALOG);
    mockApi.listSchedules
      .mockResolvedValueOnce([customizedDefault()])
      .mockResolvedValue([customizedDefault(), clone]);
    mockApi.cloneSchedule.mockResolvedValue(clone);
    renderAt("/schedules");

    // The catalog tab carries no per-repo controls (D7): clone lives in the row's menu.
    fireEvent.click(await screen.findByRole("button", { name: "More actions for Bug triage sweep on vtmocanu/uzi" }));
    fireEvent.click(screen.getByRole("menuitem", { name: "Clone to an editable copy" }));
    await waitFor(() => expect(mockApi.cloneSchedule).toHaveBeenCalledWith("d1"));

    // Switched to Schedules with the clone rendered behind the open edit modal.
    await waitFor(() => expect(screen.getByTestId("loc").textContent).toBe("?tab=schedules"));
    expect(tabNamed(/^Schedules/).getAttribute("aria-selected")).toBe("true");
    expect(await screen.findByRole("dialog")).toBeTruthy();
    const nameCell = await waitFor(() => {
      const el = document.getElementById("schedule-name-c1");
      expect(el).not.toBeNull();
      return el!;
    });
    expect(document.activeElement).not.toBe(nameCell); // the modal holds focus while open

    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    await waitFor(() => expect(document.activeElement).toBe(nameCell));
  });
});

// ── PRD #1645 M1 review follow-ups: history, focus handoffs, card order, orphan slugs ──
function BackProbe() {
  const navigate = useNavigate();
  const loc = useLocation();
  return (
    <>
      <output data-testid="path">{loc.pathname + loc.search}</output>
      <button type="button" onClick={() => navigate(-1)}>
        history back
      </button>
    </>
  );
}

describe("Schedules — M1 review follow-ups", () => {
  // D1: a tab change REPLACES the history entry, so Back leaves the page rather than
  // stepping back through tab switches.
  it("a tab change replaces the history entry: Back returns to the previous page", async () => {
    render(
      <MemoryRouter initialEntries={["/before", "/schedules"]} initialIndex={1}>
        <Schedules />
        <BackProbe />
      </MemoryRouter>,
    );
    await waitFor(() => expect(screen.getByText("Sweep eligible issues")).toBeTruthy());
    fireEvent.click(tabNamed(/^Job catalog/));
    expect(screen.getByTestId("path").textContent).toBe("/schedules?tab=catalog");
    fireEvent.click(tabNamed(/^Schedules/));
    expect(screen.getByTestId("path").textContent).toBe("/schedules?tab=schedules");
    fireEvent.click(screen.getByRole("button", { name: "history back" }));
    expect(screen.getByTestId("path").textContent).toBe("/before");
  });

  const moreButtons = () => screen.getAllByRole("button", { name: /^More actions for / });

  it("Remove hands focus to the next row's name cell", async () => {
    mockApi.listSchedules
      .mockResolvedValueOnce([
        sched({ id: "s1", target: "sweep", enabled: true }),
        sched({ id: "s2", target: "prompt", prompt: "hunt flaky tests", enabled: false }),
      ])
      .mockResolvedValue([sched({ id: "s2", target: "prompt", prompt: "hunt flaky tests", enabled: false })]);
    mockApi.deleteSchedule.mockResolvedValue(null);
    renderPage();
    await waitFor(() => expect(moreButtons()).toHaveLength(2));
    fireEvent.click(moreButtons()[0]);
    fireEvent.click(screen.getByRole("menuitem", { name: "Remove" }));
    await waitFor(() => expect(mockApi.deleteSchedule).toHaveBeenCalledWith("s1"));
    await waitFor(() => expect(document.activeElement).toBe(document.getElementById("schedule-name-s2")));
  });

  it("Remove of the last row hands focus to the previous row, and of the only row to the Schedules tab", async () => {
    mockApi.listSchedules
      .mockResolvedValueOnce([
        sched({ id: "s1", target: "sweep", enabled: true }),
        sched({ id: "s2", target: "prompt", prompt: "hunt flaky tests", enabled: false }),
      ])
      .mockResolvedValueOnce([sched({ id: "s1", target: "sweep", enabled: true })])
      .mockResolvedValue([]);
    mockApi.deleteSchedule.mockResolvedValue(null);
    renderPage();
    await waitFor(() => expect(moreButtons()).toHaveLength(2));
    fireEvent.click(moreButtons()[1]);
    fireEvent.click(screen.getByRole("menuitem", { name: "Remove" }));
    await waitFor(() => expect(mockApi.deleteSchedule).toHaveBeenCalledWith("s2"));
    await waitFor(() => expect(document.activeElement).toBe(document.getElementById("schedule-name-s1")));

    fireEvent.click(moreButtons()[0]);
    fireEvent.click(screen.getByRole("menuitem", { name: "Remove" }));
    await waitFor(() => expect(mockApi.deleteSchedule).toHaveBeenCalledWith("s1"));
    await waitFor(() => expect(screen.getByText(/Nothing runs on a clock yet/)).toBeTruthy());
    await waitFor(() => expect(document.activeElement).toBe(tabNamed(/^Schedules/)));
  });

  const siblings = () => [
    sched({ id: "g1", target: "prompt", prompt: "grouped job", repo_id: "repo-uzi", repo_path: "vtmocanu/uzi", sibling_group_id: "grp-1" }),
    sched({ id: "g2", target: "prompt", prompt: "grouped job", repo_id: "repo-atlas", repo_path: "vtmocanu/atlas", sibling_group_id: "grp-1" }),
  ];
  const UZI_MORE = "More actions for Prompt: grouped job on vtmocanu/uzi";

  it("Cancel on the add-repo panel returns focus to the row's More actions button", async () => {
    mockApi.listRepos.mockResolvedValue(REPOS3);
    mockApi.listSchedules.mockResolvedValue(siblings());
    renderPage();
    const more = await screen.findByRole("button", { name: UZI_MORE });
    fireEvent.click(more);
    fireEvent.click(screen.getByRole("menuitem", { name: "Add to another repo" }));
    const picker = await screen.findByRole("combobox", { name: /Add Prompt: grouped job on another repo/ });
    await waitFor(() => expect(document.activeElement).toBe(picker));
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(screen.queryByRole("combobox", { name: /Add Prompt: grouped job on another repo/ })).toBeNull();
    expect(document.activeElement).toBe(more);
  });

  it.each([
    ["a 409", 409],
    ["an error", 500],
  ])("after %s from add-repo, focus is on the row's More actions button", async (_label, status) => {
    mockApi.listRepos.mockResolvedValue(REPOS3);
    mockApi.listSchedules.mockResolvedValue(siblings());
    const { ApiError } = await vi.importActual<typeof import("../lib/api")>("../lib/api");
    mockApi.addScheduleRepo.mockRejectedValue(new ApiError(status, "nope"));
    renderPage();
    const more = await screen.findByRole("button", { name: UZI_MORE });
    fireEvent.click(more);
    fireEvent.click(screen.getByRole("menuitem", { name: "Add to another repo" }));
    const picker = await screen.findByRole("combobox", { name: /Add Prompt: grouped job on another repo/ });
    fireEvent.change(picker, { target: { value: "repo-new" } });
    fireEvent.click(screen.getByRole("button", { name: "Add" }));
    await waitFor(() => expect(mockApi.addScheduleRepo).toHaveBeenCalledWith("g1", "repo-new"));
    await waitFor(() => expect(screen.queryByText(status === 409 ? /already on that repo/i : /nope/i)).toBeTruthy());
    expect(document.activeElement).toBe(screen.getByRole("button", { name: UZI_MORE }));
  });

  // D11 / N3: the toggle is rendered once, where it is drawn, so DOM and focus order
  // match the layout: on a phone card it leads (name line), on a table row it trails.
  describe("toggle placement by layout", () => {
    const setMatchMedia = (matches: boolean) => {
      Object.defineProperty(window, "matchMedia", {
        configurable: true,
        writable: true,
        value: vi.fn(() => ({ matches, addEventListener: vi.fn(), removeEventListener: vi.fn() })),
      });
    };
    afterEach(() => {
      delete (window as unknown as { matchMedia?: unknown }).matchMedia;
    });

    it("below md the one toggle sits in the name cell, before the row's action buttons", async () => {
      setMatchMedia(false);
      mockApi.listSchedules.mockResolvedValue([sched({ id: "s1", target: "sweep", enabled: true })]);
      renderPage();
      const sw = await screen.findByRole("switch");
      expect(screen.getAllByRole("switch")).toHaveLength(1);
      expect(sw.closest("td")!.contains(document.getElementById("schedule-name-s1"))).toBe(true);
      const runNow = screen.getByRole("button", { name: /^Run now: / });
      expect(sw.compareDocumentPosition(runNow) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    });

    it("from md up the one toggle trails the More actions button in the last column", async () => {
      setMatchMedia(true);
      mockApi.listSchedules.mockResolvedValue([sched({ id: "s1", target: "sweep", enabled: true })]);
      renderPage();
      const sw = await screen.findByRole("switch");
      expect(screen.getAllByRole("switch")).toHaveLength(1);
      expect(sw.closest("td")!.contains(document.getElementById("schedule-name-s1"))).toBe(false);
      const more = screen.getByRole("button", { name: /^More actions for / });
      expect(more.compareDocumentPosition(sw) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    });
  });

  // N7: a default whose slug the catalog no longer carries has nothing to show or enable.
  it("a default row with no catalog entry offers no 'from catalog' link and no 'Enable on another repo'", async () => {
    mockApi.listScheduleCatalog.mockResolvedValue(CATALOG);
    mockApi.listSchedules.mockResolvedValue([customizedDefault({ catalog_slug: "retired-job" })]);
    renderPage();
    const more = await screen.findByRole("button", { name: /^More actions for / });
    expect(screen.queryByRole("button", { name: /^from catalog/ })).toBeNull();
    fireEvent.click(more);
    expect(screen.getByRole("menuitem", { name: "Clone to an editable copy" })).toBeTruthy();
    expect(screen.queryByRole("menuitem", { name: "Enable on another repo" })).toBeNull();
    expect(screen.queryByRole("menuitem", { name: "Add to another repo" })).toBeNull();
  });
});

// ── PRD #1645 M2: D5 filters, D6 fold, D12 reveal through them ───────────────
const nameIds = () =>
  [...document.querySelectorAll<HTMLElement>("[id^='schedule-name-']")].map((el) => el.id.replace("schedule-name-", ""));
const chip = (label: RegExp) => screen.getByRole("button", { name: label });
const FIRED_AT = new Date(Date.now() - 86_400_000).toISOString();
const firedOnce = (over: Partial<Schedule>) =>
  sched({ target: "issue", timing: "once", status: "fired", run_at: FIRED_AT, next_fire_at: null, cron_expr: "", ...over });

describe("Schedules — filters (PRD #1645 D5)", () => {
  // Five rows over two repos: two catalog defaults (one paused), three user rows (one paused).
  const mixed = () => [
    customizedDefault({ id: "d1", customized: false }),
    customizedDefault({ id: "d2", repo_id: "repo-atlas", repo_path: "vtmocanu/atlas", enabled: false, customized: false }),
    sched({ id: "u1", target: "prompt", prompt: "alpha", repo_id: "repo-atlas", repo_path: "vtmocanu/atlas" }),
    sched({ id: "u2", target: "prompt", prompt: "beta", enabled: false }),
    sched({ id: "u3", target: "prompt", prompt: "gamma" }),
  ];
  beforeEach(() => {
    mockApi.listScheduleCatalog.mockResolvedValue(CATALOG);
    mockApi.listSchedules.mockResolvedValue(mixed());
  });

  it("each source chip shows only its rows and is the one pressed", async () => {
    renderPage();
    await waitFor(() => expect(nameIds()).toHaveLength(5));
    expect(chip(/^All/).getAttribute("aria-pressed")).toBe("true");

    fireEvent.click(chip(/^From catalog/));
    expect(nameIds().sort()).toEqual(["d1", "d2"]);
    expect(chip(/^From catalog/).getAttribute("aria-pressed")).toBe("true");
    expect(chip(/^All/).getAttribute("aria-pressed")).toBe("false");

    fireEvent.click(chip(/^Mine/));
    expect(nameIds().sort()).toEqual(["u1", "u2", "u3"]);

    fireEvent.click(chip(/^Paused/));
    expect(nameIds().sort()).toEqual(["d2", "u2"]);

    fireEvent.click(chip(/^All/));
    expect(nameIds()).toHaveLength(5);
  });

  it("chip counts describe the full set, not the current intersection", async () => {
    renderPage();
    await waitFor(() => expect(nameIds()).toHaveLength(5));
    // Narrow by repo AND source: the intersection holds one row (u1)...
    fireEvent.change(screen.getByRole("combobox", { name: "Repo" }), { target: { value: "repo-atlas" } });
    fireEvent.click(chip(/^Mine/));
    expect(nameIds()).toEqual(["u1"]);
    // ...but every chip still counts over all five schedules.
    expect(chip(/^All/).textContent).toBe("All5");
    expect(chip(/^From catalog/).textContent).toBe("From catalog2");
    expect(chip(/^Mine/).textContent).toBe("Mine3");
    expect(chip(/^Paused/).textContent).toBe("Paused2");
  });

  it("the Repo select appears only when schedules span two or more repos, and filters by repo", async () => {
    renderPage();
    const repo = await screen.findByRole("combobox", { name: "Repo" });
    fireEvent.change(repo, { target: { value: "repo-uzi" } });
    expect(nameIds().sort()).toEqual(["d1", "u2", "u3"]);
    cleanup();

    // One repo only (the default fixture): no select. Positive control first.
    mockApi.listSchedules.mockResolvedValue([sched({ id: "s1" }), sched({ id: "s2", target: "prompt", prompt: "x" })]);
    renderPage();
    await waitFor(() => expect(nameIds()).toHaveLength(2));
    expect(screen.queryByRole("combobox", { name: "Repo" })).toBeNull();
  });

  it("an empty filtered result shows 'No schedules match'; Clear filters restores every row and focuses All", async () => {
    mockApi.listSchedules.mockResolvedValue([sched({ id: "s1" }), sched({ id: "s2", target: "prompt", prompt: "x" })]);
    renderPage();
    await waitFor(() => expect(nameIds()).toHaveLength(2));
    fireEvent.click(chip(/^Paused/));
    expect(screen.getByText("No schedules match")).toBeTruthy();
    expect(nameIds()).toEqual([]);
    // The D8 no-schedules copy is for an owner with none, not for a filter.
    expect(screen.queryByText(/Nothing runs on a clock yet/)).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "Clear filters" }));
    expect(nameIds()).toHaveLength(2);
    expect(screen.queryByText("No schedules match")).toBeNull();
    expect(chip(/^All/).getAttribute("aria-pressed")).toBe("true");
    expect(document.activeElement).toBe(chip(/^All/));
  });

  it("filter state survives a round-trip through the Job catalog tab", async () => {
    renderPage();
    await waitFor(() => expect(nameIds()).toHaveLength(5));
    fireEvent.click(chip(/^Paused/));
    fireEvent.change(screen.getByRole("combobox", { name: "Repo" }), { target: { value: "repo-atlas" } });
    expect(nameIds()).toEqual(["d2"]);

    fireEvent.click(tabNamed(/^Job catalog/));
    expect(screen.queryByRole("button", { name: /^Paused/ })).toBeNull(); // the list is unmounted
    fireEvent.click(tabNamed(/^Schedules/));

    expect(chip(/^Paused/).getAttribute("aria-pressed")).toBe("true");
    expect((screen.getByRole("combobox", { name: "Repo" }) as HTMLSelectElement).value).toBe("repo-atlas");
    expect(nameIds()).toEqual(["d2"]);
  });
});

describe("Schedules — fired one-shots fold away (PRD #1645 D6)", () => {
  const disclosure = () => screen.getByRole("button", { name: /one-time schedules? already fired/ });

  it("folds fired one-shots into one disclosure, counted within the filtered set, with their issue refs", async () => {
    mockApi.listSchedules.mockResolvedValue([
      sched({ id: "live-uzi", target: "prompt", prompt: "live on uzi" }),
      sched({ id: "live-atlas", target: "prompt", prompt: "live on atlas", repo_id: "repo-atlas", repo_path: "vtmocanu/atlas" }),
      firedOnce({ id: "f1", issue_iid: 158 }),
      firedOnce({ id: "f2", issue_iid: 159 }),
      firedOnce({ id: "f3", issue_iid: 160, repo_id: "repo-atlas", repo_path: "vtmocanu/atlas" }),
    ]);
    renderPage();
    await waitFor(() => expect(nameIds()).toEqual(["live-atlas", "live-uzi"].sort()));
    expect(disclosure().textContent).toContain("3 one-time schedules already fired");
    expect(disclosure().textContent).toContain("#158 #159 #160");
    expect(disclosure().getAttribute("aria-expanded")).toBe("false");
    const controls = disclosure().getAttribute("aria-controls")!;
    expect(document.getElementById(controls)).not.toBeNull();

    // Within the uzi repo only two fired rows survive the filter.
    fireEvent.change(screen.getByRole("combobox", { name: "Repo" }), { target: { value: "repo-uzi" } });
    expect(disclosure().textContent).toContain("2 one-time schedules already fired");
    expect(disclosure().textContent).not.toContain("#160");
    expect(nameIds()).toEqual(["live-uzi"]);

    // Expanding shows them as normal rows, inside the controlled region.
    fireEvent.click(disclosure());
    expect(disclosure().getAttribute("aria-expanded")).toBe("true");
    expect(nameIds()).toEqual(["live-uzi", "f1", "f2"]);
    expect(document.getElementById(controls)!.contains(document.getElementById("schedule-name-f1"))).toBe(true);
  });

  it("a single fired one-shot still folds", async () => {
    mockApi.listSchedules.mockResolvedValue([sched({ id: "live" }), firedOnce({ id: "f1", issue_iid: 158 })]);
    renderPage();
    await waitFor(() => expect(nameIds()).toEqual(["live"]));
    expect(disclosure().textContent).toContain("1 one-time schedule already fired");
  });

  it("starts expanded when the only matching rows are folded; a manual collapse holds until the filters change", async () => {
    mockApi.listSchedules.mockResolvedValue([
      sched({ id: "live", target: "prompt", prompt: "live" }),
      firedOnce({ id: "f1", issue_iid: 158, repo_id: "repo-atlas", repo_path: "vtmocanu/atlas" }),
    ]);
    renderPage();
    await waitFor(() => expect(nameIds()).toEqual(["live"]));
    expect(disclosure().getAttribute("aria-expanded")).toBe("false");

    fireEvent.change(screen.getByRole("combobox", { name: "Repo" }), { target: { value: "repo-atlas" } });
    // Only the folded row matches: the fold opens by itself, never an empty-looking list.
    expect(disclosure().getAttribute("aria-expanded")).toBe("true");
    expect(nameIds()).toEqual(["f1"]);
    expect(screen.queryByText("No schedules match")).toBeNull();

    fireEvent.click(disclosure());
    expect(disclosure().getAttribute("aria-expanded")).toBe("false");
    expect(nameIds()).toEqual([]);

    // A filter change re-derives the default (still only folded rows match).
    fireEvent.click(chip(/^Mine/));
    expect(disclosure().getAttribute("aria-expanded")).toBe("true");
    expect(nameIds()).toEqual(["f1"]);
  });

  it("a parked one-shot is never folded", async () => {
    mockApi.listSchedules.mockResolvedValue([
      sched({ id: "live" }),
      firedOnce({ id: "parked", issue_iid: 7, status: "error" }),
    ]);
    renderPage();
    await waitFor(() => expect(nameIds()).toEqual(["parked", "live"]));
    expect(screen.queryByRole("button", { name: /already fired/ })).toBeNull();
  });
});

describe("Schedules — reveal clears hiding filters and opens the fold (PRD #1645 D12)", () => {
  it("clone clears a hiding source filter and reveals the copy, focused when the modal closes", async () => {
    const clone = sched({ id: "c1", target: "sweep", labels: ["bug"], origin: "user" });
    mockApi.listScheduleCatalog.mockResolvedValue(CATALOG);
    mockApi.listSchedules
      .mockResolvedValueOnce([customizedDefault(), sched({ id: "u9", target: "prompt", prompt: "other" })])
      .mockResolvedValue([customizedDefault(), sched({ id: "u9", target: "prompt", prompt: "other" }), clone]);
    mockApi.cloneSchedule.mockResolvedValue(clone);
    renderPage();
    await waitFor(() => expect(nameIds()).toHaveLength(2));
    fireEvent.click(chip(/^From catalog/));
    expect(nameIds()).toEqual(["d1"]);

    fireEvent.click(screen.getByRole("button", { name: "More actions for Bug triage sweep on vtmocanu/uzi" }));
    fireEvent.click(screen.getByRole("menuitem", { name: "Clone to an editable copy" }));
    expect(await screen.findByRole("dialog")).toBeTruthy();
    // The user-origin clone would be hidden by "From catalog": that filter is cleared.
    await waitFor(() => expect(chip(/^All/).getAttribute("aria-pressed")).toBe("true"));
    const nameCell = await waitFor(() => {
      const el = document.getElementById("schedule-name-c1");
      expect(el).not.toBeNull();
      return el!;
    });
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(document.activeElement).toBe(nameCell));
  });

  const pair = () => [
    sched({ id: "g1", target: "prompt", prompt: "grouped job", repo_id: "repo-uzi", repo_path: "vtmocanu/uzi", sibling_group_id: "grp-1" }),
    sched({ id: "g2", target: "prompt", prompt: "other job", repo_id: "repo-atlas", repo_path: "vtmocanu/atlas" }),
  ];
  const addFromG1 = async () => {
    fireEvent.click(await screen.findByRole("button", { name: "More actions for Prompt: grouped job on vtmocanu/uzi" }));
    fireEvent.click(screen.getByRole("menuitem", { name: "Add to another repo" }));
    const picker = await screen.findByRole("combobox", { name: /Add Prompt: grouped job on another repo/ });
    fireEvent.change(picker, { target: { value: "repo-new" } });
    fireEvent.click(screen.getByRole("button", { name: "Add" }));
  };

  it("add-repo clears a hiding repo filter and focuses the new row", async () => {
    mockApi.listRepos.mockResolvedValue(REPOS3);
    const added = sched({ id: "g3", target: "prompt", prompt: "grouped job", repo_id: "repo-new", repo_path: "vtmocanu/newrepo", sibling_group_id: "grp-1" });
    mockApi.listSchedules.mockResolvedValueOnce(pair()).mockResolvedValue([...pair(), added]);
    mockApi.addScheduleRepo.mockResolvedValue(added);
    renderPage();
    fireEvent.change(await screen.findByRole("combobox", { name: "Repo" }), { target: { value: "repo-uzi" } });
    expect(nameIds()).toEqual(["g1"]);

    await addFromG1();
    await waitFor(() => expect(document.activeElement).toBe(document.getElementById("schedule-name-g3")));
    expect((screen.getByRole("combobox", { name: "Repo" }) as HTMLSelectElement).value).toBe("");
  });

  it("add-repo whose new row lands in the fold expands it and focuses the row", async () => {
    mockApi.listRepos.mockResolvedValue(REPOS3);
    const added = firedOnce({ id: "g3", target: "prompt", prompt: "grouped job", repo_id: "repo-new", repo_path: "vtmocanu/newrepo", sibling_group_id: "grp-1" });
    mockApi.listSchedules.mockResolvedValueOnce(pair()).mockResolvedValue([...pair(), added]);
    mockApi.addScheduleRepo.mockResolvedValue(added);
    renderPage();
    await addFromG1();
    await waitFor(() => expect(document.activeElement).toBe(document.getElementById("schedule-name-g3")));
    expect(screen.getByRole("button", { name: /already fired/ }).getAttribute("aria-expanded")).toBe("true");
  });

  it("a reveal survives an overlapping reload that superseded the one it awaited", async () => {
    // addRepo awaits reload B; a toggle's reload C starts before B settles, so B's list is
    // dropped as stale and the page still shows the pre-add list when the reveal is asked
    // for. That older list must not cancel the reveal; C's list then brings the row.
    mockApi.listRepos.mockResolvedValue(REPOS3);
    const added = sched({ id: "g3", target: "prompt", prompt: "grouped job", repo_id: "repo-new", repo_path: "vtmocanu/newrepo", sibling_group_id: "grp-1" });
    type Rows = Schedule[];
    let resolveB: (v: Rows) => void = () => {};
    let resolveC: (v: Rows) => void = () => {};
    mockApi.listSchedules
      .mockResolvedValueOnce(pair())
      .mockReturnValueOnce(new Promise<Rows>((r) => (resolveB = r)))
      .mockReturnValueOnce(new Promise<Rows>((r) => (resolveC = r)));
    mockApi.addScheduleRepo.mockResolvedValue(added);
    mockApi.updateSchedule.mockImplementation(async (id: string, input) => ({ ...pair()[1], id, enabled: input.enabled ?? true }));
    renderPage();

    await addFromG1();
    await waitFor(() => expect(mockApi.listSchedules).toHaveBeenCalledTimes(2)); // B in flight
    fireEvent.click(screen.getByRole("switch", { name: "Pause Prompt: other job on vtmocanu/atlas" }));
    await waitFor(() => expect(mockApi.listSchedules).toHaveBeenCalledTimes(3)); // C in flight
    await act(async () => resolveB([...pair(), added])); // superseded: never applied
    // The reveal is pending on the old list; focus has not been sent anywhere yet.
    expect(document.getElementById("schedule-name-g3")).toBeNull();
    expect(document.activeElement).not.toBe(tabNamed(/^Schedules/));

    await act(async () => resolveC([...pair(), added]));
    await waitFor(() => expect(document.activeElement).toBe(document.getElementById("schedule-name-g3")));
  });
});

describe("Schedules — the toggle keeps focus across a layout flip (PRD #1645 D11)", () => {
  let matches = true;
  const listeners = new Set<() => void>();
  beforeEach(() => {
    matches = true;
    listeners.clear();
    Object.defineProperty(window, "matchMedia", {
      configurable: true,
      writable: true,
      value: vi.fn(() => ({
        get matches() {
          return matches;
        },
        addEventListener: (_: string, l: () => void) => listeners.add(l),
        removeEventListener: (_: string, l: () => void) => listeners.delete(l),
      })),
    });
  });
  afterEach(() => {
    delete (window as unknown as { matchMedia?: unknown }).matchMedia;
  });
  const flip = (to: boolean) =>
    act(() => {
      matches = to;
      listeners.forEach((l) => l());
    });

  it("a focused toggle is refocused after it moves between cells", async () => {
    mockApi.listSchedules.mockResolvedValue([sched({ id: "s1" })]);
    renderPage();
    const before = await screen.findByRole("switch");
    before.focus();
    flip(false);
    const after = screen.getByRole("switch");
    expect(after).not.toBe(before); // it really was remounted in the other cell
    expect(document.activeElement).toBe(after);
    flip(true);
    expect(document.activeElement).toBe(screen.getByRole("switch"));
  });

  it("an unfocused toggle does not take focus on a flip (control)", async () => {
    mockApi.listSchedules.mockResolvedValue([sched({ id: "s1" })]);
    renderPage();
    await screen.findByRole("switch");
    const runNow = screen.getByRole("button", { name: /^Run now: / });
    runNow.focus();
    flip(false);
    expect(document.activeElement).toBe(runNow);
  });
});

// ── PRD #1093 M4: the "Pause all schedules" switch (button / picker / banner) ───
describe("Schedules — pause all (PRD #1093)", () => {
  function renderRoot() {
    return render(
      <MemoryRouter>
        <Schedules />
      </MemoryRouter>,
    );
  }

  it("shows the Pause all button while not paused and no paused banner (running state)", async () => {
    mockApi.getSchedulePause.mockResolvedValue({ paused: false, until: null });
    renderRoot();
    await waitFor(() => expect(screen.getByRole("button", { name: /Pause all/ })).toBeTruthy());
    // Positive control that the page loaded, so the banner-absent check below is meaningful.
    expect(screen.getByRole("tab", { name: /^Schedules/ })).toBeTruthy();
    expect(screen.queryByText(/All schedules paused/)).toBeNull();
  });

  it("hides the Pause all button and shows the paused banner while paused (both directions)", async () => {
    const future = new Date(Date.now() + 6 * 3_600_000).toISOString();
    mockApi.getSchedulePause.mockResolvedValue({ paused: true, until: future });
    renderRoot();
    // The banner appears, carrying the auto-resume stamp.
    await waitFor(() => expect(screen.getByText(/All schedules paused until/)).toBeTruthy());
    // The tab-row button is gone (one control per state).
    expect(screen.queryByRole("button", { name: /^Pause all$/ })).toBeNull();
    // Resume now is present.
    expect(screen.getByRole("button", { name: "Resume now" })).toBeTruthy();
  });

  it("an indefinite pause reads 'until you resume them' in the banner", async () => {
    mockApi.getSchedulePause.mockResolvedValue({ paused: true, until: null });
    renderRoot();
    await waitFor(() => expect(screen.getByText(/until you resume them/)).toBeTruthy());
  });

  it("an expired until (GET normalized to paused:false) renders the running state, not a banner", async () => {
    // The server normalizes an expired `until` to paused:false/until:null on GET; a mock
    // returning that shape (even with a past until echoed) must render the running state.
    const past = new Date(Date.now() - 3_600_000).toISOString();
    mockApi.getSchedulePause.mockResolvedValue({ paused: false, until: past });
    renderRoot();
    await waitFor(() => expect(screen.getByRole("button", { name: /Pause all/ })).toBeTruthy());
    expect(screen.queryByText(/All schedules paused/)).toBeNull();
  });

  it("re-reads the pause state when `until` passes on an open page, clearing the banner (auto-resume transition)", async () => {
    // Real timers on purpose: the page arms one setTimeout for the auto-resume instant and
    // re-fetches then. A short until makes that fire within the test; the second read is
    // the server's normalized "not paused" answer, so the banner must give way to the button.
    const soon = new Date(Date.now() + 60).toISOString();
    mockApi.getSchedulePause
      .mockResolvedValueOnce({ paused: true, until: soon })
      .mockResolvedValue({ paused: false, until: null });
    renderRoot();
    await waitFor(() => expect(screen.getByText(/All schedules paused until/)).toBeTruthy());
    await waitFor(() => expect(screen.queryByText(/All schedules paused/)).toBeNull());
    expect(screen.getByRole("button", { name: /Pause all/ })).toBeTruthy();
    // The re-read happened (initial load + the timer's refresh), not a local guess.
    expect(mockApi.getSchedulePause.mock.calls.length).toBeGreaterThanOrEqual(2);
  });

  it("keeps the last known paused state when the expiry re-read fails (never guesses 'not paused')", async () => {
    const soon = new Date(Date.now() + 60).toISOString();
    mockApi.getSchedulePause
      .mockResolvedValueOnce({ paused: true, until: soon })
      .mockRejectedValue(new Error("network down"));
    renderRoot();
    await waitFor(() => expect(screen.getByText(/All schedules paused until/)).toBeTruthy());
    // The timer fired and the re-read was attempted (and rejected).
    await waitFor(() => expect(mockApi.getSchedulePause.mock.calls.length).toBeGreaterThanOrEqual(2));
    // Positive control for the negative below: the page is still rendered.
    expect(screen.getByRole("tab", { name: /^Schedules/ })).toBeTruthy();
    // The banner stays and the running control does not appear on an unknown state.
    expect(screen.getByText(/All schedules paused until/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: /^Pause all$/ })).toBeNull();
  });

  it("the picker opens with 'Until tomorrow 09:00' as the default preset and correct resolved stamps (fixed clock)", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    // A fixed instant so the resolved preset stamps are deterministic.
    const now = new Date("2026-09-02T12:00:00Z");
    vi.setSystemTime(now);
    try {
      mockApi.getSchedulePause.mockResolvedValue({ paused: false, until: null });
      renderRoot();
      await waitFor(() => expect(screen.getByRole("button", { name: /Pause all/ })).toBeTruthy());
      fireEvent.click(screen.getByRole("button", { name: /Pause all/ }));

      // The default preset is selected.
      const tomorrow = screen.getByRole("radio", { name: /Until tomorrow 09:00/ }) as HTMLInputElement;
      expect(tomorrow.checked).toBe(true);
      // And its resolved stamp is shown (computed by the same pure helper).
      const tomorrowStamp = formatStamp(resolvePreset("tomorrow", now)!.toISOString());
      expect(screen.getAllByText(tomorrowStamp).length).toBeGreaterThan(0);
      // The Monday preset resolves to the next Monday 09:00 strictly after now.
      const mondayStamp = formatStamp(resolvePreset("monday", now)!.toISOString());
      expect(screen.getAllByText(mondayStamp).length).toBeGreaterThan(0);
    } finally {
      vi.useRealTimers();
    }
  });

  it("submits the resolved until for each preset (and null for 'Until I resume')", async () => {
    vi.useFakeTimers({ toFake: ["Date"] });
    const now = new Date("2026-09-02T12:00:00Z");
    vi.setSystemTime(now);
    try {
      const cases: { radio: RegExp; expected: string | null }[] = [
        { radio: /Until tomorrow 09:00/, expected: resolvePreset("tomorrow", now)!.toISOString() },
        { radio: /For 24 hours/, expected: resolvePreset("24h", now)!.toISOString() },
        { radio: /Until Monday 09:00/, expected: resolvePreset("monday", now)!.toISOString() },
        { radio: /Until I resume/, expected: null },
      ];
      for (const c of cases) {
        mockApi.getSchedulePause.mockResolvedValue({ paused: false, until: null });
        mockApi.putSchedulePause.mockResolvedValue({ paused: true, until: c.expected });
        renderRoot();
        await waitFor(() => expect(screen.getByRole("button", { name: /Pause all/ })).toBeTruthy());
        fireEvent.click(screen.getByRole("button", { name: /Pause all/ }));
        fireEvent.click(screen.getByRole("radio", { name: c.radio }));
        fireEvent.click(screen.getByRole("button", { name: /^Pause all/ }));
        await waitFor(() => expect(mockApi.putSchedulePause).toHaveBeenCalledWith(c.expected));
        mockApi.putSchedulePause.mockClear();
        cleanup();
      }
    } finally {
      vi.useRealTimers();
    }
  });

  it("a new pause applied after a failed re-read gets an immediate expiry refresh (retry backoff resets)", async () => {
    // Failure streak: the first expiry re-read rejects (attempt = 1). Then the user sets
    // a new pause via Change… → Until I resume; the (mocked) server answers with an
    // until that has ALREADY passed, so the effect's next delay is decided by the
    // counter: 0 when reset (this test), 60s when the stale count leaks through.
    const soon = new Date(Date.now() + 60).toISOString();
    mockApi.getSchedulePause
      .mockResolvedValueOnce({ paused: true, until: soon })
      .mockRejectedValueOnce(new Error("network down"))
      .mockResolvedValue({ paused: false, until: null });
    mockApi.putSchedulePause.mockImplementation(async () => ({
      paused: true,
      until: new Date(Date.now() - 1).toISOString(),
    }));
    renderRoot();
    await waitFor(() => expect(screen.getByRole("button", { name: "Change…" })).toBeTruthy());
    await waitFor(() => expect(mockApi.getSchedulePause).toHaveBeenCalledTimes(2));
    fireEvent.click(screen.getByRole("button", { name: "Change…" }));
    fireEvent.click(await screen.findByRole("radio", { name: /Until I resume/ }));
    fireEvent.click(screen.getByRole("button", { name: /^Pause all indefinitely$/ }));
    await waitFor(() => expect(mockApi.putSchedulePause).toHaveBeenCalledTimes(1));
    // The fresh state's expiry refresh fires at once (not after a 60s backoff), and the
    // server's normalized answer clears the banner.
    await waitFor(() => expect(mockApi.getSchedulePause).toHaveBeenCalledTimes(3));
    await waitFor(() => expect(screen.getByRole("button", { name: /^Pause all$/ })).toBeTruthy());
  });

  it("a stale expiry re-read never overwrites a newer Resume response (mutation wins)", async () => {
    // Controlled ordering: the expiry GET is left pending, the user resumes meanwhile
    // (DELETE resolves not-paused), and only THEN the stale GET resolves "still paused".
    // The mutation's answer must stand; the effect cancels the in-flight read on change.
    const soon = new Date(Date.now() + 60).toISOString();
    let resolveStale: (v: { paused: boolean; until: string | null }) => void = () => {};
    mockApi.getSchedulePause
      .mockResolvedValueOnce({ paused: true, until: soon })
      .mockReturnValueOnce(
        new Promise<{ paused: boolean; until: string | null }>((r) => {
          resolveStale = r;
        }),
      );
    mockApi.deleteSchedulePause.mockResolvedValue({ paused: false, until: null });
    renderRoot();
    await waitFor(() => expect(screen.getByRole("button", { name: "Resume now" })).toBeTruthy());
    // The timer fired: the expiry re-read is in flight (pending).
    await waitFor(() => expect(mockApi.getSchedulePause).toHaveBeenCalledTimes(2));
    fireEvent.click(screen.getByRole("button", { name: "Resume now" }));
    await waitFor(() => expect(screen.getByRole("button", { name: /^Pause all$/ })).toBeTruthy());
    // Now the stale read lands, claiming the pause is still on.
    await act(async () => {
      resolveStale({ paused: true, until: soon });
    });
    // The newer mutation result stands: still running, no banner.
    expect(screen.getByRole("button", { name: /^Pause all$/ })).toBeTruthy();
    expect(screen.queryByText(/All schedules paused/)).toBeNull();
  });

  it("a stale expiry re-read resolving in the SAME batch as the Resume response is dropped (revision guard)", async () => {
    // Both continuations land in one React batch: the effect's cancellation flag has not
    // flipped yet (it does at commit), so only the mutation-start revision bump can keep
    // the stale read from landing last and winning.
    const soon = new Date(Date.now() + 60).toISOString();
    let resolveStale: (v: { paused: boolean; until: string | null }) => void = () => {};
    let resolveDelete: (v: { paused: boolean; until: string | null }) => void = () => {};
    mockApi.getSchedulePause
      .mockResolvedValueOnce({ paused: true, until: soon })
      .mockReturnValueOnce(
        new Promise<{ paused: boolean; until: string | null }>((r) => {
          resolveStale = r;
        }),
      );
    mockApi.deleteSchedulePause.mockReturnValue(
      new Promise<{ paused: boolean; until: string | null }>((r) => {
        resolveDelete = r;
      }),
    );
    renderRoot();
    await waitFor(() => expect(screen.getByRole("button", { name: "Resume now" })).toBeTruthy());
    await waitFor(() => expect(mockApi.getSchedulePause).toHaveBeenCalledTimes(2));
    fireEvent.click(screen.getByRole("button", { name: "Resume now" }));
    await waitFor(() => expect(mockApi.deleteSchedulePause).toHaveBeenCalledTimes(1));
    // Resolve the mutation FIRST and the stale read SECOND inside one act: without the
    // revision guard the stale "still paused" answer is the last setPause of the batch.
    await act(async () => {
      resolveDelete({ paused: false, until: null });
      resolveStale({ paused: true, until: soon });
    });
    expect(screen.getByRole("button", { name: /^Pause all$/ })).toBeTruthy();
    expect(screen.queryByText(/All schedules paused/)).toBeNull();
  });

  it("a failed Resume re-reads the pause state, so an expiry read discarded by the mutation is replaced (auto-resume still lands)", async () => {
    // The expiry re-read is in flight (pending) when the user resumes; the mutation bump
    // discards it, then the DELETE fails. Without a fresh read nothing would ever clear
    // the banner even though the server has auto-resumed; the catch path must re-read.
    const soon = new Date(Date.now() + 60).toISOString();
    mockApi.getSchedulePause
      .mockResolvedValueOnce({ paused: true, until: soon })
      .mockReturnValueOnce(new Promise<{ paused: boolean; until: string | null }>(() => {}))
      .mockResolvedValue({ paused: false, until: null });
    mockApi.deleteSchedulePause.mockRejectedValue(new Error("network down"));
    renderRoot();
    await waitFor(() => expect(screen.getByRole("button", { name: "Resume now" })).toBeTruthy());
    await waitFor(() => expect(mockApi.getSchedulePause).toHaveBeenCalledTimes(2));
    fireEvent.click(screen.getByRole("button", { name: "Resume now" }));
    await waitFor(() => expect(screen.getByText(/Could not resume your schedules/)).toBeTruthy());
    // The failure triggered a fresh read (third call), whose normalized answer clears the banner.
    await waitFor(() => expect(mockApi.getSchedulePause).toHaveBeenCalledTimes(3));
    await waitFor(() => expect(screen.getByRole("button", { name: /^Pause all$/ })).toBeTruthy());
    expect(screen.queryByText(/All schedules paused/)).toBeNull();
  });

  it("Resume now triggers deleteSchedulePause", async () => {
    const future = new Date(Date.now() + 6 * 3_600_000).toISOString();
    mockApi.getSchedulePause.mockResolvedValue({ paused: true, until: future });
    mockApi.deleteSchedulePause.mockResolvedValue({ paused: false, until: null });
    renderRoot();
    await waitFor(() => expect(screen.getByRole("button", { name: "Resume now" })).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Resume now" }));
    await waitFor(() => expect(mockApi.deleteSchedulePause).toHaveBeenCalledTimes(1));
    // After resume the running-state button returns.
    await waitFor(() => expect(screen.getByRole("button", { name: /^Pause all$/ })).toBeTruthy());
  });

  it("a row with a next fire shows a warn 'paused until <stamp>' line in place of its relative 'in …' line while paused", async () => {
    const future = new Date(Date.now() + 6 * 3_600_000).toISOString();
    const nextFire = new Date(Date.now() + 3_600_000).toISOString();
    mockApi.getSchedulePause.mockResolvedValue({ paused: true, until: future });
    mockApi.listSchedules.mockResolvedValue([sched({ id: "s1", target: "sweep", enabled: true, next_fire_at: nextFire })]);
    render(
      <MemoryRouter>
        <Schedules />
      </MemoryRouter>,
    );
    await waitFor(() => expect(screen.getByText("Sweep eligible issues")).toBeTruthy());
    // The row's Next-run cell carries the paused note (the banner uses different copy:
    // "All schedules paused until …"), so match the cell's "paused until <stamp>" form.
    const note = screen.getByText(`paused until ${formatStamp(future)}`);
    const cell = note.closest("td")!;
    expect(within(cell).getByText(formatStamp(nextFire), { exact: false })).toBeTruthy();
    // The relative line is replaced, not joined: no "in …" anywhere in the cell.
    expect(within(cell).queryByText(relativeFromNow(nextFire))).toBeNull();
    expect(cell.textContent).not.toMatch(/\bin \d/);
  });

  // D4: pause-all / resume-all never write a per-row `enabled`, so resuming restores the
  // exact prior set. This is observable only against the mock's in-memory state, so drive
  // schedulesApi directly (bypassing the api mock above).
  it("resume restores the exact prior per-row enabled set (D4, mock in-memory state)", async () => {
    await schedulesApi.deleteSchedulePause();
    const snapshot = async () =>
      (await schedulesApi.listSchedules()).map((s) => [s.id, s.enabled] as const);
    const before = await snapshot();
    // The seed set has both enabled and disabled rows, so the equality below is discriminating.
    expect(before.some(([, e]) => e === true)).toBe(true);
    expect(before.some(([, e]) => e === false)).toBe(true);

    const future = new Date(Date.now() + 3_600_000).toISOString();
    expect((await schedulesApi.putSchedulePause(future)).paused).toBe(true);
    // Pausing touched no per-row enabled flag.
    expect(await snapshot()).toEqual(before);

    expect((await schedulesApi.deleteSchedulePause()).paused).toBe(false);
    // And resuming restores exactly the prior set.
    expect(await snapshot()).toEqual(before);
  });
});

// PRD #1247 (CodeRabbit finding [O4]). Production's validateScheduleConfig runs with
// allowSelfImprove = (cur.target === "self_improve"), so a PATCH converting a
// non-self_improve schedule TO self_improve is rejected 400 BEFORE any credential-override
// handling. The mock must reproduce that contract. Driven against schedulesApi directly,
// like the D4 case above, because it is a mock-behaviour assertion.
describe("Schedules — the mock rejects a self_improve target conversion (PRD #1247 O4)", () => {
  it("400s a non-self_improve → self_improve PATCH with the production message", async () => {
    const all = await schedulesApi.listSchedules();
    // A USER-origin, non-self_improve row: a default row would trip its own catalog-lock
    // 400 first (a different message), so pick a user schedule to exercise THIS path.
    const victim = all.find((s) => s.origin === "user" && s.target !== "self_improve");
    expect(victim).toBeTruthy();
    await expect(schedulesApi.updateSchedule(victim!.id, { target: "self_improve" })).rejects.toMatchObject({
      status: 400,
      message: "target must be one of: issue, sweep, prompt",
    });
  });
});

// ── PRD #1645 M3: the Job catalog tab (D7) wired into the page ────────────────
describe("Schedules — Job catalog (PRD #1645 D7)", () => {
  // A prompt entry: no label check, so Enable is offered as soon as a repo is checked.
  const DOCS = {
    ...CATALOG.entries[0],
    slug: "docs-hygiene",
    name: "Docs hygiene",
    target: "prompt" as const,
    labels: [],
    max_issues: 0,
  };
  const docsRow = (over: Partial<Schedule> = {}) =>
    sched({ id: "dh1", origin: "default", catalog_slug: "docs-hygiene", target: "prompt", ...over });

  beforeEach(() => {
    mockApi.listScheduleCatalog.mockResolvedValue({ entries: [CATALOG.entries[0], DOCS], enablements: [] });
    mockApi.listRepos.mockResolvedValue(REPOS3);
  });

  const catalogCard = (slug: string) => document.getElementById(`catalog-entry-${slug}`)!;

  it("the status link switches to Schedules with the Job filter applied and its chip shown", async () => {
    mockApi.listSchedules.mockResolvedValue([
      customizedDefault({ id: "d1" }),
      customizedDefault({ id: "d2", repo_id: "repo-atlas", repo_path: "vtmocanu/atlas", enabled: false }),
      docsRow(),
      sched({ id: "u1", target: "prompt", prompt: "mine" }),
    ]);
    renderAt("/schedules");
    await waitFor(() => expect(nameIds()).toHaveLength(4));
    fireEvent.click(chip(/^Mine/)); // a filter the link replaces
    fireEvent.click(tabNamed(/^Job catalog/));

    const link = within(catalogCard("bug-triage")).getByRole("button", { name: "Enabled on 2 repos, 1 paused" });
    fireEvent.click(link);
    expect(tabNamed(/^Schedules/).getAttribute("aria-selected")).toBe("true");
    expect(screen.getByTestId("loc").textContent).toBe("?tab=schedules");
    expect(screen.getByText("Job: Bug triage sweep")).toBeTruthy();
    expect(chip(/^All/).getAttribute("aria-pressed")).toBe("true");
    expect(nameIds().sort()).toEqual(["d1", "d2"]);
  });

  it("a full success stays on the catalog, closes the dialog, and its notice links to the filtered list", async () => {
    mockApi.listSchedules.mockResolvedValueOnce([]).mockResolvedValue([docsRow({ repo_id: "repo-atlas", repo_path: "vtmocanu/atlas" })]);
    mockApi.enableCatalogSchedule.mockResolvedValue(docsRow());
    renderAt("/schedules");
    await waitFor(() => expect(tabNamed(/^Job catalog/).getAttribute("aria-selected")).toBe("true"));

    fireEvent.click(screen.getByRole("button", { name: "Enable Docs hygiene on…" }));
    fireEvent.click(screen.getByRole("checkbox", { name: /vtmocanu\/atlas/ }));
    fireEvent.click(screen.getByRole("button", { name: "Enable 1" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(mockApi.enableCatalogSchedule).toHaveBeenCalledWith("repo-atlas", "docs-hygiene", expect.any(String));
    // No tab jump (D12); the refreshed card now links to where it runs.
    expect(tabNamed(/^Job catalog/).getAttribute("aria-selected")).toBe("true");
    expect(within(catalogCard("docs-hygiene")).getByRole("button", { name: "Enabled on 1 repo" })).toBeTruthy();
    expect(screen.getByText(/Enabled “Docs hygiene” on 1 repo/)).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "Show it on the Schedules tab" }));
    expect(tabNamed(/^Schedules/).getAttribute("aria-selected")).toBe("true");
    expect(screen.getByText("Job: Docs hygiene")).toBeTruthy();
    expect(nameIds()).toEqual(["dh1"]);
  });

  it("a partial failure keeps the dialog open, locks the succeeded repo from the refreshed list, shows the error inline and retries only the failed repo", async () => {
    const { ApiError } = await vi.importActual<typeof import("../lib/api")>("../lib/api");
    const atlasRow = docsRow({ id: "dh-a", repo_id: "repo-atlas", repo_path: "vtmocanu/atlas" });
    const newRow = docsRow({ id: "dh-n", repo_id: "repo-new", repo_path: "vtmocanu/newrepo" });
    let retrying = false;
    mockApi.listSchedules
      .mockResolvedValueOnce([])
      .mockResolvedValueOnce([atlasRow])
      .mockResolvedValue([atlasRow, newRow]);
    mockApi.enableCatalogSchedule.mockImplementation(async (repoId: string) => {
      if (repoId === "repo-new" && !retrying) {
        throw new ApiError(502, "the forge timed out");
      }
      return docsRow({ repo_id: repoId });
    });
    renderAt("/schedules");
    await waitFor(() => expect(tabNamed(/^Job catalog/).getAttribute("aria-selected")).toBe("true"));

    fireEvent.click(screen.getByRole("button", { name: "Enable Docs hygiene on…" }));
    const dialog = screen.getByRole("dialog", { name: "Enable Docs hygiene on" });
    fireEvent.click(within(dialog).getByRole("checkbox", { name: /vtmocanu\/atlas/ }));
    fireEvent.click(within(dialog).getByRole("checkbox", { name: /vtmocanu\/newrepo/ }));
    fireEvent.click(within(dialog).getByRole("button", { name: "Enable 2" }));

    // The page keeps its error text; the dialog stays, with the failure inline.
    await waitFor(() => expect(screen.getByText("Enabled “Docs hygiene” on 1 of 2 repos; 1 failed.")).toBeTruthy());
    expect(screen.getByRole("dialog")).toBe(dialog);
    expect(within(dialog).getByText("the forge timed out")).toBeTruthy();
    const atlas = within(dialog).getByRole("checkbox", { name: /vtmocanu\/atlas/ }) as HTMLInputElement;
    expect(atlas.checked && atlas.disabled).toBe(true);
    expect(atlas.closest("label")!.textContent).toContain("enabled");
    const fresh = within(dialog).getByRole("checkbox", { name: /vtmocanu\/newrepo/ }) as HTMLInputElement;
    expect(fresh.checked).toBe(true);
    expect(fresh.disabled).toBe(false);

    mockApi.enableCatalogSchedule.mockClear();
    retrying = true;
    fireEvent.click(within(dialog).getByRole("button", { name: "Enable 1" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    expect(mockApi.enableCatalogSchedule.mock.calls.map((c) => c[0])).toEqual(["repo-new"]);
  });

  it("the not-enabled pill counts entries enabled on no repo (D1) and tracks an enable", async () => {
    mockApi.listSchedules.mockResolvedValueOnce([customizedDefault()]).mockResolvedValue([customizedDefault(), docsRow()]);
    mockApi.enableCatalogSchedule.mockResolvedValue(docsRow());
    renderAt("/schedules?tab=catalog");
    await waitFor(() => expect(tabNamed(/^Job catalog · 2/).textContent).toContain("1 not enabled"));
    fireEvent.click(screen.getByRole("button", { name: "Enable Docs hygiene on…" }));
    fireEvent.click(screen.getByRole("checkbox", { name: /vtmocanu\/atlas/ }));
    fireEvent.click(screen.getByRole("button", { name: "Enable 1" }));
    await waitFor(() => expect(tabNamed(/^Job catalog · 2$/)).toBeTruthy());
  });
});

// ── PRD #1645 M2 review notes, fixed in M3 ────────────────────────────────────
describe("Schedules — M2 review notes", () => {
  it("a Repo filter is pruned when its repo's last row is removed, instead of staying active invisibly", async () => {
    const atlas = sched({ id: "a1", target: "prompt", prompt: "atlas job", repo_id: "repo-atlas", repo_path: "vtmocanu/atlas" });
    const rest = [sched({ id: "s1" }), sched({ id: "s2", target: "prompt", prompt: "x" })];
    mockApi.listSchedules.mockResolvedValueOnce([...rest, atlas]).mockResolvedValue(rest);
    mockApi.deleteSchedule.mockResolvedValue(null);
    renderPage();
    fireEvent.change(await screen.findByRole("combobox", { name: "Repo" }), { target: { value: "repo-atlas" } });
    expect(nameIds()).toEqual(["a1"]);

    fireEvent.click(screen.getByRole("button", { name: "More actions for Prompt: atlas job on vtmocanu/atlas" }));
    fireEvent.click(screen.getByRole("menuitem", { name: "Remove" }));
    // The filter is gone with its repo: every remaining row shows (not "No schedules match").
    await waitFor(() => expect(nameIds().sort()).toEqual(["s1", "s2"]));
    expect(screen.queryByText("No schedules match")).toBeNull();
    expect(screen.queryByRole("combobox", { name: "Repo" })).toBeNull();
  });

  it("a Job filter whose rows are gone keeps its visible chip while the catalog still carries the entry", async () => {
    mockApi.listScheduleCatalog.mockResolvedValue(CATALOG);
    mockApi.listSchedules.mockResolvedValueOnce([customizedDefault(), sched({ id: "u1" })]).mockResolvedValue([sched({ id: "u1" })]);
    mockApi.deleteSchedule.mockResolvedValue(null);
    renderAt("/schedules?tab=catalog");
    fireEvent.click(await screen.findByRole("button", { name: "Enabled on 1 repo" }));
    expect(nameIds()).toEqual(["d1"]);

    fireEvent.click(screen.getByRole("button", { name: "More actions for Bug triage sweep on vtmocanu/uzi" }));
    fireEvent.click(screen.getByRole("menuitem", { name: "Remove" }));
    await waitFor(() => expect(screen.getByText("No schedules match")).toBeTruthy());
    // Not invisible: the chip that explains the empty list stays, removable.
    expect(screen.getByText("Job: Bug triage sweep")).toBeTruthy();
  });

  it("a reveal whose awaited reload FAILED is dropped, so a later successful load does not steal focus", async () => {
    mockApi.listRepos.mockResolvedValue(REPOS3);
    const g1 = sched({ id: "g1", target: "prompt", prompt: "grouped job", repo_id: "repo-uzi", repo_path: "vtmocanu/uzi", sibling_group_id: "grp-1" });
    const g2 = sched({ id: "g2", target: "prompt", prompt: "other job", repo_id: "repo-atlas", repo_path: "vtmocanu/atlas" });
    const added = sched({ id: "g3", target: "prompt", prompt: "grouped job", repo_id: "repo-new", repo_path: "vtmocanu/newrepo", sibling_group_id: "grp-1" });
    mockApi.listSchedules
      .mockResolvedValueOnce([g1, g2])
      .mockRejectedValueOnce(new Error("network down")) // the reload the reveal awaits
      .mockResolvedValue([g1, g2, added]);
    mockApi.addScheduleRepo.mockResolvedValue(added);
    mockApi.updateSchedule.mockImplementation(async (id: string, input) => ({ ...g2, id, enabled: input.enabled ?? true }));
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "More actions for Prompt: grouped job on vtmocanu/uzi" }));
    fireEvent.click(screen.getByRole("menuitem", { name: "Add to another repo" }));
    fireEvent.change(await screen.findByRole("combobox", { name: /Add Prompt: grouped job on another repo/ }), {
      target: { value: "repo-new" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Add" }));
    await waitFor(() => expect(screen.getByText("Could not load schedules")).toBeTruthy());

    // Some later action reloads successfully and brings the new row: it must not be focused.
    const sw = screen.getByRole("switch", { name: "Pause Prompt: other job on vtmocanu/atlas" });
    sw.focus();
    fireEvent.click(sw);
    await waitFor(() => expect(document.getElementById("schedule-name-g3")).not.toBeNull());
    await act(async () => {});
    expect(document.activeElement).not.toBe(document.getElementById("schedule-name-g3"));
  });
});
