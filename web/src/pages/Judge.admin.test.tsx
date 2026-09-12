// @vitest-environment jsdom
//
// The admin "All users" scope switch and attribution-hidden row (PRD #1184 M4). These cover the
// behaviour the milestone pins: the switch is admin-only; the scope pref round-trips through
// `prefs` and defaults to "mine"; switching to All users re-fetches from the ADMIN endpoint and
// seeds the instance-level category default once; a ?run= anchor forces Mine and hides the
// switch; the nav badge stays the CALLER'S OWN count under All users, never the aggregate;
// occurrences render with no run link and no run title; and there is no Dismiss control under All
// users (paired with the positive on Mine). It follows Judge.test.tsx's mock-API pattern.
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { Judge } from "./Judge";
import { GroupRow } from "./judge/GroupRow";
import {
  api,
  type JudgeAdminBacklog,
  type JudgeAdminGroup,
  type JudgeAdminOccurrence,
  type JudgeBacklog,
  type RunReview,
  type TriageCounts,
} from "../lib/api";
import { deferred } from "../test-helpers";
import { judgeScope as judgeScopePref } from "../lib/prefs";
import { JudgeTodoContext } from "../components/JudgeTodoContext";
import { useAuth } from "../auth/AuthContext";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      // Owner surface (Mine / the ?run= forced view).
      getJudgeBacklog: vi.fn(),
      getJudgeCategoryStats: vi.fn(),
      getJudgeStats: vi.fn(),
      bulkSetJudgeDisposition: vi.fn(),
      deleteDisposition: vi.fn(),
      getIssueDraft: vi.fn(),
      fileIssue: vi.fn(),
      getRunReview: vi.fn(),
      listRepos: vi.fn().mockResolvedValue({ repos: [] }),
      // Admin surface (All users).
      getAdminJudgeBacklog: vi.fn(),
      getAdminJudgeCategoryStats: vi.fn(),
      adminSetJudgeDisposition: vi.fn(),
      adminUndoJudgeDisposition: vi.fn(),
      getAdminJudgeIssueDraft: vi.fn(),
      adminFileJudgeIssue: vi.fn(),
    },
  };
});
vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));

const mockApi = vi.mocked(api);

const EMPTY_MATRIX = { counts_by_bucket: { todo: {}, filed: {}, done: {}, dismissed: {}, all: {} } };
const OWNER_TRIAGE: TriageCounts = { total: 4, todo: 3, filed: 0, done: 1, dismissed: 0, false_positives: 0 };
const AGGREGATE_TRIAGE: TriageCounts = { total: 40, todo: 99, filed: 5, done: 6, dismissed: 2, false_positives: 1 };

function adminOcc(over: Partial<JudgeAdminOccurrence> = {}): JudgeAdminOccurrence {
  return { judged_at: "2026-01-01T00:00:00Z", verdict: "issues", bucket: "todo", ...over };
}

function adminGroup(over: Partial<JudgeAdminGroup> = {}): JudgeAdminGroup {
  return {
    category: "improve_uzi",
    target: "api/internal/poller",
    bucket: "todo",
    open_count: 3,
    run_count: 3,
    user_count: 3,
    rationale_preview: "Queue-to-claim latency dominated across users.",
    occurrences: [adminOcc(), adminOcc(), adminOcc()],
    ...over,
  };
}

function adminBacklog(over: Partial<JudgeAdminBacklog> = {}): JudgeAdminBacklog {
  return { bucket: "todo", groups: [adminGroup()], truncated: false, triage: AGGREGATE_TRIAGE, ...over };
}

function ownerBacklog(over: Partial<JudgeBacklog> = {}): JudgeBacklog {
  return {
    bucket: "todo",
    run: "",
    groups: [
      {
        category: "improve_uzi",
        target: "api/internal/poller",
        bucket: "todo",
        open_count: 1,
        run_count: 1,
        rationale_preview: "My own latency note.",
        occurrences: [
          { run_id: "run-1", run_title: "My run", review_id: "rev-1", rec_id: "rec-1", verdict: "issues", confidence: "", bucket: "todo" },
        ],
      },
    ],
    truncated: false,
    triage: OWNER_TRIAGE,
    ...over,
  };
}

