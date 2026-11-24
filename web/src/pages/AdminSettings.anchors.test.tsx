// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, useNavigate } from "react-router-dom";
import { AdminSettings } from "./AdminSettings";
import { healthySilentDoc } from "../mocks/data/health";
import { api } from "../lib/api";
import { ADMIN_SETTINGS_ANCHOR_IDS, visibleSectionItems } from "../components/navSections";
import { installAnchorFrames } from "../test/anchorFrames";

vi.mock("../lib/api", async (importActual) => {
  const actual = await importActual<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      getSettings: vi.fn(),
      getAdminHealth: vi.fn(),
      adminListWorkers: vi.fn(),
      updateSettings: vi.fn(),
      vaultMigration: vi.fn(),
      adminListDockerAllowlistRepos: vi.fn(),
      listRepos: vi.fn(),
      getAgentSource: vi.fn(),
      getReleaseCheck: vi.fn(),
      checkReleaseNow: vi.fn(),
      getAdminMaintenance: vi.fn(),
      setMaintenance: vi.fn(),
      getSlackStatus: vi.fn(),
    },
  };
});
vi.mock("../auth/AuthContext", () => ({ useAuth: () => ({ refresh: vi.fn().mockResolvedValue(undefined) }) }));

const mockApi = vi.mocked(api);

const settings = {
  autopilot_label: "autopilot",
  uzi_label: "uzi",
  default_theme: "ember",
  default_appearance_mode: "dark",
  default_light_theme: "hall",
  default_dark_theme: "ember",
  default_typeface: "system",
  slack_enabled: "false",
  public_base_url: "http://127.0.0.1:8080",
  judge_enabled: "false",
  judge_model: "opus",
  codex_judge_model: "gpt-6.2-sol",
  judge_enforce_all: "false",
  judge_cooldown_seconds: "60",
  judge_daily_budget: "0",
  ephemeral_workers_enabled: "false",
  hosted_worker_quota: "2",
  custody_hold_limit: "8",
  ci_autofix_enabled: "true",
  now_summary_enabled: "true",
  release_check_enabled: "true",
  release_check_banner_enabled: "true",
  summary_model: "haiku",
  codex_summary_model: "gpt-6-luna",
  health_enabled: "true",
  health_stall_seconds: "300",
  health_tool_call_seconds: "1200",
  health_near_timeout_pct: "85",
  health_queued_seconds: "600",
  health_approval_seconds: "3600",
  health_nudge_cooldown_seconds: "1800",
  run_extension_cap_seconds: "57600",
  docker_repo_allowlist: "",
  mr_review_trusted_bots: "",
  capability_aware_scheduling: "true",
  completion_interlock_rollout: "true",
  github_project_sync_enabled: "false",
  fetch_max_file_bytes: "26214400",
  fetch_max_run_bytes: "209715200",
  fetch_max_run_files: "100",
  fetch_max_concurrent_per_run: "4",
  fetch_max_run_attempts: "500",
  app_logo_mode: "default",
  app_logo_preset: "",
  app_logo_keep_name: "true",
  brand_mode: "none",
  brand_company: "",
  brand_placement: "below",
  brand_plaque: "false",
} as unknown as import("../lib/api").AppSettings;

const loadedResponse = () => ({
  settings,
  secrets: { slack_bot_token: false, slack_app_token: false },
  sources: {} as Record<string, "env" | "db" | "default">,
  defaults: {
    judge_model: "opus",
    codex_judge_model: "gpt-6.2-sol",
    summary_model: "haiku",
    codex_summary_model: "gpt-6-luna",
  },
  slack_status: "disabled",
});

const scroll = vi.fn();
const originalScroll = Object.getOwnPropertyDescriptor(HTMLElement.prototype, "scrollIntoView");

