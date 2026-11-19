/// <reference types="@vitest/browser-playwright" />
import "../index.css";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { page, userEvent } from "vitest/browser";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { AppShell } from "../components/AppShell";
import { AdminWorkers } from "./AdminWorkers";
import { api, type AppSettings } from "../lib/api";
import { useAuth } from "../auth/AuthContext";
import { setupAppShellMocks } from "../test-appshell-mocks";

// #4454: Admin -> Workers pins its "Sections on this page" pills under the tabs through the
// shared SectionPills (as Admin -> Instance does, #4419): one non-wrapping row that stays in
// view while the page scrolls, with a current-section highlight (aria-current). Sticky layout
// and IntersectionObserver behaviour need a real engine, so this runs in Chromium inside the
// real AppShell with the AdminWorkers data mocked.
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
      adminListDockerAllowlistRepos: vi.fn(),
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

beforeEach(() => {
  localStorage.clear();
  setupAppShellMocks(api, useAuth);
  mockApi.getSettings.mockResolvedValue({
    settings,
    secrets: {},
    sources: {} as Record<string, "env" | "db" | "default">,
    defaults: {},
  } as unknown as Awaited<ReturnType<typeof api.getSettings>>);
  mockApi.adminListWorkers.mockResolvedValue({ workers: [] });
  mockApi.adminListDockerAllowlistRepos.mockResolvedValue({ repos: [] });
});

afterEach(() => {
  cleanup();
  window.scrollTo(0, 0);
  vi.clearAllMocks();
});

const nextFrame = () => new Promise((r) => requestAnimationFrame(() => r(null)));

