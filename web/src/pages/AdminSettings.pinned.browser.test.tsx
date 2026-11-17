/// <reference types="@vitest/browser-playwright" />
import "../index.css";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { page, userEvent } from "vitest/browser";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { AppShell } from "../components/AppShell";
import { AdminSettings } from "./AdminSettings";
import { api, type AppSettings } from "../lib/api";
import { useAuth } from "../auth/AuthContext";
import { setupAppShellMocks } from "../test-appshell-mocks";

// #4419: Admin -> Instance pins its "Sections on this page" pills under the tabs, as one
// non-wrapping row with edge fades and a current-section highlight. Layout, sticky and
// IntersectionObserver behaviour need a real engine, so this runs in Chromium inside the
// real AppShell with the AdminSettings data mocked.
vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  const { appShellApiMocks } = await import("../test-appshell-mocks");
  return {
    ...actual,
    api: {
      ...appShellApiMocks(),
      adminListWorkers: vi.fn(),
      getSettings: vi.fn(),
      updateSettings: vi.fn(),
      vaultMigration: vi.fn(),
      adminListDockerAllowlistRepos: vi.fn(),
      getReleaseCheck: vi.fn(),
      checkReleaseNow: vi.fn(),
      getAdminMaintenance: vi.fn(),
      setMaintenance: vi.fn(),
      getSlackStatus: vi.fn(),
    },
  };
});
vi.mock("../auth/AuthContext", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../auth/AuthContext")>()),
  useAuth: vi.fn(),
}));

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
  codex_judge_model: "gpt-6.1-sol",
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
} as unknown as AppSettings;

const loadedResponse = () => ({
  settings,
  secrets: { slack_bot_token: false, slack_app_token: false },
  sources: {} as Record<string, "env" | "db" | "default">,
  defaults: {
    judge_model: "opus",
    codex_judge_model: "gpt-6.1-sol",
    summary_model: "haiku",
    codex_summary_model: "gpt-6-luna",
  },
  slack_status: "disabled",
});

beforeEach(() => {
  localStorage.clear();
  setupAppShellMocks(api, useAuth);
  mockApi.getSettings.mockResolvedValue(loadedResponse());
  mockApi.adminListWorkers.mockResolvedValue({ workers: [] });
  mockApi.vaultMigration.mockResolvedValue({ master_sealed: 0 });
  mockApi.getSlackStatus.mockResolvedValue({ slack_status: "disabled" });
  mockApi.getReleaseCheck.mockRejectedValue(new Error("not under test"));
  mockApi.adminListDockerAllowlistRepos.mockResolvedValue({ repos: [] });
  const none = { count: 0, items: [] };
  mockApi.getAdminMaintenance.mockResolvedValue({
    state: { active: false, since: null, reason: null, by_label: null },
    readout: { as_of: "2026-10-25T12:00:00Z", clear: true, active: none, held: none, custody: none, queued: none, idle_bound_workers: none },
  });
});

afterEach(() => {
  cleanup();
  window.scrollTo(0, 0);
  vi.clearAllMocks();
});

const nextFrame = () => new Promise((r) => requestAnimationFrame(() => r(null)));

async function mount(width: number, height = 700) {
  await page.viewport(width, height);
  render(
    <MemoryRouter initialEntries={["/admin/settings"]}>
      <AppShell>
        <Routes>
          <Route path="/admin/settings" element={<AdminSettings />} />
        </Routes>
      </AppShell>
    </MemoryRouter>,
  );
  await screen.findByRole("navigation", { name: "Sections on this page" });
  await nextFrame();
}

const blockEl = () => document.querySelector<HTMLElement>("[data-pinned-block-root]")!;
const navEl = () => screen.getByRole("navigation", { name: "Sections on this page" });
const pills = () => Array.from(navEl().querySelectorAll<HTMLAnchorElement>("a"));
const idOf = (a: HTMLAnchorElement) => a.getAttribute("href")!.slice(1);
const lineY = () => blockEl().getBoundingClientRect().bottom;
const currentPills = () => pills().filter((a) => a.getAttribute("aria-current") === "true");
/** Section elements that exist, in page order, paired with their pill. */
const sectionEls = () =>
  pills()
    .map((a) => document.getElementById(idOf(a)))
    .filter((el): el is HTMLElement => el !== null);

function expectInRowView(pill: HTMLElement) {
  const row = navEl().getBoundingClientRect();
  const r = pill.getBoundingClientRect();
  expect(r.left, "pill left inside row").toBeGreaterThanOrEqual(row.left - 1);
  expect(r.right, "pill right inside row").toBeLessThanOrEqual(row.right + 1);
}