beforeEach(() => {
  scroll.mockClear();
  Object.defineProperty(HTMLElement.prototype, "scrollIntoView", { configurable: true, value: scroll });
  mockApi.getSettings.mockResolvedValue(loadedResponse());
  mockApi.getAdminHealth.mockResolvedValue(healthySilentDoc());
  mockApi.adminListWorkers.mockResolvedValue({ workers: [] });
  mockApi.vaultMigration.mockResolvedValue({ master_sealed: 0 });
  mockApi.getSlackStatus.mockResolvedValue({ slack_status: "disabled" });
  mockApi.getReleaseCheck.mockRejectedValue(new Error("not under test"));
  mockApi.getAgentSource.mockRejectedValue(new Error("not under test"));
  mockApi.listRepos.mockResolvedValue({ repos: [] });
  mockApi.adminListDockerAllowlistRepos.mockResolvedValue({ repos: [] });
  const none = { count: 0, items: [] };
  mockApi.getAdminMaintenance.mockResolvedValue({
    state: { active: false, since: null, reason: null, by_label: null },
    readout: { as_of: "2026-10-25T12:00:00Z", clear: true, active: none, held: none, custody: none, queued: none, idle_bound_workers: none },
  });
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
  if (originalScroll) Object.defineProperty(HTMLElement.prototype, "scrollIntoView", originalScroll);
  else Reflect.deleteProperty(HTMLElement.prototype, "scrollIntoView");
});

function GoTo({ to }: { to: string }) {
  const navigate = useNavigate();
  return <button onClick={() => navigate(to)}>go</button>;
}

function renderAt(hash: string) {
  return render(
    <MemoryRouter initialEntries={[`/admin/settings${hash}`]}>
      <AdminSettings />
    </MemoryRouter>,
  );
}

