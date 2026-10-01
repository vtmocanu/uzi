// @vitest-environment jsdom
//
// PRD #1910 D3: AppShell consumes the pending-return entry once the session exists, so an OIDC
// login (which always lands on "/") returns to the consent page.
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { AppShell } from "./AppShell";
import { GuestRoute } from "./RouteGuards";
import { useAuth } from "../auth/AuthContext";
import { consumePendingReturn, setPendingReturn } from "../lib/pendingReturn";

vi.mock("../lib/api", () => ({
  MOCK_MODE: false,
  api: {
    listRepos: vi.fn().mockResolvedValue({ repos: [] }),
    listConnections: vi.fn().mockResolvedValue({ connections: [] }),
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
    getAdminHealth: vi.fn(),
  },
}));
vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));

const ID = "6f0e4b0a-1c2d-4e3f-8a9b-0c1d2e3f4a5b";
const user = {
  id: "u1",
  email: "u@uzi.local",
  display_name: "U",
  is_admin: false,
  is_active: true,
};

function asAuth(u: typeof user | null, loading = false) {
  vi.mocked(useAuth).mockReturnValue({ user: u, loading } as unknown as ReturnType<typeof useAuth>);
}

function renderAt(path: string) {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <AppShell>
        <Routes>
          <Route path="/" element={<div>HOME PAGE</div>} />
          <Route path="/connect" element={<div>CONNECT PAGE</div>} />
        </Routes>
      </AppShell>
    </MemoryRouter>,
  );
}

// The OIDC landing as App.tsx mounts it: "/" is a GuestRoute, which bounces a signed-in user to
// /dashboard, so the pending /connect return has to win over that Navigate.
function renderAtGuestLanding() {
  return render(
    <MemoryRouter initialEntries={["/"]}>
      <AppShell>
        <Routes>
          <Route
            path="/"
            element={
              <GuestRoute>
                <div>LANDING PAGE</div>
              </GuestRoute>
            }
          />
          <Route path="/dashboard" element={<div>DASHBOARD PAGE</div>} />
          <Route path="/connect" element={<div>CONNECT PAGE</div>} />
        </Routes>
      </AppShell>
    </MemoryRouter>,
  );
}

beforeEach(() => window.sessionStorage.clear());
afterEach(() => {
  cleanup();
  window.sessionStorage.clear();
});

describe("AppShell pending return", () => {
  it("navigates to the stored /connect path once the user is signed in, and consumes it", async () => {
    setPendingReturn(`/connect?request=${ID}`);
    asAuth(user);
    renderAt("/");
    await waitFor(() => expect(screen.getByText("CONNECT PAGE")).toBeTruthy());
    expect(consumePendingReturn()).toBeNull();
  });

  it("does nothing while the session is still loading or signed out, and keeps the entry", async () => {
    setPendingReturn(`/connect?request=${ID}`);
    asAuth(null, true);
    const first = renderAt("/");
    expect(screen.getByText("HOME PAGE")).toBeTruthy();
    first.unmount();
    asAuth(null, false);
    renderAt("/");
    expect(screen.getByText("HOME PAGE")).toBeTruthy();
    expect(consumePendingReturn()).toBe(`/connect?request=${ID}`);
  });

  it("ignores a planted entry that is not a consent return", async () => {
    window.sessionStorage.setItem("uzi.pendingReturn", "//evil.example/connect?request=" + ID);
    asAuth(user);
    renderAt("/");
    await waitFor(() => expect(window.sessionStorage.getItem("uzi.pendingReturn")).toBeNull());
    expect(screen.getByText("HOME PAGE")).toBeTruthy();
  });

  it("does not navigate when /connect already cleared the entry (a ?next= password return)", async () => {
    // Connect clears the entry on load; modelled here by the entry being absent.
    asAuth(user);
    renderAt("/");
    await new Promise((r) => setTimeout(r, 20));
    expect(screen.getByText("HOME PAGE")).toBeTruthy();
  });

  it("wins over GuestRoute's redirect to /dashboard on the real guest landing", async () => {
    setPendingReturn(`/connect?request=${ID}`);
    asAuth(user);
    renderAtGuestLanding();
    await waitFor(() => expect(screen.getByText("CONNECT PAGE")).toBeTruthy());
    // Let any later redirect settle: the consent page must stay, not be replaced by /dashboard.
    await new Promise((r) => setTimeout(r, 30));
    expect(screen.queryByText("DASHBOARD PAGE")).toBeNull();
    expect(screen.getByText("CONNECT PAGE")).toBeTruthy();
    expect(consumePendingReturn()).toBeNull();
  });

  it("still lets GuestRoute send a signed-in user to /dashboard when nothing is pending", async () => {
    asAuth(user);
    renderAtGuestLanding();
    await waitFor(() => expect(screen.getByText("DASHBOARD PAGE")).toBeTruthy());
  });
});
