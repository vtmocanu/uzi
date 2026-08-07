// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import type { ReactElement } from "react";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { AppShell, forgeIcon } from "./AppShell";
import { GitIcon, GitLabIcon } from "./icons";
import { api } from "../lib/api";
import { useAuth } from "../auth/AuthContext";

// AppShell joins repos (board children) with forge connections web-side and gates
// the signed-in shell on useAuth; both are mocked so these stay page-level and
// offline.
vi.mock("../lib/api", () => ({
  MOCK_MODE: false,
  api: {
    listRepos: vi.fn(),
    listConnections: vi.fn(),
    unreadNotificationCount: vi.fn(),
    // The Judge nav badge (PRD #98) polls /me/judge/stats on mount; default to an empty
    // backlog so the badge is absent unless a test overrides it.
    // PRD #113 M6: AppShell now owns a workers-attention poll too. Zero by default so
    // these navigation tests assert the nav STRUCTURE without an alert badge in the way.
    workerUpgradeSummary: vi.fn().mockResolvedValue({ attention: 0, target_release: "0.6.0" }),
    getJudgeStats: vi
      .fn()
      .mockResolvedValue({ total: 0, todo: 0, filed: 0, done: 0, dismissed: 0, false_positives: 0 }),
    // The Runs nav badge (PRD #239) polls /me/runs/in-progress-count on mount; default to
    // zero so these navigation tests assert the nav STRUCTURE without a badge in the way.
    runsInProgressCount: vi.fn().mockResolvedValue({ count: 0 }),
    // The status favicon (PRD #70) polls listRuns on mount via useFavicon; stub it
    // so the poll resolves to an empty run set instead of throwing on an undefined
    // mock (the throw is synchronous, so the hook's own .catch never sees it).
    listRuns: vi.fn().mockResolvedValue({ runs: [] }),
    // The sidebar-footer rate-limit micro-meters (PRD #53) self-gate: default to
    // no_token so they render nothing in these nav/collapse assertions.
    getMyRateLimits: vi.fn().mockResolvedValue({ status: "no_token" }),
    // The sidebar-footer version badge fetches GET /api/version on mount; resolve it
    // so the shared module-level promise settles instead of throwing on an undefined
    // mock. Bare (no leading v) so the "renders" test below can assert the UI adds
    // the display "v" prefix.
    //
    // The un-stamped shape (PRD #175): `version` + `founded` only, which is what
    // every local build returns. Deliberately NOT the fully-stamped one — the two
    // fixtures are exercised against the popover component in
    // BuildInfoPopover.test.tsx, because the promise below is memoised at module
    // scope and vitest isolates per file, so a second shape could not be driven
    // through this one.
    version: vi.fn().mockResolvedValue({ version: "9.9.9-test", founded: "2026-07-03" }),
  },
}));
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
  wait_on_limit: false,
  judge_anthropic_secret_id: null,
  judge_anthropic_secret_label: null,
  created_at: "2026-01-01T00:00:00Z",
  last_login: null,
};

const repos = [
  {
    id: "repo-uzi",
    connection_id: "conn-1",
    forge_project_id: 1,
    path_with_namespace: "vtmocanu/uzi",
    web_url: "https://gitlab.example.com/vtmocanu/uzi",
    default_branch: "main",
    enabled: true,
    repo_skills_enabled: false,
    repo_devbox_opt_in: false,
    pipeline: null,
  },
  {
    id: "repo-atlas",
    connection_id: "conn-2",
    forge_project_id: 2,
    path_with_namespace: "vtmocanu/atlas",
    web_url: "https://gitlab.example.com/vtmocanu/atlas",
    default_branch: "main",
    enabled: true,
    repo_skills_enabled: false,
    repo_devbox_opt_in: false,
    pipeline: null,
  },
];

const gitlabConnection = {
  id: "conn-1",
  forge_type: "gitlab",
  base_url: "https://gitlab.example.com",
  bot_username: "bot",
  bot_forge_user_id: 1,
  human_username: null,
  created_at: "2026-01-01T00:00:00Z",
  last_verified_at: null,
  privilege_status: null,
  privilege_checked_at: null,
  privilege_report: null,
};

function renderShell(path: string) {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <AppShell>
        <div>content</div>
      </AppShell>
    </MemoryRouter>,
  );
}

