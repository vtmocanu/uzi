// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { AppShell } from "./AppShell";
import { NAV_SECTIONS } from "./navItems";
import { api } from "../lib/api";
import { useAuth } from "../auth/AuthContext";
import { unknownDoc } from "../mocks/data/health";
import type { ModuleEntry } from "../modules/manifest";

// Module entries come from the runtime list; these tests feed it directly.
const modulesState = vi.hoisted(() => ({ modules: [] as ModuleEntry[] }));
vi.mock("../modules/registry", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../modules/registry")>()),
  useModules: () => ({ status: "ready" as const, modules: modulesState.modules }),
}));

// Issue #4510: the real sidebar wires the animated glyph to every NAV_SECTIONS row (by its
// key) and to nothing else. Mocks mirror AppShell.test.tsx.
vi.mock("../lib/api", () => ({
  MOCK_MODE: false,
  api: {
    listRepos: vi.fn(),
    listConnections: vi.fn(),
    listConnectionsBackground: vi.fn().mockResolvedValue({ connections: [] }),
    workerUpgradeSummary: vi.fn().mockResolvedValue({ attention: 0, target_release: "0.6.0" }),
    getJudgeStats: vi.fn().mockResolvedValue({ total: 0, todo: 0, filed: 0, done: 0, dismissed: 0, false_positives: 0 }),
    runsInProgressCount: vi.fn().mockResolvedValue({ count: 0 }),
    listSchedules: vi.fn().mockResolvedValue([]),
    listAgentTemplates: vi.fn().mockResolvedValue({ templates: [] }),
    listWorkers: vi.fn().mockResolvedValue({ workers: [] }),
    listSkills: vi.fn().mockResolvedValue({ skills: [] }),
    getRecoveryHolds: vi.fn().mockRejectedValue(new Error("no holds")),
    listFindings: vi.fn().mockResolvedValue({ bucket: "to_file", repo: "", run: "", open_count: 0, findings: [] }),
    getFindingsStats: vi.fn().mockResolvedValue({ total: 0, todo: 0, filed: 0, done: 0, dismissed: 0, false_positives: 0 }),
    listRuns: vi.fn().mockResolvedValue({ runs: [] }),
    getMyRateLimits: vi.fn().mockResolvedValue({ status: "no_token" }),
    getMyCodexRateLimits: vi.fn().mockResolvedValue({ accounts: [] }),
    getMySettings: vi.fn().mockResolvedValue({ settings: { harness_overflow_enabled: false, default_harness: null, default_model: null, default_effort: null, judge_model: null, summary_model: null, theme: null } }),
    version: vi.fn().mockResolvedValue({ version: "9.9.9-test", founded: "2026-07-03" }),
    getAdminHealth: vi.fn(),
    getAgentSource: vi.fn(),
    updateSettings: vi.fn(),
    applyAgentSource: vi.fn(),
  },
}));
vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));

const mockApi = vi.mocked(api);

const baseUser = {
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
  ephemeral_docker_enabled: false,
  wait_on_limit: false,
  notify_early_limit_reset: false,
  judge_anthropic_secret_id: null,
  judge_anthropic_secret_label: null,
  judge_anthropic_bind_mode: "default" as const,
  created_at: "2026-01-01T00:00:00Z",
  last_login: null,
};

const repo = (id: string, path: string) => ({
  id,
  connection_id: "conn-1",
  forge_project_id: 1,
  path_with_namespace: path,
  web_url: `https://gitlab.example.com/${path}`,
  default_branch: "main",
  enabled: true,
  repo_skills_enabled: false,
  repo_claudemd_enabled: false,
  repo_devbox_opt_in: false,
  repo_fold_improve_uzi_backlog: false,
  env_var_count: 0,
  pipeline: null,
  guardrail_blocked: false,
  docker_allowlisted: false,
  docker_blocked: false,
});

