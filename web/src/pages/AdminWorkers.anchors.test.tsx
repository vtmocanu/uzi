// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter, useNavigate } from "react-router-dom";
import { AdminWorkers } from "./AdminWorkers";
import { healthySilentDoc } from "../mocks/data/health";
import { api } from "../lib/api";
import { ADMIN_WORKERS_ANCHOR_IDS, visibleSectionItems } from "../components/navSections";
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
      adminListDockerAllowlistRepos: vi.fn(),
    },
  };
});
vi.mock("../auth/AuthContext", () => ({ useAuth: () => ({ refresh: vi.fn().mockResolvedValue(undefined) }) }));

const mockApi = vi.mocked(api);

// A full AppSettings fixture; tests override the fields they exercise.
const settings = (over: Partial<import("../lib/api").AppSettings> = {}) => ({
  autopilot_label: "autopilot",
  uzi_label: "uzi",
  default_theme: "ember",
  // PRD #1167 "Lights on" m2: instance appearance defaults (required AppSettings keys).
  default_appearance_mode: "dark",
  default_light_theme: "hall",
  default_dark_theme: "ember",
  default_typeface: "system",
  slack_enabled: "false",
  public_base_url: "http://127.0.0.1:8080",
  judge_enabled: "false",
  judge_model: "opus",
  // The server reports the built-in as the effective value of an unset key (source "default").
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
  // PRD #685 branding config keys (owned by the Branding tab; unbranded defaults).
  app_logo_mode: "default",
  app_logo_preset: "",
  app_logo_keep_name: "true",
  brand_mode: "none",
  brand_company: "",
  brand_placement: "below",
  brand_plaque: "false",
  ...over,
});

const loadedResponse = () => ({
  settings: settings(),
  secrets: {},
  sources: {} as Record<string, "env" | "db" | "default">,
  defaults: {},
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
  mockApi.adminListDockerAllowlistRepos.mockResolvedValue({ repos: [] });
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
    <MemoryRouter initialEntries={[`/admin/workers${hash}`]}>
      <AdminWorkers />
    </MemoryRouter>,
  );
}

describe("AdminWorkers anchors", () => {
  it("keeps a non-final deep-link pill current when an observer evaluates between frames", async () => {
    const id = "worker-hibernation";
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
    const id = "worker-hibernation";
    const frames = installAnchorFrames(id);
    scroll.mockImplementation(function(this: HTMLElement) { if (this.id === id) frames.landAtEnd(); });
    try {
      render(<MemoryRouter initialEntries={[`/admin/workers#${id}`]}>
        <GoTo to="/admin/workers#not-advertised" /><AdminWorkers />
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
    const nav = await screen.findByRole("navigation", { name: "Sections on this page" });
    await waitFor(() => {
      for (const id of ADMIN_WORKERS_ANCHOR_IDS) expect(document.getElementById(id), id).not.toBeNull();
    });
    const items = visibleSectionItems(true).filter((i) => i.key.startsWith("admin-workers:"));
    expect(items).toHaveLength(ADMIN_WORKERS_ANCHOR_IDS.size);
    for (const item of items) {
      const id = item.to.split("#")[1];
      const label = item.label.replace("Admin › Workers › ", "");
      const pill = nav.querySelector(`a[href="#${id}"]`);
      if (pill) {
        expect(pill.textContent, id).toBe(label);
      } else {
        const section = document.getElementById(id)!;
        await waitFor(() => expect(within(section).getAllByRole("heading").map((h) => h.textContent), id).toContain(label));
      }
    }
  });

  for (const id of ["docker-allowlist", "ephemeral-workers", "capability-scheduling"]) {
    it(`scrolls to #${id} once settings load`, async () => {
      renderAt(`#${id}`);
      await waitFor(() => expect(document.getElementById(id)).not.toBeNull());
      const el = document.getElementById(id)!;
      await waitFor(() => expect(scroll.mock.contexts).toContain(el));
      expect(scroll).toHaveBeenCalledWith({ block: "start" });
    });

    it(`does not scroll to #${id} after a failed load`, async () => {
      mockApi.getSettings.mockRejectedValue(new Error("boom"));
      renderAt(`#${id}`);
      await screen.findByText(/boom|Failed to load settings/);
      await act(async () => {});
      expect(scroll).not.toHaveBeenCalledWith({ block: "start" });
    });
  }

  it("does not scroll to an id Admin Workers does not advertise", async () => {
    renderAt("#review-bots");
    await waitFor(() => expect(document.getElementById("docker-allowlist")).not.toBeNull());
    await act(async () => {});
    expect(scroll).not.toHaveBeenCalledWith({ block: "start" });
  });

  it("re-scrolls when the same section is chosen again", async () => {
    render(
      <MemoryRouter initialEntries={["/admin/workers#worker-hibernation"]}>
        <GoTo to="/admin/workers#worker-hibernation" />
        <AdminWorkers />
      </MemoryRouter>,
    );
    const el = await waitFor(() => {
      const found = document.getElementById("worker-hibernation");
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
      renderAt("#hosted-worker-quota");
      await screen.findByText(/boom|Failed to load settings/);
      await waitFor(() => expect(scrollTo).toHaveBeenCalledWith(0, 0));
    } finally {
      vi.unstubAllGlobals();
    }
  });

  it("does not re-scroll to the hash after a card save", async () => {
    mockApi.updateSettings.mockResolvedValue({ ...loadedResponse(), settings: settings({ hosted_worker_quota: "5" }) });
    renderAt("#custody-hold-limit");
    await waitFor(() => expect(scroll).toHaveBeenCalledWith({ block: "start" }));
    scroll.mockClear();
    const quota = await screen.findByRole("spinbutton", { name: "Hosted workers per user" });
    fireEvent.change(quota, { target: { value: "5" } });
    fireEvent.click(screen.getByRole("button", { name: "Save hosted worker quota" }));
    await waitFor(() => expect(mockApi.updateSettings).toHaveBeenCalled());
    await act(async () => {});
    expect(scroll).not.toHaveBeenCalledWith({ block: "start" });
  });
});
