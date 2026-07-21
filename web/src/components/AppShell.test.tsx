// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import type { ReactElement } from "react";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
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
    // The status favicon (PRD #70) polls listRuns on mount via useFavicon; stub it
    // so the poll resolves to an empty run set instead of throwing on an undefined
    // mock (the throw is synchronous, so the hook's own .catch never sees it).
    listRuns: vi.fn().mockResolvedValue({ runs: [] }),
    // The sidebar-footer rate-limit micro-meters (PRD #53) self-gate: default to
    // no_token so they render nothing in these nav/collapse assertions.
    getMyRateLimits: vi.fn().mockResolvedValue({ status: "no_token" }),
    // The sidebar-footer version line fetches GET /api/version on mount; resolve it
    // so the shared module-level promise settles instead of throwing on an undefined
    // mock. Bare (no leading v) so the "renders" test below can assert the UI adds
    // the display "v" prefix.
    version: vi.fn().mockResolvedValue({ version: "9.9.9-test" }),
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
  });

  it("shows the server build version (GET /api/version) in the sidebar footer", async () => {
    renderShell("/dashboard");
    // Rendered verbatim from the endpoint; desktop shell is expanded by default.
    // (version() is memoised at module scope, so we assert the rendered text rather
    // than the call count, which is order-dependent across tests in this file.)
    // Mock returns bare "9.9.9-test"; the footer prefixes a display "v".
    expect(await screen.findByText("v9.9.9-test")).toBeTruthy();
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