function asAdmin() {
  vi.mocked(useAuth).mockReturnValue({
    user: { is_admin: true, judge_enabled: true, id: "me" },
  } as unknown as ReturnType<typeof useAuth>);
}
function asNonAdmin() {
  vi.mocked(useAuth).mockReturnValue({
    user: { is_admin: false, judge_enabled: true, id: "me" },
  } as unknown as ReturnType<typeof useAuth>);
}

beforeEach(() => {
  window.localStorage.clear();
  asAdmin();
  mockApi.getJudgeBacklog.mockResolvedValue(ownerBacklog());
  mockApi.getJudgeCategoryStats.mockResolvedValue(EMPTY_MATRIX);
  mockApi.getJudgeStats.mockResolvedValue(OWNER_TRIAGE);
  mockApi.getRunReview.mockResolvedValue({ review: null, pending_judge: null });
  mockApi.getAdminJudgeBacklog.mockResolvedValue(adminBacklog());
  mockApi.getAdminJudgeCategoryStats.mockResolvedValue(EMPTY_MATRIX);
});
afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

function renderJudge(entries: string[] = ["/judge"], onSetTodo?: (n: number) => void) {
  const tree = (
    <MemoryRouter initialEntries={entries}>
      <Judge />
    </MemoryRouter>
  );
  return render(
    onSetTodo ? <JudgeTodoContext.Provider value={onSetTodo}>{tree}</JudgeTodoContext.Provider> : tree,
  );
}

// Seed the scope pref to "all" so the page opens in the admin aggregate without a click.
function seedScopeAll() {
  window.localStorage.setItem("uzi.judgeScope", JSON.stringify("all"));
}