async function mount(width: number, height = 700, hash = "") {
  await page.viewport(width, height);
  render(
    <MemoryRouter initialEntries={[`/admin/workers${hash}`]}>
      <AppShell>
        <Routes>
          <Route path="/admin/workers" element={<AdminWorkers />} />
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
const sectionEls = () =>
  pills()
    .map((a) => document.getElementById(idOf(a)))
    .filter((el): el is HTMLElement => el !== null);

/** Reading line: the root scroll-padding gap 12 + the cards' scroll-mt-6 24 + 4 slack. */
const readingLine = () => lineY() + 40;

/** The last pill section, in page order, whose top is at or above the reading line (else the first). */
function sectionAtLine(): HTMLElement {
  const y = readingLine();
  const els = sectionEls();
  let cur = els[0];
  for (const el of els) if (el.getBoundingClientRect().top <= y) cur = el;
  return cur;
}

/** The highlight must still hold a few frames later (no late flip). */
async function holdsFrames(expected: string) {
  for (let i = 0; i < 5; i++) await nextFrame();
  expect(currentPills().map(idOf)).toEqual([expected]);
}

/** The pill row is still on screen, inside the pinned block, right under the tabs. */
function expectRowPinned() {
  const block = blockEl();
  const nav = navEl();
  expect(block.contains(nav)).toBe(true);
  const r = nav.getBoundingClientRect();
  const b = block.getBoundingClientRect();
  expect(r.top, "row top at or below the block top").toBeGreaterThanOrEqual(b.top - 0.5);
  expect(r.bottom, "row bottom inside the block").toBeLessThanOrEqual(b.bottom + 0.5);
  expect(r.top, "row top on screen").toBeGreaterThanOrEqual(0);
  expect(r.bottom, "row bottom on screen").toBeLessThanOrEqual(window.innerHeight);
  // Under the tabs: the Admin tab row shares the block, on screen, directly above the pills.
  const tab = block.querySelector<HTMLElement>('a[href="/admin/users"]');
  expect(tab, "the Admin tabs are in the pinned block").not.toBeNull();
  const t = tab!.getBoundingClientRect();
  expect(t.top, "tabs on screen").toBeGreaterThanOrEqual(0);
  expect(t.bottom, "tabs above the pill row").toBeLessThanOrEqual(r.top + 0.5);
}

// Near-page-end deep link: a tall viewport so the last cards fit below the reading line and the
// page cannot scroll a non-final target up to it. Per-width because card heights differ.
// Measured while choosing: at 1440x1100 the clamped target sat only ~4px below the reading
// line (120px from the viewport top), and at 400x1400 the page was not yet clamped (the target
// landed above the reading line, 173px).
// 1440x1200 and 400x1700 clamp the page with the target below the line. The in-test
// precondition asserts that, so a card-height change that breaks the setup fails loudly
// instead of passing vacuously.
const TALL: Record<number, number> = { 1440: 1200, 400: 1700 };
const NEAR_END_ID = "worker-hibernation";

for (const width of [1440, 400]) {
  describe(`Admin -> Workers section pills at ${width}px (#4454)`, () => {
    it("keeps the pill row pinned under the tabs on one line while scrolling", async () => {
      await mount(width);
      const nav = navEl();
      expect(nav.scrollHeight).toBe(nav.clientHeight);
      const tops = new Set(pills().map((a) => Math.round(a.getBoundingClientRect().top)));
      expect(tops.size, "every pill on the same line").toBe(1);
      expect(pills().map(idOf)).toEqual([
        "docker-allowlist",
        "worker-sizes",
        "hosted-worker-quota",
        "custody-hold-limit",
        "ephemeral-workers",
        "worker-hibernation",
        "capability-scheduling",
      ]);
      expect(document.documentElement.scrollHeight, "page scrolls").toBeGreaterThan(window.innerHeight + 200);
      for (const y of [300, 600, 100000]) {
        window.scrollTo(0, y);
        await expect.poll(() => window.scrollY).toBeGreaterThan(0);
        await nextFrame();
        expectRowPinned();
      }
    });

    it("lights the pill of the section under the reading line while scrolling", async () => {
      await mount(width);
      const seen = new Set<string>();
      const max = document.documentElement.scrollHeight - window.innerHeight;
      for (let y = 0; y <= max; y += 40) {
        window.scrollTo(0, y);
        await expect.poll(() => Math.abs(window.scrollY - y)).toBeLessThanOrEqual(1);
        for (let i = 0; i < 3; i++) await nextFrame();
        // Near the page end the end sentinel takes over, so only check away from it.
        if (y > max - 120) break;
        const cur = currentPills();
        expect(cur.length, `exactly one current pill at scrollY ${y}`).toBe(1);
        expect(idOf(cur[0]), `at scrollY ${y}`).toBe(sectionAtLine().id);
        seen.add(idOf(cur[0]));
      }
      expect(seen.size, `pills lit while scrolling: ${[...seen].join(",")}`).toBeGreaterThanOrEqual(2);
    });

    it("a mid-page pill click lands its card below the pinned block and highlights it", async () => {
      await mount(width);
      const pill = pills()[2];
      await userEvent.click(pill);
      const section = document.getElementById(idOf(pill))!;
      await expect.poll(() => section.getBoundingClientRect().top >= lineY() - 0.5).toBe(true);
      await expect.poll(() => section.getBoundingClientRect().top <= readingLine()).toBe(true);
      await expect.poll(() => currentPills().map(idOf)).toEqual([idOf(pill)]);
      await holdsFrames(idOf(pill));
      expectRowPinned();
    });

    it("a deep link to #worker-hibernation lands its card below the pinned block and highlights it", async () => {
      await mount(width, 700, "#worker-hibernation");
      const section = document.getElementById("worker-hibernation")!;
      await expect.poll(() => section.getBoundingClientRect().top >= lineY() - 0.5).toBe(true);
      await expect.poll(() => currentPills().map(idOf), { timeout: 2000 }).toEqual(["worker-hibernation"]);
      await holdsFrames("worker-hibernation");
      expectRowPinned();
    });

    it("lights the last pill at the page end", async () => {
      await mount(width);
      window.scrollTo(0, document.documentElement.scrollHeight);
      await expect
        .poll(() => window.scrollY + window.innerHeight >= document.documentElement.scrollHeight - 1)
        .toBe(true);
      await expect.poll(() => currentPills().map(idOf), { timeout: 3000 }).toEqual(["capability-scheduling"]);
      await holdsFrames("capability-scheduling");
    });

    it(`a deep link to ${NEAR_END_ID} too close to the page end to reach the line lights its own pill`, async () => {
      await mount(width, TALL[width], `#${NEAR_END_ID}`);
      const target = document.getElementById(NEAR_END_ID)!;
      // The page owns a delayed jump. Wait for it and its clamp, rather than
      // assuming mount's first frame means the deep-link scroll already ran.
      await expect.poll(() => window.scrollY, { message: "the page actually scrolled" }).toBeGreaterThan(0);
      await expect.poll(() => window.scrollY + window.innerHeight, { message: "page is scrolled to its end" })
        .toBeGreaterThanOrEqual(document.documentElement.scrollHeight - 1);
      // Precondition: the page could not bring the target up to the reading line.
      await nextFrame();
      expect(target.getBoundingClientRect().top, "target stays below the reading line").toBeGreaterThan(readingLine());
      expect(window.scrollY, "the page actually scrolled").toBeGreaterThan(0);
      expect(
        window.scrollY + window.innerHeight,
        "page is scrolled to its end",
      ).toBeGreaterThanOrEqual(document.documentElement.scrollHeight - 1);
      await expect.poll(() => currentPills().map(idOf), { timeout: 2000 }).toEqual([NEAR_END_ID]);
      await holdsFrames(NEAR_END_ID);
    });
  });
}
