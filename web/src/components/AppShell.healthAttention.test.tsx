// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { MemoryRouter, Link } from "react-router-dom";
import { AppShell } from "./AppShell";
import { api, type HealthCheck, type HealthDoc } from "../lib/api";
import { useAuth } from "../auth/AuthContext";
import { healthySilentDoc, ownerOnlyDoc, incidentDoc } from "../mocks/data/health";

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
    getMySettings: vi.fn().mockResolvedValue({ settings: { default_harness: null, default_model: null, default_effort: null, judge_model: null, summary_model: null, theme: null } }),
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

  setAdmin("admin-a");
  mockApi.listRepos.mockResolvedValue({ repos: [] });
  mockApi.listConnections.mockResolvedValue({ connections: [] });
  mockApi.getAdminHealth.mockResolvedValue(warning());
});
afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.clearAllMocks();
});

function setAdmin(id: string, is_admin = true) {
  vi.mocked(useAuth).mockReturnValue({
    ...vi.mocked(useAuth)(),
    user: { ...user, id, is_admin },
  });
}
function warning(): HealthDoc {
  const doc = healthySilentDoc();
  doc.checks = doc.checks.map((check) => check.id === "recovery.storage"
    ? { ...check, severity: "warn", attention_banner: true,
        summary: "Synthetic pressure; 0 captures currently marked quota-refused." }
    : check);
  doc.counts = { ok: 22, warn: 1, danger: 0, unknown: 0, na: 0 };
  doc.status = "warn";
  return doc;
}
function tree() {
  return <MemoryRouter initialEntries={["/dashboard"]}>
    <AppShell><Link to="/runs">Go to runs</Link></AppShell>
  </MemoryRouter>;
}
async function mount() {
  const result = render(tree());
  await act(async () => {});
  return result;
}
async function poll(doc: HealthDoc | Error) {
  if (doc instanceof Error) mockApi.getAdminHealth.mockRejectedValue(doc);
  else mockApi.getAdminHealth.mockResolvedValue(doc);
  await act(async () => { await vi.advanceTimersByTimeAsync(10000); });
}
const banner = () => screen.queryByRole("status", { name: "Admin health attention" });
function dismiss(title = "Recovery storage") {
  fireEvent.click(screen.getByRole("button", { name: `Dismiss ${title}` }));
}

