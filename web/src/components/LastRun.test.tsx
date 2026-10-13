// @vitest-environment jsdom
//
// LastRun (PRD #308 / PRD #1093 M4): the "Last fire" detail panel. A pause-all fire
// (its only skip carries the schedules_paused reason) renders ONE explanatory row instead
// of a per-candidate list; an ordinary skip fire still renders the candidate list, so the
// pause branch is discriminating in both directions. Issue #1543 adds the
// ineligible_matched note and the generic (no raise-the-cap) cap-hint copy.
import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { LastFireDetail, LastRunOutcome } from "./LastRun";
import { api, ApiError, type LastFire, type LastFireSkip, type Schedule } from "../lib/api";
import { mockRuns } from "../mocks/data/runs";
vi.mock("../lib/api", async (original) => {
  const actual = await original<typeof import("../lib/api")>();
  return { ...actual, api: { ...actual.api, getRun: vi.fn().mockRejectedValue(new actual.ApiError(404, "Not found")) } };
});

// LastFireDetail reads useAuth().uziLabel for the cap hint; a minimal stub keeps it out of
// an AuthProvider.
vi.mock("../auth/AuthContext", () => ({ useAuth: () => ({ uziLabel: "uzi" }) }));

afterEach(() => { cleanup(); vi.mocked(api.getRun).mockReset().mockRejectedValue(new ApiError(404, "Not found")); });

function startedFire(id: string): LastFire {
  return { ...fire([]), matched: 1, started: [{ run_id: id, issue_iid: 1, title: "Snapshot title",
    web_url: null, selector_label: "scheduled", label_removed: true, label_remove_failed: false }] };
}
function pendingRun() {
  let resolve!: (value: Awaited<ReturnType<typeof api.getRun>>) => void;
  const promise = new Promise<Awaited<ReturnType<typeof api.getRun>>>((done) => { resolve = done; });
  return { promise, resolve };
}
it("LastRun reads only displayed started runs and preserves immutable fire snapshot", async () => {
  const snapshot = startedFire("A");
  const saved = JSON.stringify(snapshot);
  vi.mocked(api.getRun).mockResolvedValueOnce({ run: { ...mockRuns[0], id: "A", status: "paused", hold_reason: "provider_unavailable" } });
  const collapsed = render(<MemoryRouter><LastRunOutcome fire={snapshot} expanded={false} onToggle={() => {}} panelId="panel" /></MemoryRouter>);
  expect(api.getRun).not.toHaveBeenCalled();
  collapsed.unmount();
  renderDetail(snapshot);
  await act(async () => {});
  expect(api.getRun).toHaveBeenCalledExactlyOnceWith("A");
  expect(screen.getByText("Current: provider unavailable: owner hold")).toBeTruthy();
  expect(screen.getByRole("link", { name: "View owner actions" }).getAttribute("href")).toBe("/runs/A");
  expect(screen.getByText("Snapshot title")).toBeTruthy();
  expect(JSON.stringify(snapshot)).toBe(saved);
});
it.each([404, 403])("LastRun current read refusal %s stays unknown without retries", async (status) => {
  vi.mocked(api.getRun).mockRejectedValueOnce(new ApiError(status, "Refused"));
  renderDetail(startedFire("A"));
  expect(screen.getByText("Loading current state…")).toBeTruthy();
  await act(async () => {});
  expect(screen.getByText("Current state unavailable")).toBeTruthy();
  expect(api.getRun).toHaveBeenCalledOnce();
});
it("LastRun stale guard ignores replaced expansion responses and new run loading identity", async () => {
  const old = pendingRun();
  const newer = pendingRun();
  vi.mocked(api.getRun).mockReturnValueOnce(old.promise).mockReturnValueOnce(newer.promise);
  const view = renderDetail(startedFire("A"));
  view.rerender(<MemoryRouter><LastFireDetail s={sched()} fire={startedFire("B")} /></MemoryRouter>);
  await act(async () => newer.resolve({ run: { ...mockRuns[0], id: "B", status: "running" } }));
  expect(screen.getByText("Current: running")).toBeTruthy();
  await act(async () => old.resolve({ run: { ...mockRuns[0], id: "A", status: "paused", hold_reason: "provider_unavailable" } }));
  expect(screen.getByText("Current: running")).toBeTruthy();
  expect(screen.queryByRole("link", { name: "View owner actions" })).toBeNull();
});
it("LastRun ignores response identity mismatch and retires read on unmount", async () => {
  const pending = pendingRun();
  vi.mocked(api.getRun).mockReturnValueOnce(pending.promise);
  const view = renderDetail(startedFire("A"));
  await act(async () => pending.resolve({ run: { ...mockRuns[0], id: "B", status: "paused" } }));
  expect(screen.getByText("Loading current state…")).toBeTruthy();
  view.unmount();
  expect(api.getRun).toHaveBeenCalledOnce();
});