// This jsdom build does not expose window.localStorage, so the sidebar-collapse
// preference (prefs.ts) has nothing to read/write; back it with a Map-based stub
// so the collapse state can be seeded and persisted in these tests.
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
  vi.mocked(useAuth).mockReturnValue({
    user,
    loading: false,
    prdLabel: "PRD",
    autopilotLabel: "autopilot",
    theme: "ember",
    themeOverride: null,
    defaultTheme: "ember",
    prdlessLabel: "PRDLESS",
    prdlessEnabled: false,
    vaultUnlocked: true,
    vaultExists: true,
    hasPassword: true,
    register: vi.fn(),
    login: vi.fn(),
    logout: vi.fn(),
    refresh: vi.fn(),
  });
  mockApi.listRepos.mockResolvedValue({ repos });
  mockApi.listConnections.mockResolvedValue({ connections: [gitlabConnection] });
  mockApi.unreadNotificationCount.mockResolvedValue({ unread: 0 });
});

const emptyTriage = { total: 0, todo: 0, filed: 0, done: 0, dismissed: 0, false_positives: 0 };

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("AppShell navigation", () => {
  it("renders the grouped nav with board children that carry forge icons, and no standalone Forge entry", async () => {
    renderShell("/dashboard");

    // Board children arrive from listRepos; await one before asserting.
    const uziBoard = await screen.findByRole("link", { name: "vtmocanu/uzi" });

    for (const group of ["Work", "Factory", "Configure", "Help"]) {
      expect(screen.getByText(group)).toBeTruthy();
    }
    for (const item of ["Overview", "Boards", "Runs", "Agents", "Workers", "Settings", "Docs"]) {
      expect(screen.getByRole("link", { name: item })).toBeTruthy();
    }

    // Every board child renders an inline forge glyph.
    expect(uziBoard.querySelector("svg")).not.toBeNull();
    expect(screen.getByRole("link", { name: "vtmocanu/atlas" }).querySelector("svg")).not.toBeNull();

    // Decision 3: Forge is reachable only under Settings, never as its own entry.
    expect(screen.queryByRole("link", { name: "Forge" })).toBeNull();

    // PRD #98: the Judge entry joins the Factory group.
    expect(screen.getByRole("link", { name: /Judge/ })).toBeTruthy();
  });

  it("badges the Judge nav item with the canonical /me/judge/stats.todo (PRD #98)", async () => {
    mockApi.getJudgeStats.mockResolvedValue({ ...emptyTriage, total: 12, todo: 7 });
    renderShell("/dashboard");

    // The Judge link's badge shows the to-triage count — the ONE canonical number, read as
    // .todo and never .total.
    //
    // It does NOT follow from this test that the badge agrees with the Judge page's
    // To-triage tab, and this comment used to say it did (PRD #98 review BLK-BADGE). Reading
    // the same number is only half of it: this shell is mounted ALONE, so nothing here can
    // observe the propagation gap that made the two disagree after a dispose. The agreement
    // is pinned in JudgeNavBadge.test.tsx, which mounts AppShell and Judge together —
    // the only configuration in which the bug existed.
    const judge = await screen.findByRole("link", { name: /Judge/ });
    await waitFor(() => expect(judge.textContent).toContain("7"));
    expect(judge.textContent).not.toContain("12");
  });

  it("shows the server build version (GET /api/version) in the sidebar footer", async () => {
    renderShell("/dashboard");
    // Rendered verbatim from the endpoint; desktop shell is expanded by default.
    // (version() is memoised at module scope, so we assert the rendered text rather
    // than the call count, which is order-dependent across tests in this file.)
    // Mock returns bare "9.9.9-test"; the footer prefixes a display "v".
    //
    // PRD #175: the badge is now a BUTTON that opens a build-info popover. Asserted
    // by role here, so this test also pins that the version stayed reachable and
    // keyboard-focusable through that change.
    expect(await screen.findByRole("button", { name: "v9.9.9-test" })).toBeTruthy();
  });

  it("still renders board children (with the fallback icon) when the connections join fails", async () => {
    mockApi.listConnections.mockRejectedValue(new Error("offline"));
    renderShell("/dashboard");

    const uziBoard = await screen.findByRole("link", { name: "vtmocanu/uzi" });
    expect(uziBoard.querySelector("svg")).not.toBeNull();
  });

  it("keeps Settings lit across /settings/* except /settings/workers (owned by the Workers entry)", async () => {
    // /settings itself → Settings active.
    renderShell("/settings");
    await waitFor(() => expect(mockApi.listConnections).toHaveBeenCalled());
    expect(screen.getByRole("link", { name: "Settings" }).getAttribute("aria-current")).toBe("page");
    cleanup();

    // A forge tab lands under /settings/forge → Settings stays lit.
    renderShell("/settings/forge");
    await waitFor(() => expect(mockApi.listConnections).toHaveBeenCalled());
    expect(screen.getByRole("link", { name: "Settings" }).getAttribute("aria-current")).toBe("page");
    expect(screen.getByRole("link", { name: "Workers" }).getAttribute("aria-current")).toBeNull();
    cleanup();

    // /settings/workers belongs to the Factory "Workers" entry, not Settings.
    renderShell("/settings/workers");
    await waitFor(() => expect(mockApi.listConnections).toHaveBeenCalled());
    expect(screen.getByRole("link", { name: "Workers" }).getAttribute("aria-current")).toBe("page");
    expect(screen.getByRole("link", { name: "Settings" }).getAttribute("aria-current")).toBeNull();
  });
});

