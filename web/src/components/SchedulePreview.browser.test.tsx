/// <reference types="@vitest/browser-playwright" />
import "../index.css";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { page } from "vitest/browser";
import { api } from "../lib/api";
import { RECORDED_EFFECTIVE_SCENARIOS } from "../mocks/data/effectiveSettings";
import { SchedulePreview } from "./SchedulePreview";

// Issue #3260: the preview block in a real engine. Target size and the contrast of its text per
// theme need real layout and computed styles, which jsdom cannot give.
vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return { ...actual, api: { previewScheduleSettings: vi.fn() } };
});

const mockApi = vi.mocked(api);
const originalTheme = document.documentElement.getAttribute("data-theme");

beforeEach(() => {
  const p = RECORDED_EFFECTIVE_SCENARIOS[0].schedule_previews.find(
    (x) => x.name === "sweep pinned to codex with a claude model pin",
  )!;
  mockApi.previewScheduleSettings.mockResolvedValue(structuredClone(p.response));
});
afterEach(async () => {
  cleanup();
  await page.viewport(1280, 800);
  vi.clearAllMocks();
  if (originalTheme === null) document.documentElement.removeAttribute("data-theme");
  else document.documentElement.setAttribute("data-theme", originalTheme);
});

// bg-surface is the schedule modal's dialog panel background.
function mount(width?: number) {
  render(
    <MemoryRouter>
      <div className="bg-surface p-4" data-testid="surface" style={width ? { width } : undefined}>
        <SchedulePreview request={{ target: "sweep", model: "sonnet" }} bothUsable debounceMs={0} />
      </div>
    </MemoryRouter>,
  );
}

const RUN_DEFAULTS = "Run defaults (opens in new tab)";

const heading = () => screen.findByRole("heading", { name: "If fired and claimed now" });

it("has the accessible heading and a definition list of rows", async () => {
  mount();
  await heading();
  const region = await screen.findByRole("region", { name: "If fired and claimed now" });
  await vi.waitFor(() => expect(region.querySelector("dl")).not.toBeNull());
  expect(region.querySelectorAll("dt").length).toBeGreaterThan(3);
});

it("the Run defaults link is at least 24px tall", async () => {
  mount();
  const link = await screen.findByRole("link", { name: RUN_DEFAULTS });
  expect(link.getBoundingClientRect().height).toBeGreaterThanOrEqual(24);
});

it("at a narrow width no value column is squeezed and the list does not overflow", async () => {
  await page.viewport(390, 844);
  const p = RECORDED_EFFECTIVE_SCENARIOS[0].schedule_previews.find((x) => x.name === "unpinned sweep")!;
  mockApi.previewScheduleSettings.mockResolvedValue(structuredClone(p.response));
  mount(320);
  const region = await screen.findByRole("region", { name: "If fired and claimed now" });
  await vi.waitFor(() => expect(region.querySelectorAll("dd").length).toBeGreaterThan(6));
  for (const dd of Array.from(region.querySelectorAll("dd"))) {
    if (!dd.textContent) continue;
    expect(dd.getBoundingClientRect().width, dd.textContent).toBeGreaterThanOrEqual(40);
  }
  const dl = region.querySelector("dl")!;
  expect(dl.scrollWidth).toBeLessThanOrEqual(dl.clientWidth);
});

function rgb(color: string): [number, number, number] {
  const m = color.match(/rgba?\((\d+),\s*(\d+),\s*(\d+)/);
  if (!m) throw new Error(`unparsed colour ${color}`);
  return [Number(m[1]), Number(m[2]), Number(m[3])];
}
function luminance([r, g, b]: [number, number, number]): number {
  const lin = (c: number) => {
    const s = c / 255;
    return s <= 0.03928 ? s / 12.92 : ((s + 0.055) / 1.055) ** 2.4;
  };
  return 0.2126 * lin(r) + 0.7152 * lin(g) + 0.0722 * lin(b);
}

it.each(["dawn", "ember"])("labels, values, hint and link read at WCAG AA contrast in %s", async (theme) => {
  document.documentElement.setAttribute("data-theme", theme);
  mount();
  const link = await screen.findByRole("link", { name: RUN_DEFAULTS });
  const bg = luminance(rgb(getComputedStyle(screen.getByTestId("surface")).backgroundColor));
  const targets = [
    link,
    // The warning Alert text, measured against the surface it sits on.
    screen.getByRole("status"),
    screen.getByText("Harness"),
    screen.getByText("updates as you change the picks above"),
    screen.getByText("This preview is advisory; Save validates again."),
    screen.getByRole("heading", { name: "If fired and claimed now" }),
  ];
  for (const el of targets) {
    const fg = luminance(rgb(getComputedStyle(el).color));
    const ratio = (Math.max(fg, bg) + 0.05) / (Math.min(fg, bg) + 0.05);
    expect(ratio, `${el.textContent} in ${theme}`).toBeGreaterThanOrEqual(4.5);
  }
});