it("LastRun caps a 1000-entry expanded fire at four total and in-flight reads while preserving snapshots and links", async () => {
  const snapshot = startedFire("entry-0");
  snapshot.started = Array.from({ length: 1000 }, (_, index) => ({
    ...snapshot.started[0], run_id: `entry-${index}`, title: `Snapshot ${index}`,
  }));
  const saved = JSON.stringify(snapshot);
  const requests = Array.from({ length: 4 }, pendingRun);
  let inFlight = 0;
  let peak = 0;
  vi.mocked(api.getRun).mockImplementation((id) => {
    inFlight++;
    peak = Math.max(peak, inFlight);
    const index = Number(id.slice("entry-".length));
    return (requests[index]?.promise ?? new Promise<Awaited<ReturnType<typeof api.getRun>>>(() => {}))
      .finally(() => { inFlight--; });
  });
  const collapsed = render(<MemoryRouter><LastRunOutcome fire={snapshot} expanded={false} onToggle={() => {}} panelId="panel" /></MemoryRouter>);
  expect(api.getRun).not.toHaveBeenCalled();
  collapsed.unmount();
  const view = renderDetail(snapshot);
  expect(vi.mocked(api.getRun).mock.calls.map(([id]) => id)).toEqual(["entry-0", "entry-1", "entry-2", "entry-3"]);
  expect(inFlight).toBe(4);
  expect(peak).toBe(4);
  expect(screen.getAllByText("Open run for current state")).toHaveLength(996);
  expect(screen.getByText("Current state is checked for only the first four started entries in each expanded fire.")).toBeTruthy();
  for (const index of [4, 999]) {
    const title = screen.getByText(`Snapshot ${index}`);
    expect(title.closest(".rounded-lg")?.querySelector("a[href='/runs/entry-" + index + "']")).toBeTruthy();
    expect(title.closest(".rounded-lg")?.textContent).toContain("scheduled removed");
  }
  await act(async () => {
    requests.forEach((request, index) => request.resolve({ run: { ...mockRuns[0], id: `entry-${index}`, status: "running" } }));
  });
  expect(inFlight).toBe(0);
  expect(api.getRun).toHaveBeenCalledTimes(4);
  view.rerender(<MemoryRouter><LastFireDetail s={sched()} fire={snapshot} /></MemoryRouter>);
  await act(async () => {});
  expect(api.getRun).toHaveBeenCalledTimes(4);
  expect(JSON.stringify(snapshot)).toBe(saved);
});

it("LastRun caps multiple expanded fires independently at four reads each", async () => {
  const snapshots = ["A", "B"].map((prefix) => {
    const snapshot = startedFire(prefix);
    snapshot.started = Array.from({ length: 6 }, (_, index) => ({
      ...snapshot.started[0], run_id: `${prefix}-${index}`,
    }));
    return snapshot;
  });
  vi.mocked(api.getRun).mockImplementation(() => new Promise(() => {}));
  render(<MemoryRouter>{snapshots.map((snapshot, index) =>
    <LastFireDetail key={index} s={sched()} fire={snapshot} />)}</MemoryRouter>);
  expect(vi.mocked(api.getRun).mock.calls.map(([id]) => id)).toEqual(["A-0", "A-1", "A-2", "A-3", "B-0", "B-1", "B-2", "B-3"]);
  expect(screen.getAllByText("Current state is checked for only the first four started entries in each expanded fire.")).toHaveLength(2);
  expect(screen.getAllByText("Open run for current state")).toHaveLength(4);
});