describe("AppShell sidebar collapse", () => {
  it("collapses to an icon rail via the footer toggle: board children and group labels drop, destinations stay reachable", async () => {
    renderShell("/dashboard");
    await screen.findByRole("link", { name: "vtmocanu/uzi" }); // expanded: board children present

    const toggle = screen.getByRole("button", { name: "Collapse sidebar" });
    expect(toggle.getAttribute("aria-expanded")).toBe("true");
    fireEvent.click(toggle);

    // Now collapsed: the toggle flips, board children and group labels are gone.
    const expand = screen.getByRole("button", { name: "Expand sidebar" });
    expect(expand.getAttribute("aria-expanded")).toBe("false");
    expect(screen.queryByRole("link", { name: "vtmocanu/uzi" })).toBeNull();
    expect(screen.queryByRole("link", { name: "vtmocanu/atlas" })).toBeNull();
    expect(screen.queryByText("Work")).toBeNull();
    expect(screen.queryByText("Factory")).toBeNull();

    // Every primary destination is still reachable (label survives as a title).
    for (const item of ["Overview", "Boards", "Runs", "Agents", "Workers", "Settings", "Docs"]) {
      expect(screen.getByRole("link", { name: item })).toBeTruthy();
    }
  });

  it("initialises collapsed from a persisted preference (survives a reload)", async () => {
    window.localStorage.setItem("uzi.sidebar.collapsed", "true");
    renderShell("/dashboard");
    await waitFor(() => expect(mockApi.listRepos).toHaveBeenCalled());

    // Collapsed on first paint — no click — so the state persisted across the mount.
    expect(screen.getByRole("button", { name: "Expand sidebar" })).toBeTruthy();
    expect(screen.queryByRole("link", { name: "vtmocanu/uzi" })).toBeNull();
    expect(screen.queryByText("Work")).toBeNull();
  });

  it("writes the collapsed state to localStorage each time it is toggled", async () => {
    renderShell("/dashboard");
    await screen.findByRole("link", { name: "vtmocanu/uzi" });

    fireEvent.click(screen.getByRole("button", { name: "Collapse sidebar" }));
    expect(window.localStorage.getItem("uzi.sidebar.collapsed")).toBe("true");

    fireEvent.click(screen.getByRole("button", { name: "Expand sidebar" }));
    expect(window.localStorage.getItem("uzi.sidebar.collapsed")).toBe("false");
  });
});

describe("forgeIcon (Decision 2 mapping)", () => {
  it("maps gitlab to the tanuki and any other/unknown type to the generic git mark", () => {
    expect((forgeIcon("gitlab") as ReactElement).type).toBe(GitLabIcon);
    expect((forgeIcon("forgejo") as ReactElement).type).toBe(GitIcon);
    expect((forgeIcon(undefined) as ReactElement).type).toBe(GitIcon);
  });
});

