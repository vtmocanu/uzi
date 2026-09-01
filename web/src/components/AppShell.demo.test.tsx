// @vitest-environment jsdom
//
// PRD #886 M5 — attribute-aware demo-mode masking for the AppShell user cluster.
//
// Two channels, each two-way (real with demo OFF, masked with demo ON):
//   1. the visible identity TEXT in the expanded sidebar footer;
//   2. the identity `title` TOOLTIP on the collapsed rail's avatar span
//      (AppShell.tsx: title={`${identityLabel} · …`}) — an ATTRIBUTE channel that
//      textContent/queryByText are blind to (see .claude/rules/web.md). This is the
//      load-bearing attribute-channel test: it is asserted via getByTitle, never text.
//
// The user has NO display_name, so identityLabel derives from the email (maskEmail);
// this pins that the EMAIL is what masks (maskEmail and maskName both yield "Vlad", so
// "Vlad present" alone would not prove which field masked — hence the real-email-absent
// assertion below).
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { AppShell } from "./AppShell";
import { useAuth } from "../auth/AuthContext";

vi.mock("../lib/api", () => ({
  MOCK_MODE: false,
  api: {
    listRepos: vi.fn().mockResolvedValue({ repos: [] }),
    listConnections: vi.fn().mockResolvedValue({ connections: [] }),
    unreadNotificationCount: vi.fn().mockResolvedValue({ unread: 0 }),
    workerUpgradeSummary: vi.fn().mockResolvedValue({ attention: 0, target_release: "0.6.0" }),
    getJudgeStats: vi
      .fn()
      .mockResolvedValue({ total: 0, todo: 0, filed: 0, done: 0, dismissed: 0, false_positives: 0 }),
    runsInProgressCount: vi.fn().mockResolvedValue({ count: 0 }),
    listSchedules: vi.fn().mockResolvedValue([]),
    listFindings: vi
      .fn()
      .mockResolvedValue({ bucket: "to_file", repo: "", run: "", open_count: 0, findings: [] }),
    listRuns: vi.fn().mockResolvedValue({ runs: [] }),
    getMyRateLimits: vi.fn().mockResolvedValue({ status: "no_token" }),
    getMySettings: vi.fn().mockResolvedValue({
      settings: { default_model: null, default_effort: null, judge_model: null, summary_model: null, theme: null },
    }),
    version: vi.fn().mockResolvedValue({ version: "9.9.9-test", founded: "2026-07-03" }),
  },
}));
vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));

const REAL_EMAIL = "vlad.mocanu@metaminds.com";

// display_name is null on purpose so identityLabel falls back to the email.
const user = {
  id: "u1",
  email: REAL_EMAIL,
  display_name: null as string | null,
  is_admin: false,
  is_active: true,
  autopilot_enabled: false,
  judge_enabled: false,
  ci_autofix_enabled: false,
  attribution_enabled: true,
  ephemeral_workers_enabled: false,
  wait_on_limit: false,
  judge_anthropic_secret_id: null,
  judge_anthropic_secret_label: null,
  created_at: "2026-01-01T00:00:00Z",
  last_login: null,
};

function renderShell() {
  return render(
    <MemoryRouter initialEntries={["/dashboard"]}>
      <AppShell>
        <div>content</div>
      </AppShell>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  window.localStorage.clear();
  vi.mocked(useAuth).mockReturnValue({
    user,
    loading: false,
    uziLabel: "uzi",
    autopilotLabel: "autopilot",
    theme: "ember",
    themeOverride: null,
    defaultTheme: "ember",
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
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
  window.localStorage.clear();
});

// Channel 1 — the visible identity text in the expanded footer (default, not collapsed).
describe("AppShell identity text (PRD #886 M5)", () => {
  it("demo OFF: renders the real email as visible text", async () => {
    renderShell();
    expect(await screen.findByText(REAL_EMAIL)).toBeTruthy();
  });

  it("demo ON: renders the masked first name and the real email is absent from the text", async () => {
    window.localStorage.setItem("uzi_demo_mode", "1");
    renderShell();
    expect(await screen.findByText("Vlad")).toBeTruthy();
    expect(screen.queryByText(REAL_EMAIL)).toBeNull();
  });
});

// Channel 2 — the identity `title` tooltip on the collapsed rail. ATTRIBUTE channel:
// asserted with getByTitle, NOT textContent. The collapsed state is seeded from the
// sidebar-collapse preference (prefs.ts) so the titled avatar span renders.
describe("AppShell identity title tooltip (PRD #886 M5, attribute channel)", () => {
  const titledSpan = () => screen.getByTitle(/· (User|Administrator)$/);

  it("demo OFF: the title attribute carries the real email", async () => {
    window.localStorage.setItem("uzi.sidebar.collapsed", "true");
    renderShell();
    // Wait for the collapsed shell to settle (Expand toggle only exists when collapsed).
    await screen.findByRole("button", { name: "Expand sidebar" });
    expect(titledSpan().getAttribute("title")).toBe(`${REAL_EMAIL} · User`);
  });

  it("demo ON: the title attribute is masked and does NOT contain the real email", async () => {
    window.localStorage.setItem("uzi.sidebar.collapsed", "true");
    window.localStorage.setItem("uzi_demo_mode", "1");
    renderShell();
    await screen.findByRole("button", { name: "Expand sidebar" });
    const title = titledSpan().getAttribute("title");
    expect(title).toBe("Vlad · User");
    expect(title).not.toContain(REAL_EMAIL);
  });
});