it("LastRun bounds reads to displayed started siblings and never polls", async () => {
  vi.useFakeTimers();
  try {
    const snapshot = startedFire("A");
    snapshot.started.push({ ...snapshot.started[0], run_id: "B" });
    snapshot.skips.push({ issue_iid: 9, title: "Skipped", reason: "already_running", web_url: null });
    vi.mocked(api.getRun).mockRejectedValueOnce(new ApiError(403, "Refused"))
      .mockResolvedValueOnce({ run: { ...mockRuns[0], id: "B", status: "recovery_wait", recovery_wait_cause: "provider_outage" } });
    const view = renderDetail(snapshot);
    await act(async () => {});
    expect(vi.mocked(api.getRun).mock.calls.map(([id]) => id)).toEqual(["A", "B"]);
    expect(screen.getByText("Current state unavailable")).toBeTruthy();
    expect(screen.getByText("Current: waiting for provider")).toBeTruthy();
    view.rerender(<MemoryRouter><LastFireDetail s={sched()} fire={snapshot} /></MemoryRouter>);
    await act(async () => { await vi.advanceTimersByTimeAsync(120000); });
    expect(api.getRun).toHaveBeenCalledTimes(2);
  } finally {
    vi.useRealTimers();
  }
});

it("LastRun collapse retires pending read and re-expansion reads once afresh", async () => {
  const old = pendingRun();
  const newer = pendingRun();
  vi.mocked(api.getRun).mockReturnValueOnce(old.promise).mockReturnValueOnce(newer.promise);
  const view = renderDetail(startedFire("A"));
  view.unmount();
  renderDetail(startedFire("A"));
  await act(async () => newer.resolve({ run: { ...mockRuns[0], id: "A", status: "running" } }));
  await act(async () => old.resolve({ run: { ...mockRuns[0], id: "A", status: "paused", hold_reason: "provider_unavailable" } }));
  expect(screen.getByText("Current: running")).toBeTruthy();
  expect(screen.queryByRole("link", { name: "View owner actions" })).toBeNull();
  expect(api.getRun).toHaveBeenCalledTimes(2);
});

function sched(over: Partial<Schedule> = {}): Schedule {
  return {
    id: "s1",
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
    remove_label_on_dispatch: false,
    capacity_limit: null,
    capacity_room_needed: null,
    max_issues: 3,
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
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    next_fires: [],
    ...over,
  };
}

function fire(skips: LastFireSkip[]): LastFire {
  return {
    fired_at: "2026-09-02T02:00:00Z",
    matched: skips.length,
    capped: false,
    started: [],
    skips,
  };
}

function renderDetail(f: LastFire) {
  return render(
    <MemoryRouter>
      <LastFireDetail s={sched()} fire={f} />
    </MemoryRouter>,
  );
}

it.each([
  ["harness_label_conflict", "issue harness label conflicts with schedule pin"],
  ["harness_label_invalid", "invalid issue harness labels"],
] as const)("shows %s harness label refusals as owner-actionable badges", (reason, label) => {
  renderDetail(fire([{ issue_iid: 7, title: "Harness choice", reason, web_url: null }]));
  expect(screen.getByText(label).className).toMatch(/warn/);
});

describe("LastFireDetail — schedules_paused fire (PRD #1093)", () => {
  it("renders the explanatory row instead of a candidate list for a pause-all fire", () => {
    renderDetail(fire([{ issue_iid: null, title: "", reason: "schedules_paused", web_url: null }]));
    // The explanatory copy is shown.
    expect(screen.getByText(/All your schedules were paused/)).toBeTruthy();
    expect(screen.getByText(/nothing replays on resume/)).toBeTruthy();
    // The per-candidate skip label ("all schedules paused") is NOT rendered as a row badge —
    // the pause fire replaces the candidate list entirely.
    expect(screen.queryByText("all schedules paused")).toBeNull();
  });

  it("still renders the per-candidate list for an ordinary (non-pause) skip fire (control)", () => {
    renderDetail(
      fire([{ issue_iid: 42, title: "some issue", reason: "not_eligible", web_url: null }]),
    );
    // The ordinary candidate row + its typed reason label render...
    expect(screen.getByText("some issue")).toBeTruthy();
    expect(screen.getByText("not eligible")).toBeTruthy();
    // ...and the pause explanatory copy does NOT appear.
    expect(screen.queryByText(/All your schedules were paused/)).toBeNull();
  });
});

