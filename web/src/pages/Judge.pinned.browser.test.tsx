/// <reference types="@vitest/browser-playwright" />
import "../index.css";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { page } from "vitest/browser";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { AppShell } from "../components/AppShell";
import { Judge } from "./Judge";
import { api, type JudgeBacklog, type JudgeRecommendationGroup } from "../lib/api";
import { useAuth } from "../auth/AuthContext";
import { setupAppShellMocks } from "../test-appshell-mocks";
import { aUser, authValue } from "./overviewFixtures";
import { JUDGE_CATEGORIES } from "../lib/judge";

// #4419: on Judge, the label chips pin with the bucket tabs from lg up; below lg they scroll
// away and a one-line "Filtered:" marker in the pinned block keeps the filter visible.
vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  const { appShellApiMocks } = await import("../test-appshell-mocks");
  return {
    ...actual,
    api: {
      ...appShellApiMocks(),
      getJudgeBacklog: vi.fn(),
      getJudgeCategoryStats: vi.fn(),
    },
  };
});
vi.mock("../auth/AuthContext", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../auth/AuthContext")>()),
  useAuth: vi.fn(),
}));

const mockApi = vi.mocked(api);

const group = (i: number): JudgeRecommendationGroup => ({
  category: "improve_uzi",
  target: `api/internal/target-${i}`,
  bucket: "todo",
  open_count: 1,
  run_count: 1,
  rationale_preview: `Rationale number ${i}.`,
  occurrences: [
    {
      run_id: `run-${i}`,
      run_title: `Run ${i}`,
      review_id: `rev-${i}`,
      rec_id: `rec-${i}`,
      verdict: "issues",
      confidence: "",
      bucket: "todo",
    },
  ],
});

const backlog = (): JudgeBacklog => ({
  bucket: "todo",
  run: "",
  groups: Array.from({ length: 60 }, (_, i) => group(i + 1)),
  truncated: false,
  triage: { total: 60, todo: 60, filed: 0, done: 0, dismissed: 0, false_positives: 0 },
});

beforeEach(() => {
  localStorage.clear();
  setupAppShellMocks(api, useAuth);
  vi.mocked(useAuth).mockReturnValue(authValue({ ...aUser, is_admin: false, judge_enabled: true }));
  mockApi.getJudgeBacklog.mockResolvedValue(backlog());
  mockApi.getJudgeCategoryStats.mockResolvedValue({
    counts_by_bucket: {
      todo: { improve_uzi: 60 },
      filed: {},
      done: {},
      dismissed: {},
      all: { improve_uzi: 60 },
    },
  });
});

afterEach(() => {
  cleanup();
  window.scrollTo(0, 0);
  localStorage.clear();
  vi.clearAllMocks();
});

const nextFrame = () => new Promise((r) => requestAnimationFrame(() => r(null)));

async function mount(width: number, entry = "/judge") {
  await page.viewport(width, 700);
  render(
    <MemoryRouter initialEntries={[entry]}>
      <AppShell>
        <Judge />
      </AppShell>
    </MemoryRouter>,
  );
  await screen.findByText("api/internal/target-3");
  await nextFrame();
}

const blockEl = () => document.querySelector<HTMLElement>("[data-pinned-block-root]")!;
const barEl = () => document.querySelector<HTMLElement>("div.frame.sticky")!;
const chipGroup = () => screen.getByRole("group", { name: "Filter by recommendation label" });
const chipEl = () => screen.getByRole("button", { name: /^Improve uzi,/ });
const scroller = () => chipGroup().querySelector<HTMLElement>("[data-fade-row]")!;
const markerEl = () => document.querySelector<HTMLElement>("[data-judge-filter-marker]");

async function scrollDeep(y = 1500) {
  window.scrollTo(0, y);
  await waitFor(() => expect(window.scrollY).toBeGreaterThan(y - 5));
  await nextFrame();
  await nextFrame();
}