/** The section whose box contains the reading line, or null in a gap. */
function sectionAtLine(): HTMLElement | null {
  const y = lineY();
  return sectionEls().find((el) => {
    const r = el.getBoundingClientRect();
    return r.top <= y && r.bottom > y;
  }) ?? null;
}

for (const width of [1440, 400]) {
  describe(`Admin -> Instance section pills at ${width}px (#4419)`, () => {
    it("pins the tabs and pills as one block, on one line", async () => {
      await mount(width);
      const block = blockEl();
      const nav = navEl();
      expect(block.contains(nav)).toBe(true);
      expect(block.querySelector('a[href="/admin/users"]')).not.toBeNull();
      expect(nav.scrollHeight).toBe(nav.clientHeight);
      const tops = new Set(pills().map((a) => Math.round(a.getBoundingClientRect().top)));
      expect(tops.size, "every pill on the same line").toBe(1);

      window.scrollTo(0, 900);
      await expect.poll(() => block.dataset.stuck).toBe("true");
      const barBottom = width >= 1024 ? 0 : document.querySelector("div.frame.sticky")!.getBoundingClientRect().bottom;
      expect(Math.abs(block.getBoundingClientRect().top - barBottom)).toBeLessThanOrEqual(0.5);
      expect(nav.getBoundingClientRect().bottom).toBeLessThanOrEqual(block.getBoundingClientRect().bottom + 0.5);
    });

    it("has no horizontal page overflow", async () => {
      await mount(width);
      expect(document.documentElement.scrollWidth).toBeLessThanOrEqual(width);
    });

    it("a mid-page pill click lands its section below the block and highlights it", async () => {
      await mount(width);
      const pill = pills()[Math.floor(pills().length / 2) - 2];
      await userEvent.click(pill);
      const section = document.getElementById(idOf(pill))!;
      await expect.poll(() => section.getBoundingClientRect().top >= lineY() - 0.5).toBe(true);
      await expect.poll(() => currentPills().map(idOf)).toEqual([idOf(pill)]);
      expectInRowView(pill);
    });

    it("clicking the last pill highlights it", async () => {
      await mount(width);
      const last = pills()[pills().length - 1];
      await userEvent.click(last);
      await expect.poll(() => currentPills().map(idOf)).toEqual([idOf(last)]);
      expectInRowView(last);
    });

    it("tracks the section under the reading line by scrolling alone", async () => {
      await mount(width);
      // Scroll so the reading line sits at the middle of a section, well clear of any gap and
      // of the page end (where the end sentinel takes over).
      const docH = () => document.documentElement.scrollHeight;
      // The block's bottom only equals the reading line while it is stuck.
      window.scrollTo(0, 900);
      await expect.poll(() => blockEl().dataset.stuck).toBe("true");
      const targets = sectionEls()
        .slice(1)
        .map((el) => {
          const top = el.getBoundingClientRect().top + window.scrollY;
          return { el, y: top + el.offsetHeight / 2 - lineY() };
        })
        .filter((t) => t.y > 0 && t.y < docH() - 2 * window.innerHeight);
      expect(targets.length, "enough scrollable sections").toBeGreaterThanOrEqual(3);
      const down = targets.slice(0, 4);
      const steps = [...down, ...down.slice(0, -1).reverse()];
      for (const step of steps) {
        window.scrollTo(0, step.y);
        await expect.poll(() => Math.abs(window.scrollY - step.y)).toBeLessThanOrEqual(1);
        await expect.poll(() => sectionAtLine()?.id).toBe(step.el.id);
        await expect.poll(() => currentPills().map(idOf), { timeout: 3000 }).toEqual([step.el.id]);
        expectInRowView(currentPills()[0]);
      }
    });
  });
}

describe("Admin -> Instance pill edge fade (#4419)", () => {
  const fade = () => {
    const nav = navEl();
    return [nav.hasAttribute("data-fade-start"), nav.hasAttribute("data-fade-end")];
  };

  it("fades only the edges that have more pills beyond them (400px)", async () => {
    await mount(400);
    const nav = navEl();
    expect(nav.scrollWidth).toBeGreaterThan(nav.clientWidth);
    const max = nav.scrollWidth - nav.clientWidth;
    nav.scrollLeft = 0;
    await expect.poll(fade).toEqual([false, true]);
    nav.scrollLeft = max / 2;
    await expect.poll(fade).toEqual([true, true]);
    nav.scrollLeft = max;
    await expect.poll(fade).toEqual([true, false]);
  });

  it("sets no fade when every pill fits (few sections)", async () => {
    // A failed load leaves only the always-present sections.
    mockApi.getSettings.mockRejectedValue(new Error("boom"));
    await mount(1440);
    const nav = navEl();
    expect(nav.scrollWidth).toBeLessThanOrEqual(nav.clientWidth);
    await nextFrame();
    await nextFrame();
    expect(fade()).toEqual([false, false]);
  });
});
