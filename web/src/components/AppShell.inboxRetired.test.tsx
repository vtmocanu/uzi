// @vitest-environment jsdom
//
// PRD #1650 M4 (D1 web half + D5): the Notifications inbox is retired. The shell has
// no bell nav item, makes no unread-count request, and the status favicon's amber
// ("attention") dot comes only from a run that needs the user, never from an unread
// inbox count.
//
// The unread spy below is a plain hoisted vi.fn on the api mock, deliberately NOT a
// typed api method: the real api no longer has an unread-count call, so the spy is the
// tripwire for any code path that would still ask for one (and, before the removal,
// it is what drove the favicon amber with idle runs).
import { act } from "react";
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { AppShell } from "./AppShell";
import { api } from "../lib/api";
import { useAuth } from "../auth/AuthContext";

const { unreadSpy, applyFaviconSpy } = vi.hoisted(() => ({
  unreadSpy: vi.fn(),
  applyFaviconSpy: vi.fn(),
}));

vi.mock("../lib/favicon", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../lib/favicon")>()),
  applyFavicon: applyFaviconSpy,
}));

vi.mock("../lib/api", () => ({
  MOCK_MODE: false,
  api: {
    listRepos: vi.fn().mockResolvedValue({ repos: [] }),
    listConnections: vi.fn().mockResolvedValue({ connections: [] }),
    unreadNotificationCount: unreadSpy,
    workerUpgradeSummary: vi.fn().mockResolvedValue({ attention: 0, target_release: "0.6.0" }),
    getJudgeStats: vi
      .fn()
      .mockResolvedValue({ total: 0, todo: 0, filed: 0, done: 0, dismissed: 0, false_positives: 0 }),
    runsInProgressCount: vi.fn().mockResolvedValue({ count: 0 }),
    listSchedules: vi.fn().mockResolvedValue([]),
    getFindingsStats: vi.fn().mockResolvedValue({ total: 0, todo: 0, filed: 0, done: 0, dismissed: 0, false_positives: 0 }),
    listRuns: vi.fn().mockResolvedValue({ runs: [] }),
    getMyRateLimits: vi.fn().mockResolvedValue({ status: "no_token" }),
    getMyCodexRateLimits: vi.fn().mockResolvedValue({ accounts: [] }),
    getMySettings: vi.fn().mockResolvedValue({
      settings: { default_harness: null, default_model: null, default_effort: null, judge_model: null, summary_model: null, theme: null },
    }),
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
  ci_autofix_enabled: false,
  attribution_enabled: true,
  ephemeral_workers_enabled: false,
  wait_on_limit: false,
  notify_early_limit_reset: false,
  judge_anthropic_secret_id: null,
  judge_anthropic_secret_label: null,
  judge_anthropic_bind_mode: "default" as const,
  created_at: "2026-01-01T00:00:00Z",
  last_login: null,
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

function renderShell(path = "/dashboard") {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <AppShell>
        <div>content</div>
      </AppShell>
    </MemoryRouter>,
  );
}

// Let every already-resolved poll promise (runs, the former unread poll) settle and
// its effects run, so a "never amber" assertion is not read before the state it guards.
async function settle() {
  await act(async () => {
    await new Promise((r) => setTimeout(r, 20));
  });
}

beforeEach(() => {
  Object.defineProperty(window, "localStorage", { configurable: true, value: makeStorage() });
  vi.mocked(useAuth).mockReturnValue({
    user,
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
  });
  // A server that still reported unread inbox rows must not reach the UI at all.
  unreadSpy.mockResolvedValue({ unread: 3 });
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("the retired Notifications inbox (PRD #1650 M4)", () => {
  it("renders no Notifications nav item and no /notifications link, while the rest of the nav renders", async () => {
    const { container } = renderShell();

    // Positive half first: the nav is really there, so the negatives are not vacuous.
    expect(await screen.findByRole("link", { name: "Runs" })).toBeTruthy();
    expect(screen.getByRole("link", { name: /Judge/ })).toBeTruthy();

    expect(screen.queryByRole("link", { name: /Notifications/ })).toBeNull();
    expect(container.querySelector('a[href="/notifications"]')).toBeNull();
  });

  it("makes no unread-count request and keeps the favicon off amber when runs are idle", async () => {
    renderShell();
    await screen.findByRole("link", { name: "Runs" });
    await waitFor(() => expect(mockApi.listRuns).toHaveBeenCalled());
    await settle();

    // The idle state was derived and applied (the poll really ran)...
    expect(applyFaviconSpy).toHaveBeenCalledWith("idle", null);
    // ...and nothing turned it amber, because nothing asked for an inbox count.
    // (Read the state argument directly: expect.anything() would not match the null base.)
    expect(applyFaviconSpy.mock.calls.map((c) => c[0])).not.toContain("attention");
    expect(unreadSpy).not.toHaveBeenCalled();
  });

  it("still turns the favicon amber for a run awaiting the user's approval", async () => {
    mockApi.listRuns.mockResolvedValue({
      runs: [{ id: "r1", status: "awaiting_approval", stop_kind: null }],
    } as unknown as Awaited<ReturnType<typeof api.listRuns>>);
    renderShell();
    await waitFor(() => expect(applyFaviconSpy).toHaveBeenCalledWith("attention", null));
  });
});