// PRD #113 M6: the Workers nav alert badge.
describe("Workers nav alert badge (PRD #113 M6)", () => {
  it("badges the Workers nav item with the attention count, alert-toned", async () => {
    mockApi.workerUpgradeSummary.mockResolvedValue({ attention: 2, target_release: "0.6.0" });
    renderShell("/dashboard");
    // The aria-label says what the number MEANS. "2 unread" for a worker count would be
    // wrong in a way a screen-reader user could not recover from.
    const badge = await screen.findByLabelText("2 needing attention");
    expect(badge.textContent).toBe("2");
    // Alert tone, not the brand pill the Judge/unread badges use (Decision 2): red reads
    // "go look", grey reads "there is a queue".
    expect(badge.className).toContain("bg-danger");
  });

  it("renders NO badge at zero, rather than a badge showing 0", async () => {
    mockApi.workerUpgradeSummary.mockResolvedValue({ attention: 0, target_release: "0.6.0" });
    renderShell("/dashboard");
    await screen.findByText("Workers");
    // A permanent "0" is an ornament that means nothing, and it trains the reader to stop
    // looking at the one place this feature has to be noticed.
    expect(screen.queryByLabelText(/needing attention/)).toBeNull();
  });

  it("keeps the last known count when the poll fails, rather than blanking to zero", async () => {
    mockApi.workerUpgradeSummary.mockResolvedValue({ attention: 3, target_release: "0.6.0" });
    renderShell("/dashboard");
    expect((await screen.findByLabelText("3 needing attention")).textContent).toBe("3");

    // A transient failure must not read as "resolved" — that is the one wrong answer for
    // an alert badge, because it looks exactly like the problem going away.
    mockApi.workerUpgradeSummary.mockRejectedValue(new Error("network"));
    await new Promise((r) => setTimeout(r, 0));
    expect(screen.queryByLabelText("3 needing attention")).toBeTruthy();
  });

  it("does not use the Judge badge's tone, so the two are distinguishable", async () => {
    mockApi.workerUpgradeSummary.mockResolvedValue({ attention: 1, target_release: "0.6.0" });
    mockApi.getJudgeStats.mockResolvedValue({ ...emptyTriage, total: 4, todo: 4 });
    renderShell("/dashboard");
    const workers = await screen.findByLabelText("1 needing attention");
    const judge = await screen.findByLabelText("4 unread");
    expect(workers.className).toContain("bg-danger");
    expect(judge.className).not.toContain("bg-danger");
  });
});

// PRD #239: the Runs nav count badge. Brand "count" tone (Decision 2), not the Workers
// alert red — in-progress runs are healthy activity, a queue to get to.
describe("Runs nav count badge (PRD #239)", () => {
  // The count tone reuses the shared "N unread" aria-label the Judge and Notifications
  // badges also use, so — unlike the Workers "N needing attention" label — a GLOBAL query
  // is ambiguous: another nav item's count badge (or one leaked by a prior test, since
  // clearAllMocks keeps mock implementations and beforeEach re-zeroes only `unread`) can
  // satisfy it. Every assertion below is therefore scoped to the Runs link with within().
  it("badges the Runs nav item with the in-progress count, brand-toned", async () => {
    mockApi.runsInProgressCount.mockResolvedValue({ count: 5 });
    renderShell("/dashboard");
    const runs = await screen.findByRole("link", { name: /Runs/ });
    const badge = await within(runs).findByLabelText("5 unread");
    // Brand pill, NOT the Workers alert red (Decision 2): "there is a queue", not "go look".
    expect(badge.textContent).toBe("5");
    expect(badge.className).toContain("bg-brand");
    expect(badge.className).not.toContain("bg-danger");
  });

  it("renders NO badge at zero, rather than a badge showing 0", async () => {
    mockApi.runsInProgressCount.mockResolvedValue({ count: 0 });
    renderShell("/dashboard");
    const runs = await screen.findByRole("link", { name: /Runs/ });
    // A permanent "0" is an ornament that means nothing. At zero the Runs link carries its
    // label and no digit, and no count pill of its own.
    await new Promise((r) => setTimeout(r, 0));
    expect(runs.textContent).not.toMatch(/\d/);
    expect(within(runs).queryByLabelText(/unread/)).toBeNull();
  });

  it("keeps the last known count when the poll fails, rather than blanking to zero", async () => {
    mockApi.runsInProgressCount.mockResolvedValue({ count: 4 });
    renderShell("/dashboard");
    const runs = await screen.findByRole("link", { name: /Runs/ });
    expect((await within(runs).findByLabelText("4 unread")).textContent).toBe("4");

    // A transient failure keeps the last known count (the empty catch) rather than dropping
    // to zero, which would read as "the factory went idle" when it did not.
    mockApi.runsInProgressCount.mockRejectedValue(new Error("network"));
    await new Promise((r) => setTimeout(r, 0));
    expect(within(runs).queryByLabelText("4 unread")).toBeTruthy();
  });

  it("keeps the count reachable by name in the collapsed rail (BLK-4)", async () => {
    mockApi.runsInProgressCount.mockResolvedValue({ count: 3 });
    renderShell("/dashboard");
    // Hold the Runs <a> node: collapse re-renders it in place (same tree position, so React
    // reuses the DOM node), and once collapsed its only text is the sr-only count — so its
    // accessible name is no longer "Runs" and it could not be re-found by that name.
    const runs = await screen.findByRole("link", { name: /Runs/ });
    await within(runs).findByLabelText("3 unread");
    // Collapse the rail: the expanded pill is gated on !collapsed and the rail dot is
    // aria-hidden, so the sr-only count is the only carrier left. Brand tone → "N unread".
    fireEvent.click(screen.getByRole("button", { name: /collapse sidebar/i }));
    expect(await within(runs).findByText("3 unread")).toBeTruthy();
  });
});