// Issue #1543: a label sweep filters by eligibility before its scan window, so
// selector-only issues surface as the aggregate `ineligible_matched`, not as skip rows.
// The note is selected by its role="note" handle (not its copy), so the negative
// assertions below cannot rot into vacuity on a copy change; each negative also proves
// the panel itself rendered.
describe("LastFireDetail — ineligible_matched note (issue #1543)", () => {
  const started = {
    issue_iid: 7,
    run_id: "77777777-0000-0000-0000-000000000000",
    title: "an eligible one",
    web_url: null,
  };
  const skip: LastFireSkip = { issue_iid: 8, title: "busy one", reason: "already_running", web_url: null };

  function panelRendered() {
    // The panel's own header + tally prove it mounted (non-vacuous negatives).
    expect(screen.getByText("Last fire")).toBeTruthy();
    expect(screen.getByText("examined")).toBeTruthy();
  }

  it("renders nothing for a legacy fire that predates the field", () => {
    renderDetail({ ...fire([skip]), started: [started] });
    panelRendered();
    expect(screen.getByText("busy one")).toBeTruthy();
    expect(screen.queryByRole("note")).toBeNull();
  });

  it.each([
    ["0", 0],
    ["null", null],
  ])("renders nothing when ineligible_matched is %s", (_, n) => {
    renderDetail({ ...fire([skip]), started: [started], ineligible_matched: n });
    panelRendered();
    expect(screen.getByText("an eligible one")).toBeTruthy();
    expect(screen.queryByRole("note")).toBeNull();
  });

  it("renders the count and the uzi label alongside started runs and skips", () => {
    renderDetail({ ...fire([skip]), matched: 2, started: [started], ineligible_matched: 16 });
    const note = screen.getByRole("note");
    expect(note.textContent).toMatch(/16 open issues match the selector but aren't eligible/);
    expect(note.textContent).toMatch(/assign them to uzi/);
    expect(note.querySelector("code")?.textContent).toBe("uzi");
    // The candidate list still renders next to it.
    expect(screen.getByText("an eligible one")).toBeTruthy();
    expect(screen.getByText("busy one")).toBeTruthy();
  });

  it("renders for a zero-candidate fire (matched 0, no started, no skips)", () => {
    renderDetail({
      fired_at: "2026-09-02T02:00:00Z",
      matched: 0,
      capped: false,
      started: [],
      skips: [],
      ineligible_matched: 16,
    });
    // Issue #1727: nothing started, so the header reads amber, not the neutral "matched 0".
    expect(screen.getByText("started nothing")).toBeTruthy();
    expect(screen.queryByText("matched 0")).toBeNull();
    expect(screen.getByRole("note").textContent).toMatch(/16 open issues match the selector/);
  });

  // The genuine-zero control for the header above: a zero-candidate fire whose ineligible
  // count is unknown or zero matched nothing at all, so its header stays neutral (#1727).
  it.each([
    ["absent", undefined],
    ["null", null],
    ["0", 0],
  ])("keeps the neutral matched 0 header for a genuine zero match (ineligible %s)", (_, n) => {
    renderDetail(n === undefined ? fire([]) : { ...fire([]), ineligible_matched: n });
    panelRendered();
    const badge = screen.getByText("matched 0");
    expect(badge.className).not.toMatch(/warn/);
    expect(screen.queryByText("started nothing")).toBeNull();
    expect(screen.queryByRole("note")).toBeNull();
  });

  it("pluralizes a single ineligible issue", () => {
    renderDetail({ ...fire([]), ineligible_matched: 1 });
    const text = screen.getByRole("note").textContent ?? "";
    expect(text).toMatch(/1 open issue matches the selector but isn't eligible/);
    expect(text).toMatch(/assign it to uzi to make it runnable/);
  });
});

describe("LastFireDetail — the cap hint copy (issue #1543)", () => {
  function hintBox(): HTMLElement {
    // The lead-in is the stable handle (also used by Schedules.test.tsx).
    const lead = screen.getByText("Nothing newer was reached.");
    return lead.closest<HTMLElement>("div.rounded-lg")!;
  }

  it("is generic: no raise-the-cap advice and no specific skip reason", () => {
    renderDetail({
      ...fire([
        { issue_iid: 8, title: "busy one", reason: "already_running", web_url: null },
        { issue_iid: 9, title: "flaky one", reason: "fetch_failed", web_url: null },
      ]),
      capped: true,
    });
    const text = hintBox().textContent ?? "";
    expect(text).toMatch(/candidates ahead of the newer eligible issues were skipped/);
    expect(text).toMatch(/See the reasons above/);
    expect(text).not.toMatch(/raise the cap/i);
    expect(text).not.toMatch(/max issues/i);
    expect(text).not.toMatch(/already running|fetch failed|too large|not eligible/i);
  });

  it("pluralizes a single skipped head candidate", () => {
    renderDetail({
      ...fire([{ issue_iid: 8, title: "busy one", reason: "already_running", web_url: null }]),
      capped: true,
    });
    expect(hintBox().textContent).toMatch(/The candidate ahead of the newer eligible issues was skipped\. See the reason above\./);
  });
});

// Issue #1727 item 4. The list cell's outcome badge went started → skipped → neutral
// "matched 0" without reading ineligible_matched, so a sweep whose selector matched only
// ineligible issues looked exactly like one that matched nothing. It now shares the detail
// note's predicate: when nothing started and the count is positive, the badge is amber.
describe("LastRunOutcome — a starved sweep does not read matched 0 (#1727)", () => {
  function renderOutcome(f: LastFire) {
    return render(
      <MemoryRouter>
        <LastRunOutcome fire={f} expanded={false} onToggle={() => {}} panelId="p1" />
      </MemoryRouter>,
    );
  }
  const empty: LastFire = { fired_at: "2026-09-02T02:00:00Z", matched: 0, capped: false, started: [], skips: [] };

  it("shows an amber starvation badge when nothing started and issues are ineligible", () => {
    renderOutcome({ ...empty, ineligible_matched: 16 });
    const badge = screen.getByText("0 started · 16 not eligible");
    expect(badge.className).toMatch(/warn/);
    expect(badge.getAttribute("title")).toBe("16 open issues match the selector but aren't eligible");
    expect(screen.queryByText("matched 0")).toBeNull();
  });

  it("pluralizes a single ineligible issue in the badge title", () => {
    renderOutcome({ ...empty, ineligible_matched: 1 });
    expect(screen.getByText("0 started · 1 not eligible").getAttribute("title")).toBe(
      "1 open issue matches the selector but isn't eligible",
    );
  });

  it("shows both the skips and the ineligible count when nothing started", () => {
    renderOutcome({
      ...empty,
      matched: 1,
      skips: [{ issue_iid: 8, title: "busy one", reason: "already_running", web_url: null }],
      ineligible_matched: 3,
    });
    const badge = screen.getByText("0 started · 1 skipped · 3 not eligible");
    expect(badge.className).toMatch(/warn/);
  });

  it("does not hide an owner-actionable skip behind the ineligible count", () => {
    // no_usable_credential is an amber skip the cadence does NOT fix on its own: the owner
    // must repair a credential, so the list cell must name the skips, not only starvation.
    const noCred: LastFireSkip = { issue_iid: null, title: "", reason: "no_usable_credential", web_url: null };
    renderOutcome({
      ...empty,
      matched: 3,
      skips: [noCred, { ...noCred, issue_iid: 9 }, { ...noCred, issue_iid: 10 }],
      ineligible_matched: 16,
    });
    const badge = screen.getByText("0 started · 3 skipped · 16 not eligible");
    expect(badge.className).toMatch(/warn/);
    expect(screen.queryByText("0 started · 16 not eligible")).toBeNull();
  });

  it("keeps the skips-only form when no issue is ineligible", () => {
    renderOutcome({
      ...empty,
      matched: 1,
      skips: [{ issue_iid: 8, title: "busy one", reason: "already_running", web_url: null }],
    });
    expect(screen.getByText("0 started · 1 skipped").className).toMatch(/warn/);
  });

  it("keeps the started badge when runs started, whatever the ineligible count", () => {
    renderOutcome({
      ...empty,
      matched: 1,
      started: [{ issue_iid: 7, run_id: "77777777-0000-0000-0000-000000000000", title: "t", web_url: null }],
      ineligible_matched: 16,
    });
    expect(screen.getByText("1 started")).toBeTruthy();
    expect(screen.queryByText(/not eligible/)).toBeNull();
  });

  it.each([
    ["absent", undefined],
    ["null", null],
    ["0", 0],
  ])("keeps the neutral matched 0 for a genuine zero match (ineligible %s)", (_, n) => {
    renderOutcome(n === undefined ? empty : { ...empty, ineligible_matched: n });
    const badge = screen.getByText("matched 0");
    expect(badge.className).not.toMatch(/warn/);
    expect(screen.queryByText(/not eligible/)).toBeNull();
  });
});

describe("capacity fire presentation", () => {
  it("gives blocked capacity precedence in compact and detail views with nothing examined", () => {
    const blocked: LastFire = { ...fire([]),
      capacity: { in_flight: 3, limit: 4, room_needed: 2, room: 1, blocked: true } };
    render(<MemoryRouter><LastRunOutcome fire={blocked} expanded={false} onToggle={vi.fn()} panelId="detail" />
      <LastFireDetail s={sched()} fire={blocked} /></MemoryRouter>);
    expect(screen.getAllByText("Waiting for room")).toHaveLength(2);
    expect(screen.getByText(/3 unfinished runs, limit 4/).textContent).toContain("space for 1 more run; needs 2 · nothing examined");
    expect(screen.queryByText("matched 0")).toBeNull();
    expect(screen.queryByText("examined")).toBeNull();
  });
  it("shows passing capacity alongside the ordinary outcome", () => {
    const passing: LastFire = { ...fire([]), matched: 1,
      started: [{ issue_iid: 1001, run_id: "r1", title: "An issue" }],
      capacity: { in_flight: 2, limit: 4, room_needed: 2, room: 2, blocked: false } };
    renderDetail(passing);
    expect(screen.getByText("1 started")).toBeTruthy();
    expect(screen.getByText(/2 unfinished runs, limit 4/)).toBeTruthy();
    expect(screen.queryByText("Waiting for room")).toBeNull();
  });
});

describe("fire-time selector removal outcomes", () => {
  it.each([
    [{ label_removed: true }, "on-deck removed"],
    [{ label_remove_failed: true }, "on-deck could not be removed (the run started; remove it by hand)"],
    [{ label_removed: false, label_remove_failed: false }, null],
    [{}, null],
  ] as const)("renders snapshot outcomes %j after editing the selector and flag", (flags, message) => {
    const snapshot: LastFire = { ...fire([]), matched: 1, started: [{ issue_iid: 1001, run_id: "run-done", title: "Demo", selector_label: "on-deck", ...flags }] };
    const view = render(<MemoryRouter><LastFireDetail s={sched({ labels: ["changed"], remove_label_on_dispatch: false })} fire={snapshot} /></MemoryRouter>);
    if (message) expect(screen.getByText(message)).toBeTruthy();
    else expect(view.container.textContent).not.toMatch(/removed|remove it by hand/);
    view.unmount();
    render(<MemoryRouter><LastRunOutcome fire={snapshot} expanded={false} onToggle={vi.fn()} panelId="fire" /></MemoryRouter>);
    if (message) expect(screen.getByText(message)).toBeTruthy();
    else expect(screen.queryByText(/removed|remove it by hand/)).toBeNull();
  });
  it("uses generic label for a legacy fire without a snapshot", () => {
    render(<MemoryRouter><LastFireDetail s={sched({ labels: ["changed"] })} fire={{ ...fire([]), started: [{ issue_iid: 1, run_id: "legacy", title: "", label_removed: true }] }} /></MemoryRouter>);
    expect(screen.getByText("label removed")).toBeTruthy();
  });
});