const connection = {
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

function signIn(isAdmin: boolean) {
  vi.mocked(useAuth).mockReturnValue({
    user: { ...baseUser, is_admin: isAdmin },
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
    updateUser: vi.fn(),
    serverUnreachable: false,
    retry: vi.fn(),
  });
}

beforeEach(() => {
  Object.defineProperty(window, "localStorage", { configurable: true, value: makeStorage() });
  mockApi.listRepos.mockResolvedValue({ repos: [repo("repo-uzi", "vtmocanu/uzi"), repo("repo-atlas", "vtmocanu/atlas")] });
  mockApi.listConnections.mockResolvedValue({ connections: [connection] });
  mockApi.getAdminHealth.mockResolvedValue(unknownDoc());
});

afterEach(() => {
  modulesState.modules = [];
  cleanup();
  vi.clearAllMocks();
});

describe("sidebar motion wiring in the real AppShell", () => {
  it.each([false, true])("every NAV_SECTIONS row (admin=%s) carries its own key; repo children carry none", async (isAdmin) => {
    signIn(isAdmin);
    const { container } = render(
      <MemoryRouter initialEntries={["/dashboard"]}>
        <AppShell>
          <div>content</div>
        </AppShell>
      </MemoryRouter>,
    );
    const atlas = await screen.findByRole("link", { name: "vtmocanu/atlas" });
    const uzi = screen.getByRole("link", { name: "vtmocanu/uzi" });

    const rendered = NAV_SECTIONS.flatMap((s) => s.items).filter((d) => !d.adminOnly || isAdmin);
    expect(rendered.length).toBeGreaterThan(10);
    for (const def of rendered) {
      const svg = container.querySelector(`svg[data-nav-motion="${def.key}"]`);
      expect(svg, `${def.key} svg`).not.toBeNull();
      expect(svg?.closest("a")?.getAttribute("href"), `${def.key} row`).toBe(def.to);
    }
    // Exactly one animated glyph per rendered row, and none anywhere else.
    expect(container.querySelectorAll("svg[data-nav-motion]")).toHaveLength(rendered.length);
    for (const child of [atlas, uzi]) {
      expect(child.querySelector("svg"), "repo child glyph").not.toBeNull();
      expect(child.querySelector("[data-nav-motion]"), "repo child motion").toBeNull();
    }
  });

  it("gives a module row its vocabulary icon's motion while its key and link stay the module's (issue #4777)", async () => {
    modulesState.modules = [
      { id: "wmod", version: "1", page: "Page", label: "Work Mod", icon: "chart-line", navGroup: "work", adminOnly: false },
      { id: "mmod", version: "1", page: "Page", label: "Own Mod", icon: "terminal", navGroup: "modules", adminOnly: false },
    ];
    signIn(false);
    const { container } = render(
      <MemoryRouter initialEntries={["/dashboard"]}>
        <AppShell>
          <div>content</div>
        </AppShell>
      </MemoryRouter>,
    );
    await screen.findByRole("link", { name: "vtmocanu/atlas" });
    for (const [id, icon] of [
      ["wmod", "chart-line"],
      ["mmod", "terminal"],
    ]) {
      const svg = container.querySelector(`svg[data-nav-motion="module-${icon}"]`);
      expect(svg, `${id} svg`).not.toBeNull();
      expect(svg?.closest("a")?.getAttribute("href")).toBe(`/m/${id}`);
    }
    const builtin = NAV_SECTIONS.flatMap((s) => s.items).filter((d) => !d.adminOnly).length;
    expect(container.querySelectorAll("svg[data-nav-motion]")).toHaveLength(builtin + 2);
  });

  it("renders a module's command-menu option with a static glyph, never the animated one (issue #4777)", async () => {
    modulesState.modules = [
      { id: "cmod", version: "1", page: "Page", label: "Charty Mod", icon: "chart-line", navGroup: "work", adminOnly: false },
    ];
    signIn(false);
    render(
      <MemoryRouter initialEntries={["/dashboard"]}>
        <AppShell>
          <div>content</div>
        </AppShell>
      </MemoryRouter>,
    );
    await screen.findByRole("link", { name: "vtmocanu/atlas" });
    act(() => {
      document.body.dispatchEvent(
        new KeyboardEvent("keydown", { key: "k", code: "KeyK", ctrlKey: true, bubbles: true, cancelable: true }),
      );
    });
    const dialog = screen.getByRole("dialog", { name: "Command menu" });
    const option = within(dialog).getByRole("option", { name: /Charty Mod/ });
    const glyph = option.querySelector("svg");
    expect(glyph, "option glyph").not.toBeNull();
    expect(glyph!.hasAttribute("data-nav-motion")).toBe(false);
    expect(option.querySelector("[data-nav-motion]")).toBeNull();
  });
});