// M7 — the leak that was previously only a READ of the code.
//
// Two halves have to work, and a cleanup that does only one of them looks correct: clearing
// the interval stops the timer, and `alive = false` stops an in-flight promise resolving into
// a setState on an unmounted tree. A clearInterval-only cleanup passes an eyeball review and
// still warns (or worse, keeps a reference alive) when a fetch was already in flight.
//
// web-ux cannot see either failure — a leak has no appearance — which is why this is a
// fake-timer test and not a browser check.
describe("the workers-attention poll's cleanup (PRD #113 M7)", () => {
  it("stops polling after unmount, and does not fire again on the next interval", async () => {
    vi.useFakeTimers();
    try {
      mockApi.workerUpgradeSummary.mockResolvedValue({ attention: 1, target_release: "0.6.0" });
      const { unmount } = renderShell("/dashboard");
      // Let the mount-time load run.
      await vi.advanceTimersByTimeAsync(0);
      const afterMount = mockApi.workerUpgradeSummary.mock.calls.length;
      expect(afterMount).toBeGreaterThan(0);

      // One interval tick while mounted: the poll is live, which is the control. Without
      // this the assertion below would pass against a poll that never ticks at all.
      await vi.advanceTimersByTimeAsync(60_000);
      const afterOneTick = mockApi.workerUpgradeSummary.mock.calls.length;
      expect(afterOneTick).toBeGreaterThan(afterMount);

      unmount();
      // Two more intervals. A surviving timer would add calls here.
      await vi.advanceTimersByTimeAsync(180_000);
      expect(mockApi.workerUpgradeSummary.mock.calls.length).toBe(afterOneTick);
    } finally {
      vi.useRealTimers();
    }
  });

  it("does not setState from a request still in flight at unmount", async () => {
    vi.useFakeTimers();
    const errors: unknown[] = [];
    const spy = vi.spyOn(console, "error").mockImplementation((...a) => errors.push(a));
    try {
      // A request that resolves only AFTER unmount — the case `alive = false` exists for and
      // the one clearInterval alone cannot cover.
      let release: (v: { attention: number; target_release: string }) => void = () => {};
      mockApi.workerUpgradeSummary.mockReturnValue(
        new Promise((res) => {
          release = res;
        }),
      );
      const { unmount } = renderShell("/dashboard");
      await vi.advanceTimersByTimeAsync(0);
      unmount();
      release({ attention: 5, target_release: "0.6.0" });
      await vi.advanceTimersByTimeAsync(0);

      const stateWarning = errors.filter((e) => JSON.stringify(e).includes("unmounted"));
      expect(stateWarning).toHaveLength(0);
    } finally {
      spy.mockRestore();
      vi.useRealTimers();
    }
  });
});

// BLK-4 — the count must survive collapse.
//
// The expanded pill is gated on `!collapsed` and the rail's dot is `aria-hidden`, so before
// this an assistive-tech user in the collapsed rail got no count and no tone at all — only
// `title="Workers"`. That is the information not being there rather than a visual
// degradation, and it happens in the layout where the operator has least context.
//
// Note what this test does NOT claim: it asserts an accessible NAME exists and carries the
// count and the tone. Whether a screen reader announces it usefully in this nav structure is
// the thing jsdom cannot see — correctness is not reachability.
describe("the collapsed rail keeps the badge's meaning (BLK-4)", () => {
  it("exposes the count and tone when collapsed, not just a decorative dot", async () => {
    mockApi.workerUpgradeSummary.mockResolvedValue({ attention: 2, target_release: "0.6.0" });
    renderShell("/dashboard");
    await screen.findByText("Workers");

    // Collapse the rail. getByRole, NOT queryByRole with an early return — an early return
    // here is exactly the vacuous escape hatch this suite keeps finding elsewhere: the test
    // would pass by never reaching its assertion if the toggle were ever renamed.
    fireEvent.click(screen.getByRole("button", { name: /collapse sidebar/i }));

    // The tone-bearing string must still be reachable by name.
    expect(await screen.findByText("2 needing attention")).toBeTruthy();
  });
});