describe("Judge chips and tabs pin (#4419)", () => {
  it("at 1440px the chips and the tabs share the pinned block at the top, on one line", async () => {
    await mount(1440);
    await scrollDeep();
    const block = blockEl();
    await waitFor(() => expect(block.dataset.stuck).toBe("true"));
    expect(Math.abs(block.getBoundingClientRect().top)).toBeLessThanOrEqual(0.5);
    expect(block.contains(chipGroup())).toBe(true);
    expect(block.contains(screen.getByRole("tablist", { name: "Backlog bucket" }))).toBe(true);
    expect(markerEl(), "no marker at lg").toBeNull();
    const row = scroller();
    expect(row.scrollHeight).toBe(row.clientHeight);
    const chips = Array.from(row.querySelectorAll("button"));
    expect(chips.length).toBeGreaterThan(1);
    // scrollHeight === clientHeight also holds for a wrapped, clipped row: assert one line.
    expect(new Set(chips.map((c) => Math.round(c.getBoundingClientRect().top))).size, "chips share one top").toBe(1);
    expect(document.documentElement.scrollWidth).toBeLessThanOrEqual(1440);
  });

  for (const width of [1440, 1024]) {
    it(`at ${width}px a far-right filter keeps its chip and Clear visible, and the fade follows overflow`, async () => {
      const last = JUDGE_CATEGORIES[JUDGE_CATEGORIES.length - 1];
      await mount(width, `/judge?category=${last}`);
      await scrollDeep();
      const row = scroller();
      expect(row.scrollWidth, "the chip row overflows its scroller").toBeGreaterThan(row.clientWidth);
      const pressed = screen.getByRole("button", { pressed: true });
      const clear = within(chipGroup()).getByRole("button", { name: /Clear/ });
      // Settle the keep-in-view scroll and the edge observers.
      await waitFor(() => {
        const r = row.getBoundingClientRect();
        const p = pressed.getBoundingClientRect();
        expect(p.left, "pressed chip inside the row").toBeGreaterThanOrEqual(r.left);
        expect(p.right, "pressed chip inside the row").toBeLessThanOrEqual(r.right + 0.5);
      });
      const vw = document.documentElement.clientWidth;
      for (const el of [pressed, clear]) {
        const r = el.getBoundingClientRect();
        expect(r.left).toBeGreaterThanOrEqual(0);
        expect(r.right).toBeLessThanOrEqual(vw);
      }
      expect(clear.getBoundingClientRect().left, "Clear sits outside the scroller").toBeGreaterThanOrEqual(row.getBoundingClientRect().right - 0.5);
      await waitFor(() => {
        expect(row.hasAttribute("data-fade-start")).toBe(true);
        expect(row.hasAttribute("data-fade-end")).toBe(false);
      });
      row.scrollLeft = 0;
      await waitFor(() => {
        expect(row.hasAttribute("data-fade-start")).toBe(false);
        expect(row.hasAttribute("data-fade-end")).toBe(true);
      });
    });
  }

  it("at 1024px toggling a far chip keeps that chip in the row, not the first pressed one", async () => {
    await mount(1024, `/judge?category=${JUDGE_CATEGORIES[0]}`);
    await scrollDeep();
    const row = scroller();
    expect(row.scrollWidth).toBeGreaterThan(row.clientWidth);
    row.scrollLeft = row.scrollWidth;
    await nextFrame();
    const chips = Array.from(row.querySelectorAll<HTMLElement>("button"));
    const lastChip = chips[chips.length - 1];
    lastChip.click();
    await waitFor(() => expect(lastChip.getAttribute("aria-pressed")).toBe("true"));
    for (let i = 0; i < 5; i++) await nextFrame();
    const r = row.getBoundingClientRect();
    const c = lastChip.getBoundingClientRect();
    expect(c.left, "toggled chip inside the row").toBeGreaterThanOrEqual(r.left);
    expect(c.right, "toggled chip inside the row").toBeLessThanOrEqual(r.right + 0.5);
  });

  it("at 400px the chips scroll away; an active filter shows a Filtered marker in the pinned block, and Clear clears it", async () => {
    await mount(400, "/judge?category=improve_uzi");
    const block = blockEl();
    expect(block.contains(chipGroup()), "chips are not in the pinned block").toBe(false);
    await scrollDeep();
    await waitFor(() => expect(block.dataset.stuck).toBe("true"));
    expect(chipEl().getBoundingClientRect().bottom).toBeLessThan(0);
    expect(Math.abs(block.getBoundingClientRect().top - barEl().getBoundingClientRect().bottom)).toBeLessThanOrEqual(0.5);

    const marker = markerEl()!;
    expect(marker).toBeTruthy();
    expect(block.contains(marker)).toBe(true);
    expect(marker.textContent).toContain("Filtered: Improve uzi");
    const mr = marker.getBoundingClientRect();
    expect(mr.top).toBeGreaterThanOrEqual(barEl().getBoundingClientRect().bottom - 0.5);
    expect(mr.bottom).toBeLessThanOrEqual(window.innerHeight);
    expect(mr.height).toBeLessThan(48);
    expect(document.documentElement.scrollWidth).toBeLessThanOrEqual(400);

    expect(marker.querySelector("span.truncate")!.getAttribute("title")).toBe("Filtered: Improve uzi");
    within(marker).getByRole("button", { name: "Clear label filter" }).click();
    await waitFor(() => expect(markerEl()).toBeNull());
    expect(document.activeElement).toBe(screen.getByRole("tab", { selected: true }));
    expect(chipEl().getAttribute("aria-pressed")).toBe("false");
  });

  it("at 400px with no filter there is no marker", async () => {
    await mount(400);
    expect(markerEl()).toBeNull();
    expect(chipEl().getAttribute("aria-pressed")).toBe("false");
  });
});