describe("AdminSettings anchors", () => {
  it("keeps a non-final deep-link pill current when an observer evaluates between frames", async () => {
    const id = "review-bots";
    const frames = installAnchorFrames(id);
    scroll.mockImplementation(function(this: HTMLElement) { if (this.id === id) frames.landAtEnd(); });
    try {
      renderAt(`#${id}`);
      await waitFor(() => expect(frames.hasLineObserver()).toBe(true));
      const nav = screen.getByRole("navigation", { name: "Sections on this page" });
      const links = within(nav).getAllByRole("link");
      expect(links[links.length - 1].getAttribute("href")).not.toBe(`#${id}`);
      await act(async () => { frames.advanceFrame(); });
      expect(scroll).not.toHaveBeenCalledWith({ block: "start" });
      await act(async () => { frames.evaluateAtTop(); });
      await act(async () => { frames.advanceFrame(); });
      expect(scroll).toHaveBeenCalledExactlyOnceWith({ block: "start" });
      expect(scroll.mock.contexts[0]).toBe(document.getElementById(id));
      expect(nav.querySelector('[aria-current="true"]')?.getAttribute("href")).toBe(`#${id}`);
    } finally {
      cleanup();
      frames.restore();
      scroll.mockReset();
    }
  });

  it("cancels the delayed jump when navigation changes after the first frame", async () => {
    const id = "review-bots";
    const frames = installAnchorFrames(id);
    scroll.mockImplementation(function(this: HTMLElement) { if (this.id === id) frames.landAtEnd(); });
    try {
      render(<MemoryRouter initialEntries={[`/admin/settings#${id}`]}>
        <GoTo to="/admin/settings#not-advertised" /><AdminSettings />
      </MemoryRouter>);
      await waitFor(() => expect(frames.hasLineObserver()).toBe(true));
      const target = document.getElementById(id);
      await act(async () => { frames.advanceFrame(); });
      await act(async () => { fireEvent.click(screen.getByRole("button", { name: "go" })); });
      expect(document.getElementById(id)).toBe(target); // target remains mounted, so a leaked callback would scroll
      await act(async () => { frames.advanceFrame(); });
      expect(scroll).not.toHaveBeenCalledWith({ block: "start" });
    } finally {
      cleanup();
      frames.restore();
      scroll.mockReset();
    }
  });

  it("renders every advertised anchor, labelled as the menu advertises it", async () => {
    renderAt("");
    await screen.findByRole("navigation", { name: "Sections on this page" });
    await waitFor(() => {
      for (const id of ADMIN_SETTINGS_ANCHOR_IDS) expect(document.getElementById(id), id).not.toBeNull();
    });
    const nav = screen.getByRole("navigation", { name: "Sections on this page" });
    const items = visibleSectionItems(true).filter((i) => i.key.startsWith("admin-settings:"));
    for (const item of items) {
      const id = item.to.split("#")[1];
      const label = item.label.replace("Admin › Settings › ", "");
      const pill = nav.querySelector(`a[href="#${id}"]`);
      if (pill) {
        expect(pill.textContent, id).toBe(label);
      } else {
        const section = document.getElementById(id)!;
        await waitFor(() => expect(within(section).getAllByRole("heading").map((h) => h.textContent), id).toContain(label));
      }
    }
  });

  for (const id of ["slack", "agent-source", "maintenance"]) {
    it(`scrolls to #${id} once settings load`, async () => {
      renderAt(`#${id}`);
      await waitFor(() => expect(document.getElementById(id)).not.toBeNull());
      const el = document.getElementById(id)!;
      await waitFor(() => expect(scroll.mock.contexts).toContain(el));
      expect(scroll).toHaveBeenCalledWith({ block: "start" });
    });

    // #slack never renders on a failed load; agent-source and maintenance do, so those
    // two are the real guards for "a failed load stays at the top".
    it(`does not scroll to #${id} after a failed load`, async () => {
      mockApi.getSettings.mockRejectedValue(new Error("boom"));
      renderAt(`#${id}`);
      await screen.findByText(/boom|Failed to load settings/);
      await act(async () => {});
      // AdminShell scrolls its active tab with block "nearest"; only the section
      // scroll (block "start") must be absent.
      expect(scroll).not.toHaveBeenCalledWith({ block: "start" });
    });
  }

  it("does not scroll to an unadvertised id even when it renders", async () => {
    mockApi.getSettings.mockResolvedValue({ ...loadedResponse(), oidc_status: "enabled" });
    renderAt("#sso");
    await waitFor(() => expect(document.getElementById("sso")).not.toBeNull());
    await act(async () => {});
    expect(scroll).not.toHaveBeenCalledWith({ block: "start" });
  });

  it("re-scrolls when the same section is chosen again", async () => {
    render(
      <MemoryRouter initialEntries={["/admin/settings#slack"]}>
        <GoTo to="/admin/settings#slack" />
        <AdminSettings />
      </MemoryRouter>,
    );
    const el = await waitFor(() => {
      const found = document.getElementById("slack");
      expect(found).not.toBeNull();
      return found!;
    });
    await waitFor(() => expect(scroll.mock.contexts).toContain(el));
    const startCalls = () => scroll.mock.calls.filter((c, i) => c[0]?.block === "start" && scroll.mock.contexts[i] === el).length;
    expect(startCalls()).toBe(1);
    fireEvent.click(screen.getByRole("button", { name: "go" }));
    await waitFor(() => expect(startCalls()).toBe(2));
  });

  it("resets to the top when an advertised anchor's load fails", async () => {
    const scrollTo = vi.fn();
    vi.stubGlobal("scrollTo", scrollTo);
    try {
      mockApi.getSettings.mockRejectedValue(new Error("boom"));
      renderAt("#slack");
      await screen.findByText(/boom|Failed to load settings/);
      await waitFor(() => expect(scrollTo).toHaveBeenCalledWith(0, 0));
    } finally {
      vi.unstubAllGlobals();
    }
  });

  it("does not re-scroll to the hash after a card save", async () => {
    mockApi.updateSettings.mockResolvedValue({ ...loadedResponse(), settings: { ...loadedResponse().settings, vault_policy: "optional" } });
    renderAt("#fetch-caps");
    await waitFor(() => expect(scroll).toHaveBeenCalledWith({ block: "start" }));
    scroll.mockClear();
    fireEvent.change(screen.getByLabelText("Vault policy"), { target: { value: "optional" } });
    fireEvent.click(screen.getByRole("button", { name: "Save vault policy" }));
    await waitFor(() => expect(mockApi.updateSettings).toHaveBeenCalled());
    await act(async () => {});
    expect(scroll).not.toHaveBeenCalledWith({ block: "start" });
  });
});