describe("Judge admin scope — the switch renders for admins only", () => {
  it("shows the Mine / All users switch for an admin", async () => {
    renderJudge();
    await waitFor(() => expect(mockApi.getJudgeBacklog).toHaveBeenCalled());
    expect(screen.getByRole("button", { name: "All users" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Mine" })).toBeTruthy();
  });

  it("shows NO switch for a non-admin, and never reads the pref", async () => {
    asNonAdmin();
    seedScopeAll(); // even with a stray pref, a non-admin must stay on the owner endpoint
    renderJudge();
    await waitFor(() => expect(mockApi.getJudgeBacklog).toHaveBeenCalled());
    expect(screen.queryByRole("button", { name: "All users" })).toBeNull();
    // The non-admin NEVER reads the admin aggregate, even with the pref set to "all".
    expect(mockApi.getAdminJudgeBacklog).not.toHaveBeenCalled();
  });
});

describe("Judge admin scope — the pref round-trips and defaults to mine", () => {
  it("defaults to mine and round-trips through prefs with a stubbed storage", () => {
    expect(judgeScopePref.get()).toBe("mine");
    judgeScopePref.set("all");
    expect(judgeScopePref.get()).toBe("all");
    expect(window.localStorage.getItem("uzi.judgeScope")).toBe(JSON.stringify("all"));
    judgeScopePref.set("mine");
    expect(judgeScopePref.get()).toBe("mine");
  });

  it("writes the pref when the admin switches scope", async () => {
    renderJudge();
    await waitFor(() => expect(mockApi.getJudgeBacklog).toHaveBeenCalled());
    fireEvent.click(screen.getByRole("button", { name: "All users" }));
    await waitFor(() => expect(judgeScopePref.get()).toBe("all"));
  });
});

describe("Judge admin scope — switching re-fetches from the admin endpoint + seeds categories", () => {
  it("switching to All users calls the ADMIN backlog endpoint", async () => {
    renderJudge();
    await waitFor(() => expect(mockApi.getJudgeBacklog).toHaveBeenCalled());
    fireEvent.click(screen.getByRole("button", { name: "All users" }));
    await waitFor(() => expect(mockApi.getAdminJudgeBacklog).toHaveBeenCalled());
  });

  it("seeds the five instance-level categories on the first switch when ?category= is absent", async () => {
    renderJudge();
    await waitFor(() => expect(mockApi.getJudgeBacklog).toHaveBeenCalled());
    fireEvent.click(screen.getByRole("button", { name: "All users" }));
    await waitFor(() =>
      expect(mockApi.getAdminJudgeBacklog).toHaveBeenCalledWith("todo", [
        "install_worker_tool",
        "adjust_template",
        "improve_agent",
        "add_agent",
        "improve_uzi",
      ]),
    );
  });

  it("does NOT overwrite an existing ?category= filter on switch", async () => {
    renderJudge(["/judge?category=improve_uzi"]);
    await waitFor(() => expect(mockApi.getJudgeBacklog).toHaveBeenCalled());
    fireEvent.click(screen.getByRole("button", { name: "All users" }));
    await waitFor(() => expect(mockApi.getAdminJudgeBacklog).toHaveBeenCalledWith("todo", ["improve_uzi"]));
    // The five-category default was never written over the admin's own filter.
    expect(mockApi.getAdminJudgeBacklog).not.toHaveBeenCalledWith("todo", [
      "install_worker_tool",
      "adjust_template",
      "improve_agent",
      "add_agent",
      "improve_uzi",
    ]);
  });
});

describe("Judge admin scope — a ?run= anchor forces mine", () => {
  it("forces the owner endpoint and hides the switch under a run anchor", async () => {
    seedScopeAll(); // even with the pref set to All users, the anchor wins
    renderJudge(["/judge?run=run-1"]);
    await waitFor(() => expect(mockApi.getJudgeBacklog).toHaveBeenCalled());
    // Owner endpoint, with the run anchor; the admin aggregate is never read.
    expect(mockApi.getJudgeBacklog).toHaveBeenCalledWith("all", "run-1", []);
    expect(mockApi.getAdminJudgeBacklog).not.toHaveBeenCalled();
    // The switch is hidden while the anchor forces Mine.
    expect(screen.queryByRole("button", { name: "All users" })).toBeNull();
  });
});

describe("Judge admin scope — the badge stays the caller's own count", () => {
  it("publishes the owner triage.todo, never the aggregate, under All users", async () => {
    const setTodo = vi.fn();
    seedScopeAll();
    renderJudge(["/judge"], setTodo);
    await waitFor(() => expect(mockApi.getAdminJudgeBacklog).toHaveBeenCalled());
    await waitFor(() => expect(mockApi.getJudgeStats).toHaveBeenCalled());
    // The caller's own count (OWNER_TRIAGE.todo === 3) reaches the badge…
    await waitFor(() => expect(setTodo).toHaveBeenCalledWith(3));
    // …and the aggregate's count (AGGREGATE_TRIAGE.todo === 99) NEVER does.
    expect(setTodo).not.toHaveBeenCalledWith(99);
  });
});

describe("Judge admin scope — attribution-hidden occurrences", () => {
  it("renders occurrences with no run link and no run title under All users", async () => {
    seedScopeAll();
    const { container } = renderJudge(["/judge"]);
    await waitFor(() => expect(mockApi.getAdminJudgeBacklog).toHaveBeenCalled());
    await waitFor(() => expect(screen.getByText("api/internal/poller")).toBeTruthy());
    // Expand the group's occurrences.
    fireEvent.click(screen.getByRole("button", { name: "Expand occurrences" }));
    // Each occurrence reads "A run, judged <time>" — no title. A group can carry several, so
    // match ALL of them (getByText throws on multiple); the load-bearing assertion is that none
    // is a link to a run.
    await waitFor(() => expect(screen.getAllByText(/A run, judged/).length).toBeGreaterThan(0));
    expect(container.querySelector('a[href^="/runs/"]')).toBeNull();
  });

  it("renders the admin 'Done by an admin' chip on a done occurrence", async () => {
    seedScopeAll();
    mockApi.getAdminJudgeBacklog.mockResolvedValue(
      adminBacklog({
        bucket: "done",
        groups: [
          adminGroup({
            bucket: "done",
            open_count: 0,
            run_count: 1,
            user_count: 1,
            occurrences: [adminOcc({ bucket: "done", set_via: "admin" })],
          }),
        ],
      }),
    );
    renderJudge(["/judge?bucket=done"]);
    await waitFor(() => expect(screen.getByText("api/internal/poller")).toBeTruthy());
    fireEvent.click(screen.getByRole("button", { name: "Expand occurrences" }));
    await waitFor(() => expect(screen.getByText("Done by an admin")).toBeTruthy());
  });
});

describe("Judge admin scope — no Dismiss under All users (paired with Mine)", () => {
  it("offers Dismiss under Mine but NOT under All users", async () => {
    // Mine: the Dismiss ▾ control is present on an open group.
    renderJudge();
    await waitFor(() => expect(screen.getByText("api/internal/poller")).toBeTruthy());
    expect(screen.getByText("Dismiss ▾")).toBeTruthy();
    cleanup();

    // All users: the same open group offers File issue + Mark done but NO Dismiss.
    seedScopeAll();
    renderJudge();
    await waitFor(() => expect(mockApi.getAdminJudgeBacklog).toHaveBeenCalled());
    await waitFor(() => expect(screen.getByText("api/internal/poller")).toBeTruthy());
    expect(screen.getByText("Mark done")).toBeTruthy();
    expect(screen.queryByText("Dismiss ▾")).toBeNull();
  });
});

describe("Judge admin scope — cross-user Mark done and Undo", () => {
  it("Mark done calls adminSetJudgeDisposition and Undo calls adminUndoJudgeDisposition", async () => {
    seedScopeAll();
    mockApi.adminSetJudgeDisposition.mockResolvedValue({
      updated: 3,
      groups: [adminGroup({ bucket: "done", open_count: 0 })],
      truncated: false,
      triage: AGGREGATE_TRIAGE,
    });
    mockApi.adminUndoJudgeDisposition.mockResolvedValue({
      updated: 0,
      groups: [adminGroup()],
      truncated: false,
      triage: AGGREGATE_TRIAGE,
    });
    renderJudge();
    await waitFor(() => expect(screen.getByText("api/internal/poller")).toBeTruthy());

    fireEvent.click(screen.getByText("Mark done"));
    await waitFor(() =>
      expect(mockApi.adminSetJudgeDisposition).toHaveBeenCalledWith([
        { category: "improve_uzi", target: "api/internal/poller" },
      ]),
    );

    // The toast offers Undo, which calls the admin coordinate-scoped DELETE.
    await waitFor(() => expect(screen.getByText("Undo")).toBeTruthy());
    fireEvent.click(screen.getByText("Undo"));
    await waitFor(() =>
      expect(mockApi.adminUndoJudgeDisposition).toHaveBeenCalledWith([
        { category: "improve_uzi", target: "api/internal/poller" },
      ]),
    );
  });
});

// Finding [7]: on a scope switch mine→all the two backlog fetches (getJudgeBacklog under `mine`,
// getAdminJudgeBacklog under `all`) can overlap. `load` stamps a monotonic generation and gates
// every state update on it, so a late OWNER response can never overwrite the current `all`
// backlog — which is what stops a cross-user dispose from sending an owner coordinate. Both
// completion orders are covered; the owner group's rationale preview ("My own latency note.") is
// the tell that an owner response leaked into the aggregate view.
describe("Judge admin scope — a superseded owner backlog response is dropped (Finding [7])", () => {
  const OWNER_PREVIEW = "My own latency note.";
  const ADMIN_PREVIEW = "Queue-to-claim latency dominated across users.";

  async function switchToAllWithBothInFlight() {
    const ownerD = deferred<JudgeBacklog>();
    const adminD = deferred<JudgeAdminBacklog>();
    mockApi.getJudgeBacklog.mockReturnValue(ownerD.promise);
    mockApi.getAdminJudgeBacklog.mockReturnValue(adminD.promise);

    renderJudge(); // admin, default pref → opens on Mine, so the owner fetch is issued first
    await waitFor(() => expect(mockApi.getJudgeBacklog).toHaveBeenCalled());

    // Switch to All users while the owner fetch is still parked → the two fetches now overlap.
    fireEvent.click(screen.getByRole("button", { name: "All users" }));
    await waitFor(() => expect(mockApi.getAdminJudgeBacklog).toHaveBeenCalled());
    return { ownerD, adminD };
  }

  it("admin (current) resolves first, then the late owner response is dropped", async () => {
    const { ownerD, adminD } = await switchToAllWithBothInFlight();

    // The current-scope aggregate lands first and renders.
    adminD.resolve(adminBacklog({ groups: [adminGroup({ rationale_preview: ADMIN_PREVIEW })] }));
    await screen.findByText(ADMIN_PREVIEW);

    // The late owner (mine) response resolves — it is superseded, so it applies NO state.
    await act(async () => {
      ownerD.resolve(ownerBacklog({ groups: [ownerGroupWithPreview(OWNER_PREVIEW)] }));
      await ownerD.promise;
    });
    expect(screen.getByText(ADMIN_PREVIEW)).toBeTruthy();
    expect(screen.queryByText(OWNER_PREVIEW)).toBeNull();
  });

  it("owner (superseded) resolves first and applies nothing, then admin wins", async () => {
    const { ownerD, adminD } = await switchToAllWithBothInFlight();

    // The superseded owner fetch resolves BEFORE the admin one; it must not paint anything.
    await act(async () => {
      ownerD.resolve(ownerBacklog({ groups: [ownerGroupWithPreview(OWNER_PREVIEW)] }));
      await ownerD.promise;
    });
    expect(screen.queryByText(OWNER_PREVIEW)).toBeNull();

    // The current aggregate then lands and is the only backlog shown.
    adminD.resolve(adminBacklog({ groups: [adminGroup({ rationale_preview: ADMIN_PREVIEW })] }));
    await screen.findByText(ADMIN_PREVIEW);
    expect(screen.queryByText(OWNER_PREVIEW)).toBeNull();
  });
});

// Finding [8]: GroupRow caches the owner scope's FULL rationale (rationaleMd/rationaleFetchedFor).
// Judge.tsx keys a row by coordKey alone, so the instance is reused across a scope switch; under
// `all` the fetch effect no-ops (no run_id to fetch with). Without a clear, the stale owner full
// text would keep rendering in the expander. The effect clears the cache on entering `all` so the
// expander shows only the anonymized clamped preview. Exercised at the GroupRow unit level so the
// reused-instance transition is observed directly (the page-level loading toggle would remount it).
describe("Judge admin scope — the owner full rationale is cleared on entering All users (Finding [8])", () => {
  it("drops the cached owner full rationale, leaving the clamped preview under All users", async () => {
    const fetchReview = vi.fn().mockResolvedValue({
      review: {
        recommendations: [
          { category: "improve_uzi", target: "api/internal/poller", rationale_md: "OWNER FULL RATIONALE DETAIL" },
        ],
      } as unknown as RunReview,
      pending_judge: null,
    });
    const shared = {
      selected: false,
      onToggleSelect: () => {},
      onDispose: () => {},
      repos: [],
      onFiled: () => {},
    };
    const ownerGroup = ownerGroupWithPreview("My own latency note.");
    const { rerender } = render(
      <MemoryRouter>
        <GroupRow group={ownerGroup} scope="mine" fetchReview={fetchReview} {...shared} />
      </MemoryRouter>,
    );

    // Expand under Mine → the full owner rationale replaces the clamped preview.
    fireEvent.click(screen.getByRole("button", { name: "Expand occurrences" }));
    await waitFor(() => expect(screen.getByText("OWNER FULL RATIONALE DETAIL")).toBeTruthy());

    // Re-render the SAME instance into the admin scope (fetchReview omitted, an admin group at the
    // same coordinate). The stale owner rationale must be cleared.
    rerender(
      <MemoryRouter>
        <GroupRow
          group={adminGroup({ rationale_preview: "Queue-to-claim latency dominated across users." })}
          scope="all"
          fetchReview={undefined}
          {...shared}
        />
      </MemoryRouter>,
    );
    await waitFor(() => expect(screen.queryByText("OWNER FULL RATIONALE DETAIL")).toBeNull());
    // Only the anonymized clamped preview remains (header + expander both render it).
    expect(screen.getAllByText("Queue-to-claim latency dominated across users.").length).toBeGreaterThan(0);
  });
});

// ownerGroupWithPreview builds a one-occurrence owner group carrying a distinct rationale preview,
// reused by the Finding [7]/[8] tests to tell an owner backlog apart from the admin aggregate.
function ownerGroupWithPreview(preview: string): JudgeBacklog["groups"][number] {
  return {
    category: "improve_uzi",
    target: "api/internal/poller",
    bucket: "todo",
    open_count: 1,
    run_count: 1,
    rationale_preview: preview,
    occurrences: [
      { run_id: "run-1", run_title: "My run", review_id: "rev-1", rec_id: "rec-1", verdict: "issues", confidence: "", bucket: "todo" },
    ],
  };
}
