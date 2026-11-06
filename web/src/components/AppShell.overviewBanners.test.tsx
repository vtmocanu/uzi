// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter, Link, Route, Routes } from "react-router-dom";
import { AppShell } from "./AppShell";
import { InShellHeader } from "./PageHeaderSlot";
import { api, type HealthDoc } from "../lib/api";
import { useAuth } from "../auth/AuthContext";
import { healthySilentDoc, incidentDoc } from "../mocks/data/health";

vi.mock("../lib/api", () => ({
  MOCK_MODE: false,
  api: {
    listRepos: vi.fn(),
    listConnections: vi.fn(),
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
    listSchedules: vi.fn().mockResolvedValue([]),
    // PRD #333 M7 / PRD #1183 M4: AppShell polls the Findings open-count badge on navigation from
    // getFindingsStats().todo (the canonical tally) now, not the deprecated listFindings open_count
    // meta. Zero so these navigation tests assert the nav STRUCTURE without a findings badge in the
    // way. listFindings stays stubbed for any residual caller.
    listFindings: vi.fn().mockResolvedValue({ bucket: "to_file", repo: "", run: "", open_count: 0, findings: [] }),
    getFindingsStats: vi.fn().mockResolvedValue({ total: 0, todo: 0, filed: 0, done: 0, dismissed: 0, false_positives: 0 }),
    // The status favicon (PRD #70) polls listRuns on mount via useFavicon; stub it
    // so the poll resolves to an empty run set instead of throwing on an undefined
    // mock (the throw is synchronous, so the hook's own .catch never sees it).
    listRuns: vi.fn().mockResolvedValue({ runs: [] }),
    // The sidebar-footer rate-limit micro-meters (PRD #53) self-gate: default to
    // no_token so they render nothing in these nav/collapse assertions.
    getMyRateLimits: vi.fn().mockResolvedValue({ status: "no_token" }),
    // The Codex sidebar micro-meters (PRD #1209 M3) self-gate: default to no linked
    // account so they render nothing in these nav/collapse assertions.
    getMyCodexRateLimits: vi.fn().mockResolvedValue({ accounts: [] }),
    // SidebarUsageLimits fetches the chosen sidebar-account set on mount.
    getMySettings: vi.fn().mockResolvedValue({ settings: { harness_overflow_enabled: false, default_harness: null, default_model: null, default_effort: null, judge_model: null, summary_model: null, theme: null } }),
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
    // The shared HealthStatusProvider (PRD #1484 M5) fetches only for an admin; the health
    // pip tests below set a document, every other test runs as a non-admin and never calls it.
    getAdminHealth: vi.fn(),
    snoozeAdminHealth: vi.fn().mockResolvedValue({}),
    getAgentSource: vi.fn(),
    updateSettings: vi.fn(),
    applyAgentSource: vi.fn(),
  },
}));
vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));

const mockApi = vi.mocked(api);

const user = {
  id: "u1",
  email: "admin@uzi.local",
  display_name: "Admin",
  is_admin: true,
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

beforeEach(() => {
  vi.useFakeTimers();
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
    updateUser: vi.fn(),
    serverUnreachable: false,
    retry: vi.fn(),
  });
  mockApi.listRepos.mockResolvedValue({ repos: [] });
  mockApi.listConnections.mockResolvedValue({ connections: [] });
});
afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.clearAllMocks();
});

// A blocking instance incident that is also an owner attention warning: both banners render,
// the danger one first (the stack's order).
function dangerAndAttention(): HealthDoc {
  const doc = incidentDoc();
  doc.checks = doc.checks.map((c) => c.id === "recovery.storage"
    ? { ...c, severity: "warn", attention_banner: true, summary: "Synthetic pressure; 0 captures currently marked quota-refused." }
    : c);
  return doc;
}

// The Overview preview page: its header goes through InShellHeader. The runs page has none.
function PreviewPage() {
  return <>
    <InShellHeader><h1>Hdr</h1></InShellHeader>
    <p>preview content</p>
    <Link to="/runs">Go to runs</Link>
  </>;
}
function RunsPage() {
  return <>
    <h1>Runs page</h1>
    <p>runs content</p>
    <Link to="/overview/next">Go to overview</Link>
  </>;
}
async function mountAt(path: string) {
  const result = render(
    <MemoryRouter initialEntries={[path]}>
      <AppShell>
        <Routes>
          <Route path="/overview/next" element={<PreviewPage />} />
          <Route path="/runs" element={<RunsPage />} />
        </Routes>
      </AppShell>
    </MemoryRouter>,
  );
  await act(async () => {});
  return result;
}
async function go(linkName: string) {
  await act(async () => { fireEvent.click(screen.getByRole("link", { name: linkName })); });
}

const danger = () => screen.queryByRole("alert", { name: "Instance health" });
const attention = () => screen.queryByRole("status", { name: "Admin health attention" });
const follows = (a: Node, b: Node) => (a.compareDocumentPosition(b) & Node.DOCUMENT_POSITION_FOLLOWING) !== 0;

