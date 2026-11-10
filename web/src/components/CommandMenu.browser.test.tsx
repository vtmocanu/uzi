/// <reference types="@vitest/browser-playwright" />
import "../index.css";
import "@fontsource/ibm-plex-sans/latin-400.css";
import "@fontsource/ibm-plex-sans/latin-500.css";
import "@fontsource/ibm-plex-sans/latin-600.css";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { page, userEvent } from "vitest/browser";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { AppShell } from "./AppShell";
import { api } from "../lib/api";
import { isApplePlatform } from "../lib/platform";
import { useAuth } from "../auth/AuthContext";

// PRD #3875 M1: the command menu in a real engine. Focus order, the Tab trap, focus return
// and the 400px fit need real keyboard events and layout, which jsdom cannot give.
vi.mock("../lib/api", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../lib/api")>()),
  MOCK_MODE: false,
  api: {
    listRepos: vi.fn(),
    listConnections: vi.fn(),
    workerUpgradeSummary: vi.fn().mockResolvedValue({ attention: 0, target_release: "0.6.0" }),
    getJudgeStats: vi
      .fn()
      .mockResolvedValue({ total: 0, todo: 0, filed: 0, done: 0, dismissed: 0, false_positives: 0 }),
    runsInProgressCount: vi.fn().mockResolvedValue({ count: 0 }),
    listSchedules: vi.fn().mockResolvedValue([]),
    getFindingsStats: vi
      .fn()
      .mockResolvedValue({ total: 0, todo: 0, filed: 0, done: 0, dismissed: 0, false_positives: 0 }),
    listRuns: vi.fn().mockResolvedValue({ runs: [] }),
    getRecoveryHolds: vi.fn().mockResolvedValue({
      aggregate: { open_holds: 0, custody_hold_limit: 8, decision_needed: 0, blocked_runs: 0 },
      holds: [],
    }),
    getMyRateLimits: vi.fn().mockResolvedValue({ status: "no_token" }),
    getMyCodexRateLimits: vi.fn().mockResolvedValue({ accounts: [] }),
    getMySettings: vi.fn().mockResolvedValue({ settings: {} }),
    version: vi.fn().mockResolvedValue({ version: "9.9.9-test", founded: "2026-07-03" }),
    getAdminHealth: vi.fn().mockRejectedValue(new Error("no health")),
  },
}));
vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));

const mockApi = vi.mocked(api);

