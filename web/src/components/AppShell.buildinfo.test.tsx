// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { AppShell } from "./AppShell";
import { api } from "../lib/api";
import { useAuth } from "../auth/AuthContext";
import { mockBuildInfo } from "../mocks/data";

// A FILE OF ITS OWN, and that is the whole point. AppShell memoises GET
// /api/version in a module-scope promise with no reset seam, and vitest isolates
// per file — so this is the only place a CALL-COUNT assertion is deterministic.
// AppShell.test.tsx's own comment records the same hazard from the other side
// ("order-dependent across tests in this file"), which is why it asserts rendered
// text instead. Everything here depends on this module starting cold.
//
// The response FIXTURES are exercised in BuildInfoPopover.test.tsx, where the
// component takes them as a prop and the hazard does not apply.

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
    // SidebarRateLimits fetches the chosen sidebar-token set on mount.
    getMySettings: vi.fn().mockResolvedValue({ settings: { default_model: null, theme: null } }),
    version: vi.fn(),
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
  mockApi.version.mockResolvedValue(mockBuildInfo);
});

afterEach(cleanup);

function renderShell() {
  return render(
    <MemoryRouter initialEntries={["/dashboard"]}>
      <AppShell>
        <div />
      </AppShell>
    </MemoryRouter>,
  );
}

describe("AppShell build info wiring", () => {
  it("makes ONE request for the two sidebar mounts, and scopes the popover id per mount", async () => {
    renderShell();
    // Open the mobile drawer so BOTH SidebarContent instances are mounted at once,
    // which is the configuration the desktop aside + mobile sheet produce in a real
    // narrow viewport.
    fireEvent.click(screen.getByLabelText("Open navigation"));

    const triggers = await screen.findAllByRole("button", { name: "v0.4.2" });
    expect(triggers).toHaveLength(2);

    // One request, two shapes: the module-scope promise is what makes the footer
    // and the Workers page's target release incapable of disagreeing, and it is
    // what keeps a second mount free.
    expect(mockApi.version).toHaveBeenCalledTimes(1);

    // Instance-scoped ids. A hardcoded one would put a duplicate in the document
    // here — a real bug, since aria-describedby then resolves to whichever came
    // first for BOTH triggers.
    const ids = triggers.map((t) => t.getAttribute("aria-describedby"));
    expect(ids[0]).toBeTruthy();
    expect(ids[0]).not.toBe(ids[1]);
    for (const id of ids) expect(document.getElementById(id!)).not.toBeNull();
  });

  it("renders the coordinate set from the response, not from a hardcoded string", async () => {
    renderShell();
    const trigger = await screen.findByRole("button", { name: "v0.4.2" });
    const pop = document.getElementById(trigger.getAttribute("aria-describedby")!)!;
    expect(pop.textContent).toContain("uzi v0.4.2");
    expect(pop.textContent).toContain("366a282");
    expect(pop.textContent).toContain("2,105 commits");
  });
});

// The failure case lives in AppShell.buildinfo.failure.test.tsx, NOT here — and it
// was written here first, which is how the hazard proved itself: the module-scope
// promise is already resolved by the tests above, so a mockRejectedValue in a
// fourth test is simply never consulted and the popover renders anyway. Measured,
// not theorised.