describe("AppShell banner stack and header slot (PRD #3878)", () => {
  it("renders the page header ABOVE the one banner stack on /overview/next", async () => {
    mockApi.getAdminHealth.mockResolvedValue(dangerAndAttention());
    await mountAt("/overview/next");
    const header = screen.getByRole("heading", { name: "Hdr" });
    expect(screen.getAllByRole("alert", { name: "Instance health" })).toHaveLength(1);
    expect(screen.getAllByRole("status", { name: "Admin health attention" })).toHaveLength(1);
    expect(follows(header, danger()!)).toBe(true);
    expect(follows(danger()!, attention()!)).toBe(true);
    expect(follows(attention()!, screen.getByText("preview content"))).toBe(true);
  });

  it("renders the stack once, above the content and in the same order, on /runs", async () => {
    mockApi.getAdminHealth.mockResolvedValue(dangerAndAttention());
    await mountAt("/runs");
    expect(screen.getAllByRole("alert", { name: "Instance health" })).toHaveLength(1);
    expect(screen.getAllByRole("status", { name: "Admin health attention" })).toHaveLength(1);
    expect(follows(danger()!, attention()!)).toBe(true);
    expect(follows(attention()!, screen.getByRole("heading", { name: "Runs page" }))).toBe(true);
  });

  it("keeps a danger snooze across navigation to /overview/next before any poll confirms it", async () => {
    mockApi.getAdminHealth.mockResolvedValue(dangerAndAttention());
    mockApi.snoozeAdminHealth.mockResolvedValue({ episode_id: "b1f0c2ep", snoozed_until: "2099-01-01T00:00:00Z" });
    await mountAt("/runs");
    await act(async () => { fireEvent.click(screen.getByRole("button", { name: "Snooze 1 h" })); });
    expect(mockApi.snoozeAdminHealth).toHaveBeenCalledTimes(1);
    expect(danger()).toBeNull();
    // The mocked document still has snoozed_until null, as it would before the next poll.
    await go("Go to overview");
    expect(screen.getByRole("heading", { name: "Hdr" })).toBeTruthy();
    expect(danger()).toBeNull();
    await go("Go to runs");
    expect(danger()).toBeNull();
    expect(mockApi.getAdminHealth).toHaveBeenCalledTimes(1);
  });

  it("still shows the danger banner on /overview/next after a rejected snooze", async () => {
    mockApi.getAdminHealth.mockResolvedValue(dangerAndAttention());
    mockApi.snoozeAdminHealth.mockRejectedValue(new Error("write failed"));
    await mountAt("/runs");
    await act(async () => { fireEvent.click(screen.getByRole("button", { name: "Snooze 1 h" })); });
    expect(danger()).toBeTruthy();
    await go("Go to overview");
    expect(danger()).toBeTruthy();
    expect(follows(screen.getByRole("heading", { name: "Hdr" }), danger()!)).toBe(true);
  });

  it("keeps an attention dismissal across /runs, /overview/next and back", async () => {
    mockApi.getAdminHealth.mockResolvedValue(dangerAndAttention());
    await mountAt("/runs");
    await act(async () => { fireEvent.click(screen.getByRole("button", { name: "Dismiss Recovery storage" })); });
    expect(attention()).toBeNull();
    await go("Go to overview");
    expect(attention()).toBeNull();
    await go("Go to runs");
    expect(attention()).toBeNull();
    // The shell polled nothing new: the banner is hidden by its kept state, not by a refetch.
    expect(mockApi.getAdminHealth).toHaveBeenCalledTimes(1);
    // A different banner is unaffected, proving the stack is still there.
    expect(danger()).toBeTruthy();
  });

  describe("with a protected, locked vault", () => {
    beforeEach(() => {
      vi.mocked(useAuth).mockReturnValue({
        ...vi.mocked(useAuth)(),
        vaultMode: "protected",
        vaultUnlocked: false,
        vaultExists: true,
        hasPassword: true,
      });
      mockApi.getAdminHealth.mockResolvedValue(healthySilentDoc());
    });
    const locked = () => screen.queryAllByRole("alert").filter((el) => /Vault locked/.test(el.textContent ?? ""));

    it("renders the vault-locked banner once, after the portalled header, on /overview/next", async () => {
      await mountAt("/overview/next");
      expect(locked()).toHaveLength(1);
      expect(follows(screen.getByRole("heading", { name: "Hdr" }), locked()[0])).toBe(true);
      expect(follows(locked()[0], screen.getByText("preview content"))).toBe(true);
    });

    it("renders the vault-locked banner once, before the page content, on /runs", async () => {
      await mountAt("/runs");
      expect(locked()).toHaveLength(1);
      expect(follows(locked()[0], screen.getByRole("heading", { name: "Runs page" }))).toBe(true);
    });
  });

  it("leaves no header slot content or spacing on a page that has no header", async () => {
    mockApi.getAdminHealth.mockResolvedValue(healthySilentDoc());
    const { container } = await mountAt("/runs");
    const empties = [...container.querySelectorAll("div.empty\\:hidden")];
    expect(empties).toHaveLength(1);
    expect(empties[0].childNodes).toHaveLength(0);
  });
});