beforeEach(() => {
  vi.mocked(useAuth).mockReturnValue({
    user: {
      id: "u1",
      email: "member@uzi.local",
      display_name: "Member",
      is_admin: false,
      is_active: true,
      created_at: "2026-01-01T00:00:00Z",
      last_login: null,
    },
    loading: false,
    logout: vi.fn(),
    refresh: vi.fn(),
  } as unknown as ReturnType<typeof useAuth>);
  mockApi.listRepos.mockResolvedValue({
    repos: [{ id: "r-net", connection_id: "c1", path_with_namespace: "acme/network", enabled: true } as never],
  });
  mockApi.listConnections.mockResolvedValue({ connections: [] });
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

// The shortcut with the platform's own modifier, as a real user would press it.
async function pressShortcut() {
  await userEvent.keyboard(isApplePlatform() ? "{Meta>}k{/Meta}" : "{Control>}k{/Control}");
}

const getMenu = () => screen.queryByRole("dialog", { name: "Command menu" });

async function mountShell(children: React.ReactNode = null) {
  render(
    <MemoryRouter initialEntries={["/runs"]}>
      <AppShell>{children}</AppShell>
    </MemoryRouter>,
  );
  // The shell has rendered once the mobile trigger (narrow) or a sidebar link (wide) is exposed.
  await vi.waitFor(() => {
    const ready = window.innerWidth < 1024 ? screen.queryByRole("button", { name: "Open navigation" }) : screen.queryByRole("link", { name: "Workers" });
    expect(ready).not.toBeNull();
  });
}

async function openMenuAndWait() {
  await pressShortcut();
  const dialog = await screen.findByRole("dialog", { name: "Command menu" });
  const input = await screen.findByRole("combobox");
  await vi.waitFor(() => expect(document.activeElement).toBe(input));
  return { dialog, input };
}

describe("CommandMenu in a real browser", () => {
  it("opens on the platform shortcut and focuses the input", async () => {
    await page.viewport(1200, 800);
    await mountShell();
    expect(getMenu()).toBeNull(); // precondition: closed before the keystroke
    const { dialog, input } = await openMenuAndWait();
    expect(getMenu()).toBe(dialog);
    expect(document.activeElement).toBe(input);
  });

  it("keeps Tab and Shift+Tab inside the dialog", async () => {
    await page.viewport(1200, 800);
    await mountShell();
    const { dialog, input } = await openMenuAndWait();
    // Positive precondition: there is page content outside the dialog to escape to.
    const outside = screen.getByRole("link", { name: "Workers" });
    expect(dialog.contains(outside)).toBe(false);
    expect(dialog.contains(input)).toBe(true);

    for (let i = 0; i < 6; i++) {
      await userEvent.tab();
      expect(dialog.contains(document.activeElement), `Tab #${i + 1} stays inside`).toBe(true);
      expect(document.activeElement).not.toBe(document.body);
    }
    for (let i = 0; i < 6; i++) {
      await userEvent.tab({ shift: true });
      expect(dialog.contains(document.activeElement), `Shift+Tab #${i + 1} stays inside`).toBe(true);
      expect(document.activeElement).not.toBe(document.body);
    }
  });

  it("returns focus to the element that had it when Escape closes the menu", async () => {
    await page.viewport(1200, 800);
    await mountShell(<button>probe</button>);
    const probe = await screen.findByRole("button", { name: "probe" });
    probe.focus();
    expect(document.activeElement).toBe(probe);
    const { input } = await openMenuAndWait();
    expect(document.activeElement).toBe(input);
    expect(document.activeElement).not.toBe(probe);

    await userEvent.keyboard("{Escape}");
    await vi.waitFor(() => expect(getMenu()).toBeNull());
    expect(document.activeElement).toBe(probe);
  });

  it("at 400px, opening from a focused drawer link closes the drawer and Escape lands on Open navigation", async () => {
    await page.viewport(400, 800);
    await mountShell();
    expect(window.innerWidth).toBe(400);
    const openNav = screen.getByRole("button", { name: "Open navigation" });
    openNav.click();
    const closeNav = await screen.findByRole("button", { name: "Close navigation" });
    const drawer = closeNav.parentElement as HTMLElement;
    const link = Array.from(drawer.querySelectorAll("a")).find((a) => a.getAttribute("href") === "/runs") as HTMLElement;
    expect(link).toBeTruthy();
    link.focus();
    expect(document.activeElement).toBe(link);

    await openMenuAndWait();
    expect(screen.queryByRole("button", { name: "Close navigation" })).toBeNull();

    await userEvent.keyboard("{Escape}");
    await vi.waitFor(() => expect(getMenu()).toBeNull());
    expect(document.activeElement).toBe(openNav);
    expect(document.activeElement).not.toBe(document.body);
  });

  it("at 400px, the dialog panel fits the viewport with no horizontal overflow", async () => {
    await page.viewport(400, 800);
    await mountShell();
    const { dialog } = await openMenuAndWait();
    await document.fonts.ready;
    expect(window.innerWidth).toBe(400);
    // Options have rendered, so the panel is measured with its real content.
    expect(screen.getAllByRole("option").length).toBeGreaterThan(0);

    // Modal's role="dialog" element is the full-viewport backdrop, which always fits; the
    // panel is its first child, so measure that.
    const panel = dialog.firstElementChild as HTMLElement;
    expect(panel).not.toBeNull();
    expect(panel.getBoundingClientRect().width).toBeLessThan(dialog.getBoundingClientRect().width);
    const rect = panel.getBoundingClientRect();
    expect(rect.width).toBeGreaterThan(0);
    expect(rect.left).toBeGreaterThanOrEqual(0);
    expect(rect.right).toBeLessThanOrEqual(window.innerWidth);
    expect(document.documentElement.scrollWidth).toBeLessThanOrEqual(window.innerWidth);
  });
});
