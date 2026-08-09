// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { AppShell } from "./AppShell";
import { api } from "../lib/api";
import { useAuth } from "../auth/AuthContext";

// A THIRD file for a SINGLE assertion, because the alternative does not work.
// AppShell memoises GET /api/version in a module-scope promise with no reset seam;
// vitest isolates per file, so the moment any earlier test in a file lets that
// promise RESOLVE, a later `mockRejectedValue` is never consulted and the popover
// renders from the first response. That is not a hypothesis — this assertion was
// written as a fourth case in AppShell.buildinfo.test.tsx and failed there for
// exactly that reason.

vi.mock("../lib/api", () => ({
  MOCK_MODE: false,
  api: {
    listRepos: vi.fn().mockResolvedValue({ repos: [] }),
    listConnections: vi.fn().mockResolvedValue({ connections: [] }),
    unreadNotificationCount: vi.fn().mockResolvedValue({ unread: 0 }),
    workerUpgradeSummary: vi.fn().mockResolvedValue({ attention: 0, target_release: "0.4.2" }),
    getJudgeStats: vi
      .fn()
      .mockResolvedValue({ total: 0, todo: 0, filed: 0, done: 0, dismissed: 0, false_positives: 0 }),
    runsInProgressCount: vi.fn().mockResolvedValue({ count: 0 }),
    listSchedules: vi.fn().mockResolvedValue([]),
    listRuns: vi.fn().mockResolvedValue({ runs: [] }),
    getMyRateLimits: vi.fn().mockResolvedValue({ status: "no_token" }),
    version: vi.fn().mockRejectedValue(new Error("500 from /api/version")),
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

beforeEach(() => {
  vi.mocked(useAuth).mockReturnValue({ user, logout: vi.fn() } as never);
});

afterEach(cleanup);

describe("AppShell build info — the endpoint fails", () => {
  it("swallows the rejection and renders no badge, rather than throwing inside the shell", async () => {
    render(
      <MemoryRouter initialEntries={["/dashboard"]}>
        <AppShell>
          <div data-testid="content" />
        </AppShell>
      </MemoryRouter>,
    );

    await waitFor(() => expect(mockApi.version).toHaveBeenCalled());
    await waitFor(() => expect(mockApi.listConnections).toHaveBeenCalled());

    // No badge and no popover — the same nothing the old single-string footer
    // rendered when its fetch failed.
    expect(screen.queryByRole("tooltip")).toBeNull();
    // And the rest of the chrome is still standing: a 401 or a 500 on an
    // unauthenticated build endpoint must not take the navigation down with it.
    expect(screen.getByTestId("content")).toBeTruthy();
    expect(screen.getByRole("link", { name: "Overview" })).toBeTruthy();
  });
});