describe("AppShell admin health attention", () => {
  it("shows recovery without blocking, including synthetic pressure with zero refusals, with one shared poll", async () => {
    await mount();
    expect(banner()?.textContent).toContain("0 captures currently marked quota-refused");
    expect(banner()?.getAttribute("aria-live")).toBe("polite");
    expect(within(banner()!).getByRole("link", { name: "Open health" }).getAttribute("href")).toBe("/admin/health");
    expect(screen.queryByRole("alert", { name: "Instance health" })).toBeNull();
    expect(mockApi.getAdminHealth).toHaveBeenCalledTimes(1);
    fireEvent.click(screen.getByRole("button", { name: "Open navigation" }));
    await act(async () => {});
    expect(mockApi.getAdminHealth).toHaveBeenCalledTimes(1);
    await poll(ownerOnlyDoc());
    expect(banner()?.textContent).toContain("2 captures currently marked quota-refused");
    expect(mockApi.getAdminHealth).toHaveBeenCalledTimes(2);
    await poll(new Error("offline"));
    expect(banner()?.textContent).toContain("2 captures currently marked quota-refused");
    expect(mockApi.getAdminHealth).toHaveBeenCalledTimes(3);
  });

  it("never fetches or mounts attention for a nonadmin", async () => {
    setAdmin("nonadmin", false);
    await mount();
    await act(async () => { await vi.advanceTimersByTimeAsync(10000); });
    expect(mockApi.getAdminHealth).not.toHaveBeenCalled();
    expect(banner()).toBeNull();
  });

  it("strictly selects opted-in owner warnings and keeps future checks in document order as text", async () => {
    const doc = warning();
    const base = doc.checks.find((c) => c.id === "recovery.storage")!;
    const excluded: HealthCheck[] = [false, undefined, null, "true", 1, {}].map((flag, i) => ({
      ...base, id: `optout-${i}`, title: `Excluded ${i}`, attention_banner: flag,
    } as HealthCheck));
    excluded.push(
      { ...base, id: "instance", scope: "instance", title: "Excluded instance" },
      { ...base, id: "badscope", scope: "future", title: "Excluded scope" },
      ...["danger", "unknown", "ok", "na"].map((severity) => ({
        ...base, id: severity, severity, title: `Excluded ${severity}`,
      })),
      { ...base, id: "unrelated", attention_banner: undefined, title: "Unrelated owner warning" },
    );
    doc.checks = [
      { ...base, id: "future-z", title: "<img src=x onerror=alert(1)>", summary: "<script>unsafe()</script>" },
      ...excluded,
      { ...base, id: "future-a", title: "Future A" },
    ];
    mockApi.getAdminHealth.mockResolvedValue(doc);
    await mount();
    const rows = within(banner()!).getAllByRole("listitem");
    expect(rows).toHaveLength(2);
    expect(rows[0].textContent).toContain("<img src=x onerror=alert(1)>");
    expect(rows[0].textContent).toContain("<script>unsafe()</script>");
    expect(rows[1].textContent).toContain("Future A");
    expect(banner()!.querySelector("img, script")).toBeNull();
    expect(screen.getByRole("button", { name: "Dismiss <img src=x onerror=alert(1)>" })).toBeTruthy();
    dismiss("<img src=x onerror=alert(1)>");
    expect(within(banner()!).getAllByRole("listitem")).toHaveLength(1);
  });

  it("preserves dismissal across navigation, summary/count changes, missing checks/metadata and failed polls", async () => {
    await mount();
    dismiss();
    expect(banner()).toBeNull();
    fireEvent.click(screen.getByRole("link", { name: "Go to runs" }));
    const changed = warning();
    changed.checks.find((c) => c.id === "recovery.storage")!.summary = "Changed count and summary";
    changed.counts.warn = 12;
    await poll(changed);
    expect(banner()).toBeNull();
    await poll({ ...changed, checks: [] });
    expect(banner()).toBeNull();
    for (const flag of [undefined, false]) {
      const absent = warning();
      absent.checks.find((c) => c.id === "recovery.storage")!.attention_banner = flag;
      await poll(absent);
      expect(banner()).toBeNull();
    }
    await poll(new Error("offline"));
    await poll(warning());
    expect(banner()).toBeNull();
    expect(mockApi.snoozeAdminHealth).not.toHaveBeenCalled();
  });

  it("keeps dismissal through a pending poll while newly added warnings remain independent", async () => {
    await mount();
    let finish!: (doc: HealthDoc) => void;
    mockApi.getAdminHealth.mockReturnValueOnce(new Promise((resolve) => { finish = resolve; }));
    await act(async () => { await vi.advanceTimersByTimeAsync(10000); });
    expect(mockApi.getAdminHealth).toHaveBeenCalledTimes(2);
    expect(banner()).toBeTruthy();
    dismiss();
    expect(banner()).toBeNull();

    const added = warning();
    const recovery = added.checks.find((c) => c.id === "recovery.storage")!;
    added.checks.push({ ...recovery, id: "future-check", title: "Future warning" });
    await act(async () => { finish(added); });
    expect(within(banner()!).getAllByRole("listitem")).toHaveLength(1);
    expect(within(banner()!).getByRole("button", { name: "Dismiss Future warning" })).toBeTruthy();
    dismiss("Future warning");
    expect(banner()).toBeNull();

    await poll({ ...added, checks: added.checks.map((c) =>
      c.id === "future-check" ? { ...c, severity: "ok" } : c) });
    expect(banner()).toBeNull();
    await poll(added);
    expect(within(banner()!).getAllByRole("listitem")).toHaveLength(1);
    expect(within(banner()!).getByRole("button", { name: "Dismiss Future warning" })).toBeTruthy();
    expect(mockApi.getAdminHealth).toHaveBeenCalledTimes(4);
  });

  it("resets dismissal across an admin/nonadmin switch without polling as a nonadmin", async () => {
    const view = await mount();
    dismiss();
    expect(banner()).toBeNull();
    setAdmin("admin-a", false);
    view.rerender(tree());
    await act(async () => { await vi.advanceTimersByTimeAsync(20000); });
    expect(banner()).toBeNull();
    expect(mockApi.getAdminHealth).toHaveBeenCalledTimes(1);

    setAdmin("admin-a");
    view.rerender(tree());
    await act(async () => {});
    expect(banner()).toBeTruthy();
    expect(mockApi.getAdminHealth).toHaveBeenCalledTimes(2);
    await poll(warning());
    expect(banner()).toBeTruthy();
    expect(mockApi.getAdminHealth).toHaveBeenCalledTimes(3);
  });

  it.each(["ok", "unknown", "danger", "na"])("clears dismissal on explicit %s before metadata eligibility", async (severity) => {
    await mount();
    dismiss();
    const resolved = warning();
    const check = resolved.checks.find((c) => c.id === "recovery.storage")!;
    check.severity = severity;
    delete check.attention_banner;
    await poll(resolved);
    expect(banner()).toBeNull();
    await poll(warning());
    expect(banner()).toBeTruthy();
  });

  it("survives initial loading/failure and resets after remount and admin identity switches in both directions", async () => {
    let finish!: (doc: HealthDoc) => void;
    mockApi.getAdminHealth.mockReturnValueOnce(new Promise((resolve) => { finish = resolve; }));
    const view = await mount();
    expect(banner()).toBeNull();
    await act(async () => { finish(warning()); });
    dismiss();
    setAdmin("admin-b");
    view.rerender(tree());
    await act(async () => {});
    expect(banner()).toBeTruthy();
    dismiss();
    setAdmin("admin-a");
    view.rerender(tree());
    await act(async () => {});
    expect(banner()).toBeTruthy();
    dismiss();
    view.unmount();
    mockApi.getAdminHealth.mockRejectedValueOnce(new Error("initial offline"));
    const next = await mount();
    expect(banner()).toBeNull();
    await poll(warning());
    expect(banner()).toBeTruthy();
    next.unmount();
  });

  it("keeps Danger first and independent of dismissal, later episodes and Danger snooze", async () => {
    const danger = { ...incidentDoc(), checks: [...incidentDoc().checks.filter((c) => c.id !== "recovery.storage"), warning().checks.find((c) => c.id === "recovery.storage")!] };
    mockApi.getAdminHealth.mockResolvedValue(danger);
    await mount();
    const alert = screen.getByRole("alert", { name: "Instance health" });
    expect(alert.compareDocumentPosition(banner()!) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    dismiss();
    expect(banner()).toBeNull();
    expect(screen.getByRole("alert", { name: "Instance health" })).toBeTruthy();
    await poll({ ...danger, episode_id: "later-episode" });
    expect(banner()).toBeNull();
    expect(screen.getByRole("alert", { name: "Instance health" })).toBeTruthy();
    expect(mockApi.snoozeAdminHealth).not.toHaveBeenCalled();
    await poll(healthySilentDoc());
    await poll(danger);
    expect(banner()).toBeTruthy();
    await act(async () => { fireEvent.click(screen.getByRole("button", { name: "Snooze 1 h" })); });
    expect(mockApi.snoozeAdminHealth).toHaveBeenCalledTimes(1);
    expect(screen.queryByRole("alert", { name: "Instance health" })).toBeNull();
    expect(banner()).toBeTruthy();
    dismiss();
    await poll({ ...danger, episode_id: "newer-episode" });
    expect(screen.getByRole("alert", { name: "Instance health" })).toBeTruthy();
    expect(banner()).toBeNull();
  });
});
